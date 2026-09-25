package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const (
	_day           = 24 * time.Hour
	_runRetention  = 180 * _day
	_authRetention = 365 * _day
)

func TestPrune(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	old := runAt("ns", "s", "old", RunTypeUp, now.Add(-182*_day))
	oldEnd := now.Add(-181 * _day)
	old.EndedAt = &oldEnd
	mustUpsert(t, s, old)
	recent := runAt("ns", "s", "recent", RunTypeUp, now.Add(-180*_day))
	recentEnd := now.Add(-179 * _day)
	recent.EndedAt = &recentEnd
	mustUpsert(t, s, recent)
	stuck := Run{Namespace: "ns", UpdateName: "stuck", StackName: "s", Type: RunTypeUp,
		State: RunStateRunning, ObservedAt: now.Add(-200 * _day)}
	mustUpsert(t, s, stuck)

	must(t, s.InsertAuthEvent(ctx, AuthEvent{At: now.Add(-366 * _day), Outcome: AuthOutcomeLogin}))
	must(t, s.InsertAuthEvent(ctx, AuthEvent{At: now.Add(-364 * _day), Outcome: AuthOutcomeLogin}))

	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "s", UpdatedAt: now}))
	must(t, s.MarkStackDeleted(ctx, "ns", "s", now))
	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "empty", UpdatedAt: now}))
	must(t, s.MarkStackDeleted(ctx, "ns", "empty", now))
	must(t, s.UpsertStack(ctx, Stack{Namespace: "ns", Name: "live", UpdatedAt: now}))

	got, err := s.Prune(ctx, now, _runRetention, _authRetention)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(PruneResult{Runs: 2, AuthEvents: 1, Stacks: 1}, got); diff != "" {
		t.Errorf("result (-want +got):\n%s", diff)
	}
	if n := countRuns(t, s); n != 1 {
		t.Fatalf("runs left = %d, want 1", n)
	}
	var stacks []string
	rows, err := s.pool.Query(ctx, `SELECT name FROM stacks ORDER BY name`)
	must(t, err)
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		stacks = append(stacks, n)
	}
	if diff := cmp.Diff([]string{"live", "s"}, stacks); diff != "" {
		t.Errorf("stacks (-want +got):\n%s", diff)
	}
}

func TestPruneBatchCapIsError(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		mustUpsert(t, s, runAt("ns", "s", string(rune('a'+i)), RunTypeUp, now.Add(-400*_day)))
	}
	defer func(rows, batches int) { pruneBatchRows, pruneBatchesMax = rows, batches }(
		pruneBatchRows, pruneBatchesMax)
	pruneBatchRows, pruneBatchesMax = 2, 2

	_, err := s.Prune(t.Context(), now, _runRetention, _authRetention)
	if err == nil || !strings.Contains(err.Error(), "batch cap") {
		t.Fatalf("err = %v, want batch cap", err)
	}
	if _, err := s.Prune(t.Context(), now, _runRetention, _authRetention); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if n := countRuns(t, s); n != 0 {
		t.Fatalf("runs left = %d, want 0", n)
	}
}
