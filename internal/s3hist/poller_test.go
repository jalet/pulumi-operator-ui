package s3hist

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
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
	stall   map[string]bool              // bucket -> ListObjectsV2 hangs until cancelled
	lists   []s3.ListObjectsV2Input
	gets    []string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]map[string][]byte{}, listErr: map[string]error{},
		getErr: map[string]error{}, stall: map[string]bool{}}
}

func (f *fakeS3) put(bucket, key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects[bucket] == nil {
		f.objects[bucket] = map[string][]byte{}
	}
	f.objects[bucket][key] = body
}

func (f *fakeS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input,
	_ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.stall[aws.ToString(in.Bucket)] {
		<-ctx.Done() // a connection that never answers
		return nil, ctx.Err()
	}
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
	counts    map[string]int64  // bucket|prefix -> history keys listed
	inserted  []store.HistoryEntry
	statuses  map[string]string // ns/name -> error message
	links     []store.LinkResult
	linkCalls int
	insertErr error // returned by InsertHistory when set
}

func newFakeStore(stacks ...store.S3Stack) *fakeStore {
	return &fakeStore{stacks: stacks, cursors: map[string]string{}, counts: map[string]int64{},
		statuses: map[string]string{}}
}

func (f *fakeStore) S3Stacks(context.Context) ([]store.S3Stack, error) { return f.stacks, nil }

func (f *fakeStore) HistoryCursor(_ context.Context, bucket, prefix string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[bucket+"|"+prefix], f.counts[bucket+"|"+prefix], nil
}

func (f *fakeStore) SetHistoryCursor(_ context.Context, bucket, prefix, key string, count int64,
	_ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := bucket + "|" + prefix
	if key > f.cursors[k] || (f.counts[k] == 0 && count > 0) { // as SetHistoryCursor
		f.cursors[k] = key
		f.counts[k] = count
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
	if f.insertErr != nil {
		return false, f.insertErr
	}
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
	st.counts["b|"+_prefix] = 100
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
	if a, b := st.inserted[1999], st.inserted[2000]; b.Seq != a.Seq+1 || a.Seq != 2000 {
		t.Errorf("numbering across the page cap: %d then %d, want 2000 then 2001", a.Seq, b.Seq)
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

// A stalled S3 connection must not hold up the other stacks or the next tick.
func TestTickTimesOutStalledStack(t *testing.T) {
	stalled := _stack
	stalled.Name, stalled.BackendURL = "stalled", "s3://slow/p"
	st, fs := newFakeStore(_stack, stalled), newFakeS3()
	fs.stall["slow"] = true
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239732))
	p := New(Options{Store: st, Client: func(string) (S3API, error) { return fs, nil },
		Interval: 100 * time.Millisecond, Retention: 180 * 24 * time.Hour,
		Now: func() time.Time { return _now }, Log: zerolog.Nop()})
	done := make(chan struct{})
	go func() { p.tick(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("tick blocked on a stalled stack")
	}
	if msg, _ := st.status("ns/stalled"); !strings.Contains(msg, "deadline") {
		t.Errorf("stalled stack status = %q", msg)
	}
	if len(st.inserted) != 1 {
		t.Errorf("healthy stack entries = %d, want 1", len(st.inserted))
	}
}

func TestTickSharedTargetOwnedByApplyingStack(t *testing.T) {
	drift := _stack
	drift.Name, drift.Preview = "app-drift", true
	// The preview Stack sorts first by name; ownership must not depend on order.
	st, fs := newFakeStore(drift, _stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239732))
	fs.put("b", _prefix+"dev-2.history.json", historyBody("update", 1790239800))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 2 {
		t.Fatalf("inserted %d entries, want 2", len(st.inserted))
	}
	for _, e := range st.inserted {
		if e.StackName != "app" {
			t.Errorf("entry %s attributed to %s, want app", e.Key, e.StackName)
		}
	}
	if len(fs.lists) != 1 {
		t.Errorf("listed %d times, want 1", len(fs.lists))
	}
}

func TestTickSharedTargetWithoutApplierPicksLowestName(t *testing.T) {
	a, b := _stack, _stack
	a.Name, a.Preview = "b-drift", true
	b.Name, b.Preview = "a-drift", true
	st, fs := newFakeStore(a, b), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239732))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 1 || st.inserted[0].StackName != "a-drift" {
		t.Fatalf("inserted = %+v, want one entry on a-drift", st.inserted)
	}
}

// AWS AccessDenied messages name the caller ("User: arn:aws:iam::<account>:user/..."); the
// stack page shows this text, so it must not carry the ARN or account ID.
func TestSummarizeAccessDeniedHidesIdentity(t *testing.T) {
	err := &smithy.GenericAPIError{Code: "AccessDenied", Message: "User: " +
		"arn:aws:iam::123456789012:user/system/k8s/pou is not authorized to perform: s3:ListBucket"}
	got := summarize(err)
	if strings.Contains(got, "arn:") || strings.Contains(got, "123456789012") {
		t.Fatalf("summary leaks identity: %q", got)
	}
	if !strings.Contains(got, "access denied") {
		t.Fatalf("summary = %q, want it to say access denied", got)
	}
}
func TestTickNumbersKeysInOrder(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239700))
	fs.put("b", _prefix+"dev-1.checkpoint.json", []byte(`{}`)) // not counted
	fs.put("b", _prefix+"dev-2.history.json", historyBody("update", 1790239800))
	newPoller(st, fs).tick(t.Context())
	got := map[string]int64{}
	for _, e := range st.inserted {
		got[e.Key] = e.Seq
	}
	if got[_prefix+"dev-1.history.json"] != 1 || got[_prefix+"dev-2.history.json"] != 2 {
		t.Fatalf("seq = %v", got)
	}
}

// Keys too old to keep are counted, so the numbers of newer ones stay right.
func TestTickCountsTooOldKeys(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1000)) // decades old
	fs.put("b", _prefix+"dev-2.history.json", historyBody("update", 1790239800))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 1 || st.inserted[0].Seq != 2 {
		t.Fatalf("inserted = %+v, want only dev-2 with seq 2", st.inserted)
	}
	if st.cursor() != _prefix+"dev-2.history.json" {
		t.Fatalf("cursor = %s, want dev-2", st.cursor())
	}
}

func TestSeqStableAcrossReread(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239700))
	p := newPoller(st, fs)
	p.tick(t.Context())
	fs.put("b", _prefix+"dev-2.history.json", historyBody("update", 1790239800))
	p.tick(t.Context())
	last := st.inserted[len(st.inserted)-1]
	if last.Key != _prefix+"dev-2.history.json" || last.Seq != 2 {
		t.Fatalf("second tick inserted %+v, want dev-2 seq 2", last)
	}
}

// A cursor written by the previous version during a rolling update has a history key but a
// count of 0; resuming from it would number every later key from 1. It must renumber.
func TestTickRenumbersCountlessCursor(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239700))
	fs.put("b", _prefix+"dev-2.history.json", historyBody("update", 1790239800))
	fs.put("b", _prefix+"dev-3.history.json", historyBody("update", 1790239900))
	st.cursors["b|"+_prefix] = _prefix + "dev-2.history.json" // old pod's cursor, no count
	newPoller(st, fs).tick(t.Context())
	got := map[string]int64{}
	for _, e := range st.inserted {
		got[e.Key] = e.Seq
	}
	if got[_prefix+"dev-3.history.json"] != 3 || got[_prefix+"dev-1.history.json"] != 1 {
		t.Fatalf("seq = %v, want every key renumbered from the start", got)
	}
}

// A file that can never be read (gone) is counted and passed, so the Stack's later history
// still arrives.
func TestTickSkipsUnreadableFile(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	for i := 1; i <= 3; i++ {
		fs.put("b", fmt.Sprintf("%sdev-%d.history.json", _prefix, i), historyBody("update", 1790239700+int64(i)))
	}
	fs.getErr[_prefix+"dev-2.history.json"] = &smithy.GenericAPIError{Code: "NoSuchKey", Message: "gone"}
	before := testutil.ToFloat64(_s3Errors.WithLabelValues("skipped"))
	newPoller(st, fs).tick(t.Context())
	got := map[string]int64{}
	for _, e := range st.inserted {
		got[path.Base(e.Key)] = e.Seq
	}
	if len(got) != 2 || got["dev-1.history.json"] != 1 || got["dev-3.history.json"] != 3 {
		t.Fatalf("inserted %v, want dev-1 #1 and dev-3 #3", got)
	}
	if st.cursor() != _prefix+"dev-3.history.json" ||
		testutil.ToFloat64(_s3Errors.WithLabelValues("skipped")) != before+1 {
		t.Fatalf("cursor %s, skipped metric not counted", st.cursor())
	}
}

func TestTickInsertErrorIsDB(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239800))
	st.insertErr = errors.New("db down")
	before := testutil.ToFloat64(_s3Errors.WithLabelValues("db"))
	newPoller(st, fs).tick(t.Context())
	if testutil.ToFloat64(_s3Errors.WithLabelValues("db")) != before+1 {
		t.Fatal("insert error not labelled db")
	}
}

// A transient fetch error stops the pass; the next tick numbers from where it stopped.
func TestTickNumbersAfterFetchError(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	for i := 1; i <= 3; i++ {
		fs.put("b", fmt.Sprintf("%sdev-%d.history.json", _prefix, i), historyBody("update", 1790239700+int64(i)))
	}
	fs.getErr[_prefix+"dev-2.history.json"] = errors.New("connection reset")
	p := newPoller(st, fs)
	p.tick(t.Context())
	delete(fs.getErr, _prefix+"dev-2.history.json")
	p.tick(t.Context())
	got := map[string]int64{}
	for _, e := range st.inserted {
		got[path.Base(e.Key)] = e.Seq
	}
	if got["dev-1.history.json"] != 1 || got["dev-2.history.json"] != 2 || got["dev-3.history.json"] != 3 {
		t.Fatalf("seq = %v, want 1 2 3", got)
	}
}

func TestTickReadsGzipHistory(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(historyBody("update", 1790239800))
	_ = zw.Close()
	fs.put("b", _prefix+"dev-1.history.json.gz", buf.Bytes())
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 1 || st.inserted[0].Seq != 1 {
		t.Fatalf("inserted %+v, want the gzipped entry as #1", st.inserted)
	}
}

func TestTickGzipBombCapped(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(bytes.Repeat([]byte(" "), historyBytesMax+1))
	_ = zw.Close()
	fs.put("b", _prefix+"dev-1.history.json.gz", buf.Bytes())
	before := testutil.ToFloat64(_s3Errors.WithLabelValues("size"))
	newPoller(st, fs).tick(t.Context())
	if len(st.inserted) != 0 || testutil.ToFloat64(_s3Errors.WithLabelValues("size")) != before+1 {
		t.Fatalf("inserted %d, size not counted", len(st.inserted))
	}
}

// Access denied on a file is usually the whole bucket (a missing kms:Decrypt, for example):
// it must show on the Stack and hold the cursor, not pass over every file.
func TestTickAccessDeniedHoldsCursor(t *testing.T) {
	st, fs := newFakeStore(_stack), newFakeS3()
	fs.put("b", _prefix+"dev-1.history.json", historyBody("update", 1790239800))
	fs.getErr[_prefix+"dev-1.history.json"] = &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
	newPoller(st, fs).tick(t.Context())
	msg, _ := st.status("ns/app")
	if len(st.inserted) != 0 || st.cursor() != "" || !strings.Contains(msg, "denied") {
		t.Fatalf("inserted %d cursor %q status %q, want a Stack error and no progress",
			len(st.inserted), st.cursor(), msg)
	}
}
