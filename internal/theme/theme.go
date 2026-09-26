// Package theme loads the optional color theme and renders it as CSS custom properties that
// override the defaults in app.css. Every color in the UI is one of these tokens.
package theme

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"sigs.k8s.io/yaml"
)

// Tokens are the keys a palette may set, in the order they are rendered.
var Tokens = []string{"page", "panel", "line", "ink", "muted", "focus",
	"ok", "run", "attention", "bad", "neutral",
	"okText", "runText", "attentionText", "badText", "neutralText"}

// BrandBarLen is the number of colors theme.brandBar must hold.
const BrandBarLen = 5

var _hex = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// Theme is a validated theme. Keys left out keep the defaults in app.css.
type Theme struct {
	Light    map[string]string `json:"light,omitempty"`
	Dark     map[string]string `json:"dark,omitempty"`
	BrandBar []string          `json:"brandBar,omitempty"`
}

// Load reads and validates a theme file.
func Load(path string) (Theme, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is operator configuration
	if err != nil {
		return Theme{}, fmt.Errorf("theme: %w", err)
	}
	return Parse(b)
}

// Parse validates a theme document. Unknown keys and values that are not hex colors are
// errors that name the key, all reported at once.
func Parse(b []byte) (Theme, error) {
	var t Theme
	if err := yaml.UnmarshalStrict(b, &t); err != nil {
		return Theme{}, fmt.Errorf("theme: %w", err)
	}
	var errs []error
	for _, mode := range []struct {
		name    string
		palette map[string]string
	}{{"light", t.Light}, {"dark", t.Dark}} {
		for _, k := range slices.Sorted(maps.Keys(mode.palette)) {
			key := "theme." + mode.name + "." + k
			switch v := mode.palette[k]; {
			case !slices.Contains(Tokens, k):
				errs = append(errs, fmt.Errorf("%s: unknown key", key))
			case !_hex.MatchString(v):
				errs = append(errs, badColor(key, v))
			}
		}
	}
	if n := len(t.BrandBar); n != 0 && n != BrandBarLen {
		errs = append(errs, fmt.Errorf("theme.brandBar: has %d colors, want %d", n, BrandBarLen))
	}
	for i, v := range t.BrandBar {
		if !_hex.MatchString(v) {
			errs = append(errs, badColor(fmt.Sprintf("theme.brandBar[%d]", i), v))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Theme{}, err
	}
	return t, nil
}

func badColor(key, v string) error {
	return fmt.Errorf("%s: %q is not a hex color (#rgb, #rrggbb or #rrggbbaa)", key, v)
}

// CSS renders the theme. Light values sit in a not-dark media block: this sheet is
// unlayered, so a bare :root would also override app.css's dark defaults.
func (t Theme) CSS() []byte {
	var b bytes.Buffer
	if len(t.BrandBar) > 0 {
		b.WriteString(":root {\n")
		for i, c := range t.BrandBar {
			fmt.Fprintf(&b, "  --pou-brand-%d: %s;\n", i+1, c)
		}
		b.WriteString("}\n")
	}
	writeMode(&b, "@media not all and (prefers-color-scheme: dark)", t.Light)
	writeMode(&b, "@media (prefers-color-scheme: dark)", t.Dark)
	return b.Bytes()
}

func writeMode(b *bytes.Buffer, media string, palette map[string]string) {
	if len(palette) == 0 {
		return
	}
	fmt.Fprintf(b, "%s {\n  :root {\n", media)
	for _, k := range Tokens {
		if v, ok := palette[k]; ok {
			fmt.Fprintf(b, "    %s: %s;\n", Var(k), v)
		}
	}
	b.WriteString("  }\n}\n")
}

// Var is the CSS custom property for a token: okText is --pou-ok-text.
func Var(token string) string {
	var s strings.Builder
	s.WriteString("--pou-")
	for _, r := range token {
		if unicode.IsUpper(r) {
			s.WriteByte('-')
			r = unicode.ToLower(r)
		}
		s.WriteRune(r)
	}
	return s.String()
}
