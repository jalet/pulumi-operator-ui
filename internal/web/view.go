package web

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

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
	// OverviewURL and CountersURL are the live-refresh fragment URLs, query-escaped here
	// because html/template does not treat hx-get as a URL attribute.
	OverviewURL string
	CountersURL string
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
	query := ""
	if ns != "" {
		query = "?ns=" + url.QueryEscape(ns)
	}
	return listPage{
		Stacks: shown, Counts: c, Namespaces: namespaces, Chips: chips, NS: ns,
		OverviewURL: "/fragments/stacks" + query, CountersURL: "/fragments/stacks/counters" + query,
		Counters: []counterView{
			{Label: "Stacks", Value: c.Total},
			{Label: "Ready", Value: c.Ready, Tone: "ok"},
			{Label: "Reconciling", Value: c.Reconciling, Tone: "run"},
			{Label: "Needs attention", Value: c.Attention, Tone: "att"},
		},
	}
}

var errBadTypes = errors.New("invalid types")

// parseTypes parses ?types=: lowercase known types, comma-separated, duplicates collapsed,
// empty items ignored. It returns nil (the default set) for an empty or default value.
func parseTypes(raw string) ([]store.RunType, error) {
	var set []store.RunType
	for part := range strings.SplitSeq(raw, ",") {
		if part == "" {
			continue
		}
		t := store.RunType(part)
		if !slices.Contains(store.AllRunTypes, t) {
			return nil, fmt.Errorf("%w: %q", errBadTypes, part)
		}
		if !slices.Contains(set, t) {
			set = append(set, t)
		}
	}
	return canonical(set), nil
}

// canonical orders ts as store.AllRunTypes does and maps the default set to nil.
func canonical(ts []store.RunType) []store.RunType {
	var out []store.RunType
	for _, t := range store.AllRunTypes {
		if slices.Contains(ts, t) {
			out = append(out, t)
		}
	}
	if len(out) == 0 || slices.Equal(out, store.DefaultRunTypes) {
		return nil
	}
	return out
}

// typesQuery is the canonical ?types= value for ts ("" for the default set).
func typesQuery(ts []store.RunType) string {
	ts = canonical(ts)
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = string(t)
	}
	return strings.Join(parts, ",")
}

// toggleType returns the set with t added or removed; an empty result means the default.
func toggleType(ts []store.RunType, t store.RunType) []store.RunType {
	eff := store.RunFilter{Types: ts}.EffectiveTypes()
	next := slices.DeleteFunc(slices.Clone(eff), func(x store.RunType) bool { return x == t })
	if len(next) == len(eff) {
		next = append(next, t)
	}
	return canonical(next)
}

// successRate formats succeeded/(succeeded+failed), rounded, or "-" with no finished runs.
func successRate(st store.StackStats) string {
	done := st.Succeeded + st.Failed
	if done == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", (st.Succeeded*100+done/2)/done)
}

// withTypes appends ?types= to base unless ts is the default set.
func withTypes(base string, ts []store.RunType) string {
	if q := typesQuery(ts); q != "" {
		return base + "?types=" + q
	}
	return base
}

// typeChips builds one toggle chip per run type for the stack page at base.
func typeChips(base string, ts []store.RunType, st store.StackStats) []chip {
	eff := store.RunFilter{Types: ts}.EffectiveTypes()
	chips := make([]chip, 0, len(store.AllRunTypes))
	for _, t := range store.AllRunTypes {
		label := strings.ToUpper(string(t[:1])) + string(t[1:])
		if t == store.RunTypePreview && st.HiddenPreviews > 0 {
			label += fmt.Sprintf(" (%d hidden)", st.HiddenPreviews)
		}
		chips = append(chips, chip{Label: label, Href: withTypes(base, toggleType(ts, t)),
			On: slices.Contains(eff, t)})
	}
	return chips
}

// changeChip is one change count on the run page.
type changeChip struct {
	Label string
	Count int64
	Tone  string
}

// _changeOps is the display order, label, tone and summary symbol of the usual operations.
var _changeOps = []struct{ op, label, tone, symbol string }{
	{"create", "created", "ok", "+"},
	{"update", "updated", "run", "~"},
	{"delete", "deleted", "bad", "-"},
	{"replace", "replaced", "att", "±"},
	{"same", "unchanged", "mute", ""},
}

// changeChips lists non-zero counts: the usual operations first, then any others by name.
func changeChips(counts map[string]int64) []changeChip {
	var out []changeChip
	known := map[string]bool{}
	for _, o := range _changeOps {
		known[o.op] = true
		if n := counts[o.op]; n > 0 {
			out = append(out, changeChip{Label: o.label, Count: n, Tone: o.tone})
		}
	}
	var others []string
	for op, n := range counts {
		if !known[op] && n > 0 {
			others = append(others, op)
		}
	}
	slices.Sort(others)
	for _, op := range others {
		out = append(out, changeChip{Label: op, Count: counts[op], Tone: "mute"})
	}
	return out
}

// changeSummary is the timeline's compact form, for example "+2 -1"; "" when unknown.
func changeSummary(counts map[string]int64) string {
	if counts == nil {
		return ""
	}
	var parts []string
	for _, o := range _changeOps {
		if n := counts[o.op]; n > 0 && o.symbol != "" {
			parts = append(parts, fmt.Sprintf("%s%d", o.symbol, n))
		}
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, " ")
}

// imported reports whether a run was created from S3 history rather than seen as an Update.
func imported(updateName string) bool { return strings.HasPrefix(updateName, "s3:") }
