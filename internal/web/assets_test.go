package web

import (
	"io/fs"
	"regexp"
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
