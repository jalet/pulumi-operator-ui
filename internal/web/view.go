package web

import (
	"net/url"
	"slices"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// badge is a status label with its tone, the suffix of a tone-* class in app.css:
// ok, run, att, bad or mute.
type badge struct{ Label, Tone string }

// health applies the spec's precedence: Deleted, Stalled, Reconciling, Ready, Not ready.
func health(s store.StackSummary) badge {
	switch {
	case s.DeletedAt != nil:
		return badge{"Deleted", "mute"}
	case s.Stalled:
		return badge{"Stalled", "bad"}
	case s.Reconciling:
		return badge{"Reconciling", "run"}
	case s.Ready:
		return badge{"Ready", "ok"}
	default:
		return badge{"Not ready", "att"}
	}
}

func stateBadge(st store.RunState) badge {
	switch st {
	case store.RunStateSucceeded:
		return badge{string(st), "ok"}
	case store.RunStateFailed:
		return badge{string(st), "bad"}
	case store.RunStateRunning:
		return badge{string(st), "run"}
	case store.RunStatePending:
		return badge{string(st), "mute"}
	default:
		panic("invariant violated: unknown run state " + string(st))
	}
}

type counters struct{ Total, Ready, Reconciling, Attention int }

// countStacks tallies the list page counters; Needs attention is Stalled plus Not ready.
func countStacks(stacks []store.StackSummary) counters {
	c := counters{Total: len(stacks)}
	for _, s := range stacks {
		switch health(s).Tone {
		case "ok":
			c.Ready++
		case "run":
			c.Reconciling++
		case "att", "bad":
			c.Attention++
		}
	}
	return c
}

// chip is a filter link; On marks the selected one.
type chip struct {
	Label, Href string
	On          bool
}

// counterView is one summary card; Tone colors the value (ok, run, att or "").
type counterView struct {
	Label string
	Value int
	Tone  string
}

type listPage struct {
	Stacks     []store.StackSummary
	Counts     counters
	Counters   []counterView
	Namespaces []string
	Chips      []chip
	NS         string
}

// buildListPage filters stacks to namespace ns ("" = all). Namespaces and chips always come
// from every stack, so the chips stay visible when a filter matches nothing.
func buildListPage(all []store.StackSummary, ns string) listPage {
	seen := map[string]bool{}
	var namespaces []string
	shown := all
	if ns != "" {
		shown = nil
	}
	for _, s := range all {
		if !seen[s.Namespace] {
			seen[s.Namespace] = true
			namespaces = append(namespaces, s.Namespace)
		}
		if ns != "" && s.Namespace == ns {
			shown = append(shown, s)
		}
	}
	slices.Sort(namespaces)
	c := countStacks(shown)
	chips := make([]chip, 0, len(namespaces)+1)
	chips = append(chips, chip{Label: "All", Href: "/", On: ns == ""})
	for _, n := range namespaces {
		chips = append(chips, chip{Label: n, Href: "/?ns=" + url.QueryEscape(n), On: n == ns})
	}
	return listPage{
		Stacks: shown, Counts: c, Namespaces: namespaces, Chips: chips, NS: ns,
		Counters: []counterView{
			{Label: "Stacks", Value: c.Total},
			{Label: "Ready", Value: c.Ready, Tone: "ok"},
			{Label: "Reconciling", Value: c.Reconciling, Tone: "run"},
			{Label: "Needs attention", Value: c.Attention, Tone: "att"},
		},
	}
}
