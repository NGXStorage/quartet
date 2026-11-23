package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-logr/logr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
)

// quartetFinalizer guards operand cleanup on CR deletion (a cleanup error
// must NOT silently remove the finalizer).
const quartetFinalizer = "quartet.ngxstorage.com/quartet"

// profileApplyResult is the per-profile outcome of one reconcile pass.
// Profile isolation: a failure is recorded for exactly one profile and must
// not rewrite another profile's spec or resources.
type profileApplyResult struct {
	Profile string
	// Applied is the operand count applied in this pass (0 for failures).
	Applied   int
	Reason    string // condition reason when !OK()
	Message   string
	Retryable bool
}

func (r profileApplyResult) OK() bool { return r.Reason == "" }

// inputFor projects one spec profile into the renderer input. ValidateSpec
// guarantees SecretRef/Image are non-nil for enabled profiles, so nil guards
// here only keep disabled/unsupported profiles from panicking the renderer.
func inputFor(name string, spec *v1alpha1.ProfileSpec, q *v1alpha1.Quartet, resolved v1alpha1.Platform) renderer.ProfileInput {
	in := renderer.ProfileInput{Profile: name, LogLevel: string(q.Spec.LogLevel), Platform: resolved}
	if spec.SecretRef != nil {
		in.SecretName = spec.SecretRef.Name
	}
	if spec.Image != nil {
		in.DriverImage = spec.Image.Reference
	}
	return in
}

// applyProfiles renders and applies (or cleans up) every profile in the
// fixed, deterministic order. All three profiles have real renderers; the
// nil-renderer guard remains fail-closed — an enabled nil-renderer profile
// is reported RenderError, never Ready (never presented as supported).
func (r *QuartetReconciler) applyProfiles(ctx context.Context, q *v1alpha1.Quartet, resolved v1alpha1.Platform) []profileApplyResult {
	results := []profileApplyResult{}
	for _, name := range renderer.ProfileNames() {
		spec := q.ProfileSpecs()[name]
		profile := renderer.For(inputFor(name, spec, q, resolved))
		if profile == nil {
			if spec.Enabled {
				results = append(results, profileApplyResult{
					Profile: name,
					Reason:  v1alpha1.ReasonRenderError,
					Message: fmt.Sprintf("profile %s has no renderer in this operator build; reported unsupported, not as disabled or Ready", name),
				})
			}
			continue
		}
		if !spec.Enabled {
			// Ownership evidence BEFORE the gate: a closed
			// profile this CR never managed — the API shows no operand of
			// its carrying this CR's owner-uid annotation — is a no-op. It
			// must not run the usage gate (a legacy Bound PV of that driver
			// from another installation would wrongly make the aggregate
			// DataInUse) and produces no status claim. Status is not the
			// evidence: the CR may have restarted with cleared status while
			// its operands still exist, and the read above covers that.
			// API error fails closed to managed (gate + cleanup still run).
			managed, ownerErr := r.profileHasOwnedOperands(ctx, q, profile)
			if ownerErr != nil {
				r.Log.Info("ownership evidence unavailable for disabled profile — failing closed to managed (usage gate + cleanup still run)",
					"profile", name, "error", ownerErr.Error())
				managed = true
			}
			if managed {
				// Active-usage gate: a disable must not tear down the
				// driver while bound/in-use storage is present for the
				// profile's provisioner identity. Blocked unless the
				// profile carries the explicit forceDisable override
				// (draft field). Retryable so the gate re-checks on the
				// requeue interval — usage may clear, or the admin may set
				// the override.
				if usage, gateErr := r.checkActiveUsage(ctx, spec.ForceDisable, name); gateErr != nil || usage != "" {
					reason, msg := usageGateReasonMessage(name, profile.Namespace(), usage, gateErr)
					results = append(results, profileApplyResult{
						Profile:   name,
						Reason:    reason,
						Message:   msg,
						Retryable: true,
					})
					continue
				}
				// Disable removes this profile's operands; customer
				// Secret/PV/PVC/backend/namespace are never touched.
				if err := r.deleteOwnedProfile(ctx, q, profile, r.Log.WithValues("profile", name)); err != nil {
					results = append(results, profileApplyResult{
						Profile:   name,
						Reason:    v1alpha1.ReasonCleanupFailed,
						Message:   err.Error(),
						Retryable: true,
					})
				}
			}
			continue
		}
		results = append(results, r.applyProfile(ctx, q, profile))
	}
	return results
}

// applyProfile ensures the operand namespace, verifies the installer-
// provisioned RBAC set (out-of-band model), then applies
// every desired NON-RBAC object in order, stopping at the first failure
// (partial apply is recorded, not rolled back — partial resources are
// safely re-reconciled).
func (r *QuartetReconciler) applyProfile(ctx context.Context, q *v1alpha1.Quartet, profile renderer.Profile) profileApplyResult {
	ownerUID := string(q.UID)
	res := profileApplyResult{Profile: profile.Name()}

	if reason, msg := r.ensureNamespace(ctx, profile.Namespace(), ownerUID); reason != "" {
		res.Reason, res.Message, res.Retryable = reason, msg, reason != v1alpha1.ReasonForeignResourceConflict
		return res
	}

	// Reconcile start, before any operand write: the operand RBAC set is
	// installer-provisioned (config/operands/<profile>/), and the manager
	// holds read-only RBAC — so drift gates the whole profile (Degraded +
	// RBACDrift, retryable) instead of being repaired by us.
	if reason, msg := r.verifyOperandRBAC(ctx, profile); reason != "" {
		res.Reason, res.Message, res.Retryable = reason, msg, true
		return res
	}

	objs := runtimeApplyObjects(profile)
	applier := r.applierOrDefault()
	for _, obj := range objs {
		if err := applier.applyOwned(ctx, obj, ownerUID); err != nil {
			var fc *foreignConflict
			if errors.As(err, &fc) {
				res.Reason = v1alpha1.ReasonForeignResourceConflict
				res.Message = fc.Error()
				res.Retryable = false
				return res
			}
			reason, retryable := classifyApplyError(err)
			res.Reason, res.Retryable = reason, retryable
			res.Message = fmt.Sprintf("apply %s failed: %s", kindName(obj), err.Error())
			return res
		}
		res.Applied++
	}

	// Operand APPLY is not readiness. After the set
	// converged, verify the rendered workloads actually rolled out —
	// a missing Secret key (CreateContainerConfigError), a broken image
	// (ImagePullBackOff) or a stalled rollout all keep the profile
	// WorkloadNotReady. Transient by nature: requeue until the rollout
	// settles (retryable vs terminal classification).
	ready, reason, msg := r.probeWorkloadReadiness(ctx, profile)
	res.Applied = len(objs)
	if !ready {
		res.Reason, res.Message, res.Retryable = reason, msg, true
		return res
	}
	res.Message = fmt.Sprintf("all %d operands applied and owned by field manager %q; %s",
		len(objs), renderer.FieldManager, msg)
	return res
}

// runtimeApplyObjects is the reconcile-time apply set: the renderer's full
// desired set MINUS the four RBAC kinds (out-of-band provisioning). The RBAC
// kinds stay in Profile.Objects() for the golden parity fixtures and feed
// InstallRBAC(); the installer — not the reconciler — creates them.
func runtimeApplyObjects(profile renderer.Profile) []client.Object {
	objs := profile.Objects()
	out := make([]client.Object, 0, len(objs))
	for _, obj := range objs {
		if isRBACKind(obj) {
			continue
		}
		out = append(out, obj)
	}
	return out
}

func isRBACKind(obj client.Object) bool {
	switch obj.(type) {
	case *rbacv1.Role, *rbacv1.RoleBinding, *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding:
		return true
	}
	return false
}

// verifyOperandRBAC runs the RBAC health gate at reconcile start. The
// manager holds ONLY get/list/watch on the four RBAC kinds
// — no resourceNames pin could ever constrain a role's CONTENT, and
// create/escalate on the pinned names was an open privilege-escalation
// chain (delete pinned name → recreate same name privileged → bind through
// the pinned binding), so operand RBAC is provisioned out-of-band by the
// installer from config/operands/<profile>/ and the reconciler only VERIFY
// here. For every expected object (renderer.InstallRBAC for this profile's
// resolved platform) it GETs by pinned name and compares:
//
// - missing object → drift (initial provisioning is the admin/install
// step; a fresh cluster legitimately has none yet);
// - Role/ClusterRole: rules must equal the rendered rules exactly;
// - (Cluster)RoleBinding: roleRef and subjects must equal the rendered
// values exactly.
//
// Any drift returns ("", "")'s negation — (RBACDrift, message) — and the
// caller gates operand apply behind it (Degraded + retryable requeue): the
// message names the drifted objects and tells the admin to re-apply the
// install manifest. Non-NotFound API errors are classified through
// classifyApplyError (PermissionDenied/DependencyMissing, retryable) — a
// read the manager's role.yaml must authorize. Returns ("", "") when the
// whole set is healthy.
func (r *QuartetReconciler) verifyOperandRBAC(ctx context.Context, profile renderer.Profile) (reason, message string) {
	fresh := r.freshClient()
	var drift []string
	for _, expected := range profile.InstallRBAC() {
		got := expected.DeepCopyObject().(client.Object)
		if err := fresh.Get(ctx, client.ObjectKeyFromObject(expected), got); err != nil {
			if apierrors.IsNotFound(err) {
				drift = append(drift, fmt.Sprintf("%s %q is not installed", kindName(expected), expected.GetName()))
				continue
			}
			reason, _ := classifyApplyError(err)
			return reason, fmt.Sprintf("profile %s: cannot verify the installer-provisioned operand RBAC (%s %q): %v — the operator holds read-only RBAC and will not create or repair it; apply the install manifest config/operands/%s/",
				profile.Name(), kindName(expected), expected.GetName(), err, profile.Name())
		}
		if diff := rbacDriftDetail(expected, got); diff != "" {
			drift = append(drift, fmt.Sprintf("%s %q drifted: %s", kindName(expected), expected.GetName(), diff))
		}
	}
	if len(drift) == 0 {
		return "", ""
	}
	return v1alpha1.ReasonRBACDrift, fmt.Sprintf("profile %s: operand RBAC is installer-provisioned and the cluster state drifted from the rendered set (%s) — re-apply the install manifest config/operands/%s/ with admin privileges; operands are NOT created or modified until the set verifies healthy",
		profile.Name(), strings.Join(drift, "; "), profile.Name())
}

// rbacDriftDetail reports how one stored RBAC object diverges from the
// rendered expectation ("" = healthy). Roles/ClusterRoles are compared on
// rules; bindings on roleRef + subjects — the exact fields a wrong install
// (or a tampered one) would change.
func rbacDriftDetail(expected, got client.Object) string {
	switch e := expected.(type) {
	case *rbacv1.Role:
		g, ok := got.(*rbacv1.Role)
		if !ok || !reflect.DeepEqual(e.Rules, g.Rules) {
			return "rules differ from the rendered set"
		}
		return ""
	case *rbacv1.ClusterRole:
		g, ok := got.(*rbacv1.ClusterRole)
		if !ok || !reflect.DeepEqual(e.Rules, g.Rules) {
			return "rules differ from the rendered set"
		}
		return ""
	case *rbacv1.RoleBinding:
		g, ok := got.(*rbacv1.RoleBinding)
		if !ok || !reflect.DeepEqual(e.RoleRef, g.RoleRef) {
			return "roleRef differs from the rendered set"
		}
		if !reflect.DeepEqual(e.Subjects, g.Subjects) {
			return "binding subjects differ from the rendered set"
		}
		return ""
	case *rbacv1.ClusterRoleBinding:
		g, ok := got.(*rbacv1.ClusterRoleBinding)
		if !ok || !reflect.DeepEqual(e.RoleRef, g.RoleRef) {
			return "roleRef differs from the rendered set"
		}
		if !reflect.DeepEqual(e.Subjects, g.Subjects) {
			return "binding subjects differ from the rendered set"
		}
		return ""
	default:
		return "unexpected RBAC kind"
	}
}

// probeWorkloadReadiness verifies the rendered workloads actually converged.
// Returns (ready, reason, message); ready=true's message is
// a human summary of the satisfied state.
//
// The check is deliberately PROFILE-LEVEL, not a pod-by-pod claim:
//
// - controller Deployment: observedGeneration == generation AND
// updatedNumberScheduled == replicas AND availableReplicas == replicas;
// - node DaemonSet: observedGeneration == generation AND
// updatedNumberScheduled == desiredNumberScheduled AND
// numberAvailable == desiredNumberScheduled. A zero-target-node DS
// (desiredNumberScheduled==0, fresh OCP node pool not yet labeled,
// all nodes cordoned) is NOT ready: no node runs the driver, so
// the profile surfaces WorkloadNotReady with the distinct
// NoTargetNodes marker and stays retryable until nodes appear;
// - pods in the operand namespace that are OWNED by one of these
// workloads (owner-UID filter — unrelated pods cannot affect the
// gate): any containerStatuses waitingReason surfaces WorkloadNotReady.
// Ownership is the ownerRef UID chain (pod → ReplicaSet → Deployment,
// or pod → DaemonSet), verified in deploymentReplicaSetUIDs /
// podOwnedByWorkload: the selector labels are IMMUTABLE across a
// Deployment rollout, so they cannot distinguish an old ReplicaSet's
// pod (ours, during a rolling update) from a foreign pod planted with
// the same labels. CreateContainerConfigError maps to itself
// as the reason (the operator does NOT read Secrets — the
// MissingSecret/MissingSecretKey distinction is unproven, so no promise
// of which key or secret is missing); ImagePullBackOff/ErrImagePull map
// to ImagePullFailed.
//
// Status alone is not backend I/O proof; a Ready profile here means
// "rendered and rolled out", verified against API-reported state.
// envtest cannot run kubelets, so its workloads stay unready until their
// statuses are set — the envtest readiness-transition tests simulate
// exactly that (live readiness is a separate acceptance layer).
//
// The workload NAMES are consumed from the profile's renderer
// (Profile.WorkloadTargets — pinned by the cross-worker interface
// contract): each profile probes ITS OWN controller Deployment and node
// DaemonSet in its own operand namespace. Hardcoding the NFS names here
// made FC/iSCSI unreachable for readiness even with healthy workloads —
// and, symmetrically, a ready NFS-named workload planted in another
// profile's namespace would have satisfied it.
func (r *QuartetReconciler) probeWorkloadReadiness(ctx context.Context, profile renderer.Profile) (bool, string, string) {
	fresh := r.freshClient()
	ns := profile.Namespace()
	depName, dsName := profile.WorkloadTargets()

	dep := &appsv1.Deployment{}
	if err := fresh.Get(ctx, types.NamespacedName{Name: depName, Namespace: ns}, dep); err != nil {
		return false, v1alpha1.ReasonWorkloadNotReady,
			fmt.Sprintf("controller Deployment not found during readiness probe: %v", err)
	}
	if dep.Status.ObservedGeneration != dep.Generation {
		return false, v1alpha1.ReasonWorkloadNotReady,
			fmt.Sprintf("controller Deployment rollout not observed (observedGeneration=%d generation=%d)", dep.Status.ObservedGeneration, dep.Generation)
	}
	want := *dep.Spec.Replicas
	if dep.Status.AvailableReplicas != want || dep.Status.UpdatedReplicas != want {
		return false, v1alpha1.ReasonWorkloadNotReady,
			fmt.Sprintf("controller Deployment not ready (available=%d updated=%d want=%d)",
				dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas, want)
	}

	ds := &appsv1.DaemonSet{}
	if err := fresh.Get(ctx, types.NamespacedName{Name: dsName, Namespace: ns}, ds); err != nil {
		return false, v1alpha1.ReasonWorkloadNotReady,
			fmt.Sprintf("node DaemonSet not found during readiness probe: %v", err)
	}
	if ds.Status.ObservedGeneration != ds.Generation {
		return false, v1alpha1.ReasonWorkloadNotReady,
			fmt.Sprintf("node DaemonSet rollout not observed (observedGeneration=%d generation=%d)", ds.Status.ObservedGeneration, ds.Generation)
	}
	if ds.Status.DesiredNumberScheduled == 0 {
		// Zero target nodes is NOT readiness. No node
		// runs the driver, so the profile cannot serve a single volume —
		// the earlier early-true ("explicit open state") acceptance is
		// removed. Distinct NoTargetNodes marker in the message; retryable
		// because nodes may be labeled later. (Treating a zero-node open
		// state as ready would be a product decision; not done here.)
		return false, v1alpha1.ReasonWorkloadNotReady,
			"controller Deployment ready; node DaemonSet reports NoTargetNodes (desiredNumberScheduled=0 — no node matches the driver placement; re-checking once nodes appear)"
	}
	if ds.Status.UpdatedNumberScheduled != ds.Status.DesiredNumberScheduled || ds.Status.NumberAvailable != ds.Status.DesiredNumberScheduled {
		return false, v1alpha1.ReasonWorkloadNotReady,
			fmt.Sprintf("node DaemonSet not ready (updated=%d available=%d want=%d target nodes)",
				ds.Status.UpdatedNumberScheduled, ds.Status.NumberAvailable, ds.Status.DesiredNumberScheduled)
	}

	pods := &corev1.PodList{}
	if err := fresh.List(ctx, pods, client.InNamespace(ns)); err != nil {
		return false, v1alpha1.ReasonDependencyMissing,
			fmt.Sprintf("cannot list operand pods for readiness probe: %v", err)
	}
	// Owner-chain UIDs: the Deployment's current AND previous ReplicaSets
	// (both controller-owned by the Deployment — a rolling update keeps the
	// superseded ReplicaSet alive while its pods drain).
	rsUIDs, err := deploymentReplicaSetUIDs(ctx, fresh, ns, dep)
	if err != nil {
		// A ReplicaSet list failure must NOT be read as
		// "the Deployment owns no pods" — swallowing it excludes every
		// Deployment pod from the blocked-pod scan, so a missing RBAC grant,
		// a timeout or an API 5xx could surface as Ready. Surface it as a
		// retryable probe failure instead (the Deployment/DaemonSet status
		// summary above is necessary but not sufficient — the pod-level
		// scan is mandatory).
		reason := v1alpha1.ReasonDependencyMissing
		if apierrors.IsForbidden(err) {
			reason = v1alpha1.ReasonPermissionDenied
		}
		return false, reason,
			fmt.Sprintf("cannot list ReplicaSets for the owner-chain readiness probe: %v", err)
	}
	for i := range pods.Items {
		pod := pods.Items[i]
		// Owner-chain: only pods OWNED by the
		// rendered workloads count — verified through the ownerRef UID
		// chain (pod → ReplicaSet/DaemonSet → Deployment UID), not selector
		// labels, because a Deployment's pod selector is IMMUTABLE across
		// rollouts: old ReplicaSet pods AND a foreign pod planted with the
		// same labels both match it.
		if !podOwnedByWorkload(pod, ds.UID, rsUIDs) {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting == nil {
				continue
			}
			switch cs.State.Waiting.Reason {
			case "ContainerCreating":
				// Normal transition, not a failure signal.
			case "CreateContainerConfigError":
				// No Secret read — the reason surfaces AS-IS;
				// which secret/key is missing is a kubelet-side diagnosis.
				return false, v1alpha1.ReasonWorkloadNotReady,
					fmt.Sprintf("pod %q blocked: CreateContainerConfigError (the operator does not read Secrets — verify the referenced secret keys exist)", pod.Name)
			case "ImagePullBackOff", "ErrImagePull":
				return false, v1alpha1.ReasonWorkloadNotReady,
					fmt.Sprintf("pod %q blocked: %s", pod.Name, "ImagePullFailed")
			default:
				// Unclassified waiting state: generic WorkloadNotReady,
				// reason carried in the message (no promise of a distinct
				// condition reason for states we have not verified live).
				return false, v1alpha1.ReasonWorkloadNotReady,
					fmt.Sprintf("pod %q blocked: container waiting in state with reason %q", pod.Name, cs.State.Waiting.Reason)
			}
		}
	}
	return true, "", "controller Deployment ready; node DaemonSet ready on all target nodes; no blocked pods"
}

// deploymentReplicaSetUIDs lists the ReplicaSets in the operand namespace
// whose controller ownerRef points at dep. A
// rolling update keeps the Deployment's CURRENT and PREVIOUS ReplicaSet
// alive (the superseded one only after its pods drain), and both carry
// controller ownerRefs to the Deployment — so "owned by this Deployment"
// is exactly "its ownerRef UID is in this set". The list goes through the
// FRESH client with a direct get;list grant (the readiness probe is a
// one-shot read, no watch — the same precedent as ensureNamespace's
// uncached GET), so no ReplicaSet cache subscription is needed.
func deploymentReplicaSetUIDs(ctx context.Context, c client.Client, ns string, dep *appsv1.Deployment) (map[types.UID]bool, error) {
	out := map[types.UID]bool{}
	if dep.UID == "" {
		return out, nil
	}
	rsList := &appsv1.ReplicaSetList{}
	if err := c.List(ctx, rsList, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range rsList.Items {
		for _, ref := range rsList.Items[i].OwnerReferences {
			if ref.Kind == "Deployment" && ref.UID == dep.UID {
				out[rsList.Items[i].UID] = true
			}
		}
	}
	return out, nil
}

// podOwnedByWorkload reports whether the pod belongs to one of the rendered
// workloads (owner-chain form): the pod's
// controller ownerRef UID must be the DaemonSet's UID or one of the
// Deployment's ReplicaSet UIDs (deploymentReplicaSetUIDs). The old selector-
// label match was wrong in BOTH directions: a Deployment's pod selector is
// immutable across rollouts, so a superseded ReplicaSet's pods carry the
// SAME labels (they are ours, and must count during a rolling update) while
// a foreign pod planted with those labels also matched (it must NOT).
// Kubelet-created workload pods always carry the controller ownerRef, so
// the UID chain is complete in production.
func podOwnedByWorkload(pod corev1.Pod, dsUID types.UID, deploymentRSUIDs map[types.UID]bool) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "DaemonSet":
			if dsUID != "" && ref.UID == dsUID {
				return true
			}
		case "ReplicaSet":
			if deploymentRSUIDs[ref.UID] {
				return true
			}
		}
	}
	return false
}

// ensureNamespace: the operator creates operand namespaces but NEVER
// deletes them. An existing namespace — with or without our annotation —
// is used as-is.
//
// The GET goes through the DIRECT (uncached) client: the
// namespace watch was removed because controller-runtime's cache needs
// list/watch to serve it, while the manager's RBAC grants namespaces
// get;create only. A one-shot uncached GET satisfies both existence checks
// and creation races without any cache subscription.
func (r *QuartetReconciler) ensureNamespace(ctx context.Context, ns, ownerUID string) (reason, message string) {
	existing := &corev1.Namespace{}
	err := r.freshClient().Get(ctx, client.ObjectKey{Name: ns}, existing)
	if err == nil {
		return "", ""
	}
	if !apierrors.IsNotFound(err) {
		if apierrors.IsForbidden(err) {
			return v1alpha1.ReasonPermissionDenied, fmt.Sprintf("namespace %q: %s", ns, err.Error())
		}
		return v1alpha1.ReasonNamespaceMissing, fmt.Sprintf("namespace %q: %s", ns, err.Error())
	}
	created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	stampOwnership(created, ownerUID)
	if err := r.freshClient().Create(ctx, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return "", "" // raced create: use it
		}
		if apierrors.IsForbidden(err) {
			return v1alpha1.ReasonPermissionDenied, fmt.Sprintf("namespace %q create: %s", ns, err.Error())
		}
		return v1alpha1.ReasonNamespaceMissing, fmt.Sprintf("namespace %q create: %s", ns, err.Error())
	}
	return "", ""
}

// applier is the apply mechanism. Production is ssaApplier (real
// server-side apply, field manager `quartet-operator`, no force). The
// controller-runtime FAKE client rejects apply patches entirely
// (kubernetes/kubernetes#115598), so fake-client unit tests inject
// mergeApplier — a documented test double with equivalent observable
// semantics: ownership gate, create-if-absent, desired-wins drift
// correction, and untouched fields preserved (no Lease-state rewrite).
// The real SSA path is proven by the envtest (TestEnvtest*) cells.
type applier interface {
	applyOwned(ctx context.Context, obj client.Object, ownerUID string) error
}

// ssaApplier is the production mechanism. Both legs are server-side apply
// WITHOUT force under the `quartet-operator` field manager, differing only in
// the resourceVersion precondition they carry — see applyOwned and
// createOwned):
//
// - CREATE leg: SSA apply with an impossible sentinel RV — a clean create
// when the name is free, a no-write 409 when a foreign object occupies it;
// - UPDATE leg: SSA apply with the gate's RV as the optimistic-lock
// precondition, so a concurrent write in the gate→apply gap 409s instead
// of silently merging.
//
// Both are no-ops on the idempotent second reconcile: SSA never bumps
// the stored version when nothing changed, so steady-state resourceVersions
// are stable.
type ssaApplier struct{ c client.Client }

// applyOwned closes the GET→write adoption race
// with two precondition-guarded legs — the old SSA "create-or-update" merge
// could adopt a foreign object created in the gate→apply gap (annotation
// stamped, foreign spec kept, no conflict):
//
// - CREATE leg (gate saw nothing): SSA apply carrying an impossible
// sentinel resourceVersion (createOwned). A free name creates cleanly
// (operation:Apply ownership from the first write); a foreign object
// occupying the name 409-conflicts WITHOUT being written, and is
// reclassified from a fresh read — ours (concurrent same-pass create) or
// foreignConflict; it is never merged into.
//
// - UPDATE leg (gate saw an owned object): SSA apply carrying the gate's
// resourceVersion (updateOwned). The API server rejects an apply whose
// precondition no longer matches with 409 ("object has been modified") —
// proven against the real API by
// TestEnvtestUpdateRaceStaleRVPrecondition. Any change in the gap (a
// concurrent write, even to a field we do not own) therefore lands as a
// conflict, not a silent merge; the conflict is re-verified from a fresh
// read and reclassified per the conflict classification contract below —
// owned objects retry bounded on fresh state, replaced objects go
// terminal.
//
// SSA is WITHOUT force (full contract): our managed fields are
// re-asserted under the `quartet-operator` field manager, correcting drift
// on fields WE own; fields owned by another manager are never touched —
// there is no force fallback at all.
//
// CONFLICT CLASSIFICATION CONTRACT: a conflict on the UPDATE leg is
// re-verified from a FRESH read and split into two operational classes —
//
// - RV conflict on an object that is STILL OURS (UID + ownership annotation
// intact): a concurrent write moved the stored version between the gate's
// GET and the apply. That is a benign operational race — the object is
// ours to repair — and it is retried IN-RECONCILE with a fresh
// precondition, bounded by updateConflictRetries. The bounded retry is
// the field-manager verification: if the retry resolves, the conflict was
// just a stale RV (a harmless concurrent edit); it converges and the
// profile proceeds. This removes the old reliance on "the next watch
// event will come", which is NOT a recovery guarantee — the external
// edit's own event may have been consumed during the in-flight reconcile.
//
// - Field-ownership conflict (API error carries CAUSES): an SSA apply
// without force is refused with
// metav1.StatusCause entries (reason FieldValueConflict) whenever
// another field manager owns a contested path and will not yield. The
// CAUSES are the API's own diagnosis — they are present on field-
// ownership refusals and ABSENT on a plain resourceVersion-precondition
// 409 ("object has been modified"), so retrying cannot ever resolve a
// cause-bearing conflict: it is terminal IMMEDIATELY (no bound needed),
// reported foreignConflict, the foreign field survives exactly as
// written, our fields are NOT force-repaired, and reconciling it is an
// admin decision.
//
// - Persistent RV conflict (the retry EXHAUSTS the bound with no causes
// ever seen): every attempt conflicted on the
// resourceVersion precondition alone — a sustained benign concurrency
// (e.g. a controller constantly rewriting the object), NOT a field
// steal. The old code assumed persistent foreign field-ownership from
// bound exhaustion and misdiagnosed continuous harmless races as
// terminal ForeignResourceConflict. Now the raw conflict error is
// returned: classifyApplyError maps it retryable (ApplyConflict), the
// profile requeues with backoff and re-attempts on the next pass, and
// it converges the moment the concurrency settles.
//
// - OWNERSHIP conflict (the fresh read shows a UID swap or a stripped /
// wrong ownership annotation): the object was replaced or taken over.
// That is foreignConflict — terminal: adoption and force are
// forbidden, never retried.
const updateConflictRetries = 3

// isFieldOwnershipConflict inspects the API error's STATUS CAUSES: an SSA
// apply refused for field ownership returns 409 with metav1.StatusCause
// entries; a bare resourceVersion-precondition 409 ("object has been
// modified") carries NO causes. This is the mechanism that keeps a
// sustained benign race from being misdiagnosed as foreign field ownership.
//
// The cause TYPE varies by API-server version and is not a declared
// apimachinery constant in v0.33: modern servers report
// "FieldManagerConflict" (an SSA field-owner clash, proven on the 1.37
// envtest plane) while the historical shape is "FieldValueConflict". Both
// are SSA field-ownership refusals and both are terminal no-force; a
// benign RV-precondition 409 carries neither (no causes at all).
func isFieldOwnershipConflict(err error) bool {
	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	details := statusErr.ErrStatus.Details
	if details == nil {
		return false
	}
	for _, cause := range details.Causes {
		switch cause.Type {
		case "FieldManagerConflict", "FieldValueConflict":
			return true
		}
	}
	return false
}

func (a ssaApplier) applyOwned(ctx context.Context, obj client.Object, ownerUID string) error {
	present, gateUID, gateRV, err := ownershipGate(a.c, ctx, obj, ownerUID)
	if err != nil {
		return err
	}
	if !present {
		return a.createOwned(ctx, obj, ownerUID)
	}
	return a.updateOwned(ctx, obj, ownerUID, gateUID, gateRV)
}

// updateOwned is the UPDATE leg: SSA apply carrying the gate's RV, with the
// conflict classification described in the contract above. gateRV is the RV
// of the CURRENT fresh read on every attempt, so each retry applies against
// the state the API actually stores — the precondition only ever rejects a
// write that landed in THIS attempt's read→apply gap.
func (a ssaApplier) updateOwned(ctx context.Context, obj client.Object, ownerUID string, gateUID, gateRV string) error {
	var lastRVConflict error
	for attempt := 0; attempt < updateConflictRetries; attempt++ {
		// Apply a fresh COPY: SetResourceVersion mutates, and the desired
		// object must stay clean for the next attempt and the caller.
		retry := obj.DeepCopyObject().(client.Object)
		retry.SetResourceVersion(gateRV)
		err := a.applyWithoutForce(ctx, retry)
		if !apierrors.IsConflict(err) {
			return err // nil on success; non-conflict errors propagate as-is
		}

		// Conflict: re-verify ownership from a FRESH read and branch.
		fresh := obj.DeepCopyObject().(client.Object)
		gerr := a.c.Get(ctx, client.ObjectKeyFromObject(obj), fresh)
		if apierrors.IsNotFound(gerr) {
			// Object deleted after our read: the race loser — retry the
			// atomic create leg (outside the bound: a create conflict
			// reclassifies on its own).
			return a.createOwned(ctx, obj, ownerUID)
		}
		if gerr != nil {
			return gerr
		}
		// UID swap = replacement since the gate's read (even if the
		// attacker kept the annotation); annotation missing = takeover
		// attempt. Terminal — never retried, never adopted.
		if fresh.GetAnnotations()[renderer.OwnerUIDAnnotation] != ownerUID ||
			!uidMatchesGate(fresh, gateUID) {
			return &foreignConflict{kind: kindName(obj), name: obj.GetName()}
		}
		// Still ours. CAUSE-bearing conflict = SSA field-ownership refusal
		// (another manager owns a contested path): re-applying on a fresh
		// RV can never resolve it, so the bound is moot — terminal no-force
		// conflict, immediate.
		if isFieldOwnershipConflict(err) {
			return &foreignConflict{kind: kindName(obj), name: obj.GetName(),
				detail: "no-force SSA field-ownership conflict on an owned object (API error causes present — " +
					"another field manager owns a contested path); the no-force contract does not " +
					"reclaim another manager's field — remove the contested third-party write"}
		}
		// Cause-free conflict = resourceVersion precondition only: a benign
		// concurrent write in this attempt's read→apply gap. Remember it and
		// advance the precondition to the fresh RV; the retry resolves it.
		lastRVConflict = err
		gateRV = fresh.GetResourceVersion()
	}
	// Bound exhausted with RV-only conflicts on every attempt: sustained
	// benign concurrency. NOT a field steal —
	// the API never reported causes — so this must not be diagnosed as
	// terminal foreign ownership: return the raw conflict and let
	// classifyApplyError requeue it with backoff until the concurrency
	// settles.
	return lastRVConflict
}

func (a ssaApplier) applyWithoutForce(ctx context.Context, obj client.Object) error {
	return a.c.Patch(ctx, obj, client.Apply, client.FieldOwner(renderer.FieldManager))
}

// rvNeverMatches is the impossible resourceVersion the create leg carries as
// an optimistic-lock precondition. It is the uint64 maximum — a stored
// object never has it (and "0" is deliberately avoided: the API treats an
// absent/0 RV as "no precondition", which would defeat the guard).
const rvNeverMatches = "18446744073709551615"

// createOwned is the atomic CREATE leg: a single SSA apply carrying an
// impossible resourceVersion precondition (rvNeverMatches). The leg is a
// PATCH (client.Apply serializes to a server-side-apply PATCH request — the
// controller-runtime client has no separate create verb here), never a POST:
// SSA apply is create-or-update, and the sentinel makes the "update" (adopt)
// branch unreachable for any PRE-EXISTING object — proven on the real 1.37
// envtest plane (TestEnvtestCreateRaceForeignNeverAdopted):
//
// - object ABSENT: the precondition is vacuously satisfied, so this is a
// clean SSA CREATE — operation:Apply ownership from the very first
// write. This matters for correctness, not cosmetics: a legacy create
// records the object's list-of-map fields (Deployment containers/env)
// under the creating user agent as operation:Update, which a later SSA
// apply cannot take over (proven as 7-12 conflicts on the rendered
// Deployment); an SSA create owns them as operation:Apply so every
// subsequent apply and drift repair is conflict-free;
// - object PRESENT (a foreign create in the gate→apply gap): the stored
// RV never equals the sentinel, so the apply 409-conflicts and writes
// NOTHING — the foreign object stays byte-identical (UID, annotations,
// spec, managedFields) and is reclassified from a fresh read, never
// adopted (reclassifyCreateRace).
//
// EVIDENCE SCOPE: the sentinel-RV behavior is proven ONLY on the 1.37
// envtest plane. It is NOT a normative create-only API guarantee on every
// supported Kubernetes version or on OCP — live evidence there is a
// separate acceptance layer; a documented atomic-create alternative must
// be selected if any of those planes behaves differently.
func (a ssaApplier) createOwned(ctx context.Context, obj client.Object, ownerUID string) error {
	obj.SetResourceVersion(rvNeverMatches)
	err := a.applyWithoutForce(ctx, obj)
	if !apierrors.IsConflict(err) {
		return err // nil on a clean create; other errors propagate
	}
	return reclassifyCreateRace(ctx, a.c, obj, ownerUID, err)
}

// reclassifyCreateRace re-reads an object that the create leg found present
// against its precondition: if it carries this CR's owner annotation it is a
// concurrent create of OURS and counts as done; otherwise it is a foreign
// create in the gate→write gap — foreignConflict, never adopted.
// The conflict that triggered the re-read is the sentinel-RV 409 Conflict
// on the production plane; the fake-client double (mergeApplier) surfaces
// the same race as AlreadyExists from its plain Create — both are
// reclassification triggers, and neither means the leg POSTed. If the object
// vanished again, the original create error propagates so the caller's
// error classification retries the create.
func reclassifyCreateRace(ctx context.Context, c client.Client, obj client.Object, ownerUID string, origErr error) error {
	fresh := obj.DeepCopyObject().(client.Object)
	gerr := c.Get(ctx, client.ObjectKeyFromObject(obj), fresh)
	if apierrors.IsNotFound(gerr) {
		return origErr
	}
	if gerr != nil {
		return gerr
	}
	if fresh.GetAnnotations()[renderer.OwnerUIDAnnotation] == ownerUID {
		return nil
	}
	return &foreignConflict{kind: kindName(obj), name: obj.GetName()}
}

// mergeApplier is the fake-client test double (see applier). It creates when
// absent and otherwise merge-patches the desired object: patch-present fields
// win (drift correction), patch-absent fields survive (a driver-owned Lease
// spec is never rewritten).
type mergeApplier struct{ c client.Client }

func (a mergeApplier) applyOwned(ctx context.Context, obj client.Object, ownerUID string) error {
	present, _, _, err := ownershipGate(a.c, ctx, obj, ownerUID)
	if err != nil {
		return err
	}
	if !present {
		// Same observable semantics as the production atomic create leg:
		// AlreadyExists → reclassify, never adopt.
		if cerr := a.c.Create(ctx, obj); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) {
				return reclassifyCreateRace(ctx, a.c, obj, ownerUID, cerr)
			}
			return cerr
		}
		return nil
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return a.c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, data))
}

// ownershipGate stamps the owner-uid annotation and enforces the no-force
// ownership contract before any
// write: a same-name object WITHOUT this CR's annotation is foreign —
// adopt/force is forbidden. Returns (present, gateUID, gateRV,
// error): gateUID/gateRV are the UID and resourceVersion the gate observed
// ("" when the object was absent — the write is then the atomic create leg,
// and any later-conflict fresh-read against a DIFFERENT UID is a foreign
// create in the GET→write gap). gateRV feeds the update leg's optimistic-
// lock precondition: the SSA apply configuration carries it, and the
// API server rejects the apply when the stored version has moved. The read
// MUST be fresh (direct client): a stale cached NotFound would make the
// create leg race-blind to a foreign object.
func ownershipGate(c client.Client, ctx context.Context, obj client.Object, ownerUID string) (bool, string, string, error) {
	// SSA serializes typed→unstructured, which requires the GVK to be set on
	// the object (typed literals carry an empty TypeMeta). Resolve it from
	// the scheme; unstructured SCC already carries its own.
	if obj.GetObjectKind().GroupVersionKind().Empty() {
		gvk, err := c.GroupVersionKindFor(obj)
		if err != nil {
			return false, "", "", fmt.Errorf("resolve GVK for %s: %w", obj.GetName(), err)
		}
		obj.GetObjectKind().SetGroupVersionKind(gvk)
	}
	stampOwnership(obj, ownerUID)
	existing := obj.DeepCopyObject().(client.Object)
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	if existing.GetAnnotations()[renderer.OwnerUIDAnnotation] != ownerUID {
		return true, "", "", &foreignConflict{kind: kindName(obj), name: obj.GetName()}
	}
	return true, string(existing.GetUID()), existing.GetResourceVersion(), nil
}

// uidMatchesGate reports whether the object a post-conflict fresh read found
// is the SAME object the gate verified. gateUID "" means the gate saw
// nothing — a create was in flight — so a now-present object is a foreign
// create in the GET→apply gap and never matches.
func uidMatchesGate(fresh client.Object, gateUID string) bool {
	if gateUID == "" {
		return false
	}
	return string(fresh.GetUID()) == gateUID
}

// foreignConflict signals a TERMINAL ownership/no-force conflict:
// either a same-name object exists without this CR's owner-uid annotation
// (or was replaced — UID swap — under the ownership gate), or an SSA apply
// was refused with CAUSES (another field manager owns a contested path and
// the no-force contract forbids reclaiming it — diagnosed from the API
// error's causes immediately). In every case
// adoption and force are forbidden and reconciling is a human/admin
// decision. Cause-free RV conflicts DO NOT reach this type: they either
// converge within the bounded retry or — sustained
// benign concurrency — return retryable (ApplyConflict requeue).
type foreignConflict struct {
	kind   string
	name   string
	detail string
}

func (e *foreignConflict) Error() string {
	if e.detail != "" {
		return fmt.Sprintf("%s %q: %s (no-force contract)", e.kind, e.name, e.detail)
	}
	return fmt.Sprintf("foreign resource %s %q exists without this Quartet's ownership annotation; adoption is not attempted", e.kind, e.name)
}

func stampOwnership(obj client.Object, ownerUID string) {
	// Build the full map locally, THEN set it. For unstructured objects
	// GetAnnotations() returns a converted COPY (map[string]string) and
	// SetAnnotations() stores another converted map, so mutating the
	// returned map — the old implementation — silently lost the owner-uid
	// annotation on the SCC (the only unstructured operand). That left the
	// SCC unowned, so a second reconcile's ownership gate flagged it
	// foreign. Typed objects are unaffected either way.
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[renderer.OwnerUIDAnnotation] = ownerUID
	obj.SetAnnotations(ann)
}

// classifyApplyError maps an apply/delete API error onto the condition
// reason set. Every mapped reason is retryable (requeue) EXCEPT where the
// caller overrides; foreign conflicts never reach here (checked first).
func classifyApplyError(err error) (reason string, retryable bool) {
	switch {
	case apierrors.IsForbidden(err):
		return v1alpha1.ReasonPermissionDenied, true
	case apierrors.IsConflict(err):
		return v1alpha1.ReasonApplyConflict, true
	case apierrors.IsNotFound(err), apierrors.IsTimeout(err),
		apierrors.IsServiceUnavailable(err), apierrors.IsUnexpectedServerError(err),
		errors.Is(err, context.DeadlineExceeded):
		return v1alpha1.ReasonDependencyMissing, true
	default:
		return v1alpha1.ReasonApplyConflict, true
	}
}

// deleteOwnedProfile enumerates each operand kind (namespaced → profile
// namespace; cluster-scoped → the profile's own rendered names) and deletes
// ONLY objects carrying this CR's owner-uid annotation (UID-verified
// ownership). Customer objects — including same-kind resources without the
// annotation, the namespace itself, Secrets, and PV/PVC/backend — are never
// touched.
//
// Cluster-scoped kinds are scoped to THIS profile's deterministic render
// set (profile-scoped names): every profile of the singleton shares the CR
// UID, so the owner-uid annotation alone cannot tell one profile's
// cluster-scoped operand (CSIDriver, SCC) from another's — without the
// name scope, disabling profile A would enumerate cluster-wide and delete
// profile B's owned operand. A cluster-scoped owned object that is not in
// this profile's render set belongs to another profile and is skipped
// here; namespace-scoped enumeration is unchanged (the operand namespace
// is already profile-specific).
func (r *QuartetReconciler) deleteOwnedProfile(ctx context.Context, q *v1alpha1.Quartet, profile renderer.Profile, log logr.Logger) error {
	ownerUID := string(q.UID)
	ns := profile.Namespace()
	profileNames := renderedObjectNames(profile)
	// Fresh enumeration: cleanup must read the API, not the reconciler cache —
	// a cache that has not yet observed an operand (or one created by a direct
	// write) would be silently leaked when the finalizer is removed.
	fresh := r.freshClient()
	for _, kind := range profile.OperandKinds() {
		list := kind.NewList()
		opts := []client.ListOption{}
		if !kind.ClusterScoped {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := fresh.List(ctx, list, opts...); err != nil {
			// A kind may be absent from the API (e.g. SCC on plain
			// Kubernetes, or the snapshot group) — nothing to enumerate.
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("list %s for cleanup: %w", kindName(kind.Object), err)
		}
		for _, obj := range kind.Items(list) {
			if kind.ClusterScoped && !profileNames[obj.GetName()] {
				// Another profile's cluster-scoped operand (shared CR
				// UID) — never deleted by this profile's cleanup.
				continue
			}
			if obj.GetAnnotations()[renderer.OwnerUIDAnnotation] != ownerUID {
				continue // not ours
			}
			// The list entry is a point-in-time snapshot.
			// Between enumeration and deletion the object can be replaced
			// by a foreign one (UID swap, annotation stripped). Re-verify
			// with a fresh GET and carry UID + resourceVersion into the
			// delete so the API server atomically rejects a tampered or
			// replaced object. A verify failure SKIPS the delete (log +
			// loud error to the caller): cleanup must never silently
			// destroy something that changed under us.
			err := r.verifyAndDeleteOwned(ctx, fresh, log, obj, ownerUID)
			if apierrors.IsNotFound(err) || errors.Is(err, errCleanupSkipped) {
				// gone, or ownership moved under us (foreign replacement —
				// never deleted, safe to continue; loudly logged at the skip).
				continue
			}
			if err != nil {
				// Includes errCleanupPending: the object
				// STILL EXISTS and is STILL OURS — the delete conflicted with
				// a concurrent write. That is not a skip and not a success:
				// the pass fails retryable so the finalizer is retained
				// (handleTermination) or the disable requeues (applyProfiles)
				// until the race clears and a later pass deletes it.
				return fmt.Errorf("delete %s %q: %w", kindName(obj), obj.GetName(), err)
			}
		}
	}
	return nil
}

// renderedObjectNames is the set of object names one profile's deterministic
// render produces (Profile.Objects) — the cluster-scoped cleanup scope
// described in deleteOwnedProfile. Rendering is pure and cheap; the set
// never varies between reconciles for one spec projection.
func renderedObjectNames(profile renderer.Profile) map[string]bool {
	names := map[string]bool{}
	for _, o := range profile.Objects() {
		names[o.GetName()] = true
	}
	return names
}

// errCleanupSkipped marks a delete that was INTENTIONALLY skipped because
// the object CHANGED OWNERSHIP between enumeration and verification: a
// foreign replacement now occupies the name, or the object
// vanished. The skip is loud: verifyAndDeleteOwned logs it, and the object
// must NEVER be deleted. It is safe to continue the pass — there is
// nothing left of OURS at that key.
var errCleanupSkipped = errors.New("cleanup skipped: object changed under the precondition check")

// errCleanupPending marks the OTHER conflict outcome:
// the object still exists, STILL carries our UID + ownership annotation,
// but the delete's UID+resourceVersion precondition 409'd against a
// concurrent benign write that landed between verification and delete.
// This is RETRYABLE, not a skip: the object is ours and present, so the
// pass must fail and keep the finalizer (or requeue the disable) until a
// later pass re-verifies and deletes it. The old code collapsed this case
// into errCleanupSkipped, which surfaced as success and let
// handleTermination remove the finalizer with our operand still alive.
var errCleanupPending = errors.New("cleanup pending: our delete raced a concurrent write — object still exists and is still owned, requeue")

// verifyAndDeleteOwned deletes one owned operand under preconditions:
//
// 1. fresh GET — the list entry is a snapshot; re-read the live object;
// 2. UID + owner-annotation match — a UID swap or stripped annotation
// between list and delete means someone replaced the object; skip
// loudly, never delete;
// 3. Delete with client.Preconditions{UID, ResourceVersion} — the API
// server atomically rejects the delete if the object changed since
// step 1 (even for changes that keep UID+annotation, e.g. a
// re-adoption attempt racing us). The 409 has TWO distinct outcomes:
// errCleanupPending — the re-verified object is
// still ours (UID + annotation intact) and still present, so the pass
// fails retryable and the CR finalizer is kept until it deletes;
// ownership loss is errCleanupSkipped (foreign — never deleted);
// 4. postcondition GET — a delete ACCEPT is not a deletion:
// an object carrying finalizers it has not released must be requeued,
// not marked done. Finalizer REMOVAL is a separate operator step
// (handled in handleTermination for the CR itself); for operands the
// operator never removes finalizers it does not own.
func (r *QuartetReconciler) verifyAndDeleteOwned(ctx context.Context, c client.Client, log logr.Logger, obj client.Object, ownerUID string) error {
	fresh := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // gone already; nothing to do
		}
		return err
	}
	if string(fresh.GetUID()) != string(obj.GetUID()) ||
		fresh.GetAnnotations()[renderer.OwnerUIDAnnotation] != ownerUID {
		log.Info("cleanup skipped: UID or owner annotation changed between enumeration and verification",
			"kind", kindName(obj), "expectedUID", obj.GetUID(), "gotUID", fresh.GetUID())
		return errCleanupSkipped
	}
	uid, rv := fresh.GetUID(), fresh.GetResourceVersion()
	err := c.Delete(ctx, fresh, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if apierrors.IsConflict(err) {
		// Step 2 JUST verified UID + ownership annotation on this fresh
		// read, and the precondition carries the same UID — so the 409 is
		// a concurrent benign write landing in the verify→delete gap, NOT
		// a foreign replacement. The object still exists and is still
		// ours: pending, retryable, finalizer kept.
		// A UID-swap replacement can ALSO surface here as a 409 — this
		// branch makes no guarantee about which case produced the
		// conflict. Both cases are handled safely: errCleanupPending keeps
		// the finalizer (no silent skip), the next pass re-verifies UID
		// and ownership from a fresh read before deleting, and a foreign
		// replacement is never removed.
		log.Info("cleanup pending: delete precondition raced a concurrent write — requeue and re-verify ownership", "kind", kindName(obj))
		return errCleanupPending
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	still := obj.DeepCopyObject().(client.Object)
	if perr := c.Get(ctx, client.ObjectKeyFromObject(obj), still); perr == nil {
		return fmt.Errorf("delete of %s was accepted but the object still exists (finalizers=%v) — keeping the cleanup retryable",
			kindName(obj), still.GetFinalizers())
	} else if !apierrors.IsNotFound(perr) {
		return perr
	}
	return nil
}

// --- Active-usage gate for disable/delete. ---

// usageGateMaxNames bounds the condition message: at most this many names
// per category are listed before the remainder is summarized.
const usageGateMaxNames = 5

// profileHasOwnedOperands is the API evidence that this CR MANAGED one
// profile: it lists the profile's operand kinds with EXACTLY the scopes
// deleteOwnedProfile enumerates (namespaced kinds in the profile's operand
// namespace; cluster-scoped kinds restricted to the profile's own render
// set — the shared CR UID makes owner-uid alone ambiguous there) and
// reports whether ANY of them carries this CR's owner-uid annotation.
//
// The ownership gate for a disabled profile runs ONLY on this evidence,
// never on the CR status: a closed profile this CR never managed has no
// owned runtime objects, so a disable of it is a no-op — it must not run
// the usage gate (a legacy Bound PV of that driver from ANOTHER
// installation would wrongly surface DataInUse on this CR's aggregate and
// retain its deletion finalizer), and it must not enumerate for cleanup.
// Conversely, empty status is NOT proof of never-managed: a previously-
// managed profile whose operands still exist (block pending, status wiped,
// operator restarted) proves its management through this API read and
// stays protected.
//
// Returns an error on any non-NotFound/NoMatch API failure — the caller
// MUST fail closed (treat the profile as managed so the usage gate and
// cleanup still run).
func (r *QuartetReconciler) profileHasOwnedOperands(ctx context.Context, q *v1alpha1.Quartet, profile renderer.Profile) (bool, error) {
	ownerUID := string(q.UID)
	ns := profile.Namespace()
	profileNames := renderedObjectNames(profile)
	fresh := r.freshClient()
	for _, kind := range profile.OperandKinds() {
		list := kind.NewList()
		opts := []client.ListOption{}
		if !kind.ClusterScoped {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := fresh.List(ctx, list, opts...); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue // kind absent from the API — nothing to enumerate
			}
			return false, fmt.Errorf("list %s for ownership evidence: %w", kindName(kind.Object), err)
		}
		for _, obj := range kind.Items(list) {
			if kind.ClusterScoped && !profileNames[obj.GetName()] {
				continue // another profile's cluster-scoped operand (shared CR UID)
			}
			if obj.GetAnnotations()[renderer.OwnerUIDAnnotation] == ownerUID {
				return true, nil
			}
		}
	}
	return false, nil
}

// checkActiveUsage runs the active-usage gate for one profile. force=true
// skips the scan entirely (explicit admin override). Returns (summary,
// apiError): a non-empty summary OR a non-nil error both mean "blocked".
// The error path is fail-safe — an unknown usage state is treated as
// active, never clean. The scan uses the fresh (uncached) client,
// consistent with cleanup.
//
// The namespace handed to the scanner is the profile's OPERAND namespace
// (renderer.OperandNamespace), not the profile NAME: the scanner's PVC leg is
// InNamespace-scoped, and "nfs"/"fc"/"iscsi" are not namespaces — bound PVCs
// in the real operand namespace escaped the scan's PVC summary. The
// cluster-wide PV and VolumeAttachment legs are unaffected (separate
// protection, driver-identity based).
func (r *QuartetReconciler) checkActiveUsage(ctx context.Context, force bool, profileName string) (string, error) {
	if force {
		return "", nil
	}
	driverName, handlePrefix := renderer.ProvisionerIdentity(profileName)
	return scanActiveVolumeUsage(ctx, r.freshClient(), renderer.OperandNamespace(profileName), driverName, handlePrefix)
}

// scanActiveVolumeUsage checks for ACTIVE (bound / in-use) storage produced
// by the profile's CSI provisioner and returns a bounded human summary
// ("" = clean). Scope: the provisioner serves volumes to
// ANY workload namespace, so the scan is driver-identity based and
// CLUSTER-WIDE — the old operand-namespace scope let a Bound PV in another
// app namespace escape the gate (disable/delete would cut live access),
// while an unrelated-storage PVC in the operand namespace created a false
// block:
//
// - PVs: cluster-wide list; a PV counts when status.phase == Bound AND
// it carries the driver identity — spec.csi.driver == the profile's
// provisioner name (dynamic AND static CSI PVs set this) or a
// static volumeHandle prefixed with the driver name. ClaimRef.namespace
// is deliberately NOT a filter: usage in other namespaces counts.
// RELEASED/AVAILABLE PVs hold no live mount and never block; a
// terminating PV (deletionTimestamp set) still serves its bound claim
// until it is fully gone, so it counts — fail-safe.
// - VolumeAttachments: status.attached == true on ANY driver-identity PV
// (phase-blind: a Released/mid-transition PV can still carry a live
// attachment, and that attachment blocks even though the PV itself does
// not count as bound usage).
// - PVCs: only those in the profile's operand namespace, Bound (or
// terminating-Bound — fail-safe), and bound to one of those PVs
// (spec.volumeName). A PVC backed by ANY other storage — even in the
// operand namespace — must never block.
//
// FAIL-SAFE: any API error — 403, timeout, network — is returned
// as an error and the caller MUST abort the disable/delete: unknown usage
// state is treated as active.
//
// TOCTOU LIMIT (honest scope, not a guarantee): this is a point-in-time
// scan, not an atomic lock. Between the scan and the operand deletion, new
// usage can appear — a PVC bound through the operator's own driver, or an
// attachment created concurrently. The scan is a best-effort safety gate,
// NOT a guarantee that every access survives.
func scanActiveVolumeUsage(ctx context.Context, c client.Client, ns, driverName, volumeHandlePrefix string) (string, error) {
	// Cluster-wide PV scan, filtered by driver identity.
	// driverPVs covers EVERY driver-identity PV regardless of phase (it
	// feeds the VolumeAttachment set below); activePVs is the counted
	// subset — Bound, or terminating-with-claim (fail-safe) — which feeds
	// the PVC set and the usage summary.
	driverPVs := map[string]bool{}
	activePVs := map[string]bool{}
	pvNames := []string{}
	pvs := &corev1.PersistentVolumeList{}
	if err := c.List(ctx, pvs); err != nil {
		return "", fmt.Errorf("list PVs: %w", err)
	}
	for i := range pvs.Items {
		pv := pvs.Items[i]
		if !pvCarriesDriverIdentity(pv, driverName, volumeHandlePrefix) {
			continue
		}
		driverPVs[pv.Name] = true
		// Bound PVs serve live usage. A TERMINATING PV (deletionTimestamp
		// set) that still links a claim may serve it until fully gone —
		// count it too (fail-safe). Released and stable-Available PVs hold
		// no CLAIM and never count as bound usage — but their attachments
		// are still checked (see below).
		if pv.Status.Phase == corev1.VolumeBound ||
			(!pv.DeletionTimestamp.IsZero() && pv.Spec.ClaimRef != nil) {
			activePVs[pv.Name] = true
			pvNames = append(pvNames, pv.Name)
		}
	}

	// PVCs in the operand namespace bound to one of those PVs.
	pvcNames := []string{}
	pvclist := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvclist, client.InNamespace(ns)); err != nil {
		return "", fmt.Errorf("list PVCs in %q: %w", ns, err)
	}
	for i := range pvclist.Items {
		pvc := pvclist.Items[i]
		if pvc.Status.Phase != corev1.ClaimBound {
			continue
		}
		if pvc.Spec.VolumeName != "" && activePVs[pvc.Spec.VolumeName] {
			pvcNames = append(pvcNames, pvc.Name)
		}
	}

	// VolumeAttachments attached to ANY driver-identity PV, phase-blind: the
	// old set was tied to the Bound/terminating PVs above, so a Released or
	// mid-phase-transition PV that still carried a live ATTACHMENT escaped
	// the gate. The attachment is
	// what keeps the data mounted on a node — its source PV's phase is a
	// lie in transition (the PV can show Released while the attachment has
	// not cleared), so the attachment set covers the whole driver-identity
	// PV set.
	vaNames := []string{}
	vas := &storagev1.VolumeAttachmentList{}
	if err := c.List(ctx, vas); err != nil {
		return "", fmt.Errorf("list VolumeAttachments: %w", err)
	}
	for i := range vas.Items {
		va := vas.Items[i]
		if va.Spec.Source.PersistentVolumeName == nil {
			continue
		}
		if va.Status.Attached && driverPVs[*va.Spec.Source.PersistentVolumeName] {
			vaNames = append(vaNames, va.Name)
		}
	}

	return summarizeUsage(pvcNames, pvNames, vaNames), nil
}

// pvCarriesDriverIdentity reports whether a PV was produced by (or statically
// registered for) the profile's CSI provisioner: spec.csi.driver matches the
// driver name (dynamic and static CSI PVs carry it), or — static PVs
// authored before the driver name was known — the volumeHandle starts with
// the driver's handle prefix.
func pvCarriesDriverIdentity(pv corev1.PersistentVolume, driverName, volumeHandlePrefix string) bool {
	if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == driverName {
		return true
	}
	if volumeHandlePrefix != "" && pv.Spec.CSI != nil &&
		strings.HasPrefix(pv.Spec.CSI.VolumeHandle, volumeHandlePrefix) {
		return true
	}
	return false
}

// summarizeUsage builds the bounded human summary; empty when clean.
func summarizeUsage(pvcNames, pvNames, vaNames []string) string {
	if len(pvcNames)+len(pvNames)+len(vaNames) == 0 {
		return ""
	}
	var parts []string
	if len(pvcNames) > 0 {
		parts = append(parts, fmt.Sprintf("%d bound PVC(s): %s", len(pvcNames), summarizeNames(pvcNames)))
	}
	if len(pvNames) > 0 {
		parts = append(parts, fmt.Sprintf("%d driver-produced PV(s) in use cluster-wide: %s", len(pvNames), summarizeNames(pvNames)))
	}
	if len(vaNames) > 0 {
		parts = append(parts, fmt.Sprintf("%d attached VolumeAttachment(s): %s", len(vaNames), summarizeNames(vaNames)))
	}
	return strings.Join(parts, ", ")
}

func summarizeNames(names []string) string {
	if len(names) <= usageGateMaxNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(names[:usageGateMaxNames], ", "), len(names)-usageGateMaxNames)
}

// usageGateReasonMessage maps a blocked scan onto the (reason, message)
// pair. A non-nil gateErr is fail-safe (unknown usage == active) and is
// classified PermissionDenied/DependencyMissing; a non-empty usage summary is
// the draft DataInUse reason.
func usageGateReasonMessage(profile, ns, usage string, gateErr error) (string, string) {
	if gateErr != nil {
		reason := v1alpha1.ReasonDependencyMissing
		if apierrors.IsForbidden(gateErr) {
			reason = v1alpha1.ReasonPermissionDenied
		}
		return reason, fmt.Sprintf("profile %s: cannot verify active driver storage usage (cluster-wide scan, operand namespace %q): %v — fail-safe, disable/delete cancelled until the API state is readable", profile, ns, gateErr)
	}
	return v1alpha1.ReasonDataInUse, fmt.Sprintf("profile %s: %s — active driver usage present (cluster-wide driver-identity scan); explicit override: spec.profiles.%s.forceDisable=true", profile, usage, profile)
}

// setBlockedTermination records the active-usage block while KEEPING the
// finalizer: the CR stays terminating, the reason surfaces on
// Ready/Progressing/Degraded, and the caller requeues to replay the gate.
//
// RECOVERY (admin procedure):
// 1. Preferred — the spec is still mutable on a terminating object (only
// metadata.name/namespace are immutable): set
// spec.profiles.<p>.forceDisable=true and the next cleanup pass proceeds.
// 2. If the spec is unreachable (CRD removed/unservable, admission frozen),
// break the finalizer manually AFTER verifying no live usage remains:
// kubectl patch quartet quartet --type=merge -p '{"metadata":{"finalizers":null}}'.
// WARNING: this leaves any not-yet-deleted operands orphaned — they
// survive with the ownership annotation and must be cleaned by hand;
// customer data is untouched.
// 3. OPEN LIMITATION: if the CRD or the operator is uninstalled while the
// finalizer is pending, the finalizer stays unresolved until the CRD is
// re-added — kubectl cannot edit an object whose CRD is gone.
func (r *QuartetReconciler) setBlockedTermination(ctx context.Context, log logr.Logger, q *v1alpha1.Quartet, reason, message string) {
	oldStatus := q.Status.DeepCopy()
	desired := q.Status.DeepCopy()
	setCondition(&desired.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	setCondition(&desired.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, reason, "deletion blocked; re-queued")
	setCondition(&desired.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, reason, message)
	if statusEqual(q.Status, *desired) {
		log.V(1).Info("blocked-termination status unchanged; skipping write")
		return
	}
	q.Status = *desired
	if err := r.Status().Update(ctx, q); err != nil {
		log.Error(err, "failed to update status during blocked termination")
		return
	}
	r.emitTransitionEvents(q, oldStatus, desired)
	log.Info("termination blocked; finalizer retained", "reason", reason)
}

// handleTermination runs finalizer cleanup on a deleting CR:
// delete owned operands for every profile, then remove the finalizer. Any
// failure KEEPS the finalizer and reports CleanupFailed (requeue).
func (r *QuartetReconciler) handleTermination(ctx context.Context, q *v1alpha1.Quartet) (ctrl.Result, error) {
	log := r.Log.WithValues("quartet", q.Name)
	if !controllerutil.ContainsFinalizer(q, quartetFinalizer) {
		return ctrl.Result{}, nil
	}

	// The operand set can only be enumerated from a validated spec. An
	// invalid spec at deletion keeps the finalizer fail-safe.
	if reason, msg := r.validate(q); reason != "" {
		r.setCleanupFailed(ctx, log, q, fmt.Sprintf("spec invalid at deletion (%s): %s — repair the spec or remove the finalizer manually", reason, msg))
		return ctrl.Result{RequeueAfter: retryableRequeueAfter}, nil
	}
	resolved, err := DetectPlatform(r.PlatformDiscovery, q.Spec.Platform)
	if err != nil {
		r.setCleanupFailed(ctx, log, q, fmt.Sprintf("platform detection failed during cleanup: %v", err))
		return ctrl.Result{RequeueAfter: retryableRequeueAfter}, nil
	}

	for _, name := range renderer.ProfileNames() {
		spec := q.ProfileSpecs()[name]
		profile := renderer.For(inputFor(name, spec, q, resolved))
		if profile == nil {
			continue
		}
		// Ownership evidence BEFORE the gate — same contract as the
		// disable path: a profile this CR never managed (no API-present
		// operand carries the CR's owner-uid annotation) is a no-op during
		// termination too. Its legacy volumes (another installation's
		// driver) must not retain this CR's finalizer. Previously-managed
		// profiles prove management through the read and keep the full
		// active-usage protection below; an API error fails closed to
		// managed.
		managed, ownerErr := r.profileHasOwnedOperands(ctx, q, profile)
		if ownerErr != nil {
			log.Info("ownership evidence unavailable during cleanup — failing closed to managed (usage gate + cleanup still run)",
				"profile", name, "error", ownerErr.Error())
			managed = true
		}
		if !managed {
			continue
		}
		// The active-usage gate applies to CR deletion too: a profile
		// whose operand namespace still has bound/in-use storage must not
		// be torn down without the explicit forceDisable override. The
		// finalizer stays.
		if usage, gateErr := r.checkActiveUsage(ctx, spec.ForceDisable, name); gateErr != nil || usage != "" {
			reason, msg := usageGateReasonMessage(name, profile.Namespace(), usage, gateErr)
			r.setBlockedTermination(ctx, log, q, reason, msg)
			return ctrl.Result{RequeueAfter: retryableRequeueAfter}, nil
		}
		if err := r.deleteOwnedProfile(ctx, q, profile, log.WithValues("profile", name)); err != nil {
			r.setCleanupFailed(ctx, log, q, fmt.Sprintf("profile %s cleanup failed: %v", name, err))
			return ctrl.Result{RequeueAfter: retryableRequeueAfter}, nil
		}
	}

	controllerutil.RemoveFinalizer(q, quartetFinalizer)
	if err := r.Update(ctx, q); err != nil {
		log.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setCleanupFailed records Ready=False/CleanupFailed while keeping the
// finalizer; it is the recovery surface for stuck deletions.
func (r *QuartetReconciler) setCleanupFailed(ctx context.Context, log logr.Logger, q *v1alpha1.Quartet, message string) {
	oldStatus := q.Status.DeepCopy()
	desired := q.Status.DeepCopy()
	setCondition(&desired.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonCleanupFailed, message)
	setCondition(&desired.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonCleanupFailed, "cleanup pending")
	setCondition(&desired.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonCleanupFailed, message)
	if statusEqual(q.Status, *desired) {
		log.V(1).Info("cleanup-failed status unchanged; skipping write")
		return
	}
	q.Status = *desired
	if err := r.Status().Update(ctx, q); err != nil {
		log.Error(err, "failed to update status during cleanup")
		return
	}
	r.emitTransitionEvents(q, oldStatus, desired)
	log.Info("cleanup failed; finalizer retained", "message", message)
}

func kindName(obj client.Object) string {
	return fmt.Sprintf("%s %q", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
}

// applierOrDefault resolves the apply mechanism: an injected test double when
// set (fake-client tests), otherwise production SSA. The SSA applier uses the
// fresh (uncached) client so its ownership Get is a live read.
func (r *QuartetReconciler) applierOrDefault() applier {
	if r.Applier != nil {
		return r.Applier
	}
	return ssaApplier{c: r.freshClient()}
}

// freshClient returns the uncached client used for safety-critical reads
// (ownership gate, cleanup enumeration). When DirectClient is unset (unit
// tests use a direct client throughout), it falls back to Client.
func (r *QuartetReconciler) freshClient() client.Client {
	if r.DirectClient != nil {
		return r.DirectClient
	}
	return r.Client
}
