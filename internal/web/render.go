package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/events"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

//go:embed templates/*.html
var _templates embed.FS

//go:embed static
var _static embed.FS

// Each page parses the shared layout plus the files whose fragments it uses; a later
// file's "title" and "content" override an earlier one's.
var _pageFiles = map[string][]string{
	"stacks": {"templates/layout.html", "templates/components.html", "templates/stacks.html"},
	"stack": {"templates/layout.html", "templates/components.html", "templates/stacks.html",
		"templates/stack.html"},
	"run":   {"templates/layout.html", "templates/components.html", "templates/run.html"},
	"error": {"templates/layout.html", "templates/components.html", "templates/error.html"},
}

func parsePages(now func() time.Time) map[string]*template.Template {
	funcs := template.FuncMap{
		"age":         func(t any) string { return age(now(), t) },
		"iso":         func(t any) string { return timeOf(t).UTC().Format(time.RFC3339) },
		"shortCommit": shortCommit,
		"health":      health,
		"stateBadge":  stateBadge,
		"successRate": successRate,
		"duration":    duration,
		"stackEvent":  events.StackEventName,
		"cursor":      formatCursor,
	}
	pages := make(map[string]*template.Template, len(_pageFiles))
	for name, files := range _pageFiles {
		// A template that fails to parse is a programmer error caught by every test run.
		pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(_templates, files...))
	}
	return pages
}

// render executes into a buffer first so a template error becomes a clean 500 instead
// of a half-written page.
func (s *server) render(w http.ResponseWriter, page, name string, data any, status int) {
	t, ok := s.pages[page]
	if !ok {
		panic("invariant violated: unknown page " + page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error().Err(err).Str("page", page).Str("template", name).Msg("render")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w) // a client that went away is not an error worth logging
}

func (s *server) renderError(w http.ResponseWriter, status int, title, detail string) {
	s.render(w, "error", "layout", struct{ Title, Detail string }{title, detail}, status)
}

func timeOf(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case *time.Time:
		if t != nil {
			return *t
		}
	}
	return time.Time{}
}

func age(now time.Time, v any) string {
	t := timeOf(v)
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func duration(start, end *time.Time) string {
	switch {
	case start == nil:
		return ""
	case end == nil:
		return "running"
	default:
		return end.Sub(*start).Round(time.Second).String()
	}
}

func formatCursor(c *store.Cursor) string {
	return strconv.FormatInt(c.At.UnixNano(), 10) + "." + strconv.FormatInt(c.ID, 10)
}

func parseCursor(s string) (*store.Cursor, bool) {
	at, id, ok := strings.Cut(s, ".")
	if !ok {
		return nil, false
	}
	nanos, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return nil, false
	}
	runID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || runID <= 0 {
		return nil, false
	}
	return &store.Cursor{At: time.Unix(0, nanos).UTC(), ID: runID}, true
}
