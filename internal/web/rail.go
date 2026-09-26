package web

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

type railNode struct {
	Run    store.Run
	Drift  bool // a preview that planned changes
	Latest bool // the stack's newest state change, on the live page: never grouped
}

type railItem struct {
	Node *railNode   // exactly one of Node, Fold and Group is set
	Fold []store.Run // clean previews between two nodes, newest first
	// Group is two or more ups without changes in a row (with any preview folds between
	// them), shown as one range node that expands to these items.
	Group []railItem
	// FoldID is the oldest run in Fold or Group: stable while newer runs join it, so the
	// page can reopen a fold or group the viewer had open after a live update.
	FoldID int64
}

// GroupUps returns the group's ups, newest first.
func (it railItem) GroupUps() []store.Run {
	var ups []store.Run
	for _, g := range it.Group {
		if g.Node != nil {
			ups = append(ups, g.Node.Run)
		}
	}
	return ups
}

// GroupRange is "#66-71" for the group's numbered ups, or "" when they have no numbers.
func (it railItem) GroupRange() string {
	ups := it.GroupUps()
	newest, oldest := ups[0].Seq, ups[len(ups)-1].Seq
	if newest == nil || oldest == nil {
		return ""
	}
	return fmt.Sprintf("#%d-%d", *oldest, *newest)
}

type railDay struct {
	Label string
	Items []railItem
}

func sortTime(r store.Run) time.Time {
	if r.StartedAt != nil {
		return *r.StartedAt
	}
	return r.ObservedAt
}

// drift reports whether a preview planned changes.
func drift(r store.Run) bool {
	for op, n := range r.LogChanges {
		if op != "same" && n > 0 {
			return true
		}
	}
	return false
}

// buildRail merges state changes and previews, newest first, into day groups. Clean
// previews between two nodes fold into one item unless expand is set; a failed preview or
// one that planned changes is always its own node.
// buildRail lays out a page of runs, newest first, by day. Clean previews fold between
// nodes, and unless expand is set, ups without changes group into range nodes. On the live
// page (latest) the stack's newest state change is always its own node.
func buildRail(changes, previews []store.Run, now time.Time, loc *time.Location,
	expand, latest bool) []railDay {
	all := append(slices.Clone(changes), previews...)
	slices.SortStableFunc(all, func(a, b store.Run) int {
		if c := sortTime(b).Compare(sortTime(a)); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	var days []railDay
	keepLatest := latest
	for _, r := range all {
		label := dayLabel(sortTime(r), now, loc)
		if len(days) == 0 || days[len(days)-1].Label != label {
			days = append(days, railDay{Label: label})
		}
		d := &days[len(days)-1]
		foldable := r.Type == store.RunTypePreview && !drift(r) &&
			(r.State == store.RunStateSucceeded) // failed, running and pending previews are nodes
		if foldable && !expand {
			if n := len(d.Items); n > 0 && d.Items[n-1].Fold != nil {
				d.Items[n-1].Fold = append(d.Items[n-1].Fold, r)
				d.Items[n-1].FoldID = r.ID // runs come newest first: the last one is the oldest
			} else {
				d.Items = append(d.Items, railItem{Fold: []store.Run{r}, FoldID: r.ID})
			}
			continue
		}
		node := &railNode{Run: r, Drift: r.Type == store.RunTypePreview && drift(r)}
		if keepLatest && r.Type != store.RunTypePreview {
			node.Latest, keepLatest = true, false // runs come newest first
		}
		d.Items = append(d.Items, railItem{Node: node})
	}
	if !expand {
		for i := range days {
			days[i].Items = groupNoChange(days[i].Items)
		}
	}
	return days
}

// noChangeUp reports whether r is a succeeded up whose counts show nothing but unchanged
// resources. Unknown counts (log pending or unavailable) are not "no changes".
func noChangeUp(r store.Run) bool {
	if r.Type != store.RunTypeUp || r.State != store.RunStateSucceeded {
		return false
	}
	counts := timelineCounts(r)
	if counts == nil {
		return false
	}
	for op, n := range counts {
		if op != "same" && n > 0 {
			return false
		}
	}
	return true
}

func groupable(it railItem) bool {
	return it.Node != nil && !it.Node.Latest && noChangeUp(it.Node.Run)
}

// groupNoChange folds runs of two or more no-change ups within one day into a group. Preview
// folds between them join the group; folds at its edges stay outside.
func groupNoChange(items []railItem) []railItem {
	var out []railItem
	for i := 0; i < len(items); {
		if !groupable(items[i]) {
			out = append(out, items[i])
			i++
			continue
		}
		end, ups := i, 0 // end is one past the last groupable node
		for j := i; j < len(items) && (groupable(items[j]) || items[j].Fold != nil); j++ {
			if items[j].Node != nil {
				end, ups = j+1, ups+1
			}
		}
		if ups < 2 {
			out = append(out, items[i])
			i++
			continue
		}
		g := slices.Clone(items[i:end])
		out = append(out, railItem{Group: g, FoldID: g[len(g)-1].Node.Run.ID})
		i = end
	}
	return out
}

// dayLabel is "Today", "Yesterday", "Tue 1 Sep", or with the year when it differs from now.
func dayLabel(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	today := time.Date(ny, nm, nd, 0, 0, 0, 0, loc)
	day := time.Date(ty, tm, td, 0, 0, 0, 0, loc)
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case ty == ny:
		return t.Format("Mon 2 Jan")
	default:
		return t.Format("Mon 2 Jan 2006")
	}
}
