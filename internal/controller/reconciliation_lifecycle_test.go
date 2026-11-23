package controller

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-logr/zapr"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"strings"
	"testing"
	"time"
)

// -------------------------------------------------------------------
// Workload readiness gate (apply != Ready)
// -------------------------------------------------------------------

// setDeploymentStatus applies a mutation to the live controller Deployment's
// status subresource and persists it.
func setDeploymentStatus(t *testing.T, c client.Client, mutate func(*appsv1.Deployment)) {
	t.Helper()
	d := getDeployment(t, c)
	if d == nil {
		t.Fatal("Deployment missing for status mutation")
	}
	mutate(d)
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("set Deployment status: %v", err)
	}
}

// readyDeploymentStatus marks the Deployment fully rolled out (replicas=1).
func readyDeploymentStatus(t *testing.T, c client.Client) {
	setDeploymentStatus(t, c, func(d *appsv1.Deployment) {
		d.Status.ObservedGeneration = d.Generation
		d.Status.Replicas = 1
		d.Status.UpdatedReplicas = 1
		d.Status.AvailableReplicas = 1
		d.Status.ReadyReplicas = 1
	})
}

func setDaemonSetStatus(t *testing.T, c client.Client, mutate func(*appsv1.DaemonSet)) {
	t.Helper()
	ds := &appsv1.DaemonSet{}
	owned(t, c, types.NamespacedName{Name: "nfs-csi-ngxstorage-node", Namespace: "ngxstorage-nfs"}, ds)
	mutate(ds)
	if err := c.Status().Update(context.Background(), ds); err != nil {
		t.Fatalf("set DaemonSet status: %v", err)
	}
}

// TestWorkloadGateDeploymentRollout: operand apply alone is NOT Ready; the
// Deployment rollout conditions (observedGeneration, available, updated)
// gate the profile, and the gate is retryable until it settles.
func TestWorkloadGateDeploymentRollout(t *testing.T) {
	te := newTestEnv(t, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("not-ready workload must requeue, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("aggregate = (%v %q), want (False WorkloadNotReady)", c.Status, c.Reason)
	}
	if pc := profileReadyNFS(t, te); !strings.Contains(pc.Message, "Deployment not ready") {
		t.Fatalf("profile message must name the Deployment gate: %q", pc.Message)
	}

	// Observed-but-partial rollout still fails.
	setDeploymentStatus(t, te.client, func(d *appsv1.Deployment) {
		d.Status.ObservedGeneration = d.Generation
		d.Status.AvailableReplicas = 0
	})
	te.reconcile(t, v1alpha1.QuartetName)
	c = profileReadyNFS(t, te)
	if c.Reason != v1alpha1.ReasonWorkloadNotReady || !strings.Contains(c.Message, "Deployment not ready") {
		t.Fatalf("partial rollout must stay gated: (%v %q)", c.Reason, c.Message)
	}

	// Unobserved generation fails even when replicas look ready. (The fake
	// client leaves Generation at 0, so use an explicit generation delta —
	// a real API server starts Generation at 1. Generation is metadata, so
	// it needs a main Update; status rides the subresource.)
	d := getDeployment(t, te.client)
	d.Generation = 5
	if err := te.client.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	setDeploymentStatus(t, te.client, func(d *appsv1.Deployment) {
		d.Status.ObservedGeneration = 0
		d.Status.AvailableReplicas = 1
		d.Status.UpdatedReplicas = 1
	})
	te.reconcile(t, v1alpha1.QuartetName)
	c = profileReadyNFS(t, te)
	if !strings.Contains(c.Message, "rollout not observed") {
		t.Fatalf("unobserved rollout must be named: %q", c.Message)
	}

	// Full rollout converges the gate.
	fakeWorkloadReady(t, te.client)
	res = te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("converged workloads must not requeue, got %+v", res)
	}
	c = condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("converged workloads must be Ready: (%v %q)", c.Status, c.Reason)
	}
	// The aggregate states workload convergence while the
	// profile carries the probe's detailed summary.
	if !strings.Contains(c.Message, "workloads converged") {
		t.Fatalf("aggregate Ready message must state workload convergence: %q", c.Message)
	}
	if pc := profileReadyNFS(t, te); !strings.Contains(pc.Message, "no blocked pods") {
		t.Fatalf("profile Ready message must carry the probe summary: %q", pc.Message)
	}
}

// TestWorkloadGateDaemonSetZeroNodesExplicit: a
// DaemonSet with zero target nodes is NOT ready — no node runs the driver,
// so the profile cannot serve a single volume. The gate stays
// WorkloadNotReady with the distinct NoTargetNodes marker (retryable —
// nodes may appear later), and acquiring targets re-arms it.
func TestWorkloadGateDaemonSetZeroNodesExplicit(t *testing.T) {
	te := newTestEnv(t, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("zero-target-node profile must requeue until nodes appear, got %+v", res)
	}
	readyDeploymentStatus(t, te.client)

	te.reconcile(t, v1alpha1.QuartetName)
	pc := profileReadyNFS(t, te)
	if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonWorkloadNotReady || !strings.Contains(pc.Message, "NoTargetNodes") {
		t.Fatalf("zero-target DS must be WorkloadNotReady with NoTargetNodes: (%v %q %q)", pc.Status, pc.Reason, pc.Message)
	}
	agg := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if agg.Status != metav1.ConditionFalse || agg.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("aggregate must stay not-ready with zero target nodes: (%v %q)", agg.Status, agg.Reason)
	}

	// A node pool appears but the rollout has not reached it.
	setDaemonSetStatus(t, te.client, func(ds *appsv1.DaemonSet) {
		ds.Status.DesiredNumberScheduled = 1
		ds.Status.UpdatedNumberScheduled = 0
		ds.Status.NumberAvailable = 0
		ds.Status.ObservedGeneration = ds.Generation
	})
	te.reconcile(t, v1alpha1.QuartetName)
	c := profileReadyNFS(t, te)
	if c.Reason != v1alpha1.ReasonWorkloadNotReady || !strings.Contains(c.Message, "DaemonSet not ready") {
		t.Fatalf("DS with unscheduled targets must be gated: (%v %q)", c.Reason, c.Message)
	}

	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)
	c = profileReadyNFS(t, te)
	if c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "all target nodes") {
		t.Fatalf("fully scheduled DS must converge: (%v %q)", c.Status, c.Message)
	}
}

// ensureDeploymentReplicaSet creates (or returns) a ReplicaSet in the
// operand namespace controller-owned by the rendered Deployment — the
// owner chain the readiness probe follows. The
// fake client assigns no UIDs, so a Deployment without one gets a pinned
// deterministic UID first (the real API always carries one).
func ensureDeploymentReplicaSet(t *testing.T, c client.Client, rsName string) *appsv1.ReplicaSet {
	t.Helper()
	d := getDeployment(t, c)
	if d == nil {
		t.Fatal("Deployment missing for ReplicaSet chain")
	}
	if d.UID == "" {
		d.SetUID(types.UID("uid-" + d.Name))
		if err := c.Update(context.Background(), d); err != nil {
			t.Fatalf("pin Deployment UID: %v", err)
		}
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name:      rsName,
		Namespace: "ngxstorage-nfs",
		UID:       types.UID("uid-" + rsName),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name,
			UID: d.UID, Controller: ptr.To(true),
		}},
	}}
	if err := c.Create(context.Background(), rs); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create ReplicaSet: %v", err)
		}
		got := &appsv1.ReplicaSet{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: rsName, Namespace: "ngxstorage-nfs"}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	return rs
}

// workloadOwnedPod creates a pod the readiness probe counts as workload-
// owned: ownerRef → ReplicaSet → Deployment UID chain, plus
// the rendered selector labels (real kubelet pods carry both).
func workloadOwnedPod(t *testing.T, c client.Client, rs *appsv1.ReplicaSet, name, waitingReason string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "ngxstorage-nfs",
		Labels: map[string]string{
			"app.kubernetes.io/name":      "nfs-csi-ngxstorage",
			"app.kubernetes.io/component": "controller",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name,
			UID: rs.UID, Controller: ptr.To(true),
		}},
	}}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "nfs-csi-ngxstorage",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}},
	}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// blockedPod is the test shorthand: a workload-owned pod in the given
// waiting state (owner chain via ensureDeploymentReplicaSet).
func blockedPod(t *testing.T, c client.Client, name, waitingReason string) {
	t.Helper()
	rs := ensureDeploymentReplicaSet(t, c, "blocked-rs-"+name)
	workloadOwnedPod(t, c, rs, name, waitingReason)
}

// TestWorkloadGatePodCreateContainerConfigError: a pod stuck in
// CreateContainerConfigError surfaces WorkloadNotReady with the reason AS-IS.
// The operator does not read Secrets, so no MissingSecret/
// MissingSecretKey distinction is promised — the condition reason stays
// WorkloadNotReady.
func TestWorkloadGatePodCreateContainerConfigError(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)

	blockedPod(t, te.client, "nfs-csi-ngxstorage-controller-000001", "CreateContainerConfigError")
	te.reconcile(t, v1alpha1.QuartetName)
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("CreateContainerConfigError must surface WorkloadNotReady, got %q", c.Reason)
	}
	if c.Reason == v1alpha1.ReasonMissingSecret || c.Reason == v1alpha1.ReasonMissingSecretKey {
		t.Fatal("the operator must not promise a MissingSecret/Key distinction it cannot prove")
	}
	if pc := profileReadyNFS(t, te); !strings.Contains(pc.Message, "CreateContainerConfigError") {
		t.Fatalf("profile message must carry the waiting reason verbatim: %q", pc.Message)
	}
}

// TestWorkloadGatePodImagePullBackOff: image pull failures map to the
// ImagePullFailed reason wording.
func TestWorkloadGatePodImagePullBackOff(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)

	blockedPod(t, te.client, "nfs-csi-ngxstorage-controller-000001", "ImagePullBackOff")
	te.reconcile(t, v1alpha1.QuartetName)
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("ImagePullBackOff reason = %q, want WorkloadNotReady", c.Reason)
	}
	if pc := profileReadyNFS(t, te); !strings.Contains(pc.Message, "ImagePullFailed") {
		t.Fatalf("ImagePullBackOff must surface ImagePullFailed in the profile message: %q", pc.Message)
	}

	// Pod recovers: the gate reopens on the same probe.
	pod := &corev1.Pod{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller-000001", Namespace: "ngxstorage-nfs"}, pod)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "nfs-csi-ngxstorage",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if err := te.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)
	c = condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("recovered pod must reopen the gate: (%v %q)", c.Status, c.Reason)
	}
}

// TestWorkloadGateIgnoresUnrelatedFailedPod: an unrelated failed pod in
// the operand namespace — no ownerRef to the workloads, no matching
// template labels — must NOT drag the profile out of Ready.
func TestWorkloadGateIgnoresUnrelatedFailedPod(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)
	if c := profileReadyNFS(t, te); c.Status != metav1.ConditionTrue {
		t.Fatalf("baseline must be Ready: (%v %q)", c.Status, c.Reason)
	}

	// An ImagePullBackOff pod with NO ownership link to either workload.
	stray := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated-crashing-pod", Namespace: "ngxstorage-nfs",
	}}
	if err := te.client.Create(context.Background(), stray); err != nil {
		t.Fatal(err)
	}
	stray.Status.Phase = corev1.PodPending
	stray.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	if err := te.client.Status().Update(context.Background(), stray); err != nil {
		t.Fatal(err)
	}

	te.reconcile(t, v1alpha1.QuartetName)
	c := profileReadyNFS(t, te)
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("unrelated failed pod must not affect readiness: (%v %q %q)", c.Status, c.Reason, c.Message)
	}
}

// TestWorkloadGateIgnoresUnrelatedReplicaSetPod (owner-chain form): a blocked
// pod from a ReplicaSet owned by a DIFFERENT
// Deployment in the operand namespace must not count — even though it
// carries the live Deployment's selector labels (labels are immutable
// across rollouts and therefore cannot discriminate); a blocked pod owned
// through the live Deployment's own ReplicaSet chain still does.
func TestWorkloadGateIgnoresUnrelatedReplicaSetPod(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)

	// Live chain: ReplicaSet controller-owned by the rendered Deployment.
	liveRS := ensureDeploymentReplicaSet(t, te.client, "live-rs")
	// Foreign chain: a ReplicaSet owned by ANOTHER deployment, whose pods
	// carry the SAME selector labels (the old label-only match could not
	// tell the two apart).
	foreignDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-workload", Namespace: "ngxstorage-nfs",
		UID: types.UID("uid-foreign-dep"),
	}}
	if err := te.client.Create(context.Background(), foreignDep); err != nil {
		t.Fatal(err)
	}
	foreignRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-rs", Namespace: "ngxstorage-nfs",
		UID: types.UID("uid-foreign-rs"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: foreignDep.Name,
			UID: foreignDep.UID, Controller: ptr.To(true),
		}},
	}}
	if err := te.client.Create(context.Background(), foreignRS); err != nil {
		t.Fatal(err)
	}

	selectorLabels := map[string]string{
		"app.kubernetes.io/name":      "nfs-csi-ngxstorage",
		"app.kubernetes.io/component": "controller", // live Deployment selector — also carried by the foreign pod
	}
	// Foreign pod with the SAME labels, blocked — must be IGNORED.
	foreignPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-rs-pod", Namespace: "ngxstorage-nfs",
		Labels: selectorLabels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: foreignRS.Name,
			UID: foreignRS.UID, Controller: ptr.To(true),
		}},
	}}
	if err := te.client.Create(context.Background(), foreignPod); err != nil {
		t.Fatal(err)
	}
	if err := setPodWaiting(te.client, foreignPod, "CreateContainerConfigError"); err != nil {
		t.Fatal(err)
	}
	// Live-chain pod, blocked — must COUNT.
	live := workloadOwnedPod(t, te.client, liveRS, "live-deploy-pod", "CreateContainerConfigError")

	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("workload-owned blocked pod must gate the profile, got %+v", res)
	}
	c := profileReadyNFS(t, te)
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("workload-owned blocked pod must surface WorkloadNotReady: (%v %q)", c.Status, c.Reason)
	}

	// Live pod recovers; only the foreign blocked pod remains → Ready.
	_ = setPodWaiting(te.client, live, "")
	te.reconcile(t, v1alpha1.QuartetName)
	c = profileReadyNFS(t, te)
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("foreign (same-label) pods must not hold the gate: (%v %q %q)", c.Status, c.Reason, c.Message)
	}
}

// setPodWaiting sets (or, with "", clears) a pod's container waiting state
// through the status subresource.
func setPodWaiting(c client.Client, pod *corev1.Pod, reason string) error {
	if reason == "" {
		pod.Status.ContainerStatuses = nil
		pod.Status.Phase = corev1.PodRunning
	} else {
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "c",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}
	}
	return c.Status().Update(context.Background(), pod)
}

// TestWorkloadGateRollingUpdateOwnerChain: a ROLLING UPDATE with an OLD
// ReplicaSet pod that
// failed, plus a FOREIGN pod carrying the same labels. The old pod belongs
// to the Deployment's previous revision (its ReplicaSet is still
// controller-owned by the Deployment), so it COUNTS and gates the profile;
// the foreign pod does NOT — ownership is decided by the
// ReplicaSet→Deployment UID chain, never by the immutable selector labels.
func TestWorkloadGateRollingUpdateOwnerChain(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)
	if c := profileReadyNFS(t, te); c.Status != metav1.ConditionTrue {
		t.Fatalf("baseline must be Ready: (%v %q)", c.Status, c.Reason)
	}

	// Previous revision: the Deployment keeps the superseded ReplicaSet
	// alive while its pods drain (both are controller-owned by the same
	// Deployment UID).
	prevRS := ensureDeploymentReplicaSet(t, te.client, "nfs-csi-ngxstorage-controller-11111")
	oldPod := workloadOwnedPod(t, te.client, prevRS, "old-rs-failed-pod", "ImagePullBackOff")

	// A foreign pod with the same selector labels and a foreign owner
	// chain — must not affect the gate.
	foreignRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-rs-rolling", Namespace: "ngxstorage-nfs",
		UID: types.UID("uid-foreign-rs-rolling"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "foreign-deploy",
			UID: types.UID("uid-foreign-deploy"), Controller: ptr.To(true),
		}},
	}}
	if err := te.client.Create(context.Background(), foreignRS); err != nil {
		t.Fatal(err)
	}
	foreignPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-same-labels-pod", Namespace: "ngxstorage-nfs",
		Labels: map[string]string{
			"app.kubernetes.io/name":      "nfs-csi-ngxstorage",
			"app.kubernetes.io/component": "controller",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: foreignRS.Name,
			UID: foreignRS.UID, Controller: ptr.To(true),
		}},
	}}
	if err := te.client.Create(context.Background(), foreignPod); err != nil {
		t.Fatal(err)
	}
	if err := setPodWaiting(te.client, foreignPod, "CreateContainerConfigError"); err != nil {
		t.Fatal(err)
	}

	// The OLD failed pod gates; the foreign pod is invisible to the probe.
	te.reconcile(t, v1alpha1.QuartetName)
	c := profileReadyNFS(t, te)
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("old-ReplicaSet failed pod must gate via the owner chain: (%v %q)", c.Status, c.Reason)
	}
	if !strings.Contains(c.Message, "old-rs-failed-pod") {
		t.Fatalf("gate message must name the OLD pod (owner chain counted it), got %q", c.Message)
	}

	// Old pod drains (recovers) — only the foreign pod remains → Ready.
	_ = setPodWaiting(te.client, oldPod, "")
	te.reconcile(t, v1alpha1.QuartetName)
	c = profileReadyNFS(t, te)
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("same-label foreign pod must not hold the gate after the old pod drained: (%v %q %q)", c.Status, c.Reason, c.Message)
	}
}

// profileReadyNFS reads the nfs profile's own Ready condition — the detailed
// probe message lives there; the aggregate carries the fixed wording.
func profileReadyNFS(t *testing.T, te *fakeEnv) *metav1.Condition {
	t.Helper()
	ps := te.get(t, v1alpha1.QuartetName).Status.Profiles[v1alpha1.ProfileNFS]
	return condition(t, ps.Conditions, v1alpha1.ConditionReady)
}

// ctrlConfig builds manager controller config for the setup-only tests that
// register the same controller name more than once per process (controller-
// runtime names controllers globally for metrics; skip the uniqueness check).
func ctrlConfig(skip bool) config.Controller {
	return config.Controller{SkipNameValidation: ptr.To(skip)}
}

// -------------------------------------------------------------------
// Namespace existence via uncached GET (no list/watch)
// -------------------------------------------------------------------

// TestNamespaceEnsureWithoutListWatch: the namespace check must work with a
// client that FORBIDS namespace list/watch — mirroring the manager RBAC
// (namespaces get;create only). The old cached-GET + Namespace watch needed
// the informers' list/watch and broke at startup against the real SA.
func TestNamespaceEnsureWithoutListWatch(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "",
		errors.New("RBAC: list/watch denied — manager grants get;create only"))
	funcs := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isNS := list.(*corev1.NamespaceList); isNS {
				return forbidden
			}
			return c.List(ctx, list, opts...)
		},
	}
	te := newInterceptedEnv(t, funcs, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)

	if getNamespace(te.client, "ngxstorage-nfs") == nil {
		t.Fatal("namespace not created via the direct GET/Create path")
	}
	if getCSIDriver(t, te.client) == nil {
		t.Fatal("reconcile did not proceed past the namespace check")
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason == v1alpha1.ReasonPermissionDenied || c.Reason == v1alpha1.ReasonNamespaceMissing {
		t.Fatalf("namespace check must not misclassify under get;create RBAC, got %q (%+v)", c.Reason, res)
	}
}

// -------------------------------------------------------------------
// Out-of-band operand RBAC: the RBACDrift health gate
// -------------------------------------------------------------------

// TestOperandRBACDriftGatesApply: the runtime NEVER
// creates, patches or repairs operand RBAC — the install set
// (renderer.InstallRBAC, config/operands/<profile>/) is admin-provisioned
// out-of-band, and the reconcile-start health gate verifies it by pinned
// name. Any drift — a missing object, rules that no longer match the render,
// or binding subjects pointing somewhere else — gates the WHOLE profile
// behind Degraded/RBACDrift (draft reason, retryable requeue) BEFORE any
// operand is created or modified.
func TestOperandRBACDriftGatesApply(t *testing.T) {
	cases := []struct {
		name  string
		drift func(te *fakeEnv)
	}{
		{"missing install ClusterRole", func(te *fakeEnv) {
			err := te.client.Delete(context.Background(), &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"}})
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"drifted ClusterRole rules", func(te *fakeEnv) {
			cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"}}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: cr.Name}, cr); err != nil {
				t.Fatal(err)
			}
			cr.Rules[0].Verbs = append(cr.Rules[0].Verbs, "deletecollection")
			if err := te.client.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
		}},
		{"drifted RoleBinding subject", func(te *fakeEnv) {
			rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
				Name: "nfs-csi-ngxstorage-controller-operation-lock", Namespace: "ngxstorage-nfs"}}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: rb.Name, Namespace: rb.Namespace}, rb); err != nil {
				t.Fatal(err)
			}
			rb.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "evil-sa", Namespace: rb.Namespace}}
			if err := te.client.Update(context.Background(), rb); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, validCR())
			tc.drift(te)
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != retryableRequeueAfter {
				t.Fatalf("RBAC drift must requeue (retryable — the admin may re-apply the install manifest), got %+v", res)
			}
			c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
			if c.Reason != v1alpha1.ReasonRBACDrift {
				t.Fatalf("RBAC drift reason = %q, want RBACDrift", c.Reason)
			}
			if d := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionDegraded); d.Status != metav1.ConditionTrue {
				t.Fatalf("RBAC drift must set Degraded=True, got %v", d.Status)
			}
			// The gate is BEFORE any operand write: nothing created.
			if getCSIDriver(t, te.client) != nil {
				t.Fatal("RBAC drift: CSIDriver must not be created before the gate passes")
			}
			if getDeployment(t, te.client) != nil {
				t.Fatal("RBAC drift: Deployment must not be created before the gate passes")
			}
			// And the install set is left untouched (no repair attempt).
			cr := &rbacv1.ClusterRole{}
			_ = te.client.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, cr)
			if _, has := cr.Annotations[renderer.OwnerUIDAnnotation]; has {
				t.Fatal("RBAC health gate must not stamp ownership on install objects")
			}
		})
	}
}

// TestOperandRBACHealthyInstallPassesGate (positive half):
// the pre-seeded healthy install set passes the gate and the profile proceeds
// to operand apply (workload gate on the fake env — apply happened).
func TestOperandRBACHealthyInstallPassesGate(t *testing.T) {
	te := newTestEnv(t, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("fake env settles at WorkloadNotReady (no kubelet) — must requeue, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason == v1alpha1.ReasonRBACDrift {
		t.Fatalf("healthy install must not surface RBACDrift, got %q: %s", c.Reason, c.Message)
	}
	if getCSIDriver(t, te.client) == nil {
		t.Fatal("healthy install gate must proceed to operand apply")
	}
}

// -------------------------------------------------------------------
// Ownership race + cleanup preconditions
// -------------------------------------------------------------------

// TestSSAConflictClassifiedForeign (atomic create leg, fake-client
// double): the gate saw nothing, then a concurrent foreign create lands
// BEFORE our create — the API returns AlreadyExists, the fresh re-read
// reclassifies to foreignConflict and never adopts. The create leg is
// exercised through mergeApplier (the documented fake-client double): the
// production ssaApplier's create is a sentinel-RV SSA apply, which the fake
// client cannot serve (kubernetes/kubernetes#115598). The REAL-API equivalent
// of this exact race — including the sentinel 409 leaving the foreign object
// byte-identical — is TestEnvtestCreateRaceForeignNeverAdopted.
func TestSSAConflictClassifiedForeign(t *testing.T) {
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isCD := obj.(*storagev1.CSIDriver); isCD {
				// Simulate the concurrent foreign create landing in the
				// gate→create gap.
				foreign := &storagev1.CSIDriver{
					ObjectMeta: metav1.ObjectMeta{
						Name:   obj.GetName(),
						Labels: map[string]string{"managed-by": "user"},
					},
				}
				_ = c.Create(ctx, foreign)
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithInterceptorFuncs(funcs).
		Build()
	a := mergeApplier{c: cl}
	cd := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com"}}
	err := a.applyOwned(context.Background(), cd, "uid-test")
	var fc *foreignConflict
	if !errors.As(err, &fc) {
		t.Fatalf("create race on a foreign object must classify as foreignConflict, got %v", err)
	}
	// The foreign object is untouched by the apply.
	got := &storagev1.CSIDriver{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["managed-by"] != "user" || got.GetAnnotations()[renderer.OwnerUIDAnnotation] != "" {
		t.Fatal("foreign object was modified (no adoption allowed)")
	}
}

// TestSSAConflictUIDSwapDetected: the gate verified an owned object at
// UID A; between the gate and the apply the object is REPLACED (UID B) while
// keeping the ownership annotation — annotation alone is not trusted proof.
// The post-conflict fresh read must catch the UID mismatch and classify
// foreignConflict (never adopt, never force).
func TestSSAConflictUIDSwapDetected(t *testing.T) {
	owned := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nfs.csi.ngxstorage.com",
			UID:         "uid-original",
			Annotations: map[string]string{renderer.OwnerUIDAnnotation: "uid-test"},
		},
	}
	var swapped bool
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isCD := obj.(*storagev1.CSIDriver); isCD {
				if !swapped {
					// The replacement lands in the gate→apply gap: same name
					// and annotation, DIFFERENT UID. The fake client allows a
					// UID edit via Update (the real API would not — the point
					// is the UID comparison, exercised here).
					live := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com"}}
					if err := c.Get(ctx, types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, live); err == nil {
						live.SetUID("uid-swapped")
						_ = c.Update(ctx, live)
					}
					swapped = true
					return apierrors.NewConflict(schema.GroupResource{Group: "storage.k8s.io", Resource: "csidrivers"}, obj.GetName(), errors.New("object changed since apply precondition"))
				}
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.k8s.io", Resource: "csidrivers"}, obj.GetName(), errors.New("conflict"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(owned).
		WithInterceptorFuncs(funcs).
		Build()
	a := ssaApplier{c: cl}
	cd := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com"}}
	err := a.applyOwned(context.Background(), cd, "uid-test")
	var fc *foreignConflict
	if !errors.As(err, &fc) {
		t.Fatalf("UID swap between gate and apply must classify as foreignConflict, got %v", err)
	}
	got := &storagev1.CSIDriver{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, got); err != nil {
		t.Fatal(err)
	}
	if got.GetUID() != "uid-swapped" {
		t.Fatalf("swapped object must remain (never deleted/adopted), UID=%s", got.GetUID())
	}
}

// TestSSAConflictOwnedObjectFieldOwnershipTerminal: a 409 that carries API
// error
// CAUSES — SSA field-ownership refusal (another manager owns a contested
// field, even when the object is still ours by UID + annotation) — is
// terminal IMMEDIATELY: the fresh re-read verifies ownership, the causes
// prove the conflict cannot self-heal on a fresh RV, so the bound is moot
// and no requeue is scheduled (foreignConflict → admin decision).
// The no-force contract holds: never ForceOwnership, the stolen field
// stays stolen, the object is left exactly as found. A cause-free RV
// conflict, by contrast, converges within the bound (TransientRetry) or —
// sustained — stays retryable (RVOnlyExhaustedRetryable below).
func TestSSAConflictOwnedObjectFieldOwnershipTerminal(t *testing.T) {
	owned := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nfs.csi.ngxstorage.com",
			UID:         "uid-existing",
			Annotations: map[string]string{renderer.OwnerUIDAnnotation: "uid-test"},
		},
	}
	fieldOwnershipConflict := func() error {
		// The API server's SSA refusal shape: 409 with StatusCause entries.
		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    409,
			Reason:  metav1.StatusReasonConflict,
			Message: "Operation cannot be fulfilled on csidrivers.storage.k8s.io \"nfs.csi.ngxstorage.com\": field \"spec\" is owned by another manager",
			Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{
				{Type: "FieldManagerConflict", Message: `conflict with "evil-manager" .spec.fsGroupPolicy`},
			}},
		}}
	}
	var patchAttempts int
	var anyForce bool
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isCD := obj.(*storagev1.CSIDriver); isCD {
				// client.ForceOwnership is a non-exported struct; detect it
				// by applying each option to a PatchOptions and reading Force.
				var po client.PatchOptions
				for _, o := range opts {
					o.ApplyToPatch(&po)
				}
				if po.Force != nil && *po.Force {
					anyForce = true
				}
				patchAttempts++
				// Every no-force apply is refused by the API with causes.
				return fieldOwnershipConflict()
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(owned).
		WithInterceptorFuncs(funcs).
		Build()
	a := ssaApplier{c: cl}
	cd := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com"}}
	err := a.applyOwned(context.Background(), cd, "uid-test")
	var fc *foreignConflict
	if !errors.As(err, &fc) {
		t.Fatalf("causes-bearing conflict must classify terminal foreignConflict, got %v", err)
	}
	if !strings.Contains(fc.Error(), "no-force contract") || !strings.Contains(fc.Error(), "field-ownership") {
		t.Fatalf("must be the field-ownership no-force branch, got %q", err)
	}
	if patchAttempts != 1 {
		t.Fatalf("apply attempts = %d, want 1 — a causes-bearing conflict is terminal on the FIRST refusal, the bound is moot", patchAttempts)
	}
	if anyForce {
		t.Fatal("the apply path must never use client.ForceOwnership")
	}
	// The object is untouched (no repair write landed).
	got := &storagev1.CSIDriver{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, got); err != nil {
		t.Fatal(err)
	}
	if got.GetUID() != "uid-existing" || got.GetAnnotations()[renderer.OwnerUIDAnnotation] != "uid-test" {
		t.Fatal("contended object must remain exactly as found")
	}
}

// TestSSAConflictOwnedObjectRVOnlyExhaustedRetryable (unit half): a
// PERSISTENT cause-free 409 on an object still
// verified as ours — sustained benign concurrency consuming every bounded
// retry — must NOT be diagnosed as foreign field ownership. The raw
// conflict propagates so classifyApplyError requeues it retryable; only
// causes prove a field steal (terminal), only ownership loss proves a
// takeover (terminal). The envtest half proves the same against the real
// API (TestEnvtestSustainedBenignConcurrencyNoForeignDiagnosis).
func TestSSAConflictOwnedObjectRVOnlyExhaustedRetryable(t *testing.T) {
	owned := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nfs.csi.ngxstorage.com",
			UID:         "uid-existing",
			Annotations: map[string]string{renderer.OwnerUIDAnnotation: "uid-test"},
		},
	}
	var patchAttempts int
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isCD := obj.(*storagev1.CSIDriver); isCD {
				patchAttempts++
				// Cause-free RV-precondition conflict ("object has been
				// modified") — the shape of a benign concurrent write.
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.k8s.io", Resource: "csidrivers"}, obj.GetName(), errors.New("object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(owned).
		WithInterceptorFuncs(funcs).
		Build()
	a := ssaApplier{c: cl}
	cd := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com"}}
	err := a.applyOwned(context.Background(), cd, "uid-test")
	if err == nil {
		t.Fatal("exhausted bound must not look successful")
	}
	var fc *foreignConflict
	if errors.As(err, &fc) {
		t.Fatalf("cause-free conflict exhaustion must NOT diagnose foreign field ownership, got %v", err)
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("the raw RV conflict must propagate for retryable classification, got %v", err)
	}
	if isFieldOwnershipConflict(err) {
		t.Fatalf("apierrors.NewConflict carries no causes — the discriminator misfired: %v", err)
	}
	reason, retryable := classifyApplyError(err)
	if reason != v1alpha1.ReasonApplyConflict || !retryable {
		t.Fatalf("exhausted RV-only conflict must classify (ApplyConflict, retryable), got (%q %v)", reason, retryable)
	}
	if patchAttempts != updateConflictRetries {
		t.Fatalf("apply attempts = %d, want the bounded %d", patchAttempts, updateConflictRetries)
	}
}

// TestSSAConflictOwnedObjectTransientRetry (positive half):
// the same conflict that resolves on the SECOND attempt (a concurrent benign
// write bumped the RV in the gate→apply gap) must converge inside the bounded
// retry — nil error, no requeue — with the fresh RV precondition each time.
func TestSSAConflictOwnedObjectTransientRetry(t *testing.T) {
	owned := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nfs.csi.ngxstorage.com",
			UID:         "uid-existing",
			Annotations: map[string]string{renderer.OwnerUIDAnnotation: "uid-test"},
		},
	}
	var patchAttempts int
	var seenRVs []string
	// The interceptor short-circuits EVERY attempt (the fake client rejects
	// apply patches outright — kubernetes/kubernetes#115598 — so the SSA
	// semantics under test are the retry control flow, not the write itself):
	// attempt 1 409s, attempt 2 succeeds. The RV precondition each attempt
	// carries is captured and asserted below.
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isCD := obj.(*storagev1.CSIDriver); isCD {
				patchAttempts++
				seenRVs = append(seenRVs, obj.GetResourceVersion())
				if patchAttempts == 1 {
					// The benign concurrent write landed between the gate's
					// read and the first apply: stale precondition → 409.
					return apierrors.NewConflict(schema.GroupResource{Group: "storage.k8s.io", Resource: "csidrivers"}, obj.GetName(), errors.New("object has been modified"))
				}
				return nil
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(owned).
		WithInterceptorFuncs(funcs).
		Build()
	a := ssaApplier{c: cl}
	cd := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com"}}
	if err := a.applyOwned(context.Background(), cd, "uid-test"); err != nil {
		t.Fatalf("transient conflict must converge within the bounded retry, got %v", err)
	}
	if patchAttempts != 2 {
		t.Fatalf("apply attempts = %d, want 2 (first stale, retried on the fresh RV)", patchAttempts)
	}
	if seenRVs[0] == "" || seenRVs[1] == "" {
		t.Fatalf("every apply attempt must carry the optimistic-lock RV precondition, saw %v", seenRVs)
	}
}

// TestCleanupVerifySkipsReplacedObject: between enumeration and deletion an
// attacker strips the ownership annotation (a real API forbids UID edits, so
// annotation-tamper is the realistic race — ownership annotations are not,
// alone, trusted proof). The precondition re-GET must detect the change and
// SKIP — never delete an object that changed under us.
func TestCleanupVerifySkipsReplacedObject(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	q := te.get(t, v1alpha1.QuartetName)

	live := getCSIDriver(t, te.client)
	snapshot := live.DeepCopy() // the enumerated list entry (still annotated)

	// Attacker strips the ownership annotation; UID is unchanged.
	tampered := live.DeepCopy()
	ann := tampered.GetAnnotations()
	delete(ann, renderer.OwnerUIDAnnotation)
	tampered.SetAnnotations(ann)
	if err := te.client.Update(context.Background(), tampered); err != nil {
		t.Fatal(err)
	}

	err := te.reconciler.verifyAndDeleteOwned(context.Background(), te.client,
		logf.Log.WithName("test"), snapshot, string(q.UID))
	if !errors.Is(err, errCleanupSkipped) {
		t.Fatalf("annotation-stripped object must be skipped, got %v", err)
	}
	// The tampered object survives (not deleted).
	got := &storagev1.CSIDriver{}
	owned(t, te.client, types.NamespacedName{Name: snapshot.Name}, got)
	if _, has := got.GetAnnotations()[renderer.OwnerUIDAnnotation]; has {
		t.Fatal("unexpected: annotation reappeared")
	}
}

// TestCleanupVerifySkipsUIDSwap: the same guard catches a UID mismatch
// (exercised via the fake client, which permits a UID change the real API
// would reject — this isolates the comparison logic).
func TestCleanupVerifySkipsUIDSwap(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	q := te.get(t, v1alpha1.QuartetName)

	live := getCSIDriver(t, te.client)
	snapshot := live.DeepCopy()

	replaced := live.DeepCopy()
	replaced.SetUID("some-other-uid")
	if err := te.client.Update(context.Background(), replaced); err != nil {
		t.Fatal(err)
	}

	err := te.reconciler.verifyAndDeleteOwned(context.Background(), te.client,
		logf.Log.WithName("test"), snapshot, string(q.UID))
	if !errors.Is(err, errCleanupSkipped) {
		t.Fatalf("UID-swapped object must be skipped, got %v", err)
	}
}

// TestCleanupPostconditionFinalizer: a delete ACCEPT is not a deletion. An
// operand that still exists after delete (carrying a finalizer we do not
// own) must produce a requeue-able error, not a silent success.
func TestCleanupPostconditionFinalizer(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	q := te.get(t, v1alpha1.QuartetName)

	cm := &corev1.ConfigMap{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-coordination-config", Namespace: "ngxstorage-nfs"}, cm)
	cm.Finalizers = append(cm.Finalizers, "external.example.com/keep")
	if err := te.client.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	snapshot := cm.DeepCopy()

	err := te.reconciler.verifyAndDeleteOwned(context.Background(), te.client,
		logf.Log.WithName("test"), snapshot, string(q.UID))
	if err == nil {
		t.Fatal("finalized operand must not report successful cleanup")
	}
	if errors.Is(err, errCleanupSkipped) {
		t.Fatal("finalizer-held object is OURS — this is a retryable failure, not a skip")
	}
	// The object is still present, terminating, finalizer retained — the
	// postcondition keeps cleanup retryable instead of reporting success.
	still := &corev1.ConfigMap{}
	owned(t, te.client, types.NamespacedName{Name: snapshot.Name, Namespace: snapshot.Namespace}, still)
	if still.DeletionTimestamp.IsZero() {
		t.Fatal("object must be terminating after the accepted delete")
	}
	if len(still.Finalizers) == 0 {
		t.Fatal("external finalizer must still be present (we never remove it)")
	}
}

// -------------------------------------------------------------------
// Per-watch predicates
// -------------------------------------------------------------------

// TestMainResourcePredicateTriggers: the CR watch fires on generation,
// deletionTimestamp and finalizer transitions — and NOT on status-only
// writes (which the old global GenerationChangedPredicate also suppressed,
// but while suppressing deletion handling too).
func TestMainResourcePredicateTriggers(t *testing.T) {
	p := mainResourcePredicate()
	mk := func(mutate func(*v1alpha1.Quartet)) *v1alpha1.Quartet {
		q := &v1alpha1.Quartet{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.QuartetName, Generation: 1}}
		if mutate != nil {
			mutate(q)
		}
		return q
	}
	ucase := func(oldObj, newObj *v1alpha1.Quartet) bool {
		return p.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
	}

	if ucase(mk(nil), mk(nil)) {
		t.Fatal("status-only (generation-equal) update must not fire")
	}
	if !ucase(mk(nil), mk(func(q *v1alpha1.Quartet) { q.Generation = 2 })) {
		t.Fatal("generation bump must fire")
	}
	now := metav1.Now()
	if !ucase(mk(nil), mk(func(q *v1alpha1.Quartet) { q.DeletionTimestamp = &now })) {
		t.Fatal("deletionTimestamp transition must fire (finalizer cleanup starvation)")
	}
	if !ucase(mk(func(q *v1alpha1.Quartet) { q.Finalizers = []string{"a"} }),
		mk(func(q *v1alpha1.Quartet) { q.Finalizers = []string{"a", "b"} })) {
		t.Fatal("finalizer change must fire")
	}
	if !p.Delete(event.DeleteEvent{Object: mk(nil)}) {
		t.Fatal("delete event must fire")
	}
}

// TestLeasePredicateIgnoresRenewal: Lease renewal churn (holder identity +
// renew timestamps) must NOT enqueue — the coordination state is
// driver-owned and updates at high frequency.
func TestLeasePredicateIgnoresRenewal(t *testing.T) {
	p := leasePredicate()
	ann := map[string]string{renderer.OwnerUIDAnnotation: "u1"}
	oldObj := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: "nfs-csi-ngxstorage-operation-lock-000", Namespace: "ngxstorage-nfs",
		Annotations: ann,
	}}
	t1 := metav1.NewMicroTime(time.Now())
	oldObj.Spec.RenewTime = &t1

	renewed := oldObj.DeepCopy()
	t2 := metav1.NewMicroTime(t1.Time.Add(time.Second))
	renewed.Spec.HolderIdentity = ptr.To("driver-pod-2")
	renewed.Spec.RenewTime = &t2
	if p.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: renewed}) {
		t.Fatal("Lease renewal must not trigger a reconcile")
	}

	stripped := renewed.DeepCopy()
	delete(stripped.Annotations, renderer.OwnerUIDAnnotation)
	if !p.Update(event.UpdateEvent{ObjectOld: renewed, ObjectNew: stripped}) {
		t.Fatal("ownership annotation loss must trigger a reconcile (ownership drift)")
	}
}

// TestPredicateForObjectSelection: each child watch kind gets its intended
// predicate; SCC (unstructured) shares the ownership-change gate.
func TestPredicateForObjectSelection(t *testing.T) {
	renewal := func() event.UpdateEvent {
		oldObj := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{renderer.OwnerUIDAnnotation: "u1"}}}
		newObj := oldObj.DeepCopy()
		now := metav1.NewMicroTime(time.Now())
		newObj.Spec.RenewTime = &now
		return event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}
	}

	for obj, wantRenewalFire := range map[client.Object]bool{
		&coordinationv1.Lease{}:      false, // churn-excluded
		&unstructured.Unstructured{}: false, // SCC: ownership-change only
		&appsv1.Deployment{}:         true,  // drift source
		&corev1.ConfigMap{}:          true,
		&rbacv1.Role{}:               true,
		&storagev1.CSIDriver{}:       true,
	} {
		if got := predicateForObject(obj).Update(renewal()); got != wantRenewalFire {
			t.Errorf("predicateForObject(%T).Update(renewal-like) = %v, want %v", obj, got, wantRenewalFire)
		}
	}

	// Pod gate: kubelet status churn (no waiting change) is suppressed; a
	// container entering CreateContainerConfigError fires exactly once.
	healthy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ngxstorage-nfs"}}
	churn := healthy.DeepCopy()
	churn.Status.Phase = corev1.PodRunning
	if predicateForObject(&corev1.Pod{}).Update(event.UpdateEvent{ObjectOld: healthy, ObjectNew: churn}) {
		t.Fatal("pod status churn without waiting change must not fire")
	}
	blocked := churn.DeepCopy()
	blocked.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "c",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}},
	}}
	if !predicateForObject(&corev1.Pod{}).Update(event.UpdateEvent{ObjectOld: churn, ObjectNew: blocked}) {
		t.Fatal("pod entering CreateContainerConfigError must fire")
	}
}

// TestOperandEnqueuerFilters: the annotation gate plus the pod namespace
// gate decide what reaches the singleton workqueue.
func TestOperandEnqueuerFilters(t *testing.T) {
	r := &QuartetReconciler{}
	ownedRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: "x", Namespace: "ngxstorage-nfs",
		Annotations: map[string]string{renderer.OwnerUIDAnnotation: "u1"},
	}}
	if reqs := r.operandEnqueuer(context.Background(), ownedRole); len(reqs) != 1 || reqs[0].Name != v1alpha1.QuartetName {
		t.Fatalf("owned child must enqueue the singleton, got %v", reqs)
	}
	foreignRole := ownedRole.DeepCopy()
	foreignRole.Annotations = nil
	if reqs := r.operandEnqueuer(context.Background(), foreignRole); len(reqs) != 0 {
		t.Fatalf("foreign child must not enqueue, got %v", reqs)
	}
	podInNS := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ngxstorage-nfs"}}
	if reqs := r.operandEnqueuer(context.Background(), podInNS); len(reqs) != 1 {
		t.Fatalf("operand-namespace pod must enqueue (kubelet-created, unstamped), got %v", reqs)
	}
	podElsewhere := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "kube-system"}}
	if reqs := r.operandEnqueuer(context.Background(), podElsewhere); len(reqs) != 0 {
		t.Fatalf("pod outside operand namespaces must not enqueue, got %v", reqs)
	}
}

// -------------------------------------------------------------------
// Conditional SCC watch (single drift mechanism)
// -------------------------------------------------------------------

// TestSCCWatchConditional: discovery decides — OCP registers the unstructured
// SCC watch (and its GVKs), plain Kubernetes does not.
func TestSCCWatchConditional(t *testing.T) {
	if !sccWatchEnabled(fakeDiscovery{groups: ocpGroups()}) {
		t.Fatal("OpenShift discovery must enable the SCC watch")
	}
	if sccWatchEnabled(fakeDiscovery{groups: k8sGroups()}) {
		t.Fatal("plain Kubernetes must not enable the SCC watch")
	}
	// Forbidden discovery: run SCC-free, never block setup.
	if sccWatchEnabled(fakeDiscovery{err: apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("denied"))}) {
		t.Fatal("forbidden discovery must disable the SCC watch")
	}

	scheme := newTestScheme(t)
	cfg := &rest.Config{Host: "http://127.0.0.1:1"} // never dialed: setup only
	skipValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             ctrlConfig(skipValidation),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &QuartetReconciler{
		Client:            mgr.GetClient(),
		Scheme:            scheme,
		Log:               logf.Log.WithName("test"),
		PlatformDiscovery: fakeDiscovery{groups: ocpGroups()},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager with the SCC watch: %v", err)
	}
	if !scheme.Recognizes(renderer.SCCGVK) {
		t.Fatal("SCC watch must register the unstructured GVK in the scheme")
	}

	// Kubernetes path: the same setup must succeed WITHOUT registering SCC.
	schemeK := newTestScheme(t)
	mgrK, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 schemeK,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             ctrlConfig(skipValidation),
	})
	if err != nil {
		t.Fatal(err)
	}
	rK := &QuartetReconciler{
		Client:            mgrK.GetClient(),
		Scheme:            schemeK,
		Log:               logf.Log.WithName("test"),
		PlatformDiscovery: fakeDiscovery{groups: k8sGroups()},
	}
	if err := rK.SetupWithManager(mgrK); err != nil {
		t.Fatalf("SetupWithManager without the SCC watch: %v", err)
	}
	if schemeK.Recognizes(renderer.SCCGVK) {
		t.Fatal("plain Kubernetes must not register the SCC GVK")
	}
}

// -------------------------------------------------------------------
// Envtest mode selection
// -------------------------------------------------------------------

// TestEnvtestModeDefault: required when assets are present (silent-skip
// window closed), optional without, explicit flag wins both ways.
func TestEnvtestModeDefault(t *testing.T) {
	cases := []struct {
		name, flag, assets string
		want               bool
	}{
		{"no flag no assets", "", "", false},
		{"no flag with assets", "", "/some/path", true},
		{"required no assets", "required", "", true},
		{"optional with assets", "optional", "/some/path", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUARTET_TEST_ENVTEST", tc.flag)
			if tc.assets == "" {
				t.Setenv("KUBEBUILDER_ASSETS", "")
			} else {
				t.Setenv("KUBEBUILDER_ASSETS", tc.assets)
			}
			if got := envtestRequiredMode(); got != tc.want {
				t.Fatalf("envtestRequiredMode() = %v, want %v", got, tc.want)
			}
		})
	}
}

var testImageRef = "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("a", 64)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// validCR builds a spec-valid singleton at generation 1.
func validCR() *v1alpha1.Quartet {
	return &v1alpha1.Quartet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       v1alpha1.QuartetName,
			Namespace:  "",
			Generation: 1,
		},
		Spec: v1alpha1.QuartetSpec{
			Platform: v1alpha1.PlatformAuto,
			LogLevel: v1alpha1.LogLevelInfo,
			Profiles: v1alpha1.Profiles{
				NFS: v1alpha1.ProfileSpec{
					Enabled:   true,
					SecretRef: &v1alpha1.SecretRef{Name: "nfs-csi-ngxstorage-config"},
					Image:     &v1alpha1.ImageSpec{Reference: testImageRef},
				},
			},
		},
	}
}

type fakeEnv struct {
	reconciler *QuartetReconciler
	client     client.Client
	recorder   *record.FakeRecorder
}

func newTestEnv(t *testing.T, objs ...client.Object) *fakeEnv {
	t.Helper()
	// Pre-seed the k8s installer-owned RBAC set: the
	// out-of-band model verifies operand RBAC at reconcile start, so the
	// fake env carries a healthy install for the RBACDrift gate to pass.
	objs = append(objs, installRBACSeed(v1alpha1.PlatformKubernetes)...)
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(seedUIDs(objs)...).
		WithStatusSubresource(&v1alpha1.Quartet{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).
		Build()
	rec := record.NewFakeRecorder(100)
	return &fakeEnv{
		reconciler: &QuartetReconciler{
			Client:            cl,
			Scheme:            cl.Scheme(),
			Log:               logf.Log.WithName("test"),
			Recorder:          rec,
			PlatformDiscovery: k8sDiscovery(),
			// The fake client rejects apply patches (#115598); the merge
			// double carries equivalent ownership semantics for unit tests.
			Applier: mergeApplier{c: cl},
		},
		client:   cl,
		recorder: rec,
	}
}

func k8sDiscovery() fakeDiscovery { return fakeDiscovery{groups: k8sGroups()} }

func (te *fakeEnv) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := te.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile(%q) error: %v", name, err)
	}
	return res
}

func (te *fakeEnv) get(t *testing.T, name string) *v1alpha1.Quartet {
	t.Helper()
	q := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: name}, q); err != nil {
		t.Fatalf("get %q: %v", name, err)
	}
	return q
}

func condition(t *testing.T, conds []metav1.Condition, condType string) *metav1.Condition {
	t.Helper()
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	t.Fatalf("condition %q not found in %v", condType, conds)
	return nil
}

func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestReconcileValidSpecResolvesKubernetes(t *testing.T) {
	te := newTestEnv(t, validCR())
	// A valid spec reaches Ready only after the rendered
	// workloads converge (apply alone is not readiness).
	res := convergeReadiness(t, te)
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("steady reconcile must not requeue, got %+v", res)
	}
	q := te.get(t, v1alpha1.QuartetName)
	if q.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", q.Status.ObservedGeneration)
	}
	if q.Status.SpecPlatform != v1alpha1.PlatformAuto {
		t.Errorf("specPlatform = %q, want auto", q.Status.SpecPlatform)
	}
	if q.Status.ResolvedPlatform != v1alpha1.PlatformKubernetes {
		t.Errorf("resolvedPlatform = %q, want kubernetes", q.Status.ResolvedPlatform)
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonReady {
		t.Errorf("aggregate Ready = (%v, %q), want (True, Ready)", c.Status, c.Reason)
	}
	ps, ok := q.Status.Profiles[v1alpha1.ProfileNFS]
	if !ok {
		t.Fatal("status.profiles[nfs] missing for enabled profile")
	}
	if c := condition(t, ps.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Errorf("profile Ready = %v, want True", c.Status)
	}
	if _, ok := q.Status.Profiles[v1alpha1.ProfileFC]; ok {
		t.Error("disabled profile must not appear in status.profiles")
	}
}

func TestReconcileRejectsNonSingletonName(t *testing.T) {
	q := validCR()
	q.Name = "quartet-extra"
	te := newTestEnv(t, q)
	res := te.reconcile(t, "quartet-extra")
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("singleton violation must be terminal, got %+v", res)
	}
	got := te.get(t, "quartet-extra")
	c := condition(t, got.Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("Ready = (%v, %q), want (False, InvalidSpec)", c.Status, c.Reason)
	}
	if got.Status.SpecPlatform != "" {
		t.Error("rejected CR must not record an accepted specPlatform")
	}
	// Strengthened singleton claim: the rejected foreign-named CR never manages
	// operands — no operand kind, no operand namespace (the API accepts
	// the name; singleton enforcement is reconciler-side only).
	assertNoOperands(t, te.client)
	events := drainEvents(te.recorder)
	if len(events) == 0 || !strings.Contains(events[len(events)-1], v1alpha1.ReasonInvalidSpec) {
		t.Errorf("expected an InvalidSpec warning event, got %v", events)
	}
}

// TestSingletonRaceSimulated: two Quartet objects in the same list — only the
// fixed name is reconciled into operand state; the other gets InvalidSpec
// (the API-level create race is covered separately in envtest).
func TestSingletonRaceSimulated(t *testing.T) {
	// Two CRs in the same list, as a concurrent-create race would leave
	// behind. The reconciler must only drive `quartet` into operand state.
	second := validCR()
	second.Name = "quartet-2"
	te := newTestEnv(t, validCR(), second)
	convergeReadiness(t, te)
	te.reconcile(t, "quartet-2")

	good := te.get(t, v1alpha1.QuartetName)
	if c := condition(t, good.Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Errorf("quartet must be Ready, got (%v %q)", c.Status, c.Reason)
	}
	other := te.get(t, "quartet-2")
	if c := condition(t, other.Status.Conditions, v1alpha1.ConditionReady); c.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("quartet-2 must be InvalidSpec, got (%v %q)", c.Status, c.Reason)
	}
	if other.Status.Profiles != nil {
		t.Error("non-singleton must not produce profile status claims")
	}
}

func TestReconcileInvalidSpecVariants(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(q *v1alpha1.Quartet)
	}{
		{"bad platform enum", func(q *v1alpha1.Quartet) { q.Spec.Platform = v1alpha1.Platform("windows") }},
		{"enabled without secretRef", func(q *v1alpha1.Quartet) { q.Spec.Profiles.NFS.SecretRef = nil }},
		{"enabled without image", func(q *v1alpha1.Quartet) { q.Spec.Profiles.NFS.Image = nil }},
		{"image tag rejected", func(q *v1alpha1.Quartet) {
			q.Spec.Profiles.NFS.Image.Reference = "quay.io/ngxstorage/nfs:v1.2.3"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validCR()
			tc.mutate(q)
			te := newTestEnv(t, q)
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.Requeue || res.RequeueAfter != 0 {
				t.Fatalf("spec validation must be terminal, got %+v", res)
			}
			c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
			if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonInvalidSpec {
				t.Fatalf("Ready = (%v, %q), want (False, InvalidSpec)", c.Status, c.Reason)
			}
		})
	}
}

func TestImmutablePlatformReconcilerSideCheck(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.Platform = v1alpha1.PlatformOpenShift
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("platform change must be InvalidSpec, got (%v, %q)", c.Status, c.Reason)
	}
}

// TestStatusChangeGuard: a no-op reconcile must not write status (deep-equal
// guard). Interceptor counts Status().Update calls.
func TestStatusChangeGuard(t *testing.T) {
	var statusUpdates int
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(validCR()).
		WithStatusSubresource(&v1alpha1.Quartet{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResource == "status" {
					statusUpdates++
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		Build()
	rec := record.NewFakeRecorder(100)
	te := &fakeEnv{
		reconciler: &QuartetReconciler{
			Client:            cl,
			Scheme:            cl.Scheme(),
			Log:               logf.Log.WithName("test"),
			Recorder:          rec,
			PlatformDiscovery: k8sDiscovery(),
			Applier:           mergeApplier{c: cl},
		},
		client:   cl,
		recorder: rec,
	}

	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName)
	if statusUpdates != 1 {
		t.Fatalf("status written %d times across 3 reconciles, want 1", statusUpdates)
	}
}

// TestEventDedupe: the same steady-state failure emits exactly one warning
// event (bounds event churn).
func TestEventDedupe(t *testing.T) {
	q := validCR()
	q.Spec.Profiles.NFS.Image = nil
	te := newTestEnv(t, q)
	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName)
	events := drainEvents(te.recorder)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], corev1.EventTypeWarning) {
		t.Fatalf("event should be a Warning, got %q", events[0])
	}
}

func TestDetectionFailureReasonsAndRequeue(t *testing.T) {
	cases := []struct {
		name      string
		discovery fakeDiscovery
		want      string
	}{
		{"override conflict", fakeDiscovery{groups: k8sGroups()}, v1alpha1.ReasonPlatformDetectionFailed},
		{"forbidden", fakeDiscovery{err: apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("denied"))}, v1alpha1.ReasonPermissionDenied},
		{"network", fakeDiscovery{err: errors.New("connection refused")}, v1alpha1.ReasonDependencyMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validCR()
			if tc.name == "override conflict" {
				q.Spec.Platform = v1alpha1.PlatformOpenShift
			}
			te := newTestEnv(t, q)
			te.reconciler.PlatformDiscovery = tc.discovery
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter <= 0 {
				t.Fatalf("detection failure must requeue, got %+v", res)
			}
			c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
			if c.Status != metav1.ConditionFalse || c.Reason != tc.want {
				t.Fatalf("Ready = (%v, %q), want (False, %q)", c.Status, c.Reason, tc.want)
			}
		})
	}
}

func TestTerminatingCRNoStatusWrite(t *testing.T) {
	var statusUpdates int
	now := metav1.Now()
	q := validCR()
	// The fake client only accepts a DeletionTimestamp when a finalizer is
	// present.
	q.Finalizers = []string{"quartet.ngxstorage.com/test"}
	q.DeletionTimestamp = &now

	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(q).
		WithStatusSubresource(&v1alpha1.Quartet{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResource == "status" {
					statusUpdates++
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		Build()
	te := newTestEnv(t)
	te.client = cl
	te.reconciler.Client = cl
	te.reconciler.Applier = mergeApplier{c: cl}

	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("terminating CR must not requeue, got %+v", res)
	}
	if statusUpdates != 0 {
		t.Fatalf("terminating CR must not write status, got %d updates", statusUpdates)
	}
}

// TestRedactionLogOutput pins the secret-redaction discipline end to end:
// the reconciler log stream — captured through the same zapr wrapping
// main.go uses around the zap logger — carries Secret NAMES (spec
// references) but never values. The operator does not read the
// Secret API at all, so no value exists anywhere in the pipeline.
func TestRedactionLogOutput(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	zlogger := zap.New(core)
	te := newTestEnv(t, validCR())
	te.reconciler.Log = zapr.NewLogger(zlogger)

	sentinel := "SUPER_SECRET_VALUE_NEVER_LOGGED"
	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName) // no-op pass must log its own distinct message

	sawSecretName := false
	sawNoopMessage := false
	for _, entry := range observed.All() {
		for _, f := range entry.Context {
			// zapr forwards slice kvs as zapcore.Any — inspect the rendered
			// value, not just typed-string fields.
			rendered := fmt.Sprintf("%s%v", f.String, f.Interface)
			if strings.Contains(rendered, sentinel) {
				t.Fatalf("redaction violated: sentinel value present in log field %+v", entry)
			}
			if strings.Contains(rendered, "nfs-csi-ngxstorage-config") {
				sawSecretName = true
			}
		}
		if strings.Contains(entry.Message, sentinel) {
			t.Fatalf("redaction violated in message %q", entry.Message)
		}
		if strings.Contains(entry.Message, "status unchanged") {
			sawNoopMessage = true
		}
	}
	if !sawSecretName {
		t.Error("expected Secret NAME to appear in the log (allowed); it did not")
	}
	if !sawNoopMessage {
		t.Error("expected the distinct no-op reconcile message; it did not appear")
	}
}
