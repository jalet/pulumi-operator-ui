package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// noRedirect returns the first response instead of following a redirect.
func noRedirect(t *testing.T, srv interface {
	Client() *http.Client
}, url string) *http.Response {
	t.Helper()
	c := *srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestStackPageLimit(t *testing.T) {
	for _, tc := range []struct {
		query  string
		status int
		limit  int
	}{
		{"", http.StatusOK, 50},
		{"?limit=10", http.StatusOK, 10},
		{"?limit=25", http.StatusOK, 25},
		{"?limit=100", http.StatusOK, 100},
		{"?limit=7", http.StatusBadRequest, 0},
		{"?limit=abc", http.StatusBadRequest, 0},
		{"?limit=500", http.StatusBadRequest, 0},
	} {
		r := sampleReader()
		resp, _ := get(t, newServer(t, r, nil), "/stacks/ns/app"+tc.query)
		if resp.StatusCode != tc.status || (tc.limit > 0 && r.gotLimit != tc.limit) {
			t.Errorf("%q: status %d limit %d, want %d and %d", tc.query, resp.StatusCode,
				r.gotLimit, tc.status, tc.limit)
		}
	}
}

func TestStackPageBeforeAndAfter(t *testing.T) {
	resp, _ := get(t, newServer(t, sampleReader(), nil),
		"/stacks/ns/app?before=1758801600000000000.5&after=1758801600000000000.4")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for both cursors", resp.StatusCode)
	}
	resp, _ = get(t, newServer(t, sampleReader(), nil), "/stacks/ns/app?after=nope")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a bad after cursor", resp.StatusCode)
	}
}

// Nothing newer than a full page: the newer page is the latest one, which is live.
func TestStackPageAfterRedirectsToLatest(t *testing.T) {
	r := sampleReader()
	srv := newServer(t, r, nil)
	resp := noRedirect(t, srv, srv.URL+"/stacks/ns/app?after=1758801600000000000.4&limit=25&previews=all")
	if resp.StatusCode != http.StatusSeeOther ||
		resp.Header.Get("Location") != "/stacks/ns/app?limit=25&previews=all" {
		t.Fatalf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if r.gotAfter == nil || r.gotAfter.ID != 4 {
		t.Fatalf("anchor asked about %+v, want id 4", r.gotAfter)
	}
}

func TestStackPageAfterUsesAnchor(t *testing.T) {
	r := sampleReader()
	r.anchor = &store.Cursor{At: time.Unix(0, 1758801600000000000).UTC(), ID: 9}
	resp, body := get(t, newServer(t, r, nil), "/stacks/ns/app?after=1758801600000000000.4")
	if resp.StatusCode != http.StatusOK || r.gotCur == nil || r.gotCur.ID != 9 {
		t.Fatalf("status %d, ListTimeline before = %+v, want the anchor", resp.StatusCode, r.gotCur)
	}
	if strings.Contains(body, "hx-get=\"/fragments/stacks/") {
		t.Error("a newer page that is not the latest must not live-update")
	}
}

func TestStackPagerOnLatestPage(t *testing.T) {
	r := sampleReader()
	r.next = &store.Cursor{At: time.Unix(0, 1758801600000000000).UTC(), ID: 5}
	_, body := get(t, newServer(t, r, nil), "/stacks/ns/app?limit=25")
	if !strings.Contains(body, `href="/stacks/ns/app?before=1758801600000000000.5&amp;limit=25"`) {
		t.Error("Older link lacks the cursor or the page size")
	}
	if strings.Contains(body, ">Newer<") || strings.Contains(body, ">Latest<") {
		t.Error("the latest page offers Newer or Latest")
	}
}

func TestStackPagerOnOlderPage(t *testing.T) {
	r := sampleReader()
	newest := formatCursor(&store.Cursor{At: *r.runs[0].StartedAt, ID: r.runs[0].ID})
	_, body := get(t, newServer(t, r, nil), "/stacks/ns/app?before=1758801600000000000.5&previews=all")
	for _, want := range []string{
		`href="/stacks/ns/app?after=` + newest + `&amp;previews=all">Newer<`,
		`href="/stacks/ns/app?previews=all">Latest<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("older page lacks %s", want)
		}
	}
	if strings.Contains(body, ">Older<") {
		t.Error("the oldest page offers Older")
	}
}

func TestStackPageSizeLinks(t *testing.T) {
	_, body := get(t, newServer(t, sampleReader(), nil), "/stacks/ns/app?before=1758801600000000000.5&limit=25")
	for _, want := range []string{
		`href="/stacks/ns/app?before=1758801600000000000.5&amp;limit=10">10<`,
		`aria-current="true">25<`,
		`href="/stacks/ns/app?before=1758801600000000000.5">50<`,
		`href="/stacks/ns/app?before=1758801600000000000.5&amp;limit=100">100<`,
		`href="/stacks/ns/app?before=1758801600000000000.5&amp;limit=25&amp;previews=all">Show all previews<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func TestStackRunsFragmentKeepsLimit(t *testing.T) {
	r := sampleReader()
	srv := newServer(t, r, nil)
	_, page := get(t, srv, "/stacks/ns/app?limit=10")
	if !strings.Contains(page, `hx-get="/fragments/stacks/ns/app/runs?limit=10"`) {
		t.Error("the live fragment URL drops the page size")
	}
	resp, _ := get(t, srv, "/fragments/stacks/ns/app/runs?limit=10")
	if resp.StatusCode != http.StatusOK || r.gotLimit != 10 {
		t.Fatalf("fragment status %d limit %d, want 200 and 10", resp.StatusCode, r.gotLimit)
	}
	resp, _ = get(t, srv, "/fragments/stacks/ns/app/runs?limit=3")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fragment status %d, want 400 for a bad size", resp.StatusCode)
	}
}
