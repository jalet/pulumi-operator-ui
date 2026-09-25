package s3hist

import (
	"context"
	"errors"
	"io"
	"regexp"
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
	HistoryCursor(ctx context.Context, bucket, prefix string) (string, int64, error)
	SetHistoryCursor(ctx context.Context, bucket, prefix, key string, count int64,
		at time.Time) error
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
	owners := p.owners(ctx, stacks)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(stackConcurrency)
	for _, o := range owners {
		g.Go(func() error {
			p.pollStack(gctx, o.stack, o.target)
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

type owned struct {
	stack  store.S3Stack
	target Target
}

// owners resolves each Stack's history target and picks one owner per target, recording a
// failure for Stacks whose target cannot be resolved. Several Stacks can run the same Pulumi
// stack, such as an applying Stack and a preview-only drift Stack. The history is theirs
// jointly but is read once and shown on one of them: the first that applies, else the
// lowest namespace/name, so ownership never depends on listing order.
func (p *Poller) owners(ctx context.Context, stacks []store.S3Stack) []owned {
	byTarget := map[string]int{}
	var out []owned
	for _, s := range stacks {
		t, err := TargetFor(s)
		if err != nil {
			reason := "backend"
			switch {
			case errors.Is(err, ErrEndpoint):
				reason = "endpoint"
			case errors.Is(err, ErrStackName):
				reason = "stack_name"
			}
			p.fail(ctx, s, reason, err)
			continue
		}
		key := t.Bucket + "/" + t.Prefix
		i, ok := byTarget[key]
		if !ok {
			byTarget[key] = len(out)
			out = append(out, owned{s, t})
			continue
		}
		if prefer(s, out[i].stack) {
			out[i] = owned{s, t}
		}
	}
	return out
}

// prefer reports whether a should own a shared target instead of b.
func prefer(a, b store.S3Stack) bool {
	if a.Preview != b.Preview {
		return !a.Preview
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

func (p *Poller) pollStack(ctx context.Context, s store.S3Stack, t Target) {
	now := p.o.Now()
	client, err := p.o.Client(t.Region)
	if err != nil {
		p.fail(ctx, s, "client", err)
		return
	}
	// S3 calls get a deadline of one interval, so a stalled connection cannot hold up the
	// other stacks or the next tick; store writes keep ctx so the outcome is still recorded.
	s3ctx, cancel := context.WithTimeout(ctx, p.o.Interval)
	defer cancel()
	if err := p.pollTarget(ctx, s3ctx, client, t, now); err != nil {
		p.fail(ctx, s, err.reason, err.err)
		return
	}
	if err := p.o.Store.SetStackS3Status(ctx, s.Namespace, s.Name, "", now); err != nil {
		p.o.Log.Error().Err(err).Str("stack", s.Namespace+"/"+s.Name).Msg("s3 history: status")
	}
}

type pollError struct {
	reason string
	err    error
}

// pollTarget lists and ingests page by page. The cursor moves past every processed key and
// is saved after each page and before returning on an error, so a transient failure retries
// the failed key next tick and the page cap simply resumes next tick.
func (p *Poller) pollTarget(ctx, s3ctx context.Context, client S3API, t Target,
	now time.Time) *pollError {
	cursor, count, err := p.o.Store.HistoryCursor(ctx, t.Bucket, t.Prefix)
	if err != nil {
		return &pollError{"db", err}
	}
	if count == 0 && strings.HasSuffix(cursor, ".history.json") {
		// A history key with no count was written by a version that did not number keys
		// (a rolling update or a rollback): list from the start so numbers stay right.
		cursor = ""
	}
	// last and lastCount move together: the key processed last and how many history keys
	// were listed up to it, which numbers each entry by its place among the prefix's keys.
	last, lastCount := cursor, count
	save := func() *pollError {
		if last == cursor {
			return nil
		}
		if err := p.o.Store.SetHistoryCursor(ctx, t.Bucket, t.Prefix, last, lastCount,
			now); err != nil {
			return &pollError{"db", err}
		}
		cursor, count = last, lastCount
		return nil
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(t.Bucket), Prefix: aws.String(t.Prefix),
		MaxKeys: aws.Int32(listKeysPerPage)}
	if cursor != "" {
		in.StartAfter = aws.String(cursor)
	}
	for range listPagesMax {
		out, err := client.ListObjectsV2(s3ctx, in)
		if err != nil {
			if serr := save(); serr != nil {
				return serr
			}
			return &pollError{"list", err}
		}
		for _, o := range out.Contents {
			key := aws.ToString(o.Key)
			if strings.HasSuffix(key, ".history.json") {
				if err := p.ingest(ctx, s3ctx, client, t, key, lastCount+1, now); err != nil {
					if serr := save(); serr != nil {
						return serr
					}
					return &pollError{"get", err}
				}
				lastCount++
			}
			last = key // S3 lists in key order, so the cursor only moves forward
		}
		if serr := save(); serr != nil {
			return serr
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		in.ContinuationToken = out.NextContinuationToken
	}
	p.o.Log.Info().Str("prefix", t.Prefix).Int("pages", listPagesMax).
		Msg("s3 history: page cap reached; resuming next tick")
	return nil
}

// ingest fetches and stores one key. Permanent problems with the file (bad kind or result,
// bad JSON, too large, too old) are skipped and counted; only fetch errors are returned.
func (p *Poller) ingest(ctx, s3ctx context.Context, client S3API, t Target, key string,
	seq int64, now time.Time) error {
	out, err := client.GetObject(s3ctx, &s3.GetObjectInput{Bucket: aws.String(t.Bucket),
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
		return nil // counted by the caller, so newer keys keep their numbers
	}
	e.Seq = seq
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

// _identity matches what AWS error text says about the caller: ARNs and 12-digit account IDs.
var _identity = regexp.MustCompile(`arn:aws[\w-]*:[^\s"',]+|\b\d{12}\b`)

// summarize turns an error into a short message for the stack page. SDK errors carry no
// credentials, but they can be long and AccessDenied text names the caller's ARN and
// account, so known codes get a plain phrase and anything else has identities removed.
func summarize(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "AllAccessDisabled":
			return "access denied: check the reader's IAM policy for this bucket and prefix"
		case "NoSuchBucket":
			return "bucket not found"
		case "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken":
			return "invalid AWS credentials"
		}
	}
	msg := _identity.ReplaceAllString(err.Error(), "[redacted]")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
