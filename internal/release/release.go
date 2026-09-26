// Package release reports which build of the app is running: the release version, the
// commit, when that commit was made, and when the binary was built.
package release

import (
	"cmp"
	"runtime/debug"
	"time"
)

// Set at build time by .ko.yaml with -ldflags "-X github.com/jalet/pulumi-operator-ui/
// internal/release.<name>=...". Empty in dev builds, which fall back to the VCS stamp the
// Go toolchain records.
var (
	version    string
	commit     string
	commitTime string // RFC 3339
	buildTime  string // RFC 3339
)

// Info describes the running build. Unknown values are empty or zero.
type Info struct {
	Version    string // "dev" when not stamped
	Commit     string // full SHA
	Dirty      bool   // built from a tree with uncommitted changes (dev builds only)
	CommitTime time.Time
	BuildTime  time.Time
}

// Get returns the running build's information.
func Get() Info {
	return resolve(version, commit, commitTime, buildTime, debug.ReadBuildInfo)
}

// ShortCommit is the commit's first seven characters, or "" when unknown.
func (i Info) ShortCommit() string {
	return i.Commit[:min(7, len(i.Commit))]
}

func resolve(v, c, ct, bt string, read func() (*debug.BuildInfo, bool)) Info {
	info := Info{Version: cmp.Or(v, "dev"), Commit: c, CommitTime: parse(ct),
		BuildTime: parse(bt)}
	bi, ok := read()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = cmp.Or(info.Commit, s.Value)
		case "vcs.time":
			if info.CommitTime.IsZero() {
				info.CommitTime = parse(s.Value)
			}
		case "vcs.modified":
			info.Dirty = s.Value == "true"
		}
	}
	return info
}

func parse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
