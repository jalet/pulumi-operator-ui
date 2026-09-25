package s3hist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

const (
	listKeysPerPage = 1000
	// Four Stacks at a time keeps a small fleet quick without bursting S3 requests.
	stackConcurrency = 4
	// LinkHistory handles 500 rows per call; 20 calls covers a 10k-entry first backlog.
	linkCallsMax = 20
)

// listPagesMax bounds one Stack's listing per tick (50k keys); a var so tests can shrink it.
var listPagesMax = 50

var (
	_s3Errors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pou_s3_errors_total",
		Help: "S3 history problems by reason.",
	}, []string{"reason"})
	_metricsOnce sync.Once
)

// S3API is the part of *s3.Client the poller uses.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input,
		opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput,
		opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Store is the part of the store the poller uses.
type Store interface {
	S3Stacks(ctx context.Context) ([]store.S3Stack, error)
	NewestHistoryKey(ctx context.Context, bucket, keyPrefix string) (string, error)
	InsertHistory(ctx context.Context, e store.HistoryEntry, seenAt time.Time) (bool, error)
	LinkHistory(ctx context.Context, now time.Time) (store.LinkResult, error)
	SetStackS3Status(ctx context.Context, namespace, name, errMsg string, at time.Time) error
}

// Options configures New.
type Options struct {
	Store     Store
	Client    func(region string) (S3API, error) // one client per region, cached by the caller
	Interval  time.Duration
	Retention time.Duration
	Now       func() time.Time
	Log       zerolog.Logger
}

// Poller ingests S3 history for every Stack on an S3 backend.
type Poller struct {
	o Options
}

// New returns a poller; it touches no AWS API until Run.
func New(o Options) *Poller {
	if o.Store == nil || o.Client == nil || o.Now == nil || o.Interval <= 0 {
		panic("invariant violated: s3hist options incomplete")
	}
	_metricsOnce.Do(func() { ctrlmetrics.Registry.MustRegister(_s3Errors) })
	return &Poller{o: o}
}

// Run polls once, then every Interval, until ctx ends. Failures are per Stack and recorded,
// never returned, so a broken bucket cannot stop the process.
func (p *Poller) Run(ctx context.Context) error {
	t := time.NewTicker(p.o.Interval)
	defer t.Stop()
	for {
		p.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	stacks, err := p.o.Store.S3Stacks(ctx)
	if err != nil {
		p.o.Log.Error().Err(err).Msg("s3 history: list stacks")
		return
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(stackConcurrency)
	for _, s := range stacks {
		g.Go(func() error {
			p.pollStack(gctx, s)
			return nil // per-Stack failures are recorded, not propagated
		})
	}
	_ = g.Wait() // every goroutine returns nil
	p.link(ctx)
}

// link drains pending entries until none remain or a call makes no progress.
func (p *Poller) link(ctx context.Context) {
	for range linkCallsMax {
		res, err := p.o.Store.LinkHistory(ctx, p.o.Now())
		if err != nil {
			p.o.Log.Error().Err(err).Msg("s3 history: link")
			return
		}
		if res.Pending == 0 || res.Linked+res.Imported+res.Ambiguous == 0 {
			return
		}
	}
	p.o.Log.Warn().Int("calls", linkCallsMax).Msg("s3 history: link backlog remains for next tick")
}

func (p *Poller) pollStack(ctx context.Context, s store.S3Stack) {
	now := p.o.Now()
	t, err := TargetFor(s)
	if err != nil {
		reason := "backend"
		if errors.Is(err, ErrEndpoint) {
			reason = "endpoint"
		}
		p.fail(ctx, s, reason, err)
		return
	}
	client, err := p.o.Client(t.Region)
	if err != nil {
		p.fail(ctx, s, "client", err)
		return
	}
	keys, err := p.listNew(ctx, client, t)
	if err != nil {
		p.fail(ctx, s, "list", err)
		return
	}
	for _, key := range keys {
		if err := p.ingest(ctx, client, t, key, now); err != nil {
			// A transient fetch error stops this Stack's pass: the cursor is the newest stored
			// key, so storing a later one would skip this key for good.
			p.fail(ctx, s, "get", err)
			return
		}
	}
	if err := p.o.Store.SetStackS3Status(ctx, s.Namespace, s.Name, "", now); err != nil {
		p.o.Log.Error().Err(err).Str("stack", s.Namespace+"/"+s.Name).Msg("s3 history: status")
	}
}

// listNew returns the history keys after the newest stored one, in key (time) order.
func (p *Poller) listNew(ctx context.Context, client S3API, t Target) ([]string, error) {
	after, err := p.o.Store.NewestHistoryKey(ctx, t.Bucket, t.Prefix)
	if err != nil {
		return nil, err
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(t.Bucket), Prefix: aws.String(t.Prefix),
		MaxKeys: aws.Int32(listKeysPerPage)}
	if after != "" {
		in.StartAfter = aws.String(after)
	}
	var keys []string
	for range listPagesMax {
		out, err := client.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			if k := aws.ToString(o.Key); strings.HasSuffix(k, ".history.json") {
				keys = append(keys, k)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			slices.Sort(keys)
			return keys, nil
		}
		in.ContinuationToken = out.NextContinuationToken
	}
	return nil, fmt.Errorf("listing exceeds page cap (%d pages of %d keys)", listPagesMax,
		listKeysPerPage)
}

// ingest fetches and stores one key. Permanent problems with the file (bad kind or result,
// bad JSON, too large, too old) are skipped and counted; only fetch errors are returned.
func (p *Poller) ingest(ctx context.Context, client S3API, t Target, key string,
	now time.Time) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(t.Bucket),
		Key: aws.String(key)})
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(out.Body, historyBytesMax+1))
	_ = out.Body.Close() // read-only stream; the read error below is what matters
	if err != nil {
		return err
	}
	e, err := ParseEntry(t, key, body)
	if err != nil {
		reason := "parse"
		switch {
		case errors.Is(err, ErrBadKind):
			reason = "kind"
		case errors.Is(err, ErrBadResult):
			reason = "result"
		case errors.Is(err, ErrTooLarge):
			reason = "size"
		}
		_s3Errors.WithLabelValues(reason).Inc()
		p.o.Log.Warn().Err(err).Str("key", key).Msg("s3 history: skipped file")
		return nil
	}
	if e.EndedAt.Before(now.Add(-p.o.Retention)) {
		return nil
	}
	if _, err := p.o.Store.InsertHistory(ctx, e, now); err != nil {
		return err
	}
	return nil
}

func (p *Poller) fail(ctx context.Context, s store.S3Stack, reason string, err error) {
	_s3Errors.WithLabelValues(reason).Inc()
	msg := summarize(err)
	p.o.Log.Warn().Err(err).Str("stack", s.Namespace+"/"+s.Name).Str("reason", reason).
		Msg("s3 history unavailable")
	if serr := p.o.Store.SetStackS3Status(ctx, s.Namespace, s.Name, msg, p.o.Now()); serr != nil {
		p.o.Log.Error().Err(serr).Msg("s3 history: status")
	}
}

// summarize turns an error into a short message for the stack page. SDK errors carry no
// credentials, but they can be long, so known codes get a plain phrase.
func summarize(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "AllAccessDisabled":
			return "access denied: " + api.ErrorMessage()
		case "NoSuchBucket":
			return "bucket not found"
		case "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken":
			return "invalid AWS credentials"
		}
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
