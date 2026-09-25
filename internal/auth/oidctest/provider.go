// Package oidctest is a minimal OIDC provider for tests and local development. It
// auto-approves every login, enforces PKCE (S256) and single-use codes, and signs RS256
// ID tokens. Never use it outside tests or a developer's machine.
package oidctest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const _keyID = "oidctest"

// Options configures the provider.
type Options struct {
	Addr         string // listen address; "" = 127.0.0.1:0
	ClientID     string
	ClientSecret string         `json:"-"`
	Claims       map[string]any // merged into every ID token; "sub" defaults to "user-1"
	UserInfo     map[string]any // served at /userinfo
	RedirectURIs []string       // empty = any
}

type grant struct {
	nonce, challenge, redirectURI string
}

// Provider is a running stub IdP.
type Provider struct {
	URL string

	opts   Options
	key    *rsa.PrivateKey
	signer jose.Signer
	srv    *http.Server
	ln     net.Listener
	done   chan struct{}

	mu            sync.Mutex
	codes         map[string]grant
	tokens        map[string]struct{}
	nonceOverride string
}

// NewServer starts a provider; Close stops it.
func NewServer(o Options) (*Provider, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", _keyID))
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	p := &Provider{
		URL: "http://" + ln.Addr().String(), opts: o, key: key, signer: signer, ln: ln,
		done: make(chan struct{}), codes: map[string]grant{}, tokens: map[string]struct{}{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	p.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { // ends when Close shuts the server down
		defer close(p.done)
		_ = p.srv.Serve(ln) // returns http.ErrServerClosed on Close
	}()
	return p, nil
}

// Close stops the provider and waits for it to exit.
func (p *Provider) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.srv.Shutdown(ctx) // best effort in tests
	<-p.done
}

// SetNonceOverride makes subsequent ID tokens carry nonce n instead of the requested one.
func (p *Provider) SetNonceOverride(n string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nonceOverride = n
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"jwks_uri":                              p.URL + "/jwks",
		"userinfo_endpoint":                     p.URL + "/userinfo",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &p.key.PublicKey, KeyID: _keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}})
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	switch {
	case q.Get("client_id") != p.opts.ClientID:
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	case len(p.opts.RedirectURIs) > 0 && !slices.Contains(p.opts.RedirectURIs, redirect):
		http.Error(w, "redirect_uri not allowed", http.StatusBadRequest)
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		http.Error(w, "PKCE S256 required", http.StatusBadRequest)
		return
	}
	code := randomString()
	p.mu.Lock()
	p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirectURI: redirect}
	p.mu.Unlock()
	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if err := p.checkClient(r); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, ok := p.codes[code]
	delete(p.codes, code) // single use
	nonce := g.nonce
	if p.nonceOverride != "" {
		nonce = p.nonceOverride
	}
	p.mu.Unlock()
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" ||
		r.PostForm.Get("redirect_uri") != g.redirectURI {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		http.Error(w, `{"error":"invalid_grant","error_description":"pkce"}`,
			http.StatusBadRequest)
		return
	}
	idToken, err := p.idToken(nonce)
	if err != nil {
		http.Error(w, "sign", http.StatusInternalServerError)
		return
	}
	access := randomString()
	p.mu.Lock()
	p.tokens[access] = struct{}{}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer",
		"expires_in": 300, "id_token": idToken})
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearer(r)
	p.mu.Lock()
	_, known := p.tokens[tok]
	p.mu.Unlock()
	if !ok || !known {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	info := map[string]any{"sub": p.subject()}
	maps.Copy(info, p.opts.UserInfo)
	writeJSON(w, info)
}

func (p *Provider) checkClient(r *http.Request) error {
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.opts.ClientID ||
		subtle.ConstantTimeCompare([]byte(secret), []byte(p.opts.ClientSecret)) != 1 {
		return errors.New("bad client credentials")
	}
	return nil
}

func (p *Provider) idToken(nonce string) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"iss": p.URL, "sub": p.subject(), "aud": p.opts.ClientID,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": nonce,
	}
	maps.Copy(claims, p.opts.Claims)
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sig, err := p.signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}

func (p *Provider) subject() string {
	if s, ok := p.opts.Claims["sub"].(string); ok {
		return s
	}
	return "user-1"
}

func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}
	return h[len(prefix):], true
}

func randomString() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("invariant violated: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) // client disconnects are irrelevant to the stub
}
