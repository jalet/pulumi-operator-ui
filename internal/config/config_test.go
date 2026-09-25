package config

import (
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
		return "postgres://pou@db:5432/pou"
	}
	return ""
}

func defaults() Config {
	return Config{
		HTTPAddr:    ":8080",
		MetricsAddr: ":9090",
		DatabaseURL: "postgres://pou@db:5432/pou",
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
