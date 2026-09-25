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

	"github.com/jalet/pulumi-operator-ui/internal/auth"
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
				Commit: "<script>alert(1)</script>", StartedAt: &started, EndedAt: &ended},
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
		`id="stack-overview"`,
		`hx-trigger="sse:stacks, sse:resync"`,
		`id="stack-counters"`,
		`hx-get="/fragments/stacks/counters"`,
		`hx-trigger="sse:stack-any delay:500ms"`,
		`hx-trigger="sse:` + events.StackEventName("ns", "app") + `"`,
		`hx-get="/fragments/stacks/ns/app"`,
		`<script src="/static/htmx.min.js?v=`,
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
		{"/fragments/stacks", `id="stack-overview"`},
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
	if _, page := get(t, srv, "/stacks/ns/app"); !strings.Contains(page, "Deleted") {
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

func TestStackPageDefaultTypes(t *testing.T) {
	r := sampleReader()
	r.stats = store.StackStats{HiddenPreviews: 5}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	if r.gotF == nil || len(r.gotF.Types) != 0 {
		t.Fatalf("filter = %+v, want default", r.gotF)
	}
	for _, want := range []string{
		`<a class="chip chip-on" href="/stacks/ns/app?types=refresh,destroy" aria-current="true">Up</a>`,
		`<a class="chip chip-on" href="/stacks/ns/app?types=up,destroy" aria-current="true">Refresh</a>`,
		`<a class="chip chip-on" href="/stacks/ns/app?types=up,refresh" aria-current="true">Destroy</a>`,
		`<a class="chip" href="/stacks/ns/app?types=up,refresh,destroy,preview">Preview (5 hidden)</a>`,
		`hx-get="/fragments/stacks/ns/app/runs"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
}

func TestStackPageTypesParam(t *testing.T) {
	r := sampleReader()
	r.next = &store.Cursor{At: time.Unix(0, 5), ID: 3}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app?types=preview")
	if r.gotF == nil || len(r.gotF.Types) != 1 || r.gotF.Types[0] != store.RunTypePreview {
		t.Fatalf("filter = %+v, want [preview]", r.gotF)
	}
	for _, want := range []string{
		`hx-get="/fragments/stacks/ns/app/runs?types=preview"`,
		`href="/stacks/ns/app?types=preview&amp;before=5.3"`,
		`<a class="chip chip-on" href="/stacks/ns/app" aria-current="true">Preview</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
}

func TestStackPageBadTypes(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	for _, path := range []string{"/stacks/ns/app?types=bogus", "/fragments/stacks/ns/app/runs?types=UP"} {
		if resp, _ := get(t, srv, path); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestRunsFragmentKeepsTypes(t *testing.T) {
	r := sampleReader()
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/fragments/stacks/ns/app/runs?types=up")
	if r.gotF == nil || len(r.gotF.Types) != 1 || r.gotF.Types[0] != store.RunTypeUp {
		t.Fatalf("filter = %+v, want [up]", r.gotF)
	}
	if !strings.Contains(body, `hx-get="/fragments/stacks/ns/app/runs?types=up"`) {
		t.Error("refreshed fragment dropped the types")
	}
}

func TestStackHeaderStats(t *testing.T) {
	r := sampleReader()
	r.stats = store.StackStats{Total: 3, Succeeded: 2, Failed: 1}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	for _, want := range []string{">67%<", ">3<", "Runs (7d)", "Success", `<span class="pill tone-ok">`} {
		if !strings.Contains(body, want) {
			t.Errorf("header lacks %s", want)
		}
	}
}

func TestEmptyTimelineOffersPreviews(t *testing.T) {
	r := sampleReader()
	r.runs = nil
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	if !strings.Contains(body, "No runs of the selected types.") ||
		!strings.Contains(body, `href="/stacks/ns/app?types=up,refresh,destroy,preview"`) {
		t.Fatalf("empty state missing:\n%s", body)
	}
}

func TestStackTableScrolls(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/stacks/ns/app")
	if !strings.Contains(body, `<div class="panel overflow-x-auto">`) {
		t.Error("run table is not in a horizontally scrolling panel")
	}
}
func twoNamespaceReader() *fakeReader {
	r := sampleReader()
	r.stacks = append(r.stacks, store.StackSummary{Stack: store.Stack{Namespace: "infra",
		Name: "net", Stalled: true, UpdatedAt: _now}})
	return r
}

func TestListCounters(t *testing.T) {
	srv := newServer(t, twoNamespaceReader(), nil)
	_, body := get(t, srv, "/")
	if n := strings.Count(body, `<div class="counter">`); n != 4 {
		t.Fatalf("%d counters, want 4", n)
	}
	for _, want := range []string{
		`>2</div><div class="text-xs text-muted">Stacks</div>`,
		`>1</div><div class="text-xs text-muted">Ready</div>`,
		`>0</div><div class="text-xs text-muted">Reconciling</div>`,
		`>1</div><div class="text-xs text-muted">Needs attention</div>`,
		`<span class="pill tone-bad">`, // the stalled stack's health pill
		`5m0s`,                         // last up duration
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
}

func TestNamespaceChips(t *testing.T) {
	srv := newServer(t, twoNamespaceReader(), nil)
	_, body := get(t, srv, "/?ns=ns")
	for _, want := range []string{
		`<a class="chip" href="/">All</a>`,
		`<a class="chip chip-on" href="/?ns=ns" aria-current="true">ns</a>`,
		`<a class="chip" href="/?ns=infra">infra</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s", want)
		}
	}
	if strings.Contains(body, "/stacks/infra/net") {
		t.Error("filtered list shows a stack from another namespace")
	}
	resp, body := get(t, srv, "/?ns=other")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "No stacks in this namespace") {
		t.Fatalf("unknown namespace = %d", resp.StatusCode)
	}
	if _, all := get(t, srv, "/"); !strings.Contains(all, `<a class="chip chip-on" href="/" aria-current="true">All</a>`) {
		t.Error("All chip not selected without ns")
	}
}

func TestListFragmentKeepsNamespace(t *testing.T) {
	srv := newServer(t, twoNamespaceReader(), nil)
	_, body := get(t, srv, "/fragments/stacks?ns=infra")
	if !strings.Contains(body, `hx-get="/fragments/stacks?ns=infra"`) {
		t.Error("overview fragment dropped the namespace")
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment returned a full page")
	}
}

func TestListLayoutScrolls(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/")
	if !strings.Contains(body, `<div class="panel overflow-x-auto">`) {
		t.Error("stack table is not in a horizontally scrolling panel")
	}
}

func TestRunPageLayout(t *testing.T) {
	r := sampleReader()
	r.runs[0].State = store.RunStateFailed
	r.runs[0].Message = strings.Repeat("x", 300) // one long unbroken token
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/runs/7")
	for _, want := range []string{
		`href="/stacks/ns/app"`,
		`<span class="pill tone-bad">`,
		">Commit<", ">Started<", ">Duration<", ">Stack<",
		`<button type="button" class="copy-btn" data-copy="abc">Copy</button>`,
		`whitespace-pre-wrap break-words`,
		`message-failed`,
		"appear here with log capture",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %s", want)
		}
	}
}

func TestRunPageSucceededHasNoFailedTint(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	if _, body := get(t, srv, "/runs/7"); strings.Contains(body, "message-failed") {
		t.Fatal("succeeded run has the failed tint")
	}
}

// One stack event must refresh the counters only, not re-swap the table and chips (which
// would drop keyboard focus and horizontal scroll).
func TestOverviewDoesNotSwapOnEveryStackEvent(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/")
	before, after, ok := strings.Cut(body, `id="stack-overview"`)
	if !ok {
		t.Fatal("no overview section")
	}
	tag, _, _ := strings.Cut(after, ">")
	tag = before[strings.LastIndex(before, "<"):] + tag
	if strings.Contains(tag, "stack-any") {
		t.Fatalf("overview swaps on stack-any: %s", tag)
	}
}

func TestCountersFragment(t *testing.T) {
	srv := newServer(t, twoNamespaceReader(), nil)
	resp, body := get(t, srv, "/fragments/stacks/counters?ns=infra")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="stack-counters"`) ||
		strings.Contains(body, "<table") {
		t.Fatalf("counters fragment = %d:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `hx-get="/fragments/stacks/counters?ns=infra"`) ||
		!strings.Contains(body, `>1</div><div class="text-xs text-muted">Stacks</div>`) {
		t.Errorf("counters fragment lost the namespace:\n%s", body)
	}
}

func TestFragmentURLsEscapeNamespace(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/?ns=a%26b%3Dc")
	if strings.Contains(body, "ns=a&amp;b=c") || strings.Contains(body, "ns=a&b=c") {
		t.Fatal("namespace not query-escaped in fragment URLs")
	}
	if !strings.Contains(body, `hx-get="/fragments/stacks?ns=a%26b%3Dc"`) {
		t.Errorf("overview URL not escaped:\n%s", body)
	}
}

func TestHeaderShowsSignedInUser(t *testing.T) {
	signedIn := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s := auth.Session{Subject: "u1", Name: "Joakim <J>", Email: "j@example.com",
				ExpiresAt: time.Now().Add(time.Hour)}
			next.ServeHTTP(w, r.WithContext(auth.WithSession(r.Context(), s)))
		})
	}
	srv := newServer(t, sampleReader(), signedIn)
	_, body := get(t, srv, "/")
	if !strings.Contains(body, "Joakim &lt;J&gt;") {
		t.Errorf("header lacks the signed-in user's name")
	}
	emailOnly := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s := auth.Session{Subject: "u1", Email: "j@example.com", ExpiresAt: time.Now().Add(time.Hour)}
			next.ServeHTTP(w, r.WithContext(auth.WithSession(r.Context(), s)))
		})
	}
	srv2 := newServer(t, sampleReader(), emailOnly)
	if _, body := get(t, srv2, "/stacks/ns/app"); !strings.Contains(body, "j@example.com") {
		t.Error("header lacks the email fallback")
	}
}
