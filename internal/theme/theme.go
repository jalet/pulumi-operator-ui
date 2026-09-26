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

// Parse validates a theme document. Keys are matched exactly (a JSON decoder would accept
// LIGHT for light), unknown keys and values that are not quoted hex colors are errors that
// name the key, and all are reported at once.
func Parse(b []byte) (Theme, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return Theme{}, fmt.Errorf("theme: %w", err)
	}
	var t Theme
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		switch k {
		case "light":
			t.Light = palette("theme.light", raw[k], &errs)
		case "dark":
			t.Dark = palette("theme.dark", raw[k], &errs)
		case "brandBar":
			t.BrandBar = brandBar(raw[k], &errs)
		default:
			errs = append(errs, fmt.Errorf("theme.%s: unknown key", k))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Theme{}, err
	}
	return t, nil
}

// palette validates one mode: known token keys with quoted hex values.
func palette(name string, v any, errs *[]error) map[string]string {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		*errs = append(*errs, fmt.Errorf("%s: want a map of colors", name))
		return nil
	}
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		key := name + "." + k
		s, isString := m[k].(string)
		switch {
		case !slices.Contains(Tokens, k):
			*errs = append(*errs, fmt.Errorf("%s: unknown key", key))
		case !isString || !_hex.MatchString(s):
			*errs = append(*errs, badColor(key, m[k]))
		default:
			out[k] = s
		}
	}
	return out
}

func brandBar(v any, errs *[]error) []string {
	if v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		*errs = append(*errs, errors.New("theme.brandBar: want a list of colors"))
		return nil
	}
	if len(list) != BrandBarLen {
		*errs = append(*errs, fmt.Errorf("theme.brandBar: has %d colors, want %d", len(list),
			BrandBarLen))
	}
	out := make([]string, 0, len(list))
	for i, c := range list {
		s, isString := c.(string)
		if !isString || !_hex.MatchString(s) {
			*errs = append(*errs, badColor(fmt.Sprintf("theme.brandBar[%d]", i), c))
			continue
		}
		out = append(out, s)
	}
	return out
}

func badColor(key string, v any) error {
	return fmt.Errorf("%s: %v is not a quoted hex color (#rgb, #rrggbb or #rrggbbaa)", key, v)
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
