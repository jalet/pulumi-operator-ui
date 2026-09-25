package logs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

const (
	intervalDefault = 5 * time.Second
	jobsPerTick     = 10
	workers         = 2
	windowPad       = 5 * time.Second
	retryFirst      = 10 * time.Second
	retryCap        = 2 * time.Minute
	retryWindow     = 10 * time.Minute // after ended_at
	staleAfter      = 24 * time.Hour   // after ended_at: do not even try
)

var (
	_captured = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pou_logs_captured_total",
		Help: "Engine log captures by result.",
	}, []string{"result"})
	_metricsOnce sync.Once
)

// Store is the part of the store the capturer uses.
type Store interface {
	PendingLogs(ctx context.Context, limit int) ([]store.LogJob, error)
	SaveLog(ctx context.Context, runID int64, status string, counts map[string]int64,
		resources []store.LogResource) error
}

// Options configures New.
type Options struct {
	Store    Store
	Source   Source
	Now      func() time.Time
	Log      zerolog.Logger
	Interval time.Duration // 0 = 5s
}

// Capturer captures engine logs for finished runs.
type Capturer struct {
	o         Options
	mu        sync.Mutex
	retry     map[int64]retryState
	forbidden sync.Once
}

type retryState struct {
	next  time.Time
	delay time.Duration
}

// New returns a capturer; it touches nothing until Run.
func New(o Options) *Capturer {
	if o.Store == nil || o.Source == nil || o.Now == nil {
		panic("invariant violated: logs options incomplete")
	}
	if o.Interval == 0 {
		o.Interval = intervalDefault
	}
	_metricsOnce.Do(func() { ctrlmetrics.Registry.MustRegister(_captured) })
	return &Capturer{o: o, retry: map[int64]retryState{}}
}

// Run captures once, then every Interval, until ctx ends. Errors are logged, never returned.
func (c *Capturer) Run(ctx context.Context) error {
	t := time.NewTicker(c.o.Interval)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (c *Capturer) tick(ctx context.Context) {
	jobs, err := c.o.Store.PendingLogs(ctx, jobsPerTick)
	if err != nil {
		c.o.Log.Error().Err(err).Msg("logs: pending")
		return
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for _, j := range jobs {
		g.Go(func() error {
			c.capture(gctx, j)
			return nil // outcomes are recorded per run
		})
	}
	_ = g.Wait() // every goroutine returns nil
}

func (c *Capturer) capture(ctx context.Context, j store.LogJob) {
	now := c.o.Now()
	if now.Sub(j.EndedAt) > staleAfter {
		c.save(ctx, j, store.LogStatusUnavailable, Result{}, "unavailable")
		return
	}
	c.mu.Lock()
	rs, waiting := c.retry[j.RunID]
	c.mu.Unlock()
	if waiting && now.Before(rs.next) {
		return
	}
	lines, truncated, err := c.read(ctx, j)
	switch {
	case errors.Is(err, ErrForbidden):
		c.forbidden.Do(func() {
			c.o.Log.Warn().Err(err).Msg("logs: no access to pods/log; change details stay unavailable")
		})
		c.save(ctx, j, store.LogStatusUnavailable, Result{}, "forbidden")
	case errors.Is(err, ErrGone):
		c.save(ctx, j, store.LogStatusUnavailable, Result{}, "unavailable")
	case err != nil:
		if now.Sub(j.EndedAt) > retryWindow {
			c.save(ctx, j, store.LogStatusUnavailable, Result{}, "unavailable")
			return
		}
		next := retryState{delay: retryFirst}
		if waiting {
			next.delay = min(rs.delay*2, retryCap)
		}
		next.next = now.Add(next.delay)
		c.mu.Lock()
		c.retry[j.RunID] = next
		c.mu.Unlock()
		c.o.Log.Warn().Err(err).Int64("run", j.RunID).Msg("logs: read failed; will retry")
	default:
		res := Parse(lines)
		if !res.Summary && len(res.Resources) == 0 {
			c.save(ctx, j, store.LogStatusUnavailable, Result{}, "unavailable")
			return
		}
		result := "captured"
		if truncated || res.Truncated {
			result = "truncated"
		}
		c.save(ctx, j, store.LogStatusCaptured, res, result)
	}
}

func (c *Capturer) read(ctx context.Context, j store.LogJob) ([]string, bool, error) {
	from, to := j.StartedAt.Add(-windowPad), j.EndedAt.Add(windowPad)
	rc, err := c.o.Source.Stream(ctx, j.Namespace, j.StackName+"-workspace-0", from)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rc.Close() }() // read-only stream
	lines, truncated, err := engineLines(rc, from, to, logBytesMax)
	if err != nil {
		return nil, false, err
	}
	if len(lines) == 0 {
		return nil, false, ErrGone
	}
	return lines, truncated, nil
}

func (c *Capturer) save(ctx context.Context, j store.LogJob, status string, res Result,
	metric string) {
	if err := c.o.Store.SaveLog(ctx, j.RunID, status, res.Counts, res.Resources); err != nil {
		c.o.Log.Error().Err(err).Int64("run", j.RunID).Msg("logs: save")
		return
	}
	c.mu.Lock()
	delete(c.retry, j.RunID)
	c.mu.Unlock()
	_captured.WithLabelValues(metric).Inc()
}
