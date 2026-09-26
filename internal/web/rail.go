package web

import (
	"cmp"
	"slices"
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

type railNode struct {
	Run   store.Run
	Drift bool // a preview that planned changes
}

type railItem struct {
	Node *railNode   // exactly one of Node and Fold is set
	Fold []store.Run // clean previews between two nodes, newest first
	// FoldID is the oldest run in Fold: stable while newer previews join the fold, so the
	// page can reopen a fold the viewer had open after a live update.
	FoldID int64
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
func buildRail(changes, previews []store.Run, now time.Time, loc *time.Location,
	expand bool) []railDay {
	all := append(slices.Clone(changes), previews...)
	slices.SortStableFunc(all, func(a, b store.Run) int {
		if c := sortTime(b).Compare(sortTime(a)); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	var days []railDay
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
		d.Items = append(d.Items, railItem{Node: &railNode{Run: r,
			Drift: r.Type == store.RunTypePreview && drift(r)}})
	}
	return days
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
