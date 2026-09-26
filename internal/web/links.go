package web

import (
	"regexp"
	"strings"
)

var (
	_repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	_sha      = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// repoURL links a run's commit to GitHub: the Stack's repo when it is a GitHub repo, else the
// repo the history recorded (vcs.*), else nothing.
func repoURL(stackRepo, vcsRepo string) (string, bool) {
	if path, ok := githubPath(stackRepo); ok {
		if u, ok := githubRepoURL(path); ok {
			return u, true
		}
	}
	if rest, ok := strings.CutPrefix(vcsRepo, "github.com/"); ok {
		return githubRepoURL(rest)
	}
	return "", false
}

// githubRepoURL validates "owner/repo" (with an optional .git or trailing slash).
func githubRepoURL(path string) (string, bool) {
	owner, repo, found := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git"), "/")
	if !found || !_repoPart.MatchString(owner) || !_repoPart.MatchString(repo) ||
		owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", false
	}
	return "https://github.com/" + owner + "/" + repo, true
}

func githubPath(u string) (string, bool) {
	for _, p := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/"} {
		if rest, ok := strings.CutPrefix(u, p); ok {
			return rest, true
		}
	}
	return "", false
}

// commitURL links a commit on GitHub, or returns "" when repo or sha fail validation.
func commitURL(stackRepo, vcsRepo, sha string) string {
	base, ok := repoURL(stackRepo, vcsRepo)
	if !ok || !_sha.MatchString(sha) {
		return ""
	}
	return base + "/commit/" + sha
}
