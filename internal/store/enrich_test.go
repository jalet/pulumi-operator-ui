package store

import (
	"testing"
	"time"
)

func TestMigrationAllowsImportType(t *testing.T) {
	s, _ := newTestStore(t)
	r := runAt("ns", "s", "imp", RunTypeImport, _t0)
	mustUpsert(t, s, r)
	if got := getRunByName(t, s, "ns", "imp"); got.Type != RunTypeImport {
		t.Fatalf("type = %q", got.Type)
	}
}

func TestMigrationResetsCursors(t *testing.T) {
	s, _ := newTestStore(t)
	var n int
	must(t, s.pool.QueryRow(t.Context(), `SELECT count(*) FROM s3_cursors`).Scan(&n))
	if n != 0 {
		t.Fatalf("cursors = %d after migrating, want 0", n)
	}
}

func TestStackRepoURLStoredAndJoined(t *testing.T) {
	s, _ := newTestStore(t)
	must(t, s.UpsertStack(t.Context(), Stack{Namespace: "ns", Name: "s", UpdatedAt: time.Now(),
		RepoURL: "git@github.com:o/r.git"}))
	mustUpsert(t, s, runAt("ns", "s", "u1", RunTypeUp, _t0))
	st, err := s.GetStack(t.Context(), "ns", "s")
	must(t, err)
	if st.RepoURL != "git@github.com:o/r.git" {
		t.Errorf("stack repo = %q", st.RepoURL)
	}
	r, err := s.GetRun(t.Context(), getRunByName(t, s, "ns", "u1").ID)
	must(t, err)
	if r.StackRepoURL != "git@github.com:o/r.git" {
		t.Errorf("run stack repo = %q", r.StackRepoURL)
	}
}

func TestDefaultTypesIncludeImport(t *testing.T) {
	found := false
	for _, typ := range DefaultRunTypes {
		found = found || typ == RunTypeImport
	}
	if !found {
		t.Fatal("import is not in the default timeline types")
	}
}
