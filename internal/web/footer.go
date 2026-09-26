package web

import (
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/release"
)

// _sourceRepo is where released commits live; the footer links the running commit there.
const _sourceRepo = "https://github.com/jalet/pulumi-operator-ui"

// footer is the page footer's view of the running build.
type footer struct {
	Version    string // "v0.1.0", or "dev"
	Short      string // short commit, "" when unknown
	URL        string // the commit on GitHub, only for a real SHA
	Dirty      bool
	CommitTime string // in the display timezone, "" when unknown
	CommitISO  string
	BuildTime  string
	BuildISO   string
}

func newFooter(b release.Info, loc *time.Location) footer {
	f := footer{Version: b.Version, Short: b.ShortCommit(), Dirty: b.Dirty}
	if f.Version != "dev" {
		f.Version = "v" + f.Version
	}
	if _sha.MatchString(b.Commit) && !b.Dirty {
		f.URL = _sourceRepo + "/commit/" + b.Commit
	}
	f.CommitTime, f.CommitISO = footerTime(b.CommitTime, loc)
	f.BuildTime, f.BuildISO = footerTime(b.BuildTime, loc)
	return f
}

func footerTime(t time.Time, loc *time.Location) (string, string) {
	if t.IsZero() {
		return "", ""
	}
	return t.In(loc).Format("2 Jan 2006 15:04"), t.UTC().Format(time.RFC3339)
}
