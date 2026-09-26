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
	for _, typ := range StateChangeTypes {
		found = found || typ == RunTypeImport
	}
	if !found {
		t.Fatal("import is not in the default timeline types")
	}
}
func withOrigin(e HistoryEntry) HistoryEntry {
	e.ExecKind, e.ExecAgent = "cli", "agent-x"
	e.Message, e.VCSRepo = "chore: tidy", "github.com/o/r"
	return e
}

func TestImportCopiesOriginAndTitle(t *testing.T) {
	s, _ := newTestStore(t)
	e := withOrigin(entry(_histKey, RunTypeUp, RunStateSucceeded, _t0))
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	link(t, s, e.EndedAt.Add(16*time.Minute))
	r := getRunByName(t, s, "ns", "s3:dev-1790239744268661000")
	got, err := s.GetRun(t.Context(), r.ID)
	must(t, err)
	if got.ExecKind != "cli" || got.ExecAgent != "agent-x" || got.Title != "chore: tidy" ||
		got.VCSRepo != "github.com/o/r" {
		t.Fatalf("imported run = %+v", got)
	}
}

func TestLinkCopiesOriginButNoTitle(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, runAt("ns", "s", "u1", RunTypeUp, _t0))
	e := withOrigin(entry(_histKey, RunTypeUp, RunStateSucceeded, _t0))
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	link(t, s, _t0.Add(time.Minute))
	got, err := s.GetRun(t.Context(), getRunByName(t, s, "ns", "u1").ID)
	must(t, err)
	if got.ExecKind != "cli" || got.VCSRepo != "github.com/o/r" || got.Title != "" {
		t.Fatalf("linked run = %+v, want origin and repo without a title", got)
	}
}

// The cursor reset re-reads files already stored: new fields fill in, nothing else moves.
func TestInsertHistoryEnrichesWithoutRelinking(t *testing.T) {
	s, _ := newTestStore(t)
	e := entry(_histKey, RunTypeUp, RunStateSucceeded, _t0)
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	link(t, s, e.EndedAt.Add(16*time.Minute))
	before := countRuns(t, s)
	stateBefore, idBefore := historyRow(t, s, _histKey)

	inserted, err := s.InsertHistory(t.Context(), withOrigin(e), _t0.Add(time.Hour))
	must(t, err)
	if inserted {
		t.Error("re-read reported as a new entry")
	}
	if countRuns(t, s) != before {
		t.Error("re-read created a run")
	}
	if state, id := historyRow(t, s, _histKey); state != stateBefore || *id != *idBefore {
		t.Errorf("link changed: %s %v -> %s %v", stateBefore, *idBefore, state, *id)
	}
	got, err := s.GetRun(t.Context(), *idBefore)
	must(t, err)
	if got.ExecKind != "cli" || got.Title != "chore: tidy" {
		t.Errorf("imported run not enriched: %+v", got)
	}
	// A value already set is not overwritten by a later read.
	again := withOrigin(e)
	again.ExecKind = "other"
	if _, err := s.InsertHistory(t.Context(), again, _t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(t.Context(), *idBefore); got.ExecKind != "cli" {
		t.Errorf("exec kind overwritten: %q", got.ExecKind)
	}
}

func TestImportEntryNeverLinksUp(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, runAt("ns", "s", "u1", RunTypeUp, _t0))
	e := entry(_histKey, RunTypeImport, RunStateSucceeded, _t0)
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	if got := link(t, s, e.EndedAt.Add(16*time.Minute)); got.Linked != 0 || got.Imported != 1 {
		t.Fatalf("link result = %+v, want imported, never linked to the up", got)
	}
}

// A run backfilled from Stack.status.lastUpdate has no UID but is an operator run with a
// real Update name; its history's message (PKO's "New commit detected...") is not a title.
func TestBackfilledRunGetsNoTitle(t *testing.T) {
	s, _ := newTestStore(t)
	must(t, s.BackfillRun(t.Context(), Run{Namespace: "ns", UpdateName: "prod-1a0d", StackName: "s",
		Type: RunTypeUp, State: RunStateSucceeded, ObservedAt: _t0.Add(time.Hour)}))
	e := withOrigin(entry(_histKey, RunTypeUp, RunStateSucceeded, _t0.Add(9*time.Minute)))
	e.Message, e.ExecKind = "New commit detected", "auto.local"
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	if got := link(t, s, _t0.Add(2*time.Hour)); got.Linked != 1 {
		t.Fatalf("result = %+v, want the backfilled run linked", got)
	}
	got, err := s.GetRun(t.Context(), getRunByName(t, s, "ns", "prod-1a0d").ID)
	must(t, err)
	if got.Title != "" || got.ExecKind != "auto.local" {
		t.Fatalf("backfilled run = title %q exec %q, want no title and the origin", got.Title, got.ExecKind)
	}
}

// A laptop import between the operator's last up and the backfill must not stop that up's
// history from linking to the backfilled run (which would import a duplicate).
func TestImportEntryDoesNotBlockBackfillLink(t *testing.T) {
	s, _ := newTestStore(t)
	must(t, s.BackfillRun(t.Context(), Run{Namespace: "ns", UpdateName: "prod-1a0d", StackName: "s",
		Type: RunTypeUp, State: RunStateSucceeded, ObservedAt: _t0.Add(time.Hour)}))
	up := entry(_histKey, RunTypeUp, RunStateSucceeded, _t0.Add(9*time.Minute))
	imp := entry("p/.pulumi/history/proj/dev/dev-1790239799000000000.history.json",
		RunTypeImport, RunStateSucceeded, _t0.Add(20*time.Minute))
	for _, e := range []HistoryEntry{up, imp} {
		if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
			t.Fatal(err)
		}
	}
	link(t, s, _t0.Add(2*time.Hour))
	if state, id := historyRow(t, s, _histKey); state != "linked" || id == nil ||
		*id != getRunByName(t, s, "ns", "prod-1a0d").ID {
		t.Fatalf("up entry = %s %v, want linked to the backfilled run", state, id)
	}
}
