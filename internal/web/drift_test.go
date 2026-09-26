package web

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// stack is a Stack deploying example-infra's prod stack from one backend, as prod and
// prod-drift both do.
func stack(ns, name string, preview bool) store.StackSummary {
	return store.StackSummary{Stack: store.Stack{Namespace: ns, Name: name, Preview: preview,
		BackendURL: "s3://state-bucket/pulumi/example", Project: "example-infra", PulumiStack: "prod"}}
}

func rowNames(rows []stackRow) []string {
	var out []string
	for _, r := range rows {
		name := r.Namespace + "/" + r.Name
		for _, d := range r.Drift {
			name += "+" + d.Detector.Name
		}
		out = append(out, name)
	}
	return out
}

func TestFoldPairsDetectorWithTheStackItPreviews(t *testing.T) {
	got := rowNames(foldDetectors([]store.StackSummary{stack("p", "prod", false),
		stack("p", "prod-drift", true)}))
	if diff := cmp.Diff([]string{"p/prod+prod-drift"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

// Only a preview Stack that deploys the same Pulumi stack in the same namespace is folded.
func TestFoldKeepsUnpairedStacks(t *testing.T) {
	other := stack("p", "staging-drift", true)
	other.PulumiStack = "staging"
	elsewhere := stack("q", "prod-drift", true)
	noBackend := stack("p", "local", true)
	noBackend.BackendURL = ""
	target := stack("p", "prod", false)
	target.BackendURL = ""
	got := rowNames(foldDetectors([]store.StackSummary{stack("p", "prod", false), other, elsewhere}))
	if diff := cmp.Diff([]string{"p/prod", "p/staging-drift", "q/prod-drift"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
	got = rowNames(foldDetectors([]store.StackSummary{target, noBackend}))
	if diff := cmp.Diff([]string{"p/prod", "p/local"}, got); diff != "" {
		t.Fatalf("empty backends must not pair (-want +got):\n%s", diff)
	}
}

func TestFoldAnnotationOverrides(t *testing.T) {
	ptr := func(s string) *string { return &s }
	cross := stack("checks", "prod-drift", true)
	cross.Watches = ptr("p/prod")
	optOut := stack("p", "pr-preview", true)
	optOut.Watches = ptr("")
	named := stack("p", "odd", true)
	named.PulumiStack, named.Watches = "unrelated", ptr("prod")
	onTarget := stack("p", "prod", false)
	onTarget.Watches = ptr("")
	got := rowNames(foldDetectors([]store.StackSummary{onTarget, cross, optOut, named}))
	want := []string{"p/prod+prod-drift+odd", "p/pr-preview"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestFoldDetectorWithTwoTargets(t *testing.T) {
	got := rowNames(foldDetectors([]store.StackSummary{stack("p", "prod", false),
		stack("p", "prod-copy", false), stack("p", "prod-drift", true)}))
	want := []string{"p/prod+prod-drift", "p/prod-copy+prod-drift"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestDriftStates(t *testing.T) {
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		last        *store.RunBrief
		label, tone string
	}{
		{nil, "not checked", "mute"},
		{&store.RunBrief{ID: 1, State: store.RunStateRunning, At: at}, "checking", "run"},
		{&store.RunBrief{ID: 1, State: store.RunStatePending, At: at}, "checking", "run"},
		{&store.RunBrief{ID: 1, State: store.RunStateSucceeded, At: at}, "no drift", "ok"},
		{&store.RunBrief{ID: 1, State: store.RunStateFailed, At: at}, "drift", "att"},
	} {
		d := stack("p", "prod-drift", true)
		d.LastPreview = tc.last
		got := newDrift(d)
		if got.Label != tc.label || got.Tone != tc.tone || got.Last != tc.last {
			t.Errorf("%v: got %q %q, want %q %q", tc.last, got.Label, got.Tone, tc.label, tc.tone)
		}
	}
}

// A detector is one stack on the list, not two; its failed check is an alarm the counters
// show on the stack it watches.
func TestCountersFoldDetectors(t *testing.T) {
	prod := stack("p", "prod", false)
	prod.Ready = true
	drift := stack("p", "prod-drift", true)
	drift.Ready = true
	drift.LastPreview = &store.RunBrief{ID: 1, State: store.RunStateFailed}
	c := countStacks(foldDetectors([]store.StackSummary{prod, drift}))
	if c.Total != 1 || c.Ready != 0 || c.Attention != 1 {
		t.Fatalf("counters %+v, want 1 stack needing attention", c)
	}
}
