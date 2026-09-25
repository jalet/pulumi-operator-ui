// Package watch runs controller-runtime reconcilers that record Stacks and Updates.
package watch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/jalet/pulumi-operator-ui/internal/record"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

var (
	_skipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pou_record_skipped_total",
		Help: "Objects seen but not recorded, by reason.",
	}, []string{"reason"})
	_registerOnce sync.Once
)

// Writer is the subset of the store the reconcilers need.
type Writer interface {
	UpsertStack(ctx context.Context, s store.Stack) error
	MarkStackDeleted(ctx context.Context, namespace, name string, at time.Time) error
	UpsertRun(ctx context.Context, r store.Run) error
	BackfillRun(ctx context.Context, r store.Run) error
	SweepStacks(ctx context.Context, present []store.StackKey, cutoff, at time.Time) (int64, error)
}

// Options configures NewManager.
type Options struct {
	Namespaces  []string // empty = all
	MetricsAddr string   // "0" disables the metrics listener
	Writer      Writer
	Now         func() time.Time
	Logger      logr.Logger // zero value = the global controller-runtime logger

	// skipNameValidation lets tests start several managers in one process.
	skipNameValidation bool
}

// NewManager builds a manager with the Stack and Update reconcilers registered.
// Leader election is off: the design assumes a single replica.
func NewManager(cfg *rest.Config, o Options) (manager.Manager, error) {
	if o.Writer == nil || o.Now == nil {
		panic("invariant violated: watch options need Writer and Now")
	}
	_registerOnce.Do(func() { ctrlmetrics.Registry.MustRegister(_skipped) })

	cacheOpts := cache.Options{}
	if len(o.Namespaces) > 0 {
		cacheOpts.DefaultNamespaces = make(map[string]cache.Config, len(o.Namespaces))
		for _, ns := range o.Namespaces {
			cacheOpts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Cache: cacheOpts,
		// Reads of unstructured objects go through the cache too, so the app only needs
		// list/watch and never issues per-reconcile GETs against the API server.
		Client:                 client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Metrics:                metricsserver.Options{BindAddress: o.MetricsAddr},
		HealthProbeBindAddress: "0", // probes are served by internal/web
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &o.skipNameValidation},
		Logger:                 o.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("new manager: %w", err)
	}
	stacks := &stackReconciler{c: mgr.GetClient(), w: o.Writer, now: o.Now}
	if err := ctrl.NewControllerManagedBy(mgr).Named("stack").
		For(newObject(record.StackGVK)).Complete(stacks); err != nil {
		return nil, fmt.Errorf("stack controller: %w", err)
	}
	updates := &updateReconciler{c: mgr.GetClient(), w: o.Writer, now: o.Now}
	if err := ctrl.NewControllerManagedBy(mgr).Named("update").
		For(newObject(record.UpdateGVK)).Complete(updates); err != nil {
		return nil, fmt.Errorf("update controller: %w", err)
	}
	// Stacks deleted while the app was down produce no event after a restart; sweep them
	// once the cache has listed what exists.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		sweepDeleted(ctx, mgr.GetCache(), o.Writer, o.Now)
		return nil
	})); err != nil {
		return nil, fmt.Errorf("add stack sweep: %w", err)
	}
	return mgr, nil
}

// sweepDeleted soft-deletes recorded stacks the watch cannot see. It runs once per start;
// failures are logged, because a missed sweep only leaves stale rows until the next start.
func sweepDeleted(ctx context.Context, c cache.Cache, w Writer, now func() time.Time) {
	log := ctrl.LoggerFrom(ctx).WithName("sweep")
	cutoff := now() // before listing, so rows a racing reconcile writes are kept
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(record.StackGVK.GroupVersion().WithKind("StackList"))
	if err := c.List(ctx, list); err != nil {
		log.Error(err, "list stacks")
		return
	}
	present := make([]store.StackKey, 0, len(list.Items))
	for _, item := range list.Items {
		present = append(present, store.StackKey{Namespace: item.GetNamespace(), Name: item.GetName()})
	}
	n, err := w.SweepStacks(ctx, present, cutoff, now())
	if err != nil {
		log.Error(err, "sweep stacks")
		return
	}
	log.Info("swept stacks not found in the cluster", "count", n, "present", len(present))
}

func newObject(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return obj
}
