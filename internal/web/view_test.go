package web

import (
	"errors"
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

func TestNoStart(t *testing.T) {
	tests := []struct {
		give store.RunState
		want string
	}{
		{store.RunStatePending, "not started"},
		{store.RunStateRunning, "not started"},
		{store.RunStateSucceeded, "not recorded"},
		{store.RunStateFailed, "not recorded"},
	}
	for _, tt := range tests {
		if got := noStart(tt.give); got != tt.want {
			t.Errorf("noStart(%s) = %q, want %q", tt.give, got, tt.want)
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

func TestParseTypes(t *testing.T) {
	tests := []struct {
		give    string
		want    []store.RunType
		wantErr bool
	}{
		{give: "", want: nil},
		{give: "up", want: []store.RunType{store.RunTypeUp}},
		{give: "preview,up", want: []store.RunType{store.RunTypeUp, store.RunTypePreview}},
		{give: "up,up,", want: []store.RunType{store.RunTypeUp}},
		{give: ",", want: nil},
		{give: "up,refresh,destroy", want: nil}, // the default set
		{give: "UP", wantErr: true},
		{give: "import", wantErr: true},
		{give: "up,bogus", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseTypes(tt.give)
		if tt.wantErr {
			if !errors.Is(err, errBadTypes) {
				t.Errorf("parseTypes(%q) err = %v, want errBadTypes", tt.give, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTypes(%q): %v", tt.give, err)
			continue
		}
		if diff := cmp.Diff(tt.want, got); diff != "" {
			t.Errorf("parseTypes(%q) (-want +got):\n%s", tt.give, diff)
		}
	}
}

func TestTypesQuery(t *testing.T) {
	if got := typesQuery(nil); got != "" {
		t.Errorf("default = %q", got)
	}
	if got := typesQuery(store.DefaultRunTypes); got != "" {
		t.Errorf("explicit default = %q, want empty", got)
	}
	if got := typesQuery([]store.RunType{store.RunTypePreview, store.RunTypeUp}); got != "up,preview" {
		t.Errorf("got %q, want canonical order up,preview", got)
	}
}

func TestToggleType(t *testing.T) {
	var def []store.RunType
	if got := typesQuery(toggleType(def, store.RunTypePreview)); got != "up,refresh,destroy,preview" {
		t.Errorf("add preview to default = %q", got)
	}
	if got := typesQuery(toggleType(def, store.RunTypeUp)); got != "refresh,destroy" {
		t.Errorf("remove up from default = %q", got)
	}
	if got := toggleType([]store.RunType{store.RunTypeUp}, store.RunTypeUp); got != nil {
		t.Errorf("removing the last type = %v, want nil (default)", got)
	}
}

func TestSuccessRate(t *testing.T) {
	tests := []struct {
		give store.StackStats
		want string
	}{
		{store.StackStats{}, "-"},
		{store.StackStats{Total: 3}, "-"}, // only running or pending
		{store.StackStats{Total: 2, Succeeded: 2}, "100%"},
		{store.StackStats{Total: 3, Succeeded: 2, Failed: 1}, "67%"},
		{store.StackStats{Total: 1, Failed: 1}, "0%"},
	}
	for _, tt := range tests {
		if got := successRate(tt.give); got != tt.want {
			t.Errorf("successRate(%+v) = %q, want %q", tt.give, got, tt.want)
		}
	}
}

func TestChangeChips(t *testing.T) {
	got := changeChips(map[string]int64{"same": 5, "delete": 1, "create": 2, "read": 3, "replace": 0})
	want := []changeChip{{"created", 2, "ok"}, {"deleted", 1, "bad"}, {"unchanged", 5, "mute"},
		{"read", 3, "mute"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if changeChips(nil) != nil {
		t.Error("nil counts should give no chips")
	}
}

func TestChangeSummary(t *testing.T) {
	tests := []struct {
		give map[string]int64
		want string
	}{
		{map[string]int64{"create": 2, "delete": 1, "same": 9}, "+2 -1"},
		{map[string]int64{"update": 1, "replace": 3}, "~1 ±3"},
		{map[string]int64{"same": 4}, "no changes"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := changeSummary(tt.give); got != tt.want {
			t.Errorf("changeSummary(%v) = %q, want %q", tt.give, got, tt.want)
		}
	}
}

func TestImported(t *testing.T) {
	if !imported("s3:prod-1") || imported("prod-1a0d") {
		t.Fatal("imported() misclassifies update names")
	}
}
