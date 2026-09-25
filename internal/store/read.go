package store

import (
	"context"
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
	ID     int64
	State  RunState
	At     time.Time // COALESCE(ended_at, started_at, observed_at)
	Commit string
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
       s.deleted_at, p.id, p.state, p.at, p.commit, u.id, u.state, u.at, u.commit
FROM stacks s
LEFT JOIN LATERAL (
    SELECT id, state, commit, COALESCE(ended_at, started_at, observed_at) AS at
    FROM runs r
    WHERE r.namespace = s.namespace AND r.stack_name = s.name AND r.type = 'preview'
    ORDER BY COALESCE(r.started_at, r.observed_at) DESC, r.id DESC LIMIT 1) p ON true
LEFT JOIN LATERAL (
    SELECT id, state, commit, COALESCE(ended_at, started_at, observed_at) AS at
    FROM runs r
    WHERE r.namespace = s.namespace AND r.stack_name = s.name AND r.type = 'up'
    ORDER BY COALESCE(r.started_at, r.observed_at) DESC, r.id DESC LIMIT 1) u ON true`

const _runSelect = `
SELECT id, namespace, update_name, COALESCE(uid, ''), stack_name, type, commit, commit_source,
       state, message, started_at, ended_at, observed_at
FROM runs`

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

// RunFilter narrows a stack's timeline.
type RunFilter struct {
	Previews bool // include preview runs; false shows only up, refresh and destroy
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
		  AND ($6 OR type <> 'preview')
		ORDER BY COALESCE(started_at, observed_at) DESC, id DESC
		LIMIT $5`, namespace, name, at, id, limit+1, f.Previews)
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
	err := row.Scan(&r.ID, &r.Namespace, &r.UpdateName, &r.UID, &r.StackName, &r.Type,
		&r.Commit, &r.CommitSource, &r.State, &r.Message, &r.StartedAt, &r.EndedAt,
		&r.ObservedAt)
	return r, err
}

func scanStackSummary(row pgx.CollectableRow) (StackSummary, error) {
	var (
		st             StackSummary
		pID, uID       *int64
		pState, uState *RunState
		pAt, uAt       *time.Time
		pCom, uCom     *string
	)
	err := row.Scan(&st.Namespace, &st.Name, &st.Ready, &st.Reconciling, &st.Stalled,
		&st.LastCommit, &st.UpdatedAt, &st.DeletedAt,
		&pID, &pState, &pAt, &pCom, &uID, &uState, &uAt, &uCom)
	if err != nil {
		return StackSummary{}, err
	}
	st.LastPreview = brief(pID, pState, pAt, pCom)
	st.LastUp = brief(uID, uState, uAt, uCom)
	return st, nil
}

func brief(id *int64, state *RunState, at *time.Time, commit *string) *RunBrief {
	if id == nil {
		return nil
	}
	if state == nil || at == nil || commit == nil {
		panic("invariant violated: run brief columns are NOT NULL when id is set")
	}
	return &RunBrief{ID: *id, State: *state, At: *at, Commit: *commit}
}
