package auth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/auth/oidctest"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

type memRecorder struct {
	mu     sync.Mutex
	events []store.AuthEvent
}

func (m *memRecorder) InsertAuthEvent(_ context.Context, e store.AuthEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

func (m *memRecorder) all() []store.AuthEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.AuthEvent(nil), m.events...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testApp struct {
	srv    *httptest.Server
	client *http.Client // follows redirects, keeps cookies
	idp    *oidctest.Provider
	rec    *memRecorder
	clock  *clock
}

func newApp(t *testing.T, claims, userinfo map[string]any) *testApp {
	t.Helper()
	idp, err := oidctest.NewServer(oidctest.Options{ClientID: "pou", ClientSecret: "secret",
		Claims: claims, UserInfo: userinfo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idp.Close)

	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	codec, err := NewCodec(bytes.Repeat([]byte("k"), 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	app := &testApp{srv: srv, idp: idp, rec: &memRecorder{}, clock: &clock{t: time.Now()}}
	a, err := New(t.Context(), Config{
		Issuer: idp.URL, ClientID: "pou", ClientSecret: "secret",
		RedirectURL: srv.URL + "/auth/callback", Claim: "groups",
		Allowed: []string{"Pulumi Viewers"}, SessionAgeMax: 8 * time.Hour,
	}, codec, app.rec, zerolog.Nop(), app.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	a.Routes(mux)
	mux.Handle("/", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := SessionFrom(r.Context())
		if !ok {
			t.Error("Require passed a request without a session")
		}
		_, _ = fmt.Fprintf(w, "ok %s", s.Subject)
	})))

	app.client = srv.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	app.client.Jar = jar
	return app
}

// noRedirect returns a client that shares the cookie jar but stops at the first response.
func (a *testApp) noRedirect() *http.Client {
	c := *a.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (a *testApp) get(t *testing.T, c *http.Client, path string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return do(t, c, req)
}

func (a *testApp) post(t *testing.T, c *http.Client, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, c, req)
}

func do(t *testing.T, c *http.Client, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := c.Do(req)
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

func (a *testApp) sessionCookie(t *testing.T) *http.Cookie {
	t.Helper()
	u, err := url.Parse(a.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range a.client.Jar.Cookies(u) {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func (a *testApp) login(t *testing.T) {
	t.Helper()
	resp, body := a.get(t, a.client, "/")
	if resp.StatusCode != http.StatusOK || body != "ok user-1" {
		t.Fatalf("login: %d %q", resp.StatusCode, body)
	}
}

var _viewer = map[string]any{"groups": []string{"x", "Pulumi Viewers"}, "email": "u@example.com"}

func TestLoginAllowed(t *testing.T) {
	a := newApp(t, _viewer, nil)
	resp, body := a.get(t, a.client, "/stacks/a/b?before=1.2")
	if resp.StatusCode != http.StatusOK || body != "ok user-1" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if got := resp.Request.URL.RequestURI(); got != "/stacks/a/b?before=1.2" {
		t.Fatalf("landed on %s", got)
	}
	events := a.rec.all()
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	want := store.AuthEvent{At: events[0].At, Subject: "user-1", Email: "u@example.com",
		Outcome: store.AuthOutcomeLogin, ClaimValues: []string{"x", "Pulumi Viewers"}}
	if diff := cmp.Diff(want, events[0]); diff != "" {
		t.Errorf("event (-want +got):\n%s", diff)
	}
}

func TestLoginDenied(t *testing.T) {
	a := newApp(t, map[string]any{"groups": []string{"other"}}, nil)
	resp, body := a.get(t, a.client, "/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "not allowed") {
		t.Fatalf("body = %q", body)
	}
	if a.sessionCookie(t) != nil {
		t.Fatal("session cookie set for a denied user")
	}
	events := a.rec.all()
	if len(events) != 1 || events[0].Outcome != store.AuthOutcomeDenied {
		t.Fatalf("events = %+v", events)
	}
}

func TestClaimFromUserInfo(t *testing.T) {
	a := newApp(t, nil, map[string]any{"groups": []any{"Pulumi Viewers"}})
	a.login(t)
}

func TestStringClaim(t *testing.T) {
	a := newApp(t, map[string]any{"groups": "Pulumi Viewers"}, nil)
	a.login(t)
}

func TestStateMismatch(t *testing.T) {
	a := newApp(t, _viewer, nil)
	nr := a.noRedirect()
	if resp, _ := a.get(t, nr, "/auth/login"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	resp, _ := a.get(t, nr, "/auth/callback?code=x&state=wrong")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertErrorEvent(t, a, "state")
}

func TestCallbackWithoutFlowCookie(t *testing.T) {
	a := newApp(t, _viewer, nil)
	resp, _ := a.get(t, a.noRedirect(), "/auth/callback?code=x&state=y")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertErrorEvent(t, a, "flow")
}

func TestNonceMismatch(t *testing.T) {
	a := newApp(t, _viewer, nil)
	a.idp.SetNonceOverride("bad")
	resp, _ := a.get(t, a.client, "/")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertErrorEvent(t, a, "nonce")
}

func TestIdPErrorParameter(t *testing.T) {
	a := newApp(t, _viewer, nil)
	nr := a.noRedirect()
	a.get(t, nr, "/auth/login")
	resp, body := a.get(t, nr, "/auth/callback?error=access_denied&state=x")
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(body, "access_denied<") {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
	assertErrorEvent(t, a, "idp")
}

func TestOpenRedirectBlocked(t *testing.T) {
	a := newApp(t, _viewer, nil)
	resp, _ := a.get(t, a.client, "/auth/login?return=//evil.example")
	if resp.Request.URL.Host != mustHost(t, a.srv.URL) || resp.Request.URL.Path != "/" {
		t.Fatalf("landed on %s", resp.Request.URL)
	}
}

func TestSessionExpiry(t *testing.T) {
	a := newApp(t, _viewer, nil)
	a.login(t)
	a.clock.advance(8*time.Hour + time.Second)
	resp, _ := a.get(t, a.noRedirect(), "/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/auth/login?return=%2F" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestTamperedSessionRejected(t *testing.T) {
	a := newApp(t, _viewer, nil)
	u, err := url.Parse(a.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	a.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookieName, Value: "abc.def",
		Path: "/", Secure: true}})
	resp, _ := a.get(t, a.noRedirect(), "/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestUnauthenticatedResponses(t *testing.T) {
	a := newApp(t, _viewer, nil)
	nr := a.noRedirect()
	resp, _ := a.get(t, nr, "/stacks/a/b", "HX-Request", "true")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("htmx status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("HX-Redirect"); got != "/auth/login?return=%2Fstacks%2Fa%2Fb" {
		t.Fatalf("HX-Redirect = %q", got)
	}
	if resp, _ := a.get(t, nr, "/events"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/events status = %d", resp.StatusCode)
	}
	if resp, _ := a.post(t, nr, "/anything"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
}

func TestLogout(t *testing.T) {
	a := newApp(t, _viewer, nil)
	a.login(t)
	resp, _ := a.post(t, a.noRedirect(), "/auth/logout")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("logout = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if a.sessionCookie(t) != nil {
		t.Fatal("session cookie still present")
	}
	if resp, _ := a.get(t, a.noRedirect(), "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after logout status = %d", resp.StatusCode)
	}
}

func TestCookieAttributes(t *testing.T) {
	a := newApp(t, _viewer, nil)
	nr := a.noRedirect()
	resp, _ := a.get(t, nr, "/auth/login")
	flow := setCookie(t, resp, flowCookieName)
	for _, attr := range []string{"Path=/", "HttpOnly", "Secure", "SameSite=Lax", "Max-Age=600"} {
		if !strings.Contains(flow, attr) {
			t.Errorf("flow cookie %q lacks %s", flow, attr)
		}
	}
	idpResp, _ := do(t, nr, mustRequest(t, resp.Header.Get("Location")))
	cb, _ := do(t, nr, mustRequest(t, idpResp.Header.Get("Location")))
	sess := setCookie(t, cb, sessionCookieName)
	for _, attr := range []string{"Path=/", "HttpOnly", "Secure", "SameSite=Lax", "Max-Age=28800"} {
		if !strings.Contains(sess, attr) {
			t.Errorf("session cookie %q lacks %s", sess, attr)
		}
	}
	if strings.Contains(strings.ToLower(sess), "domain=") {
		t.Errorf("session cookie %q sets Domain", sess)
	}
}

func assertErrorEvent(t *testing.T, a *testApp, detail string) {
	t.Helper()
	for _, e := range a.rec.all() {
		if e.Outcome == store.AuthOutcomeError && strings.Contains(e.Detail, detail) {
			return
		}
	}
	t.Fatalf("no error event with %q in %+v", detail, a.rec.all())
}

func setCookie(t *testing.T, resp *http.Response, name string) string {
	t.Helper()
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, name+"=") && !strings.Contains(h, "Max-Age=0") {
			return h
		}
	}
	t.Fatalf("no Set-Cookie %s in %v", name, resp.Header.Values("Set-Cookie"))
	return ""
}

func mustRequest(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
