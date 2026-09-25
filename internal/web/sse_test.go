package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/auth"
	"github.com/jalet/pulumi-operator-ui/internal/events"
)

func newSSEServer(t *testing.T, heartbeat, sessionLeft time.Duration) (*httptest.Server,
	*events.Broker) {
	t.Helper()
	b := events.NewBroker()
	withSession := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s := auth.Session{Subject: "u", ExpiresAt: time.Now().Add(sessionLeft)}
			next.ServeHTTP(w, r.WithContext(auth.WithSession(r.Context(), s)))
		})
	}
	h := New(Deps{Store: sampleReader(), Broker: b, RequireAuth: withSession,
		AuthRoutes: func(*http.ServeMux) {}, Log: zerolog.Nop(), Now: time.Now,
		heartbeat: heartbeat})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, b
}

type stream struct {
	resp   *http.Response
	lines  chan string // closed at EOF
	cancel context.CancelFunc
}

func openStream(t *testing.T, srv *httptest.Server) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{resp: resp, lines: make(chan string, 64), cancel: cancel}
	go func() { // ends at EOF or when cancel closes the body
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				s.lines <- line
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	return s
}

// next returns the next line that is not "data:", failing after timeout.
func (s *stream) next(t *testing.T, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				return "", false
			}
			if strings.HasPrefix(line, "data:") {
				continue
			}
			return line, true
		case <-deadline:
			t.Fatal("timed out reading the stream")
		}
	}
}

func TestSSEResyncFirstThenEvents(t *testing.T) {
	srv, b := newSSEServer(t, time.Hour, time.Hour)
	s := openStream(t, srv)
	if line, _ := s.next(t, time.Second); line != "event: resync" {
		t.Fatalf("first line = %q", line)
	}
	b.Publish(events.Event{Kind: events.KindRun, RunID: 3})
	if line, _ := s.next(t, time.Second); line != "event: run-3" {
		t.Fatalf("line = %q", line)
	}
	b.Publish(events.Event{Kind: events.KindStack, Namespace: "ns", Stack: "app"})
	if line, _ := s.next(t, time.Second); line != "event: "+events.StackEventName("ns", "app") {
		t.Fatalf("line = %q", line)
	}
}

func TestSSEHeaders(t *testing.T) {
	srv, _ := newSSEServer(t, time.Hour, time.Hour)
	s := openStream(t, srv)
	for k, want := range map[string]string{"Content-Type": "text/event-stream",
		"Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := s.resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestSSEHeartbeat(t *testing.T) {
	srv, _ := newSSEServer(t, 20*time.Millisecond, time.Hour)
	s := openStream(t, srv)
	s.next(t, time.Second) // resync
	if line, _ := s.next(t, time.Second); line != ": ping" {
		t.Fatalf("line = %q, want heartbeat", line)
	}
}

func TestSSEClosesAtSessionExpiry(t *testing.T) {
	srv, _ := newSSEServer(t, time.Hour, 100*time.Millisecond)
	s := openStream(t, srv)
	s.next(t, time.Second) // resync
	if line, _ := s.next(t, 2*time.Second); line != "event: session-expired" {
		t.Fatalf("line = %q, want session-expired before close", line)
	}
	if line, ok := s.next(t, 2*time.Second); ok {
		t.Fatalf("stream still open, got %q", line)
	}
}

func TestSSEBusy(t *testing.T) {
	srv, b := newSSEServer(t, time.Hour, time.Hour)
	for {
		if _, err := b.Subscribe(t.Context()); err != nil {
			break
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestSSEUnsubscribesOnDisconnect(t *testing.T) {
	srv, b := newSSEServer(t, time.Hour, time.Hour)
	s := openStream(t, srv)
	s.next(t, time.Second)
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}
	s.cancel()
	deadline := time.Now().Add(2 * time.Second)
	for b.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Len = %d after disconnect", b.Len())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEventsRequiresAuth(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
	srv := newServer(t, sampleReader(), deny)
	if resp, _ := get(t, srv, "/events"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestSessionFragment(t *testing.T) {
	srv, _ := newSSEServer(t, time.Hour, time.Hour)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/fragments/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestLayoutHandlesSessionExpiry(t *testing.T) {
	srv := newServer(t, sampleReader(), nil)
	_, body := get(t, srv, "/")
	for _, want := range []string{`hx-get="/fragments/session"`, `hx-trigger="sse:session-expired"`} {
		if !strings.Contains(body, want) {
			t.Errorf("layout lacks %s", want)
		}
	}
}
