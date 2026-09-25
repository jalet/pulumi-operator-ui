package watch

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jalet/pulumi-operator-ui/internal/record"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// Writer errors are returned so controller-runtime requeues with its rate limiter;
// skippable mapping errors are counted and dropped so they never loop.

type stackReconciler struct {
	c   client.Reader
	w   Writer
	now func() time.Time
}

func (r *stackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := newObject(record.StackGVK)
	if err := r.c.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.w.MarkStackDeleted(ctx, req.Namespace, req.Name, r.now())
		}
		return ctrl.Result{}, fmt.Errorf("get stack: %w", err)
	}
	st, err := record.StackFromObject(obj, r.now())
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("map stack: %w", err)
	}
	if err := r.w.UpsertStack(ctx, st); err != nil {
		return ctrl.Result{}, err
	}
	run, ok, err := record.RunFromStackLastUpdate(obj, r.now())
	switch {
	case err != nil && record.SkipReason(err) != "":
		_skipped.WithLabelValues(record.SkipReason(err)).Inc()
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("map stack last update: %w", err)
	case !ok:
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.w.InsertRunIfAbsent(ctx, run)
}

type updateReconciler struct {
	c   client.Reader
	w   Writer
	now func() time.Time
}

func (r *updateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := newObject(record.UpdateGVK)
	if err := r.c.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // GC'd Update: its recorded history stays
		}
		return ctrl.Result{}, fmt.Errorf("get update: %w", err)
	}
	stackName, err := record.StackOwner(obj)
	if err != nil {
		_skipped.WithLabelValues(record.SkipReason(err)).Inc()
		return ctrl.Result{}, nil
	}
	commit, source, err := r.commitFor(ctx, req.Namespace, stackName, req.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	run, err := record.RunFromUpdate(obj, commit, source, r.now())
	if err != nil {
		if reason := record.SkipReason(err); reason != "" {
			_skipped.WithLabelValues(reason).Inc()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("map update: %w", err)
	}
	return ctrl.Result{}, r.w.UpsertRun(ctx, run)
}

func (r *updateReconciler) commitFor(ctx context.Context, ns, stackName, updateName string) (
	string, store.CommitSource, error) {
	stack := newObject(record.StackGVK)
	err := r.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: stackName}, stack)
	if apierrors.IsNotFound(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("get owning stack: %w", err)
	}
	commit, source := record.CommitFor(stack, updateName)
	return commit, source, nil
}
