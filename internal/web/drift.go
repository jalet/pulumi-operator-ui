package web

import (
	"strings"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// A drift detector is a preview-only Stack that checks another Stack: PKO previews the same
// Pulumi stack on a schedule and fails when it would change anything. The list shows it
// folded into the Stack it checks, as that Stack's drift status, rather than as a row of its
// own that never deploys.

// stackRow is one row of the stack list: a Stack and the drift detectors that check it.
type stackRow struct {
	store.StackSummary
	Drift []driftView
}

// driftView is one detector's latest check, as the list and the stack page show it.
type driftView struct {
	Detector    store.StackSummary
	Label, Tone string          // "no drift" ok, "drift" att, "checking" run, "not checked" mute
	Last        *store.RunBrief // the latest check, nil before the first
}

func newDrift(d store.StackSummary) driftView {
	v := driftView{Detector: d, Last: d.LastPreview, Label: "not checked", Tone: "mute"}
	if d.LastPreview == nil {
		return v
	}
	switch d.LastPreview.State {
	case store.RunStateSucceeded:
		v.Label, v.Tone = "no drift", "ok"
	case store.RunStateFailed:
		// A detector runs with expectNoChanges, so a failed check is how drift shows up.
		v.Label, v.Tone = "drift", "att"
	default:
		v.Label, v.Tone = "checking", "run"
	}
	return v
}

// watches reports whether detector checks target. The pulumi-operator-ui/watches annotation
// decides when present ("name" in the detector's namespace, "namespace/name", or "" for none);
// otherwise a detector checks the Stack in its namespace that deploys the same Pulumi stack
// from the same backend.
func watches(detector, target store.StackSummary) bool {
	if !detector.Preview || target.Preview {
		return false
	}
	if w := detector.Watches; w != nil {
		ns, name, found := strings.Cut(*w, "/")
		if !found {
			ns, name = detector.Namespace, *w
		}
		return name != "" && ns == target.Namespace && name == target.Name
	}
	return detector.Namespace == target.Namespace && detector.BackendURL != "" &&
		detector.BackendURL == target.BackendURL && detector.Project != "" &&
		detector.Project == target.Project && detector.PulumiStack != "" &&
		detector.PulumiStack == target.PulumiStack
}

// foldDetectors turns stacks into list rows: a detector that checks a listed Stack becomes
// that Stack's drift status instead of a row; one that checks nothing listed stays a row.
func foldDetectors(stacks []store.StackSummary) []stackRow {
	folded := map[int]bool{}
	rows := make([]stackRow, 0, len(stacks))
	for _, target := range stacks {
		row := stackRow{StackSummary: target}
		for i, d := range stacks {
			if watches(d, target) {
				row.Drift = append(row.Drift, newDrift(d))
				folded[i] = true
			}
		}
		rows = append(rows, row)
	}
	out := rows[:0]
	for i, r := range rows {
		if !folded[i] {
			out = append(out, r)
		}
	}
	return out
}

// drifted reports whether any of the row's detectors found drift.
func (r stackRow) drifted() bool {
	for _, d := range r.Drift {
		if d.Tone == "att" {
			return true
		}
	}
	return false
}
