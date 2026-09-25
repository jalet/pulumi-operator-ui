package web

import "testing"

func TestRepoURLAccepts(t *testing.T) {
	for give, want := range map[string]string{
		"git@github.com:o/r.git":                "https://github.com/o/r",
		"git@github.com:o/r":                    "https://github.com/o/r",
		"ssh://git@github.com/o/r.git":          "https://github.com/o/r",
		"https://github.com/o/r.git":            "https://github.com/o/r",
		"https://github.com/jalet/k8s-home-lab": "https://github.com/jalet/k8s-home-lab",
	} {
		if got, ok := repoURL(give, ""); !ok || got != want {
			t.Errorf("repoURL(%q) = %q, %v; want %q", give, got, ok, want)
		}
	}
	if got, ok := repoURL("", "github.com/o/r"); !ok || got != "https://github.com/o/r" {
		t.Errorf("vcs fallback = %q, %v", got, ok)
	}
}

func TestRepoURLRejects(t *testing.T) {
	for _, give := range []string{
		"", "git@gitlab.com:o/r.git", "https://example.com/o/r", "javascript:alert(1)",
		`https://github.com/o/r"onmouseover="x`, "https://github.com/o/r/extra",
		"https://github.com/../r", "git@github.com:o", "https://github.com/o/r?x=1",
	} {
		if got, ok := repoURL(give, ""); ok {
			t.Errorf("repoURL(%q) = %q, want rejected", give, got)
		}
	}
	if _, ok := repoURL("", "gitlab.com/o/r"); ok {
		t.Error("non-GitHub vcs fallback accepted")
	}
}

func TestCommitURL(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	if got := commitURL("git@github.com:o/r.git", "", sha); got != "https://github.com/o/r/commit/"+sha {
		t.Errorf("commitURL = %q", got)
	}
	for _, bad := range []string{"", "xyz", "012345", "0123456789abcdef0123456789abcdef012345678",
		"ABCDEF0"} {
		if got := commitURL("git@github.com:o/r.git", "", bad); got != "" {
			t.Errorf("commitURL(sha %q) = %q, want none", bad, got)
		}
	}
	if got := commitURL("git@github.com:o/r.git", "", "0123456"); got == "" {
		t.Error("a 7-character short sha must link")
	}
	if got := commitURL("", "", sha); got != "" {
		t.Errorf("no repo: %q", got)
	}
}
