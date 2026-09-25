package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// S3Stack is an active Stack whose backend may hold S3 history.
type S3Stack struct {
	Namespace, Name, BackendURL, Project, PulumiStack string
}

// HistoryEntry is one parsed history file, reduced to the stored fields.
type HistoryEntry struct {
	Key, Bucket, Namespace, StackName string
	Type                              RunType  // mapped: up, refresh or destroy
	State                             RunState // succeeded or failed
	StartedAt, EndedAt                time.Time
	Commit                            string
	Counts                            map[string]int64
}

// S3Stacks returns active stacks with an s3:// backend and a known project and pulumi stack.
func (s *Store) S3Stacks(ctx context.Context) ([]S3Stack, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT namespace, name, backend_url, project, pulumi_stack FROM stacks
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

// NewestHistoryKey returns the greatest stored key in bucket that starts with keyPrefix, or ""
// when none: the StartAfter cursor for listing that prefix.
func (s *Store) NewestHistoryKey(ctx context.Context, bucket, keyPrefix string) (string, error) {
	// keyPrefix is a literal: escape LIKE wildcards before appending %.
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(keyPrefix) + "%"
	var key *string
	err := s.pool.QueryRow(ctx, `SELECT max(key) FROM s3_history
		WHERE bucket = $1 AND key LIKE $2`, bucket, like).Scan(&key)
	if err != nil {
		return "", fmt.Errorf("newest history key: %w", err)
	}
	if key == nil {
		return "", nil
	}
	return *key, nil
}

// InsertHistory stores e as pending; an existing key is left untouched (inserted false).
func (s *Store) InsertHistory(ctx context.Context, e HistoryEntry, seenAt time.Time) (
	bool, error) {
	if e.Key == "" || e.Type == "" || e.State == "" || e.Namespace == "" || e.StackName == "" {
		panic("invariant violated: incomplete history entry")
	}
	counts, err := json.Marshal(e.Counts)
	if err != nil {
		panic("invariant violated: marshal counts: " + err.Error())
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO s3_history (key, bucket, namespace, stack_name, type, state, started_at,
		                        ended_at, commit, counts, seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (key) DO NOTHING`,
		e.Key, e.Bucket, e.Namespace, e.StackName, e.Type, e.State, e.StartedAt, e.EndedAt,
		e.Commit, counts, seenAt)
	if err != nil {
		return false, fmt.Errorf("insert history %s: %w", e.Key, err)
	}
	return tag.RowsAffected() == 1, nil
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
		SELECT key, bucket, namespace, stack_name, type, state, started_at, ended_at, commit, counts
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
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.commit_source FROM runs r
		WHERE r.namespace = $1 AND r.stack_name = $2 AND r.type = $3 AND r.state = $4
		  AND r.started_at BETWEEN $5::timestamptz - make_interval(secs => $6)
		                       AND $5::timestamptz + make_interval(secs => $6)
		  AND NOT EXISTS (SELECT 1 FROM run_changes c WHERE c.run_id = r.id AND c.source = 's3')
		LIMIT 2`, e.Namespace, e.StackName, e.Type, e.State, e.StartedAt, linkWindow.Seconds())
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
		err = setLinkState(ctx, tx, e.Key, "ambiguous", 0)
	case len(cands) == 1:
		outcome, runID = outcomeLinked, cands[0].id
		err = linkRun(ctx, tx, e, runID, cands[0].source)
	case e.EndedAt.Before(now.Add(-importGrace)):
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
	if e.Commit != "" && (src == "" || src == CommitSourceStack) {
		if _, err := tx.Exec(ctx, `UPDATE runs SET commit = $2, commit_source = 'history'
			WHERE id = $1`, runID, e.Commit); err != nil {
			return fmt.Errorf("upgrade commit: %w", err)
		}
	}
	return setLinkState(ctx, tx, e.Key, "linked", runID)
}

func importRun(ctx context.Context, tx pgx.Tx, e HistoryEntry) (int64, error) {
	name := "s3:" + strings.TrimSuffix(path.Base(e.Key), ".history.json")
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
	return id, setLinkState(ctx, tx, e.Key, "imported", id)
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

func setLinkState(ctx context.Context, tx pgx.Tx, key, state string, runID int64) error {
	var id *int64
	if runID != 0 {
		id = &runID
	}
	if _, err := tx.Exec(ctx, `UPDATE s3_history SET link_state = $2, run_id = $3 WHERE key = $1`,
		key, state, id); err != nil {
		return fmt.Errorf("set link state: %w", err)
	}
	return nil
}

func scanHistoryEntry(row pgx.CollectableRow) (HistoryEntry, error) {
	var e HistoryEntry
	err := row.Scan(&e.Key, &e.Bucket, &e.Namespace, &e.StackName, &e.Type, &e.State,
		&e.StartedAt, &e.EndedAt, &e.Commit, &e.Counts)
	return e, err
}
