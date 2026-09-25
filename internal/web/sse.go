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
		case <-t.C:
			if !sseSend(w, rc, ": ping\n\n") {
				return
			}
		}
	}
}

func sseSend(w io.Writer, rc *http.ResponseController, msg string) bool {
	if _, err := io.WriteString(w, msg); err != nil {
		return false
	}
	return rc.Flush() == nil
}
