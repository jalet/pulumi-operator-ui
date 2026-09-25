package store

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

func mustUpsert(t *testing.T, s *Store, r Run) {
	t.Helper()
	must(t, s.UpsertRun(t.Context(), r))
}

func TestUpsertRunNeverRegressesTerminalState(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStateSucceeded)
	mustUpsert(t, s, r)
	r.State = RunStateRunning // late Progressing event after Complete
	mustUpsert(t, s, r)
	if got := getRunByName(t, s, "ns", "u1"); got.State != RunStateSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
}

func TestUpsertRunAdvancesNonTerminalState(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStatePending)
	mustUpsert(t, s, r)
	r.State = RunStateFailed
	mustUpsert(t, s, r)
	if got := getRunByName(t, s, "ns", "u1"); got.State != RunStateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

func TestUpsertRunCommitFirstSeenWins(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStatePending)
	r.Commit, r.CommitSource = "aaa", CommitSourceStack
	mustUpsert(t, s, r)
	r.Commit = "bbb" // the Stack moved on before the Update completed
	r.State = RunStateSucceeded
	mustUpsert(t, s, r)
	got := getRunByName(t, s, "ns", "u1")
	if got.Commit != "aaa" || got.CommitSource != CommitSourceStack {
		t.Fatalf("commit = %s/%s, want aaa/stack", got.Commit, got.CommitSource)
	}
}

func TestUpsertRunEmptyCommitFilledLater(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStatePending)
	mustUpsert(t, s, r)
	r.Commit, r.CommitSource = "aaa", CommitSourceStack
	mustUpsert(t, s, r)
	if got := getRunByName(t, s, "ns", "u1"); got.Commit != "aaa" {
		t.Fatalf("commit = %q, want aaa", got.Commit)
	}
}

func TestUpsertRunUpdateSourceOverridesStackSource(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStatePending)
	r.Commit, r.CommitSource = "aaa", CommitSourceStack
	mustUpsert(t, s, r)
	r.Commit, r.CommitSource = "ccc", CommitSourceUpdate
	mustUpsert(t, s, r)
	got := getRunByName(t, s, "ns", "u1")
	if got.Commit != "ccc" || got.CommitSource != CommitSourceUpdate {
		t.Fatalf("commit = %s/%s, want ccc/update", got.Commit, got.CommitSource)
	}
}

func TestUpsertRunKeepsFirstTimes(t *testing.T) {
	s, _ := newTestStore(t)
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	r := run("ns", "u1", RunStateRunning)
	r.StartedAt = &t0
	mustUpsert(t, s, r)
	later := t0.Add(time.Hour)
	r.StartedAt, r.EndedAt, r.State = &later, &later, RunStateSucceeded
	mustUpsert(t, s, r)
	got := getRunByName(t, s, "ns", "u1")
	if !got.StartedAt.Equal(t0) || !got.EndedAt.Equal(later) {
		t.Fatalf("times = %v..%v", got.StartedAt, got.EndedAt)
	}
}

func TestUpsertRunPublishes(t *testing.T) {
	s, pub := newTestStore(t)
	mustUpsert(t, s, run("ns", "u1", RunStatePending))
	got := getRunByName(t, s, "ns", "u1")
	want := []events.Event{
		{Kind: events.KindRun, Namespace: "ns", Stack: "s", RunID: got.ID},
		{Kind: events.KindStack, Namespace: "ns", Stack: "s"},
	}
	if diff := cmp.Diff(want, pub.events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestBackfillThenUpdateMerges(t *testing.T) {
	s, _ := newTestStore(t)
	b := run("ns", "u1", RunStateFailed)
	b.UID, b.Commit, b.CommitSource = "", "aaa", CommitSourceStack
	must(t, s.BackfillRun(t.Context(), b))
	if got := getRunByName(t, s, "ns", "u1"); got.UID != "" {
		t.Fatalf("backfilled uid = %q, want empty", got.UID)
	}
	u := run("ns", "u1", RunStateFailed)
	u.UID = "uid-1"
	mustUpsert(t, s, u)
	got := getRunByName(t, s, "ns", "u1")
	if got.UID != "uid-1" || got.Commit != "aaa" {
		t.Fatalf("got uid=%q commit=%q", got.UID, got.Commit)
	}
	if n := countRuns(t, s); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestBackfillKeepsTerminalRow(t *testing.T) {
	s, pub := newTestStore(t)
	u := run("ns", "u1", RunStateSucceeded)
	u.Message = "from update"
	mustUpsert(t, s, u)
	pub.reset()
	b := run("ns", "u1", RunStateFailed)
	b.Message = "from stack"
	must(t, s.BackfillRun(t.Context(), b))
	if got := getRunByName(t, s, "ns", "u1"); got.Message != "from update" {
		t.Fatalf("message = %q", got.Message)
	}
	if len(pub.kinds()) != 0 {
		t.Fatalf("published %v for a no-op insert", pub.kinds())
	}
}

func TestStackDeleteAndRecreate(t *testing.T) {
	s, pub := newTestStore(t)
	ctx := t.Context()
	st := Stack{Namespace: "ns", Name: "s", Ready: true, UpdatedAt: time.Now()}
	must(t, s.UpsertStack(ctx, st))
	must(t, s.MarkStackDeleted(ctx, "ns", "s", time.Now()))
	if got := getStackRow(t, s, "ns", "s"); got.DeletedAt == nil {
		t.Fatal("stack not marked deleted")
	}
	must(t, s.UpsertStack(ctx, st))
	if got := getStackRow(t, s, "ns", "s"); got.DeletedAt != nil {
		t.Fatal("recreated stack still deleted")
	}
	want := []events.Kind{events.KindStackSet, events.KindStackSet, events.KindStackSet}
	if diff := cmp.Diff(want, pub.kinds()); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestMarkStackDeletedTwicePublishesOnce(t *testing.T) {
	s, pub := newTestStore(t)
	ctx := t.Context()
	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "s", UpdatedAt: time.Now()}))
	pub.reset()
	must(t, s.MarkStackDeleted(ctx, "ns", "s", time.Now()))
	must(t, s.MarkStackDeleted(ctx, "ns", "s", time.Now()))
	must(t, s.MarkStackDeleted(ctx, "ns", "never-seen", time.Now()))
	if diff := cmp.Diff([]events.Kind{events.KindStackSet}, pub.kinds()); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestUpsertStackChangePublishesRowEventOnly(t *testing.T) {
	s, pub := newTestStore(t)
	st := Stack{Namespace: "ns", Name: "s", UpdatedAt: time.Now()}
	must(t, s.UpsertStack(t.Context(), st))
	pub.reset()
	st.Ready, st.LastCommit = true, "abc"
	must(t, s.UpsertStack(t.Context(), st))
	if diff := cmp.Diff([]events.Kind{events.KindStack}, pub.kinds()); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	got := getStackRow(t, s, "ns", "s")
	if !got.Ready || got.LastCommit != "abc" {
		t.Fatalf("row not updated: %+v", got)
	}
}

func TestOpenTwiceIsIdempotent(t *testing.T) {
	u := newDatabase(t)
	for range 2 {
		s, err := Open(t.Context(), Options{URL: u, Pub: &recordingPublisher{}, Log: zerolog.Nop()})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

// A run recorded as running whose Update was GC'd while the app was down only
// learns its end state from Stack.status.lastUpdate.
func TestBackfillCompletesStuckRun(t *testing.T) {
	s, pub := newTestStore(t)
	r := run("ns", "u1", RunStateRunning)
	r.Commit, r.CommitSource = "aaa", CommitSourceStack
	mustUpsert(t, s, r)
	pub.reset()
	b := run("ns", "u1", RunStateSucceeded)
	b.UID, b.Commit, b.CommitSource, b.Message = "", "aaa", CommitSourceUpdate, "done"
	must(t, s.BackfillRun(t.Context(), b))
	got := getRunByName(t, s, "ns", "u1")
	if got.State != RunStateSucceeded || got.CommitSource != CommitSourceUpdate ||
		got.Message != "done" || got.UID != "uid-u1" {
		t.Fatalf("got %+v", got)
	}
	if len(pub.kinds()) == 0 {
		t.Fatal("no event for a converged run")
	}
}

func TestBackfillUpgradesApproximateCommit(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStateSucceeded)
	r.Commit, r.CommitSource = "old", CommitSourceStack
	mustUpsert(t, s, r)
	b := run("ns", "u1", RunStateSucceeded)
	b.Commit, b.CommitSource = "exact", CommitSourceUpdate
	must(t, s.BackfillRun(t.Context(), b))
	got := getRunByName(t, s, "ns", "u1")
	if got.Commit != "exact" || got.CommitSource != CommitSourceUpdate {
		t.Fatalf("commit = %s/%s, want exact/update", got.Commit, got.CommitSource)
	}
}

func TestBackfillKeepsExactCommit(t *testing.T) {
	s, _ := newTestStore(t)
	r := run("ns", "u1", RunStateSucceeded)
	r.Commit, r.CommitSource = "exact", CommitSourceUpdate
	mustUpsert(t, s, r)
	b := run("ns", "u1", RunStateSucceeded)
	b.Commit, b.CommitSource = "other", CommitSourceUpdate
	must(t, s.BackfillRun(t.Context(), b))
	if got := getRunByName(t, s, "ns", "u1"); got.Commit != "exact" {
		t.Fatalf("commit = %s, want exact", got.Commit)
	}
}
