package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	_inlineStyle   = regexp.MustCompile(`\sstyle\s*=`)
	_inlineHandler = regexp.MustCompile(`\son[a-z]+\s*=`)
	_scriptTag     = regexp.MustCompile(`<script\b[^>]*>`)
)

// The CSP allows no inline styles, handlers or scripts; a template that adds one would
// render broken in the browser while every Go test still passes.
func TestTemplatesAreCSPClean(t *testing.T) {
	err := fs.WalkDir(_templates, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(_templates, p)
		if err != nil {
			return err
		}
		src := string(b)
		if _inlineStyle.MatchString(src) {
			t.Errorf("%s: inline style attribute", p)
		}
		if _inlineHandler.MatchString(src) {
			t.Errorf("%s: inline event handler", p)
		}
		for _, tag := range _scriptTag.FindAllString(src, -1) {
			if !strings.Contains(tag, "src=") {
				t.Errorf("%s: inline script %s", p, tag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Tone classes are chosen in Go at runtime, so Tailwind cannot see them in the templates.
func TestCompiledCSSHasComponentClasses(t *testing.T) {
	b, err := _static.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := strings.ToLower(string(b))
	for _, class := range []string{".brand-bar", ".pill", ".tone-ok", ".tone-run", ".tone-att",
		".tone-bad", ".tone-mute", ".panel", ".chip", ".chip-on", ".counter", ".pulse-run",
		".counter-ok", ".counter-run", ".counter-att"} {
		if !strings.Contains(css, class) {
			t.Errorf("compiled CSS lacks %s", class)
		}
	}
	for _, hex := range []string{"#4cc2ff", "#34a853", "#fbbc05", "#ff9902", "#ea4335",
		"#0a0a0f", "#f6f7f9"} {
		if !strings.Contains(css, hex) {
			t.Errorf("compiled CSS lacks brand color %s", hex)
		}
	}
	if !strings.Contains(css, "prefers-color-scheme:dark") &&
		!strings.Contains(css, "prefers-color-scheme: dark") {
		t.Error("compiled CSS has no dark palette")
	}
}

func TestFontsServed(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	for _, f := range []string{"geist-sans-400.woff2", "geist-sans-700.woff2", "geist-mono-400.woff2"} {
		resp, body := get(t, srv, "/static/fonts/"+f)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "font/woff2" || len(body) < 1000 {
			t.Errorf("%s: status %d type %q size %d", f, resp.StatusCode,
				resp.Header.Get("Content-Type"), len(body))
		}
	}
}

func TestLayoutHasBrandBar(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/")
	if !strings.Contains(body, `class="brand-bar"`) {
		t.Error("layout lacks the brand bar")
	}
}

func TestAppJS(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	resp, body := get(t, srv, "/static/app.js")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"data-copy", "navigator.clipboard"} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	for _, banned := range []string{"eval(", "innerHTML", "new Function"} {
		if strings.Contains(body, banned) {
			t.Errorf("app.js uses %s", banned)
		}
	}
}

// contrast is the WCAG 2.2 contrast ratio between two #rgb or #rrggbb colors.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	lum := func(hex string) float64 {
		h := strings.TrimPrefix(hex, "#")
		if len(h) == 3 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		if len(h) != 6 {
			t.Fatalf("bad color %q", hex)
		}
		var rgb [3]float64
		for i := range 3 {
			v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
			if err != nil {
				t.Fatalf("bad color %q", hex)
			}
			c := float64(v) / 255
			if c <= 0.04045 {
				rgb[i] = c / 12.92
			} else {
				rgb[i] = math.Pow((c+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	}
	la, lb := lum(a), lum(b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

// cssVar returns the first value of a custom property inside the given CSS block.
func cssVar(t *testing.T, block, name string) string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(name) + `:(#[0-9a-fA-F]{3,6})`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("no %s in %.80q", name, block)
	}
	return m[1]
}

// The light theme must meet WCAG 2.2 AA: counter values are large bold text (3:1, held to
// 4.5:1 here) and the focus ring is a non-text indicator (3:1). Dark must too.
func TestPaletteContrast(t *testing.T) {
	b, err := _static.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	light := cssBlock(t, css, ":root{color-scheme", "}")
	dark := cssBlock(t, css, "@media (prefers-color-scheme:dark){:root{", "}}")

	lightPage, lightPanel := cssVar(t, light, "--pou-page"), cssVar(t, light, "--pou-panel")
	darkPage, darkPanel := cssVar(t, dark, "--pou-page"), cssVar(t, dark, "--pou-panel")
	checks := []struct {
		name   string
		fg, bg string
		min    float64
	}{
		{"light focus on page", cssVar(t, light, "--pou-focus"), lightPage, 3},
		{"light focus on panel", cssVar(t, light, "--pou-focus"), lightPanel, 3},
		{"dark focus on page", cssVar(t, dark, "--pou-focus"), darkPage, 3},
	}
	for _, tone := range []string{"ok", "run", "att"} {
		checks = append(checks, struct {
			name   string
			fg, bg string
			min    float64
		}{"light counter-" + tone, cssVar(t, light, "--pou-counter-"+tone), lightPanel, 4.5},
			struct {
				name   string
				fg, bg string
				min    float64
			}{"dark counter-" + tone, cssVar(t, dark, "--pou-counter-"+tone), darkPanel, 4.5})
	}
	for _, c := range checks {
		if got := contrast(t, c.fg, c.bg); got < c.min {
			t.Errorf("%s: %s on %s = %.2f:1, want >= %.1f:1", c.name, c.fg, c.bg, got, c.min)
		}
	}
	if !strings.Contains(css, "outline-color:var(--pou-focus)") {
		t.Error("focus ring does not use the themed focus color")
	}
}

// cssBlock returns the text from start up to the first end after it.
func cssBlock(t *testing.T, css, start, end string) string {
	t.Helper()
	_, rest, ok := strings.Cut(css, start)
	if !ok {
		t.Fatalf("compiled CSS lacks %q", start)
	}
	block, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("unterminated block after %q", start)
	}
	return block
}

// Static files are cached as immutable, so every page must link them with a version that
// changes whenever the file does; otherwise browsers keep an old stylesheet for a year.
func TestAssetURLsAreVersioned(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/")
	for _, name := range []string{"app.css", "app.js", "htmx.min.js", "sse.js"} {
		b, err := _static.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		want := "/static/" + name + "?v=" + hex.EncodeToString(sum[:])[:12] + `"`
		if !strings.Contains(body, want) {
			t.Errorf("page does not link %s", want)
		}
		if strings.Contains(body, `"/static/`+name+`"`) {
			t.Errorf("page links unversioned /static/%s", name)
		}
	}
	resp, _ := get(t, srv, "/static/app.css?v=anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("versioned URL = %d", resp.StatusCode)
	}
}

func TestLayoutLinksFavicon(t *testing.T) {
	_, body := get(t, newServer(t, sampleReader(), nil), "/")
	if !regexp.MustCompile(`<link rel="icon" href="/static/favicon\.ico\?v=[0-9a-f]{12}"`).
		MatchString(body) {
		t.Error("layout lacks a versioned favicon link")
	}
}

// Browsers ask for /favicon.ico before anyone signs in, so it is served without a session.
func TestFaviconAtRootWithoutAuth(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	resp, body := get(t, newServer(t, sampleReader(), deny), "/favicon.ico")
	want, err := fs.ReadFile(_static, "static/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body != string(want) {
		t.Fatalf("status %d, %d bytes; want 200 and the static icon", resp.StatusCode, len(body))
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "icon") {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
}

func TestFaviconHasTabSizesOnly(t *testing.T) {
	b, err := fs.ReadFile(_static, "static/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 6 || b[0] != 0 || b[2] != 1 {
		t.Fatal("not an ICO file")
	}
	var sizes []int
	for i := range int(b[4]) {
		sizes = append(sizes, int(b[6+16*i]))
	}
	if !slices.Equal(sizes, []int{16, 32, 48}) || len(b) > 20<<10 {
		t.Errorf("sizes %v, %d bytes; want [16 32 48] and at most 20 KiB", sizes, len(b))
	}
}
