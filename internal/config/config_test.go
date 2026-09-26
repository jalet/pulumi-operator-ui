package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var _base = []string{
	"--oidc.issuer=https://idp.example",
	"--oidc.client-id=pou",
	"--oidc.client-secret-file=/s/client-secret",
	"--oidc.redirect-url=https://pou.example/auth/callback",
	"--auth.allowed=Pulumi Viewers,platform",
	"--session.key-file=/s/key",
}

func testEnv(k string) string {
	if k == "DATABASE_URL" {
		return "postgres://pou@db:5432/pou?sslmode=verify-full"
	}
	return ""
}

func defaults() Config {
	return Config{
		HTTPAddr:    ":8080",
		MetricsAddr: ":9090",
		DatabaseURL: "postgres://pou@db:5432/pou?sslmode=verify-full",
		OIDC: OIDC{
			Issuer:           "https://idp.example",
			ClientID:         "pou",
			ClientSecretFile: "/s/client-secret",
			RedirectURL:      "https://pou.example/auth/callback",
		},
		AuthClaim:      "groups",
		AuthAllowed:    []string{"Pulumi Viewers", "platform"},
		SessionKeyFile: "/s/key",
		SessionAgeMax:  8 * time.Hour,
		RetentionRuns:  4320 * time.Hour,
		RetentionAuth:  8760 * time.Hour,

		S3HistoryInterval: 5 * time.Minute,
		DisplayTimezone:   "UTC",
	}
}

func with(args ...string) []string { return append(slices.Clone(_base), args...) }

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		giveArgs []string
		want     func(*Config)
		wantErr  string
	}{
		{name: "defaults", giveArgs: _base, want: func(*Config) {}},
		{
			name:     "namespaces trimmed",
			giveArgs: with("--namespaces= a, b ,,"),
			want:     func(c *Config) { c.Namespaces = []string{"a", "b"} },
		},
		{
			name:     "flag beats env",
			giveArgs: with("--database-url=postgres://x@localhost/pou"),
			want:     func(c *Config) { c.DatabaseURL = "postgres://x@localhost/pou" },
		},
		{
			name:     "retention below one hour",
			giveArgs: with("--retention=30m"),
			wantErr:  "--retention must be at least 1h",
		},
		{
			name:     "auth retention below one hour",
			giveArgs: with("--auth-retention=30m"),
			wantErr:  "--auth-retention must be at least 1h",
		},
		{
			name:     "session max age out of range",
			giveArgs: with("--session.max-age=48h"),
			wantErr:  "--session.max-age must be between 5m and 24h",
		},
		{
			name:     "http redirect url off localhost",
			giveArgs: with("--oidc.redirect-url=http://pou.example/cb"),
			wantErr:  "--oidc.redirect-url must be https",
		},
		{
			name:     "http redirect url on localhost",
			giveArgs: with("--oidc.redirect-url=http://localhost:8080/auth/callback"),
			want: func(c *Config) {
				c.OIDC.RedirectURL = "http://localhost:8080/auth/callback"
			},
		},
		{
			name:     "sslmode disable off localhost",
			giveArgs: with("--database-url=postgres://u@db/pou?sslmode=disable"),
			wantErr:  "sslmode=disable is only allowed for localhost",
		},
		{
			name:     "s3 history on",
			giveArgs: with("--s3-history.enabled", "--s3-history.interval=2m"),
			want: func(c *Config) {
				c.S3HistoryEnabled, c.S3HistoryInterval = true, 2*time.Minute
			},
		},
		{
			name:     "s3 history interval too short",
			giveArgs: with("--s3-history.interval=30s"),
			wantErr:  "--s3-history.interval must be at least 1m",
		},
		{
			name:     "no sslmode off localhost",
			giveArgs: with("--database-url=postgres://u@db/pou"),
			wantErr:  "--database-url: TLS must be verified",
		},
		{
			name:     "sslmode require off localhost",
			giveArgs: with("--database-url=postgres://u@db/pou?sslmode=require"),
			wantErr:  "--database-url: TLS must be verified",
		},
		{
			name:     "sslmode prefer with CA file",
			giveArgs: with("--database-url=postgres://u@db/pou", "--database.ca-file=/s/ca.crt"),
			want: func(c *Config) {
				c.DatabaseURL, c.DatabaseCAFile = "postgres://u@db/pou", "/s/ca.crt"
			},
		},
		{
			name:     "sslmode verify-ca off localhost",
			giveArgs: with("--database-url=postgres://u@db/pou?sslmode=verify-ca"),
			want:     func(c *Config) { c.DatabaseURL = "postgres://u@db/pou?sslmode=verify-ca" },
		},
		{
			name: "sslmode disable with CA file off localhost",
			giveArgs: with("--database-url=postgres://u@db/pou?sslmode=disable",
				"--database.ca-file=/s/ca.crt"),
			wantErr: "sslmode=disable is only allowed for localhost",
		},
		{
			name:     "sslmode disable on localhost",
			giveArgs: with("--database-url=postgres://u@localhost/pou?sslmode=disable"),
			want: func(c *Config) {
				c.DatabaseURL = "postgres://u@localhost/pou?sslmode=disable"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.giveArgs, testEnv)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := defaults()
			tt.want(&want)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseMissingRequiredListsAll(t *testing.T) {
	_, err := Parse(nil, func(string) string { return "" })
	for _, flag := range []string{"--database-url", "--oidc.issuer", "--oidc.client-id",
		"--oidc.client-secret-file", "--oidc.redirect-url", "--auth.allowed",
		"--session.key-file"} {
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("error %v does not mention %s", err, flag)
		}
	}
}

func TestParseInvalidDatabaseURLHidesValue(t *testing.T) {
	_, err := Parse(with("--database-url=postgres://u:s3cret@db:bad/pou"), testEnv)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks the URL: %v", err)
	}
}
func TestDisplayTimezone(t *testing.T) {
	c, err := Parse(with("--display-timezone=Europe/Stockholm"), testEnv)
	if err != nil || c.DisplayTimezone != "Europe/Stockholm" {
		t.Fatalf("got %q err %v", c.DisplayTimezone, err)
	}
	if _, err := Parse(with("--display-timezone=Mars/Olympus"), testEnv); err == nil ||
		!strings.Contains(err.Error(), "--display-timezone") {
		t.Fatalf("err = %v, want a --display-timezone error", err)
	}
}

func TestThemeFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte(`light: {page: "#ffffff"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(`light: {page: "white"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(with("--theme-file="+good), testEnv)
	if err != nil || c.Theme.Light["page"] != "#ffffff" {
		t.Fatalf("got %+v err %v", c.Theme, err)
	}
	for _, p := range []string{bad, filepath.Join(dir, "missing.yaml")} {
		if _, err := Parse(with("--theme-file="+p), testEnv); err == nil ||
			!strings.Contains(err.Error(), "--theme-file") {
			t.Errorf("%s: err = %v, want a --theme-file error", p, err)
		}
	}
	if _, err := Parse(with("--theme-file="+bad), testEnv); err == nil ||
		!strings.Contains(err.Error(), "theme.light.page") {
		t.Errorf("err = %v, want the bad key named", err)
	}
}

func TestLocalLogoutFlag(t *testing.T) {
	c, err := Parse(with(), testEnv)
	if err != nil || c.OIDC.LocalLogout {
		t.Fatalf("default local logout = %v err %v, want false", c.OIDC.LocalLogout, err)
	}
	c, err = Parse(with("--oidc.local-logout"), testEnv)
	if err != nil || !c.OIDC.LocalLogout {
		t.Fatalf("local logout = %v err %v, want true", c.OIDC.LocalLogout, err)
	}
}
