package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/failsafehttp"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

const (
	sessionCookieName = "__Host-pou_session"
	flowCookieName    = "__Host-pou_flow"
	// A login round trip through the IdP takes seconds; ten minutes covers MFA prompts.
	flowTTL = 10 * time.Minute
	// Per-attempt bound on IdP calls; the client-wide timeout bounds retries as a whole.
	idpAttemptTimeout = 10 * time.Second
	idpClientTimeout  = 30 * time.Second
)

// Config configures the Authenticator.
type Config struct {
	Issuer        string
	ClientID      string
	ClientSecret  string `json:"-"`
	RedirectURL   string
	CAPool        *x509.CertPool // nil = system roots
	Claim         string
	Allowed       []string
	SessionAgeMax time.Duration
}

// EventRecorder persists auth audit events.
type EventRecorder interface {
	InsertAuthEvent(ctx context.Context, e store.AuthEvent) error
}

// Authenticator runs the OIDC authorization-code flow with PKCE and guards handlers.
type Authenticator struct {
	cfg      Config
	codec    *Codec
	rec      EventRecorder
	log      zerolog.Logger
	now      func() time.Time
	client   *http.Client
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config

	endSession string // the IdP's end_session_endpoint; "" = logout stays local
	postLogout string // where the IdP returns after logout: the app root
}

type flowState struct {
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	Return   string    `json:"r"`
	Expires  time.Time `json:"e"`
}

type identity struct {
	subject, email, name string
	values               []string
}

// flowError is a failed login step: detail goes to the audit log, never to the browser.
type flowError struct {
	detail string
	status int
}

func (e *flowError) Error() string { return e.detail }

type sessionKey struct{}

// New discovers the issuer and returns a ready Authenticator.
func New(ctx context.Context, cfg Config, codec *Codec, rec EventRecorder, log zerolog.Logger,
	now func() time.Time) (*Authenticator, error) {
	if codec == nil || rec == nil || now == nil || len(cfg.Allowed) == 0 {
		panic("invariant violated: auth.New needs codec, recorder, clock and an allowlist")
	}
	a := &Authenticator{cfg: cfg, codec: codec, rec: rec, log: log, now: now,
		client: newIDPClient(cfg.CAPool)}
	// The provider keeps this context for later JWKS refreshes, so it must carry the
	// hardened client and live as long as the process.
	provider, err := oidc.NewProvider(a.idpContext(ctx), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	scopes := []string{oidc.ScopeOpenID, "profile", "email"}
	if cfg.Claim == "groups" {
		scopes = append(scopes, "groups")
	}
	a.provider = provider
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	a.endSession = meta.EndSession
	ru, err := url.Parse(cfg.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("oidc redirect url: %w", err)
	}
	a.postLogout = (&url.URL{Scheme: ru.Scheme, Host: ru.Host, Path: "/"}).String()
	a.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID, Now: now})
	a.oauth = oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		RedirectURL: cfg.RedirectURL, Endpoint: provider.Endpoint(), Scopes: scopes}
	return a, nil
}

// Routes registers GET /auth/login, GET /auth/callback and POST /auth/logout. Logout is
// guarded against cross-origin requests (Sec-Fetch-Site and Origin), so another site cannot
// sign a viewer out.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.Handle("POST /auth/logout",
		http.NewCrossOriginProtection().Handler(http.HandlerFunc(a.logout)))
}

// Require passes requests with a valid session to next. Without one: htmx requests get
// 401 plus HX-Redirect, /events and non-GET requests get 401, and page GETs are sent to
// the login.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, ok := a.session(r); ok {
			next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), s)))
			return
		}
		loginURL := "/auth/login?return=" + url.QueryEscape(returnTarget(r))
		switch {
		case r.Header.Get("HX-Request") == "true":
			w.Header().Set("HX-Redirect", loginURL)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/events" || r.Method != http.MethodGet:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.Redirect(w, r, loginURL, http.StatusSeeOther)
		}
	})
}

// returnTarget is where the login sends the user back to. An htmx request is for a fragment,
// so it uses the page htmx reports in HX-Current-URL (path and query only; safeReturn checks
// it at login), and a fragment path without that header falls back to "/".
func returnTarget(r *http.Request) string {
	if r.Header.Get("HX-Request") == "true" {
		if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && u.Path != "" {
			return (&url.URL{Path: u.Path, RawQuery: u.RawQuery}).RequestURI()
		}
	}
	if strings.HasPrefix(r.URL.Path, "/fragments/") {
		return "/"
	}
	return r.URL.RequestURI()
}

// WithSession returns ctx carrying s. Require uses it; tests and middleware may too.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom returns the session Require attached to ctx.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	f := flowState{State: randomToken(), Nonce: randomToken(),
		Verifier: oauth2.GenerateVerifier(), Return: safeReturn(r.URL.Query().Get("return")),
		Expires: a.now().Add(flowTTL)}
	b, err := json.Marshal(f)
	if err != nil {
		panic("invariant violated: marshal flow state: " + err.Error())
	}
	a.setCookie(w, flowCookieName, a.codec.Seal("flow", b), flowTTL)
	http.Redirect(w, r, a.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce),
		oauth2.S256ChallengeOption(f.Verifier)), http.StatusSeeOther)
}

func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	f, err := a.readFlow(r)
	a.clearCookie(w, flowCookieName)
	if err != nil {
		a.fail(w, r, &flowError{detail: "flow: " + err.Error(), status: http.StatusBadRequest})
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		a.fail(w, r, &flowError{detail: "idp returned an error", status: http.StatusBadRequest})
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		a.fail(w, r, &flowError{detail: "state mismatch", status: http.StatusBadRequest})
		return
	}
	id, ferr := a.exchange(a.idpContext(r.Context()), q.Get("code"), f)
	if ferr != nil {
		a.fail(w, r, ferr)
		return
	}
	now := a.now()
	if !Allowed(id.values, a.cfg.Allowed) {
		a.record(r.Context(), store.AuthEvent{At: now, Subject: id.subject, Email: id.email,
			Outcome: store.AuthOutcomeDenied, ClaimValues: id.values})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, "You are signed in, but your account is not allowed to use "+
			"this application. Ask an administrator for access.") // client gone: nothing to do
		return
	}
	s := Session{Subject: id.subject, Email: id.email, Name: id.name, IssuedAt: now,
		ExpiresAt: now.Add(a.cfg.SessionAgeMax)}
	b, err := json.Marshal(s)
	if err != nil {
		panic("invariant violated: marshal session: " + err.Error())
	}
	a.setCookie(w, sessionCookieName, a.codec.Seal("session", b), a.cfg.SessionAgeMax)
	a.record(r.Context(), store.AuthEvent{At: now, Subject: id.subject, Email: id.email,
		Outcome: store.AuthOutcomeLogin, ClaimValues: id.values})
	http.Redirect(w, r, f.Return, http.StatusSeeOther)
}

// exchange redeems the code, verifies the ID token and collects the allowlist claim,
// falling back to userinfo when the ID token does not carry it.
func (a *Authenticator) exchange(ctx context.Context, code string, f flowState) (
	identity, *flowError) {
	tok, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return identity{}, &flowError{detail: "token exchange rejected",
				status: http.StatusBadRequest}
		}
		return identity{}, &flowError{detail: "token exchange failed", status: http.StatusBadGateway}
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return identity{}, &flowError{detail: "missing id_token", status: http.StatusBadGateway}
	}
	idt, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return identity{}, &flowError{detail: "id token invalid", status: http.StatusBadRequest}
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		return identity{}, &flowError{detail: "nonce mismatch", status: http.StatusBadRequest}
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return identity{}, &flowError{detail: "id token claims", status: http.StatusBadRequest}
	}
	id := identity{subject: idt.Subject, values: ClaimValues(claims, a.cfg.Claim)}
	id.email, _ = claims["email"].(string)
	id.name, _ = claims["name"].(string)
	if len(id.values) == 0 {
		values, ferr := a.userInfoValues(ctx, tok, idt.Subject)
		if ferr != nil {
			return identity{}, ferr
		}
		id.values = values
	}
	return id, nil
}

// userInfoValues reads the allowlist claim from userinfo. A userinfo failure is an error,
// not a denial, and a response for another subject is rejected (OIDC Core 1.0 5.3.2).
func (a *Authenticator) userInfoValues(ctx context.Context, tok *oauth2.Token,
	subject string) ([]string, *flowError) {
	ui, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, &flowError{detail: "userinfo failed", status: http.StatusBadGateway}
	}
	if subtle.ConstantTimeCompare([]byte(ui.Subject), []byte(subject)) != 1 {
		return nil, &flowError{detail: "userinfo subject mismatch", status: http.StatusBadGateway}
	}
	var claims map[string]any
	if err := ui.Claims(&claims); err != nil {
		return nil, &flowError{detail: "userinfo claims", status: http.StatusBadGateway}
	}
	return ClaimValues(claims, a.cfg.Claim), nil
}

// logout ends the app session and, when the IdP supports RP-initiated logout, its session
// too. The IdP hop is a meta refresh rather than a redirect: CSP form-action 'self' would
// block a redirect off-site after the form post.
func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookieName)
	if a.endSession == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	sep := "?"
	if strings.Contains(a.endSession, "?") {
		sep = "&"
	}
	target := a.endSession + sep + url.Values{"client_id": {a.oauth.ClientID},
		"post_logout_redirect_uri": {a.postLogout}}.Encode()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := _logoutPage.Execute(w, target); err != nil {
		a.log.Warn().Err(err).Msg("auth: render logout page")
	}
}

// _logoutPage hands the browser to the IdP's end session endpoint.
var _logoutPage = template.Must(template.New("logout").Parse(`<!doctype html>` +
	`<meta http-equiv="refresh" content="0;url={{.}}"><title>Signing out</title>` +
	`<p><a href="{{.}}">Continue signing out</a></p>`))

func (a *Authenticator) session(r *http.Request) (Session, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Session{}, false
	}
	b, err := a.codec.Open("session", c.Value)
	if err != nil {
		return Session{}, false
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil || s.Subject == "" {
		return Session{}, false
	}
	return s, a.now().Before(s.ExpiresAt)
}

func (a *Authenticator) readFlow(r *http.Request) (flowState, error) {
	c, err := r.Cookie(flowCookieName)
	if err != nil {
		return flowState{}, errors.New("missing cookie")
	}
	b, err := a.codec.Open("flow", c.Value)
	if err != nil {
		return flowState{}, errors.New("invalid cookie")
	}
	var f flowState
	if err := json.Unmarshal(b, &f); err != nil {
		return flowState{}, errors.New("invalid cookie")
	}
	if !a.now().Before(f.Expires) {
		return flowState{}, errors.New("expired")
	}
	return f, nil
}

func (a *Authenticator) fail(w http.ResponseWriter, r *http.Request, e *flowError) {
	a.record(r.Context(), store.AuthEvent{At: a.now(), Outcome: store.AuthOutcomeError,
		Detail: e.detail})
	http.Error(w, "Sign-in failed. Please try again.", e.status)
}

func (a *Authenticator) record(ctx context.Context, e store.AuthEvent) {
	// Subject only: email and names are personal data and stay out of process logs.
	a.log.Info().Str("outcome", string(e.Outcome)).Str("subject", e.Subject).
		Str("detail", e.Detail).Msg("auth event")
	if err := a.rec.InsertAuthEvent(ctx, e); err != nil {
		a.log.Error().Err(err).Msg("auth: record event")
	}
}

func (a *Authenticator) setCookie(w http.ResponseWriter, name, value string, age time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/",
		MaxAge: int(age.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// idpContext makes both go-oidc and x/oauth2 use the hardened client.
func (a *Authenticator) idpContext(ctx context.Context) context.Context {
	return oidc.ClientContext(context.WithValue(ctx, oauth2.HTTPClient, a.client), a.client)
}

// newIDPClient retries idempotent GETs (discovery, JWKS, userinfo) but never the token
// POST, whose authorization code is single-use. Both share one circuit breaker.
//
//nolint:bodyclose // *http.Response is only a failsafe type parameter here; no body exists
func newIDPClient(roots *x509.CertPool) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("invariant violated: http.DefaultTransport is not *http.Transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	breaker := circuitbreaker.NewBuilder[*http.Response]().
		HandleIf(func(resp *http.Response, err error) bool {
			return err != nil || (resp != nil && resp.StatusCode >= http.StatusInternalServerError)
		}).
		WithFailureThreshold(5).
		WithDelay(30 * time.Second).
		Build()
	attempt := timeout.New[*http.Response](idpAttemptTimeout)
	retry := failsafehttp.NewRetryPolicyBuilder().
		WithMaxAttempts(3).
		WithBackoff(200*time.Millisecond, 2*time.Second).
		Build()
	return &http.Client{
		Timeout: idpClientTimeout,
		Transport: methodRoundTripper{
			get:   failsafehttp.NewRoundTripper(tr, retry, breaker, attempt),
			other: failsafehttp.NewRoundTripper(tr, breaker, attempt),
		},
	}
}

type methodRoundTripper struct {
	get, other http.RoundTripper
}

func (m methodRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		return m.get.RoundTrip(req)
	}
	return m.other.RoundTrip(req)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("invariant violated: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
