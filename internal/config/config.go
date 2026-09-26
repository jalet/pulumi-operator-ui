// Package config parses and validates the command-line configuration.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jalet/pulumi-operator-ui/internal/theme"
)

const (
	_retentionFloor    = time.Hour
	_sessionAgeFloor   = 5 * time.Minute
	_sessionAgeCeiling = 24 * time.Hour
)

// OIDC holds the identity provider settings.
type OIDC struct {
	Issuer           string
	ClientID         string
	ClientSecretFile string
	RedirectURL      string
	CAFile           string
	LocalLogout      bool // sign out of the app only, not of the IdP
}

// Config is the validated process configuration.
type Config struct {
	Namespaces         []string // empty = all
	HTTPAddr           string
	MetricsAddr        string
	DatabaseURL        string
	DatabaseCAFile     string
	OIDC               OIDC
	AuthClaim          string
	AuthAllowed        []string
	SessionKeyFile     string
	SessionPrevKeyFile string
	SessionAgeMax      time.Duration
	RetentionRuns      time.Duration
	RetentionAuth      time.Duration

	S3HistoryEnabled  bool
	S3HistoryInterval time.Duration

	DisplayTimezone string // IANA zone for day headers
	LogDiffs        bool   // store the property diffs of captured engine logs

	ThemeFile string      // optional YAML color overrides; see internal/theme
	Theme     theme.Theme // loaded from ThemeFile by Parse; zero when unset
}

// Parse reads flags from args (without argv[0]); getenv supplies the DATABASE_URL fallback.
// All validation errors are joined so the operator sees every problem at once.
func Parse(args []string, getenv func(string) string) (Config, error) {
	var (
		c          Config
		namespaces string
		allowed    string
	)
	fs := flag.NewFlagSet("pulumi-operator-ui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&namespaces, "namespaces", "", "comma-separated namespaces to watch; empty = all")
	fs.StringVar(&c.HTTPAddr, "http-addr", ":8080", "listen address for the UI")
	fs.StringVar(&c.MetricsAddr, "metrics-addr", ":9090", "listen address for /metrics")
	fs.StringVar(&c.DatabaseURL, "database-url", "", "PostgreSQL URL; falls back to DATABASE_URL")
	fs.StringVar(&c.DatabaseCAFile, "database.ca-file", "", "CA bundle for the database TLS")
	fs.StringVar(&c.OIDC.Issuer, "oidc.issuer", "", "OIDC issuer URL")
	fs.StringVar(&c.OIDC.ClientID, "oidc.client-id", "", "OIDC client ID")
	fs.StringVar(&c.OIDC.ClientSecretFile, "oidc.client-secret-file", "",
		"file with the client secret")
	fs.StringVar(&c.OIDC.RedirectURL, "oidc.redirect-url", "", "OIDC redirect URL")
	fs.StringVar(&c.OIDC.CAFile, "oidc.ca-file", "", "extra CA bundle for the IdP")
	fs.BoolVar(&c.OIDC.LocalLogout, "oidc.local-logout", false,
		"sign out of the app only, even when the IdP supports RP-initiated logout")
	fs.StringVar(&c.AuthClaim, "auth.claim", "groups", "claim checked against the allowlist")
	fs.StringVar(&allowed, "auth.allowed", "", "comma-separated allowed claim values")
	fs.StringVar(&c.SessionKeyFile, "session.key-file", "", "file with the session HMAC key")
	fs.StringVar(&c.SessionPrevKeyFile, "session.previous-key-file", "",
		"file with the previous session key, accepted for verification")
	fs.DurationVar(&c.SessionAgeMax, "session.max-age", 8*time.Hour, "absolute session lifetime")
	fs.DurationVar(&c.RetentionRuns, "retention", 4320*time.Hour, "retention for runs")
	fs.DurationVar(&c.RetentionAuth, "auth-retention", 720*time.Hour,
		"retention for sign-in events, which hold email addresses and claim values")
	fs.BoolVar(&c.S3HistoryEnabled, "s3-history.enabled", false,
		"read Pulumi update history from each Stack's S3 backend")
	fs.DurationVar(&c.S3HistoryInterval, "s3-history.interval", 5*time.Minute,
		"S3 history poll interval")
	fs.StringVar(&c.DisplayTimezone, "display-timezone", "UTC", "IANA time zone for day headers")
	fs.BoolVar(&c.LogDiffs, "logs.diffs", true,
		"store the property diffs from engine logs; false keeps only counts and resource names")
	fs.StringVar(&c.ThemeFile, "theme-file", "",
		"YAML file with color overrides; empty = built-in colors")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}
	c.Namespaces = splitList(namespaces)
	c.AuthAllowed = splitList(allowed)
	if c.DatabaseURL == "" {
		c.DatabaseURL = getenv("DATABASE_URL")
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []error
	required := []struct{ flag, value string }{
		{"--database-url (or DATABASE_URL)", c.DatabaseURL},
		{"--oidc.issuer", c.OIDC.Issuer},
		{"--oidc.client-id", c.OIDC.ClientID},
		{"--oidc.client-secret-file", c.OIDC.ClientSecretFile},
		{"--oidc.redirect-url", c.OIDC.RedirectURL},
		{"--auth.claim", c.AuthClaim},
		{"--session.key-file", c.SessionKeyFile},
	}
	for _, r := range required {
		if r.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.flag))
		}
	}
	if len(c.AuthAllowed) == 0 {
		errs = append(errs, errors.New("--auth.allowed is required"))
	}
	if c.RetentionRuns < _retentionFloor {
		errs = append(errs, errors.New("--retention must be at least 1h"))
	}
	if c.RetentionAuth < _retentionFloor {
		errs = append(errs, errors.New("--auth-retention must be at least 1h"))
	}
	if c.S3HistoryInterval < time.Minute {
		errs = append(errs, errors.New("--s3-history.interval must be at least 1m"))
	}
	if _, err := time.LoadLocation(c.DisplayTimezone); err != nil {
		errs = append(errs, fmt.Errorf("--display-timezone: unknown zone %q", c.DisplayTimezone))
	}
	if c.ThemeFile != "" {
		t, err := theme.Load(c.ThemeFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("--theme-file: %w", err))
		}
		c.Theme = t
	}
	if c.SessionAgeMax < _sessionAgeFloor || c.SessionAgeMax > _sessionAgeCeiling {
		errs = append(errs, errors.New("--session.max-age must be between 5m and 24h"))
	}
	if c.OIDC.Issuer != "" {
		// Discovery and the token signing keys come from here.
		errs = append(errs, validateHTTPSURL("--oidc.issuer", c.OIDC.Issuer))
	}
	if c.OIDC.RedirectURL != "" {
		errs = append(errs, validateHTTPSURL("--oidc.redirect-url", c.OIDC.RedirectURL))
	}
	if c.DatabaseURL != "" {
		errs = append(errs, validateDatabaseURL(c.DatabaseURL, c.DatabaseCAFile != ""))
	}
	return errors.Join(errs...)
}

// validateHTTPSURL requires an https URL, allowing http only for localhost (the dev IdP).
func validateHTTPSURL(flagName, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: invalid", flagName)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLocalhost(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("%s must be https (http only for localhost)", flagName)
}

// validateDatabaseURL requires verified TLS off localhost: sslmode=verify-ca/verify-full,
// or a CA file (the store then forces verification). The default "prefer", "allow" and
// "require" do not verify the server, and "prefer"/"allow" fall back to plaintext.
// Errors never include the URL: it may carry a password.
func validateDatabaseURL(raw string, haveCA bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("--database-url: invalid")
	}
	// Judge the hosts pgx will actually dial: ?host= overrides the URL's host, and a URL can
	// list several.
	pc, err := pgconn.ParseConfig(raw)
	if err != nil {
		return errors.New("--database-url: invalid")
	}
	local := isLocalhost(pc.Host)
	for _, fb := range pc.Fallbacks {
		local = local && isLocalhost(fb.Host)
	}
	if local {
		return nil
	}
	switch mode := u.Query().Get("sslmode"); {
	case mode == "disable":
		return errors.New("--database-url: sslmode=disable is only allowed for localhost")
	case mode == "verify-ca" || mode == "verify-full" || haveCA:
		return nil
	default:
		return errors.New("--database-url: TLS must be verified; set sslmode=verify-full " +
			"or --database.ca-file")
	}
}

func isLocalhost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
