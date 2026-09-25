package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
