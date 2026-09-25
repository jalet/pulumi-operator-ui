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
		{give: "up,refresh,destroy,import", want: nil}, // the default set
		{give: "import", want: []store.RunType{store.RunTypeImport}},
		{give: "UP", wantErr: true},
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
	if got := typesQuery(toggleType(def, store.RunTypePreview)); got != "up,refresh,destroy,import,preview" {
		t.Errorf("add preview to default = %q", got)
	}
	if got := typesQuery(toggleType(def, store.RunTypeUp)); got != "refresh,destroy,import" {
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
		{map[string]int64{"read": 3, "same": 4}, "read 3"},
		{map[string]int64{"create": 1, "discard": 2}, "+1 discard 2"},
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

func TestShortType(t *testing.T) {
	for give, want := range map[string]string{
		"aws:iam/userPolicy:UserPolicy":                    "iam/UserPolicy",
		"vault:kubernetes/authBackendRole:AuthBackendRole": "kubernetes/AuthBackendRole",
		"random:index/randomId:RandomId":                   "RandomId",
		"custom:thing":                                     "custom:thing",
	} {
		if got := shortType(give); got != want {
			t.Errorf("shortType(%q) = %q, want %q", give, got, want)
		}
	}
}

func TestOperatorMessage(t *testing.T) {
	for give, want := range map[string]string{
		`"New commit detected: \"2ea4\""`: `New commit detected: "2ea4"`,
		`up failed: exit status 1`:        `up failed: exit status 1`,
		`"unterminated`:                   `"unterminated`,
	} {
		if got := operatorMessage(give); got != want {
			t.Errorf("operatorMessage(%q) = %q, want %q", give, got, want)
		}
	}
}

func TestResourceRowsToneDiffLines(t *testing.T) {
	rows := resourceRows([]store.LogResource{{Op: "update", Type: "aws:iam/userPolicy:UserPolicy",
		Name: "p", Diff: "~ policy: {\n  + Sid: \"A\"\n  - Old: 1\n  same: 2"}})
	if len(rows) != 1 || rows[0].Tone != "run" || rows[0].ShortType != "iam/UserPolicy" {
		t.Fatalf("rows = %+v", rows)
	}
	var tones []string
	for _, l := range rows[0].Lines {
		tones = append(tones, l.Tone)
	}
	if diff := cmp.Diff([]string{"upd", "add", "del", ""}, tones); diff != "" {
		t.Errorf("tones (-want +got):\n%s", diff)
	}
}

func TestChangesNote(t *testing.T) {
	up := store.Run{Type: store.RunTypeUp, UpdateName: "prod-1"}
	tests := []struct {
		name string
		give store.Run
		s3   time.Duration
		want string
	}{
		{"pending", with(up, func(r *store.Run) { r.LogStatus = store.LogStatusPending }), 0,
			"Reading the engine log..."},
		{"captured nothing", with(up, func(r *store.Run) {
			r.LogStatus, r.LogChanges = store.LogStatusCaptured, map[string]int64{"same": 3}
		}), 0, "No resources changed."},
		{"unavailable, waiting for s3", with(up, func(r *store.Run) {
			r.LogStatus = store.LogStatusUnavailable
		}), 2 * time.Minute, "The engine log was no longer available. Waiting for Pulumi history, read every 2 minutes."},
		{"unavailable preview", with(up, func(r *store.Run) {
			r.Type, r.LogStatus = store.RunTypePreview, store.LogStatusUnavailable
		}), 2 * time.Minute, "The engine log was no longer available."},
		{"imported", with(up, func(r *store.Run) {
			r.UpdateName, r.Changes = "s3:prod-1", map[string]int64{"same": 1}
		}), 0, "Only counts are available for this run."},
		{"nothing at all", up, 0, "No change details are available for this run."},
		{"resources listed", with(up, func(r *store.Run) {
			r.LogStatus = store.LogStatusCaptured
			r.Resources = []store.LogResource{{Op: "create"}}
		}), 0, ""},
	}
	for _, tt := range tests {
		if got := changesNote(tt.give, tt.s3, true); got != tt.want {
			t.Errorf("%s: changesNote = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestChangesNoteWaitingWording(t *testing.T) {
	r := store.Run{Type: store.RunTypeUp, LogStatus: store.LogStatusUnavailable}
	const gone = "The engine log was no longer available."
	for _, tt := range []struct {
		interval time.Duration
		s3Stack  bool
		want     string
	}{
		{time.Minute, true, gone + " Waiting for Pulumi history, read every minute."},
		{90 * time.Second, true, gone + " Waiting for Pulumi history, read every 1m30s."},
		{5 * time.Minute, true, gone + " Waiting for Pulumi history, read every 5 minutes."},
		{2 * time.Minute, false, gone}, // the Stack has no S3 backend
	} {
		if got := changesNote(r, tt.interval, tt.s3Stack); got != tt.want {
			t.Errorf("interval %v s3Stack %v: %q, want %q", tt.interval, tt.s3Stack, got, tt.want)
		}
	}
}

func with(r store.Run, f func(*store.Run)) store.Run { f(&r); return r }
func TestOriginBadge(t *testing.T) {
	for _, tt := range []struct {
		name string
		give store.Run
		want *originBadge
	}{
		{"laptop", store.Run{ExecKind: "cli", ExecAgent: "a"}, &originBadge{"laptop", "att", "a"}},
		{"operator from history", store.Run{ExecKind: "auto.local"}, &originBadge{"operator", "mute", ""}},
		{"operator before history", store.Run{UID: "u"}, &originBadge{"operator", "mute", ""}},
		{"other value", store.Run{ExecKind: "remote"}, &originBadge{"remote", "mute", ""}},
		{"unknown import", store.Run{UpdateName: "s3:x"}, nil},
	} {
		if diff := cmp.Diff(tt.want, origin(tt.give)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tt.name, diff)
		}
	}
}

func TestChangeChipsImport(t *testing.T) {
	got := changeChips(map[string]int64{"import": 3, "same": 1})
	if len(got) != 2 || got[0] != (changeChip{Label: "imported", Count: 3, Tone: "run"}) {
		t.Fatalf("chips = %+v", got)
	}
	if s := changeSummary(map[string]int64{"import": 3}); s != "↓3" {
		t.Errorf("summary = %q", s)
	}
}
