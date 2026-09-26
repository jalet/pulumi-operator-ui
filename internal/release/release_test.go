package release

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func vcs(settings ...string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		bi := &debug.BuildInfo{}
		for i := 0; i+1 < len(settings); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return bi, true
	}
}

// Values stamped at build time win over what the Go toolchain recorded.
func TestStampedValuesWin(t *testing.T) {
	got := resolve("0.1.0", "abcdef0123456789abcdef0123456789abcdef01", "2026-09-25T14:02:00Z",
		"2026-09-26T08:12:00Z", vcs("vcs.revision", "ffffffffffffffffffffffffffffffffffffffff",
			"vcs.time", "2020-01-01T00:00:00Z"))
	if got.Version != "0.1.0" || got.Commit != "abcdef0123456789abcdef0123456789abcdef01" ||
		!got.CommitTime.Equal(time.Date(2026, 9, 25, 14, 2, 0, 0, time.UTC)) ||
		!got.BuildTime.Equal(time.Date(2026, 9, 26, 8, 12, 0, 0, time.UTC)) {
		t.Fatalf("resolve = %+v", got)
	}
	if got.ShortCommit() != "abcdef0" {
		t.Errorf("short = %q", got.ShortCommit())
	}
}

// A dev build (go run, tests) still knows its commit from the VCS stamp.
func TestFallsBackToVCSStamp(t *testing.T) {
	got := resolve("", "", "", "", vcs("vcs.revision", "abcdef0123456789abcdef0123456789abcdef01",
		"vcs.time", "2026-09-25T14:02:00Z", "vcs.modified", "true"))
	if got.Version != "dev" || got.ShortCommit() != "abcdef0" || !got.Dirty ||
		got.CommitTime.IsZero() || !got.BuildTime.IsZero() {
		t.Fatalf("resolve = %+v", got)
	}
}

func TestUnknownIsEmpty(t *testing.T) {
	got := resolve("", "", "not a time", "", func() (*debug.BuildInfo, bool) { return nil, false })
	if got.Version != "dev" || got.Commit != "" || got.ShortCommit() != "" || !got.CommitTime.IsZero() {
		t.Fatalf("resolve = %+v", got)
	}
}

// The release build must stamp all four values.
func TestKoStampsBuildInfo(t *testing.T) {
	b, err := os.ReadFile("../../.ko.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"version={{.Env.VERSION}}", "commit={{.Git.FullCommit}}",
		"commitTime={{.Git.CommitDate}}", "buildTime={{.Date}}"} {
		if !strings.Contains(string(b), "-X github.com/jalet/pulumi-operator-ui/internal/release."+v) {
			t.Errorf(".ko.yaml does not stamp %s", v)
		}
	}
}

// ko renders {{.Date}} in local time with an offset.
func TestParsesOffsetTime(t *testing.T) {
	got := resolve("", "", "", "2026-09-26T08:55:38+02:00", func() (*debug.BuildInfo, bool) { return nil, false })
	if !got.BuildTime.Equal(time.Date(2026, 9, 26, 6, 55, 38, 0, time.UTC)) || got.BuildTime.Location() != time.UTC {
		t.Fatalf("build time = %v, want 06:55:38 UTC", got.BuildTime)
	}
}
