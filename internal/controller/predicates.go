package controller

import (
	"encoding/json"
	"reflect"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/ngxstorage/quartet-operator/internal/renderer"
)

// Watch predicates. The global GenerationChangedPredicate
// that used to blanket-filter every watch was wrong in three ways:
//
// 1. it suppressed the main CR's deletionTimestamp/finalizer transitions
// (they do not bump spec generation) — finalizer cleanup could starve;
// 2. it suppressed owned-child metadata drift (annotation loss) because
// metadata-only updates never bump generation;
// 3. it could not be scoped per watch, so child watches either fired on
// every Lease renewal (churn) or never fired on drift.
//
// Every watch now carries its own predicate (predicateForObject), and the
// enqueuer keeps the ownership-annotation filter, so foreign objects of the
// watched kinds still drive zero reconcile requests.

// mainResourcePredicate gates the Quartet CR watch: a reconcile fires when
// the spec generation changes, when the deletionTimestamp transitions
// (deletion must always run the finalizer cleanup — it bumps no generation),
// or when the finalizer list changes (a manual finalizer edit is an explicit
// operator intervention we must observe). Status-only writes bump neither,
// which is the churn the old predicate suppressed correctly.
func mainResourcePredicate() predicate.Funcs {
	return predicate.Funcs{
		GenericFunc: func(e event.GenericEvent) bool { return true },
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObj, ok := e.ObjectOld.(client.Object)
			if !ok {
				return true
			}
			newObj, ok := e.ObjectNew.(client.Object)
			if !ok {
				return true
			}
			if !oldObj.GetDeletionTimestamp().IsZero() != !newObj.GetDeletionTimestamp().IsZero() {
				return true
			}
			if !finalizersEqual(oldObj.GetFinalizers(), newObj.GetFinalizers()) {
				return true
			}
			return oldObj.GetGeneration() != newObj.GetGeneration()
		},
	}
}

func finalizersEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ownershipChangedPredicate is the child-watch metadata gate: it fires when
// the operator ownership annotation is ADDED or REMOVED (removal is the
// drift / foreign-takeover signal) and ignores every other metadata-only
// write — including the Lease renewal churn it shares with the Lease pool.
func ownershipChangedPredicate() predicate.Funcs {
	return predicate.Funcs{
		GenericFunc: func(e event.GenericEvent) bool { return true },
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldAnn := e.ObjectOld.GetAnnotations()
			newAnn := e.ObjectNew.GetAnnotations()
			return oldAnn[renderer.OwnerUIDAnnotation] != newAnn[renderer.OwnerUIDAnnotation]
		},
	}
}

// leasePredicate gates the coordination Lease pool: only ownership-
// annotation transitions fire. Lease renewal events (spec.holderIdentity /
// renewTime churn — the driver's own steady-state traffic) must NEVER
// trigger a reconcile.
func leasePredicate() predicate.Funcs { return ownershipChangedPredicate() }

// predicateForObject selects the watch-specific predicate for a child
// operand kind:
// - Leases: ownership-change only (metadata gate) — renewal churn is
// explicitly excluded;
// - the unstructured SCC: SCC drift predicate — owner-annotation
// change OR spec/metadata drift fires, resourceVersion/managedFields
// churn does not;
// - any other unstructured operand: ownership-change only (none exist
// today; keep the conservative gate if one is added);
// - Pods: container waiting-reason transitions only — the kubelet updates
// pod status constantly (phase, restart counts, probes) and most of it
// is not a readiness signal; only CreateContainerConfigError /
// ImagePullBackOff appearing or clearing matters (bounds the event
// storm);
// - every other operand kind: any update fires. We never write
// Deployment/DaemonSet status or RBAC/CM data ourselves in a way that
// is not drift, and the enqueuer still drops events from objects that
// lack the ownership annotation, so annotation loss, spec/data drift
// and re-addition all reconcile exactly once.
func predicateForObject(obj client.Object) predicate.Funcs {
	switch o := obj.(type) {
	case *coordinationv1.Lease:
		return leasePredicate()
	case *unstructured.Unstructured:
		if o.GroupVersionKind() == renderer.SCCGVK {
			return sccDriftPredicate()
		}
		return leasePredicate()
	case *corev1.Pod:
		return podWaitingPredicate()
	default:
		return driftPredicate()
	}
}

// sccDriftPredicate gates the unstructured OpenShift SCC watch. The old
// leasePredicate chose ONLY the owner-annotation change —
// a spec/grant edit under the SAME annotation (allowPrivilegedContainer
// flipped by hand) passed the gate silently and was never repaired,
// because the controller performs no periodic requeue. The SCC has no
// renewal churn of its own (unlike the Lease pool), so ANY real drift must
// fire: spec and all metadata — owner annotation included — are compared;
// only the fields the API server rewrites on every update (resourceVersion,
// managedFields) are excluded. The enqueuer still drops events from SCCs
// lacking the ownership annotation, so foreign SCCs stay churn-free.
func sccDriftPredicate() predicate.Funcs {
	return predicate.Funcs{
		GenericFunc: func(e event.GenericEvent) bool { return true },
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldU, okOld := e.ObjectOld.(*unstructured.Unstructured)
			newU, okNew := e.ObjectNew.(*unstructured.Unstructured)
			if !okOld || !okNew {
				return true
			}
			o := oldU.DeepCopy()
			unstructured.RemoveNestedField(o.Object, "metadata", "resourceVersion")
			unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
			n := newU.DeepCopy()
			unstructured.RemoveNestedField(n.Object, "metadata", "resourceVersion")
			unstructured.RemoveNestedField(n.Object, "metadata", "managedFields")
			return !reflect.DeepEqual(o.Object, n.Object)
		},
	}
}

// podWaitingPredicate fires only when the set of container waiting reasons
// (container→reason) CHANGES: ImagePullBackOff appearing, CreateContainer-
// ConfigError clearing, a new blocked container. Plain kubelet status churn
// on healthy pods produces no reconcile.
func podWaitingPredicate() predicate.Funcs {
	waitingReasons := func(obj client.Object) string {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return ""
		}
		out := map[string]string{}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				out[cs.Name] = cs.State.Waiting.Reason
			}
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	return predicate.Funcs{
		GenericFunc: func(e event.GenericEvent) bool { return true },
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return waitingReasons(e.ObjectOld) != waitingReasons(e.ObjectNew)
		},
	}
}

// driftPredicate gates spec/data-carrying workloads (Deployment/DaemonSet/
// Pod) and the remaining operand kinds: any update fires.
func driftPredicate() predicate.Funcs {
	return predicate.Funcs{
		GenericFunc: func(e event.GenericEvent) bool { return true },
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		UpdateFunc:  func(e event.UpdateEvent) bool { return true },
	}
}

// sccWatchEnabled reports whether discovery reports the OpenShift config
// group. This is the single mechanism gating the OCP-only SCC watch: the
// SCC is cluster-scoped, unstructured, and absent on plain
// Kubernetes, so a static watch entry would need a GVK the manager scheme
// cannot verify pre-start. Chosen over a periodic requeue because the
// steady-state contract is "no requeue" (TestIdempotentSecondReconcileNoop)
// and a watch triggers only on real SCC change, not on a timer.
func sccWatchEnabled(d platformDiscovery) bool {
	groups, err := d.ServerGroups()
	if err != nil {
		// Forbidden/network: run SCC-free rather than fail setup. The
		// reconcile-side detection classifies PermissionDenied per attempt
		// (API absence vs permission must stay distinct), and the
		// missing watch only means SCC drift is caught by the next CR
		// generation change on OCP.
		if apierrors.IsForbidden(err) {
			return false
		}
		return false
	}
	for _, g := range groups.Groups {
		if g.Name == openShiftConfigGroup {
			return true
		}
	}
	return false
}

// ensureSCCUnstructuredReady registers the unstructured SCC GVK (and its
// list kind) in the manager scheme so the conditional watch resolves them
// before cache start.
func ensureSCCUnstructuredReady(scheme *runtime.Scheme) {
	sccListGVK := renderer.SCCGVK.GroupVersion().WithKind("SecurityContextConstraintsList")
	if !scheme.Recognizes(renderer.SCCGVK) {
		scheme.AddKnownTypeWithName(renderer.SCCGVK, &unstructured.Unstructured{})
	}
	if !scheme.Recognizes(sccListGVK) {
		scheme.AddKnownTypeWithName(sccListGVK, &unstructured.UnstructuredList{})
	}
}
