package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var _t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func runAt(ns, stack, name string, typ RunType, started time.Time) Run {
	return Run{
		Namespace: ns, UpdateName: name, UID: "uid-" + name, StackName: stack, Type: typ,
		State: RunStateSucceeded, StartedAt: &started, ObservedAt: started,
	}
}

func TestListStacksLatestPerType(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()
	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "a", Ready: true, UpdatedAt: _t0}))
	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "gone", UpdatedAt: _t0}))
	must(t, s.MarkStackDeleted(ctx, "ns", "gone", _t0))
	mustUpsert(t, s, runAt("ns", "a", "p-old", RunTypePreview, _t0))
	newer := runAt("ns", "a", "p-new", RunTypePreview, _t0.Add(time.Hour))
	newer.Commit, newer.CommitSource = "abc", CommitSourceStack
	mustUpsert(t, s, newer)
	mustUpsert(t, s, runAt("ns", "a", "u1", RunTypeUp, _t0.Add(30*time.Minute)))

	got, err := s.ListStacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("stacks = %+v, want only a", got)
	}
	pNew := getRunByName(t, s, "ns", "p-new")
	if got[0].LastPreview == nil || got[0].LastPreview.ID != pNew.ID {
		t.Fatalf("LastPreview = %+v, want id %d", got[0].LastPreview, pNew.ID)
	}
	if got[0].LastPreview.Commit != "abc" || got[0].LastPreview.State != RunStateSucceeded {
		t.Fatalf("LastPreview = %+v", got[0].LastPreview)
	}
	if got[0].LastUp == nil || got[0].LastUp.ID != getRunByName(t, s, "ns", "u1").ID {
		t.Fatalf("LastUp = %+v", got[0].LastUp)
	}
}

func TestListStacksWithoutRuns(t *testing.T) {
	s, _ := newTestStore(t)
	must(t, s.UpsertStack(t.Context(), Stack{Namespace: "ns", Name: "a", UpdatedAt: _t0}))
	got, err := s.ListStacks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LastPreview != nil || got[0].LastUp != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestGetStackIncludesDeleted(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()
	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "gone", UpdatedAt: _t0}))
	must(t, s.MarkStackDeleted(ctx, "ns", "gone", _t0))
	got, err := s.GetStack(ctx, "ns", "gone")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeletedAt == nil {
		t.Fatal("DeletedAt not set")
	}
	if _, err := s.GetStack(ctx, "ns", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListRunsKeyset(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()
	var want []string
	for i := range 5 {
		name := string(rune('a' + i))
		mustUpsert(t, s, runAt("ns", "s", name, RunTypeUp, _t0.Add(time.Duration(i)*time.Minute)))
		want = append([]string{name}, want...) // newest first
	}
	mustUpsert(t, s, runAt("ns", "other", "x", RunTypeUp, _t0))

	var got []string
	var cursor *Cursor
	for page := 1; page <= 10; page++ {
		runs, next, err := s.ListRuns(ctx, "ns", "s", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			got = append(got, r.UpdateName)
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("runs (-want +got):\n%s", diff)
	}
}

func TestListRunsSameTimeTieBreaksOnID(t *testing.T) {
	s, _ := newTestStore(t)
	for _, name := range []string{"a", "b", "c"} {
		mustUpsert(t, s, runAt("ns", "s", name, RunTypeUp, _t0))
	}
	first, next, err := s.ListRuns(t.Context(), "ns", "s", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	rest, last, err := s.ListRuns(t.Context(), "ns", "s", next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(rest) != 1 || last != nil {
		t.Fatalf("pages = %d, %d, last=%v", len(first), len(rest), last)
	}
	if rest[0].UpdateName != "a" {
		t.Fatalf("last page = %s, want a", rest[0].UpdateName)
	}
}

func TestGetRun(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, runAt("ns", "s", "u1", RunTypeUp, _t0))
	id := getRunByName(t, s, "ns", "u1").ID
	got, err := s.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdateName != "u1" || got.UID != "uid-u1" {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetRun(t.Context(), id+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestInsertAuthEventCapsFields(t *testing.T) {
	s, _ := newTestStore(t)
	values := make([]string, 80)
	for i := range values {
		values[i] = strings.Repeat("v", 400)
	}
	must(t, s.InsertAuthEvent(t.Context(), AuthEvent{
		At: _t0, Subject: "sub", Outcome: AuthOutcomeDenied, ClaimValues: values,
		Detail: strings.Repeat("é", 1000), // 2000 bytes, multi-byte runes
	}))
	var gotValues []string
	var detail string
	must(t, s.pool.QueryRow(t.Context(),
		`SELECT claim_values, detail FROM auth_events`).Scan(&gotValues, &detail))
	if len(gotValues) != authClaimValuesMax {
		t.Fatalf("claim values = %d, want %d", len(gotValues), authClaimValuesMax)
	}
	if len(gotValues[0]) != authClaimValueBytesMax {
		t.Fatalf("value bytes = %d, want %d", len(gotValues[0]), authClaimValueBytesMax)
	}
	if len(detail) > authDetailBytesMax || !strings.HasPrefix(detail, "é") {
		t.Fatalf("detail bytes = %d", len(detail))
	}
}
