package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// LogJob is a finished run whose engine log has not been captured yet.
type LogJob struct {
	RunID                int64
	Namespace, StackName string
	StartedAt, EndedAt   time.Time
}

// PendingLogs returns up to limit runs waiting for log capture, oldest end first.
func (s *Store) PendingLogs(ctx context.Context, limit int) ([]LogJob, error) {
	assert(limit > 0, "pending logs limit")
	rows, err := s.pool.Query(ctx, `
		SELECT id, namespace, stack_name, started_at, ended_at FROM runs
		WHERE log_status = 'pending' ORDER BY ended_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("pending logs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogJob, error) {
		var j LogJob
		return j, r.Scan(&j.RunID, &j.Namespace, &j.StackName, &j.StartedAt, &j.EndedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("pending logs: %w", err)
	}
	return jobs, nil
}

// SaveLog records a capture outcome. For LogStatusCaptured it stores counts and resources as
// the run's source 'log' changes, and truncated when a cap cut the result short; for
// LogStatusUnavailable it only sets the status.
func (s *Store) SaveLog(ctx context.Context, runID int64, status string,
	counts map[string]int64, resources []LogResource, truncated bool) error {
	assert(status == LogStatusCaptured || status == LogStatusUnavailable, "log status")
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save log %d: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	var ns, stack string
	err = tx.QueryRow(ctx, `UPDATE runs SET log_status = $2, log_truncated = $3 WHERE id = $1
		RETURNING namespace, stack_name`, runID, status,
		truncated && status == LogStatusCaptured).Scan(&ns, &stack)
	if err != nil {
		return fmt.Errorf("save log %d: %w", runID, err)
	}
	if status == LogStatusCaptured {
		if counts == nil {
			counts = map[string]int64{}
		}
		if resources == nil {
			resources = []LogResource{}
		}
		c, err := json.Marshal(counts)
		if err != nil {
			panic("invariant violated: marshal counts: " + err.Error())
		}
		r, err := json.Marshal(resources)
		if err != nil {
			panic("invariant violated: marshal resources: " + err.Error())
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_changes (run_id, source, counts, resources) VALUES ($1, 'log', $2, $3)
			ON CONFLICT (run_id, source) DO UPDATE
			SET counts = EXCLUDED.counts, resources = EXCLUDED.resources`, runID, c, r); err != nil {
			return fmt.Errorf("save log %d: changes: %w", runID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save log %d: commit: %w", runID, err)
	}
	s.pub.Publish(events.Event{Kind: events.KindRun, Namespace: ns, Stack: stack, RunID: runID})
	return nil
}
