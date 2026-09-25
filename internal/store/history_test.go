package store

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
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
