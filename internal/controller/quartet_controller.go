// Package controller implements the Quartet singleton reconciler:
// validation, platform detection, the explicit per-profile renderers, SSA
// apply with ownership, drift/cleanup and the condition status contract.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
)

// Retryable detection errors (PermissionDenied / DependencyMissing) are
// requeued after this interval. Validation errors are terminal until the
// spec changes.
const retryableRequeueAfter = 30 * time.Second

// RBAC surface: see the package markers in api/v1alpha1/groupversion_info.go
// (controller-gen v0.16 reads kubebuilder:rbac from package comments).
// The role covers quartets(+status/finalizers), events, the manager's own
// leases, namespaces (standalone line, get;create only — the namespace
// watch was removed so no list/watch is needed), the operand kinds and pods
// read (workload-readiness probe). The four rbac.authorization.k8s.io kinds
// are READ-ONLY (get;list;watch): operand RBAC is provisioned out-of-band by
// the installer (config/operands/<profile>/) and verified by pinned name at
// reconcile start (RBACDrift gate) — the manager never
// creates, patches, deletes or binds RBAC. secrets remain deliberately
// absent.
type QuartetReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// PlatformDiscovery backs platform detection (config.openshift.io group
	// presence). Injected so unit tests run against a fake; main.go wires
	// the real client-go discovery client.
	PlatformDiscovery platformDiscovery

	// Applier is the apply mechanism. Nil (production/envtest) selects
	// real server-side apply (ssaApplier); fake-client tests inject
	// mergeApplier because the fake client rejects apply patches
	// (kubernetes/kubernetes#115598). See the applier doc in apply.go.
	Applier applier

	// DirectClient is an uncached client for safety-critical fresh reads
	// (the ownership gate and cleanup enumeration). Nil falls back to
	// Client, which is correct for the fake-client tests (already direct).
	// main.go and the envtest suite wire an uncached client here.
	DirectClient client.Client
}

// Reconcile implements the fixed reconcile ordering:
//
//	(2) singleton name + spec validation + immutability + platform detection
//	 (Secret API untouched);
//	(3) per-profile renderers produce the full desired operand sets;
//	(4-5) status: observedGeneration + aggregate Ready + profiles[].conditions,
//	 written only on meaningful change; events only on condition
//	 transitions (churn bound).
func (r *QuartetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("quartet", req.Name)
	log.V(1).Info("reconciling")

	q := &v1alpha1.Quartet{}
	if err := r.Get(ctx, req.NamespacedName, q); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get quartet")
		return ctrl.Result{}, err
	}

	if !q.DeletionTimestamp.IsZero() {
		// Finalizer cleanup — owned operands only, namespace and
		// customer data retained.
		return r.handleTermination(ctx, q)
	}

	// Step 2: validation failures are terminal — they require a spec
	// change, so they are reported once and never requeued.
	if reason, msg := r.validate(q); reason != "" {
		r.setFailedStatus(ctx, log, q, reason, msg)
		return ctrl.Result{}, nil
	}

	// Platform detection: API absence vs permission/network error are
	// distinct outcomes; an override conflict is a hard error (no silent
	// fallback).
	resolved, err := DetectPlatform(r.PlatformDiscovery, q.Spec.Platform)
	if err != nil {
		reason, retry := detectionReason(err)
		r.setFailedStatus(ctx, log, q, reason, msgWithErr(err))
		if retry {
			return ctrl.Result{RequeueAfter: retryableRequeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}

	// Ensure the cleanup finalizer BEFORE any operand mutation: a
	// deletion mid-apply must not orphan operands.
	if !controllerutil.ContainsFinalizer(q, quartetFinalizer) {
		controllerutil.AddFinalizer(q, quartetFinalizer)
		if err := r.Update(ctx, q); err != nil {
			log.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Steps 3-4: render + apply each profile (golden parity set).
	results := r.applyProfiles(ctx, q, resolved)
	// Reconcile exit is debug-level by logging contract; failures log at
	// info/warning through their own distinct messages below.
	log.V(1).Info("profiles reconciled",
		"platform", string(resolved),
		"applied", appliedOperandCounts(results),
		// Secret redaction discipline: logs and status carry the Secret
		// NAME (a spec reference) only — never values, which the operator
		// cannot read.
		"secretRefs", secretRefNames(q))

	// Step 5: status with deep-equal guard.
	oldStatus := q.Status.DeepCopy()
	desired, changed := r.buildStatus(q, resolved, results)
	if !changed {
		log.V(1).Info("status unchanged; skipping write")
		return r.retryResult(results), nil
	}
	q.Status = desired
	if err := r.Status().Update(ctx, q); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}
	// Event churn bound: one event per condition whose signature (type+
	// status+reason) changed, none when the status write was suppressed.
	r.emitTransitionEvents(q, oldStatus, &desired)
	return r.retryResult(results), nil
}

// retryResult requeues when any profile recorded a retryable failure
// (intermediate errors are retryable); terminal failures (validation,
// foreign conflict) wait for spec/watch triggers.
func (r *QuartetReconciler) retryResult(results []profileApplyResult) ctrl.Result {
	for _, res := range results {
		if res.Retryable {
			return ctrl.Result{RequeueAfter: retryableRequeueAfter}
		}
	}
	return ctrl.Result{}
}

// validate returns (reason, message) for the first failed check, or ("", "").
func (r *QuartetReconciler) validate(q *v1alpha1.Quartet) (string, string) {
	if err := q.ValidateName(); err != nil {
		return v1alpha1.ReasonInvalidSpec, err.Error()
	}
	if err := q.ValidateSpec(); err != nil {
		return v1alpha1.ReasonInvalidSpec, err.Error()
	}
	if err := q.ValidateImmutablePlatform(); err != nil {
		return v1alpha1.ReasonInvalidSpec, err.Error()
	}
	return "", ""
}

// setFailedStatus records an aggregate Ready=False with the given (slash-free)
// reason and emits an event only when the condition actually transitioned.
func (r *QuartetReconciler) setFailedStatus(ctx context.Context, log logr.Logger, q *v1alpha1.Quartet, reason, message string) {
	oldStatus := q.Status.DeepCopy()
	desired, changed := failedStatus(q, reason, message)
	if !changed {
		log.V(1).Info("failure status unchanged; skipping write", "reason", reason)
		return
	}
	q.Status = desired
	if err := r.Status().Update(ctx, q); err != nil {
		log.Error(err, "failed to update status", "reason", reason)
		return
	}
	r.emitTransitionEvents(q, oldStatus, &desired)
	log.Info("rejected spec", "reason", reason, "message", message)
}

func detectionReason(err error) (reason string, retryable bool) {
	switch {
	case errors.Is(err, ErrPlatformConflict):
		return v1alpha1.ReasonPlatformDetectionFailed, true
	case errors.Is(err, ErrDiscoveryPermission):
		return v1alpha1.ReasonPermissionDenied, true
	case errors.Is(err, ErrDiscoveryFailure):
		return v1alpha1.ReasonDependencyMissing, true
	default:
		return v1alpha1.ReasonUnsupportedPlatform, false
	}
}

func msgWithErr(err error) string { return err.Error() }

// statusEqual compares two statuses in their canonical JSON form.
// reflect.DeepEqual is deliberately NOT used: metav1.Time embeds time.Time
// with unexported internals, and DeepCopyInto normalizes via UTC() — a
// JSON-parsed status and its deep copy compare unequal on those internals
// even when they are equal on the wire, which would make the guard write on
// every reconcile.
func statusEqual(a, b v1alpha1.QuartetStatus) bool {
	aj, errA := json.Marshal(a)
	bj, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// buildStatus assembles the full status for a validated spec. The
// returned bool is the change guard: false means the current status already
// matches and nothing may be written (status is written only on meaningful
// change).
// buildStatus assembles the status from per-profile apply results. The
// returned bool is the change guard: false means the current status already
// matches and nothing may be written. Profiles that produced no
// result (disabled, supported-or-not) make no claim; a disabled profile
// whose CLEANUP failed appears Degraded (fail-safe surfacing).
func (r *QuartetReconciler) buildStatus(q *v1alpha1.Quartet, resolved v1alpha1.Platform, results []profileApplyResult) (v1alpha1.QuartetStatus, bool) {
	desired := q.Status.DeepCopy()
	desired.ObservedGeneration = q.Generation
	desired.SpecPlatform = q.Spec.Platform
	desired.ResolvedPlatform = resolved

	profiles := map[string]v1alpha1.ProfileStatus{}
	var firstFailure *profileApplyResult
	totalApplied := 0
	for i := range results {
		res := results[i]
		totalApplied += res.Applied
		ps := v1alpha1.ProfileStatus{}
		// Seed from the existing profile conditions so SetStatusCondition
		// PRESERVES lastTransitionTime on no-op reconciles. Rebuilding from an
		// empty slice would bump every timestamp each second and defeat the
		// status change guard (churn bound).
		if existing, ok := desired.Profiles[res.Profile]; ok {
			ps.Conditions = append([]metav1.Condition(nil), existing.Conditions...)
		}
		if res.OK() {
			setCondition(&ps.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReady, res.Message)
			setCondition(&ps.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonReady, "operands converged")
			setCondition(&ps.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonReady, "no failures observed")
		} else {
			setCondition(&ps.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, res.Reason, res.Message)
			setCondition(&ps.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, res.Reason, "blocked until the Ready condition clears")
			setCondition(&ps.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, res.Reason, res.Message)
		}
		profiles[res.Profile] = ps
		if !res.OK() && firstFailure == nil {
			firstFailure = &results[i]
		}
	}
	if len(profiles) == 0 {
		profiles = nil
	}
	desired.Profiles = profiles

	if firstFailure != nil {
		summary := fmt.Sprintf("profile %s blocked: %s", firstFailure.Profile, firstFailure.Message)
		setCondition(&desired.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, firstFailure.Reason, summary)
		setCondition(&desired.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, firstFailure.Reason, summary)
		setCondition(&desired.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, firstFailure.Reason, summary)
	} else if totalApplied > 0 {
		// Pod Ready != backend I/O proof. The Ready bar asserts
		// operand apply + ownership AND workload convergence (Deployment/
		// DaemonSet rollout + no blocked pods) — still explicitly not
		// backend I/O readiness.
		setCondition(&desired.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReady,
			fmt.Sprintf("singleton validated; platform=%s; %d operands applied and owned; workloads converged (not a backend I/O proof)", resolved, totalApplied))
		setCondition(&desired.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonReady, "nothing in progress")
		setCondition(&desired.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonReady, "no failures observed")
	} else {
		setCondition(&desired.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonReady,
			fmt.Sprintf("singleton validated; platform=%s; no enabled profile renders operands", resolved))
		setCondition(&desired.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonReady, "nothing in progress")
		setCondition(&desired.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonReady, "no failures observed")
	}

	return *desired, !statusEqual(q.Status, *desired)
}

// failedStatus assembles a Ready=False status for validation/detection
// failures. Profiles are cleared: an unvalidated spec must not keep stale
// per-profile claims.
func failedStatus(q *v1alpha1.Quartet, reason, message string) (v1alpha1.QuartetStatus, bool) {
	desired := q.Status.DeepCopy()
	desired.ObservedGeneration = q.Generation
	// SpecPlatform is intentionally NOT advanced on failure: an immutable
	// value is only accepted when it passes validation.
	desired.Profiles = nil
	setCondition(&desired.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	setCondition(&desired.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, reason, "no progress until the spec is fixed")
	setCondition(&desired.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, reason, message)
	return *desired, !statusEqual(q.Status, *desired)
}

// conditionKey is the event-dedup signature: type+status+reason. A Warning
// event fires only when this signature is absent from the previous status.
func conditionKey(c metav1.Condition) string {
	return string(c.Type) + "|" + string(c.Status) + "|" + c.Reason
}

// emitTransitionEvents converts condition transitions into k8s events.
// Called only when the change guard allowed a status write, which by
// construction bounds event churn: steady-state reconciles emit nothing.
// Aggregate Ready plus each profile's Ready/Degraded transition emits once
// per distinct signature (event dedupe).
func (r *QuartetReconciler) emitTransitionEvents(q *v1alpha1.Quartet, oldStatus, newStatus *v1alpha1.QuartetStatus) {
	seen := map[string]bool{}
	for _, c := range oldStatus.Conditions {
		seen[conditionKey(c)] = true
	}
	for prof, oldPS := range oldStatus.Profiles {
		for _, c := range oldPS.Conditions {
			seen[prof+"|"+conditionKey(c)] = true
		}
	}
	for _, c := range newStatus.Conditions {
		if c.Type != v1alpha1.ConditionReady {
			continue
		}
		if seen[conditionKey(c)] {
			continue
		}
		r.emitEvent(q, c)
	}
	for prof, ps := range newStatus.Profiles {
		for _, c := range ps.Conditions {
			// Degraded emits only when it turns TRUE (a failure); the
			// Degraded=False steady state must not produce Warning events.
			if c.Type == v1alpha1.ConditionDegraded && c.Status != metav1.ConditionTrue {
				continue
			}
			if c.Type != v1alpha1.ConditionReady && c.Type != v1alpha1.ConditionDegraded {
				continue
			}
			key := prof + "|" + conditionKey(c)
			if seen[key] {
				continue
			}
			scoped := c
			scoped.Message = fmt.Sprintf("profile %s: %s", prof, c.Message)
			r.emitEvent(q, scoped)
		}
	}
}

func (r *QuartetReconciler) emitEvent(q *v1alpha1.Quartet, c metav1.Condition) {
	// k8s events cap reason at 1024 and message at 1024; our messages are
	// bounded by operand counts, but keep the helper the single emit point.
	if c.Status == metav1.ConditionTrue && c.Type == v1alpha1.ConditionReady {
		r.Recorder.Event(q, corev1.EventTypeNormal, c.Reason, c.Message)
	} else {
		r.Recorder.Event(q, corev1.EventTypeWarning, c.Reason, c.Message)
	}
}

func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:    condType,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

func appliedOperandCounts(results []profileApplyResult) string {
	if len(results) == 0 {
		return "none"
	}
	out := make([]string, 0, len(results))
	for _, res := range results {
		out = append(out, fmt.Sprintf("%s=%d", res.Profile, res.Applied))
	}
	return strings.Join(out, ",")
}

func secretRefNames(q *v1alpha1.Quartet) []string {
	var out []string
	for name, spec := range q.ProfileSpecs() {
		if spec.Enabled && spec.SecretRef != nil {
			out = append(out, fmt.Sprintf("%s:%s", name, spec.SecretRef.Name))
		}
	}
	return out
}

// SetupWithManager registers the controller. MaxConcurrentReconciles=1
// serializes the singleton's own reconciles. The main CR watch carries
// mainResourcePredicate (generation + deletionTimestamp + finalizer
// transitions); every child watch carries its own
// predicate via predicateForObject, and the shared enqueuer keeps the
// ownership-annotation filter (foreign objects of the watched kinds drive
// zero reconcile requests). The four installer-owned RBAC kinds carry a
// SEPARATE read-only watch with a fixed profile/kind/name enqueuer filter
// (watchedInstallRBAC) — drift or recovery of the
// install set triggers the RBACDrift health gate at reconcile start.
//
// The OCP-only SCC watch is added CONDITIONALLY from API
// discovery, exactly like platform detection: a plain-Kubernetes
// manager never lists/watches security.openshift.io at all, so its RBAC
// need not and does not include that group on the watch path. SCC drift
// is covered by the watch itself — NOT by a periodic requeue — so the
// steady-state "no requeue" contract holds on both platforms.
func (r *QuartetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("quartet-controller")
	}
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Quartet{}, builder.WithPredicates(mainResourcePredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1})

	if r.PlatformDiscovery != nil && sccWatchEnabled(r.PlatformDiscovery) {
		ensureSCCUnstructuredReady(mgr.GetScheme())
		scc := &unstructured.Unstructured{}
		scc.SetGroupVersionKind(renderer.SCCGVK)
		bldr = bldr.Watches(scc, handler.EnqueueRequestsFromMapFunc(r.operandEnqueuer),
			builder.WithPredicates(predicateForObject(scc)))
	}

	for _, obj := range watchedOperands() {
		bldr = bldr.Watches(obj, handler.EnqueueRequestsFromMapFunc(r.operandEnqueuer),
			builder.WithPredicates(predicateForObject(obj)))
	}

	// Install-owned RBAC drift trigger: the four RBAC
	// kinds rejoin the watch set — READ-ONLY, with a fixed profile/kind/name
	// filter (installRBACEnqueuer), so foreign RBAC objects stay churn-free
	// and no RBAC mutation capability is granted or exercised.
	for _, obj := range watchedInstallRBAC() {
		bldr = bldr.Watches(obj, handler.EnqueueRequestsFromMapFunc(r.installRBACEnqueuer),
			builder.WithPredicates(driftPredicate()))
	}
	return bldr.Complete(r)
}

// watchedOperands is the owned-child watch set. Namespace is intentionally
// ABSENT: controller-runtime's cache needs list/watch to
// serve a watch, and the manager's RBAC grants namespaces get;create only.
// Namespace existence/creation is a one-shot check against the direct
// (uncached) client in ensureNamespace; a namespace never changes in a way
// the reconciler must react to (it is never owned or deleted).
// Pods ARE watched: pod container-status transitions (CreateContainerConfigError,
// ImagePullBackOff) are the readiness signal of the workload gate.
// The four RBAC kinds are ABSENT from the OWNED-operand set: operand RBAC
// is installer-owned out-of-band and carries no ownership annotation, so
// the ownership-filtered operandEnqueuer would drop every event of it.
// They ARE watched — separately, read-only — by watchedInstallRBAC: the
// health gate at reconcile
// start (RBACDrift) catches drift that predates a reconcile, and the watch
// catches drift that lands AFTER one.
func watchedOperands() []client.Object {
	return []client.Object{
		&corev1.ServiceAccount{},
		&corev1.ConfigMap{},
		&coordinationv1.Lease{},
		&corev1.Service{},
		&corev1.Pod{},
		&appsv1.Deployment{},
		&appsv1.DaemonSet{},
		&storagev1.CSIDriver{},
	}
}

// operandEnqueuer maps an owned operand event to the singleton CR. Only
// objects carrying the operator ownership annotation enqueue (foreign
// objects of the same kind must not drive reconcile churn). Pods are an
// exception: the operator never stamps pods (the kubelet creates them), so
// pod events in the operand namespaces are reconciled whenever their
// namespace is one this operator manages.
func (r *QuartetReconciler) operandEnqueuer(ctx context.Context, obj client.Object) []reconcile.Request {
	if _, isPod := obj.(*corev1.Pod); isPod {
		if isOperandNamespace(obj.GetNamespace()) {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}}}
		}
		return nil
	}
	if uid, owned := obj.GetAnnotations()[renderer.OwnerUIDAnnotation]; owned && uid != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}}}
	}
	return nil
}

// isOperandNamespace reports whether a namespace follows the per-profile
// operand naming ("ngxstorage-<profile>"). Pods elsewhere must not
// drive singleton churn.
func isOperandNamespace(ns string) bool {
	return strings.HasPrefix(ns, "ngxstorage-")
}

// --- Install-owned RBAC watch. ---
//
// verifyOperandRBAC runs only at reconcile start (applyProfile), and a
// healthy steady-state reconcile returns an empty Result — so a rules/
// subject edit or a delete of an installer-provisioned RBAC object left
// status Ready stale until some unrelated CR/child event happened to fire.
// The fix is a READ-ONLY watch on the four RBAC kinds restricted to the
// FIXED install set (profile/kind/name pins below): the manager's
// get;list;watch grant (the existing out-of-band RBAC model) authorizes
// exactly this informer, the enqueuer turns ONLY pinned operand events
// into a singleton reconcile, and no RBAC mutation is added — runtime
// RBAC stays install-owned, verification-only.
// Churn is structurally bounded: the install set is static (admin-applied,
// never operator-written), so the watch fires only on real drift or
// recovery; periodic requeue is deliberately not used (the controller has
// no other steady-state requeue).

// watchedInstallRBAC is the four installer-owned RBAC kinds. Registration is
// unconditional: the kinds exist on both platforms (the OCP superset set
// adds objects, not kinds).
func watchedInstallRBAC() []client.Object {
	return []client.Object{
		&rbacv1.Role{},
		&rbacv1.RoleBinding{},
		&rbacv1.ClusterRole{},
		&rbacv1.ClusterRoleBinding{},
	}
}

// installRBACNameSet is the fixed (kind → pinned names) UNION filter over
// every rendered profile — the union of renderer.OperandRBACNames,
// precomputed once. It is the pin for the CLUSTER-SCOPED kinds only:
// namespaced kinds are pinned on the full (kind, name, namespace) tuple
// per profile in isInstallOwnedRBAC (a union match in another profile's
// namespace is not install-owned).
var installRBACNameSet = buildInstallRBACNameSet()

func buildInstallRBACNameSet() map[string]map[string]bool {
	out := map[string]map[string]bool{
		"Role": {}, "RoleBinding": {}, "ClusterRole": {}, "ClusterRoleBinding": {},
	}
	for _, p := range renderer.ProfileNames() {
		n := renderer.OperandRBACNames(p)
		add := func(kind string, names []string) {
			for _, name := range names {
				out[kind][name] = true
			}
		}
		add("Role", n.Roles)
		add("RoleBinding", n.RoleBindings)
		add("ClusterRole", n.ClusterRoles)
		add("ClusterRoleBinding", n.ClusterRoleBindings)
	}
	return out
}

// installRBACEnqueuer maps one RBAC event to the singleton reconcile ONLY
// when the object is part of the install set: the pinned name, and — for
// the namespaced kinds — the pinned operand namespace of the profile that
// owns that name. Foreign RBAC objects (same name elsewhere, same kind
// elsewhere) drive zero reconcile requests, exactly like operandEnqueuer.
func (r *QuartetReconciler) installRBACEnqueuer(ctx context.Context, obj client.Object) []reconcile.Request {
	if !isInstallOwnedRBAC(obj) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}}}
}

func isInstallOwnedRBAC(obj client.Object) bool {
	// Kind from the TYPE, not the object's TypeMeta: informer-delivered
	// events carry typed objects with zeroed TypeMeta, so GVK().Kind is
	// empty for every real watch event.
	var kind string
	switch obj.(type) {
	case *rbacv1.Role:
		kind = "Role"
	case *rbacv1.RoleBinding:
		kind = "RoleBinding"
	case *rbacv1.ClusterRole:
		kind = "ClusterRole"
	case *rbacv1.ClusterRoleBinding:
		kind = "ClusterRoleBinding"
	default:
		return false
	}
	if kind == "Role" || kind == "RoleBinding" {
		// Namespaced kinds are pinned per-profile on the FULL tuple
		// (kind, name, namespace): the name must belong to the SAME
		// profile's install set as the namespace it sits in. The old
		// two-leg check (name in the union set, namespace in ANY profile's)
		// let a Role named for the NFS install set planted in the FC
		// namespace — or vice versa — enqueue a reconcile that no profile
		// actually owns.
		for _, p := range renderer.ProfileNames() {
			names := renderer.OperandRBACNames(p)
			pinned := names.Roles
			if kind == "RoleBinding" {
				pinned = names.RoleBindings
			}
			for _, name := range pinned {
				if name == obj.GetName() && obj.GetNamespace() == renderer.OperandNamespace(p) {
					return true
				}
			}
		}
		return false
	}
	// Cluster-scoped kinds have no namespace to bind: the union of pinned
	// names is the pin.
	return installRBACNameSet[kind][obj.GetName()]
}
