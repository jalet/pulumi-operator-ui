package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// RunsPageMax is the largest page ListRuns serves.
	RunsPageMax = 200
	// A cluster with more than 5000 stacks is outside this tool's design; fail loudly
	// rather than show a silently truncated list.
	stacksMax = 5000
)

// RunBrief is the summary of a stack's latest run of one type.
type RunBrief struct {
	ID        int64
	State     RunState
	At        time.Time // COALESCE(ended_at, started_at, observed_at)
	Commit    string
	StartedAt *time.Time
	EndedAt   *time.Time
}

// StackSummary is a stack with its latest preview and up.
type StackSummary struct {
	Stack
	LastPreview *RunBrief
	LastUp      *RunBrief
}

// Cursor is a keyset position in a stack's run timeline.
type Cursor struct {
	At time.Time
	ID int64
}

const _stackSummarySelect = `
SELECT s.namespace, s.name, s.ready, s.reconciling, s.stalled, s.last_commit, s.updated_at,
       s.deleted_at, s.backend_url, s.project, s.pulumi_stack, s.s3_error, s.s3_checked_at,
       p.id, p.state, p.at, p.commit, p.started_at, p.ended_at,
       u.id, u.state, u.at, u.commit, u.started_at, u.ended_at
FROM stacks s
LEFT JOIN LATERAL (
    SELECT id, state, commit, started_at, ended_at,
           COALESCE(ended_at, started_at, observed_at) AS at
    FROM runs r
    WHERE r.namespace = s.namespace AND r.stack_name = s.name AND r.type = 'preview'
    ORDER BY COALESCE(r.started_at, r.observed_at) DESC, r.id DESC LIMIT 1) p ON true
LEFT JOIN LATERAL (
    SELECT id, state, commit, started_at, ended_at,
           COALESCE(ended_at, started_at, observed_at) AS at
    FROM runs r
    WHERE r.namespace = s.namespace AND r.stack_name = s.name AND r.type = 'up'
    ORDER BY COALESCE(r.started_at, r.observed_at) DESC, r.id DESC LIMIT 1) u ON true`

const _runSelect = `
SELECT id, namespace, update_name, COALESCE(uid, ''), stack_name, type, commit, commit_source,
       state, message, started_at, ended_at, observed_at, c.counts, log_status, lc.counts,
       lc.resources
FROM runs
LEFT JOIN run_changes c ON c.run_id = runs.id AND c.source = 's3'
LEFT JOIN run_changes lc ON lc.run_id = runs.id AND lc.source = 'log'`

// ListStacks returns active (not soft-deleted) stacks ordered by namespace and name.
func (s *Store) ListStacks(ctx context.Context) ([]StackSummary, error) {
	rows, err := s.pool.Query(ctx, _stackSummarySelect+`
		WHERE s.deleted_at IS NULL ORDER BY s.namespace, s.name LIMIT $1`, stacksMax+1)
	if err != nil {
		return nil, fmt.Errorf("list stacks: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanStackSummary)
	if err != nil {
		return nil, fmt.Errorf("list stacks: %w", err)
	}
	if len(out) > stacksMax {
		return nil, fmt.Errorf("list stacks: more than %d stacks", stacksMax)
	}
	return out, nil
}

// GetStack returns one stack, including a soft-deleted one so its timeline stays reachable.
func (s *Store) GetStack(ctx context.Context, namespace, name string) (StackSummary, error) {
	rows, err := s.pool.Query(ctx, _stackSummarySelect+`
		WHERE s.namespace = $1 AND s.name = $2`, namespace, name)
	if err != nil {
		return StackSummary{}, fmt.Errorf("get stack: %w", err)
	}
	st, err := pgx.CollectExactlyOneRow(rows, scanStackSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		return StackSummary{}, ErrNotFound
	}
	if err != nil {
		return StackSummary{}, fmt.Errorf("get stack: %w", err)
	}
	return st, nil
}

// RunFilter narrows a stack's timeline. Empty Types means DefaultRunTypes.
type RunFilter struct {
	Types []RunType
}

// Run type sets. The default hides previews, which PKO runs on every resync.
var (
	DefaultRunTypes = []RunType{RunTypeUp, RunTypeRefresh, RunTypeDestroy}
	AllRunTypes     = []RunType{RunTypeUp, RunTypeRefresh, RunTypeDestroy, RunTypePreview}
)

// EffectiveTypes returns f.Types, or DefaultRunTypes when empty.
func (f RunFilter) EffectiveTypes() []RunType {
	if len(f.Types) == 0 {
		return DefaultRunTypes
	}
	return f.Types
}

// StackStats summarizes a stack's runs since a point in time.
type StackStats struct {
	Total, Succeeded, Failed int64 // runs of the selected types
	HiddenPreviews           int64 // previews not selected
}

// ListRuns returns one page of a stack's runs matching f, newest first. The next cursor
// is nil on the last page.
func (s *Store) ListRuns(ctx context.Context, namespace, name string, f RunFilter,
	before *Cursor, limit int) ([]Run, *Cursor, error) {
	assert(limit > 0 && limit <= RunsPageMax, "runs limit")
	var at *time.Time
	var id int64
	if before != nil {
		at, id = &before.At, before.ID
	}
	rows, err := s.pool.Query(ctx, _runSelect+`
		WHERE namespace = $1 AND stack_name = $2
		  AND ($3::timestamptz IS NULL OR (COALESCE(started_at, observed_at), id) < ($3, $4))
		  AND type = ANY($6::text[])
		ORDER BY COALESCE(started_at, observed_at) DESC, id DESC
		LIMIT $5`, namespace, name, at, id, limit+1, typeStrings(f.EffectiveTypes()))
	if err != nil {
		return nil, nil, fmt.Errorf("list runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, nil, fmt.Errorf("list runs: %w", err)
	}
	if len(runs) <= limit {
		return runs, nil, nil
	}
	runs = runs[:limit]
	last := runs[limit-1]
	next := &Cursor{At: last.ObservedAt, ID: last.ID}
	if last.StartedAt != nil {
		next.At = *last.StartedAt
	}
	return runs, next, nil
}

// StackStats counts one stack's runs of the selected types started since the given time,
// plus the previews the filter hides.
func (s *Store) StackStats(ctx context.Context, namespace, name string, f RunFilter,
	since time.Time) (StackStats, error) {
	var st StackStats
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE type = ANY($3::text[])),
		       count(*) FILTER (WHERE type = ANY($3::text[]) AND state = 'succeeded'),
		       count(*) FILTER (WHERE type = ANY($3::text[]) AND state = 'failed'),
		       count(*) FILTER (WHERE type = 'preview' AND NOT ('preview' = ANY($3::text[])))
		FROM runs
		WHERE namespace = $1 AND stack_name = $2 AND COALESCE(started_at, observed_at) >= $4`,
		namespace, name, typeStrings(f.EffectiveTypes()), since).
		Scan(&st.Total, &st.Succeeded, &st.Failed, &st.HiddenPreviews)
	if err != nil {
		return StackStats{}, fmt.Errorf("stack stats: %w", err)
	}
	return st, nil
}

func typeStrings(ts []RunType) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// GetRun returns one run by id.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	rows, err := s.pool.Query(ctx, _runSelect+` WHERE id = $1`, id)
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", err)
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

func scanRun(row pgx.CollectableRow) (Run, error) {
	var r Run
	var counts, logCounts, resources []byte
	err := row.Scan(&r.ID, &r.Namespace, &r.UpdateName, &r.UID, &r.StackName, &r.Type,
		&r.Commit, &r.CommitSource, &r.State, &r.Message, &r.StartedAt, &r.EndedAt,
		&r.ObservedAt, &counts, &r.LogStatus, &logCounts, &resources)
	if err != nil {
		return r, err
	}
	for _, f := range []struct {
		raw  []byte
		into any
	}{{counts, &r.Changes}, {logCounts, &r.LogChanges}, {resources, &r.Resources}} {
		if f.raw == nil {
			continue
		}
		if err := json.Unmarshal(f.raw, f.into); err != nil {
			return r, fmt.Errorf("decode run changes: %w", err)
		}
	}
	if len(r.Resources) == 0 {
		r.Resources = nil
	}
	return r, nil
}

func scanStackSummary(row pgx.CollectableRow) (StackSummary, error) {
	var (
		st   StackSummary
		p, u briefCols
	)
	err := row.Scan(&st.Namespace, &st.Name, &st.Ready, &st.Reconciling, &st.Stalled,
		&st.LastCommit, &st.UpdatedAt, &st.DeletedAt,
		&st.BackendURL, &st.Project, &st.PulumiStack, &st.S3Error, &st.S3CheckedAt,
		&p.id, &p.state, &p.at, &p.commit, &p.started, &p.ended,
		&u.id, &u.state, &u.at, &u.commit, &u.started, &u.ended)
	if err != nil {
		return StackSummary{}, err
	}
	st.LastPreview = p.brief()
	st.LastUp = u.brief()
	return st, nil
}

// briefCols receives one LEFT JOIN LATERAL run; every column is NULL when no run exists.
type briefCols struct {
	id             *int64
	state          *RunState
	at             *time.Time
	commit         *string
	started, ended *time.Time
}

func (c briefCols) brief() *RunBrief {
	if c.id == nil {
		return nil
	}
	if c.state == nil || c.at == nil || c.commit == nil {
		panic("invariant violated: run brief columns are NOT NULL when id is set")
	}
	return &RunBrief{ID: *c.id, State: *c.state, At: *c.at, Commit: *c.commit,
		StartedAt: c.started, EndedAt: c.ended}
}
