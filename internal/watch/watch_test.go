package watch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/jalet/pulumi-operator-ui/internal/record"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var (
	_cfg *rest.Config
	_c   client.Client
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../test/crds/pko-2.9.1"},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	_cfg, err = env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (is KUBEBUILDER_ASSETS set? use mise run test):", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stop envtest:", err)
		}
	}()
	_c, err = client.New(_cfg, client.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		return 1
	}
	return m.Run()
}

// memWriter records the latest row written per key.
type memWriter struct {
	mu      sync.Mutex
	stacks  map[string]store.Stack
	deleted map[string]int
	runs    map[string]store.Run
}

func newMemWriter() *memWriter {
	return &memWriter{stacks: map[string]store.Stack{}, deleted: map[string]int{},
		runs: map[string]store.Run{}}
}

func (w *memWriter) UpsertStack(_ context.Context, s store.Stack) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stacks[s.Namespace+"/"+s.Name] = s
	return nil
}

func (w *memWriter) MarkStackDeleted(_ context.Context, ns, name string, _ time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted[ns+"/"+name]++
	delete(w.stacks, ns+"/"+name)
	return nil
}

func (w *memWriter) UpsertRun(_ context.Context, r store.Run) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runs[r.Namespace+"/"+r.UpdateName] = r
	return nil
}

func (w *memWriter) InsertRunIfAbsent(_ context.Context, r store.Run) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.runs[r.Namespace+"/"+r.UpdateName]; !ok {
		w.runs[r.Namespace+"/"+r.UpdateName] = r
	}
	return nil
}

func (w *memWriter) stack(key string) (store.Stack, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.stacks[key]
	return s, ok
}

func (w *memWriter) run(key string) (store.Run, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.runs[key]
	return r, ok
}

func (w *memWriter) deletions(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deleted[key]
}

// startManager runs a manager until the test ends.
func startManager(t *testing.T, namespaces ...string) *memWriter {
	t.Helper()
	w := newMemWriter()
	mgr, err := NewManager(_cfg, Options{Namespaces: namespaces, MetricsAddr: "0", Writer: w,
		Now: time.Now, skipNameValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)            // single result from mgr.Start
	go func() { done <- mgr.Start(ctx) }() // ends when cancel is called in Cleanup
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	return w
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func never(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("unexpectedly saw %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func newNamespace(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	ns := prefix + "-" + hex.EncodeToString(b)
	if err := _c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	return ns
}

func fromFile(t *testing.T, path, ns string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(b, &obj.Object); err != nil {
		t.Fatal(err)
	}
	obj.SetNamespace(ns)
	return obj
}

func condition(typ, status string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": "Test", "message": "",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}
}

func setStatus(t *testing.T, obj *unstructured.Unstructured, status map[string]any) {
	t.Helper()
	obj.Object["status"] = status
	if err := _c.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}

func createStack(t *testing.T, ns string, status map[string]any) *unstructured.Unstructured {
	t.Helper()
	st := fromFile(t, "testdata/stack.yaml", ns)
	if err := _c.Create(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if status != nil {
		setStatus(t, st, status)
	}
	return st
}

func createUpdate(t *testing.T, ns string, owner *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	u := fromFile(t, "testdata/update.yaml", ns)
	if owner != nil {
		u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "pulumi.com/v1", Kind: "Stack",
			Name: owner.GetName(), UID: owner.GetUID()}})
	}
	if err := _c.Create(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestStackRecorded(t *testing.T) {
	ns := newNamespace(t, "rec")
	w := startManager(t)
	createStack(t, ns, map[string]any{
		"conditions": []any{condition("Ready", "True")},
		"lastUpdate": map[string]any{"name": "u0", "type": "up", "state": "succeeded",
			"lastAttemptedCommit": "aaa", "lastSuccessfulCommit": "aaa"},
	})
	eventually(t, "ready stack", func() bool {
		s, ok := w.stack(ns + "/app")
		return ok && s.Ready && s.LastCommit == "aaa"
	})
	eventually(t, "backfilled run", func() bool {
		r, ok := w.run(ns + "/u0")
		return ok && r.Commit == "aaa" && r.UID == "" && r.State == store.RunStateSucceeded
	})
}

func TestUpdateRecordedWithStackCommit(t *testing.T) {
	ns := newNamespace(t, "upd")
	w := startManager(t)
	st := createStack(t, ns, map[string]any{
		"currentUpdate": map[string]any{"name": "app-u1", "commit": "bbb"},
	})
	u := createUpdate(t, ns, st)
	setStatus(t, u, map[string]any{"conditions": []any{condition("Progressing", "True")}})
	eventually(t, "running run", func() bool {
		r, ok := w.run(ns + "/app-u1")
		return ok && r.State == store.RunStateRunning
	})
	setStatus(t, u, map[string]any{"conditions": []any{
		condition("Progressing", "False"), condition("Complete", "True"),
		condition("Failed", "False")}})
	eventually(t, "succeeded run with exact commit", func() bool {
		r, ok := w.run(ns + "/app-u1")
		return ok && r.State == store.RunStateSucceeded && r.Commit == "bbb" &&
			r.CommitSource == store.CommitSourceUpdate && r.UID == string(u.GetUID()) &&
			r.StackName == "app"
	})
}

func TestUpdateWithoutOwnerSkipped(t *testing.T) {
	ns := newNamespace(t, "own")
	w := startManager(t)
	before := testutil.ToFloat64(_skipped.WithLabelValues(
		record.SkipReason(record.ErrNoStackOwner)))
	createUpdate(t, ns, nil)
	eventually(t, "skip counter", func() bool {
		return testutil.ToFloat64(_skipped.WithLabelValues("no_stack_owner")) > before
	})
	if _, ok := w.run(ns + "/app-u1"); ok {
		t.Fatal("ownerless update was recorded")
	}
}

func TestStackDeletedThenRecreated(t *testing.T) {
	ns := newNamespace(t, "del")
	w := startManager(t)
	st := createStack(t, ns, nil)
	eventually(t, "stack", func() bool { _, ok := w.stack(ns + "/app"); return ok })
	if err := _c.Delete(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deletion", func() bool { return w.deletions(ns+"/app") > 0 })
	createStack(t, ns, nil)
	eventually(t, "recreated stack", func() bool { _, ok := w.stack(ns + "/app"); return ok })
}

func TestNamespacedMode(t *testing.T) {
	watched, other := newNamespace(t, "watched"), newNamespace(t, "other")
	w := startManager(t, watched)
	createStack(t, other, nil)
	createStack(t, watched, nil)
	eventually(t, "watched stack", func() bool { _, ok := w.stack(watched + "/app"); return ok })
	never(t, "other-namespace stack", func() bool { _, ok := w.stack(other + "/app"); return ok })
}
