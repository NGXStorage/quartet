package controller

import (
	"context"
	"errors"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"testing"
	"time"
)

const gateNS = "ngxstorage-nfs"

// --- seed helpers (fake client: status subresource is NOT registered for
// PVC/PV/VA, so plain Create stores the full object incl. status). ---
//
// Usage must carry the profile's provisioner identity to count.
// seedDriverPV creates a Bound PV recognized by the scan (spec.csi.driver ==
// the NFS driver, volumeHandle under the driver prefix); seedForeignPV a
// plain hostPath PV that NO profile scan recognizes.

// seedBoundPVC seeds a Bound PVC in the operand namespace tied to the named
// PV (spec.volumeName). A Bound PVC with NO driver PV behind it must not
// block (foreign storage).
func seedBoundPVC(t *testing.T, c client.Client, name string) {
	t.Helper()
	seedBoundPVCOnPV(t, c, name, name)
}

func seedBoundPVCOnPV(t *testing.T, c client.Client, name, volumeName string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gateNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  volumeName,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if err := c.Create(context.Background(), pvc); err != nil {
		t.Fatalf("seed bound PVC %q: %v", name, err)
	}
}

// seedDriverPV seeds a Bound driver-produced PV (CSI source, driver name +
// handle prefix) whose claimRef points at ns/name (claimRef is irrelevant to
// the match: any namespace counts).
func seedDriverPV(t *testing.T, c client.Client, pvName, claimNS string) {
	t.Helper()
	driverName, prefix := renderer.ProvisionerIdentity(v1alpha1.ProfileNFS)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       driverName,
					VolumeHandle: prefix + pvName,
				},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: pvName + "-pvc",
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	if err := c.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed driver PV %q: %v", pvName, err)
	}
}

// seedReleasedDriverPV seeds a driver-identity PV that has LEFT the Bound
// phase (Released). The PV itself holds no live claim and never counts as
// bound usage — but an attached VolumeAttachment on it still must block
// (the VA set is driver-identity, phase-blind).
func seedReleasedDriverPV(t *testing.T, c client.Client, pvName, claimNS string) {
	t.Helper()
	driverName, prefix := renderer.ProvisionerIdentity(v1alpha1.ProfileNFS)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       driverName,
					VolumeHandle: prefix + pvName,
				},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	if err := c.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed released driver PV %q: %v", pvName, err)
	}
}

// seedForeignPV seeds a Bound NON-driver PV (hostPath source — another
// storage, no CSI identity). Must never block the NFS gate.
func seedForeignPV(t *testing.T, c client.Client, pvName, claimNS string) {
	t.Helper()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/quartet-test/" + pvName},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: pvName + "-pvc",
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	if err := c.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed foreign PV %q: %v", pvName, err)
	}
}

// seedTerminatingBoundPVC: a PVC that is being deleted but still Bound and
// tied to the named PV — it still holds live mounts, so the gate must count
// it (fail-safe).
func seedTerminatingBoundPVC(t *testing.T, c client.Client, name, volumeName string) {
	t.Helper()
	now := metav1.Now()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: gateNS,
			Finalizers:        []string{"test.example.com/keep"},
			DeletionTimestamp: &now,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  volumeName,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if err := c.Create(context.Background(), pvc); err != nil {
		t.Fatalf("seed terminating bound PVC %q: %v", name, err)
	}
}

// seedAttachedVA: a VolumeAttachment attached to the named PV.
func seedAttachedVA(t *testing.T, c client.Client, vaName, pvName string) {
	t.Helper()
	pv := pvName
	va := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: vaName},
		Spec: storagev1.VolumeAttachmentSpec{
			NodeName: "node-1",
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
	if err := c.Create(context.Background(), va); err != nil {
		t.Fatalf("seed attached VA %q: %v", vaName, err)
	}
}

// pvcListForbidden blocks the gate's PVC list with a 403 (boundary
// simulation: the API's response is what is faked, the client objects are
// real).
func pvcListForbidden() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isPVC := list.(*corev1.PersistentVolumeClaimList); isPVC {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "", errors.New("denied by test policy"))
			}
			return c.List(ctx, list, opts...)
		},
	}
}

func pvListError() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isPV := list.(*corev1.PersistentVolumeList); isPV {
				return errors.New("connection refused (test simulation)")
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// assertGateBlocked verifies the profile+aggregate status surface of a
// blocked disable: Degraded=True, Ready=False with the given reason, and a
// retryable requeue.
func assertGateBlocked(t *testing.T, q *v1alpha1.Quartet, reason string) {
	t.Helper()
	prof := q.Status.Profiles[v1alpha1.ProfileNFS]
	if c := condition(t, prof.Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionTrue || c.Reason != reason {
		t.Fatalf("profile nfs Degraded = (%v %q), want (True %q)", c.Status, c.Reason, reason)
	}
	if c := condition(t, prof.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != reason {
		t.Fatalf("profile nfs Ready = (%v %q), want (False %q)", c.Status, c.Reason, reason)
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != reason {
		t.Fatalf("aggregate Ready = (%v %q), want (False %q)", c.Status, c.Reason, reason)
	}
}

// assertOperandsIntact verifies the block actually cancelled the cleanup:
// every operand kind the profile creates is still present.
func assertOperandsIntact(t *testing.T, c client.Client) {
	t.Helper()
	if getDeployment(t, c) == nil {
		t.Fatal("blocked disable/delete must retain the controller Deployment")
	}
	if getCSIDriver(t, c) == nil {
		t.Fatal("blocked disable/delete must retain the CSIDriver")
	}
}

// disableNFS flips the singleton to nfs.enabled=false and reconciles once.
func disableNFS(t *testing.T, te *fakeEnv) {
	t.Helper()
	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.Profiles.NFS.Enabled = false
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)
}

// -------------------------------------------------------------------
// Disable gate
// -------------------------------------------------------------------

// TestDisableBlockedByBoundPVC: a Bound PVC in the operand
// namespace tied to a DRIVER PV blocks the disable — Degraded/Ready surface
// DataInUse, operands are retained, the reconcile requeues (fail-safe
// retry).
func TestDisableBlockedByBoundPVC(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "data-pv", gateNS)
	seedBoundPVCOnPV(t, te.client, "data-pvc", "data-pv")

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName) // block must persist on replay
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("blocked disable must requeue to replay the gate, got %+v", res)
	}
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
	q := te.get(t, v1alpha1.QuartetName)
	msg := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady).Message
	if msg == "" {
		t.Fatal("blocked profile condition must carry the usage summary")
	}
}

// TestDisableBlockedByAttachedPVAndVA: a DRIVER PV bound to a claim
// in the operand namespace plus an attached VolumeAttachment blocks the
// disable.
func TestDisableBlockedByAttachedPVAndVA(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "driver-pv", gateNS)
	seedAttachedVA(t, te.client, "va-driver-pv", "driver-pv")

	disableNFS(t, te)
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
}

// TestDisableBlockedByReleasedDriverPVAttachedVA: a driver-produced PV that
// has transitioned OUT of Bound (Released)
// but still carries an attached VolumeAttachment must BLOCK the disable.
// The old scan tied the VA set to the Bound/terminating PV set, so this
// Released-phase attached VA escaped the gate — the attachment keeps the
// data mounted on a node even though the PV no longer counts as bound
// usage. The VA set is now driver-identity and phase-blind.
func TestDisableBlockedByReleasedDriverPVAttachedVA(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedReleasedDriverPV(t, te.client, "released-driver-pv", gateNS)
	seedAttachedVA(t, te.client, "va-released-driver-pv", "released-driver-pv")

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("Released driver PV with an attached VA must block (requeue), got %+v", res)
	}
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
	msg := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady).Message
	if !strings.Contains(msg, "va-released-driver-pv") {
		t.Fatalf("blocked message must name the attached VolumeAttachment on the Released driver PV: %q", msg)
	}
}

// TestDisableBlockedByOtherNamespaceSameDriver: a driver-produced PV bound
// to a claim in ANOTHER app namespace, with an attached VolumeAttachment,
// MUST block. The CSI provisioner serves any workload namespace; scoping
// the gate to the operand namespace would cut live access.
// claimRef.namespace is deliberately not a filter.
func TestDisableBlockedByOtherNamespaceSameDriver(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "other-ns-pv", "some-other-ns")
	seedAttachedVA(t, te.client, "va-other-ns-pv", "other-ns-pv")

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("same-driver usage in another namespace must block (requeue), got %+v", res)
	}
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
	msg := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady).Message
	if !strings.Contains(msg, "other-ns-pv") {
		t.Fatalf("blocked message must name the other-namespace driver PV: %q", msg)
	}
}

// TestDisableBlockedByStaticPVVolumeHandlePrefix: a static PV that does
// not carry the driver NAME but does carry the driver's volumeHandle prefix
// is driver-produced — it blocks too.
func TestDisableBlockedByStaticPVVolumeHandlePrefix(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	_, prefix := renderer.ProvisionerIdentity(v1alpha1.ProfileNFS)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "static-pv"},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "", // identity via the volumeHandle prefix only
					VolumeHandle: prefix + "static-vol-1",
				},
			},
			ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: gateNS, Name: "static-pvc"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	if err := te.client.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed static PV: %v", err)
	}

	disableNFS(t, te)
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
}

// TestDisableIgnoresForeignDriverOtherNamespace: the SAME namespace
// scoping fix must not over-block. Storage in another namespace that does
// NOT carry the driver identity (hostPath PV + its attachment) is clean.
func TestDisableIgnoresForeignDriverOtherNamespace(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedForeignPV(t, te.client, "other-ns-pv", "some-other-ns")
	seedAttachedVA(t, te.client, "va-other-ns-pv", "other-ns-pv")

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("foreign-driver storage must not block, got %+v", res)
	}
	if getDeployment(t, te.client) != nil || getCSIDriver(t, te.client) != nil {
		t.Fatal("disable must remove the operands when the gate is clean")
	}
}

// TestDisableIgnoresForeignDriverInOperandNS: even in the operand
// namespace, another storage's bound PV + attachment + a PVC tied to it must
// not block — the old scan blocked on ANY bound PVC/PV there.
func TestDisableIgnoresForeignDriverInOperandNS(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedForeignPV(t, te.client, "hp-pv", gateNS)
	seedAttachedVA(t, te.client, "va-hp-pv", "hp-pv")
	seedBoundPVCOnPV(t, te.client, "hp-pvc", "hp-pv")

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("foreign-storage usage in the operand namespace must not block, got %+v", res)
	}
	if getDeployment(t, te.client) != nil || getCSIDriver(t, te.client) != nil {
		t.Fatal("disable must remove the operands when the gate is clean")
	}
}

// seedTerminatingDriverPV creates a driver PV ALREADY terminating (the fake
// client treats deletionTimestamp as immutable post-create, so it is seeded
// with the timestamp + finalizer at birth). phase selects the case.
func seedTerminatingDriverPV(t *testing.T, c client.Client, pvName, claimNS string, phase corev1.PersistentVolumePhase) {
	t.Helper()
	driverName, prefix := renderer.ProvisionerIdentity(v1alpha1.ProfileNFS)
	now := metav1.Now()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              pvName,
			Finalizers:        []string{"kubernetes.io/pvc-protection"},
			DeletionTimestamp: &now,
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driverName, VolumeHandle: prefix + pvName},
			},
			ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: pvName + "-pvc"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase},
	}
	if err := c.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed terminating driver PV %q: %v", pvName, err)
	}
}

// TestDisableBlockedByTerminatingDriverPV (fail-safe): a driver PV that
// is terminating but still bound may serve its claim until it is fully gone.
func TestDisableBlockedByTerminatingDriverPV(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedTerminatingDriverPV(t, te.client, "terminating-pv", gateNS, corev1.VolumeBound)

	disableNFS(t, te)
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
}

// TestDisableBlockedByUnboundDriverPVCountsTerminatingClaim (fail-safe):
// a terminating driver PV whose phase has already left Bound (Available +
// claimRef still linked during the unbind) still blocks — unsettled usage
// counts as active. The fake client strips deletionTimestamp on Create, so
// the terminating PV is injected into the PV LIST response (boundary
// simulation of the API server's actual reply — same technique as the
// 403/network fail-safe tests).
func TestDisableBlockedByUnboundDriverPVCountsTerminatingClaim(t *testing.T) {
	driverName, prefix := renderer.ProvisionerIdentity(v1alpha1.ProfileNFS)
	now := metav1.Now()
	unbinding := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "unbinding-pv",
			Finalizers:        []string{"kubernetes.io/pvc-protection"},
			DeletionTimestamp: &now,
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driverName, VolumeHandle: prefix + "unbinding-1"},
			},
			ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: gateNS, Name: "unbinding-pvc"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
	funcs := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if pvs, ok := list.(*corev1.PersistentVolumeList); ok {
				pvs.Items = append(pvs.Items, *unbinding)
			}
			return nil
		},
	}
	te := newInterceptedEnv(t, funcs, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	disableNFS(t, te)
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
}

// TestDisableBlockedByTerminatingBoundPVC: a terminating but still
// Bound PVC holds live mounts until it is gone — the gate counts it
// (fail-safe), the disable is blocked.
func TestDisableBlockedByTerminatingBoundPVC(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "terminating-pv", gateNS)
	seedTerminatingBoundPVC(t, te.client, "terminating-pvc", "terminating-pv")

	disableNFS(t, te)
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
}

// TestForceDisableBypassesUsageGate: the explicit forceDisable
// override skips the scan — cleanup proceeds despite active usage.
func TestForceDisableBypassesUsageGate(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "data-pvc", gateNS)
	seedBoundPVC(t, te.client, "data-pvc")

	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.Profiles.NFS.Enabled = false
	q.Spec.Profiles.NFS.ForceDisable = true
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("forceDisable must complete cleanup without requeue, got %+v", res)
	}
	if getDeployment(t, te.client) != nil || getCSIDriver(t, te.client) != nil {
		t.Fatal("forceDisable must remove the operands")
	}
	// Customer data is still untouched.
	got := &corev1.PersistentVolumeClaim{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: "data-pvc", Namespace: gateNS}, got); err != nil {
		t.Fatalf("forceDisable removed customer PVC: %v", err)
	}
}

// TestUsageGateScanForbiddenFailSafe: a 403 on the usage scan is
// FAIL-SAFE — the cleanup is cancelled, PermissionDenied is surfaced, the
// operands are retained and the reconcile requeues.
func TestUsageGateScanForbiddenFailSafe(t *testing.T) {
	te := newInterceptedEnv(t, pvcListForbidden(), validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedBoundPVC(t, te.client, "data-pvc")

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("scan failure must requeue, got %+v", res)
	}
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonPermissionDenied)
	assertOperandsIntact(t, te.client)
}

// TestUsageGateScanErrorFailSafe: a non-403 scan error (network/API
// down) is also FAIL-SAFE — DependencyMissing, cleanup cancelled.
func TestUsageGateScanErrorFailSafe(t *testing.T) {
	te := newInterceptedEnv(t, pvListError(), validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("scan error must requeue, got %+v", res)
	}
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDependencyMissing)
	assertOperandsIntact(t, te.client)
}

// TestUsageGateProfileIsolation: a blocked nfs disable
// must not rewrite another profile's state — an enabled fc keeps its OWN
// condition (WorkloadNotReady in the fake env — no kubelet — its rendered
// operands included), and nfs alone carries the DataInUse block. The
// inverse direction (fc's live usage must not block nfs) is
// TestProfileContractUsageGateIdentity in profile_contract_test.go.
func TestUsageGateProfileIsolation(t *testing.T) {
	te := newTestEnv(t, validCRWithFC())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "data-pvc", gateNS)
	seedBoundPVC(t, te.client, "data-pvc")

	disableNFS(t, te)
	q := te.get(t, v1alpha1.QuartetName)
	assertGateBlocked(t, q, v1alpha1.ReasonDataInUse)
	assertOperandsIntact(t, te.client)
	// fc keeps its OWN condition — it must not inherit nfs's DataInUse
	// block, and its rendered operands must survive the blocked disable.
	if c := condition(t, q.Status.Profiles[v1alpha1.ProfileFC].Conditions, v1alpha1.ConditionReady); c.Reason == v1alpha1.ReasonDataInUse {
		t.Fatalf("fc must not inherit nfs's DataInUse block (profile isolation): (%v %q)", c.Status, c.Reason)
	}
	fcDriver, _ := renderer.ProvisionerIdentity(v1alpha1.ProfileFC)
	cd := &storagev1.CSIDriver{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: fcDriver}, cd); err != nil {
		t.Fatalf("fc's rendered operands must survive a blocked nfs disable: %v", err)
	}
}

// -------------------------------------------------------------------
// CR deletion gate (finalizer path)
// -------------------------------------------------------------------

// TestDeleteBlockedByBoundPVC: deleting the CR while a Bound PVC is
// present KEEPS the finalizer, surfaces DataInUse and requeues; the
// forceDisable set on the TERMINATING CR (spec stays mutable during
// deletion) then completes the cleanup.
func TestDeleteBlockedByBoundPVC(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	seedDriverPV(t, te.client, "data-pvc", gateNS)
	seedBoundPVC(t, te.client, "data-pvc")

	q := te.get(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName) // sees deletionTimestamp
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("blocked deletion must requeue, got %+v", res)
	}
	stuck := te.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(stuck, quartetFinalizer) {
		t.Fatal("blocked deletion must KEEP the finalizer")
	}
	if stuck.DeletionTimestamp.IsZero() {
		t.Fatal("CR must be terminating")
	}
	// The block surfaces on the aggregate (not per-profile): Ready/
	// Progressing False + Degraded True, reason DataInUse.
	c := condition(t, stuck.Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonDataInUse {
		t.Fatalf("aggregate Ready = (%v %q), want (False DataInUse)", c.Status, c.Reason)
	}
	if c2 := condition(t, stuck.Status.Conditions, v1alpha1.ConditionDegraded); c2.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = (%v %q), want True", c2.Status, c2.Reason)
	}
	assertOperandsIntact(t, te.client)

	// Recovery (preferred path): set forceDisable on the terminating CR —
	// the spec is still mutable while deletionTimestamp is set.
	stuck.Spec.Profiles.NFS.ForceDisable = true
	if err := te.client.Update(context.Background(), stuck); err != nil {
		t.Fatalf("forceDisable on a terminating CR must be updatable: %v", err)
	}
	res = te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("forced replay must complete, got %+v", res)
	}
	got := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, got); !apierrors.IsNotFound(err) {
		t.Fatalf("CR must be gone after forced replay (err=%v)", err)
	}
	if getCSIDriver(t, te.client) != nil || getDeployment(t, te.client) != nil {
		t.Fatal("forced replay must remove the operands")
	}
}

// TestDeleteScanForbiddenFailSafe: a 403 on the usage scan during
// deletion is FAIL-SAFE — finalizer kept, PermissionDenied surfaced,
// requeue.
func TestDeleteScanForbiddenFailSafe(t *testing.T) {
	te := newInterceptedEnv(t, pvcListForbidden(), validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("blocked deletion must requeue, got %+v", res)
	}
	stuck := te.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(stuck, quartetFinalizer) {
		t.Fatal("scan failure must KEEP the finalizer")
	}
	c := condition(t, stuck.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonPermissionDenied {
		t.Fatalf("aggregate reason = %q, want PermissionDenied", c.Reason)
	}
	assertOperandsIntact(t, te.client)
}

const (
	activeUsagePVC = "active-usage-pvc"
	activeUsagePV  = "active-usage-pv"
)

// envtestCreateDriverUsage creates the driver usage the gate must recognize on
// the REAL API: a Bound driver-produced PV (CSI source carrying the NFS
// provisioner identity) claimed by a Bound PVC in the operand namespace.
// The usage lives in the operand namespace here; the cross-namespace scope
// is proven by the fake-client tests (the real API server runs no
// PV/PVC controller, so only manually bound state is realistic in envtest).
func envtestCreateDriverUsage(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	driverName, prefix := renderer.ProvisionerIdentity(v1alpha1.ProfileNFS)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: activeUsagePV},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driverName, VolumeHandle: prefix + activeUsagePV},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", Namespace: envtestNFSNS, Name: activeUsagePVC,
			},
		},
	}
	if err := testClient.Create(ctx, pv); err != nil {
		t.Fatalf("create PV %q: %v", activeUsagePV, err)
	}
	pv.Status.Phase = corev1.VolumeBound
	if err := testClient.Status().Update(ctx, pv); err != nil {
		t.Fatalf("set PV %q phase Bound: %v", activeUsagePV, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: activeUsagePVC, Namespace: envtestNFSNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  activeUsagePV,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := testClient.Create(ctx, pvc); err != nil {
		t.Fatalf("create PVC %q: %v", activeUsagePVC, err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := testClient.Status().Update(ctx, pvc); err != nil {
		t.Fatalf("set PVC %q phase Bound: %v", activeUsagePVC, err)
	}
}

// envtestDeleteUsage best-effort removes the test PV + PVC. Called at test
// START (leftover from an earlier failure must not block ensureSingleton's
// deletion) and via defer at the end.
//
// TEST HYGIENE, NOT a production behavior: a PVC whose phase went Bound
// carries the kubernetes.io/pvc-protection finalizer (added by admission),
// and envtest runs NO PVC controller to remove it — such a PVC would stay
// terminating forever and (correctly, fail-safe) keep blocking the usage
// gate. Strip the finalizer before the delete so the object actually
// disappears.
func envtestDeleteUsage(ctx context.Context) {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: activeUsagePV}}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(pv), pv); err == nil {
		pv.Finalizers = nil
		if testClient.Update(ctx, pv) == nil {
			_ = testClient.Delete(ctx, pv)
		}
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: activeUsagePVC, Namespace: envtestNFSNS}}
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
		return // absent or unreachable: nothing to clean
	}
	pvc.Finalizers = nil
	if err := testClient.Update(ctx, pvc); err != nil {
		return
	}
	if err := testClient.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		// A hard failure here would only mask the real test outcome.
		_ = err
	}
}

// pollAggregateReason polls the aggregate Ready reason.
func pollAggregateReason(t *testing.T, wantReason string) {
	t.Helper()
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		for _, c := range q.Status.Conditions {
			if c.Type == v1alpha1.ConditionReady {
				return c.Reason == wantReason
			}
		}
		return false
	})
}

// TestEnvtestDisableBlockedActiveUsageThenForce (on the real API):
// disabling the profile while a Bound PVC lives in the operand namespace is
// BLOCKED — DataInUse surfaces on the profile and aggregate conditions, the
// SSA operands stay, and nothing requeues away silently. Setting the
// explicit forceDisable override then completes the cleanup.
func TestEnvtestDisableBlockedActiveUsageThenForce(t *testing.T) {
	requireEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout*2)
	defer cancel()
	envtestDeleteUsage(ctx) // start clean even after an earlier failure
	defer envtestDeleteUsage(context.Background())

	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	envtestCreateDriverUsage(t)

	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	q.Spec.Profiles.NFS.Enabled = false
	if err := testClient.Update(ctx, q); err != nil {
		t.Fatal(err)
	}

	pollAggregateReason(t, v1alpha1.ReasonDataInUse)
	// Profile-level surface: Degraded=True + Ready=False, reason DataInUse.
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		ps, ok := q.Status.Profiles[v1alpha1.ProfileNFS]
		if !ok {
			return false
		}
		for _, c := range ps.Conditions {
			if c.Type == v1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonDataInUse {
				return true
			}
		}
		return false
	})
	// The block cancelled the cleanup: operands are still present.
	pollObj(t, func(ctx context.Context) bool {
		return envtestDeployment(t) != nil && envtestCSIDriver(t) != nil
	})
	time.Sleep(1500 * time.Millisecond) // let a blocked pass settle; it must NOT delete
	if envtestDeployment(t) == nil || envtestCSIDriver(t) == nil {
		t.Fatal("blocked disable removed operands — fail-safe violated")
	}

	// Explicit override: the cleanup completes.
	q = &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	q.Spec.Profiles.NFS.ForceDisable = true
	if err := testClient.Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	pollObj(t, func(ctx context.Context) bool {
		return envtestDeployment(t) == nil && envtestCSIDriver(t) == nil
	})
}

// TestEnvtestDeleteBlockedThenForce (on the real API): deleting the CR
// while a Bound PVC is present KEEPS the finalizer (CR stays terminating),
// surfaces DataInUse on the aggregate conditions, and requeues. Setting
// forceDisable on the TERMINATING CR (the spec stays mutable — only
// metadata.name/namespace are immutable) then completes the deletion.
func TestEnvtestDeleteBlockedThenForce(t *testing.T) {
	requireEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout*2)
	defer cancel()
	envtestDeleteUsage(ctx)
	defer envtestDeleteUsage(context.Background())

	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	envtestCreateDriverUsage(t)

	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctx, q); err != nil {
		t.Fatal(err)
	}

	pollAggregateReason(t, v1alpha1.ReasonDataInUse)
	stuck := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, stuck); err != nil {
		t.Fatalf("blocked deletion removed the CR too early: %v", err)
	}
	if stuck.DeletionTimestamp.IsZero() {
		t.Fatal("CR must be terminating while the gate blocks")
	}
	if !containsFinalizer(stuck, quartetFinalizer) {
		t.Fatal("blocked deletion must KEEP the finalizer")
	}
	// Force on the terminating CR — the documented recovery path.
	stuck.Spec.Profiles.NFS.ForceDisable = true
	if err := testClient.Update(ctx, stuck); err != nil {
		t.Fatalf("forceDisable on a terminating CR must be updatable: %v", err)
	}
	waitForDeletion(t, ctx)
}

// TestEnvtestDeleteUnblocksWhenUsageClears (replay): with no override,
// the block clears on its own once the usage goes away — the retryable
// requeue re-runs the scan and completes the deletion.
func TestEnvtestDeleteUnblocksWhenUsageClears(t *testing.T) {
	requireEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout*4)
	defer cancel()
	envtestDeleteUsage(ctx)
	defer envtestDeleteUsage(context.Background())

	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	envtestCreateDriverUsage(t)

	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctx, q); err != nil {
		t.Fatal(err)
	}
	pollAggregateReason(t, v1alpha1.ReasonDataInUse)

	// Usage clears; the gate's requeue (retryableRequeueAfter) must then
	// finish the deletion WITHOUT any override.
	envtestDeleteUsage(ctx)
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond,
		retryableRequeueAfter+15*time.Second, true, func(ctx context.Context) (bool, error) {
			got := &v1alpha1.Quartet{}
			if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, got); err != nil {
				return client.IgnoreNotFound(err) == nil, nil
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("CR not reaped after usage cleared and gate replay: %v", err)
	}
}

// deleteRaceInterceptor bumps a concurrent benign metadata write into the
// verify→delete gap of every operand delete while race is armed: the write
// keeps UID + ownership annotation intact (a real concurrent editor), only
// the resourceVersion moves — so the delete precondition rejects with 409.
func deleteRaceInterceptor(race *bool) interceptor.Funcs {
	return interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if *race && obj.GetName() != v1alpha1.QuartetName {
				live := obj.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err == nil {
					ann := live.GetAnnotations()
					if ann == nil {
						ann = map[string]string{}
					}
					ann["concurrent.example/write"] = "raced-into-delete-gap"
					live.SetAnnotations(ann)
					_ = c.Update(ctx, live)
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// TestCleanupDeleteConflictKeepsFinalizerPending (pending outcome, delete path):
// a UID/RV race during cleanup (same UID + annotation, concurrent metadata
// update) must NOT skip: the first reconcile keeps the finalizer, reports
// CleanupFailed and requeues; when the race clears, replay completes the
// deletion. The object stays ours (annotation intact) throughout.
func TestCleanupDeleteConflictKeepsFinalizerPending(t *testing.T) {
	race := true
	te := newInterceptedEnv(t, deleteRaceInterceptor(&race), validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(q, quartetFinalizer) {
		t.Fatal("reconcile must add the finalizer before applying")
	}
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("raced delete must be PENDING (retryable requeue), got %+v", res)
	}
	stuck := te.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(stuck, quartetFinalizer) {
		t.Fatal("raced delete must KEEP the finalizer — the object still exists and is still ours")
	}
	c := condition(t, stuck.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonCleanupFailed {
		t.Fatalf("pending cleanup reason = %q, want CleanupFailed", c.Reason)
	}
	if !strings.Contains(c.Message, "cleanup pending") {
		t.Fatalf("pending cleanup must be surfaced as pending (not skipped), message: %q", c.Message)
	}
	// The pending operand still exists and is still ours.
	cd := getCSIDriver(t, te.client)
	if cd == nil {
		t.Fatal("the pending object must STILL EXIST (it is ours)")
	}
	if cd.Annotations[renderer.OwnerUIDAnnotation] == "" {
		t.Fatal("the pending object must still carry our ownership annotation")
	}

	// The race clears: replay completes.
	race = false
	res = te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("cleared race must complete cleanup, got %+v", res)
	}
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("replay must delete the pending operand")
	}
	got := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, got); !isNotFoundErr(err) {
		t.Fatalf("CR must be reaped after replay (err=%v)", err)
	}
}

// TestCleanupConflictPendingSurvivesRestart (pending outcome, CR restart): the
// pending state must be durable across a manager RESTART — a fresh
// reconciler instance over the same client (finalizer persisted on the CR)
// re-derives the pending cleanup and still keeps the finalizer; only when
// the race clears does the deletion finish.
func TestCleanupConflictPendingSurvivesRestart(t *testing.T) {
	race := true
	te := newInterceptedEnv(t, deleteRaceInterceptor(&race), validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	q := te.get(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName) // arms the pending state

	// "Restart": a fresh reconciler instance over the SAME client, as a new
	// manager process would see it (TestRestartIdempotent pattern).
	te2 := restartedFakeEnv(t, te.client)
	res := te2.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("post-restart reconcile must re-derive pending cleanup, got %+v", res)
	}
	stuck := te2.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(stuck, quartetFinalizer) {
		t.Fatal("restart must NOT clear the finalizer while cleanup is pending")
	}
	if getCSIDriver(t, te.client) == nil {
		t.Fatal("restart must not delete a conflicting pending operand")
	}

	// Race clears — the restarted instance completes the deletion.
	race = false
	res = te2.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("cleared race after restart must complete, got %+v", res)
	}
	got := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, got); !isNotFoundErr(err) {
		t.Fatalf("CR must be reaped after restart replay (err=%v)", err)
	}
}

// TestCleanupConflictPendingDisablePath (pending outcome, disable path): pending
// cleanup must not look SUCCESSFUL on a profile disable either — the
// profile reports CleanupFailed retryable, operands remain, and the requeue
// drives completion once the race clears.
func TestCleanupConflictPendingDisablePath(t *testing.T) {
	race := true
	te := newInterceptedEnv(t, deleteRaceInterceptor(&race), validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.Profiles.NFS.Enabled = false
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("pending cleanup on disable must requeue, got %+v", res)
	}
	pc := profileReadyNFS(t, te)
	if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonCleanupFailed {
		t.Fatalf("disable path must surface CleanupFailed, not success: (%v %q)", pc.Status, pc.Reason)
	}
	if !strings.Contains(pc.Message, "cleanup pending") {
		t.Fatalf("disable message must say the cleanup is PENDING, got %q", pc.Message)
	}
	if getCSIDriver(t, te.client) == nil {
		t.Fatal("pending operand must still exist on the disable path")
	}

	race = false
	res = te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("cleared race must complete the disable cleanup, got %+v", res)
	}
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("disable cleanup must remove the pending operand")
	}
}

// TestCleanupForeignReplacementNeverDeleted (ownership-change outcome): when the object
// CHANGES OWNERSHIP between enumeration and verification (a foreign editor
// strips the ownership annotation in the gap — a re-adoption attempt racing
// us), the fresh re-verification rejects it as foreign BEFORE the delete is
// issued: safe skip, never deleted, pass continues. This is the case that
// MUST still skip — the distinction from the pending outcome is
// ownership, not conflict.
func TestCleanupForeignReplacementNeverDeleted(t *testing.T) {
	const targetName = "nfs-csi-ngxstorage-controller" // first enumerated operand (ServiceAccount)
	steal := false
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, k, obj, opts...)
			if err == nil && steal && k.Name == targetName {
				// Serve the foreign replacement (annotation stripped) to the
				// fresh re-verification — exactly what a takeover in the gap
				// would look like to a live read.
				ann := obj.GetAnnotations()
				delete(ann, renderer.OwnerUIDAnnotation)
				obj.SetAnnotations(ann)
			}
			return err
		},
	}
	te := newInterceptedEnv(t, funcs, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	steal = true
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("foreign replacement must be a SAFE SKIP (continue), got %+v", res)
	}
	// The foreign object survives — it was never deleted.
	gotSA := &corev1.ServiceAccount{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: targetName, Namespace: "ngxstorage-nfs"}, gotSA); err != nil {
		t.Fatalf("foreign replacement must NOT be deleted (err=%v)", err)
	}
	// ...and the rest of cleanup completed: CR reaped.
	got := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, got); !isNotFoundErr(err) {
		t.Fatalf("foreign-replacement skip must not keep the finalizer (err=%v)", err)
	}
	// Every OTHER operand WAS deleted (the skip was targeted, not blanket).
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("cleanup must still delete the remaining owned operands")
	}
}

// restartedFakeEnv mirrors TestRestartIdempotent's fresh-instance wiring.
func restartedFakeEnv(t *testing.T, c client.Client) *fakeEnv {
	t.Helper()
	rec := record.NewFakeRecorder(100)
	return &fakeEnv{
		reconciler: &QuartetReconciler{
			Client:            c,
			Scheme:            c.Scheme(),
			Log:               logf.Log.WithName("test-restarted"),
			Recorder:          rec,
			PlatformDiscovery: k8sDiscovery(),
			Applier:           mergeApplier{c: c},
		},
		client:   c,
		recorder: rec,
	}
}

func isNotFoundErr(err error) bool {
	return apierrors.IsNotFound(err)
}
