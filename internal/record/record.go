// Package record maps PKO Stack and Update objects to store rows. It is pure: no I/O.
//
// Objects are decoded into minimal local structs instead of importing PKO's API module,
// which would pull in the Pulumi SDK and tie this binary to one operator version.
package record

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// Operator messages are stored for display; 8 KiB keeps a pathological message from
// bloating the runs table while still showing any real error in full.
const messageBytesMax = 8 << 10

// GVKs of the PKO objects this package understands.
var (
	StackGVK  = schema.GroupVersionKind{Group: "pulumi.com", Version: "v1", Kind: "Stack"}
	UpdateGVK = schema.GroupVersionKind{Group: "auto.pulumi.com", Version: "v1alpha1",
		Kind: "Update"}
)

// Skippable mapping errors: the object is valid Kubernetes data this app does not record.
var (
	ErrNoStackOwner = errors.New("update has no Stack owner")
	ErrUnknownType  = errors.New("unknown update type")
	ErrUnknownState = errors.New("unknown lastUpdate state")
)

var _runTypes = map[string]store.RunType{
	"preview": store.RunTypePreview,
	"up":      store.RunTypeUp,
	"refresh": store.RunTypeRefresh,
	"destroy": store.RunTypeDestroy,
}

var _lastUpdateStates = map[string]store.RunState{
	"succeeded": store.RunStateSucceeded,
	"failed":    store.RunStateFailed,
}

type updateObj struct {
	Spec struct {
		Type string `json:"type"`
	} `json:"spec"`
	Status struct {
		Conditions []metav1.Condition `json:"conditions"`
		StartTime  *metav1.Time       `json:"startTime"`
		EndTime    *metav1.Time       `json:"endTime"`
		Message    string             `json:"message"`
	} `json:"status"`
}

type lastUpdate struct {
	Name                 string `json:"name"`
	Type                 string `json:"type"`
	State                string `json:"state"`
	Message              string `json:"message"`
	LastAttemptedCommit  string `json:"lastAttemptedCommit"`
	LastSuccessfulCommit string `json:"lastSuccessfulCommit"`
}

type currentUpdate struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

type stackObj struct {
	Spec struct {
		Stack   string `json:"stack"`
		Backend string `json:"backend"`
		Preview bool   `json:"preview"`
	} `json:"spec"`
	Status struct {
		ProjectInfo *struct {
			Name string `json:"name"`
		} `json:"projectInfo"`
		Conditions    []metav1.Condition `json:"conditions"`
		CurrentUpdate *currentUpdate     `json:"currentUpdate"`
		LastUpdate    *lastUpdate        `json:"lastUpdate"`
	} `json:"status"`
}

// SkipReason returns a metric label for the skippable errors, and "" for any other error.
func SkipReason(err error) string {
	switch {
	case errors.Is(err, ErrNoStackOwner):
		return "no_stack_owner"
	case errors.Is(err, ErrUnknownType):
		return "unknown_type"
	case errors.Is(err, ErrUnknownState):
		return "unknown_state"
	default:
		return ""
	}
}

// StackFromObject maps a Stack to its row.
func StackFromObject(obj *unstructured.Unstructured, now time.Time) (store.Stack, error) {
	var s stackObj
	if err := decode(obj, &s); err != nil {
		return store.Stack{}, err
	}
	st := store.Stack{
		Namespace:   obj.GetNamespace(),
		Name:        obj.GetName(),
		Ready:       meta.IsStatusConditionTrue(s.Status.Conditions, "Ready"),
		Reconciling: meta.IsStatusConditionTrue(s.Status.Conditions, "Reconciling"),
		Stalled:     meta.IsStatusConditionTrue(s.Status.Conditions, "Stalled"),
		UpdatedAt:   now,
		BackendURL:  s.Spec.Backend,
		PulumiStack: s.Spec.Stack,
		Preview:     s.Spec.Preview,
	}
	if s.Status.ProjectInfo != nil {
		st.Project = s.Status.ProjectInfo.Name
	}
	if s.Status.LastUpdate != nil {
		st.LastCommit = s.Status.LastUpdate.LastSuccessfulCommit
	}
	return st, nil
}

// CommitFor returns the commit of the named Update as the Stack reports it. When the
// Stack's currentUpdate or lastUpdate names that Update, the commit is exact and sourced
// "update". Otherwise it falls back to lastAttemptedCommit, sourced "stack", which is only
// an approximation. Both are "" when the Stack reports nothing.
func CommitFor(stack *unstructured.Unstructured, updateName string) (string, store.CommitSource) {
	var s stackObj
	if err := decode(stack, &s); err != nil {
		return "", ""
	}
	if cu := s.Status.CurrentUpdate; cu != nil && cu.Name == updateName && cu.Commit != "" {
		return cu.Commit, store.CommitSourceUpdate
	}
	lu := s.Status.LastUpdate
	if lu == nil || lu.LastAttemptedCommit == "" {
		return "", ""
	}
	if lu.Name == updateName {
		return lu.LastAttemptedCommit, store.CommitSourceUpdate
	}
	return lu.LastAttemptedCommit, store.CommitSourceStack
}

// StackOwner returns the name of the Stack that owns obj.
func StackOwner(obj *unstructured.Unstructured) (string, error) {
	for _, ref := range obj.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			continue
		}
		if ref.Kind == StackGVK.Kind && gv.Group == StackGVK.Group && ref.Name != "" {
			return ref.Name, nil
		}
	}
	return "", fmt.Errorf("%s/%s: %w", obj.GetNamespace(), obj.GetName(), ErrNoStackOwner)
}

// RunFromUpdate maps an Update to its run. commit and source come from CommitFor on the
// owning Stack; both are "" when unknown.
func RunFromUpdate(obj *unstructured.Unstructured, commit string, source store.CommitSource,
	now time.Time) (store.Run, error) {
	if obj.GroupVersionKind() != UpdateGVK {
		panic("invariant violated: RunFromUpdate called with " + obj.GroupVersionKind().String())
	}
	stack, err := StackOwner(obj)
	if err != nil {
		return store.Run{}, err
	}
	var u updateObj
	if err := decode(obj, &u); err != nil {
		return store.Run{}, err
	}
	typ, ok := _runTypes[u.Spec.Type]
	if !ok {
		return store.Run{}, fmt.Errorf("%q: %w", u.Spec.Type, ErrUnknownType)
	}
	r := store.Run{
		Namespace:  obj.GetNamespace(),
		UpdateName: obj.GetName(),
		UID:        string(obj.GetUID()),
		StackName:  stack,
		Type:       typ,
		State:      runState(u.Status.Conditions),
		Message:    truncate(u.Status.Message, messageBytesMax),
		StartedAt:  timePtr(u.Status.StartTime),
		EndedAt:    timePtr(u.Status.EndTime),
		ObservedAt: now,
	}
	if commit != "" {
		r.Commit, r.CommitSource = commit, source
	}
	return r, nil
}

// RunFromStackLastUpdate maps a Stack's lastUpdate summary to a backfill run.
// ok is false when the Stack has no lastUpdate.
func RunFromStackLastUpdate(obj *unstructured.Unstructured, now time.Time) (
	store.Run, bool, error) {
	var s stackObj
	if err := decode(obj, &s); err != nil {
		return store.Run{}, false, err
	}
	lu := s.Status.LastUpdate
	if lu == nil || lu.Name == "" {
		return store.Run{}, false, nil
	}
	typ, ok := _runTypes[lu.Type]
	if !ok {
		return store.Run{}, false, fmt.Errorf("%q: %w", lu.Type, ErrUnknownType)
	}
	state, ok := _lastUpdateStates[lu.State]
	if !ok {
		return store.Run{}, false, fmt.Errorf("%q: %w", lu.State, ErrUnknownState)
	}
	r := store.Run{
		Namespace:  obj.GetNamespace(),
		UpdateName: lu.Name,
		StackName:  obj.GetName(),
		Type:       typ,
		State:      state,
		Message:    truncate(lu.Message, messageBytesMax),
		ObservedAt: now,
	}
	// lastUpdate names this very run, so its commit is exact, as in CommitFor.
	if lu.LastAttemptedCommit != "" {
		r.Commit, r.CommitSource = lu.LastAttemptedCommit, store.CommitSourceUpdate
	}
	return r, true, nil
}

func runState(conds []metav1.Condition) store.RunState {
	complete := meta.IsStatusConditionTrue(conds, "Complete")
	switch {
	case complete && meta.IsStatusConditionTrue(conds, "Failed"):
		return store.RunStateFailed
	case complete:
		return store.RunStateSucceeded
	case meta.IsStatusConditionTrue(conds, "Progressing"):
		return store.RunStateRunning
	default:
		return store.RunStatePending
	}
}

func decode(obj *unstructured.Unstructured, into any) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, into); err != nil {
		return fmt.Errorf("decode %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(),
			obj.GetName(), err)
	}
	return nil
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
