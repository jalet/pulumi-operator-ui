package store

import (
	"database/sql"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migratedTo returns a fresh database migrated up to version, its goose provider and URL.
func migratedTo(t *testing.T, version int64) (*sql.DB, *goose.Provider, string) {
	t.Helper()
	url := newDatabase(t)
	cfg, err := pgxpool.ParseConfig(url)
	must(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	sub, err := fs.Sub(_migrations, "migrations")
	must(t, err)
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	must(t, err)
	if _, err := p.UpTo(t.Context(), version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	return db, p, url
}

func TestMigrationBackfillsSummary(t *testing.T) {
	db, p, _ := migratedTo(t, 6)
	ctx := t.Context()
	var id int64
	must(t, db.QueryRowContext(ctx, `INSERT INTO runs (namespace, update_name, stack_name, type,
		state, observed_at) VALUES ('ns', 'u1', 's', 'up', 'succeeded', now()) RETURNING id`).Scan(&id))
	_, err := db.ExecContext(ctx, `INSERT INTO run_changes (run_id, source, resources) VALUES
		($1, 'log', '[{"type":"a:b:C","name":"one"},{"type":"a:b:C","name":"two"},
		  {"type":"a:b:C","name":"three"},{"type":"a:b:C","name":"four"}]')`, id)
	must(t, err)
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	var summary string
	var total int
	must(t, db.QueryRowContext(ctx, `SELECT summary::text, resource_total FROM run_changes
		WHERE run_id = $1`, id).Scan(&summary, &total))
	if total != 4 || summary != `[{"name": "one", "type": "a:b:C"}, {"name": "two", "type": "a:b:C"}, {"name": "three", "type": "a:b:C"}]` {
		t.Fatalf("summary %s total %d", summary, total)
	}
}

// The cursor reset in 00006 re-reads every key once; a cursor written before it is gone.
func TestMigrationResetsCursors(t *testing.T) {
	db, p, _ := migratedTo(t, 5)
	_, err := db.ExecContext(t.Context(), `INSERT INTO s3_cursors (bucket, prefix, last_key,
		updated_at) VALUES ('b', 'p/', 'p/k', now())`)
	must(t, err)
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	must(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM s3_cursors`).Scan(&n))
	if n != 0 {
		t.Fatalf("cursors = %d after 00006, want 0", n)
	}
}

// Every migration can be rolled back with data in place, including history-sourced commits.
func TestMigrationsRollBackWithData(t *testing.T) {
	_, p, url := migratedTo(t, 7)
	s, err := Open(t.Context(), Options{URL: url, Pub: &recordingPublisher{}})
	must(t, err)
	r := run("ns", "u1", RunStateSucceeded)
	r.Commit, r.CommitSource = "abc1234", CommitSourceHistory
	must(t, s.UpsertRun(t.Context(), r))
	s.Close()
	if _, err := p.DownTo(t.Context(), 0); err != nil {
		t.Fatalf("roll back: %v", err)
	}
}

func TestHistorySameKeyInTwoBuckets(t *testing.T) {
	s, _ := newTestStore(t)
	a, b := entry(_histKey, RunTypeUp, RunStateSucceeded, _t0), entry(_histKey, RunTypeUp, RunStateSucceeded, _t0)
	b.Bucket = "other"
	for _, e := range []HistoryEntry{a, b} {
		inserted, err := s.InsertHistory(t.Context(), e, _t0)
		must(t, err)
		if !inserted {
			t.Fatalf("bucket %s: not inserted, want its own row", e.Bucket)
		}
	}
}
