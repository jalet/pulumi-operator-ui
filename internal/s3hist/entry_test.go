package s3hist

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var _target = Target{Namespace: "ns", Stack: "app", Bucket: "b", Region: "eu-north-1",
	Prefix: "p/.pulumi/history/proj/dev/"}

const _key = "p/.pulumi/history/proj/dev/dev-1790239732000000000.history.json"

func fixture(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/update-succeeded.history.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseEntry(t *testing.T) {
	got, err := ParseEntry(_target, _key, fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	want := store.HistoryEntry{Key: _key, Bucket: "b", Namespace: "ns", StackName: "app",
		Type: store.RunTypeUp, State: store.RunStateSucceeded,
		StartedAt: time.Unix(1790239727, 0).UTC(), EndedAt: time.Unix(1790239732, 0).UTC(),
		Commit: "0123456789abcdef0123456789abcdef01234567",
		Counts: map[string]int64{"create": 2, "delete": 1, "same": 108}, Message: "deploy"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if s := fmt.Sprintf("%+v", got); strings.Contains(s, "person@example.com") ||
		strings.Contains(s, "v1:AAAA") || strings.Contains(s, "A Person") {
		t.Fatalf("sensitive field leaked into the entry: %s", s)
	}
}

func TestParseEntryRejects(t *testing.T) {
	base := string(fixture(t))
	tests := []struct {
		name    string
		give    string
		wantErr error
	}{
		{"preview kind", strings.Replace(base, `"update"`, `"preview"`, 1), ErrBadKind},
		{"in-progress result", strings.Replace(base, `"succeeded"`, `"in-progress"`, 1), ErrBadResult},
		{"too large", base + strings.Repeat(" ", historyBytesMax), ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseEntry(_target, _key, []byte(tt.give)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if _, err := ParseEntry(_target, _key, []byte("{not json")); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	for kind, want := range map[string]store.RunType{"refresh": store.RunTypeRefresh,
		"destroy": store.RunTypeDestroy} {
		e, err := ParseEntry(_target, _key, []byte(strings.Replace(base, `"update"`, `"`+kind+`"`, 1)))
		if err != nil || e.Type != want {
			t.Errorf("%s: type %q err %v", kind, e.Type, err)
		}
	}
}

func FuzzParseEntry(f *testing.F) {
	f.Add(fixture(f))
	f.Add([]byte(`{}`))
	f.Fuzz(func(_ *testing.T, body []byte) { _, _ = ParseEntry(_target, _key, body) })
}
func TestParseEntryImportWithOrigin(t *testing.T) {
	b, err := os.ReadFile("testdata/import-cli.history.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEntry(_target, _key, b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != store.RunTypeImport || got.ExecKind != "cli" || got.ExecAgent != "some-agent" ||
		got.Message != "feat(net): import the legacy VPC" || got.VCSRepo != "github.com/o/r" ||
		got.Counts["import"] != 3 {
		t.Fatalf("entry = %+v", got)
	}
	if s := fmt.Sprintf("%+v", got); strings.Contains(s, "person@example.com") ||
		strings.Contains(s, "A Person") || strings.Contains(s, "v1:AAAA") {
		t.Fatalf("sensitive field leaked: %s", s)
	}
}

func TestParseEntryCaps(t *testing.T) {
	long := strings.Repeat("€", 100) // 300 bytes of 3-byte runes: 200 falls mid-rune
	body := fmt.Sprintf(`{"kind":"update","startTime":1,"endTime":2,"result":"succeeded",`+
		`"message":%q,"environment":{"exec.kind":%q}}`, long, strings.Repeat("k", 40))
	got, err := ParseEntry(_target, _key, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Message) != 198 || !utf8.ValidString(got.Message) {
		t.Errorf("message: %d bytes, valid UTF-8 %v", len(got.Message), utf8.ValidString(got.Message))
	}
	if len(got.ExecKind) != 32 {
		t.Errorf("exec kind = %d bytes, want 32", len(got.ExecKind))
	}
}

func TestParseEntryVCSRepoNeedsAllParts(t *testing.T) {
	body := `{"kind":"update","startTime":1,"endTime":2,"result":"succeeded",` +
		`"environment":{"vcs.kind":"github.com","vcs.owner":"o"}}`
	got, err := ParseEntry(_target, _key, []byte(body))
	if err != nil || got.VCSRepo != "" {
		t.Fatalf("vcs repo = %q err %v, want empty without vcs.repo", got.VCSRepo, err)
	}
}
