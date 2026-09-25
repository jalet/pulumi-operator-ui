package web

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

func TestHealth(t *testing.T) {
	deleted := time.Now()
	tests := []struct {
		name string
		give store.Stack
		want badge
	}{
		{"deleted wins", store.Stack{DeletedAt: &deleted, Stalled: true, Ready: true},
			badge{"Deleted", "mute"}},
		{"stalled beats ready", store.Stack{Stalled: true, Ready: true}, badge{"Stalled", "bad"}},
		{"stalled beats reconciling", store.Stack{Stalled: true, Reconciling: true},
			badge{"Stalled", "bad"}},
		{"reconciling beats ready", store.Stack{Reconciling: true, Ready: true},
			badge{"Reconciling", "run"}},
		{"ready", store.Stack{Ready: true}, badge{"Ready", "ok"}},
		{"nothing set", store.Stack{}, badge{"Not ready", "att"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := health(store.StackSummary{Stack: tt.give}); got != tt.want {
				t.Fatalf("health = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStateBadge(t *testing.T) {
	tests := []struct {
		give store.RunState
		want badge
	}{
		{store.RunStateSucceeded, badge{"succeeded", "ok"}},
		{store.RunStateFailed, badge{"failed", "bad"}},
		{store.RunStateRunning, badge{"running", "run"}},
		{store.RunStatePending, badge{"pending", "mute"}},
	}
	for _, tt := range tests {
		if got := stateBadge(tt.give); got != tt.want {
			t.Errorf("stateBadge(%s) = %+v, want %+v", tt.give, got, tt.want)
		}
	}
}

func TestCountStacks(t *testing.T) {
	give := []store.StackSummary{
		{Stack: store.Stack{Ready: true}}, {Stack: store.Stack{Ready: true}},
		{Stack: store.Stack{Reconciling: true}}, {Stack: store.Stack{Stalled: true}}, {},
	}
	want := counters{Total: 5, Ready: 2, Reconciling: 1, Attention: 2}
	if got := countStacks(give); got != want {
		t.Fatalf("countStacks = %+v, want %+v", got, want)
	}
}

func TestBuildListPage(t *testing.T) {
	all := []store.StackSummary{
		{Stack: store.Stack{Namespace: "infra", Name: "net", Ready: true}},
		{Stack: store.Stack{Namespace: "pulumi", Name: "prod", Stalled: true}},
		{Stack: store.Stack{Namespace: "infra", Name: "dns", Reconciling: true}},
	}
	got := buildListPage(all, "infra")
	if len(got.Stacks) != 2 || got.NS != "infra" {
		t.Fatalf("stacks = %d, ns = %q", len(got.Stacks), got.NS)
	}
	if diff := cmp.Diff([]string{"infra", "pulumi"}, got.Namespaces); diff != "" {
		t.Errorf("namespaces (-want +got):\n%s", diff)
	}
	if want := (counters{Total: 2, Ready: 1, Reconciling: 1}); got.Counts != want {
		t.Errorf("counts = %+v, want %+v", got.Counts, want)
	}
	none := buildListPage(all, "nope")
	if len(none.Stacks) != 0 || none.Counts.Total != 0 || len(none.Namespaces) != 2 {
		t.Errorf("unknown namespace: %+v", none)
	}
	if every := buildListPage(all, ""); len(every.Stacks) != 3 || every.Counts.Attention != 1 {
		t.Errorf("all: %+v", every)
	}
}
