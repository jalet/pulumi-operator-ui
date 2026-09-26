package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StateChangeTypes are the run types that change stack state and get a place on the rail.
var StateChangeTypes = []RunType{RunTypeUp, RunTypeRefresh, RunTypeDestroy, RunTypeImport}

// previewsPageMax bounds the previews one timeline page returns; a var so tests can shrink it.
var previewsPageMax = 1000

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
	if len(previews) == previewsPageMax {
		// The window held more previews than one page shows: end the page at the oldest
		// preview returned, keep only the changes newer than it, and continue from there.
		oldest := previews[len(previews)-1]
		next = &Cursor{At: runSortTime(oldest), ID: oldest.ID}
		kept := changes[:0]
		for _, c := range changes {
			if newerThan(c, *next) {
				kept = append(kept, c)
			}
		}
		changes = kept
	}
	return changes, previews, next, nil
}

func runSortTime(r Run) time.Time {
	if r.StartedAt != nil {
		return *r.StartedAt
	}
	return r.ObservedAt
}

// newerThan reports whether r sorts after cursor c in the timeline's (time, id) order.
func newerThan(r Run, c Cursor) bool {
	t := runSortTime(r)
	return t.After(c.At) || (t.Equal(c.At) && r.ID > c.ID)
}

// TimelineNewerAnchor finds the page of limit state changes just newer than after, as the
// before cursor that ListTimeline takes: the (limit+1)th state change newer than after.
// Anchoring on the change that starts the next newer page keeps the page boundaries, and so
// the previews each page shows, the same as paging older. found is false when the changes
// newer than after fit on the latest page.
func (s *Store) TimelineNewerAnchor(ctx context.Context, namespace, name string, after Cursor,
	limit int) (anchor Cursor, found bool, err error) {
	assert(limit > 0 && limit <= RunsPageMax, "runs limit")
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(started_at, observed_at), id FROM runs
		WHERE namespace = $1 AND stack_name = $2 AND type = ANY($3::text[])
		  AND (COALESCE(started_at, observed_at), id) > ($4, $5)
		ORDER BY COALESCE(started_at, observed_at), id
		OFFSET $6 LIMIT 1`, namespace, name, typeStrings(StateChangeTypes), after.At, after.ID,
		limit).Scan(&anchor.At, &anchor.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, false, nil
	}
	if err != nil {
		return Cursor{}, false, fmt.Errorf("timeline newer anchor: %w", err)
	}
	return anchor, true, nil
}
