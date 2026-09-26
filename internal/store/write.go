package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// Each column's merged value on conflict. The same expression sets the column and decides
// whether the row changed; observed_at is left out of the check, so a later observation of
// the same state writes and publishes nothing.
const (
	_mergeUID    = `COALESCE(EXCLUDED.uid, runs.uid)`
	_mergeCommit = `CASE WHEN EXCLUDED.commit_source = 'update' AND EXCLUDED.commit <> ''
	                       THEN EXCLUDED.commit
	                     WHEN runs.commit = '' THEN EXCLUDED.commit
	                     ELSE runs.commit END`
	_mergeCommitSource = `CASE WHEN EXCLUDED.commit_source = 'update' AND EXCLUDED.commit <> ''
	                       THEN 'update'
	                     WHEN runs.commit = '' THEN EXCLUDED.commit_source
	                     ELSE runs.commit_source END`
	_mergeState = `CASE WHEN runs.state IN ('succeeded', 'failed') THEN runs.state
	                    ELSE EXCLUDED.state END`
	_mergeMessage = `CASE WHEN EXCLUDED.message <> '' THEN EXCLUDED.message ELSE runs.message END`
	_mergeStarted = `COALESCE(runs.started_at, EXCLUDED.started_at)`
	_mergeEnded   = `COALESCE(runs.ended_at, EXCLUDED.ended_at)`
	// A run queues for log capture once the merged row is finished with both times known.
	_mergeLogStatus = `CASE WHEN runs.log_status <> '' THEN runs.log_status
	                        WHEN (` + _mergeState + `) IN ('succeeded', 'failed')
	                             AND (` + _mergeUID + `) IS NOT NULL
	                             AND (` + _mergeStarted + `) IS NOT NULL
	                             AND (` + _mergeEnded + `) IS NOT NULL
	                        THEN 'pending' ELSE '' END`
)

const _upsertRun = `
INSERT INTO runs (namespace, update_name, uid, stack_name, type, commit, commit_source,
                  state, message, started_at, ended_at, observed_at, log_status)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, $12,
        CASE WHEN $8::text IN ('succeeded', 'failed') AND $3::text <> ''
                  AND $10::timestamptz IS NOT NULL
                  AND $11::timestamptz IS NOT NULL
             THEN 'pending' ELSE '' END)
ON CONFLICT (namespace, update_name) DO UPDATE SET
    uid = ` + _mergeUID + `, stack_name = EXCLUDED.stack_name, type = EXCLUDED.type,
    commit = ` + _mergeCommit + `, commit_source = ` + _mergeCommitSource + `,
    state = ` + _mergeState + `, message = ` + _mergeMessage + `,
    started_at = ` + _mergeStarted + `, ended_at = ` + _mergeEnded + `,
    observed_at = LEAST(runs.observed_at, EXCLUDED.observed_at),
    log_status = ` + _mergeLogStatus + `
WHERE (runs.uid, runs.stack_name, runs.type, runs.commit, runs.commit_source, runs.state,
       runs.message, runs.started_at, runs.ended_at, runs.log_status)
  IS DISTINCT FROM (` + _mergeUID + `, EXCLUDED.stack_name, EXCLUDED.type, ` + _mergeCommit +
	`, ` + _mergeCommitSource + `, ` + _mergeState + `, ` + _mergeMessage + `, ` +
	_mergeStarted + `, ` + _mergeEnded + `, ` + _mergeLogStatus + `)
RETURNING id`

// _backfillRun merges a run seen through Stack.status.lastUpdate (always terminal). It
// completes a run still recorded as pending or running, which happens when its Update was
// GC'd while the app was down, and upgrades an approximate commit to the exact one. It
// never changes a terminal row otherwise; RETURNING yields no row when nothing changed.
const _backfillRun = `
INSERT INTO runs (namespace, update_name, uid, stack_name, type, commit, commit_source,
                  state, message, started_at, ended_at, observed_at)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (namespace, update_name) DO UPDATE SET
    state         = CASE WHEN runs.state IN ('succeeded', 'failed') THEN runs.state
                         ELSE EXCLUDED.state END,
    message       = CASE WHEN runs.state IN ('succeeded', 'failed') OR EXCLUDED.message = ''
                           THEN runs.message
                         ELSE EXCLUDED.message END,
    commit        = CASE WHEN EXCLUDED.commit_source = 'update' AND EXCLUDED.commit <> ''
                              AND runs.commit_source <> 'update'
                           THEN EXCLUDED.commit
                         ELSE runs.commit END,
    commit_source = CASE WHEN EXCLUDED.commit_source = 'update' AND EXCLUDED.commit <> ''
                              AND runs.commit_source <> 'update'
                           THEN 'update'
                         ELSE runs.commit_source END
WHERE runs.state NOT IN ('succeeded', 'failed')
   OR (EXCLUDED.commit_source = 'update' AND EXCLUDED.commit <> ''
       AND runs.commit_source <> 'update')
RETURNING id`

// UpsertRun records a run seen through its Update. It is idempotent and converges:
// terminal states never regress, the first non-empty commit wins unless the new one comes
// from the Update itself, and the first observed start and end times are kept, as is the
// earliest observed_at, which orders runs that have no start time.
func (s *Store) UpsertRun(ctx context.Context, r Run) error {
	assertRun(r)
	var id int64
	err := s.pool.QueryRow(ctx, _upsertRun, runArgs(r)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing changed
	}
	if err != nil {
		return fmt.Errorf("upsert run %s/%s: %w", r.Namespace, r.UpdateName, err)
	}
	s.publishRun(r, id)
	return nil
}

// BackfillRun records a run from Stack.status.lastUpdate. A new row is inserted; an
// existing one only converges (see _backfillRun), because the Update carries better data
// than the Stack summary.
func (s *Store) BackfillRun(ctx context.Context, r Run) error {
	assertRun(r)
	var id int64
	err := s.pool.QueryRow(ctx, _backfillRun, runArgs(r)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert run %s/%s: %w", r.Namespace, r.UpdateName, err)
	}
	s.publishRun(r, id)
	return nil
}

// UpsertStack records the current stack status and clears any soft delete.
func (s *Store) UpsertStack(ctx context.Context, st Stack) error {
	assert(st.Namespace != "" && st.Name != "", "stack key")
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("upsert stack: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	var deletedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT deleted_at FROM stacks WHERE namespace = $1 AND name = $2
		FOR UPDATE`, st.Namespace, st.Name).Scan(&deletedAt)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("upsert stack %s/%s: %w", st.Namespace, st.Name, err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO stacks (namespace, name, ready, reconciling, stalled, last_commit, updated_at,
		                    backend_url, project, pulumi_stack, preview, repo_url, watches)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (namespace, name) DO UPDATE SET
		    ready = EXCLUDED.ready, reconciling = EXCLUDED.reconciling,
		    stalled = EXCLUDED.stalled, last_commit = EXCLUDED.last_commit,
		    updated_at = EXCLUDED.updated_at, deleted_at = NULL,
		    backend_url = EXCLUDED.backend_url, project = EXCLUDED.project,
		    pulumi_stack = EXCLUDED.pulumi_stack, preview = EXCLUDED.preview,
		    repo_url = EXCLUDED.repo_url, watches = EXCLUDED.watches
		WHERE (stacks.ready, stacks.reconciling, stacks.stalled, stacks.last_commit,
		       stacks.deleted_at, stacks.backend_url, stacks.project, stacks.pulumi_stack,
		       stacks.preview, stacks.repo_url, stacks.watches)
		  IS DISTINCT FROM (EXCLUDED.ready, EXCLUDED.reconciling, EXCLUDED.stalled,
		       EXCLUDED.last_commit, NULL::timestamptz, EXCLUDED.backend_url, EXCLUDED.project,
		       EXCLUDED.pulumi_stack, EXCLUDED.preview, EXCLUDED.repo_url, EXCLUDED.watches)`,
		st.Namespace, st.Name, st.Ready, st.Reconciling, st.Stalled, st.LastCommit, st.UpdatedAt,
		st.BackendURL, st.Project, st.PulumiStack, st.Preview, st.RepoURL, st.Watches)
	if err != nil {
		return fmt.Errorf("upsert stack %s/%s: %w", st.Namespace, st.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("upsert stack %s/%s: commit: %w", st.Namespace, st.Name, err)
	}
	if existed && deletedAt == nil && tag.RowsAffected() == 0 {
		return nil // only updated_at would have moved: nothing to show
	}
	kind := events.KindStack
	if !existed || deletedAt != nil {
		kind = events.KindStackSet
	}
	s.pub.Publish(events.Event{Kind: kind, Namespace: st.Namespace, Stack: st.Name})
	return nil
}

// SetStackS3Status records the outcome of polling one Stack's history: errMsg "" on success,
// which also stamps s3_checked_at. An unknown stack is a no-op.
func (s *Store) SetStackS3Status(ctx context.Context, namespace, name, errMsg string,
	at time.Time) error {
	q := `UPDATE stacks SET s3_error = $3 WHERE namespace = $1 AND name = $2`
	args := []any{namespace, name, truncate(errMsg, 512)}
	if errMsg == "" {
		q = `UPDATE stacks SET s3_error = '', s3_checked_at = $3
		     WHERE namespace = $1 AND name = $2`
		args = []any{namespace, name, at}
	}
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("set s3 status %s/%s: %w", namespace, name, err)
	}
	return nil
}

// MarkStackDeleted soft-deletes a stack. Its runs stay and follow normal retention.
func (s *Store) MarkStackDeleted(ctx context.Context, namespace, name string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE stacks SET deleted_at = $3
		WHERE namespace = $1 AND name = $2 AND deleted_at IS NULL`, namespace, name, at)
	if err != nil {
		return fmt.Errorf("mark stack %s/%s deleted: %w", namespace, name, err)
	}
	if tag.RowsAffected() > 0 {
		s.pub.Publish(events.Event{Kind: events.KindStackSet, Namespace: namespace, Stack: name})
	}
	return nil
}

func (s *Store) publishRun(r Run, id int64) {
	s.pub.Publish(events.Event{Kind: events.KindRun, Namespace: r.Namespace, Stack: r.StackName,
		RunID: id})
	// The stack list shows each stack's latest runs, so its row must refresh too.
	s.pub.Publish(events.Event{Kind: events.KindStack, Namespace: r.Namespace, Stack: r.StackName})
}

func runArgs(r Run) []any {
	return []any{r.Namespace, r.UpdateName, r.UID, r.StackName, r.Type, r.Commit,
		r.CommitSource, r.State, r.Message, r.StartedAt, r.EndedAt, r.ObservedAt}
}

func assertRun(r Run) {
	assert(r.Namespace != "" && r.UpdateName != "", "run key")
	assert(r.StackName != "", "run stack")
	assert(r.Type != "" && r.State != "", "run enums")
}

// StackKey identifies a stack.
type StackKey struct {
	Namespace string
	Name      string
}

// SweepStacks soft-deletes active stacks that are absent from present, the full set of
// Stacks the watch can currently see. Only rows last written before cutoff are touched,
// so a stack upserted by a reconcile racing the sweep is never swept. It returns the
// number of stacks marked deleted.
func (s *Store) SweepStacks(ctx context.Context, present []StackKey, cutoff, at time.Time) (
	int64, error) {
	namespaces := make([]string, len(present))
	names := make([]string, len(present))
	for i, k := range present {
		namespaces[i], names[i] = k.Namespace, k.Name
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE stacks SET deleted_at = $1
		WHERE deleted_at IS NULL AND updated_at < $2
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest($3::text[], $4::text[]) AS p(namespace, name)
		      WHERE p.namespace = stacks.namespace AND p.name = stacks.name)`,
		at, cutoff, namespaces, names)
	if err != nil {
		return 0, fmt.Errorf("sweep stacks: %w", err)
	}
	if tag.RowsAffected() > 0 {
		s.pub.Publish(events.Event{Kind: events.KindStackSet})
	}
	return tag.RowsAffected(), nil
}
