package auth

import (
	"net/url"
	"slices"
	"strings"
)

// ClaimValues returns the string values of claim, which IdPs send as a string or a list.
// Non-string entries are dropped; a missing or mistyped claim yields nil.
func ClaimValues(claims map[string]any, claim string) []string {
	switch v := claims[claim].(type) {
	case string:
		return []string{v}
	case []string:
		return slices.Clone(v)
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// Allowed reports whether any non-empty value is on the allowlist (exact, case-sensitive).
func Allowed(values, allowlist []string) bool {
	for _, v := range values {
		if v != "" && slices.Contains(allowlist, v) {
			return true
		}
	}
	return false
}

// safeReturn keeps a post-login redirect on this origin: only absolute paths, never
// scheme-relative ("//host") or backslash tricks ("/\host") that browsers treat as hosts.
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}
