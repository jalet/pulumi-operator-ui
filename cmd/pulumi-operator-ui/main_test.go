package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/jalet/pulumi-operator-ui/internal/auth/oidctest"
	"github.com/jalet/pulumi-operator-ui/internal/record"
)

func TestRunConfigError(t *testing.T) {
	err := run(t.Context(), nil, func(string) string { return "" }, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "--database-url") {
		t.Fatalf("err = %v, want a --database-url error", err)
	}
}

func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	restCfg, k8s := startEnvtest(t)
	dbURL := startPostgres(t)
	httpAddr, metricsAddr := freeAddr(t), freeAddr(t)
	base := "http://" + httpAddr

	idp, err := oidctest.NewServer(oidctest.Options{ClientID: "pou", ClientSecret: "secret",
		Claims: map[string]any{"groups": []string{"viewers"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idp.Close)

	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	args := []string{
		"--http-addr=" + httpAddr, "--metrics-addr=" + metricsAddr,
		"--database-url=" + dbURL,
		"--oidc.issuer=" + idp.URL, "--oidc.client-id=pou",
		"--oidc.client-secret-file=" + write("client-secret", "secret\n"),
		"--oidc.redirect-url=" + base + "/auth/callback",
		"--auth.allowed=viewers",
		"--session.key-file=" + write("key", strings.Repeat("k", 48)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1) // single result from run
	noAWS := func(context.Context) (aws.Config, error) {
		t.Error("AWS config loaded while S3 history is off")
		return aws.Config{}, nil
	}
	go func() {
		done <- run(ctx, args, func(string) string { return "" },
			func() (*rest.Config, error) { return restCfg, nil }, noAWS)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("run did not stop within 20s")
		}
	})

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	eventually(t, "readyz", func() bool {
		return statusOf(t, c, base+"/readyz") == http.StatusOK
	})

	createStack(t, k8s)
	session := login(t, c, base)
	eventually(t, "stack on the list page", func() bool {
		return strings.Contains(getBody(t, c, base+"/", session), "e2e-app")
	})

	metrics := getBody(t, c, "http://"+metricsAddr+"/metrics", nil)
	if !strings.Contains(metrics, "pou_sse_clients") {
		t.Error("metrics listener lacks pou_sse_clients")
	}
	if got := statusOf(t, c, base+"/metrics"); got != http.StatusNotFound {
		t.Errorf("/metrics on the UI listener = %d, want 404", got)
	}
}

// login drives the OIDC flow by hand. The app runs plain HTTP on loopback, and Go's
// cookie jar will not store the Secure cookies, so they are carried manually.
func login(t *testing.T, c *http.Client, base string) *http.Cookie {
	t.Helper()
	resp := mustGet(t, c, base+"/auth/login", nil)
	flow := cookieNamed(t, resp, "__Host-pou_flow_") // one per login, suffixed by its state
	atIDP := mustGet(t, c, resp.Header.Get("Location"), nil)
	callback := mustGet(t, c, atIDP.Header.Get("Location"), flow)
	if callback.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback status = %d", callback.StatusCode)
	}
	return cookieNamed(t, callback, "__Host-pou_session")
}

func mustGet(t *testing.T, c *http.Client, u string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func getBody(t *testing.T, c *http.Client, u string, cookie *http.Cookie) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cookieNamed(t *testing.T, resp *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		// A name ending in "_" matches by prefix: the flow cookie carries its login's state.
		match := c.Name == name || (strings.HasSuffix(name, "_") && strings.HasPrefix(c.Name, name))
		if match && c.MaxAge >= 0 && c.Value != "" {
			return &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	t.Fatalf("no cookie %s (status %d)", name, resp.StatusCode)
	return nil
}

func startEnvtest(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../test/crds/pko-2.9.1"},
		ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest (run via mise run test): %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, c
}

func startPostgres(t *testing.T) string {
	t.Helper()
	ctr, err := postgres.Run(t.Context(), "postgres:17-alpine", postgres.WithDatabase("pou"),
		postgres.WithUsername("pou"), postgres.WithPassword("pou"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Errorf("terminate postgres: %v", err)
		}
	})
	u, err := ctr.ConnectionString(t.Context(), "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func createStack(t *testing.T, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e"}}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	st := &unstructured.Unstructured{}
	st.SetGroupVersionKind(record.StackGVK)
	st.SetNamespace("e2e")
	st.SetName("e2e-app")
	st.Object["spec"] = map[string]any{"stack": "dev"}
	if err := c.Create(t.Context(), st); err != nil {
		t.Fatal(err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// statusOf returns the status of a GET, or 0 when the server is not reachable yet.
func statusOf(t *testing.T, c *http.Client, u string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestNewS3ClientsCachesPerRegion(t *testing.T) {
	clients := newS3Clients(aws.Config{Region: "eu-north-1"})
	a, err := clients("eu-north-1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := clients("eu-north-1")
	c, _ := clients("eu-west-1")
	d, _ := clients("")
	if a != b {
		t.Error("same region returned different clients")
	}
	if a == c {
		t.Error("different regions share a client")
	}
	if d == nil {
		t.Error("default region client is nil")
	}
}
