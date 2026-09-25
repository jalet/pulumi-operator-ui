package s3hist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var _now = time.Unix(1790239732, 0).UTC().Add(time.Hour)

// fakeS3 holds objects per bucket and records every call.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]map[string][]byte // bucket -> key -> body
	listErr map[string]error             // bucket -> error
	getErr  map[string]error             // key -> error
	lists   []s3.ListObjectsV2Input
	gets    []string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]map[string][]byte{}, listErr: map[string]error{},
		getErr: map[string]error{}}
}

func (f *fakeS3) put(bucket, key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects[bucket] == nil {
		f.objects[bucket] = map[string][]byte{}
	}
	f.objects[bucket][key] = body
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input,
	_ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, *in)
	bucket := aws.ToString(in.Bucket)
	if err := f.listErr[bucket]; err != nil {
		return nil, err
	}
	var keys []string
	for k := range f.objects[bucket] {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) && k > aws.ToString(in.StartAfter) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	start := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := min(start+int(aws.ToInt32(in.MaxKeys)), len(keys))
	out := &s3.ListObjectsV2Output{}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k)})
	}
	if end < len(keys) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput,
	_ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := aws.ToString(in.Key)
	f.gets = append(f.gets, key)
	if err := f.getErr[key]; err != nil {
		return nil, err
	}
	b, ok := f.objects[aws.ToString(in.Bucket)][key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "not found"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// fakeStore records what the poller writes.
type fakeStore struct {
	mu        sync.Mutex
	stacks    []store.S3Stack
	cursors   map[string]string // bucket|prefix -> key
	inserted  []store.HistoryEntry
	statuses  map[string]string // ns/name -> error message
	links     []store.LinkResult
	linkCalls int
}

func newFakeStore(stacks ...store.S3Stack) *fakeStore {
	return &fakeStore{stacks: stacks, cursors: map[string]string{}, statuses: map[string]string{}}
}

func (f *fakeStore) S3Stacks(context.Context) ([]store.S3Stack, error) { return f.stacks, nil }

func (f *fakeStore) HistoryCursor(_ context.Context, bucket, prefix string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[bucket+"|"+prefix], nil
}

func (f *fakeStore) SetHistoryCursor(_ context.Context, bucket, prefix, key string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key > f.cursors[bucket+"|"+prefix] {
		f.cursors[bucket+"|"+prefix] = key
	}
	return nil
}

func (f *fakeStore) cursor() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors["b|"+_prefix]
}

func (f *fakeStore) InsertHistory(_ context.Context, e store.HistoryEntry, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, e)
	return true, nil
}

func (f *fakeStore) LinkHistory(context.Context, time.Time) (store.LinkResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkCalls++
	if len(f.links) == 0 {
		return store.LinkResult{}, nil
	}
	r := f.links[0]
	if len(f.links) > 1 {
		f.links = f.links[1:]
	}
	return r, nil
}

func (f *fakeStore) SetStackS3Status(_ context.Context, ns, name, msg string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[ns+"/"+name] = msg
	return nil
}

func (f *fakeStore) status(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.statuses[key]
	return s, ok
}

func historyBody(kind string, end int64) []byte {
	return []byte(fmt.Sprintf(`{"kind":%q,"startTime":%d,"endTime":%d,"result":"succeeded",`+
		`"resourceChanges":{"same":1},"environment":{"git.head":"abc"}}`, kind, end-5, end))
}

func newPoller(st *fakeStore, s3api *fakeS3) *Poller {
	return New(Options{Store: st, Client: func(string) (S3API, error) { return s3api, nil },
		Interval: time.Minute, Retention: 180 * 24 * time.Hour, Now: func() time.Time { return _now },
		Log: zerolog.Nop()})
}

var _stack = store.S3Stack{Namespace: "ns", Name: "app", BackendURL: "s3://b/p?region=eu-north-1",
	Project: "proj", PulumiStack: "dev"}

const _prefix = "p/.pulumi/history/proj/dev/"

func TestTickIngestsAndLinks(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239732))
	fs.put("b", _prefix+"dev-1.checkpoint.json", []byte(`{"secret":"state"}`))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 1 || st.inserted[0].Type != store.RunTypeUp ||
		st.inserted[0].Key != _prefix+"dev-1.history.json" {
		t.Fatalf("inserted = %+v", st.inserted)
	}
	if st.linkCalls == 0 {
		t.Error("LinkHistory not called")
	}
	if msg, ok := st.status("ns/app"); !ok || msg != "" {
		t.Errorf("status = %q, %v; want success", msg, ok)
	}
	for _, k := range fs.gets {
		if strings.HasSuffix(k, ".checkpoint.json") {
			t.Fatalf("fetched checkpoint %s", k)
		}
	}
}

func TestTickUsesStartAfter(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	st.cursors["b|"+_prefix] = _prefix + "dev-100.history.json"
	newPoller(st, fs).tick(t.Context())
	if len(fs.lists) == 0 {
		t.Fatal("no list call")
	}
	in := fs.lists[0]
	if aws.ToString(in.StartAfter) != _prefix+"dev-100.history.json" || aws.ToString(in.Prefix) != _prefix {
		t.Fatalf("list input StartAfter=%q Prefix=%q", aws.ToString(in.StartAfter), aws.ToString(in.Prefix))
	}
}

func TestTickPaginates(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	for i := range 2500 {
		fs.put("b", fmt.Sprintf("%sdev-%05d.history.json", _prefix, i), historyBody("update", 1790239732))
	}
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 2500 || len(fs.lists) != 3 {
		t.Fatalf("inserted %d over %d list calls, want 2500 over 3", len(st.inserted), len(fs.lists))
	}
}

// Hitting the page cap is not an error: the cursor moves with every processed key, so the
// next tick continues where this one stopped.
func TestTickPageCapResumesNextTick(t *testing.T) {
	defer func(n int) { listPagesMax = n }(listPagesMax)
	listPagesMax = 2
	st, fs := newFakeStore(_stack), newFakeS3()
	for i := range 2500 {
		fs.put("b", fmt.Sprintf("%sdev-%05d.history.json", _prefix, i), historyBody("update", 1790239732))
	}
	p := newPoller(st, fs)
	p.tick(t.Context())
	if len(st.inserted) != 2000 {
		t.Fatalf("first tick inserted %d, want 2000", len(st.inserted))
	}
	if msg, _ := st.status("ns/app"); msg != "" {
		t.Errorf("page cap reported as a failure: %q", msg)
	}
	p.tick(t.Context())
	if len(st.inserted) != 2500 {
		t.Fatalf("after second tick inserted %d, want 2500", len(st.inserted))
	}
}

// Keys skipped as too old or unreadable still move the cursor, so they are not re-fetched.
func TestTickAdvancesPastSkippedEntries(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	old := _now.Add(-200 * 24 * time.Hour).Unix()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", old))
	fs.put("b", _prefix+"dev-2.history.json", []byte("{nope"))
	p := newPoller(st, fs)
	p.tick(t.Context())
	if got := st.cursor(); got != _prefix+"dev-2.history.json" {
		t.Fatalf("cursor = %q, want past both skipped keys", got)
	}
	fetched := len(fs.gets)
	p.tick(t.Context())
	if len(fs.gets) != fetched {
		t.Fatalf("second tick re-fetched skipped keys: %v", fs.gets[fetched:])
	}
}
func TestTickSkipsOldEntries(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	old := _now.Add(-200 * 24 * time.Hour).Unix()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", old))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 0 {
		t.Fatalf("inserted an entry older than retention: %+v", st.inserted)
	}
}

func TestTickIsolatesStackErrors(t *testing.T) {
	other := _stack
	other.Name, other.BackendURL = "denied", "s3://locked/p"
	st, fs := newFakeStore(_stack, other), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239732))
	fs.listErr["locked"] = &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
	before := testutil.ToFloat64(_s3Errors.WithLabelValues("list"))
	newPoller(st, fs).tick(t.Context())
	if msg, _ := st.status("ns/denied"); !strings.HasPrefix(msg, "access denied") {
		t.Errorf("denied stack status = %q", msg)
	}
	if msg, ok := st.status("ns/app"); !ok || msg != "" {
		t.Errorf("healthy stack status = %q, %v", msg, ok)
	}
	if len(st.inserted) != 1 {
		t.Errorf("healthy stack entries = %d, want 1", len(st.inserted))
	}
	if testutil.ToFloat64(_s3Errors.WithLabelValues("list")) <= before {
		t.Error("list error metric did not increment")
	}
}

func TestTickSkipsBadFiles(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("preview", 1790239732))
	fs.put("b", _prefix+"dev-2.history.json", []byte("{nope"))
	fs.put("b", _prefix+"dev-3.history.json", historyBody("refresh", 1790239732))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 1 || st.inserted[0].Type != store.RunTypeRefresh {
		t.Fatalf("inserted = %+v, want only the refresh", st.inserted)
	}
	if msg, _ := st.status("ns/app"); msg != "" {
		t.Errorf("bad files must not mark the stack failed: %q", msg)
	}
}

func TestTickSkipsUnsupportedBackends(t *testing.T) {
	minio := _stack
	minio.BackendURL = "s3://b/p?endpoint=minio:9000"
	st, fs := newFakeStore(minio), newFakeS3()
	newPoller(st, fs).tick(t.Context())
	if len(fs.lists) != 0 {
		t.Fatal("listed a bucket behind an unsupported endpoint")
	}
	if msg, _ := st.status("ns/app"); !strings.Contains(msg, "endpoint") {
		t.Fatalf("status = %q", msg)
	}
}

func TestTickLinksUntilDone(t *testing.T) {
	st, fs := newFakeStore(), newFakeS3()
	st.links = []store.LinkResult{{Imported: 2, Pending: 3}, {Pending: 3}}
	newPoller(st, fs).tick(t.Context())
	if st.linkCalls != 2 {
		t.Fatalf("LinkHistory called %d times, want 2 (stop when no progress)", st.linkCalls)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	p := newPoller(newFakeStore(), newFakeS3())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// The cursor is the newest stored key, so a transient fetch error must stop the pass:
// storing a later key would skip the failed one for good.
func TestTickStopsAtFetchError(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	for i := 1; i <= 3; i++ {
		fs.put("b", fmt.Sprintf("%sdev-%d.history.json", _prefix, i), historyBody("update", 1790239732))
	}
	fs.getErr[_prefix+"dev-2.history.json"] = errors.New("connection reset")
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 1 || st.inserted[0].Key != _prefix+"dev-1.history.json" {
		t.Fatalf("inserted = %+v, want only dev-1", st.inserted)
	}
	if msg, _ := st.status("ns/app"); !strings.Contains(msg, "connection reset") {
		t.Fatalf("status = %q", msg)
	}
	if got := st.cursor(); got != _prefix+"dev-1.history.json" {
		t.Fatalf("cursor = %q, want dev-1 so dev-2 is retried", got)
	}
}
