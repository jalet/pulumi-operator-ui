package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRendersEveryBlock(t *testing.T) {
	th, err := Parse([]byte(`
light: {page: "#ffffff", okText: "#1E7A3A"}
dark: {bad: "#f28b8280"}
brandBar: ["#111", "#222", "#333", "#444", "#555"]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := `:root {
  --pou-brand-1: #111;
  --pou-brand-2: #222;
  --pou-brand-3: #333;
  --pou-brand-4: #444;
  --pou-brand-5: #555;
}
@media not all and (prefers-color-scheme: dark) {
  :root {
    --pou-page: #ffffff;
    --pou-ok-text: #1E7A3A;
  }
}
@media (prefers-color-scheme: dark) {
  :root {
    --pou-bad: #f28b8280;
  }
}
`
	if got := string(th.CSS()); got != want {
		t.Fatalf("CSS =\n%s\nwant\n%s", got, want)
	}
}

// The sheet is unlayered, so a bare :root would beat app.css's dark defaults too.
func TestLightStaysOutOfDark(t *testing.T) {
	th, err := Parse([]byte(`light: {page: "#ffffff"}`))
	if err != nil {
		t.Fatal(err)
	}
	css := string(th.CSS())
	if !strings.HasPrefix(css, "@media not all and (prefers-color-scheme: dark) {") ||
		strings.Contains(css, "@media (prefers-color-scheme: dark)") {
		t.Fatalf("light-only CSS leaks into dark mode:\n%s", css)
	}
}

func TestPartialThemeRendersOnlyItsKeys(t *testing.T) {
	th, err := Parse([]byte(`dark: {page: "#000000"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "@media (prefers-color-scheme: dark) {\n  :root {\n    --pou-page: #000000;\n  }\n}\n"
	if got := string(th.CSS()); got != want {
		t.Fatalf("CSS = %q, want %q", got, want)
	}
}

func TestEmptyThemeRendersNothing(t *testing.T) {
	th, err := Parse(nil)
	if err != nil || len(th.CSS()) != 0 {
		t.Fatalf("err %v css %q, want no error and no CSS", err, th.CSS())
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ doc, want string }{
		{`colors: {page: "#fff"}`, "colors"},
		{`dark: {bda: "#fff"}`, "theme.dark.bda: unknown key"},
		{`light: {page: "#ggg"}`, "theme.light.page"},
		{`light: {page: "#abcd"}`, "theme.light.page"},
		{`light: {page: red}`, "theme.light.page"},
		{`light: {page: "#fff; } body { display: none"}`, "theme.light.page"},
		{"light:\n  page: #fff\n", "theme.light.page"}, // unquoted: a YAML comment, so null
		{`brandBar: ["#111", "#222", "#333", "#444"]`, "theme.brandBar: has 4 colors, want 5"},
		{`brandBar: ["#111", "#222", "#333", "#444", "blue"]`, "theme.brandBar[4]"},
		{`LIGHT: {page: "#fff"}`, "theme.LIGHT: unknown key"},
		{`light: {page: 123}`, "theme.light.page"},
		{`light: [1]`, "theme.light: want a map"},
		{`brandBar: "#fff"`, "theme.brandBar: want a list"},
	} {
		if _, err := Parse([]byte(tc.doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want it to mention %q", tc.doc, err, tc.want)
		}
	}
}

func TestVar(t *testing.T) {
	for token, want := range map[string]string{"page": "--pou-page", "okText": "--pou-ok-text",
		"attention": "--pou-attention", "neutralText": "--pou-neutral-text"} {
		if got := Var(token); got != want {
			t.Errorf("Var(%q) = %q, want %q", token, got, want)
		}
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "theme.yaml")
	if err := os.WriteFile(p, []byte(`light: {ink: "#000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	th, err := Load(p)
	if err != nil || th.Light["ink"] != "#000" {
		t.Fatalf("Load = %+v, %v", th, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil ||
		!strings.Contains(err.Error(), "missing.yaml") {
		t.Fatalf("missing file err = %v, want the path", err)
	}
}
