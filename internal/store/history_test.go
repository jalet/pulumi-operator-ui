package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

func s3Stack(t *testing.T, s *Store, ns, name, backend, project string) {
	t.Helper()
	must(t, s.UpsertStack(t.Context(), Stack{Namespace: ns, Name: name, UpdatedAt: time.Now(),
		BackendURL: backend, Project: project, PulumiStack: "dev"}))
}

func TestS3Stacks(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()
	s3Stack(t, s, "ns", "a", "s3://b/p?region=eu-north-1", "proj")
	s3Stack(t, s, "ns", "cloud", "https://api.pulumi.com", "proj")
	s3Stack(t, s, "ns", "noproj", "s3://b/p", "")
	s3Stack(t, s, "ns", "gone", "s3://b/p", "proj")
	must(t, s.MarkStackDeleted(ctx, "ns", "gone", time.Now()))

	got, err := s.S3Stacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []S3Stack{{Namespace: "ns", Name: "a", BackendURL: "s3://b/p?region=eu-north-1",
		Project: "proj", PulumiStack: "dev"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func entry(key string, typ RunType, state RunState, start time.Time) HistoryEntry {
	return HistoryEntry{Key: key, Bucket: "b", Namespace: "ns", StackName: "s", Type: typ,
		State: state, StartedAt: start, EndedAt: start.Add(30 * time.Second), Commit: "h1",
		Counts: map[string]int64{"create": 2, "same": 5}}
}

func TestInsertHistoryIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	e := entry("p/.pulumi/history/proj/dev/dev-1.history.json", RunTypeUp, RunStateSucceeded, _t0)
	ok, err := s.InsertHistory(t.Context(), e, _t0)
	if err != nil || !ok {
		t.Fatalf("first insert = %v, %v", ok, err)
	}
	ok, err = s.InsertHistory(t.Context(), e, _t0)
	if err != nil || ok {
		t.Fatalf("second insert = %v, %v", ok, err)
	}
	var state, commit string
	var counts map[string]int64
	must(t, s.pool.QueryRow(t.Context(), `SELECT link_state, commit, counts FROM s3_history
		WHERE key = $1`, e.Key).Scan(&state, &commit, &counts))
	if state != "pending" || commit != "h1" {
		t.Fatalf("state %q commit %q", state, commit)
	}
	if diff := cmp.Diff(e.Counts, counts); diff != "" {
		t.Errorf("counts (-want +got):\n%s", diff)
	}
}

func TestNewestHistoryKeyIsPerPrefix(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()
	for _, k := range []string{
		"p1/.pulumi/history/proj/dev/dev-100.history.json",
		"p1/.pulumi/history/proj/dev/dev-200.history.json",
		"p2/.pulumi/history/proj/dev/dev-300.history.json",
	} {
		if _, err := s.InsertHistory(ctx, entry(k, RunTypeUp, RunStateSucceeded, _t0), _t0); err != nil {
			t.Fatal(err)
		}
	}
	other := entry("p1/.pulumi/history/proj/dev/dev-900.history.json", RunTypeUp, RunStateSucceeded, _t0)
	other.Bucket = "other"
	if _, err := s.InsertHistory(ctx, other, _t0); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ bucket, prefix, want string }{
		{"b", "p1/.pulumi/history/proj/dev/", "p1/.pulumi/history/proj/dev/dev-200.history.json"},
		{"b", "p2/.pulumi/history/proj/dev/", "p2/.pulumi/history/proj/dev/dev-300.history.json"},
		{"b", "p3/.pulumi/history/proj/dev/", ""},
		{"other", "p1/.pulumi/history/proj/dev/", "p1/.pulumi/history/proj/dev/dev-900.history.json"},
		{"b", "p_/.pulumi/history/proj/dev/", ""}, // "_" is literal, not a LIKE wildcard
	}
	for _, tt := range tests {
		got, err := s.NewestHistoryKey(ctx, tt.bucket, tt.prefix)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("NewestHistoryKey(%s, %s) = %q, want %q", tt.bucket, tt.prefix, got, tt.want)
		}
	}
}

const _histKey = "p/.pulumi/history/proj/dev/dev-1790239744268661000.history.json"

func seedRun(t *testing.T, s *Store, name string, typ RunType, state RunState, start time.Time,
	src CommitSource) Run {
	t.Helper()
	end := start.Add(30 * time.Second)
	r := Run{Namespace: "ns", UpdateName: name, UID: "uid-" + name, StackName: "s", Type: typ,
		State: state, StartedAt: &start, EndedAt: &end, ObservedAt: end, Commit: "c-" + name,
		CommitSource: src}
	mustUpsert(t, s, r)
	return getRunByName(t, s, "ns", name)
}

func seedEntry(t *testing.T, s *Store, key string, start time.Time) HistoryEntry {
	t.Helper()
	e := entry(key, RunTypeUp, RunStateSucceeded, start)
	if _, err := s.InsertHistory(t.Context(), e, start); err != nil {
		t.Fatal(err)
	}
	return e
}

func historyRow(t *testing.T, s *Store, key string) (string, *int64) {
	t.Helper()
	var state string
	var runID *int64
	must(t, s.pool.QueryRow(t.Context(), `SELECT link_state, run_id FROM s3_history WHERE key = $1`,
		key).Scan(&state, &runID))
	return state, runID
}

func changesOf(t *testing.T, s *Store, runID int64) map[string]int64 {
	t.Helper()
	var counts map[string]int64
	err := s.pool.QueryRow(t.Context(), `SELECT counts FROM run_changes
		WHERE run_id = $1 AND source = 's3'`, runID).Scan(&counts)
	if err != nil {
		return nil
	}
	return counts
}

func link(t *testing.T, s *Store, now time.Time) LinkResult {
	t.Helper()
	res, err := s.LinkHistory(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLinkOneMatch(t *testing.T) {
	s, _ := newTestStore(t)
	r := seedRun(t, s, "u1", RunTypeUp, RunStateSucceeded, _t0, CommitSourceStack)
	seedEntry(t, s, _histKey, _t0.Add(5*time.Second))
	if got := link(t, s, _t0.Add(time.Minute)); got != (LinkResult{Linked: 1}) {
		t.Fatalf("result = %+v", got)
	}
	if diff := cmp.Diff(map[string]int64{"create": 2, "same": 5}, changesOf(t, s, r.ID)); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	got := getRunByName(t, s, "ns", "u1")
	if got.Commit != "h1" || got.CommitSource != CommitSourceHistory {
		t.Errorf("commit = %s/%s, want h1/history", got.Commit, got.CommitSource)
	}
	if state, id := historyRow(t, s, _histKey); state != "linked" || id == nil || *id != r.ID {
		t.Errorf("entry = %s %v", state, id)
	}
}

func TestLinkKeepsExactCommit(t *testing.T) {
	s, _ := newTestStore(t)
	seedRun(t, s, "u1", RunTypeUp, RunStateSucceeded, _t0, CommitSourceUpdate)
	seedEntry(t, s, _histKey, _t0)
	link(t, s, _t0.Add(time.Minute))
	if got := getRunByName(t, s, "ns", "u1"); got.Commit != "c-u1" || got.CommitSource != CommitSourceUpdate {
		t.Errorf("exact commit replaced: %s/%s", got.Commit, got.CommitSource)
	}
}

func TestLinkToleratesSkew(t *testing.T) {
	s, _ := newTestStore(t)
	near := seedRun(t, s, "near", RunTypeUp, RunStateSucceeded, _t0, CommitSourceStack)
	seedEntry(t, s, _histKey, _t0.Add(59*time.Second))
	if got := link(t, s, _t0.Add(2*time.Minute)); got.Linked != 1 {
		t.Fatalf("59s skew not linked: %+v", got)
	}
	if changesOf(t, s, near.ID) == nil {
		t.Fatal("near run has no changes")
	}
	far := "p/.pulumi/history/proj/dev/dev-2.history.json"
	seedRun(t, s, "far", RunTypeUp, RunStateSucceeded, _t0.Add(time.Hour), CommitSourceStack)
	seedEntry(t, s, far, _t0.Add(time.Hour+61*time.Second))
	if got := link(t, s, _t0.Add(time.Hour+2*time.Minute)); got != (LinkResult{Pending: 1}) {
		t.Fatalf("61s skew: %+v, want pending (inside grace)", got)
	}
}

func TestLinkRequiresSameTypeAndState(t *testing.T) {
	s, _ := newTestStore(t)
	seedRun(t, s, "r", RunTypeRefresh, RunStateSucceeded, _t0, CommitSourceStack)
	seedRun(t, s, "f", RunTypeUp, RunStateFailed, _t0, CommitSourceStack)
	seedEntry(t, s, _histKey, _t0)
	if got := link(t, s, _t0.Add(time.Minute)); got != (LinkResult{Pending: 1}) {
		t.Fatalf("result = %+v, want pending", got)
	}
}

func TestLinkAmbiguous(t *testing.T) {
	s, _ := newTestStore(t)
	a := seedRun(t, s, "a", RunTypeUp, RunStateSucceeded, _t0, CommitSourceStack)
	b := seedRun(t, s, "b", RunTypeUp, RunStateSucceeded, _t0.Add(10*time.Second), CommitSourceStack)
	seedEntry(t, s, _histKey, _t0.Add(5*time.Second))
	if got := link(t, s, _t0.Add(time.Hour)); got != (LinkResult{Ambiguous: 1}) {
		t.Fatalf("result = %+v", got)
	}
	if state, _ := historyRow(t, s, _histKey); state != "ambiguous" {
		t.Errorf("state = %s", state)
	}
	if changesOf(t, s, a.ID) != nil || changesOf(t, s, b.ID) != nil || countRuns(t, s) != 2 {
		t.Error("ambiguous entry linked or imported")
	}
}

func TestImportAfterGrace(t *testing.T) {
	s, _ := newTestStore(t)
	e := seedEntry(t, s, _histKey, _t0)
	if got := link(t, s, e.EndedAt.Add(14*time.Minute)); got != (LinkResult{Pending: 1}) {
		t.Fatalf("inside grace: %+v", got)
	}
	if got := link(t, s, e.EndedAt.Add(16*time.Minute)); got != (LinkResult{Imported: 1}) {
		t.Fatalf("after grace: %+v", got)
	}
	r := getRunByName(t, s, "ns", "s3:dev-1790239744268661000")
	want := Run{ID: r.ID, Namespace: "ns", UpdateName: "s3:dev-1790239744268661000",
		StackName: "s", Type: RunTypeUp, Commit: "h1", CommitSource: CommitSourceHistory,
		State: RunStateSucceeded, StartedAt: &e.StartedAt, EndedAt: &e.EndedAt,
		ObservedAt: e.EndedAt}
	if diff := cmp.Diff(want, r, cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })); diff != "" {
		t.Errorf("imported run (-want +got):\n%s", diff)
	}
	if changesOf(t, s, r.ID) == nil {
		t.Error("imported run has no changes")
	}
	if state, id := historyRow(t, s, _histKey); state != "imported" || id == nil || *id != r.ID {
		t.Errorf("entry = %s %v", state, id)
	}
}

func TestLinkIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	seedEntry(t, s, _histKey, _t0)
	link(t, s, _t0.Add(time.Hour))
	if got := link(t, s, _t0.Add(time.Hour)); got != (LinkResult{}) {
		t.Fatalf("second call = %+v, want no work", got)
	}
	if n := countRuns(t, s); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestLinkProcessesBacklogInBatches(t *testing.T) {
	s, _ := newTestStore(t)
	defer func(n int) { linkBatchRowsMax = n }(linkBatchRowsMax)
	linkBatchRowsMax = 2
	for i := range 5 {
		seedEntry(t, s, fmt.Sprintf("p/.pulumi/history/proj/dev/dev-%d.history.json", i),
			_t0.Add(time.Duration(i)*time.Minute))
	}
	now := _t0.Add(time.Hour)
	for i, want := range []int{2, 2, 1, 0} {
		if got := link(t, s, now); got.Imported != want {
			t.Fatalf("call %d imported %d, want %d", i+1, got.Imported, want)
		}
	}
}

func TestLinkPublishesEvents(t *testing.T) {
	s, pub := newTestStore(t)
	seedEntry(t, s, _histKey, _t0)
	pub.reset()
	link(t, s, _t0.Add(time.Hour))
	r := getRunByName(t, s, "ns", "s3:dev-1790239744268661000")
	want := []events.Event{
		{Kind: events.KindRun, Namespace: "ns", Stack: "s", RunID: r.ID},
		{Kind: events.KindStack, Namespace: "ns", Stack: "s"},
	}
	if diff := cmp.Diff(want, pub.events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}
