package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/events"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var _now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

type fakeReader struct {
	stacks  []store.StackSummary
	runs    []store.Run
	next    *store.Cursor
	pingErr error
	gotCur  *store.Cursor
	gotF    *store.RunFilter
	stats   store.StackStats
}

func (f *fakeReader) StackStats(context.Context, string, string, store.RunFilter,
	time.Time) (store.StackStats, error) {
	return f.stats, nil
}

func (f *fakeReader) ListStacks(context.Context) ([]store.StackSummary, error) {
	return f.stacks, nil
}

func (f *fakeReader) GetStack(_ context.Context, ns, name string) (store.StackSummary, error) {
	for _, s := range f.stacks {
		if s.Namespace == ns && s.Name == name {
			return s, nil
		}
	}
	return store.StackSummary{}, store.ErrNotFound
}

func (f *fakeReader) ListRuns(_ context.Context, _, _ string, rf store.RunFilter,
	before *store.Cursor, _ int) ([]store.Run, *store.Cursor, error) {
	f.gotCur, f.gotF = before, &rf
	return f.runs, f.next, nil
}

func (f *fakeReader) GetRun(_ context.Context, id int64) (store.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Run{}, store.ErrNotFound
}

func (f *fakeReader) Ping(context.Context) error { return f.pingErr }

func passthrough(next http.Handler) http.Handler { return next }

func newServer(t *testing.T, r *fakeReader, require func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	if require == nil {
		require = passthrough
	}
	h := New(Deps{Store: r, Broker: events.NewBroker(), RequireAuth: require,
		AuthRoutes: func(*http.ServeMux) {}, Log: zerolog.Nop(), Now: func() time.Time { return _now }})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func sampleReader() *fakeReader {
	started := _now.Add(-10 * time.Minute)
	ended := _now.Add(-5 * time.Minute)
	return &fakeReader{
		stacks: []store.StackSummary{{
			Stack: store.Stack{Namespace: "ns", Name: "app", Ready: true, LastCommit: "0123456789abcdef",
				UpdatedAt: _now.Add(-time.Hour)},
			LastUp: &store.RunBrief{ID: 7, State: store.RunStateSucceeded, At: ended,
				Commit: "<script>alert(1)</script>"},
		}},
		runs: []store.Run{{
			ID: 7, Namespace: "ns", UpdateName: "app-u1", StackName: "app", Type: store.RunTypeUp,
			Commit: "abc", CommitSource: store.CommitSourceStack, State: store.RunStateSucceeded,
			Message: "<b>done</b>", StartedAt: &started, EndedAt: &ended, ObservedAt: ended,
		}},
	}
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	for _, path := range []string{"/", "/healthz", "/static/app.css", "/nope", "/runs/7"} {
		resp, _ := get(t, srv, path)
		h := resp.Header
		if h.Get("Content-Security-Policy") != _csp {
			t.Errorf("%s: CSP = %q", path, h.Get("Content-Security-Policy"))
		}
		for k, want := range map[string]string{"X-Content-Type-Options": "nosniff",
			"Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
			if h.Get(k) != want {
				t.Errorf("%s: %s = %q, want %q", path, k, h.Get(k), want)
			}
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	r := sampleReader()
	srv := newServer(t, r, nil)
	if resp, body := get(t, srv, "/healthz"); resp.StatusCode != http.StatusOK || body != "" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}
	if resp, body := get(t, srv, "/readyz"); resp.StatusCode != http.StatusOK || body != "" {
		t.Fatalf("readyz = %d %q", resp.StatusCode, body)
	}
	r.pingErr = errors.New("db down")
	if resp, body := get(t, srv, "/readyz"); resp.StatusCode != http.StatusServiceUnavailable || body != "" {
		t.Fatalf("readyz with db down = %d %q", resp.StatusCode, body)
	}
}

func TestStackListEscapes(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	resp, body := get(t, srv, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("unescaped commit in body")
	}
	if !strings.Contains(body, "app") || !strings.Contains(body, "0123456789ab") {
		t.Fatalf("stack row missing: %s", body)
	}
}

func TestStackListHasSSEWiring(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/")
	for _, want := range []string{
		`sse-connect="/events"`,
		`hx-trigger="sse:stacks, sse:resync"`,
		`hx-trigger="sse:` + events.StackEventName("ns", "app") + `"`,
		`hx-get="/fragments/stacks/ns/app"`,
		`<script src="/static/htmx.min.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
	if strings.Contains(body, "style=") || strings.Contains(body, "<script>") {
		t.Error("inline style or script violates the CSP")
	}
}

func TestFragments(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	tests := []struct {
		path, want string
	}{
		{"/fragments/stacks", "<tbody"},
		{"/fragments/stacks/ns/app", "<tr"},
		{"/fragments/stacks/ns/app/runs", "app-u1"},
		{"/fragments/runs/7/row", "run-7"},
		{"/fragments/runs/7/header", "app-u1"},
	}
	for _, tt := range tests {
		resp, body := get(t, srv, tt.path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, tt.want) {
			t.Errorf("%s = %d, want body containing %q:\n%s", tt.path, resp.StatusCode, tt.want, body)
		}
		if strings.Contains(body, "<html") {
			t.Errorf("%s returned a full page", tt.path)
		}
	}
}

func TestDeletedStackRowFragmentIsEmpty(t *testing.T) {
	r := sampleReader()
	deleted := _now
	r.stacks[0].DeletedAt = &deleted
	srv := newServer(t, r, nil)
	resp, body := get(t, srv, "/fragments/stacks/ns/app")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != "" {
		t.Fatalf("got %d %q, want empty 200 so htmx removes the row", resp.StatusCode, body)
	}
	if _, page := get(t, srv, "/stacks/ns/app"); !strings.Contains(page, "deleted") {
		t.Fatal("stack page does not mark the stack deleted")
	}
}

func TestNotFound(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	for _, path := range []string{"/stacks/ns/nope", "/runs/99", "/runs/abc", "/runs/-1",
		"/fragments/runs/99/row", "/fragments/stacks/ns/nope"} {
		if resp, _ := get(t, srv, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestRunsCursor(t *testing.T) {
	r := sampleReader()
	r.next = &store.Cursor{At: time.Unix(0, 1758801600000000000), ID: 5}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	if !strings.Contains(body, `href="/stacks/ns/app?before=1758801600000000000.5"`) {
		t.Fatalf("no Older link with cursor:\n%s", body)
	}
	if resp, _ := get(t, srv, "/stacks/ns/app?before=1758801600000000000.5"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if r.gotCur == nil || r.gotCur.ID != 5 || r.gotCur.At.UnixNano() != 1758801600000000000 {
		t.Fatalf("cursor passed = %+v", r.gotCur)
	}
	for _, bad := range []string{"garbage", "1.", ".5", "1.x", "1.2.3"} {
		if resp, _ := get(t, srv, "/stacks/ns/app?before="+bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("before=%s = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestRunPage(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	resp, body := get(t, srv, "/runs/7")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{"app-u1", "&lt;b&gt;done&lt;/b&gt;", "5m0s", `class="commit commit-approx"`,
		"approximate: taken from the Stack when the run was first seen", `hx-trigger="sse:run-7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %q", want)
		}
	}
}

func TestExactCommitNotMarked(t *testing.T) {
	r := sampleReader()
	r.runs[0].CommitSource = store.CommitSourceUpdate
	srv := newServer(t, r, nil)
	if _, body := get(t, srv, "/runs/7"); strings.Contains(body, "commit-approx") {
		t.Fatal("exact commit marked approximate")
	}
}

func TestAuthRequired(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	srv := newServer(t, sampleReader(), deny)
	for _, path := range []string{"/", "/stacks/ns/app", "/runs/7", "/fragments/stacks"} {
		if resp, _ := get(t, srv, path); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/static/app.css"} {
		if resp, _ := get(t, srv, path); resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestStaticIntegrity(t *testing.T) {
	sums, err := _static.ReadFile("static/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		b, err := _static.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s sha256 = %s, want %s", name, got, want)
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("checked %d files, want 2", checked)
	}
}

func TestStaticHeaders(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	resp, _ := get(t, srv, "/static/htmx.min.js")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if resp, _ := get(t, srv, "/"); resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("page Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestStackPageHidesPreviewsByDefault(t *testing.T) {
	r := sampleReader()
	r.next = &store.Cursor{At: time.Unix(0, 5), ID: 3}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	if r.gotF == nil || len(r.gotF.Types) != 0 {
		t.Fatalf("filter = %+v, want previews hidden", r.gotF)
	}
	for _, want := range []string{
		`<a href="/stacks/ns/app?previews=1">Show previews</a>`,
		`hx-get="/fragments/stacks/ns/app/runs"`,
		`href="/stacks/ns/app?before=5.3"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
}

func TestStackPageShowsPreviewsWhenAsked(t *testing.T) {
	r := sampleReader()
	r.next = &store.Cursor{At: time.Unix(0, 5), ID: 3}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app?previews=1")
	if r.gotF == nil || len(r.gotF.Types) != 4 {
		t.Fatalf("filter = %+v, want previews shown", r.gotF)
	}
	for _, want := range []string{
		`<a href="/stacks/ns/app">Hide previews</a>`,
		`hx-get="/fragments/stacks/ns/app/runs?previews=1"`,
		`href="/stacks/ns/app?previews=1&amp;before=5.3"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
}

func TestRunsFragmentKeepsPreviewChoice(t *testing.T) {
	r := sampleReader()
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/fragments/stacks/ns/app/runs?previews=1")
	if r.gotF == nil || len(r.gotF.Types) != 4 {
		t.Fatalf("filter = %+v, want previews shown", r.gotF)
	}
	if !strings.Contains(body, `hx-get="/fragments/stacks/ns/app/runs?previews=1"`) {
		t.Error("refreshed fragment dropped the previews choice")
	}
	get(t, srv, "/fragments/stacks/ns/app/runs")
	if len(r.gotF.Types) != 0 {
		t.Fatal("default fragment shows previews")
	}
}

func TestEmptyTimelineOffersPreviews(t *testing.T) {
	r := sampleReader()
	r.runs = nil
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	if !strings.Contains(body, "No up, refresh or destroy runs recorded.") ||
		!strings.Contains(body, `href="/stacks/ns/app?previews=1"`) {
		t.Fatalf("empty state missing:\n%s", body)
	}
}
