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
	authn  *Authenticator
}

// cfgMaxAge changes the running Authenticator's session limit, as a restart with a lower
// --session.max-age would.
func (a *testApp) cfgMaxAge(d time.Duration) { a.authn.cfg.SessionAgeMax = d }

func newApp(t *testing.T, claims, userinfo map[string]any,
	opts ...func(*oidctest.Options)) *testApp {
	t.Helper()
	return newAppWith(t, claims, userinfo, func(*Config) {}, opts...)
}

// newAppWith is newApp with a hook to change the Authenticator's Config.
func newAppWith(t *testing.T, claims, userinfo map[string]any, cfgOpt func(*Config),
	opts ...func(*oidctest.Options)) *testApp {
	t.Helper()
	o := oidctest.Options{ClientID: "pou", ClientSecret: "secret", Claims: claims,
		UserInfo: userinfo}
	for _, opt := range opts {
		opt(&o)
	}
	idp, err := oidctest.NewServer(o)
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
	cfg := Config{
		Issuer: idp.URL, ClientID: "pou", ClientSecret: "secret",
		RedirectURL: srv.URL + "/auth/callback", Claim: "groups",
		Allowed: []string{"Pulumi Viewers"}, SessionAgeMax: 8 * time.Hour,
	}
	cfgOpt(&cfg)
	a, err := New(t.Context(), cfg, codec, app.rec, zerolog.Nop(), app.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	app.authn = a
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
	login, _ := a.get(t, nr, "/auth/login")
	authz, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// An IdP that refuses echoes the login's state with the error.
	resp, body := a.get(t, nr, "/auth/callback?error=access_denied&state="+authz.Query().Get("state"))
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
	flow := setCookieWithPrefix(t, resp, flowCookieName+"_") // one per login, named by state
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

// setCookieWithPrefix returns the first live Set-Cookie whose name starts with prefix.
func setCookieWithPrefix(t *testing.T, resp *http.Response, prefix string) string {
	t.Helper()
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, prefix) && !strings.Contains(h, "Max-Age=0") {
			return h
		}
	}
	t.Fatalf("no Set-Cookie %s* in %v", prefix, resp.Header.Values("Set-Cookie"))
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

// OIDC Core 1.0 section 5.3.2: the userinfo sub must equal the ID token sub, or the userinfo
// response must not be used. Here only userinfo carries the allowed group.
func TestUserInfoSubjectMismatch(t *testing.T) {
	a := newApp(t, nil, map[string]any{"sub": "someone-else", "groups": []any{"Pulumi Viewers"}})
	resp, body := a.get(t, a.client, "/")
	if resp.StatusCode == http.StatusOK || strings.HasPrefix(body, "ok ") {
		t.Fatalf("signed in with a mismatched userinfo subject: %d %q", resp.StatusCode, body)
	}
	assertErrorEvent(t, a, "userinfo subject")
}

// An expired session noticed by an htmx fragment request must return the user to the page
// they were on, never to the fragment URL, which renders as a bare snippet.
func TestHtmxLoginReturnsToPage(t *testing.T) {
	a := newApp(t, _viewer, nil)
	nr := a.noRedirect()
	resp, _ := a.get(t, nr, "/fragments/runs/7/header", "HX-Request", "true",
		"HX-Current-URL", a.srv.URL+"/runs/7?x=1")
	if got := resp.Header.Get("HX-Redirect"); got != "/auth/login?return=%2Fruns%2F7%3Fx%3D1" {
		t.Errorf("with HX-Current-URL: HX-Redirect = %q", got)
	}
	resp, _ = a.get(t, nr, "/fragments/runs/7/header", "HX-Request", "true")
	if got := resp.Header.Get("HX-Redirect"); got != "/auth/login?return=%2F" {
		t.Errorf("fragment without HX-Current-URL: HX-Redirect = %q", got)
	}
}

func TestLogoutRejectsCrossSite(t *testing.T) {
	a := newApp(t, _viewer, nil)
	a.login(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.srv.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ := do(t, a.noRedirect(), req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403 for a cross-site logout", resp.StatusCode)
	}
	if _, body := a.get(t, a.client, "/"); !strings.HasPrefix(body, "ok ") {
		t.Fatal("cross-site logout ended the session")
	}
}

func TestLogoutAtIdP(t *testing.T) {
	a := newApp(t, _viewer, nil, func(o *oidctest.Options) { o.EndSession = true })
	a.login(t)
	resp, body := a.post(t, a.noRedirect(), "/auth/logout")
	want := a.idp.URL + "/logout?client_id=pou&amp;post_logout_redirect_uri=" +
		url.QueryEscape(a.srv.URL+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `http-equiv="refresh"`) ||
		!strings.Contains(body, want) {
		t.Fatalf("status %d body %s, want a refresh to %s", resp.StatusCode, body, want)
	}
	if a.sessionCookie(t) != nil {
		t.Error("session cookie not cleared")
	}
}

// Two tabs can sign in at the same time: each login has its own flow cookie.
func TestParallelLogins(t *testing.T) {
	a := newApp(t, _viewer, nil)
	c := a.noRedirect()
	start := func() string {
		resp, _ := do(t, c, mustRequest(t, a.srv.URL+"/auth/login"))
		return resp.Header.Get("Location") // the IdP's authorize URL
	}
	first, second := start(), start()
	for _, authz := range []string{second, first} {
		resp, _ := do(t, c, mustRequest(t, authz))
		resp, _ = do(t, c, mustRequest(t, resp.Header.Get("Location"))) // our callback
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("callback status %d location %q, want 303 to /", resp.StatusCode,
				resp.Header.Get("Location"))
		}
	}
}

func TestCallbackBadState(t *testing.T) {
	a := newApp(t, _viewer, nil)
	for _, state := range []string{"", "x", "../../etc", strings.Repeat("a", 200)} {
		resp, _ := do(t, a.noRedirect(), mustRequest(t,
			a.srv.URL+"/auth/callback?code=c&state="+url.QueryEscape(state)))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("state %q: status %d, want 400", state, resp.StatusCode)
		}
	}
}

// An operator whose IdP does not have the app root registered as a post-logout redirect URI
// can keep logout local.
func TestLocalLogoutSkipsIdP(t *testing.T) {
	a := newAppWith(t, _viewer, nil, func(c *Config) { c.LocalLogout = true },
		func(o *oidctest.Options) { o.EndSession = true })
	a.login(t)
	resp, _ := a.post(t, a.noRedirect(), "/auth/logout")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("status %d location %q, want a local 303 to /", resp.StatusCode,
			resp.Header.Get("Location"))
	}
}

// An allowlist of email addresses only means something for addresses the IdP verified.
func TestEmailClaimNeedsVerifiedEmail(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claims   map[string]any
		wantCode int
	}{
		{"unverified", map[string]any{"email": "boss@corp.example", "email_verified": false}, http.StatusForbidden},
		{"no flag", map[string]any{"email": "boss@corp.example"}, http.StatusForbidden},
		{"verified", map[string]any{"email": "boss@corp.example", "email_verified": true}, http.StatusOK},
	} {
		a := newAppWith(t, tc.claims, nil, func(c *Config) {
			c.Claim, c.Allowed = "email", []string{"boss@corp.example"}
		})
		if resp, _ := a.get(t, a.client, "/"); resp.StatusCode != tc.wantCode {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.wantCode)
		}
	}
}

// A session issued before --session.max-age was lowered must not outlive the new limit.
func TestSessionHonoursCurrentMaxAge(t *testing.T) {
	a := newApp(t, _viewer, nil)
	a.login(t)
	a.clock.advance(2 * time.Hour)
	a.cfgMaxAge(time.Hour)
	if resp, _ := a.get(t, a.noRedirect(), "/"); resp.StatusCode == http.StatusOK {
		t.Fatal("a 2h-old session passed a 1h max age")
	}
}
