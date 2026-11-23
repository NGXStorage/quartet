package controller

import (
	"context"
	"errors"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	apps "k8s.io/api/apps/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"strings"
	"testing"
)

// --- shared helpers (profile-parameterized; the nfs-specific helpers in
// --- active_volume_protection_test.go stay for the nfs-pinned tests). ---

// profileSpecFor builds a spec-valid enabled ProfileSpec for one profile
// name: the secret name follows the renderer's canonical
// <profile>-csi-ngxstorage-config identity and the image satisfies the
// v1alpha1 digest contract (any 64-hex digest — the value is a test
// projection, never a support claim).
func profileSpecFor(prof string) v1alpha1.ProfileSpec {
	return v1alpha1.ProfileSpec{
		Enabled:   true,
		SecretRef: &v1alpha1.SecretRef{Name: prof + "-csi-ngxstorage-config"},
		Image:     &v1alpha1.ImageSpec{Reference: "quay.io/ngxstorage/" + prof + "-csi-ngxstorage@sha256:" + strings.Repeat("f", 64)},
	}
}

// validCRWithOnly returns the singleton with exactly the listed profiles
// enabled: unlisted profiles are disabled, and each listed non-nfs profile
// receives a spec-valid projection (validCR enables nfs alone).
func validCRWithOnly(profs ...string) *v1alpha1.Quartet {
	q := validCR()
	enabled := map[string]bool{}
	for _, p := range profs {
		enabled[p] = true
		// nfs keeps its canonical validCR spec; only non-nfs profiles get
		// the generic projection. (ProfileSpecs() returns a throwaway map
		// of pointers, so assign through the real spec fields.)
		switch p {
		case v1alpha1.ProfileFC:
			q.Spec.Profiles.FC = profileSpecFor(p)
		case v1alpha1.ProfileISCSI:
			q.Spec.Profiles.ISCSI = profileSpecFor(p)
		}
	}
	for name := range q.ProfileSpecs() {
		if !enabled[name] {
			q.ProfileSpecs()[name].Enabled = false
		}
	}
	return q
}

// renderedProfileOrSkip returns the renderer for one profile, or skips the
// test when the profile has no renderer registered.
func renderedProfileOrSkip(t *testing.T, prof string) renderer.Profile {
	t.Helper()
	p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
	if p == nil {
		t.Skipf("profile %s has no renderer registered — contract activates at registry wiring", prof)
	}
	return p
}

// removeInstallRBACFor deletes the profile's seeded installer-owned RBAC
// set (the admin never applied it / drifted it away).
func removeInstallRBACFor(t *testing.T, te *fakeEnv, prof string) {
	t.Helper()
	renderedProfileOrSkip(t, prof)
	p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
	for _, obj := range p.InstallRBAC() {
		if err := te.client.Delete(context.Background(), obj); err != nil {
			t.Fatalf("remove install RBAC for %s: %v", prof, err)
		}
	}
}

// restoreInstallRBACFor admin re-applies the profile's installer-owned set
// (the recovery use of the committed install manifests).
func restoreInstallRBACFor(t *testing.T, te *fakeEnv, prof string) {
	t.Helper()
	p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
	for _, obj := range p.InstallRBAC() {
		if err := te.client.Create(context.Background(), obj); err != nil {
			t.Fatalf("restore install RBAC for %s: %v", prof, err)
		}
	}
}

// seedDriverPVFor seeds a Bound PV carrying ONE profile's provisioner
// identity (cluster-wide, like seedDriverPV but identity-parameterized).
func seedDriverPVFor(t *testing.T, c client.Client, prof, pvName, claimNS string) {
	t.Helper()
	driverName, prefix := renderer.ProvisionerIdentity(prof)
	if driverName == "" {
		t.Fatalf("profile %s contributes no provisioner identity — cannot seed its usage", prof)
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driverName, VolumeHandle: prefix + pvName},
			},
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: pvName + "-pvc",
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	if err := c.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed driver PV for %s: %v", prof, err)
	}
}

// seedBoundPVCIn seeds a Bound PVC in the named operand namespace tied to
// the given PV (the PVC scan is operand-namespace scoped).
func seedBoundPVCIn(t *testing.T, c client.Client, ns, name, volumeName string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  volumeName,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if err := c.Create(context.Background(), pvc); err != nil {
		t.Fatalf("seed bound PVC in %s: %v", ns, err)
	}
}

// disableProfile flips one profile to enabled=false and reconciles once.
func disableProfile(t *testing.T, te *fakeEnv, prof string) {
	t.Helper()
	q := te.get(t, v1alpha1.QuartetName)
	q.ProfileSpecs()[prof].Enabled = false
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)
}

// profileCSIDriver returns the profile's own CSIDriver (by its provisioner
// identity name) or nil.
func profileCSIDriver(t *testing.T, c client.Client, prof string) *storagev1.CSIDriver {
	t.Helper()
	driverName, _ := renderer.ProvisionerIdentity(prof)
	cd := &storagev1.CSIDriver{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: driverName}, cd); err != nil {
		return nil
	}
	return cd
}

// profileControllerDeployment finds the profile's controller Deployment by
// ownership annotation in its operand namespace (renderer-independent).
func profileControllerDeployment(t *testing.T, te *fakeEnv, prof string) *apps.Deployment {
	t.Helper()
	q := te.get(t, v1alpha1.QuartetName)
	deps := &apps.DeploymentList{}
	if err := te.client.List(context.Background(), deps,
		client.InNamespace(renderer.OperandNamespace(prof))); err != nil {
		t.Fatal(err)
	}
	for i := range deps.Items {
		if deps.Items[i].GetAnnotations()[renderer.OwnerUIDAnnotation] == string(q.UID) {
			return &deps.Items[i]
		}
	}
	return nil
}

// --- contracts ---

// TestProfileContractStatusSurface: the shared status API per profile.
// Enabled → own status.profiles entry with its own condition (never
// RenderError/RBACDrift on a healthy seeded install set — the fake env
// settles at WorkloadNotReady, nfs's own fake-env surface); disabled-clean
// → no entry, no operands, no namespace. The aggregate names the blocked
// profile honestly.
func TestProfileContractStatusSurface(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			renderedProfileOrSkip(t, prof)
			te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS, prof))
			te.reconcile(t, v1alpha1.QuartetName)
			fakeWorkloadReady(t, te.client) // nfs workloads only — nfs-specific names
			te.reconcile(t, v1alpha1.QuartetName)
			q := te.get(t, v1alpha1.QuartetName)

			ps, ok := q.Status.Profiles[prof]
			if !ok {
				t.Fatalf("enabled %s must claim status.profiles.%s", prof, prof)
			}
			c := condition(t, ps.Conditions, v1alpha1.ConditionReady)
			if prof == v1alpha1.ProfileNFS {
				// nfs's own converged surface (fakeWorkloadReady above).
				if c.Status != metav1.ConditionTrue {
					t.Fatalf("nfs Ready = (%v %q), want True after fake convergence", c.Status, c.Reason)
				}
			} else {
				// Its OWN gated condition — the fake env settles non-nfs
				// profiles at WorkloadNotReady; never RenderError (the
				// renderer exists) and never RBACDrift (healthy seed).
				if c.Status != metav1.ConditionFalse ||
					c.Reason == v1alpha1.ReasonRenderError || c.Reason == v1alpha1.ReasonRBACDrift {
					t.Fatalf("%s Ready = (%v %q) — a wired profile on a healthy install set gates on its own surface, never RenderError/RBACDrift", prof, c.Status, c.Reason)
				}
				if c := condition(t, ps.Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionTrue {
					t.Fatalf("%s Degraded must be True while gated: %v", prof, c.Status)
				}
			}
			// Its own operand materialized behind the seeded install set.
			if profileCSIDriver(t, te.client, prof) == nil {
				t.Fatalf("enabled %s must create its own CSIDriver", prof)
			}
			// NFS stays independently Ready.
			if c := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
				t.Fatalf("nfs must stay Ready alongside %s: (%v %q)", prof, c.Status, c.Reason)
			}
			// The aggregate honestly names the blocked profile (nfs alone
			// converges, so for prof != nfs the aggregate is gated).
			agg := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
			if prof != v1alpha1.ProfileNFS {
				if agg.Status == metav1.ConditionTrue {
					t.Fatalf("aggregate must not be Ready while %s is gated", prof)
				}
				if !strings.Contains(agg.Message, prof) {
					t.Fatalf("aggregate must name the failed profile %s: %s", prof, agg.Message)
				}
			}

			// Disabled-clean: no status entry, no operands; the namespace
			// (once ensured) is RETAINED — disable never deletes it.
			disableProfile(t, te, prof)
			q = te.get(t, v1alpha1.QuartetName)
			if _, ok := q.Status.Profiles[prof]; ok {
				t.Fatalf("disabled-clean %s must claim no status entry (a silent no-op is not a claim)", prof)
			}
			if profileCSIDriver(t, te.client, prof) != nil || profileControllerDeployment(t, te, prof) != nil {
				t.Fatalf("disabled-clean %s must leave no operands", prof)
			}
			if getNamespace(te.client, renderer.OperandNamespace(prof)) == nil {
				t.Fatalf("disable must retain %s's namespace", prof)
			}
		})
	}
}

// TestProfileContractRBACDriftGatesAndRecovers: the read-only manager RBAC
// contract per profile — the installer-owned set gates operand apply BEFORE
// any operand write (missing set → Degraded/RBACDrift, zero operands, other
// profiles unaffected), the reconciler never creates or repairs the set,
// and an admin re-apply recovers the profile.
func TestProfileContractRBACDriftGatesAndRecovers(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			renderedProfileOrSkip(t, prof)
			te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS, prof))
			removeInstallRBACFor(t, te, prof)
			te.reconcile(t, v1alpha1.QuartetName)
			if prof != v1alpha1.ProfileNFS {
				// Convergence the NFS side only — nfs-specific workload
				// names, and nfs must prove independently Ready.
				fakeWorkloadReady(t, te.client)
				te.reconcile(t, v1alpha1.QuartetName)
			}
			q := te.get(t, v1alpha1.QuartetName)

			ps := q.Status.Profiles[prof]
			if c := condition(t, ps.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonRBACDrift {
				t.Fatalf("%s without its install set = (%v %q), want (False RBACDrift)", prof, c.Status, c.Reason)
			}
			if c := condition(t, ps.Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionTrue {
				t.Fatalf("%s Degraded must be True on RBAC drift: %v", prof, c.Status)
			}
			// The gate is BEFORE any operand write.
			if profileCSIDriver(t, te.client, prof) != nil || profileControllerDeployment(t, te, prof) != nil {
				t.Fatalf("%s must create no operands behind an unapplied install set", prof)
			}
			// The reconciler never creates or repairs the missing set.
			p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
			for _, obj := range p.InstallRBAC() {
				got := obj.DeepCopyObject().(client.Object)
				if err := te.client.Get(context.Background(), client.ObjectKeyFromObject(obj), got); err == nil {
					t.Fatalf("%s: the reconciler must NOT recreate missing install RBAC %s %q", prof, kindName(got), obj.GetName())
				} else if !apierrors.IsNotFound(err) {
					t.Fatalf("%s: get install RBAC %q: %v", prof, obj.GetName(), err)
				}
			}
			// NFS independently Ready (when it is the other profile).
			if prof != v1alpha1.ProfileNFS {
				if c := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
					t.Fatalf("nfs must stay Ready despite %s's RBAC drift: (%v %q)", prof, c.Status, c.Reason)
				}
			}

			// Admin re-apply recovers: the profile proceeds to operand apply.
			restoreInstallRBACFor(t, te, prof)
			te.reconcile(t, v1alpha1.QuartetName)
			q = te.get(t, v1alpha1.QuartetName)
			if c := condition(t, q.Status.Profiles[prof].Conditions, v1alpha1.ConditionReady); c.Reason == v1alpha1.ReasonRBACDrift {
				t.Fatalf("%s must recover after the admin re-applies its install set: (%v %q)", prof, c.Status, c.Reason)
			}
			if profileCSIDriver(t, te.client, prof) == nil {
				t.Fatalf("%s must apply its operands once the install set verifies healthy", prof)
			}
			// Recovery also converges the NFS side when nfs is the other
			// profile — nothing may stay corrupted behind the drift.
			if prof != v1alpha1.ProfileNFS {
				fakeWorkloadReady(t, te.client)
				te.reconcile(t, v1alpha1.QuartetName)
				if c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
					t.Fatalf("nfs must converge after %s's recovery: (%v %q)", prof, c.Status, c.Reason)
				}
			}
		})
	}
}

// TestProfileContractOwnershipNoForce: per profile — a foreign
// object pre-created at the profile's CSIDriver name is never adopted: the
// profile reports ForeignResourceConflict, and the foreign object carries
// no ownership annotation and its spec untouched (no force).
func TestProfileContractOwnershipNoForce(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			renderedProfileOrSkip(t, prof)
			driverName, _ := renderer.ProvisionerIdentity(prof)
			foreign := &storagev1.CSIDriver{
				ObjectMeta: metav1.ObjectMeta{Name: driverName, Labels: map[string]string{"managed-by": "user"}},
				Spec:       storagev1.CSIDriverSpec{PodInfoOnMount: ptrToBool(true)},
			}
			te := newTestEnv(t, validCRWithOnly(prof), foreign)
			te.reconcile(t, v1alpha1.QuartetName)
			q := te.get(t, v1alpha1.QuartetName)

			c := condition(t, q.Status.Profiles[prof].Conditions, v1alpha1.ConditionReady)
			if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonForeignResourceConflict {
				t.Fatalf("%s on a foreign CSIDriver = (%v %q), want (False ForeignResourceConflict)", prof, c.Status, c.Reason)
			}
			got := &storagev1.CSIDriver{}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: driverName}, got); err != nil {
				t.Fatalf("get foreign CSIDriver: %v", err)
			}
			if got.Labels["managed-by"] != "user" || got.Spec.PodInfoOnMount == nil || !*got.Spec.PodInfoOnMount {
				t.Fatalf("%s: foreign CSIDriver was modified (no adoption, no force): labels=%v podInfoOnMount=%v", prof, got.Labels, got.Spec.PodInfoOnMount)
			}
			if _, has := got.Annotations[renderer.OwnerUIDAnnotation]; has {
				t.Fatalf("%s: foreign CSIDriver gained the ownership annotation", prof)
			}
		})
	}
}

// TestProfileContractUsageGateIdentity: per profile — the profile's OWN
// live storage blocks its disable with DataInUse (operands retained);
// ANOTHER profile's live storage never blocks it (cross-profile identity
// isolation — the gate matches cluster-wide PVs against the profile's own
// provisioner identity); forceDisable overrides explicitly.
func TestProfileContractUsageGateIdentity(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			p := renderedProfileOrSkip(t, prof)
			_ = p
			other := ""
			for _, cand := range renderer.ProfileNames() {
				if cand != prof {
					other = cand
					break
				}
			}

			t.Run("own-usage-blocks", func(t *testing.T) {
				te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS, prof))
				te.reconcile(t, v1alpha1.QuartetName)
				fakeWorkloadReady(t, te.client)
				te.reconcile(t, v1alpha1.QuartetName)
				ns := renderer.OperandNamespace(prof)
				seedDriverPVFor(t, te.client, prof, "data-pv", ns)
				seedBoundPVCIn(t, te.client, ns, "data-pvc", "data-pv")

				disableProfile(t, te, prof)
				q := te.get(t, v1alpha1.QuartetName)
				ps := q.Status.Profiles[prof]
				if c := condition(t, ps.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonDataInUse {
					t.Fatalf("%s disable with own live storage = (%v %q), want (False DataInUse)", prof, c.Status, c.Reason)
				}
				if profileCSIDriver(t, te.client, prof) == nil {
					t.Fatalf("blocked %s disable must retain its operands", prof)
				}
				// When the other profile is nfs, it must stay independently
				// Ready (the block is scoped to prof only).
				if prof != v1alpha1.ProfileNFS {
					if c := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
						t.Fatalf("nfs must stay Ready while %s's disable is blocked: (%v %q)", prof, c.Status, c.Reason)
					}
				}
			})

			t.Run("other-profile-usage-never-blocks", func(t *testing.T) {
				te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS, prof))
				te.reconcile(t, v1alpha1.QuartetName)
				fakeWorkloadReady(t, te.client)
				te.reconcile(t, v1alpha1.QuartetName)
				// OTHER profile's live storage (the nfs-specific helper seeds the
				// nfs identity; the nfs profile's own usage is the "other"
				// identity for non-nfs profiles).
				seedDriverPVFor(t, te.client, other, "other-pv", renderer.OperandNamespace(other))
				seedBoundPVCIn(t, te.client, renderer.OperandNamespace(other), "other-pvc", "other-pv")

				disableProfile(t, te, prof)
				if profileCSIDriver(t, te.client, prof) != nil {
					t.Fatalf("%s's disable was blocked by %s's live storage — the usage gate must match only %s's own provisioner identity", prof, other, prof)
				}
				if getNamespace(te.client, renderer.OperandNamespace(prof)) == nil {
					t.Fatalf("clean %s disable must retain its namespace", prof)
				}
			})

			t.Run("forceDisable-overrides", func(t *testing.T) {
				te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS, prof))
				te.reconcile(t, v1alpha1.QuartetName)
				fakeWorkloadReady(t, te.client)
				te.reconcile(t, v1alpha1.QuartetName)
				ns := renderer.OperandNamespace(prof)
				seedDriverPVFor(t, te.client, prof, "data-pv", ns)
				seedBoundPVCIn(t, te.client, ns, "data-pvc", "data-pv")

				q := te.get(t, v1alpha1.QuartetName)
				q.ProfileSpecs()[prof].Enabled = false
				q.ProfileSpecs()[prof].ForceDisable = true
				if err := te.client.Update(context.Background(), q); err != nil {
					t.Fatal(err)
				}
				te.reconcile(t, v1alpha1.QuartetName)
				if profileCSIDriver(t, te.client, prof) != nil {
					t.Fatalf("explicit %s forceDisable must override the usage gate and complete the cleanup", prof)
				}
			})
		})
	}
}

// TestProfileContractAtomicCleanup: per profile — a clean disable
// removes EXACTLY the profile's owned operands: its CSIDriver and its
// controller Deployment are gone, the operand namespace is retained, a
// customer Secret in the operand namespace is retained, and no OTHER
// profile's cluster-scoped operand is touched — the shared CR UID makes
// owner-uid alone ambiguous for cluster-scoped kinds, so the cleanup must
// scope by the profile's own render set.
func TestProfileContractAtomicCleanup(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			renderedProfileOrSkip(t, prof)
			te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS, prof))
			te.reconcile(t, v1alpha1.QuartetName)
			fakeWorkloadReady(t, te.client)
			te.reconcile(t, v1alpha1.QuartetName)
			before := captureProfileOwnedState(t, te.client, v1alpha1.ProfileNFS)
			ns := renderer.OperandNamespace(prof)
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: prof + "-customer-secret", Namespace: ns},
				Data: map[string][]byte{"k": []byte("v")}}
			if err := te.client.Create(context.Background(), sec); err != nil {
				t.Fatal(err)
			}
			if profileCSIDriver(t, te.client, prof) == nil {
				t.Fatalf("precondition: %s's CSIDriver must exist before disable", prof)
			}

			disableProfile(t, te, prof)

			if profileCSIDriver(t, te.client, prof) != nil {
				t.Fatalf("clean %s disable must remove its own CSIDriver", prof)
			}
			if profileControllerDeployment(t, te, prof) != nil {
				t.Fatalf("clean %s disable must remove its own controller Deployment", prof)
			}
			if getNamespace(te.client, ns) == nil {
				t.Fatalf("clean %s disable must retain its namespace", prof)
			}
			got := &corev1.Secret{}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: prof + "-customer-secret", Namespace: ns}, got); err != nil {
				t.Fatalf("clean %s disable must retain the customer Secret: %v", prof, err)
			}
			// The shared-CR-UID hazard: NFS's cluster-scoped
			// CSIDriver must survive a FOREIGN profile's cleanup, and the
			// rest of NFS's owned state must be byte-stable. When prof is
			// nfs itself, that hazard assertion does not apply (nfs's own
			// disable legitimately removes nfs's operands).
			if prof != v1alpha1.ProfileNFS {
				if profileCSIDriver(t, te.client, v1alpha1.ProfileNFS) == nil {
					t.Fatalf("disabling %s deleted NFS's cluster-scoped CSIDriver — cleanup is not profile-scoped", prof)
				}
				if after := captureProfileOwnedState(t, te.client, v1alpha1.ProfileNFS); before != after {
					t.Fatalf("disabling %s rewrote NFS-owned objects", prof)
				}
			}
		})
	}
}

// TestProfileContractInstallRBACWatchFilter: the read-only install-RBAC
// watch enqueues ONLY pinned operand events per profile — a namespaced
// pinned name counts from its owning profile's operand namespace and
// nowhere else; a cluster-scoped pinned name counts globally; anything
// foreign drives zero reconcile.
func TestProfileContractInstallRBACWatchFilter(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			renderedProfileOrSkip(t, prof)
			names := renderer.OperandRBACNames(prof)
			ns := renderer.OperandNamespace(prof)
			if len(names.Roles) == 0 || len(names.ClusterRoles) == 0 {
				t.Fatalf("%s pinned set is empty — nothing to filter", prof)
			}
			role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: names.Roles[0], Namespace: ns}}
			if !isInstallOwnedRBAC(role) {
				t.Fatalf("%s: pinned Role %s/%s must enqueue", prof, ns, names.Roles[0])
			}
			foreignNS := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: names.Roles[0], Namespace: "elsewhere"}}
			if isInstallOwnedRBAC(foreignNS) {
				t.Fatalf("%s: same Role name outside %s must NOT enqueue (namespace is part of the pin)", prof, ns)
			}
			cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: names.ClusterRoles[0]}}
			if !isInstallOwnedRBAC(cr) {
				t.Fatalf("%s: pinned ClusterRole %s must enqueue", prof, names.ClusterRoles[0])
			}
			other := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "not-pinned-role", Namespace: ns}}
			if isInstallOwnedRBAC(other) {
				t.Fatalf("%s: an unpinned Role name in %s must NOT enqueue", prof, ns)
			}
		})
	}
}

// TestProfileContractDeleteGateFinalizer: the CR deletion gate per profile
// — deleting the singleton while the profile's own storage is in use KEEPS
// the finalizer, surfaces DataInUse on the aggregate, and retains the
// profile's operands until the usage clears or forceDisable overrides.
func TestProfileContractDeleteGateFinalizer(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			renderedProfileOrSkip(t, prof)
			te := newTestEnv(t, validCRWithOnly(prof))
			te.reconcile(t, v1alpha1.QuartetName)
			ns := renderer.OperandNamespace(prof)
			seedDriverPVFor(t, te.client, prof, "data-pv", ns)
			seedBoundPVCIn(t, te.client, ns, "data-pvc", "data-pv")

			q := te.get(t, v1alpha1.QuartetName)
			if err := te.client.Delete(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != retryableRequeueAfter {
				t.Fatalf("blocked %s deletion must requeue, got %+v", prof, res)
			}
			stuck := te.get(t, v1alpha1.QuartetName)
			if !containsFinalizer(stuck, quartetFinalizer) {
				t.Fatalf("blocked %s deletion must KEEP the finalizer", prof)
			}
			c := condition(t, stuck.Status.Conditions, v1alpha1.ConditionReady)
			if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonDataInUse {
				t.Fatalf("aggregate Ready = (%v %q), want (False DataInUse) — the blocked profile's own usage", c.Status, c.Reason)
			}
			if profileCSIDriver(t, te.client, prof) == nil {
				t.Fatalf("blocked %s deletion must retain its operands", prof)
			}
		})
	}
}

// TestProfileContractUnknownProfile (the invalid-profile negative): a
// profile name outside the typed set renders nothing (For returns nil),
// contributes no identity and no RBAC names, and the reconciler would
// report RenderError for it — never disabled-or-Ready.
func TestProfileContractUnknownProfile(t *testing.T) {
	if p := renderer.For(renderer.ProfileInput{Profile: "bogus", Platform: v1alpha1.PlatformKubernetes}); p != nil {
		t.Fatalf("For(unknown profile) must return nil — no renderer may be invented for %q", "bogus")
	}
	if driver, prefix := renderer.ProvisionerIdentity("bogus"); driver != "" || prefix != "" {
		t.Fatalf("unknown profile must contribute no provisioner identity: %q %q", driver, prefix)
	}
}

func ptrToBool(v bool) *bool { return &v }

// -------------------------------------------------------------------
// Profile-consumed readiness targets (Profile.WorkloadTargets)
// -------------------------------------------------------------------

// renderedProfileFor returns the kubernetes-platform renderer for one
// profile or fails the test (all three profiles are wired — an honest
// skip would mask the contract going quiet).
func renderedProfileFor(t *testing.T, prof string) renderer.Profile {
	t.Helper()
	p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
	if p == nil {
		t.Fatalf("profile %s has no renderer — the three-profile contract is wired", prof)
	}
	return p
}

// fakeWorkloadReadyFor marks ONE profile's OWN rendered workloads (the
// names consumed from Profile.WorkloadTargets) as a converged rollout —
// the profile-parameterized analogue of the nfs-pinned fakeWorkloadReady.
func fakeWorkloadReadyFor(t *testing.T, c client.Client, prof string) {
	t.Helper()
	p := renderedProfileFor(t, prof)
	depName, dsName := p.WorkloadTargets()
	ns := p.Namespace()
	if depName == "" || dsName == "" {
		t.Fatalf("%s WorkloadTargets returned an empty name", prof)
	}
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: depName, Namespace: ns}, d); err != nil {
		t.Fatalf("get %s controller Deployment %q: %v", prof, depName, err)
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.AvailableReplicas = 1
	d.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("set %s Deployment status: %v", prof, err)
	}
	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: dsName, Namespace: ns}, ds); err != nil {
		t.Fatalf("get %s node DaemonSet %q: %v", prof, dsName, err)
	}
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = 1
	ds.Status.UpdatedNumberScheduled = 1
	ds.Status.NumberAvailable = 1
	if err := c.Status().Update(context.Background(), ds); err != nil {
		t.Fatalf("set %s DaemonSet status: %v", prof, err)
	}
}

// setDeploymentReadyFor marks only the profile's controller Deployment as
// converged (the DaemonSet stays in the state the test sets it to).
func setDeploymentReadyFor(t *testing.T, c client.Client, prof string) {
	t.Helper()
	p := renderedProfileFor(t, prof)
	depName, _ := p.WorkloadTargets()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: depName, Namespace: p.Namespace()}, d); err != nil {
		t.Fatalf("get %s controller Deployment: %v", prof, err)
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.AvailableReplicas = 1
	d.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("set %s Deployment status: %v", prof, err)
	}
}

// setDaemonSetTargetNodesFor sets the profile's DaemonSet rollout status to
// the given target-node count (zero = NoTargetNodes branch).
func setDaemonSetTargetNodesFor(t *testing.T, c client.Client, prof string, targets int32) {
	t.Helper()
	p := renderedProfileFor(t, prof)
	_, dsName := p.WorkloadTargets()
	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: dsName, Namespace: p.Namespace()}, ds); err != nil {
		t.Fatalf("get %s node DaemonSet: %v", prof, err)
	}
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = targets
	ds.Status.UpdatedNumberScheduled = targets
	ds.Status.NumberAvailable = targets
	if err := c.Status().Update(context.Background(), ds); err != nil {
		t.Fatalf("set %s DaemonSet status: %v", prof, err)
	}
}

// plantOtherProfileWorkloads plants ANOTHER profile's rendered workload
// names, fully converged, into prof's operand namespace — the foreign
// object that the old hardcoded-nfs-name probe would have accepted.
func plantOtherProfileWorkloads(t *testing.T, c client.Client, prof, other string) {
	t.Helper()
	depName, dsName := renderedProfileFor(t, other).WorkloadTargets()
	ns := renderer.OperandNamespace(prof)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: ns}}
	if err := c.Create(context.Background(), dep); err != nil {
		t.Fatalf("plant foreign Deployment: %v", err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Spec.Replicas = ptr.To(int32(1))
	dep.Status.Replicas = 1
	dep.Status.UpdatedReplicas = 1
	dep.Status.AvailableReplicas = 1
	if err := c.Status().Update(context.Background(), dep); err != nil {
		t.Fatalf("set planted Deployment status: %v", err)
	}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dsName, Namespace: ns}}
	if err := c.Create(context.Background(), ds); err != nil {
		t.Fatalf("plant foreign DaemonSet: %v", err)
	}
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = 1
	ds.Status.UpdatedNumberScheduled = 1
	ds.Status.NumberAvailable = 1
	if err := c.Status().Update(context.Background(), ds); err != nil {
		t.Fatalf("set planted DaemonSet status: %v", err)
	}
}

// blockedPodFor creates a DaemonSet-owned pod (ownerRef UID chain — the
// probe's ownership proof) stuck in the given waiting state in prof's
// operand namespace.
func blockedPodFor(t *testing.T, c client.Client, prof, name, waitingReason string) {
	t.Helper()
	p := renderedProfileFor(t, prof)
	_, dsName := p.WorkloadTargets()
	ns := p.Namespace()
	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: dsName, Namespace: ns}, ds); err != nil {
		t.Fatalf("get %s DaemonSet for owner chain: %v", prof, err)
	}
	// The fake client assigns no UIDs on create (the real API always does)
	// — pin one so the owner-chain match in the probe is meaningful.
	if ds.UID == "" {
		ds.SetUID(types.UID("uid-" + dsName))
		if err := c.Update(context.Background(), ds); err != nil {
			t.Fatalf("pin DaemonSet UID: %v", err)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "DaemonSet", Name: dsName,
			UID: ds.UID, Controller: ptr.To(true),
		}},
	}}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("create blocked pod: %v", err)
	}
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "driver",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}},
	}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("set blocked pod status: %v", err)
	}
}

// TestProfileContractWorkloadTargetsReadyTransition: each profile becomes
// Ready from ITS OWN controller Deployment + node DaemonSet, real render
// names consumed from Profile.WorkloadTargets in ITS OWN operand namespace.
// The old probe hardcoded the NFS names: fc/iscsi could never reach Ready
// here even with their own workloads healthy.
func TestProfileContractWorkloadTargetsReadyTransition(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t, validCRWithOnly(prof))
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != retryableRequeueAfter {
				t.Fatalf("%s: applied-but-unrolled workloads must requeue, got %+v", prof, res)
			}
			if c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[prof].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonWorkloadNotReady {
				t.Fatalf("%s: apply is not readiness = (%v %q), want (False WorkloadNotReady)", prof, c.Status, c.Reason)
			}

			fakeWorkloadReadyFor(t, te.client, prof)
			res = te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != 0 {
				t.Fatalf("%s: converged own workloads must not requeue, got %+v", prof, res)
			}
			q := te.get(t, v1alpha1.QuartetName)
			if c := condition(t, q.Status.Profiles[prof].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
				t.Fatalf("%s: own-name workloads converged = (%v %q), want Ready=True", prof, c.Status, c.Reason)
			}
			if agg := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); agg.Status != metav1.ConditionTrue {
				t.Fatalf("%s: aggregate must be Ready once its only enabled profile converged: (%v %q)", prof, agg.Status, agg.Reason)
			}
		})
	}
}

// TestProfileContractWorkloadTargetsIgnoreForeignProfileWorkloads: a
// ready workload set planted under ANOTHER profile's names in prof's
// operand namespace must not satisfy prof's probe — the probe consumes
// (names × namespace × owner-chain) of ITS OWN profile. The old
// hardcoded-nfs-name probe marked fc Ready on NFS-named objects planted
// in ngxstorage-fc.
func TestProfileContractWorkloadTargetsIgnoreForeignProfileWorkloads(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			other := ""
			for _, cand := range renderer.ProfileNames() {
				if cand != prof {
					other = cand
					break
				}
			}
			te := newTestEnv(t, validCRWithOnly(prof))
			te.reconcile(t, v1alpha1.QuartetName) // applies prof's own (unrolled) workloads
			plantOtherProfileWorkloads(t, te.client, prof, other)
			te.reconcile(t, v1alpha1.QuartetName)

			pc := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[prof].Conditions, v1alpha1.ConditionReady)
			if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonWorkloadNotReady {
				t.Fatalf("%s: foreign-named workloads satisfied the probe = (%v %q), want (False WorkloadNotReady)", prof, pc.Status, pc.Reason)
			}
			if !strings.Contains(pc.Message, "Deployment not ready") {
				t.Fatalf("%s: the probe must gate on prof's OWN Deployment rollout, not the planted names: %q", prof, pc.Message)
			}
		})
	}
}

// TestProfileContractWorkloadTargetsZeroTargetNodes: a zero-target-node
// DaemonSet keeps prof WorkloadNotReady with the NoTargetNodes marker on
// the profile's OWN rendered DaemonSet — per profile, not nfs only.
func TestProfileContractWorkloadTargetsZeroTargetNodes(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t, validCRWithOnly(prof))
			te.reconcile(t, v1alpha1.QuartetName)
			setDeploymentReadyFor(t, te.client, prof)
			setDaemonSetTargetNodesFor(t, te.client, prof, 0)
			te.reconcile(t, v1alpha1.QuartetName)

			pc := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[prof].Conditions, v1alpha1.ConditionReady)
			if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonWorkloadNotReady || !strings.Contains(pc.Message, "NoTargetNodes") {
				t.Fatalf("%s: zero-target-node own DaemonSet must be WorkloadNotReady+NoTargetNodes: (%v %q %q)", prof, pc.Status, pc.Reason, pc.Message)
			}
		})
	}
}

// TestProfileContractWorkloadTargetsBlockedPod: a blocked pod OWNED by
// prof's rendered DaemonSet (ownerRef UID chain) surfaces WorkloadNotReady
// naming the pod — per profile, in the profile's own namespace.
func TestProfileContractWorkloadTargetsBlockedPod(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t, validCRWithOnly(prof))
			te.reconcile(t, v1alpha1.QuartetName)
			fakeWorkloadReadyFor(t, te.client, prof)
			blockedPodFor(t, te.client, prof, prof+"-blocked-pod", "CreateContainerConfigError")
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != retryableRequeueAfter {
				t.Fatalf("%s: blocked owned pod must keep the profile retryable, got %+v", prof, res)
			}
			pc := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[prof].Conditions, v1alpha1.ConditionReady)
			if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonWorkloadNotReady {
				t.Fatalf("%s: blocked owned pod = (%v %q), want (False WorkloadNotReady)", prof, pc.Status, pc.Reason)
			}
			if !strings.Contains(pc.Message, prof+"-blocked-pod") || !strings.Contains(pc.Message, "CreateContainerConfigError") {
				t.Fatalf("%s: message must name the blocked pod and its reason as-is: %q", prof, pc.Message)
			}
		})
	}
}

// TestProfileContractWorkloadTargetsMissingWorkload: probing a profile
// whose rendered workloads do not exist is WorkloadNotReady on the profile's
// OWN target names — never the NFS names.
func TestProfileContractWorkloadTargetsMissingWorkload(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t)
			p := renderedProfileFor(t, prof)
			ready, reason, msg := te.reconciler.probeWorkloadReadiness(context.Background(), p)
			if ready || reason != v1alpha1.ReasonWorkloadNotReady {
				t.Fatalf("%s: missing workloads = (ready=%v reason=%q), want (false WorkloadNotReady)", prof, ready, reason)
			}
			depName, _ := p.WorkloadTargets()
			if !strings.Contains(msg, depName) {
				t.Fatalf("%s: missing-workload message must name the profile's OWN controller Deployment %q: %q", prof, depName, msg)
			}
		})
	}
}

// podListFailure blocks the probe's operand-pod List with an API error
// (boundary simulation: the client objects are real, the API's response
// is what is faked).
func podListFailure() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errors.New("pod list unavailable (test simulation)")
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// TestProfileContractWorkloadTargetsPodListAPIError: an API error listing
// operand pods during the probe surfaces DependencyMissing (retryable) —
// per profile, on the profile's own operand namespace scan.
func TestProfileContractWorkloadTargetsPodListAPIError(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			te := newInterceptedEnv(t, podListFailure(), validCRWithOnly(prof))
			te.reconcile(t, v1alpha1.QuartetName)    // apply; the probe stops at the unrolled Deployment
			fakeWorkloadReadyFor(t, te.client, prof) // Get/Status().Update — not intercepted (List-only fault)
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != retryableRequeueAfter {
				t.Fatalf("%s: probe API error must requeue, got %+v", prof, res)
			}
			pc := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[prof].Conditions, v1alpha1.ConditionReady)
			if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonDependencyMissing {
				t.Fatalf("%s: pod-list API error = (%v %q), want (False DependencyMissing)", prof, pc.Status, pc.Reason)
			}
			if !strings.Contains(pc.Message, "cannot list operand pods") {
				t.Fatalf("%s: message must name the failed pod scan: %q", prof, pc.Message)
			}
		})
	}
}

// -------------------------------------------------------------------
// Never-managed closed-profile gate (disable + termination)
// -------------------------------------------------------------------

// legacyUsageFor seeds a Bound PV + attached VolumeAttachment carrying
// prof's provisioner identity — usage that EXISTS but that this CR never
// managed (no operand of prof's carries the CR's owner-uid annotation).
func legacyUsageFor(t *testing.T, c client.Client, prof string) {
	t.Helper()
	ns := renderer.OperandNamespace(prof)
	seedDriverPVFor(t, c, prof, "legacy-"+prof+"-pv", ns)
	seedAttachedVA(t, c, "legacy-va-"+prof, "legacy-"+prof+"-pv")
}

// TestNeverManagedClosedProfileLegacyUsageIsNoOp: a closed profile this
// CR never managed (no owned operand exists in the API) is a NO-OP —
// even with that driver's legacy Bound PV + attached VA present. It must
// not make the aggregate DataInUse, claim a status entry, or requeue.
// The old gate ran for every renderable closed profile, so a legacy FC or
// iSCSI PV blocked an NFS-only CR's aggregate Ready.
func TestNeverManagedClosedProfileLegacyUsageIsNoOp(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		if prof == v1alpha1.ProfileNFS {
			continue // nfs is the enabled profile of this test
		}
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS))
			te.reconcile(t, v1alpha1.QuartetName)
			fakeWorkloadReady(t, te.client)
			te.reconcile(t, v1alpha1.QuartetName)
			legacyUsageFor(t, te.client, prof)

			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != 0 {
				t.Fatalf("%s never-managed closed profile must be a no-op, not a requeue: %+v", prof, res)
			}
			q := te.get(t, v1alpha1.QuartetName)
			if _, ok := q.Status.Profiles[prof]; ok {
				t.Fatalf("%s never-managed closed profile must claim no status entry (a no-op is not a claim)", prof)
			}
			if c := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
				t.Fatalf("nfs Ready must be preserved despite %s legacy usage: (%v %q)", prof, c.Status, c.Reason)
			}
			if agg := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); agg.Status != metav1.ConditionTrue || agg.Reason != v1alpha1.ReasonReady {
				t.Fatalf("aggregate must stay Ready: (%v %q)", agg.Status, agg.Reason)
			}
		})
	}
}

// TestNeverManagedClosedProfileTerminationCompletes: deleting an NFS-only
// CR while a never-managed closed profile has legacy Bound PV + attached
// VA COMPLETES — the finalizer is removed and the CR is gone; the legacy
// storage of the other installation is untouched. The old termination
// gate retained the finalizer DataInUse on exactly this cluster state.
func TestNeverManagedClosedProfileTerminationCompletes(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		if prof == v1alpha1.ProfileNFS {
			continue
		}
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t, validCRWithOnly(v1alpha1.ProfileNFS))
			te.reconcile(t, v1alpha1.QuartetName)
			fakeWorkloadReady(t, te.client)
			te.reconcile(t, v1alpha1.QuartetName)
			legacyUsageFor(t, te.client, prof)

			q := te.get(t, v1alpha1.QuartetName)
			if err := te.client.Delete(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			res := te.reconcile(t, v1alpha1.QuartetName)
			if res.RequeueAfter != 0 {
				t.Fatalf("termination with never-managed %s legacy usage must complete, not requeue: %+v", prof, res)
			}
			got := &v1alpha1.Quartet{}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, got); !apierrors.IsNotFound(err) {
				t.Fatalf("CR must be gone after finalization (err=%v)", err)
			}
			// The managed profile's operands were cleaned; the legacy
			// storage of the never-managed profile survives untouched.
			if getDeployment(t, te.client) != nil || getCSIDriver(t, te.client) != nil {
				t.Fatal("nfs (managed) operands must be cleaned on termination")
			}
			pv := &corev1.PersistentVolume{}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: "legacy-" + prof + "-pv"}, pv); err != nil {
				t.Fatalf("legacy %s PV must survive untouched: %v", prof, err)
			}
			va := &storagev1.VolumeAttachment{}
			if err := te.client.Get(context.Background(), types.NamespacedName{Name: "legacy-va-" + prof}, va); err != nil {
				t.Fatalf("legacy %s VA must survive untouched: %v", prof, err)
			}
		})
	}
}

// deploymentListFailure blocks one of the ownership-evidence lists
// (DeploymentList — an operand kind every profile enumerates) with an API
// error. It does NOT block the usage scan's PV/PVC/VA lists or the
// cleanup's other kinds, so the tests isolate the detection error branch.
func deploymentListFailure() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.DeploymentList); ok {
				return errors.New("deployment list unavailable (test simulation)")
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// TestOwnedOperandDetectionFailClosedBlocksOnAPIError: when the
// ownership-evidence read fails, the profile is treated as MANAGED — the
// usage gate still runs and active usage still blocks (DataInUse,
// finalizer retained on deletion). Fail-open would silently no-op a
// profile that may own live drivers.
func TestOwnedOperandDetectionFailClosedBlocksOnAPIError(t *testing.T) {
	t.Run("disable-path", func(t *testing.T) {
		te := newInterceptedEnv(t, deploymentListFailure(), validCR())
		te.reconcile(t, v1alpha1.QuartetName) // nfs applied (apply lists no Deployments)
		seedDriverPV(t, te.client, "data-pv", gateNS)
		seedBoundPVC(t, te.client, "data-pvc")

		disableNFS(t, te)
		res := te.reconcile(t, v1alpha1.QuartetName)
		if res.RequeueAfter != retryableRequeueAfter {
			t.Fatalf("fail-closed detection with active usage must requeue the gate, got %+v", res)
		}
		assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
		if getDeployment(t, te.client) == nil || getCSIDriver(t, te.client) == nil {
			t.Fatal("fail-closed detection must retain the managed profile's operands")
		}
	})

	t.Run("termination-path", func(t *testing.T) {
		te := newInterceptedEnv(t, deploymentListFailure(), validCR())
		te.reconcile(t, v1alpha1.QuartetName)
		seedDriverPV(t, te.client, "data-pv", gateNS)
		seedBoundPVC(t, te.client, "data-pvc")

		q := te.get(t, v1alpha1.QuartetName)
		if err := te.client.Delete(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		res := te.reconcile(t, v1alpha1.QuartetName)
		if res.RequeueAfter != retryableRequeueAfter {
			t.Fatalf("fail-closed detection on a terminating CR must requeue, got %+v", res)
		}
		stuck := te.get(t, v1alpha1.QuartetName)
		if !containsFinalizer(stuck, quartetFinalizer) {
			t.Fatal("fail-closed detection must KEEP the finalizer")
		}
		if c := condition(t, stuck.Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonDataInUse {
			t.Fatalf("aggregate = (%v %q), want (False DataInUse) — detection error failed closed to managed", c.Status, c.Reason)
		}
	})
}

// restartFakeEnv simulates an operator RESTART against the same API state:
// a fresh reconciler and a fresh fake client seeded with every object the
// source env's tracker holds (namespaces, the operand kinds, PV/PVC/VA)
// plus the CR exactly as supplied. An operator restart loses the
// reconciler's in-memory state, not the cluster state — any detection that
// survives it must read the API. The four installer-owned RBAC kinds are
// NOT copied: newTestEnv re-seeds that identical healthy set, and the
// source copy would double-create it.
func restartFakeEnv(t *testing.T, te *fakeEnv, cr *v1alpha1.Quartet) *fakeEnv {
	t.Helper()
	var objs []client.Object
	copyAll := func(list client.ObjectList) {
		if err := te.client.List(context.Background(), list); err != nil {
			t.Fatalf("list for restart copy: %v", err)
		}
		items := reflect.ValueOf(list).Elem().FieldByName("Items")
		for i := 0; i < items.Len(); i++ {
			objs = append(objs, items.Index(i).Addr().Interface().(client.Object))
		}
	}
	nsList := &corev1.NamespaceList{}
	copyAll(nsList)
	for _, k := range []client.Object{
		&corev1.ServiceAccount{}, &corev1.ConfigMap{}, &coordinationv1.Lease{},
		&corev1.Service{}, &appsv1.Deployment{}, &appsv1.DaemonSet{},
		&storagev1.CSIDriver{},
	} {
		copyAll(listFor(t, k))
	}
	copyAll(&corev1.PersistentVolumeList{})
	copyAll(&corev1.PersistentVolumeClaimList{})
	copyAll(&storagev1.VolumeAttachmentList{})
	objs = append(objs, cr)
	return newTestEnv(t, objs...)
}

// TestPreviouslyManagedDisableGateSurvivesRestartAndStatusClear: a
// previously-managed profile (owned operands still present in the API,
// active usage seeded) keeps its disable-gate protection across an operator
// restart — with the CR status PRESERVED and with it CLEARED. Empty status
// alone is not proof of never-managed: the evidence is the API read.
func TestPreviouslyManagedDisableGateSurvivesRestartAndStatusClear(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName) // nfs applied: owned operands now exist
	seedDriverPV(t, te.client, "data-pv", gateNS)
	seedBoundPVC(t, te.client, "data-pvc")
	disableNFS(t, te)
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("precondition: active usage must block the disable, got %+v", res)
	}
	assertGateBlocked(t, te.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)

	srcCR := te.get(t, v1alpha1.QuartetName)

	t.Run("restart-status-preserved", func(t *testing.T) {
		te2 := restartFakeEnv(t, te, srcCR.DeepCopyObject().(*v1alpha1.Quartet))
		res := te2.reconcile(t, v1alpha1.QuartetName)
		if res.RequeueAfter != retryableRequeueAfter {
			t.Fatalf("restart must keep the gate blocked (requeue), got %+v", res)
		}
		assertGateBlocked(t, te2.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
		if getDeployment(t, te2.client) == nil || getCSIDriver(t, te2.client) == nil {
			t.Fatal("restart must not tear down the managed profile's operands")
		}
	})

	t.Run("restart-status-cleared", func(t *testing.T) {
		cleared := srcCR.DeepCopyObject().(*v1alpha1.Quartet)
		cleared.Status = v1alpha1.QuartetStatus{} // empty status is NOT proof of never-managed
		te3 := restartFakeEnv(t, te, cleared)
		res := te3.reconcile(t, v1alpha1.QuartetName)
		if res.RequeueAfter != retryableRequeueAfter {
			t.Fatalf("status-cleared restart must keep the gate blocked (requeue), got %+v", res)
		}
		assertGateBlocked(t, te3.get(t, v1alpha1.QuartetName), v1alpha1.ReasonDataInUse)
		if getDeployment(t, te3.client) == nil || getCSIDriver(t, te3.client) == nil {
			t.Fatal("status-cleared restart must not tear down the managed profile's operands")
		}
	})
}

// -------------------------------------------------------------------
// Install-RBAC watch tuple: (kind, name, namespace) bound to ONE profile
// -------------------------------------------------------------------

// TestInstallRBACWatchFilterBindsTupleToSameProfile: a namespaced install-
// RBAC object enqueues only when its pinned NAME and its NAMESPACE belong
// to the SAME profile. The old two-leg check (name in the union set,
// namespace in ANY profile's) let a Role named for NFS's install set sit
// in the FC namespace — or any cross-profile combination — and enqueue a
// reconcile no profile actually owns.
func TestInstallRBACWatchFilterBindsTupleToSameProfile(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			names := renderer.OperandRBACNames(prof)
			ns := renderer.OperandNamespace(prof)
			if len(names.Roles) == 0 || len(names.RoleBindings) == 0 {
				t.Fatalf("%s pinned set is empty — nothing to filter", prof)
			}
			// Positive: the profile's own pinned name in its own namespace.
			if !isInstallOwnedRBAC(&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: names.Roles[0], Namespace: ns}}) {
				t.Fatalf("%s: own pinned Role %s/%s must enqueue", prof, ns, names.Roles[0])
			}
			if !isInstallOwnedRBAC(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: names.RoleBindings[0], Namespace: ns}}) {
				t.Fatalf("%s: own pinned RoleBinding %s/%s must enqueue", prof, ns, names.RoleBindings[0])
			}
			// Negative: the SAME pinned name in EVERY other profile's
			// operand namespace (the exact hole: nfs Role in ngxstorage-fc).
			for _, other := range renderer.ProfileNames() {
				if other == prof {
					continue
				}
				otherNS := renderer.OperandNamespace(other)
				if isInstallOwnedRBAC(&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: names.Roles[0], Namespace: otherNS}}) {
					t.Fatalf("%s: Role %q planted in %s must NOT enqueue — (kind,name,namespace) is pinned to one profile", prof, names.Roles[0], otherNS)
				}
				if isInstallOwnedRBAC(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: names.RoleBindings[0], Namespace: otherNS}}) {
					t.Fatalf("%s: RoleBinding %q planted in %s must NOT enqueue", prof, names.RoleBindings[0], otherNS)
				}
				// Negative: ANOTHER profile's pinned name in prof's
				// namespace (the mirror direction).
				otherNames := renderer.OperandRBACNames(other)
				if otherNames.Roles[0] == names.Roles[0] {
					t.Fatalf("pinned Role sets of %s and %s must be disjoint: %q", prof, other, names.Roles[0])
				}
				if isInstallOwnedRBAC(&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: otherNames.Roles[0], Namespace: ns}}) {
					t.Fatalf("%s: Role %q named for %s's set must NOT enqueue from %s", prof, otherNames.Roles[0], other, ns)
				}
			}
		})
	}
}

// -------------------------------------------------------------------
// Usage scanner: the PVC leg scans the OPERAND namespace, not the name
// -------------------------------------------------------------------

// TestUsageGatePVCScanScansOperandNamespace: a Bound PVC tied to an active
// driver PV counts for the gate's PVC leg only in the profile's operand
// namespace (ngxstorage-<profile>). Using the profile NAME
// ("nfs"/"fc"/"iscsi") as the namespace would scan a namespace that never
// holds operands — the summary line could not name the PVC. The
// cluster-wide PV/VA legs are unaffected.
func TestUsageGatePVCScanScansOperandNamespace(t *testing.T) {
	for _, prof := range renderer.ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			te := newTestEnv(t, validCRWithOnly(prof))
			te.reconcile(t, v1alpha1.QuartetName)
			ns := renderer.OperandNamespace(prof)
			seedDriverPVFor(t, te.client, prof, "nscheck-pv", "some-other-ns")
			seedBoundPVCIn(t, te.client, ns, "nscheck-pvc", "nscheck-pv")

			disableProfile(t, te, prof)
			pc := condition(t, te.get(t, v1alpha1.QuartetName).Status.Profiles[prof].Conditions, v1alpha1.ConditionReady)
			if pc.Status != metav1.ConditionFalse || pc.Reason != v1alpha1.ReasonDataInUse {
				t.Fatalf("%s: bound PVC + active PV must block the disable: (%v %q)", prof, pc.Status, pc.Reason)
			}
			if !strings.Contains(pc.Message, "nscheck-pvc") {
				t.Fatalf("%s: the gate's PVC leg must scan the operand namespace %q and name the PVC: %q", prof, ns, pc.Message)
			}
		})
	}
}
