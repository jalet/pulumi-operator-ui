package web

import (
	"regexp"
	"strings"
)

var (
	_repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	_sha      = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// repoURL returns the GitHub web URL for a Stack's projectRepo, or for the history file's
// vcs repo when the Stack has none. Only github.com, and only an owner/repo made of safe
// characters, ever yields a link.
func repoURL(stackRepo, vcsRepo string) (string, bool) {
	path, ok := githubPath(stackRepo)
	if !ok && stackRepo == "" {
		if rest, found := strings.CutPrefix(vcsRepo, "github.com/"); found {
			path, ok = rest, true
		}
	}
	if !ok {
		return "", false
	}
	owner, repo, found := strings.Cut(strings.TrimSuffix(path, ".git"), "/")
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
