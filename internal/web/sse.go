package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/jalet/pulumi-operator-ui/internal/auth"
	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// 20s is below common proxy idle timeouts (many load balancers use 60s) with margin.
const sseHeartbeatIntervalDefault = 20 * time.Second

var (
	_sseClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pou_sse_clients",
		Help: "Open /events streams.",
	})
	_registerOnce sync.Once
)

const (
	// sseStreamsPerSubject bounds one viewer's streams (a tab each), so one viewer or a
	// runaway script cannot take the broker's whole budget.
	sseStreamsPerSubject = 8
	// sseWriteTimeout bounds each write, so a client that stops reading frees its goroutine.
	sseWriteTimeout = 10 * time.Second
)

// streamCounts tracks each subject's open event streams.
type streamCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *streamCounts) acquire(subject string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	if c.n[subject] >= sseStreamsPerSubject {
		return false
	}
	c.n[subject]++
	return true
}

func (c *streamCounts) release(subject string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[subject]--; c.n[subject] <= 0 {
		delete(c.n, subject)
	}
}

func registerMetrics() {
	_registerOnce.Do(func() { ctrlmetrics.Registry.MustRegister(_sseClients) })
}

// events streams change notifications as SSE. The stream is bounded: it ends when the
// session expires (announced as "session-expired" so the page can send the user to the
// login), the client leaves, or the broker drops it as slow. Browsers reconnect
// on their own and get "resync" first, so a dropped stream only costs one refetch.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	sess, ok := auth.SessionFrom(r.Context())
	if !ok {
		panic("invariant violated: /events reached without a session")
	}
	if !s.streams.acquire(sess.Subject) {
		http.Error(w, "too many streams for this user", http.StatusTooManyRequests)
		return
	}
	defer s.streams.release(sess.Subject)
	ctx, cancel := context.WithDeadline(r.Context(), sess.ExpiresAt)
	defer cancel()
	ch, err := s.broker.Subscribe(ctx)
	if errors.Is(err, events.ErrTooManySubscribers) {
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		panic("invariant violated: unexpected subscribe error: " + err.Error())
	}
	_sseClients.Inc()
	defer _sseClients.Dec()

	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn().Err(err).Msg("sse: clear write deadline")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if !sseSend(w, rc, "event: resync\ndata:\n\n") {
		return
	}
	t := time.NewTicker(s.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.Context().Err() == nil { // the session ended, not the client
				sseSend(w, rc, "event: session-expired\ndata:\n\n")
			}
			return
		case e, open := <-ch:
			if !open {
				return
			}
			if !sseSend(w, rc, "event: "+e.Name()+"\ndata:\n\n") {
				return
			}
			// The list page counters depend on every stack, so they listen to this one.
			if e.Kind == events.KindStack && !sseSend(w, rc, "event: stack-any\ndata:\n\n") {
				return
			}
		case <-t.C:
			if !sseSend(w, rc, ": ping\n\n") {
				return
			}
		}
	}
}

func sseSend(w io.Writer, rc *http.ResponseController, msg string) bool {
	// Best effort: a writer without deadlines (a test recorder) still gets the message.
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := io.WriteString(w, msg); err != nil {
		return false
	}
	return rc.Flush() == nil
}
