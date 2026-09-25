package logs

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func golden(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestParseUpdate(t *testing.T) {
	got := Parse(golden(t, "up-update.log"))
	if diff := cmp.Diff(map[string]int64{"update": 1, "same": 116}, got.Counts); diff != "" {
		t.Errorf("counts (-want +got):\n%s", diff)
	}
	if !got.Summary || len(got.Resources) != 1 {
		t.Fatalf("summary=%v resources=%d, want true and 1", got.Summary, len(got.Resources))
	}
	r := got.Resources[0]
	if r.Op != "update" || r.Type != "aws:iam/userPolicy:UserPolicy" || r.Name != "pulumi-operator-ui" ||
		r.URN != "urn:pulumi:prod::example-infra::aws:iam/userPolicy:UserPolicy::pulumi-operator-ui" {
		t.Errorf("resource = %+v", r)
	}
	for _, want := range []string{`+ Sid      : "ListHistory"`, `+ Sid     : "ReadHistoryFiles"`,
		`~ policy: (json) {`} {
		if !strings.Contains(r.Diff, want) {
			t.Errorf("diff lacks %q", want)
		}
	}
	for _, bad := range []string{"--outputs:--", "accountId", "[urn=", "[provider=", "[id="} {
		if strings.Contains(r.Diff, bad) {
			t.Errorf("diff contains %q", bad)
		}
	}
	if !strings.HasPrefix(r.Diff, "~ policy: (json) {") {
		t.Errorf("diff not dedented: %q", r.Diff[:40])
	}
}

func TestParseCleanPreview(t *testing.T) {
	got := Parse(golden(t, "preview-clean.log"))
	if diff := cmp.Diff(map[string]int64{"same": 117}, got.Counts); diff != "" {
		t.Errorf("counts (-want +got):\n%s", diff)
	}
	if !got.Summary || len(got.Resources) != 0 {
		t.Fatalf("summary=%v resources=%+v", got.Summary, got.Resources)
	}
}

func TestParseOps(t *testing.T) {
	lines := []string{
		"Updating (prod):",
		"    + aws:s3/bucket:Bucket: (create)",
		"        [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::logs]",
		"        bucket: \"logs\"",
		"    - aws:iam/user:User: (delete)",
		"        [urn=urn:pulumi:prod::p::aws:iam/user:User::old]",
		"    +-aws:kms/key:Key: (replace) 🔒",
		"        [urn=urn:pulumi:prod::p::aws:kms/key:Key::k]",
		"      ~ description: \"a\" => \"b\"",
		"    ~ aws:s3/bucket:Bucket: (refresh)",
		"        [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::quiet]",
		"    ~ aws:s3/bucket:Bucket: (refresh)",
		"        [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::drifted]",
		"      ~ tags: {",
		"          + owner: \"me\"",
		"        }",
		"      aws:s3/bucket:Bucket: (same)",
		"        [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::same]",
		"Resources:",
		"    + 1 created",
		"    - 1 deleted",
		"    +-1 replaced",
		"    3 changes. 2 unchanged",
		"",
		"Duration: 3s",
	}
	got := Parse(lines)
	type row struct{ Op, Name string }
	var rows []row
	for _, r := range got.Resources {
		rows = append(rows, row{r.Op, r.Name})
	}
	want := []row{{"create", "logs"}, {"delete", "old"}, {"replace", "k"}, {"refresh", "drifted"}}
	if diff := cmp.Diff(want, rows); diff != "" {
		t.Errorf("resources (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]int64{"create": 1, "delete": 1, "replace": 1, "same": 2},
		got.Counts); diff != "" {
		t.Errorf("counts (-want +got):\n%s", diff)
	}
	if got.Resources[0].Diff != `bucket: "logs"` {
		t.Errorf("create diff = %q", got.Resources[0].Diff)
	}
}

func TestParsePreviewSummaryForms(t *testing.T) {
	got := Parse([]string{"Resources:", "    + 2 to create", "    ~ 1 to update",
		"    - 1 to delete", "    +-1 to replace", "    10 unchanged"})
	want := map[string]int64{"create": 2, "update": 1, "delete": 1, "replace": 1, "same": 10}
	if diff := cmp.Diff(want, got.Counts); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestParseSkipsStackEntries(t *testing.T) {
	got := Parse([]string{
		"~ pulumi:pulumi:Stack: (refresh)",
		"    [urn=urn:pulumi:prod::p::pulumi:pulumi:Stack::p-prod]",
		"    --outputs:--",
		"    accountId: \"123456789012\"",
		"  ~ pulumi:pulumi:Stack: (update)",
		"    [urn=urn:pulumi:prod::p::pulumi:pulumi:Stack::p-prod]",
		"      ~ secretOut: [secret]",
	})
	if len(got.Resources) != 0 {
		t.Fatalf("resources = %+v, want none", got.Resources)
	}
}

func TestParseFailedRunHasNoSummary(t *testing.T) {
	got := Parse([]string{"Updating (prod):", "error: user: Current requires cgo or $USER set"})
	if got.Summary || len(got.Resources) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseCaps(t *testing.T) {
	long := strings.Repeat("x", 1000)
	lines := []string{"    ~ aws:s3/bucket:Bucket: (update)",
		"        [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::big]"}
	for range 100 { // 100 KB of diff
		lines = append(lines, "      ~ "+long)
	}
	for i := range resourcesMax + 10 {
		lines = append(lines, "    + aws:s3/bucket:Bucket: (create)",
			"        [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::b"+strings.Repeat("0", i%3)+"]",
			"      bucket: \""+long+"\"")
	}
	got := Parse(lines)
	if !got.Resources[0].Truncated || len(got.Resources[0].Diff) > diffBytesMax {
		t.Errorf("first diff: truncated=%v len=%d", got.Resources[0].Truncated,
			len(got.Resources[0].Diff))
	}
	if len(got.Resources) > resourcesMax || !got.Truncated {
		t.Errorf("resources = %d truncated=%v", len(got.Resources), got.Truncated)
	}
	total := 0
	for _, r := range got.Resources {
		total += len(r.Diff)
	}
	if total > runBytesMax {
		t.Errorf("total diff bytes = %d, want <= %d", total, runBytesMax)
	}
}

// A refresh entry prints its changes after --outputs:--, as the real Stack refresh in the
// captured logs does; for refresh that marker must not end the block.
func TestParseRefreshDriftAfterOutputs(t *testing.T) {
	got := Parse([]string{
		"~ aws:s3/bucket:Bucket: (refresh)",
		"    [urn=urn:pulumi:prod::p::aws:s3/bucket:Bucket::logs]",
		"    --outputs:--",
		"  ~ tags: {",
		"      + owner: \"console\"",
		"    }",
		"Compiling the program ...",
	})
	if len(got.Resources) != 1 || got.Resources[0].Op != "refresh" ||
		!strings.Contains(got.Resources[0].Diff, `+ owner: "console"`) {
		t.Fatalf("resources = %+v", got.Resources)
	}
	if strings.Contains(got.Resources[0].Diff, "--outputs:--") {
		t.Error("diff keeps the outputs marker")
	}
}

func TestParseURNWithoutSeparatorKeepsName(t *testing.T) {
	got := Parse([]string{
		"    + a:b/c:D: (create)",
		"        [urn=odd-urn]",
		"      k: 1",
	})
	if len(got.Resources) != 1 || got.Resources[0].Name != "odd-urn" {
		t.Fatalf("resources = %+v, want name odd-urn", got.Resources)
	}
}
