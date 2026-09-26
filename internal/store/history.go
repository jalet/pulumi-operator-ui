package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// S3Stack is an active Stack whose backend may hold S3 history.
type S3Stack struct {
	Namespace, Name, BackendURL, Project, PulumiStack string
	Preview                                           bool // spec.preview
}

// HistoryEntry is one parsed history file, reduced to the stored fields.
type HistoryEntry struct {
	Key, Bucket, Namespace, StackName string
	Type                              RunType  // mapped: up, refresh or destroy
	State                             RunState // succeeded or failed
	StartedAt, EndedAt                time.Time
	Commit                            string
	Counts                            map[string]int64
	ExecKind, ExecAgent, Message      string // origin and commit subject
	VCSRepo                           string // "<host>/<owner>/<repo>"
	Seq                               int64  // position among history keys; 0 = unknown
	// SeenAt is set when read back; InsertHistory takes it as an argument instead.
	SeenAt time.Time
}

// S3Stacks returns active stacks with an s3:// backend and a known project and pulumi stack.
func (s *Store) S3Stacks(ctx context.Context) ([]S3Stack, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT namespace, name, backend_url, project, pulumi_stack, preview FROM stacks
		WHERE deleted_at IS NULL AND backend_url LIKE 's3://%'
		  AND project <> '' AND pulumi_stack <> ''
		ORDER BY namespace, name LIMIT $1`, stacksMax+1)
	if err != nil {
		return nil, fmt.Errorf("s3 stacks: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[S3Stack])
	if err != nil {
		return nil, fmt.Errorf("s3 stacks: %w", err)
	}
	if len(out) > stacksMax {
		return nil, fmt.Errorf("s3 stacks: more than %d stacks", stacksMax)
	}
	return out, nil
}

// HistoryCursor returns the last processed key for bucket and prefix and how many history
// keys were listed up to it; "" and 0 when none.
func (s *Store) HistoryCursor(ctx context.Context, bucket, prefix string) (string, int64, error) {
	var key string
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT last_key, key_count FROM s3_cursors
		WHERE bucket = $1 AND prefix = $2`, bucket, prefix).Scan(&key, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("history cursor: %w", err)
	}
	return key, n, nil
}

// SetHistoryCursor records key as processed with count history keys listed up to it. The
// cursor only moves forward, so a late or repeated call never re-reads or renumbers keys.
// The exception is a countless cursor (key_count 0, written by a version that did not
// count, for example during a rolling update): any counted write replaces it.
func (s *Store) SetHistoryCursor(ctx context.Context, bucket, prefix, key string, count int64,
	at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO s3_cursors (bucket, prefix, last_key, key_count, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (bucket, prefix) DO UPDATE SET
		    last_key   = CASE WHEN s3_cursors.key_count = 0 AND EXCLUDED.key_count > 0
		                      THEN EXCLUDED.last_key
		                      ELSE GREATEST(s3_cursors.last_key, EXCLUDED.last_key) END,
		    key_count  = CASE WHEN s3_cursors.key_count = 0 AND EXCLUDED.key_count > 0
		                           OR EXCLUDED.last_key > s3_cursors.last_key
		                      THEN EXCLUDED.key_count ELSE s3_cursors.key_count END,
		    updated_at = EXCLUDED.updated_at`, bucket, prefix, key, count, at)
	if err != nil {
		return fmt.Errorf("set history cursor: %w", err)
	}
	return nil
}

// InsertHistory stores e as pending. For an existing key it fills columns that are still
// empty (origin, message, repo, seq) and copies them onto the run the key is linked to;
// inserted is false then.
func (s *Store) InsertHistory(ctx context.Context, e HistoryEntry, seenAt time.Time) (
	bool, error) {
	if e.Key == "" || e.Type == "" || e.State == "" || e.Namespace == "" || e.StackName == "" {
		panic("invariant violated: incomplete history entry")
	}
	if e.Counts == nil {
		e.Counts = map[string]int64{} // a file without resourceChanges: {} rather than null
	}
	counts, err := json.Marshal(e.Counts)
	if err != nil {
		panic("invariant violated: marshal counts: " + err.Error())
	}
	// An existing key only has its empty origin, message and repo filled: a re-read after
	// the cursor reset enriches old rows but never changes their link, times or counts.
	var inserted bool
	var runID *int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO s3_history (key, bucket, namespace, stack_name, type, state, started_at,
		                        ended_at, commit, counts, seen_at, exec_kind, exec_agent,
		                        message, vcs_repo, seq)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (bucket, key) DO UPDATE SET
		    exec_kind  = CASE WHEN s3_history.exec_kind = ''  THEN EXCLUDED.exec_kind
		                      ELSE s3_history.exec_kind END,
		    exec_agent = CASE WHEN s3_history.exec_agent = '' THEN EXCLUDED.exec_agent
		                      ELSE s3_history.exec_agent END,
		    message    = CASE WHEN s3_history.message = ''    THEN EXCLUDED.message
		                      ELSE s3_history.message END,
		    vcs_repo   = CASE WHEN s3_history.vcs_repo = ''   THEN EXCLUDED.vcs_repo
		                      ELSE s3_history.vcs_repo END,
		    seq        = COALESCE(s3_history.seq, EXCLUDED.seq)
		WHERE (s3_history.exec_kind = '' AND EXCLUDED.exec_kind <> '')
		   OR (s3_history.exec_agent = '' AND EXCLUDED.exec_agent <> '')
		   OR (s3_history.message = '' AND EXCLUDED.message <> '')
		   OR (s3_history.vcs_repo = '' AND EXCLUDED.vcs_repo <> '')
		   OR (s3_history.seq IS NULL AND EXCLUDED.seq IS NOT NULL)
		RETURNING (xmax = 0), run_id`,
		e.Key, e.Bucket, e.Namespace, e.StackName, e.Type, e.State, e.StartedAt, e.EndedAt,
		e.Commit, counts, seenAt, e.ExecKind, e.ExecAgent, e.Message, e.VCSRepo, seqArg(e.Seq)).
		Scan(&inserted, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // an existing key with nothing left to fill
	}
	if err != nil {
		return false, fmt.Errorf("insert history %s: %w", e.Key, err)
	}
	if !inserted && runID != nil {
		changed, err := enrichRun(ctx, s.pool, *runID, e)
		if err != nil {
			return false, err
		}
		if changed {
			s.pub.Publish(events.Event{Kind: events.KindRun, Namespace: e.Namespace,
				Stack: e.StackName, RunID: *runID})
			s.pub.Publish(events.Event{Kind: events.KindStack, Namespace: e.Namespace,
				Stack: e.StackName})
		}
	}
	return inserted, nil
}

// dbExec is satisfied by both the pool and a transaction.
type dbExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// enrichRun copies origin and repository from a history entry onto its run, filling only
// empty values; the commit subject becomes the title only for imported runs (named "s3:").
// Runs backfilled from Stack.status.lastUpdate have no UID either, but they are operator
// runs whose history message is PKO's, not a commit subject. It reports whether the run row
// changed, so a caller can publish it.
func enrichRun(ctx context.Context, db dbExec, runID int64, e HistoryEntry) (bool, error) {
	var id int64
	err := db.QueryRow(ctx, `UPDATE runs SET
			exec_kind  = CASE WHEN exec_kind = ''  THEN $2 ELSE exec_kind END,
			exec_agent = CASE WHEN exec_agent = '' THEN $3 ELSE exec_agent END,
			vcs_repo   = CASE WHEN vcs_repo = ''   THEN $4 ELSE vcs_repo END,
			title      = CASE WHEN update_name LIKE 's3:%' AND title = '' THEN $5 ELSE title END,
			seq        = COALESCE(seq, $6)
		WHERE id = $1 AND ((exec_kind = '' AND $2 <> '') OR (exec_agent = '' AND $3 <> '')
		   OR (vcs_repo = '' AND $4 <> '') OR (update_name LIKE 's3:%' AND title = '' AND $5 <> '')
		   OR (seq IS NULL AND $6::bigint IS NOT NULL))
		RETURNING id`, runID, e.ExecKind, e.ExecAgent, e.VCSRepo, e.Message, seqArg(e.Seq)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("enrich run %d: %w", runID, err)
	}
	return true, nil
}

const (
	linkWindow  = 60 * time.Second // PKO Update times vs engine times
	importGrace = 15 * time.Minute // let a late Update record claim the entry first
)

// linkBatchRowsMax bounds one LinkHistory call; a var so tests can shrink it, as prune does.
var linkBatchRowsMax = 500

var (
	_s3Ambiguous = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pou_s3_ambiguous_total",
		Help: "S3 history entries that matched more than one run and were left unlinked.",
	})
	_metricsOnce sync.Once
)

func registerMetrics() {
	_metricsOnce.Do(func() { ctrlmetrics.Registry.MustRegister(_s3Ambiguous) })
}

// LinkResult counts what one LinkHistory call did with pending entries.
type LinkResult struct{ Linked, Imported, Ambiguous, Pending int }

type linkOutcome int

const (
	outcomePending linkOutcome = iota + 1
	outcomeLinked
	outcomeImported
	outcomeAmbiguous
)

// LinkHistory links or imports up to linkBatchRowsMax pending entries, oldest first. Call it
// until Pending is 0 or a call makes no progress.
func (s *Store) LinkHistory(ctx context.Context, now time.Time) (LinkResult, error) {
	var res LinkResult
	rows, err := s.pool.Query(ctx, `
		SELECT key, bucket, namespace, stack_name, type, state, started_at, ended_at, commit, counts,
		       seen_at, exec_kind, exec_agent, message, vcs_repo, COALESCE(seq, 0)
		FROM s3_history WHERE link_state = 'pending' ORDER BY ended_at, key LIMIT $1`,
		linkBatchRowsMax)
	if err != nil {
		return res, fmt.Errorf("link history: %w", err)
	}
	pending, err := pgx.CollectRows(rows, scanHistoryEntry)
	if err != nil {
		return res, fmt.Errorf("link history: %w", err)
	}
	for _, e := range pending {
		outcome, runID, err := s.linkOne(ctx, e, now)
		if err != nil {
			return res, err
		}
		switch outcome {
		case outcomeLinked:
			res.Linked++
		case outcomeImported:
			res.Imported++
		case outcomeAmbiguous:
			res.Ambiguous++
			_s3Ambiguous.Inc()
		case outcomePending:
			res.Pending++
		}
		if runID != 0 {
			s.pub.Publish(events.Event{Kind: events.KindRun, Namespace: e.Namespace,
				Stack: e.StackName, RunID: runID})
			s.pub.Publish(events.Event{Kind: events.KindStack, Namespace: e.Namespace,
				Stack: e.StackName})
		}
	}
	return res, nil
}

// linkOne decides one entry in its own transaction, so each decision is atomic.
func (s *Store) linkOne(ctx context.Context, e HistoryEntry, now time.Time) (
	linkOutcome, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("link %s: begin: %w", e.Key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	type candidate struct {
		id     int64
		source CommitSource
	}
	// A candidate either started within linkWindow of the entry, or was backfilled from
	// Stack.status.lastUpdate (no start time, no UID): that run is the stack's last update as
	// of observed_at, so it matches only the newest entry that ended by then.
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.commit_source FROM runs r
		WHERE r.namespace = $1 AND r.stack_name = $2 AND r.type = $3 AND r.state = $4
		  AND NOT EXISTS (SELECT 1 FROM run_changes c WHERE c.run_id = r.id AND c.source = 's3')
		  AND (r.started_at BETWEEN $5::timestamptz - make_interval(secs => $6)
		                        AND $5::timestamptz + make_interval(secs => $6)
		       OR (r.started_at IS NULL AND r.uid IS NULL AND $7::timestamptz <= r.observed_at
		           AND NOT EXISTS (SELECT 1 FROM s3_history h
		                           WHERE h.namespace = r.namespace AND h.stack_name = r.stack_name
		                             AND h.type = r.type
		                             AND h.ended_at > $7 AND h.ended_at <= r.observed_at)))
		LIMIT 2`, e.Namespace, e.StackName, e.Type, e.State, e.StartedAt, linkWindow.Seconds(),
		e.EndedAt)
	if err != nil {
		return 0, 0, fmt.Errorf("link %s: candidates: %w", e.Key, err)
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (candidate, error) {
		var c candidate
		return c, r.Scan(&c.id, &c.source)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("link %s: candidates: %w", e.Key, err)
	}
	var (
		outcome linkOutcome
		runID   int64
	)
	switch {
	case len(cands) > 1:
		outcome = outcomeAmbiguous
		err = setLinkState(ctx, tx, e.Bucket, e.Key, "ambiguous", 0)
	case len(cands) == 1:
		outcome, runID = outcomeLinked, cands[0].id
		err = linkRun(ctx, tx, e, runID, cands[0].source)
	case e.EndedAt.Before(now.Add(-importGrace)) && e.SeenAt.Before(now.Add(-importGrace)):
		// Both clocks: after downtime an old entry is seen at once, and the controllers
		// need a moment to record its Update before an import would duplicate it.
		outcome = outcomeImported
		runID, err = importRun(ctx, tx, e)
	default:
		return outcomePending, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("link %s: %w", e.Key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("link %s: commit: %w", e.Key, err)
	}
	return outcome, runID, nil
}

func linkRun(ctx context.Context, tx pgx.Tx, e HistoryEntry, runID int64, src CommitSource) error {
	if err := insertChanges(ctx, tx, runID, e.Counts); err != nil {
		return err
	}
	// Backfilled runs have no times of their own; the history entry supplies them.
	if _, err := tx.Exec(ctx, `UPDATE runs SET started_at = COALESCE(started_at, $2),
		ended_at = COALESCE(ended_at, $3) WHERE id = $1`, runID, e.StartedAt, e.EndedAt); err != nil {
		return fmt.Errorf("fill run times: %w", err)
	}
	if _, err := enrichRun(ctx, tx, runID, e); err != nil {
		return err
	}
	if e.Commit != "" && (src == "" || src == CommitSourceStack) {
		if err := upgradeCommit(ctx, tx, runID, e.Commit); err != nil {
			return err
		}
	}
	return setLinkState(ctx, tx, e.Bucket, e.Key, "linked", runID)
}

func importRun(ctx context.Context, tx pgx.Tx, e HistoryEntry) (int64, error) {
	name := "s3:" + strings.TrimSuffix(strings.TrimSuffix(path.Base(e.Key), ".gz"), ".history.json")
	src := CommitSourceHistory
	if e.Commit == "" {
		src = ""
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO runs (namespace, update_name, stack_name, type, commit, commit_source, state,
		                  started_at, ended_at, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		ON CONFLICT (namespace, update_name) DO UPDATE SET update_name = EXCLUDED.update_name
		RETURNING id`, e.Namespace, name, e.StackName, e.Type, e.Commit, src, e.State,
		e.StartedAt, e.EndedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("import run: %w", err)
	}
	if err := insertChanges(ctx, tx, id, e.Counts); err != nil {
		return 0, err
	}
	if _, err := enrichRun(ctx, tx, id, e); err != nil {
		return 0, err
	}
	return id, setLinkState(ctx, tx, e.Bucket, e.Key, "imported", id)
}

func insertChanges(ctx context.Context, tx pgx.Tx, runID int64, counts map[string]int64) error {
	b, err := json.Marshal(counts)
	if err != nil {
		panic("invariant violated: marshal counts: " + err.Error())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_changes (run_id, source, counts) VALUES ($1, 's3', $2)
		ON CONFLICT (run_id, source) DO UPDATE SET counts = EXCLUDED.counts`, runID, b); err != nil {
		return fmt.Errorf("insert changes: %w", err)
	}
	return nil
}

// upgradeCommit replaces an approximate commit with the history's exact one; an exact
// commit from the Update is never replaced.
func upgradeCommit(ctx context.Context, tx pgx.Tx, runID int64, commit string) error {
	if _, err := tx.Exec(ctx, `UPDATE runs SET commit = $2, commit_source = 'history'
		WHERE id = $1 AND commit_source IN ('', 'stack')`, runID, commit); err != nil {
		return fmt.Errorf("upgrade commit %d: %w", runID, err)
	}
	return nil
}

func setLinkState(ctx context.Context, tx pgx.Tx, bucket, key, state string, runID int64) error {
	var id *int64
	if runID != 0 {
		id = &runID
	}
	// Only a pending entry moves: another writer may have linked it since it was read.
	if _, err := tx.Exec(ctx, `UPDATE s3_history SET link_state = $3, run_id = $4
		WHERE bucket = $1 AND key = $2 AND link_state = 'pending'`, bucket, key, state, id); err != nil {
		return fmt.Errorf("set link state: %w", err)
	}
	return nil
}

func scanHistoryEntry(row pgx.CollectableRow) (HistoryEntry, error) {
	var e HistoryEntry
	err := row.Scan(&e.Key, &e.Bucket, &e.Namespace, &e.StackName, &e.Type, &e.State,
		&e.StartedAt, &e.EndedAt, &e.Commit, &e.Counts, &e.SeenAt, &e.ExecKind, &e.ExecAgent,
		&e.Message, &e.VCSRepo, &e.Seq)
	return e, err
}

// seqArg maps an unknown sequence number (0) to NULL.
func seqArg(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
