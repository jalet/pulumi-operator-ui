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
	// The neighbouring runs on the same workspace, which bound this run's slice of the log.
	PrevEnded, NextStarted *time.Time
}

// PendingLogs returns up to limit runs waiting for log capture, oldest end first.
func (s *Store) PendingLogs(ctx context.Context, limit int) ([]LogJob, error) {
	assert(limit > 0, "pending logs limit")
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.namespace, r.stack_name, r.started_at, r.ended_at,
		       (SELECT max(p.ended_at) FROM runs p
		        WHERE p.namespace = r.namespace AND p.stack_name = r.stack_name
		          AND p.id <> r.id AND p.ended_at <= r.started_at),
		       (SELECT min(n.started_at) FROM runs n
		        WHERE n.namespace = r.namespace AND n.stack_name = r.stack_name
		          AND n.id <> r.id AND n.started_at >= r.ended_at)
		FROM runs r WHERE r.log_status = 'pending' ORDER BY r.ended_at, r.id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("pending logs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogJob, error) {
		var j LogJob
		return j, r.Scan(&j.RunID, &j.Namespace, &j.StackName, &j.StartedAt, &j.EndedAt,
			&j.PrevEnded, &j.NextStarted)
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
		summary := make([]ResourceRef, 0, 3)
		for _, lr := range resources[:min(3, len(resources))] {
			summary = append(summary, ResourceRef{Type: lr.Type, Name: lr.Name})
		}
		sm, err := json.Marshal(summary)
		if err != nil {
			panic("invariant violated: marshal summary: " + err.Error())
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_changes (run_id, source, counts, resources, summary, resource_total)
			VALUES ($1, 'log', $2, $3, $4, $5)
			ON CONFLICT (run_id, source) DO UPDATE
			SET counts = EXCLUDED.counts, resources = EXCLUDED.resources,
			    summary = EXCLUDED.summary, resource_total = EXCLUDED.resource_total`,
			runID, c, r, sm, len(resources)); err != nil {
			return fmt.Errorf("save log %d: changes: %w", runID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save log %d: commit: %w", runID, err)
	}
	s.pub.Publish(events.Event{Kind: events.KindRun, Namespace: ns, Stack: stack, RunID: runID})
	// Folded previews have no per-run fragment, so the stack's timeline must re-render for a
	// capture that finds drift to show.
	s.pub.Publish(events.Event{Kind: events.KindStack, Namespace: ns, Stack: stack})
	return nil
}
