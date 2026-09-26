package web

import (
	"strconv"
	"strings"
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
			switch {
			case it.Group != nil:
				var names []string
				for _, g := range it.Group {
					if g.Node != nil {
						names = append(names, g.Node.Run.UpdateName)
					} else {
						names = append(names, "fold:"+strconv.Itoa(len(g.Fold)))
					}
				}
				out = append(out, "group:"+strings.Join(names, ","))
			case it.Node != nil:
				out = append(out, it.Node.Run.UpdateName)
			default:
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
	got := shape(buildRail(changes, previews, now, time.UTC, false, true))
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
	got := buildRail(nil, previews, now, time.UTC, false, true)
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
	got := shape(buildRail([]store.Run{late, early}, nil, now, _stockholm, false, true))
	want := []string{"day:Today", "late", "day:Yesterday", "early"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailOlderDayLabel(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := railRun("old", store.RunTypeUp, store.RunStateSucceeded, time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	older := railRun("older", store.RunTypeUp, store.RunStateSucceeded, time.Date(2025, 12, 30, 9, 0, 0, 0, time.UTC))
	got := shape(buildRail([]store.Run{old, older}, nil, now, time.UTC, false, true))
	want := []string{"day:Tue 1 Sep", "old", "day:Tue 30 Dec 2025", "older"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailExpandShowsEveryPreview(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	previews := []store.Run{railRun("p2", store.RunTypePreview, store.RunStateSucceeded, now.Add(-time.Hour)),
		railRun("p1", store.RunTypePreview, store.RunStateSucceeded, now.Add(-2*time.Hour))}
	got := shape(buildRail(nil, previews, now, time.UTC, true, true))
	if diff := cmp.Diff([]string{"day:Today", "p2", "p1"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailRunningPreviewIsANode(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	running := railRun("p1", store.RunTypePreview, store.RunStateRunning, now.Add(-time.Minute))
	got := shape(buildRail(nil, []store.Run{running}, now, time.UTC, false, true))
	if diff := cmp.Diff([]string{"day:Today", "p1"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

// A fold keeps its identity as newer previews join it, so a viewer's open fold stays open.
func TestRailFoldIDIsOldestPreview(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	up := railRun("up1", store.RunTypeUp, store.RunStateSucceeded, now.Add(-time.Hour))
	p1 := railRun("p1", store.RunTypePreview, store.RunStateSucceeded, now.Add(-30*time.Minute))
	p2 := railRun("p2", store.RunTypePreview, store.RunStateSucceeded, now.Add(-10*time.Minute))
	up.ID, p1.ID, p2.ID = 1, 2, 3
	days := buildRail([]store.Run{up}, []store.Run{p2, p1}, now, time.UTC, false, true)
	if fold := days[0].Items[0]; fold.Fold == nil || fold.FoldID != 2 {
		t.Fatalf("first item %+v, want a fold with id 2", fold)
	}
}

// noChange is a succeeded up whose engine log counted nothing but unchanged resources.
func noChange(name string, at time.Time) store.Run {
	r := railRun(name, store.RunTypeUp, store.RunStateSucceeded, at)
	r.LogChanges = map[string]int64{"same": 12}
	return r
}

func changed(name string, at time.Time) store.Run {
	r := railRun(name, store.RunTypeUp, store.RunStateSucceeded, at)
	r.LogChanges = map[string]int64{"update": 1, "same": 11}
	return r
}

// Two or more ups in a row without changes become one group; the latest run and a lone
// no-change run stay nodes.
func TestRailGroupsNoChangeUps(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	changes := []store.Run{noChange("u75", now.Add(-1*time.Hour)), noChange("u74", now.Add(-2*time.Hour)),
		noChange("u73", now.Add(-3*time.Hour)), noChange("u72", now.Add(-4*time.Hour)),
		changed("u65", now.Add(-5*time.Hour)), noChange("u64", now.Add(-6*time.Hour)),
		changed("u63", now.Add(-7*time.Hour))}
	got := shape(buildRail(changes, nil, now, time.UTC, false, true))
	want := []string{"day:Today", "u75", "group:u74,u73,u72", "u65", "u64", "u63"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

// On an older page the top run is not the latest, so it can be grouped.
func TestRailGroupsTopRunOnOlderPages(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	changes := []store.Run{noChange("u2", now.Add(-1*time.Hour)), noChange("u1", now.Add(-2*time.Hour))}
	got := shape(buildRail(changes, nil, now, time.UTC, false, false))
	if diff := cmp.Diff([]string{"day:Today", "group:u2,u1"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

// A preview fold between grouped ups joins the group; folds at its edges stay outside, and
// groups split at day headers like folds do.
func TestRailGroupSpansFoldsWithinADay(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	changes := []store.Run{changed("u9", now.Add(-1*time.Hour)), noChange("u8", now.Add(-3*time.Hour)),
		noChange("u7", now.Add(-5*time.Hour)), noChange("u6", now.Add(-20*time.Hour)),
		noChange("u5", now.Add(-21*time.Hour))}
	previews := []store.Run{railRun("p1", store.RunTypePreview, store.RunStateSucceeded, now.Add(-2*time.Hour)),
		railRun("p2", store.RunTypePreview, store.RunStateSucceeded, now.Add(-4*time.Hour))}
	got := shape(buildRail(changes, previews, now, time.UTC, false, true))
	want := []string{"day:Today", "u9", "fold:1", "group:u8,fold:1,u7", "day:Yesterday", "group:u6,u5"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

// Runs whose outcome matters or is not known yet are never hidden in a group.
func TestRailNeverGroupsFailedRunningOrUnknown(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	failed := noChange("failed", now.Add(-2*time.Hour))
	failed.State = store.RunStateFailed
	running := noChange("running", now.Add(-3*time.Hour))
	running.State = store.RunStateRunning
	unknown := railRun("unknown", store.RunTypeUp, store.RunStateSucceeded, now.Add(-4*time.Hour))
	refresh := noChange("refresh", now.Add(-5*time.Hour))
	refresh.Type = store.RunTypeRefresh
	changes := []store.Run{changed("top", now.Add(-1*time.Hour)), failed, running, unknown, refresh}
	got := shape(buildRail(changes, nil, now, time.UTC, false, true))
	if diff := cmp.Diff([]string{"day:Today", "top", "failed", "running", "unknown", "refresh"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestRailExpandShowsEveryRun(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	changes := []store.Run{noChange("u3", now.Add(-1*time.Hour)), noChange("u2", now.Add(-2*time.Hour)),
		noChange("u1", now.Add(-3*time.Hour))}
	got := shape(buildRail(changes, nil, now, time.UTC, true, true))
	if diff := cmp.Diff([]string{"day:Today", "u3", "u2", "u1"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}
