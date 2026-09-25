package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

func TestSeqCopiedOntoImportedRun(t *testing.T) {
	s, _ := newTestStore(t)
	e := entry(_histKey, RunTypeUp, RunStateSucceeded, _t0)
	e.Seq = 42
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	link(t, s, e.EndedAt.Add(16*time.Minute))
	r, err := s.GetRun(t.Context(), getRunByName(t, s, "ns", "s3:dev-1790239744268661000").ID)
	must(t, err)
	if r.Seq == nil || *r.Seq != 42 {
		t.Fatalf("seq = %v, want 42", r.Seq)
	}
}

func TestSeqFilledOnRereadNotChanged(t *testing.T) {
	s, _ := newTestStore(t)
	e := entry(_histKey, RunTypeUp, RunStateSucceeded, _t0)
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil { // no seq yet
		t.Fatal(err)
	}
	e.Seq = 7
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	e.Seq = 9 // a later, different count must not renumber
	if _, err := s.InsertHistory(t.Context(), e, _t0); err != nil {
		t.Fatal(err)
	}
	var seq int64
	must(t, s.pool.QueryRow(t.Context(), `SELECT seq FROM s3_history WHERE key = $1`, _histKey).Scan(&seq))
	if seq != 7 {
		t.Fatalf("seq = %d, want 7", seq)
	}
}

func TestCursorCarriesCount(t *testing.T) {
	s, _ := newTestStore(t)
	must(t, s.SetHistoryCursor(t.Context(), "b", "p/", "p/k2", 2, _t0))
	must(t, s.SetHistoryCursor(t.Context(), "b", "p/", "p/k1", 1, _t0)) // never moves back
	key, n, err := s.HistoryCursor(t.Context(), "b", "p/")
	must(t, err)
	if key != "p/k2" || n != 2 {
		t.Fatalf("cursor = %q %d, want p/k2 2", key, n)
	}
}

func TestRunResourceSummary(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, finished("u1", _t0))
	id := getRunByName(t, s, "ns", "u1").ID
	res := []LogResource{{Op: "create", Type: "a:b/c:D", Name: "one"}, {Op: "create", Type: "a:b/c:D", Name: "two"},
		{Op: "update", Type: "a:b/c:E", Name: "three"}, {Op: "delete", Type: "a:b/c:E", Name: "four"}}
	must(t, s.SaveLog(t.Context(), id, LogStatusCaptured, map[string]int64{"create": 2}, res, false))
	changes, _, _, err := s.ListTimeline(t.Context(), "ns", "s", nil, 50)
	must(t, err)
	if len(changes) != 1 || len(changes[0].Summary) != 3 || changes[0].ResourceTotal != 4 ||
		changes[0].Summary[0] != (ResourceRef{Type: "a:b/c:D", Name: "one"}) {
		t.Fatalf("summary = %+v total %d", changes[0].Summary, changes[0].ResourceTotal)
	}
}

func timelineRun(name string, typ RunType, at time.Time) Run {
	r := runAt("ns", "s", name, typ, at)
	end := at.Add(20 * time.Second)
	r.EndedAt = &end
	return r
}

func TestTimelineReturnsChangesAndPreviewsBetween(t *testing.T) {
	s, _ := newTestStore(t)
	mustUpsert(t, s, timelineRun("up1", RunTypeUp, _t0))
	mustUpsert(t, s, timelineRun("pv1", RunTypePreview, _t0.Add(10*time.Minute)))
	mustUpsert(t, s, timelineRun("pv2", RunTypePreview, _t0.Add(20*time.Minute)))
	mustUpsert(t, s, timelineRun("up2", RunTypeUp, _t0.Add(30*time.Minute)))
	mustUpsert(t, s, timelineRun("pv3", RunTypePreview, _t0.Add(40*time.Minute)))
	changes, previews, next, err := s.ListTimeline(t.Context(), "ns", "s", nil, 50)
	must(t, err)
	if len(changes) != 2 || changes[0].UpdateName != "up2" || next != nil {
		t.Fatalf("changes = %v next %v", names(changes), next)
	}
	if got := names(previews); len(got) != 3 || got[0] != "pv3" || got[2] != "pv1" {
		t.Fatalf("previews = %v, want pv3 pv2 pv1", got)
	}
}

func TestTimelinePreviewsSplitAtPageBoundary(t *testing.T) {
	s, _ := newTestStore(t)
	for i, n := range []string{"up1", "pvA", "up2", "pvB", "up3"} {
		typ := RunTypeUp
		if n[:2] == "pv" {
			typ = RunTypePreview
		}
		mustUpsert(t, s, timelineRun(n, typ, _t0.Add(time.Duration(i)*time.Minute)))
	}
	c1, p1, next, err := s.ListTimeline(t.Context(), "ns", "s", nil, 2) // up3, up2
	must(t, err)
	c2, p2, _, err := s.ListTimeline(t.Context(), "ns", "s", next, 2) // up1
	must(t, err)
	seen := map[string]int{}
	for _, r := range append(p1, p2...) {
		seen[r.UpdateName]++
	}
	if len(c1) != 2 || len(c2) != 1 || seen["pvA"] != 1 || seen["pvB"] != 1 {
		t.Fatalf("page1 %v %v, page2 %v %v", names(c1), names(p1), names(c2), names(p2))
	}
}

func names(rs []Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.UpdateName
	}
	return out
}

// When one page's window holds more previews than the cap, the page ends at the oldest
// preview it returned and the next page continues from there: none is lost.
func TestTimelinePreviewCapPages(t *testing.T) {
	defer func(n int) { previewsPageMax = n }(previewsPageMax)
	previewsPageMax = 2
	s, _ := newTestStore(t)
	mustUpsert(t, s, timelineRun("up1", RunTypeUp, _t0))
	for i := 1; i <= 5; i++ {
		mustUpsert(t, s, timelineRun(fmt.Sprintf("pv%d", i), RunTypePreview, _t0.Add(time.Duration(i)*time.Minute)))
	}
	seen := map[string]int{}
	var before *Cursor
	for page := 0; page < 10; page++ {
		c, p, next, err := s.ListTimeline(t.Context(), "ns", "s", before, 50)
		must(t, err)
		for _, r := range append(c, p...) {
			seen[r.UpdateName]++
		}
		if next == nil {
			break
		}
		before = next
	}
	for _, n := range []string{"up1", "pv1", "pv2", "pv3", "pv4", "pv5"} {
		if seen[n] != 1 {
			t.Errorf("%s seen %d times, want 1 (all: %v)", n, seen[n], seen)
		}
	}
}

func TestSaveLogPublishesStackEvent(t *testing.T) {
	s, pub := newTestStore(t)
	mustUpsert(t, s, finished("u1", _t0))
	id := getRunByName(t, s, "ns", "u1").ID
	pub.reset()
	must(t, s.SaveLog(t.Context(), id, LogStatusCaptured, map[string]int64{"update": 1}, nil, false))
	found := false
	for _, k := range pub.kinds() {
		found = found || k == events.KindStack
	}
	if !found {
		t.Fatalf("events = %v, want a stack event so folded previews re-render", pub.kinds())
	}
}

// A countless cursor (written by the previous version) is replaced by the first counted
// write, even from behind it, so renumbering happens once rather than on every tick.
func TestCountedCursorReplacesCountless(t *testing.T) {
	s, _ := newTestStore(t)
	must(t, s.SetHistoryCursor(t.Context(), "b", "p/", "p/k9", 0, _t0))
	must(t, s.SetHistoryCursor(t.Context(), "b", "p/", "p/k3", 3, _t0))
	key, n, err := s.HistoryCursor(t.Context(), "b", "p/")
	must(t, err)
	if key != "p/k3" || n != 3 {
		t.Fatalf("cursor = %q %d, want p/k3 3", key, n)
	}
}
