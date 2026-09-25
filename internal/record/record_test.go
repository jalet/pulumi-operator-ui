package record

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var (
	_now     = time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC)
	_started = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
)

func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func load(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	var obj unstructured.Unstructured
	if err := obj.UnmarshalJSON(readFixture(t, name)); err != nil {
		t.Fatal(err)
	}
	return &obj
}

func ptr(t time.Time) *time.Time { return &t }

func TestRunFromUpdate(t *testing.T) {
	tests := []struct {
		give    string
		want    store.Run
		wantErr error
	}{
		{
			give: "update-progressing",
			want: store.Run{Namespace: "ns", UpdateName: "update-progressing",
				UID: "uid-update-progressing", StackName: "app", Type: store.RunTypeUp,
				Commit: "c1", CommitSource: store.CommitSourceStack,
				State: store.RunStateRunning, StartedAt: ptr(_started), ObservedAt: _now},
		},
		{
			give: "update-succeeded",
			want: store.Run{Namespace: "ns", UpdateName: "update-succeeded",
				UID: "uid-update-succeeded", StackName: "app", Type: store.RunTypeUp,
				Commit: "c1", CommitSource: store.CommitSourceStack,
				State: store.RunStateSucceeded, Message: "ok", StartedAt: ptr(_started),
				EndedAt: ptr(_started.Add(5 * time.Minute)), ObservedAt: _now},
		},
		{
			give: "update-failed",
			want: store.Run{Namespace: "ns", UpdateName: "update-failed",
				UID: "uid-update-failed", StackName: "app", Type: store.RunTypePreview,
				Commit: "c1", CommitSource: store.CommitSourceStack,
				State: store.RunStateFailed, Message: "error: boom", StartedAt: ptr(_started),
				EndedAt: ptr(_started.Add(time.Minute)), ObservedAt: _now},
		},
		{
			give: "update-no-conditions",
			want: store.Run{Namespace: "ns", UpdateName: "update-no-conditions",
				UID: "uid-update-no-conditions", StackName: "app", Type: store.RunTypeRefresh,
				Commit: "c1", CommitSource: store.CommitSourceStack,
				State: store.RunStatePending, ObservedAt: _now},
		},
		{give: "update-no-owner", wantErr: ErrNoStackOwner},
		{give: "update-type-import", wantErr: ErrUnknownType},
	}
	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			got, err := RunFromUpdate(load(t, tt.give), "c1", store.CommitSourceStack, _now)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRunFromUpdateWithoutStackCommit(t *testing.T) {
	got, err := RunFromUpdate(load(t, "update-progressing"), "", "", _now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != "" || got.CommitSource != "" {
		t.Fatalf("commit = %q/%q, want empty", got.Commit, got.CommitSource)
	}
}

func TestRunFromUpdateTruncatesMessage(t *testing.T) {
	obj := load(t, "update-failed")
	long := strings.Repeat("é", 10<<10) // 20 KiB of two-byte runes
	if err := unstructured.SetNestedField(obj.Object, long, "status", "message"); err != nil {
		t.Fatal(err)
	}
	got, err := RunFromUpdate(obj, "c1", store.CommitSourceStack, _now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Message) > messageBytesMax || !utf8.ValidString(got.Message) {
		t.Fatalf("message len %d, valid utf8 %v", len(got.Message), utf8.ValidString(got.Message))
	}
}

func TestSkipReason(t *testing.T) {
	tests := []struct {
		give error
		want string
	}{
		{give: ErrNoStackOwner, want: "no_stack_owner"},
		{give: ErrUnknownType, want: "unknown_type"},
		{give: ErrUnknownState, want: "unknown_state"},
		{give: errors.New("other"), want: ""},
	}
	for _, tt := range tests {
		if got := SkipReason(tt.give); got != tt.want {
			t.Errorf("SkipReason(%v) = %q, want %q", tt.give, got, tt.want)
		}
	}
}

func TestStackFromObject(t *testing.T) {
	tests := []struct {
		give string
		want store.Stack
	}{
		{
			give: "stack-ready",
			want: store.Stack{Namespace: "ns", Name: "app", Ready: true, LastCommit: "bbb",
				UpdatedAt: _now},
		},
		{
			give: "stack-stalled",
			want: store.Stack{Namespace: "ns", Name: "app", Stalled: true, UpdatedAt: _now},
		},
	}
	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			got, err := StackFromObject(load(t, tt.give), _now)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStackOwner(t *testing.T) {
	if got, err := StackOwner(load(t, "update-succeeded")); err != nil || got != "app" {
		t.Fatalf("StackOwner = %q, %v", got, err)
	}
	if _, err := StackOwner(load(t, "update-no-owner")); !errors.Is(err, ErrNoStackOwner) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFromStackLastUpdate(t *testing.T) {
	got, ok, err := RunFromStackLastUpdate(load(t, "stack-ready"), _now)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := store.Run{Namespace: "ns", UpdateName: "app-u0", StackName: "app",
		Type: store.RunTypeUp, Commit: "aaa", CommitSource: store.CommitSourceStack,
		State: store.RunStateSucceeded, Message: "done", ObservedAt: _now}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}

	if _, ok, err := RunFromStackLastUpdate(load(t, "stack-stalled"), _now); ok || err != nil {
		t.Fatalf("no lastUpdate: ok=%v err=%v", ok, err)
	}
	_, _, err = RunFromStackLastUpdate(load(t, "stack-weird-state"), _now)
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
}

func fixtureNames(tb testing.TB) []string {
	tb.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		tb.Fatal(err)
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	return names
}

func FuzzRunFromUpdate(f *testing.F) {
	for _, name := range fixtureNames(f) {
		f.Add(readFixture(f, name))
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		var obj unstructured.Unstructured
		if err := obj.UnmarshalJSON(data); err != nil {
			return
		}
		obj.SetGroupVersionKind(UpdateGVK)
		_, _ = RunFromUpdate(&obj, "c", store.CommitSourceStack, time.Unix(0, 0)) // must not panic
	})
}

func FuzzRunFromStackLastUpdate(f *testing.F) {
	for _, name := range fixtureNames(f) {
		f.Add(readFixture(f, name))
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		var obj unstructured.Unstructured
		if err := obj.UnmarshalJSON(data); err != nil {
			return
		}
		obj.SetGroupVersionKind(StackGVK)
		_, _, _ = RunFromStackLastUpdate(&obj, time.Unix(0, 0))
		_, _ = StackFromObject(&obj, time.Unix(0, 0))
	})
}

func TestCommitFor(t *testing.T) {
	tests := []struct {
		name       string
		giveStack  string
		giveUpdate string
		wantCommit string
		wantSource store.CommitSource
	}{
		{name: "current update", giveStack: "stack-running", giveUpdate: "app-u2",
			wantCommit: "ccc", wantSource: store.CommitSourceUpdate},
		{name: "last update", giveStack: "stack-running", giveUpdate: "app-u1",
			wantCommit: "bbb", wantSource: store.CommitSourceUpdate},
		{name: "unrelated update falls back to last attempted", giveStack: "stack-running",
			giveUpdate: "app-u9", wantCommit: "bbb", wantSource: store.CommitSourceStack},
		{name: "no status", giveStack: "stack-stalled", giveUpdate: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commit, source := CommitFor(load(t, tt.giveStack), tt.giveUpdate)
			if commit != tt.wantCommit || source != tt.wantSource {
				t.Fatalf("CommitFor = %q/%q, want %q/%q", commit, source, tt.wantCommit,
					tt.wantSource)
			}
		})
	}
}

func TestRunFromUpdateExactCommit(t *testing.T) {
	got, err := RunFromUpdate(load(t, "update-progressing"), "ccc", store.CommitSourceUpdate,
		_now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != "ccc" || got.CommitSource != store.CommitSourceUpdate {
		t.Fatalf("commit = %q/%q", got.Commit, got.CommitSource)
	}
}
