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
	"regexp"
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
	stats   store.StackStats

	gotLimit int           // the page size ListTimeline was asked for
	anchor   *store.Cursor // what TimelineNewerAnchor answers
	gotAfter *store.Cursor // the cursor TimelineNewerAnchor was asked about
}

func (f *fakeReader) TimelineNewerAnchor(_ context.Context, _, _ string, after store.Cursor,
	_ int) (store.Cursor, bool, error) {
	f.gotAfter = &after
	if f.anchor == nil {
		return store.Cursor{}, false, nil
	}
	return *f.anchor, true, nil
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

func (f *fakeReader) ListTimeline(_ context.Context, _, _ string, before *store.Cursor,
	limit int) ([]store.Run, []store.Run, *store.Cursor, error) {
	f.gotCur, f.gotLimit = before, limit
	var changes, previews []store.Run
	for _, r := range f.runs {
		if r.Type == store.RunTypePreview {
			previews = append(previews, r)
		} else {
			changes = append(changes, r)
		}
	}
	return changes, previews, f.next, nil
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
		{"/fragments/stacks/ns/app/runs", `class="rail"`},
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
		"approximate: taken from the Stack when the run was first seen", `hx-trigger="sse:run-7, sse:resync"`} {
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
	resp, _ := get(t, srv, "/static/htmx.min.js?v="+assetVersions()["htmx.min.js"])
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
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %s", want)
		}
	}
}

func TestRunPageNeverStartedFailure(t *testing.T) {
	r := sampleReader()
	r.runs[0].State, r.runs[0].StartedAt, r.runs[0].EndedAt = store.RunStateFailed, nil, nil
	srv := newServer(t, r, nil)
	for _, path := range []string{"/runs/7", "/stacks/ns/app"} {
		_, body := get(t, srv, path)
		if strings.Contains(body, "not started") || !strings.Contains(body, "not recorded") {
			t.Errorf("%s: a failed run without a start time must read \"not recorded\"", path)
		}
	}
}

func TestRunPageNoPhasePlaceholder(t *testing.T) {
	_, body := get(t, newServer(t, sampleReader(), nil), "/runs/7")
	if strings.Contains(body, "phase 2") || strings.Contains(body, "log capture") {
		t.Error("run page still shows the roadmap placeholder")
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

func TestRunPageShowsChanges(t *testing.T) {
	r := sampleReader()
	r.runs[0].Changes = map[string]int64{"create": 2, "same": 5}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/runs/7")
	for _, want := range []string{"2 created", "5 unchanged", "from Pulumi history"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %q", want)
		}
	}
	if strings.Contains(body, "No change details") {
		t.Error("placeholder shown despite changes")
	}
}

func TestRunPageImportedBadge(t *testing.T) {
	r := sampleReader()
	r.runs[0].UpdateName = "s3:prod-179"
	srv := newServer(t, r, nil)
	if _, body := get(t, srv, "/runs/7"); !strings.Contains(body, "imported from Pulumi history") {
		t.Error("imported run has no badge")
	}
	if _, body := get(t, newServer(t, sampleReader(), nil), "/runs/7"); strings.Contains(body, "imported from Pulumi history") {
		t.Error("operator run shows the imported badge")
	}
}

func TestTimelineChangesColumn(t *testing.T) {
	r := sampleReader()
	r.runs[0].Changes = map[string]int64{"create": 2}
	srv := newServer(t, r, nil)
	_, body := get(t, srv, "/stacks/ns/app")
	// html/template escapes "+" in text as &#43;; browsers show "+2" either way.
	if !strings.Contains(body, ">+2<") && !strings.Contains(body, ">&#43;2<") {
		t.Errorf("timeline lacks the Changes column:\n%s", body)
	}
}

// newServerS3 is newServer with S3 history enabled at the given interval.
func newServerS3(t *testing.T, r *fakeReader, interval time.Duration) *httptest.Server {
	t.Helper()
	h := New(Deps{Store: r, Broker: events.NewBroker(), RequireAuth: passthrough,
		AuthRoutes: func(*http.ServeMux) {}, Log: zerolog.Nop(), Now: func() time.Time { return _now },
		S3Interval: interval})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// A stale s3_error stays on the row after S3 history is turned off; it must not show then.
func TestStackS3NoticeHiddenWhenS3Off(t *testing.T) {
	r := sampleReader()
	r.stacks[0].S3Error = "access denied"
	if _, body := get(t, newServer(t, r, nil), "/stacks/ns/app"); strings.Contains(body, "Pulumi history unavailable") {
		t.Error("S3 notice shown with S3 history off")
	}
}

func TestStackS3Notice(t *testing.T) {
	r := sampleReader()
	r.stacks[0].S3Error = "access denied"
	srv := newServerS3(t, r, 2*time.Minute)
	if _, body := get(t, srv, "/stacks/ns/app"); !strings.Contains(body, "Pulumi history unavailable: access denied") {
		t.Error("no S3 notice")
	}
	if _, body := get(t, newServer(t, sampleReader(), nil), "/stacks/ns/app"); strings.Contains(body, "Pulumi history unavailable") {
		t.Error("notice shown without an error")
	}
}

func TestRunPageListsResources(t *testing.T) {
	r := sampleReader()
	r.runs[0].LogStatus = store.LogStatusCaptured
	r.runs[0].LogChanges = map[string]int64{"update": 1, "same": 116}
	r.runs[0].Resources = []store.LogResource{{Op: "update", Type: "aws:iam/userPolicy:UserPolicy",
		Name: "pulumi-operator-ui", Diff: "~ policy: {\n    + Sid: \"ListHistory\"\n}"}}
	_, body := get(t, newServer(t, r, nil), "/runs/7")
	for _, want := range []string{"1 updated", "116 unchanged", "from engine log",
		"<details", "iam/UserPolicy", `title="aws:iam/userPolicy:UserPolicy"`,
		"pulumi-operator-ui", `class="diff-add"`, "&#43; Sid: &#34;ListHistory&#34;"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page lacks %s", want)
		}
	}
}

func TestRunPageDiffEscaped(t *testing.T) {
	r := sampleReader()
	r.runs[0].LogStatus = store.LogStatusCaptured
	r.runs[0].Resources = []store.LogResource{{Op: "create", Type: "a:b/c:D", Name: "x",
		Diff: `+ tag: "<script>alert(1)</script>"`}}
	_, body := get(t, newServer(t, r, nil), "/runs/7")
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("diff rendered unescaped")
	}
}

func TestRunPagePendingNote(t *testing.T) {
	r := sampleReader()
	r.runs[0].LogStatus = store.LogStatusPending
	_, body := get(t, newServer(t, r, nil), "/runs/7")
	if !strings.Contains(body, "Reading the engine log...") {
		t.Fatal("pending note missing")
	}
}

func TestRunPageDecodesOperatorMessage(t *testing.T) {
	r := sampleReader()
	r.runs[0].Message = `"New commit detected: \"2ea4\""`
	_, body := get(t, newServer(t, r, nil), "/runs/7")
	if !strings.Contains(body, "New commit detected: &#34;2ea4&#34;") ||
		strings.Contains(body, `\&#34;`) {
		t.Fatal("operator message not decoded")
	}
}

func TestRunPageTruncatedNote(t *testing.T) {
	r := sampleReader()
	r.runs[0].LogStatus, r.runs[0].LogTruncated = store.LogStatusCaptured, true
	r.runs[0].Resources = []store.LogResource{{Op: "create", Type: "a:b/c:D", Name: "x"}}
	_, body := get(t, newServer(t, r, nil), "/runs/7")
	if !strings.Contains(body, "The change list is incomplete") {
		t.Fatal("truncation note missing")
	}
}

func TestTimelineFallsBackToLogCounts(t *testing.T) {
	r := sampleReader()
	r.runs[0].LogChanges = map[string]int64{"update": 1, "same": 116}
	_, body := get(t, newServer(t, r, nil), "/stacks/ns/app")
	if !strings.Contains(body, "~1") {
		t.Fatal("timeline lacks the engine log counts")
	}
}

var _changesURL = regexp.MustCompile(`hx-get="(/fragments/runs/7/changes\?v=[0-9a-f]{12})"`)

// The changes panel is refreshed apart from the header and only re-rendered when its content
// changed, so a diff the user opened stays open across live updates.
func TestRunChangesFragmentSkipsUnchanged(t *testing.T) {
	r := sampleReader()
	r.runs[0].LogStatus = store.LogStatusCaptured
	r.runs[0].Resources = []store.LogResource{{Op: "create", Type: "a:b/c:D", Name: "x", Diff: "k: 1"}}
	srv := newServer(t, r, nil)
	_, page := get(t, srv, "/runs/7")
	m := _changesURL.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("run page lacks a versioned changes fragment URL")
	}
	if !strings.Contains(page, `hx-trigger="sse:run-7, sse:resync"`) {
		t.Error("run page does not listen to sse:resync")
	}
	url := strings.ReplaceAll(m[1], "&amp;", "&")
	if resp, _ := get(t, srv, url); resp.StatusCode != http.StatusNoContent {
		t.Errorf("unchanged panel: status %d, want 204", resp.StatusCode)
	}
	r.runs[0].Resources = append(r.runs[0].Resources, store.LogResource{Op: "delete", Name: "y"})
	resp, body := get(t, srv, url)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, ">y<") {
		t.Errorf("changed panel: status %d, want 200 with the new resource", resp.StatusCode)
	}
	if _, hdr := get(t, srv, "/fragments/runs/7/header"); strings.Contains(hdr, "<details") {
		t.Error("header fragment still carries the changes panel")
	}
}
func TestRunPageLinksCommit(t *testing.T) {
	r := sampleReader()
	r.runs[0].Commit = "0123456789abcdef0123456789abcdef01234567"
	r.runs[0].StackRepoURL = "git@github.com:o/r.git"
	_, body := get(t, newServer(t, r, nil), "/runs/7")
	if !strings.Contains(body, `href="https://github.com/o/r/commit/0123456789abcdef0123456789abcdef01234567"`) ||
		!strings.Contains(body, `rel="noopener noreferrer"`) {
		t.Fatal("run page does not link the commit")
	}
}
func TestTimelineShowsTitleAndOrigin(t *testing.T) {
	r := sampleReader()
	r.runs[0].UpdateName, r.runs[0].UID = "s3:prod-1", ""
	r.runs[0].Title, r.runs[0].ExecKind = "chore: <tidy>", "cli"
	_, body := get(t, newServer(t, r, nil), "/stacks/ns/app")
	for _, want := range []string{"chore: &lt;tidy&gt;", ">laptop<"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline lacks %s", want)
		}
	}
}

func TestStackPageRendersRail(t *testing.T) {
	r := sampleReader()
	seq := int64(69)
	r.runs[0].Seq = &seq
	r.runs[0].ExecKind = "auto.local"
	r.runs[0].Summary = []store.ResourceRef{{Type: "aws:iam/userPolicy:UserPolicy", Name: "pulumi-operator-ui"}}
	r.runs[0].ResourceTotal = 1
	_, body := get(t, newServer(t, r, nil), "/stacks/ns/app")
	for _, want := range []string{`class="rail"`, ">#69<", "iam/UserPolicy pulumi-operator-ui", ">operator<",
		"counts updates in the state bucket's history"} {
		if !strings.Contains(body, want) {
			t.Errorf("stack page lacks %s", want)
		}
	}
	if strings.Contains(body, `aria-label="Run types"`) {
		t.Error("type chips still rendered")
	}
}

func TestStackPageIgnoresTypes(t *testing.T) {
	resp, _ := get(t, newServer(t, sampleReader(), nil), "/stacks/ns/app?types=up,bogus")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 for an old ?types= link", resp.StatusCode)
	}
}

func TestStackPagePreviewsToggle(t *testing.T) {
	r := sampleReader()
	at := _now.Add(-time.Minute)
	r.runs = append(r.runs, store.Run{ID: 8, Namespace: "ns", UpdateName: "pv1", StackName: "app",
		Type: store.RunTypePreview, State: store.RunStateSucceeded, StartedAt: &at, ObservedAt: at})
	srv := newServer(t, r, nil)
	_, folded := get(t, srv, "/stacks/ns/app")
	_, all := get(t, srv, "/stacks/ns/app?previews=all")
	if !strings.Contains(folded, "<details") || !strings.Contains(folded, `href="/stacks/ns/app?previews=all"`) {
		t.Error("folded page lacks the fold or the toggle")
	}
	if !strings.Contains(all, `id="run-8"`) || !strings.Contains(all, `href="/stacks/ns/app"`) {
		t.Error("expanded page does not list the preview as a node or lacks the toggle back")
	}
}

func TestRunningRefreshPulses(t *testing.T) {
	r := sampleReader()
	r.runs[0].Type, r.runs[0].State, r.runs[0].EndedAt = store.RunTypeRefresh, store.RunStateRunning, nil
	if _, body := get(t, newServer(t, r, nil), "/stacks/ns/app"); !strings.Contains(body, `class="rail-dot rail-dot-run"`) {
		t.Error("a running refresh does not show the pulsing dot")
	}
}

func TestFoldHasStableID(t *testing.T) {
	r := sampleReader()
	at := _now.Add(-time.Minute)
	r.runs = append(r.runs, store.Run{ID: 8, Namespace: "ns", UpdateName: "pv1", StackName: "app",
		Type: store.RunTypePreview, State: store.RunStateSucceeded, StartedAt: &at, ObservedAt: at})
	if _, body := get(t, newServer(t, r, nil), "/stacks/ns/app"); !strings.Contains(body, `<details class="rail-fold" id="fold-8"`) {
		t.Error("the preview fold has no stable id")
	}
}
