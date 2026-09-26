package store

import (
	"context"
	"fmt"
	"time"
)

// Bounds keep one prune tick from holding locks or running unbounded: 1000 rows per
// statement, 1000 statements per table per tick (1M rows). A backlog beyond that is
// reported as an error and finished on the next tick. Variables so tests can shrink them.
var (
	pruneBatchRows  = 1000
	pruneBatchesMax = 1000
)

// PruneResult counts the rows one Prune call removed.
type PruneResult struct {
	Runs       int64
	AuthEvents int64
	Stacks     int64
	History    int64 // s3_history rows
}

// Prune deletes runs and auth events older than their retention, then purges
// soft-deleted stacks that have no runs left.
func (s *Store) Prune(ctx context.Context, now time.Time, runs, auth time.Duration) (
	PruneResult, error) {
	var res PruneResult
	var err error
	res.Runs, err = s.pruneBatched(ctx, "runs", `
		WITH doomed AS (SELECT id FROM runs WHERE COALESCE(ended_at, observed_at) < $1
		                ORDER BY id LIMIT $2)
		DELETE FROM runs WHERE id IN (SELECT id FROM doomed)`, now.Add(-runs))
	if err != nil {
		return res, err
	}
	res.History, err = s.pruneBatched(ctx, "s3_history", `
		WITH doomed AS (SELECT bucket, key FROM s3_history WHERE ended_at < $1
		                ORDER BY bucket, key LIMIT $2)
		DELETE FROM s3_history WHERE (bucket, key) IN (SELECT bucket, key FROM doomed)`, now.Add(-runs))
	if err != nil {
		return res, err
	}
	res.AuthEvents, err = s.pruneBatched(ctx, "auth_events", `
		WITH doomed AS (SELECT id FROM auth_events WHERE at < $1 ORDER BY id LIMIT $2)
		DELETE FROM auth_events WHERE id IN (SELECT id FROM doomed)`, now.Add(-auth))
	if err != nil {
		return res, err
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM stacks s WHERE deleted_at IS NOT NULL AND NOT EXISTS (
		    SELECT 1 FROM runs r WHERE r.namespace = s.namespace AND r.stack_name = s.name)`)
	if err != nil {
		return res, fmt.Errorf("prune stacks: %w", err)
	}
	res.Stacks = tag.RowsAffected()
	return res, nil
}

// RunPruner prunes now and then every interval until ctx ends. Errors are logged, never
// returned, so a failing prune never stops the process.
func (s *Store) RunPruner(ctx context.Context, every, runs, auth time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		res, err := s.Prune(ctx, time.Now(), runs, auth)
		if err != nil {
			s.log.Error().Err(err).Msg("prune")
		} else {
			s.log.Info().Int64("runs", res.Runs).Int64("auth_events", res.AuthEvents).
				Int64("stacks", res.Stacks).Int64("history", res.History).Msg("pruned")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (s *Store) pruneBatched(ctx context.Context, table, query string, cutoff time.Time) (
	int64, error) {
	var total int64
	for batch := 1; batch <= pruneBatchesMax; batch++ {
		tag, err := s.pool.Exec(ctx, query, cutoff, pruneBatchRows)
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", table, err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(pruneBatchRows) {
			return total, nil
		}
	}
	return total, fmt.Errorf("prune %s: batch cap (%d x %d rows) reached", table,
		pruneBatchesMax, pruneBatchRows)
}
