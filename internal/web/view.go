package web

import "github.com/jalet/pulumi-operator-ui/internal/store"

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
