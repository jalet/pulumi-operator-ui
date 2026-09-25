package logs

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

type saved struct {
	status    string
	counts    map[string]int64
	resources []store.LogResource
	truncated bool
}

type fakeStore struct {
	mu    sync.Mutex
	jobs  []store.LogJob
	saves map[int64]saved
}

func (f *fakeStore) PendingLogs(context.Context, int) ([]store.LogJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.LogJob
	for _, j := range f.jobs {
		if _, done := f.saves[j.RunID]; !done {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeStore) SaveLog(_ context.Context, id int64, status string, c map[string]int64,
	r []store.LogResource, truncated bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves[id] = saved{status, c, r, truncated}
	return nil
}

type fakeSource struct {
	mu    sync.Mutex
	body  map[string]string // pod -> log text
	err   error
	calls int
}

func (f *fakeSource) Stream(_ context.Context, _, pod string, _ time.Time) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.body[pod]
	if !ok {
		return nil, ErrGone
	}
	return io.NopCloser(strings.NewReader(b)), nil
}

func job(id int64, ended time.Time) store.LogJob {
	return store.LogJob{RunID: id, Namespace: "pulumi", StackName: "prod",
		StartedAt: ended.Add(-30 * time.Second), EndedAt: ended}
}

func newCapturer(st *fakeStore, src Source, now time.Time) *Capturer {
	return New(Options{Store: st, Source: src, Now: func() time.Time { return now },
		Log: zerolog.Nop()})
}

func TestCaptureStoresResult(t *testing.T) {
	var b strings.Builder
	b.WriteString(kline(_t0.Add(-20*time.Second), "pulumi", "    ~ a:b/c:D: (update)"))
	b.WriteString(kline(_t0.Add(-20*time.Second), "pulumi", "        [urn=urn:pulumi:prod::p::a:b/c:D::x]"))
	b.WriteString(kline(_t0.Add(-20*time.Second), "pulumi", "      ~ k: 1 => 2"))
	b.WriteString(kline(_t0.Add(-19*time.Second), "pulumi", "Resources:"))
	b.WriteString(kline(_t0.Add(-19*time.Second), "pulumi", "    ~ 1 updated"))
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	src := &fakeSource{body: map[string]string{"prod-workspace-0": b.String()}}
	newCapturer(st, src, _t0.Add(time.Second)).tick(t.Context())
	got := st.saves[1]
	if got.status != store.LogStatusCaptured || len(got.resources) != 1 ||
		got.resources[0].Name != "x" || got.counts["update"] != 1 {
		t.Fatalf("saved %+v", got)
	}
}

func TestCaptureGoneIsUnavailable(t *testing.T) {
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	newCapturer(st, &fakeSource{body: map[string]string{}}, _t0.Add(time.Second)).tick(t.Context())
	if st.saves[1].status != store.LogStatusUnavailable {
		t.Fatalf("saved %+v", st.saves[1])
	}
}

func TestCaptureNoEngineLinesIsUnavailable(t *testing.T) {
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	src := &fakeSource{body: map[string]string{"prod-workspace-0": kline(_t0, "server", "hi")}}
	newCapturer(st, src, _t0.Add(time.Second)).tick(t.Context())
	if st.saves[1].status != store.LogStatusUnavailable {
		t.Fatalf("saved %+v", st.saves[1])
	}
}

func TestCaptureForbiddenIsUnavailable(t *testing.T) {
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	src := &fakeSource{err: ErrForbidden}
	newCapturer(st, src, _t0.Add(time.Second)).tick(t.Context())
	if st.saves[1].status != store.LogStatusUnavailable {
		t.Fatalf("saved %+v", st.saves[1])
	}
}

func TestCaptureTransientRetriesThenGivesUp(t *testing.T) {
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	src := &fakeSource{err: errors.New("connection reset")}
	now := _t0.Add(time.Second)
	c := New(Options{Store: st, Source: src, Now: func() time.Time { return now }, Log: zerolog.Nop()})
	c.tick(t.Context())
	if _, done := st.saves[1]; done {
		t.Fatal("gave up on the first transient error")
	}
	c.tick(t.Context()) // inside the backoff: no new attempt
	if src.calls != 1 {
		t.Fatalf("calls = %d during backoff, want 1", src.calls)
	}
	now = _t0.Add(11 * time.Minute)
	c.tick(t.Context())
	if st.saves[1].status != store.LogStatusUnavailable {
		t.Fatalf("saved %+v after the retry window", st.saves[1])
	}
}

func TestCaptureOldPendingIsUnavailable(t *testing.T) {
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	src := &fakeSource{body: map[string]string{"prod-workspace-0": kline(_t0, "pulumi", "x")}}
	newCapturer(st, src, _t0.Add(25*time.Hour)).tick(t.Context())
	if st.saves[1].status != store.LogStatusUnavailable || src.calls != 0 {
		t.Fatalf("saved %+v calls %d", st.saves[1], src.calls)
	}
}

type stallSource struct{}

func (stallSource) Stream(ctx context.Context, _, _ string, _ time.Time) (io.ReadCloser, error) {
	<-ctx.Done() // a log proxy that never answers
	return nil, ctx.Err()
}

func TestCaptureStalledStreamTimesOut(t *testing.T) {
	defer func(d time.Duration) { captureTimeout = d }(captureTimeout)
	captureTimeout = 50 * time.Millisecond
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	c := newCapturer(st, stallSource{}, _t0.Add(time.Second))
	done := make(chan struct{})
	go func() { c.tick(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick blocked on a stalled log stream")
	}
	if _, saved := st.saves[1]; saved {
		t.Fatal("a timeout inside the retry window must retry, not give up")
	}
}

func TestCaptureMarksReadCapTruncation(t *testing.T) {
	defer func(n int) { logBytesMax = n }(logBytesMax)
	var b strings.Builder
	b.WriteString(kline(_t0.Add(-20*time.Second), "pulumi", "    + a:b/c:D: (create)"))
	b.WriteString(kline(_t0.Add(-20*time.Second), "pulumi", "        [urn=urn:pulumi:prod::p::a:b/c:D::x]"))
	for range 50 {
		b.WriteString(kline(_t0.Add(-20*time.Second), "pulumi", "      k: \""+strings.Repeat("v", 80)+"\""))
	}
	logBytesMax = b.Len() / 2 // the summary is never reached
	st := &fakeStore{jobs: []store.LogJob{job(1, _t0)}, saves: map[int64]saved{}}
	src := &fakeSource{body: map[string]string{"prod-workspace-0": b.String()}}
	newCapturer(st, src, _t0.Add(time.Second)).tick(t.Context())
	got := st.saves[1]
	if got.status != store.LogStatusCaptured || !got.truncated || len(got.resources) != 1 ||
		!got.resources[0].Truncated {
		t.Fatalf("saved %+v, want captured, truncated, last resource marked", got)
	}
}
