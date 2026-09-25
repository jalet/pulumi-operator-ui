package store

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func finished(name string, started time.Time) Run {
	r := runAt("ns", "s", name, RunTypeUp, started)
	end := started.Add(30 * time.Second)
	r.EndedAt = &end
	return r
}

func TestFinishedRunBecomesPending(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, finished("u1", _t0))
	jobs, err := s.PendingLogs(t.Context(), 10)
	must(t, err)
	want := []LogJob{{RunID: getRunByName(t, s, "ns", "u1").ID, Namespace: "ns",
		StackName: "s", StartedAt: _t0, EndedAt: _t0.Add(30 * time.Second)}}
	if diff := cmp.Diff(want, jobs); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRunsWithoutWindowOrUIDAreNotPending(t *testing.T) {
	s, _ := newTestStore(t)
	running := finished("running", _t0)
	running.State, running.EndedAt = RunStateRunning, nil
	mustUpsert(t, s, running)
	noStart := finished("nostart", _t0)
	noStart.StartedAt = nil
	mustUpsert(t, s, noStart)
	backfill := finished("backfill", _t0)
	backfill.UID = ""
	must(t, s.BackfillRun(t.Context(), backfill))
	jobs, err := s.PendingLogs(t.Context(), 10)
	must(t, err)
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none", jobs)
	}
}

func TestRunBecomesPendingWhenItFinishes(t *testing.T) {
	s, _ := newTestStore(t)
	r := finished("u1", _t0)
	r.State, r.EndedAt = RunStateRunning, nil
	mustUpsert(t, s, r)
	mustUpsert(t, s, finished("u1", _t0))
	jobs, err := s.PendingLogs(t.Context(), 10)
	must(t, err)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
}

func TestSaveLogCaptured(t *testing.T) {
	s, pub := newTestStore(t)
	mustUpsert(t, s, finished("u1", _t0))
	id := getRunByName(t, s, "ns", "u1").ID
	pub.reset()
	res := []LogResource{{Op: "update", Type: "aws:iam/userPolicy:UserPolicy", Name: "p",
		URN: "urn:pulumi:prod::x::aws:iam/userPolicy:UserPolicy::p", Diff: "+ Sid: \"A\""}}
	must(t, s.SaveLog(t.Context(), id, LogStatusCaptured,
		map[string]int64{"update": 1, "same": 3}, res))
	got, err := s.GetRun(t.Context(), id)
	must(t, err)
	if got.LogStatus != LogStatusCaptured {
		t.Errorf("status = %q", got.LogStatus)
	}
	if diff := cmp.Diff(map[string]int64{"update": 1, "same": 3}, got.LogChanges); diff != "" {
		t.Errorf("counts (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(res, got.Resources); diff != "" {
		t.Errorf("resources (-want +got):\n%s", diff)
	}
	if got.Changes != nil {
		t.Errorf("S3 counts = %v, want nil", got.Changes)
	}
	if len(pub.kinds()) == 0 {
		t.Error("no event published")
	}
	if jobs, _ := s.PendingLogs(t.Context(), 10); len(jobs) != 0 {
		t.Errorf("still pending: %+v", jobs)
	}
}

func TestSaveLogUnavailable(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, finished("u1", _t0))
	id := getRunByName(t, s, "ns", "u1").ID
	must(t, s.SaveLog(t.Context(), id, LogStatusUnavailable, nil, nil))
	got, err := s.GetRun(t.Context(), id)
	must(t, err)
	if got.LogStatus != LogStatusUnavailable || got.LogChanges != nil || got.Resources != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestCapturedStatusSurvivesLaterUpserts(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, finished("u1", _t0))
	id := getRunByName(t, s, "ns", "u1").ID
	must(t, s.SaveLog(t.Context(), id, LogStatusCaptured, map[string]int64{"same": 1}, nil))
	mustUpsert(t, s, finished("u1", _t0))
	got, err := s.GetRun(t.Context(), id)
	must(t, err)
	if got.LogStatus != LogStatusCaptured {
		t.Fatalf("status = %q after re-upsert", got.LogStatus)
	}
}
