package web

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var _stockholm = func() *time.Location { l, _ := time.LoadLocation("Europe/Stockholm"); return l }()

func railRun(name string, typ store.RunType, state store.RunState, at time.Time) store.Run {
	return store.Run{UpdateName: name, Type: typ, State: state, StartedAt: &at, ObservedAt: at}
}

func shape(days []railDay) []string {
	var out []string
	for _, d := range days {
		out = append(out, "day:"+d.Label)
		for _, it := range d.Items {
			if it.Node != nil {
				out = append(out, it.Node.Run.UpdateName)
			} else {
				out = append(out, "fold:"+strconv.Itoa(len(it.Fold)))
			}
		}
	}
	return out
}

func TestRailFoldsCleanPreviews(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	changes := []store.Run{railRun("up2", store.RunTypeUp, store.RunStateSucceeded, now.Add(-time.Hour)),
		railRun("up1", store.RunTypeUp, store.RunStateSucceeded, now.Add(-4*time.Hour))}
	previews := []store.Run{railRun("p3", store.RunTypePreview, store.RunStateSucceeded, now.Add(-10*time.Minute)),
		railRun("p2", store.RunTypePreview, store.RunStateSucceeded, now.Add(-2*time.Hour)),
		railRun("p1", store.RunTypePreview, store.RunStateSucceeded, now.Add(-3*time.Hour))}
	got := shape(buildRail(changes, previews, now, time.UTC, false))
	want := []string{"day:Today", "fold:1", "up2", "fold:2", "up1"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailDriftPreviewSplitsFold(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	drift := railRun("p2", store.RunTypePreview, store.RunStateSucceeded, now.Add(-2*time.Hour))
	drift.LogChanges = map[string]int64{"update": 1, "same": 9}
	failed := railRun("p0", store.RunTypePreview, store.RunStateFailed, now.Add(-3*time.Hour-30*time.Minute))
	previews := []store.Run{railRun("p3", store.RunTypePreview, store.RunStateSucceeded, now.Add(-90*time.Minute)),
		drift, railRun("p1", store.RunTypePreview, store.RunStateSucceeded, now.Add(-3*time.Hour)), failed}
	got := buildRail(nil, previews, now, time.UTC, false)
	if diff := cmp.Diff([]string{"day:Today", "fold:1", "p2", "fold:1", "p0"}, shape(got)); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
	if !got[0].Items[1].Node.Drift || got[0].Items[3].Node.Drift {
		t.Error("only the preview with changes is drift")
	}
}

func TestRailDaysUseTimezone(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)                                                                // 12:00 in Stockholm
	late := railRun("late", store.RunTypeUp, store.RunStateSucceeded, time.Date(2026, 9, 24, 22, 30, 0, 0, time.UTC))   // 00:30 local, 25 Sep
	early := railRun("early", store.RunTypeUp, store.RunStateSucceeded, time.Date(2026, 9, 24, 21, 30, 0, 0, time.UTC)) // 23:30 local, 24 Sep
	got := shape(buildRail([]store.Run{late, early}, nil, now, _stockholm, false))
	want := []string{"day:Today", "late", "day:Yesterday", "early"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailOlderDayLabel(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := railRun("old", store.RunTypeUp, store.RunStateSucceeded, time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	older := railRun("older", store.RunTypeUp, store.RunStateSucceeded, time.Date(2025, 12, 30, 9, 0, 0, 0, time.UTC))
	got := shape(buildRail([]store.Run{old, older}, nil, now, time.UTC, false))
	want := []string{"day:Tue 1 Sep", "old", "day:Tue 30 Dec 2025", "older"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailExpandShowsEveryPreview(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	previews := []store.Run{railRun("p2", store.RunTypePreview, store.RunStateSucceeded, now.Add(-time.Hour)),
		railRun("p1", store.RunTypePreview, store.RunStateSucceeded, now.Add(-2*time.Hour))}
	got := shape(buildRail(nil, previews, now, time.UTC, true))
	if diff := cmp.Diff([]string{"day:Today", "p2", "p1"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}
