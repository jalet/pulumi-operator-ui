package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StateChangeTypes are the run types that change stack state and get a place on the rail.
var StateChangeTypes = []RunType{RunTypeUp, RunTypeRefresh, RunTypeDestroy, RunTypeImport}

// previewsPageMax bounds the previews one timeline page returns.
const previewsPageMax = 1000

// ListTimeline returns one page of a stack's state changes, newest first, and the previews
// in the same stretch of time: newer than the page's oldest change, and older than the
// cursor (or unbounded on the first page). Page boundaries fall on state changes, so a
// preview is on exactly one page.
func (s *Store) ListTimeline(ctx context.Context, namespace, name string, before *Cursor,
	limit int) ([]Run, []Run, *Cursor, error) {
	changes, next, err := s.ListRuns(ctx, namespace, name, RunFilter{Types: StateChangeTypes},
		before, limit)
	if err != nil {
		return nil, nil, nil, err
	}
	var upper, lower *time.Time
	var upperID, lowerID int64
	if before != nil {
		upper, upperID = &before.At, before.ID
	}
	if next != nil {
		lower, lowerID = &next.At, next.ID
	}
	rows, err := s.pool.Query(ctx, _runSelect+`
		WHERE runs.namespace = $1 AND stack_name = $2 AND type = 'preview'
		  AND ($3::timestamptz IS NULL OR (COALESCE(started_at, observed_at), runs.id) < ($3, $4))
		  AND ($5::timestamptz IS NULL OR (COALESCE(started_at, observed_at), runs.id) > ($5, $6))
		ORDER BY COALESCE(started_at, observed_at) DESC, runs.id DESC
		LIMIT $7`, namespace, name, upper, upperID, lower, lowerID, previewsPageMax)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list timeline previews: %w", err)
	}
	previews, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list timeline previews: %w", err)
	}
	return changes, previews, next, nil
}
