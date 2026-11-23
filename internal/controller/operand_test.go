package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"testing"
	"time"
)

// --- fake-client helpers for operand tests. ---

// sccTestGVK registers the OpenShift SCC kind in fake-client test schemes
// (envtest cannot simulate OpenShift API groups).
// sccListTestGVK registers the LIST kind too: disable/delete cleanup
// enumerates the SCC kind (cluster-scoped OperandKind), and the fake
// tracker resolves the list GVK through the scheme.
var (
	sccTestGVK     = schema.GroupVersionKind{Group: "security.openshift.io", Version: "v1", Kind: "SecurityContextConstraints"}
	sccListTestGVK = schema.GroupVersionKind{Group: "security.openshift.io", Version: "v1", Kind: "SecurityContextConstraintsList"}
)

// installRBACSeed returns the renderer's installer-owned RBAC set for one
// platform — the UNION over every rendered profile — WITHOUT the CR owner
// annotation: the out-of-band model provisions operand RBAC via the install
// manifest (config/operands/<profile>/), and the reconciler only verifies it
// by pinned name at reconcile start (RBACDrift gate). Fake reconcile-level
// envs seed it so the health gate sees a healthy install; drift tests remove
// or mutate specific objects on top of the seed. Unrendered profiles are
// skipped (they have no install set to seed).
func installRBACSeed(platform v1alpha1.Platform) []client.Object {
	var out []client.Object
	for _, prof := range renderer.ProfileNames() {
		p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: platform})
		if p == nil {
			continue
		}
		out = append(out, p.InstallRBAC()...)
	}
	return out
}

// seedUIDs assigns deterministic UIDs to seeded objects lacking one. The fake
// client's WithObjects does NOT assign UIDs the way the real API server does
// on create; ownership checks compare against the CR UID, so tests must
// mimic the API's behavior or every object would look owned by UID "".
func seedUIDs(objs []client.Object) []client.Object {
	for _, o := range objs {
		if o.GetUID() == "" {
			// Seeded objects have unique names within one env (a CR plus
			// optional foreign operands), so name alone is a stable UID.
			o.SetUID(types.UID("uid-" + o.GetName()))
		}
	}
	return objs
}

func newOCPTestScheme(t *testing.T) *runtime.Scheme {
	s := newTestScheme(t)
	// Register the unstructured SCC GVK so the fake tracker can resolve it.
	// (Unstructured objects need no scheme conversion beyond this mapping.)
	s.AddKnownTypeWithName(sccTestGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(sccListTestGVK, &unstructured.UnstructuredList{})
	return s
}

// newOCPTestEnv builds a fake env whose discovery reports an OpenShift
// platform, with the SCC GVK registered for the fake tracker.
func newOCPTestEnv(t *testing.T, objs ...client.Object) *fakeEnv {
	t.Helper()
	// The OCP installer-owned RBAC set (incl. the node SCC use grant) is
	// pre-seeded so the RBACDrift health gate sees a healthy install.
	objs = append(objs, installRBACSeed(v1alpha1.PlatformOpenShift)...)
	cl := fake.NewClientBuilder().
		WithScheme(newOCPTestScheme(t)).
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
			PlatformDiscovery: fakeDiscovery{groups: ocpGroups()},
			Applier:           mergeApplier{c: cl},
		},
		client:   cl,
		recorder: rec,
	}
}

// newInterceptedEnv builds a fake env whose client returns the injected API
// error from interceptor.Funcs (boundary simulation: 403/409/404/timeout
// classification is exercised against real client objects; the API's
// response is what is faked).
func newInterceptedEnv(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *fakeEnv {
	t.Helper()
	// Pre-seed the k8s installer-owned RBAC set (healthy install for the
	// RBACDrift gate); interception tests exercise the error paths on top.
	objs = append(objs, installRBACSeed(v1alpha1.PlatformKubernetes)...)
	cl := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(seedUIDs(objs)...).
		WithStatusSubresource(&v1alpha1.Quartet{}, &appsv1.Deployment{}, &appsv1.DaemonSet{}).
		WithInterceptorFuncs(funcs).
		Build()
	rec := record.NewFakeRecorder(100)
	return &fakeEnv{
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
}

func getDeployment(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: "ngxstorage-nfs"}, d); err != nil {
		return nil
	}
	return d
}

func driverImage(d *appsv1.Deployment) string {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "nfs-csi-ngxstorage" {
			return c.Image
		}
	}
	return ""
}

// fakeWorkloadReady sets the API-reported workload status the readiness
// probe gates on. The fake client has no kubelet or
// Deployment controller, so tests that expect Ready=True must simulate a
// converged rollout explicitly — the probe logic is the subject, not the
// scheduler. Live rollout readiness requires a real cluster.
func fakeWorkloadReady(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	d := getDeployment(t, c)
	if d == nil {
		t.Fatal("Deployment missing for fakeWorkloadReady")
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.AvailableReplicas = 1
	d.Status.ReadyReplicas = 1
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatalf("set Deployment status: %v", err)
	}
	ds := &appsv1.DaemonSet{}
	owned(t, c, types.NamespacedName{Name: "nfs-csi-ngxstorage-node", Namespace: "ngxstorage-nfs"}, ds)
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = 1
	ds.Status.UpdatedNumberScheduled = 1
	ds.Status.NumberAvailable = 1
	if err := c.Status().Update(ctx, ds); err != nil {
		t.Fatalf("set DaemonSet status: %v", err)
	}
}

// convergeReadiness applies the operand set (first reconcile), simulates a
// converged rollout via fakeWorkloadReady, and reconciles again so the
// readiness probe observes the rollout. Returns the final reconcile result
// (steady-state: no requeue) for assertion.
func convergeReadiness(t *testing.T, te *fakeEnv) ctrl.Result {
	t.Helper()
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	return te.reconcile(t, v1alpha1.QuartetName)
}

func getCSIDriver(t *testing.T, c client.Client) *storagev1.CSIDriver {
	t.Helper()
	cd := &storagev1.CSIDriver{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, cd); err != nil {
		return nil
	}
	return cd
}

func getNamespace(c client.Client, name string) *corev1.Namespace {
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, ns); err != nil {
		return nil
	}
	return ns
}

// countOwnedLeases returns how many coordination leases in the operand
// namespace carry the ownership annotation.
func countOwnedLeases(t *testing.T, c client.Client, ownerUID string) int {
	t.Helper()
	list := &coordinationv1.LeaseList{}
	if err := c.List(context.Background(), list, client.InNamespace("ngxstorage-nfs")); err != nil {
		t.Fatalf("list leases: %v", err)
	}
	n := 0
	for i := range list.Items {
		if list.Items[i].Annotations[renderer.OwnerUIDAnnotation] == ownerUID {
			n++
		}
	}
	return n
}

// captureOwnedState serializes every owned operand (11 kinds + namespace)
// into a canonical JSON snapshot for no-op / isolation comparisons.
func captureOwnedState(t *testing.T, c client.Client) string {
	t.Helper()
	var sb strings.Builder
	kinds := []client.Object{
		&corev1.ConfigMap{}, &coordinationv1.Lease{}, &corev1.Service{},
		&appsv1.Deployment{}, &appsv1.DaemonSet{}, &rbacv1.Role{}, &rbacv1.RoleBinding{},
		&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}, &storagev1.CSIDriver{},
		&corev1.ServiceAccount{},
	}
	for _, k := range kinds {
		list := listFor(t, k)
		opts := []client.ListOption{}
		if !isClusterScopedObj(k) {
			opts = append(opts, client.InNamespace("ngxstorage-nfs"))
		}
		if err := c.List(context.Background(), list, opts...); err != nil {
			t.Fatalf("list %T: %v", k, err)
		}
		sb.Write(stripServerMeta(t, list))
	}
	if ns := getNamespace(c, "ngxstorage-nfs"); ns != nil {
		b, _ := json.Marshal(ns)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if md, _ := m["metadata"].(map[string]any); md != nil {
			for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields"} {
				delete(md, k)
			}
		}
		b2, _ := json.Marshal(m)
		sb.Write(b2)
	}
	return sb.String()
}

// stripServerMeta serializes a list with the server-managed metadata fields
// (resourceVersion/uid/creationTimestamp/managedFields) removed, so the
// no-op/isolation comparisons test CONTENT stability, not etcd bookkeeping
// (each fake-client write bumps resourceVersion even for identical content).
func stripServerMeta(t *testing.T, list client.ObjectList) []byte {
	t.Helper()
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal list %T: %v", list, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal list %T: %v", list, err)
	}
	items, _ := m["items"].([]any)
	for _, it := range items {
		im, _ := it.(map[string]any)
		md, _ := im["metadata"].(map[string]any)
		for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields"} {
			delete(md, k)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal list %T: %v", list, err)
	}
	return out
}

func isClusterScopedObj(obj client.Object) bool {
	switch obj.(type) {
	case *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding, *storagev1.CSIDriver:
		return true
	}
	return false
}

func listFor(t *testing.T, obj client.Object) client.ObjectList {
	t.Helper()
	switch obj.(type) {
	case *corev1.ConfigMap:
		return &corev1.ConfigMapList{}
	case *coordinationv1.Lease:
		return &coordinationv1.LeaseList{}
	case *corev1.Service:
		return &corev1.ServiceList{}
	case *appsv1.Deployment:
		return &appsv1.DeploymentList{}
	case *appsv1.DaemonSet:
		return &appsv1.DaemonSetList{}
	case *rbacv1.Role:
		return &rbacv1.RoleList{}
	case *rbacv1.RoleBinding:
		return &rbacv1.RoleBindingList{}
	case *rbacv1.ClusterRole:
		return &rbacv1.ClusterRoleList{}
	case *rbacv1.ClusterRoleBinding:
		return &rbacv1.ClusterRoleBindingList{}
	case *storagev1.CSIDriver:
		return &storagev1.CSIDriverList{}
	case *corev1.ServiceAccount:
		return &corev1.ServiceAccountList{}
	default:
		t.Fatalf("no list for %T", obj)
		return nil
	}
}

func owned(t *testing.T, c client.Client, key types.NamespacedName, into client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), key, into); err != nil {
		t.Fatalf("get %v: %v", key, err)
	}
}

// -------------------------------------------------------------------
// Operand lifecycle — create / idempotent / drift / delete / foreign / UID checks
// -------------------------------------------------------------------

// TestNFSOperandsCreatedAndOwned: a single reconcile materializes
// the full NFS RUNTIME set (the 141-object render minus the six installer-
// owned RBAC objects) plus the operand namespace, every
// runtime object stamped with the CR UID ownership annotation.
func TestNFSOperandsCreatedAndOwned(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	// Operand apply is not readiness. Converge the workload
	// statuses, then reconcile again so the probe sees a rolled-out set.
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)
	q := te.get(t, v1alpha1.QuartetName)
	uid := string(q.UID)

	if getNamespace(te.client, "ngxstorage-nfs") == nil {
		t.Fatal("operand namespace ngxstorage-nfs not created")
	}
	if getCSIDriver(t, te.client) == nil {
		t.Fatal("CSIDriver not created")
	}
	if getDeployment(t, te.client) == nil {
		t.Fatal("controller Deployment not created")
	}
	if got := countOwnedLeases(t, te.client, uid); got != 128 {
		t.Fatalf("owned leases = %d, want 128", got)
	}
	// Every RUNTIME operand carries the ownership annotation.
	cm := &corev1.ConfigMap{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-coordination-config", Namespace: "ngxstorage-nfs"}, cm)
	if cm.Annotations[renderer.OwnerUIDAnnotation] != uid {
		t.Fatalf("ConfigMap owner annotation = %q, want CR UID %q", cm.Annotations[renderer.OwnerUIDAnnotation], uid)
	}
	// The installer-owned operand RBAC stays annotation-FREE and untouched:
	// the runtime never applies it, only verifies it (RBACDrift gate).
	role := &rbacv1.ClusterRole{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, role)
	if _, has := role.Annotations[renderer.OwnerUIDAnnotation]; has {
		t.Fatalf("install-owned ClusterRole must NOT carry the CR owner annotation, got %q", role.Annotations[renderer.OwnerUIDAnnotation])
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("aggregate Ready = (%v %q), want True", c.Status, c.Reason)
	}
}

// TestIdempotentSecondReconcileNoop: the second reconcile
// writes no status and leaves every owned object byte-identical.
func TestIdempotentSecondReconcileNoop(t *testing.T) {
	// Counts ONLY Quartet CR status writes: the workload-status updates from
	// fakeWorkloadReady are also "status" subresource writes and must not be
	// attributed to the CR change guard.
	var statusWrites int
	te := newInterceptedEnv(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if sub == "status" {
				if _, isCR := obj.(*v1alpha1.Quartet); isCR {
					statusWrites++
				}
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName) // converge to Ready steady state
	before := captureOwnedState(t, te.client)
	base := statusWrites

	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("steady reconcile must not requeue, got %+v", res)
	}
	if after := captureOwnedState(t, te.client); before != after {
		t.Error("second reconcile mutated owned objects (idempotency violated)")
	}
	if statusWrites != base {
		t.Fatalf("steady reconcile wrote Quartet status %d extra times, want 0 (change guard)", statusWrites-base)
	}
}

// TestDriftCorrectedExternally: an external edit to an owned
// Deployment is corrected on the next reconcile (desired wins, no-force
// SSA semantics).
func TestDriftCorrectedExternally(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	// Mutate the DRIVER container by name (not containers[0])
	// and verify the broken state before asserting the repair — the mutation
	// and the assertion must read the same field.
	d := getDeployment(t, te.client)
	c := driverContainer(d)
	if c == nil {
		t.Fatal("driver container nfs-csi-ngxstorage not found")
	}
	if c.Image != testImageRef {
		t.Fatalf("precondition: driver image = %q, want %q", c.Image, testImageRef)
	}
	evil := "evil.example/attacker@sha256:" + strings.Repeat("c", 64)
	c.Image = evil
	if err := te.client.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	broken := getDeployment(t, te.client)
	if got := driverContainer(broken); got == nil || got.Image != evil {
		t.Fatalf("precondition failed: drift not applied to the driver container (got %+v)", got)
	}

	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)

	got := getDeployment(t, te.client)
	if driverImage(got) != testImageRef {
		t.Fatalf("drifted driver image not corrected, got %q", driverImage(got))
	}
	if c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("aggregate Ready after drift correction = (%v %q)", c.Status, c.Reason)
	}
}

// TestDeletedOwnedChildRecreated: a deleted owned
// CSIDriver is recreated on the next reconcile.
func TestDeletedOwnedChildRecreated(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), getCSIDriver(t, te.client)); err != nil {
		t.Fatal(err)
	}
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("precondition: CSIDriver still present")
	}
	te.reconcile(t, v1alpha1.QuartetName)
	q := te.get(t, v1alpha1.QuartetName)
	cd := getCSIDriver(t, te.client)
	if cd == nil {
		t.Fatal("deleted owned CSIDriver was not recreated")
	}
	if cd.Annotations[renderer.OwnerUIDAnnotation] != string(q.UID) {
		t.Fatal("recreated CSIDriver lacks the ownership annotation")
	}
}

// TestLeaseStateNeverRewritten: the driver's coordination Lease
// state (holder identity, timestamps) survives a reconcile — only metadata
// is owned, Lease state is never reset.
func TestLeaseStateNeverRewritten(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	l := &coordinationv1.Lease{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-operation-lock-000", Namespace: "ngxstorage-nfs"}, l)
	l.Spec.HolderIdentity = ptr.To("driver-pod-1")
	now := metav1.NewMicroTime(time.Now())
	l.Spec.AcquireTime = &now
	l.Spec.RenewTime = &now
	if err := te.client.Update(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)

	got := &coordinationv1.Lease{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-operation-lock-000", Namespace: "ngxstorage-nfs"}, got)
	if got.Spec.HolderIdentity == nil || *got.Spec.HolderIdentity != "driver-pod-1" {
		t.Fatal("reconcile rewrote the coordination Lease state")
	}
}

// TestForeignConflictNoAdoption: a same-name CSIDriver
// without the ownership annotation blocks the profile with
// ForeignResourceConflict — no adoption, no modification, no force.
func TestForeignConflictNoAdoption(t *testing.T) {
	foreign := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "nfs.csi.ngxstorage.com",
			Labels: map[string]string{"managed-by": "user"},
		},
		Spec: storagev1.CSIDriverSpec{},
	}
	te := newInterceptedEnv(t, interceptor.Funcs{}, foreign, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("foreign conflict is terminal (watch/spec-triggered recovery), got %+v", res)
	}
	q := te.get(t, v1alpha1.QuartetName)
	c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonForeignResourceConflict {
		t.Fatalf("aggregate Ready reason = %q, want ForeignResourceConflict", c.Reason)
	}
	got := &storagev1.CSIDriver{}
	owned(t, te.client, types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, got)
	if got.Labels["managed-by"] != "user" {
		t.Fatal("foreign CSIDriver was modified — adoption is forbidden")
	}
	if _, has := got.Annotations[renderer.OwnerUIDAnnotation]; has {
		t.Fatal("foreign CSIDriver must not gain the ownership annotation")
	}
	ps := q.Status.Profiles[v1alpha1.ProfileNFS]
	if pc := condition(t, ps.Conditions, v1alpha1.ConditionReady); pc.Reason != v1alpha1.ReasonForeignResourceConflict {
		t.Fatalf("profile nfs reason = %q, want ForeignResourceConflict", pc.Reason)
	}
}

// TestForeignUIDConflict: an object carrying ANOTHER
// Quartet's owner UID is foreign too (UID verification, not just presence).
func TestForeignUIDConflict(t *testing.T) {
	foreign := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nfs.csi.ngxstorage.com",
			Annotations: map[string]string{renderer.OwnerUIDAnnotation: "some-other-cr-uid"},
		},
	}
	te := newInterceptedEnv(t, interceptor.Funcs{}, foreign, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonForeignResourceConflict {
		t.Fatalf("aggregate reason = %q, want ForeignResourceConflict for foreign UID", c.Reason)
	}
}

// -------------------------------------------------------------------
// NFS input validation + platform error (no operand side effects)
// -------------------------------------------------------------------

// TestNFSInvalidInputNoOperands: reconciler-side input validation
// (the fake client bypasses CRD schema, so the reconciler check is the
// subject) leaves NO operand or namespace behind.
func TestNFSInvalidInputNoOperands(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(q *v1alpha1.Quartet)
	}{
		{"bad secret name", func(q *v1alpha1.Quartet) { q.Spec.Profiles.NFS.SecretRef.Name = "Bad_Name" }},
		{"tag instead of digest", func(q *v1alpha1.Quartet) { q.Spec.Profiles.NFS.Image.Reference = "quay.io/ngxstorage/nfs:v1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := validCR()
			tc.mutate(q)
			te := newTestEnv(t, q)
			te.reconcile(t, v1alpha1.QuartetName)
			c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
			if c.Reason != v1alpha1.ReasonInvalidSpec {
				t.Fatalf("reason = %q, want InvalidSpec", c.Reason)
			}
			if getNamespace(te.client, "ngxstorage-nfs") != nil {
				t.Fatal("invalid spec must not create the operand namespace")
			}
			if getCSIDriver(t, te.client) != nil {
				t.Fatal("invalid spec must not create operands")
			}
		})
	}
}

// TestNFSPlatformErrorNoOperands: a platform detection conflict stops
// before any operand exists.
func TestNFSPlatformErrorNoOperands(t *testing.T) {
	q := validCR()
	q.Spec.Platform = v1alpha1.PlatformOpenShift // conflicts with k8s discovery
	te := newTestEnv(t, q)
	te.reconcile(t, v1alpha1.QuartetName)
	if getNamespace(te.client, "ngxstorage-nfs") != nil {
		t.Fatal("platform error must precede any operand creation")
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonPlatformDetectionFailed {
		t.Fatalf("reason = %q, want PlatformDetectionFailed", c.Reason)
	}
}

// -------------------------------------------------------------------
// Singleton: a non-singleton CR produces no operands
// -------------------------------------------------------------------

func TestSingletonRaceNoOperands(t *testing.T) {
	second := validCR()
	second.Name = "quartet-2"
	te := newTestEnv(t, validCR(), second)
	te.reconcile(t, "quartet-2") // the foreign name reconciles first
	// Strengthened singleton claim: the foreign-named CR never manages operands —
	// the FULL absence of every operand kind and the operand namespace.
	// The API accepts the name; only the reconciler refuses it.
	assertNoOperands(t, te.client)
	c := condition(t, te.get(t, "quartet-2").Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("reason = %q, want InvalidSpec", c.Reason)
	}
}

// -------------------------------------------------------------------
// OCP path (fake client — envtest cannot simulate OpenShift groups)
// -------------------------------------------------------------------

// TestOCPRendersAppliesSCC: on OpenShift the full 144-object set
// materializes — the runtime apply covers everything EXCEPT the 8 installer-
// owned RBAC objects (the scc-use Role/RoleBinding are
// provisioned by the install set, pre-seeded here; the SCC itself remains
// operator-applied) — including the SCC and the worker-pinned node DS.
func TestOCPRendersAppliesSCC(t *testing.T) {
	te := newOCPTestEnv(t, validCR())
	convergeReadiness(t, te)
	q := te.get(t, v1alpha1.QuartetName)
	if q.Status.ResolvedPlatform != v1alpha1.PlatformOpenShift {
		t.Fatalf("resolved platform = %q, want openshift", q.Status.ResolvedPlatform)
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("aggregate Ready = (%v %q)", c.Status, c.Reason)
	}

	// The fake tracker scopes the unregistered SCC GVK namespaced (the real
	// OCP API treats SCC as cluster-scoped and ignores the namespace), so the
	// test lookup must carry the namespace the renderer set.
	scc := &unstructured.Unstructured{}
	scc.SetGroupVersionKind(sccTestGVK)
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-node-scc", Namespace: "ngxstorage-nfs"}, scc); err != nil {
		t.Fatalf("SCC not applied on OCP: %v", err)
	}
	if !scc.Object["allowPrivilegedContainer"].(bool) {
		t.Fatal("SCC must allow privileged containers")
	}
	// The SCC is the only unstructured operand, and unstructured
	// Get/SetAnnotations convert maps — an ownership stamp that does not
	// survive that conversion would make a second reconcile classify the
	// SCC as foreign. Ownership must survive apply+readback.
	if scc.GetAnnotations()[renderer.OwnerUIDAnnotation] != string(q.UID) {
		t.Fatalf("SCC owner annotation = %v, want CR UID %q", scc.GetAnnotations(), q.UID)
	}
	role := &rbacv1.Role{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-node-scc-use", Namespace: "ngxstorage-nfs"}, role)
	rule := role.Rules[0]
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "nfs-csi-ngxstorage-node-scc" {
		t.Fatalf("scc-use Role must grant use on the SCC by resourceName, got %v", rule.ResourceNames)
	}
	ds := &appsv1.DaemonSet{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-node", Namespace: "ngxstorage-nfs"}, ds)
	if _, pinned := ds.Spec.Template.Spec.NodeSelector["node-role.kubernetes.io/worker"]; !pinned {
		t.Fatal("OCP node DaemonSet must be worker-pinned")
	}
	if ds.Spec.Template.Annotations["openshift.io/required-scc"] != "nfs-csi-ngxstorage-node-scc" {
		t.Fatal("OCP node DaemonSet must carry required-scc")
	}
}

// TestKubernetesNeverRendersSCC: on plain Kubernetes there is
// no SCC, no scc-use RBAC, no worker pin.
func TestKubernetesNeverRendersSCC(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	role := &rbacv1.Role{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-node-scc-use", Namespace: "ngxstorage-nfs"}, role); !apierrors.IsNotFound(err) {
		t.Fatalf("k8s must not create scc-use Role (err=%v)", err)
	}
	ds := &appsv1.DaemonSet{}
	owned(t, te.client, types.NamespacedName{Name: "nfs-csi-ngxstorage-node", Namespace: "ngxstorage-nfs"}, ds)
	if _, pinned := ds.Spec.Template.Spec.NodeSelector["node-role.kubernetes.io/worker"]; pinned {
		t.Fatal("k8s node DaemonSet must not be worker-pinned")
	}
	if _, has := ds.Spec.Template.Annotations["openshift.io/required-scc"]; has {
		t.Fatal("k8s node DaemonSet must not carry required-scc")
	}
}

// -------------------------------------------------------------------
// Partial apply + 403/409/404 classification
// -------------------------------------------------------------------

// blockOn blocks both Create and Patch for the target kind. mergeApplier uses
// Create on first pass and Patch (merge) on steady state, so a classification
// test must block both verbs or the first reconcile would create everything
// and never hit the injected error.
func blockOn(isTarget func(client.Object) bool, errFor func(obj client.Object) error) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if isTarget(obj) {
				return errFor(obj)
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if isTarget(obj) {
				return errFor(obj)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestPartialApplyForbidden: a 403 mid-set stops the
// profile with PermissionDenied; earlier objects stay applied, later ones are
// absent; the reconcile requeues (retryable). The 403 targets the controller
// Deployment — a RUNTIME operand (the RBAC kinds are
// installer-owned out-of-band and never applied at runtime, so a mid-set RBAC
// 403 is not a shape the reconciler can hit).
func TestPartialApplyForbidden(t *testing.T) {
	te := newInterceptedEnv(t, blockOn(
		func(o client.Object) bool { _, ok := o.(*appsv1.Deployment); return ok },
		func(obj client.Object) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("denied by test policy"))
		},
	), validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("403 must requeue after %s, got %+v", retryableRequeueAfter, res)
	}
	q := te.get(t, v1alpha1.QuartetName)
	c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonPermissionDenied {
		t.Fatalf("aggregate reason = %q, want PermissionDenied", c.Reason)
	}
	sa := &corev1.ServiceAccount{}
	if err := te.client.Get(context.Background(),
		types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: "ngxstorage-nfs"}, sa); err != nil {
		t.Fatalf("partial apply: earlier ServiceAccount must remain, got %v", err)
	}
	if getDeployment(t, te.client) != nil {
		t.Fatal("partial apply: later Deployment must be absent after the 403")
	}
}

// TestApplyConflict409: an apply conflict is classified ApplyConflict
// and requeued.
func TestApplyConflict409(t *testing.T) {
	funcs := blockOn(
		func(o client.Object) bool { _, ok := o.(*appsv1.Deployment); return ok },
		func(obj client.Object) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("conflicting update"))
		},
	)
	te := newInterceptedEnv(t, funcs, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("409 must requeue, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonApplyConflict {
		t.Fatalf("aggregate reason = %q, want ApplyConflict", c.Reason)
	}
}

// TestNamespaceCreate404: a namespace create that 404s (API not
// served) is NamespaceMissing.
func TestNamespaceCreate404(t *testing.T) {
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isNS := obj.(*corev1.Namespace); isNS {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, obj.GetName())
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	te := newInterceptedEnv(t, funcs, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("namespace failure must requeue, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonNamespaceMissing {
		t.Fatalf("aggregate reason = %q, want NamespaceMissing", c.Reason)
	}
}

// TestOperandApply404: an operand apply that 404s (resource absent
// from the API) is DependencyMissing, retryable.
func TestOperandApply404(t *testing.T) {
	funcs := blockOn(
		func(o client.Object) bool { _, ok := o.(*storagev1.CSIDriver); return ok },
		func(obj client.Object) error {
			return apierrors.NewNotFound(schema.GroupResource{Group: "storage.k8s.io", Resource: "csidrivers"}, obj.GetName())
		},
	)
	te := newInterceptedEnv(t, funcs, validCR())
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("404 must requeue, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonDependencyMissing {
		t.Fatalf("aggregate reason = %q, want DependencyMissing", c.Reason)
	}
}

// TestRedactionStatusEvents: a Secret value placed in the
// operand namespace never appears in status conditions or events (the
// operator never reads Secrets; names only).
func TestRedactionStatusEvents(t *testing.T) {
	sentinel := "SUPER_SECRET_VALUE_NEVER_LOGGED"
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName) // creates the namespace first
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-config", Namespace: "ngxstorage-nfs"},
		Data:       map[string][]byte{"storage-ips": []byte(sentinel)},
	}
	if err := te.client.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	for _, c := range q.Status.Conditions {
		if strings.Contains(c.Message, sentinel) || strings.Contains(c.Reason, sentinel) {
			t.Fatalf("redaction violated in aggregate status: %v", c)
		}
	}
	for prof, ps := range q.Status.Profiles {
		for _, c := range ps.Conditions {
			if strings.Contains(c.Message, sentinel) {
				t.Fatalf("redaction violated in profile %s status: %v", prof, c)
			}
		}
	}
	for _, ev := range drainEvents(te.recorder) {
		if strings.Contains(ev, sentinel) {
			t.Fatalf("redaction violated in event: %s", ev)
		}
	}
}

// -------------------------------------------------------------------
// Status transitions + profile isolation
// -------------------------------------------------------------------

// captureProfileOwnedState serializes ONE profile's owned operand state for
// isolation comparisons: every namespaced operand kind in the profile's
// operand namespace, the profile's OWN CSIDriver (cluster-scoped —
// matched by name, the name the renderer emits for its provisioner
// identity), and the namespace object. Installer-owned RBAC is excluded
// (never operator-written), and another profile's cluster-scoped operands
// are out of scope by construction, which is the isolation the comparison
// checks. captureOwnedState (namespace-scoped + cluster-wide RBAC) stays
// for the nfs-only no-op tests; cross-profile tests use this.
func captureProfileOwnedState(t *testing.T, c client.Client, profile string) string {
	t.Helper()
	driverName, _ := renderer.ProvisionerIdentity(profile)
	if driverName == "" {
		t.Fatalf("profile %s contributes no provisioner identity — cannot scope its owned state", profile)
	}
	ns := renderer.OperandNamespace(profile)
	var sb strings.Builder
	kinds := []client.Object{
		&corev1.ConfigMap{}, &coordinationv1.Lease{}, &corev1.Service{},
		&appsv1.Deployment{}, &appsv1.DaemonSet{}, &rbacv1.Role{}, &rbacv1.RoleBinding{},
		&corev1.ServiceAccount{},
	}
	for _, k := range kinds {
		list := listFor(t, k)
		if err := c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
			t.Fatalf("list %T: %v", k, err)
		}
		sb.Write(stripServerMeta(t, list))
	}
	cd := &storagev1.CSIDriver{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: driverName}, cd); err == nil {
		b, _ := json.Marshal(cd)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if md, _ := m["metadata"].(map[string]any); md != nil {
			for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields"} {
				delete(md, k)
			}
		}
		b2, _ := json.Marshal(m)
		sb.Write(b2)
	}
	if nsobj := getNamespace(c, ns); nsobj != nil {
		b, _ := json.Marshal(nsobj)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if md, _ := m["metadata"].(map[string]any); md != nil {
			for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "managedFields"} {
				delete(md, k)
			}
		}
		b2, _ := json.Marshal(m)
		sb.Write(b2)
	}
	return sb.String()
}

// TestEnabledFCProfileRendersIsolated: an enabled fc profile renders
// behind the seeded installer-owned RBAC set. It appears in
// status.profiles with its OWN condition — the fake env gates it
// WorkloadNotReady (no kubelet), never RenderError (the renderer exists) —
// the aggregate HONESTLY surfaces the blocked profile, and NFS stays Ready.
// The RenderError negative for a profile the renderer cannot serve is
// TestProfileContractUnknownProfile (invalid-profile dispatch).
func TestEnabledFCProfileRendersIsolated(t *testing.T) {
	te := newTestEnv(t, validCRWithFC())
	convergeReadiness(t, te)
	q := te.get(t, v1alpha1.QuartetName)

	fc := q.Status.Profiles[v1alpha1.ProfileFC]
	if c := condition(t, fc.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason == v1alpha1.ReasonRenderError {
		t.Fatalf("fc Ready = (%v %q) — a wired renderer profile gates on its own surface (WorkloadNotReady in the fake env), never RenderError", c.Status, c.Reason)
	}
	if c := condition(t, fc.Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionTrue {
		t.Fatal("fc Degraded must be True while it is gated")
	}
	// fc rendered its operands behind the seeded install set.
	fcDriver, _ := renderer.ProvisionerIdentity(v1alpha1.ProfileFC)
	cd := &storagev1.CSIDriver{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: fcDriver}, cd); err != nil {
		t.Fatalf("enabled fc must create its CSIDriver behind a healthy install set: %v", err)
	}
	if getNamespace(te.client, renderer.OperandNamespace(v1alpha1.ProfileFC)) == nil {
		t.Fatal("enabled fc must ensure its operand namespace")
	}

	nfs := q.Status.Profiles[v1alpha1.ProfileNFS]
	if c := condition(t, nfs.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("nfs must stay Ready despite fc being gated: (%v %q)", c.Status, c.Reason)
	}
	agg := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if agg.Status != metav1.ConditionFalse || agg.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("aggregate = (%v %q), want (False WorkloadNotReady) — the gated profile's own reason", agg.Status, agg.Reason)
	}
	if !strings.Contains(agg.Message, v1alpha1.ProfileFC) {
		t.Fatalf("aggregate must name the failed profile honestly: %s", agg.Message)
	}
}

// TestProfileIsolationNoCrossRewrite: enabling a second
// profile (fc) must not rewrite the healthy NFS owned state — and must
// render ONLY fc's own operands in fc's own namespace. The capture is
// profile-scoped (captureProfileOwnedState): fc creating its own
// cluster-scoped CSIDriver is composition, not a cross-rewrite.
func TestProfileIsolationNoCrossRewrite(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	before := captureProfileOwnedState(t, te.client, v1alpha1.ProfileNFS)

	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.Profiles.FC = validCRWithFC().Spec.Profiles.FC
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)

	after := captureProfileOwnedState(t, te.client, v1alpha1.ProfileNFS)
	if before != after {
		t.Fatal("profile isolation violated: enabling fc rewrote NFS-owned objects")
	}
	// Honest composition: fc's OWN state materialized in fc's own namespace.
	fcDriver, _ := renderer.ProvisionerIdentity(v1alpha1.ProfileFC)
	cd := &storagev1.CSIDriver{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: fcDriver}, cd); err != nil {
		t.Fatalf("enabling fc must create fc's own CSIDriver: %v", err)
	}
}

// TestObservedGenerationAdvances: a spec update advances
// status.observedGeneration once reconciled.
func TestObservedGenerationAdvances(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.LogLevel = v1alpha1.LogLevelDebug
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)
	got := te.get(t, v1alpha1.QuartetName)
	if got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("observedGeneration=%d generation=%d after spec change", got.Status.ObservedGeneration, got.Generation)
	}
	// The rendered --log arg follows the spec, unquoted.
	d := getDeployment(t, te.client)
	found := false
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "nfs-csi-ngxstorage" {
			continue
		}
		for _, a := range c.Args {
			if a == "--log=debug" {
				found = true
			} else if strings.HasPrefix(a, "--log") {
				t.Fatalf("unexpected --log arg %q", a)
			}
		}
	}
	if !found {
		t.Fatal("driver args must carry --log=debug after spec change")
	}
}

// forbiddenOnDeployment 403s the controller Deployment's create/patch — a
// stable mid-set failure for classification/event tests (the RBAC kinds are
// not runtime-applied, so the 403 targets a workload).
func forbiddenOnDeployment() interceptor.Funcs {
	return blockOn(
		func(o client.Object) bool { _, ok := o.(*appsv1.Deployment); return ok },
		func(obj client.Object) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("denied by test policy"))
		},
	)
}

// TestEventDedupeProfileFailure: repeated reconciles with
// the same per-profile failure emit one warning event per profile signature.
func TestEventDedupeProfileFailure(t *testing.T) {
	te := newInterceptedEnv(t, forbiddenOnDeployment(), validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName)
	te.reconcile(t, v1alpha1.QuartetName)
	events := drainEvents(te.recorder)
	warn := 0
	for _, ev := range events {
		if strings.Contains(ev, corev1.EventTypeWarning) {
			warn++
		}
	}
	// aggregate Ready transition + profile nfs Degraded + profile nfs Ready
	// failure = 3 distinct signatures; nothing repeats.
	if warn != 3 {
		t.Fatalf("expected 3 distinct warning events, got %d: %v", warn, events)
	}
}

// -------------------------------------------------------------------
// Disable / delete / data retention / finalizer replay
// -------------------------------------------------------------------

// TestDisableRemovesOperandsRetainsData: nfs.enabled=false
// removes every NFS operand but keeps the namespace, customer
// Secret, PV and PVC.
func TestDisableRemovesOperandsRetainsData(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	ns := "ngxstorage-nfs"
	customerSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-config", Namespace: ns}}
	customerPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-pvc", Namespace: ns}}
	customerPV := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "data-pv"}}
	for _, o := range []client.Object{customerSecret, customerPVC, customerPV} {
		if err := te.client.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}

	q := te.get(t, v1alpha1.QuartetName)
	q.Spec.Profiles.NFS.Enabled = false
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("disable must not requeue, got %+v", res)
	}

	if getDeployment(t, te.client) != nil {
		t.Fatal("disable must remove the controller Deployment")
	}
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("disable must remove the CSIDriver")
	}
	if countOwnedLeases(t, te.client, string(q.UID)) != 0 {
		t.Fatal("disable must remove the owned Lease pool")
	}
	if getNamespace(te.client, ns) == nil {
		t.Fatal("disable must NOT remove the operand namespace")
	}
	for _, o := range []client.Object{customerSecret, customerPVC, customerPV} {
		got := o.DeepCopyObject().(client.Object)
		if err := te.client.Get(context.Background(), client.ObjectKeyFromObject(o), got); err != nil {
			t.Fatalf("disable removed customer data %T: %v", o, err)
		}
	}
	gotStatus := te.get(t, v1alpha1.QuartetName).Status
	if _, present := gotStatus.Profiles[v1alpha1.ProfileNFS]; present {
		t.Fatal("disabled profile must make no status claim")
	}
}

// TestDeleteFinalizerCleanup: deleting the CR removes owned
// operands and the finalizer, but keeps customer data and the namespace.
func TestDeleteFinalizerCleanup(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	ns := "ngxstorage-nfs"
	customerSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-config", Namespace: ns}}
	if err := te.client.Create(context.Background(), customerSecret); err != nil {
		t.Fatal(err)
	}

	q := te.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(q, quartetFinalizer) {
		t.Fatal("reconcile must have added the finalizer before applying operands")
	}
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName) // sees deletionTimestamp
	if res.RequeueAfter != 0 {
		t.Fatalf("successful cleanup must not requeue, got %+v", res)
	}
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("delete must remove the owned CSIDriver")
	}
	if getDeployment(t, te.client) != nil {
		t.Fatal("delete must remove the owned Deployment")
	}
	if getNamespace(te.client, ns) == nil {
		t.Fatal("delete must NOT remove the operand namespace")
	}
	got := &corev1.Secret{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-config", Namespace: ns}, got); err != nil {
		t.Fatalf("delete removed the customer Secret: %v", err)
	}
	// The CR is gone (finalizer removed → fake client reaps it).
	c := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, c); !apierrors.IsNotFound(err) {
		t.Fatalf("CR must be deleted after finalizer removal (err=%v)", err)
	}
}

// TestFinalizerReplayCleanupFailure: a cleanup error
// KEEPS the finalizer, reports CleanupFailed, requeues; once the error
// clears, replay completes the deletion.
func TestFinalizerReplayCleanupFailure(t *testing.T) {
	fail := true
	funcs := interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if fail && obj.GetName() != v1alpha1.QuartetName {
				return apierrors.NewServerTimeout(schema.GroupResource{Resource: "deployments"}, "delete", 1)
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
	te := newInterceptedEnv(t, funcs, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("cleanup failure must requeue, got %+v", res)
	}
	stuck := te.get(t, v1alpha1.QuartetName)
	if !containsFinalizer(stuck, quartetFinalizer) {
		t.Fatal("cleanup failure must KEEP the finalizer")
	}
	c := condition(t, stuck.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonCleanupFailed {
		t.Fatalf("reason = %q, want CleanupFailed", c.Reason)
	}
	if getCSIDriver(t, te.client) == nil {
		t.Fatal("failed cleanup must not have removed operands (or ordering wrong)")
	}

	fail = false
	res = te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("replay must complete, got %+v", res)
	}
	if getCSIDriver(t, te.client) != nil {
		t.Fatal("replay must remove the CSIDriver")
	}
	got := &v1alpha1.Quartet{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, got); !apierrors.IsNotFound(err) {
		t.Fatalf("CR must be gone after replay (err=%v)", err)
	}
}

// TestCleanupSkipsForeignUID: a CSIDriver carrying a
// DIFFERENT owner UID is foreign — cleanup skips it.
func TestCleanupSkipsForeignUID(t *testing.T) {
	foreign := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nfs.csi.ngxstorage.com",
			Annotations: map[string]string{renderer.OwnerUIDAnnotation: "not-this-cr"},
		},
	}
	te := newInterceptedEnv(t, interceptor.Funcs{}, foreign, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	// The profile is blocked by the foreign object; delete the CR anyway —
	// cleanup must enumerate and skip the foreign UID without error.
	q := te.get(t, v1alpha1.QuartetName)
	if err := te.client.Delete(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	res := te.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("foreign UID must not block cleanup, got %+v", res)
	}
	got := &storagev1.CSIDriver{}
	owned(t, te.client, types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, got)
	if got.Annotations[renderer.OwnerUIDAnnotation] != "not-this-cr" {
		t.Fatal("cleanup touched a foreign-UID CSIDriver")
	}
}

// -------------------------------------------------------------------
// Manager restart (limited: two sequential reconciler instances
// over the same client must be idempotent — no double-create, no status
// churn, finalizer kept single).
// -------------------------------------------------------------------

func TestRestartIdempotent(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	before := captureOwnedState(t, te.client)

	// "Restart": a fresh reconciler instance (fresh recorder/cache) over the
	// SAME client, as a new manager process would see it.
	rec := record.NewFakeRecorder(100)
	te2 := &fakeEnv{
		reconciler: &QuartetReconciler{
			Client:            te.client,
			Scheme:            te.client.Scheme(),
			Log:               logf.Log.WithName("test-restarted"),
			Recorder:          rec,
			PlatformDiscovery: k8sDiscovery(),
			Applier:           mergeApplier{c: te.client},
		},
		client:   te.client,
		recorder: rec,
	}
	res := te2.reconcile(t, v1alpha1.QuartetName)
	if res.RequeueAfter != 0 {
		t.Fatalf("post-restart reconcile must be steady, got %+v", res)
	}
	if after := captureOwnedState(t, te.client); before != after {
		t.Fatal("restart reconcile mutated owned objects")
	}
	q := te2.get(t, v1alpha1.QuartetName)
	if n := countFinalizers(q, quartetFinalizer); n != 1 {
		t.Fatalf("finalizer count after restart = %d, want exactly 1", n)
	}
	c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("post-restart Ready = (%v %q)", c.Status, c.Reason)
	}
}

// --- tiny local helpers (avoid importing controllerutil in tests). ---

func validCRWithFC() *v1alpha1.Quartet {
	q := validCR()
	q.Spec.Profiles.FC = v1alpha1.ProfileSpec{
		Enabled:   true,
		SecretRef: &v1alpha1.SecretRef{Name: "fc-csi-ngxstorage-config"},
		Image:     &v1alpha1.ImageSpec{Reference: "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:" + strings.Repeat("d", 64)},
	}
	return q
}

func containsFinalizer(q *v1alpha1.Quartet, f string) bool {
	for _, x := range q.Finalizers {
		if x == f {
			return true
		}
	}
	return false
}

func countFinalizers(q *v1alpha1.Quartet, f string) int {
	n := 0
	for _, x := range q.Finalizers {
		if x == f {
			n++
		}
	}
	return n
}

// The envtest suite exercises the PRODUCTION apply path (real server-side
// apply, field manager quartet-operator) against a real API server. envtest
// cannot simulate OpenShift API groups or a kubelet, so OCP render
// and workload readiness stay with the fake-client and live-cluster layers.

const envtestNFSNS = "ngxstorage-nfs"

// pollObj waits until the predicate over the operand client is true.
func pollObj(t *testing.T, check func(context.Context) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, suiteTimeout, true, func(ctx context.Context) (bool, error) {
		return check(ctx), nil
	}); err != nil {
		t.Fatalf("operand condition not reached within %s: %v", suiteTimeout, err)
	}
}

func envtestDeployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: envtestNFSNS}, d); err != nil {
		return nil
	}
	return d
}

func envtestCSIDriver(t *testing.T) *storagev1.CSIDriver {
	t.Helper()
	cd := &storagev1.CSIDriver{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, cd); err != nil {
		return nil
	}
	return cd
}

// driverContainer returns the driver container by name from a Deployment —
// the SAME container the drift test mutates (mutation and assertion must
// target one and the same container, or the test passes while looking at a
// field it never touched).
func driverContainer(d *appsv1.Deployment) *corev1.Container {
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == "nfs-csi-ngxstorage" {
			return &d.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// pollWorkloadGate waits for the aggregate to settle at WorkloadNotReady:
// envtest runs no kubelet, so operand APPLY converges but workload
// READINESS can never be satisfied by a real rollout — the
// expected steady state on the fake API is exactly this condition. The
// readiness-transition tests then simulate the kubelet by patching workload
// status; live readiness requires a real cluster.
func pollWorkloadGate(t *testing.T) {
	t.Helper()
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("aggregate reason = %q (message: %s), want WorkloadNotReady", c.Reason, c.Message)
	}
}

// setDeploymentReady simulates the kubelet/Deployment-controller converging
// the controller Deployment (observed generation + full replica rollout).
func setDeploymentReady(t *testing.T) {
	t.Helper()
	d := envtestDeployment(t)
	if d == nil {
		t.Fatal("Deployment missing for status simulation")
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.AvailableReplicas = 1
	d.Status.ReadyReplicas = 1
	if err := testClient.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("set Deployment status: %v", err)
	}
}

// setDaemonSetReady simulates the node DaemonSet converging on one target
// node (observed generation + full schedule).
func setDaemonSetReady(t *testing.T) {
	t.Helper()
	ds := &appsv1.DaemonSet{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "nfs-csi-ngxstorage-node", Namespace: envtestNFSNS}, ds); err != nil {
		t.Fatalf("DaemonSet missing for status simulation: %v", err)
	}
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = 1
	ds.Status.UpdatedNumberScheduled = 1
	ds.Status.NumberAvailable = 1
	if err := testClient.Status().Update(context.Background(), ds); err != nil {
		t.Fatalf("set DaemonSet status: %v", err)
	}
}

// convergeWorkloads is the combined simulation used by the drift/idempotency
// tests that need a full Ready steady state.
func convergeWorkloads(t *testing.T) {
	t.Helper()
	setDeploymentReady(t)
	setDaemonSetReady(t)
}

// TestEnvtestNFSOperandsApplied (on the real API): a valid singleton
// renders the full 141-object NFS set; the runtime SSA-applies the 135
// NON-RBAC objects under the field manager `quartet-operator`, every one
// owner-annotated, while the 6 installer-owned RBAC objects come from the
// committed install manifest (suite bootstrap). No StorageClass or
// VolumeSnapshotClass is created.
func TestEnvtestNFSOperandsApplied(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	// On envtest (no kubelet) the steady state after apply is
	// WorkloadNotReady — the operand set is fully materialized BY THEN.
	pollWorkloadGate(t)

	ctx := context.Background()
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	uid := string(q.UID)

	// Namespace.
	ns := &corev1.Namespace{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: envtestNFSNS}, ns); err != nil {
		t.Fatalf("operand namespace missing: %v", err)
	}
	// CSIDriver + ownership annotation + canonical fsGroupPolicy.
	cd := envtestCSIDriver(t)
	if cd == nil {
		t.Fatal("CSIDriver missing")
	}
	if cd.Annotations[renderer.OwnerUIDAnnotation] != uid {
		t.Fatalf("CSIDriver owner annotation = %q, want CR UID %q", cd.Annotations[renderer.OwnerUIDAnnotation], uid)
	}
	if cd.Spec.FSGroupPolicy == nil || *cd.Spec.FSGroupPolicy != storagev1.NoneFSGroupPolicy {
		t.Fatalf("CSIDriver fsGroupPolicy = %v, want None", cd.Spec.FSGroupPolicy)
	}
	// 128 owned leases.
	leases := &coordinationv1.LeaseList{}
	if err := testClient.List(ctx, leases, client.InNamespace(envtestNFSNS)); err != nil {
		t.Fatal(err)
	}
	owned := 0
	for i := range leases.Items {
		if leases.Items[i].Annotations[renderer.OwnerUIDAnnotation] == uid {
			owned++
		}
	}
	if owned != 128 {
		t.Fatalf("owned leases = %d, want 128", owned)
	}
	// ClusterRole: 12 rules, no volumeattributesclasses access.
	cr := &rbacv1.ClusterRole{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, cr); err != nil {
		t.Fatal(err)
	}
	if len(cr.Rules) != 12 {
		t.Fatalf("ClusterRole rules = %d, want 12 (raw 13 minus volumeattributesclasses)", len(cr.Rules))
	}
	for _, r := range cr.Rules {
		for _, res := range r.Resources {
			if res == "volumeattributesclasses" {
				t.Fatal("volumeattributesclasses rule must not be present")
			}
		}
	}
	// Deployment: the SSA field manager is quartet-operator.
	du := &unstructured.Unstructured{}
	du.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	if err := testClient.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller", Namespace: envtestNFSNS}, du); err != nil {
		t.Fatal(err)
	}
	mf, _, _ := unstructured.NestedSlice(du.Object, "metadata", "managedFields")
	sawManager := false
	for _, e := range mf {
		m, _ := e.(map[string]interface{})
		if m["manager"] == renderer.FieldManager {
			sawManager = true
		}
	}
	if !sawManager {
		t.Fatalf("Deployment managedFields lacks field manager %q: %v", renderer.FieldManager, mf)
	}
	// No StorageClass / VolumeSnapshotClass anywhere.
	for _, bad := range []schema.GroupVersionKind{
		{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"},
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"},
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(bad)
		err := testClient.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage"}, u)
		// NotFound = absent; NoMatch = the snapshot API itself is not served
		// by envtest, which also proves the object cannot exist.
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			t.Fatalf("%s must not exist (err=%v)", bad.Kind, err)
		}
	}
}

// TestEnvtestRBACDriftGatesOperandApply (branch (e) on
// the main plane — where the manager runs): the suite bootstrap applied the
// COMMITTED install manifest with admin identity; the reconciler verifies
// it healthy and proceeds to operand apply (the Ready path is only gated by
// the workload probe — envtest has no kubelet). When the install set drifts
// (admin removes an object, as uninstall/tampering would), a DIRECT
// reconcile call — no watch event, the RBAC kinds are not watched — gates
// the profile behind Degraded/RBACDrift, leaves every operand byte-stable,
// and requeues; when the admin re-applies the install manifest, the next
// reconcile passes the gate and operand apply resumes.
func TestEnvtestRBACDriftGatesOperandApply(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	// Healthy half: the committed install set verifies and operands apply —
	// the steady state is the workload gate, NOT RBACDrift.
	pollWorkloadGate(t)

	ctx := context.Background()
	depBefore := envtestDeployment(t)
	if depBefore == nil {
		t.Fatal("Deployment missing after healthy gate")
	}
	rvBefore := depBefore.ResourceVersion

	// Drift: admin removes the install ClusterRole.
	if err := testClient.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"}}); err != nil {
		t.Fatalf("remove install ClusterRole: %v", err)
	}
	if _, err := testReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}}); err != nil {
		t.Fatalf("direct reconcile after drift: %v", err)
	}

	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		for _, c := range q.Status.Conditions {
			if c.Type == v1alpha1.ConditionReady && c.Reason == v1alpha1.ReasonRBACDrift {
				return true
			}
		}
		return false
	})
	// The gate is BEFORE operand writes: nothing modified.
	depAfter := envtestDeployment(t)
	if depAfter == nil || depAfter.ResourceVersion != rvBefore {
		t.Fatalf("RBACDrift gate must not modify operands (resourceVersion %s -> %v)", rvBefore, envtestDeployment(t))
	}
	// Retryable: a requeue drives the re-verification once the admin fixes
	// the install.
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if c := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonRBACDrift {
		t.Fatalf("profile Degraded = (%v %q), want (True RBACDrift)", c.Status, c.Reason)
	}

	// Recovery: admin re-applies the install manifest; the next reconcile
	// passes the gate and operand apply resumes (workload gate again).
	if err := applyCommittedInstallSet(testClient, v1alpha1.ProfileNFS); err != nil {
		t.Fatalf("re-apply install manifest: %v", err)
	}
	if _, err := testReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.QuartetName}}); err != nil {
		t.Fatalf("direct reconcile after re-install: %v", err)
	}
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		for _, c := range q.Status.Conditions {
			if c.Type == v1alpha1.ConditionReady && c.Reason == v1alpha1.ReasonWorkloadNotReady {
				return true
			}
		}
		return false
	})
}

// TestEnvtestSpecImageAndLogRendered (on the real API): the CRD
// accepts a full NFS spec (digest image, debug logLevel) and the reconciler
// renders exactly that — driver image from spec, --log unquoted.
func TestEnvtestSpecImageAndLogRendered(t *testing.T) {
	requireEnvtest(t)
	specImage := "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("f", 64)
	ensureSingleton(t, func(q *v1alpha1.Quartet) {
		q.Spec.LogLevel = v1alpha1.LogLevelDebug
		q.Spec.Profiles.NFS.Image.Reference = specImage
	})
	pollWorkloadGate(t)
	pollObj(t, func(ctx context.Context) bool {
		d := envtestDeployment(t)
		if d == nil {
			return false
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name != "nfs-csi-ngxstorage" {
				continue
			}
			if c.Image != specImage {
				return false
			}
			for _, a := range c.Args {
				if a == "--log=debug" {
					return true
				}
			}
		}
		return false
	})
}

// TestEnvtestLogLevelEnum (schema validation on the real API): the CRD
// accepts every canonical logLevel value and rejects unknown ones.
func TestEnvtestLogLevelEnum(t *testing.T) {
	requireEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	for _, lvl := range []v1alpha1.LogLevel{"debug", "info", "warn", "error"} {
		cr := validCR()
		cr.Name = "quartet-lvl-" + string(lvl)
		cr.Spec.LogLevel = lvl
		if err := testClient.Create(ctx, cr); err != nil {
			t.Fatalf("logLevel %q must be API-accepted: %v", lvl, err)
		}
		if err := testClient.Delete(ctx, cr); err != nil {
			t.Fatalf("cleanup logLevel CR %q: %v", lvl, err)
		}
	}

	bad := validCR()
	bad.Name = "quartet-lvl-bad"
	bad.Spec.LogLevel = v1alpha1.LogLevel("trace")
	if err := testClient.Create(ctx, bad); err == nil {
		t.Fatal("logLevel enum violation must be rejected by the API server")
	}
}

// TestEnvtestDeletedOwnedChildRecreated (drift/delete on the real API):
// deleting an owned child is observed by the operand watch and recreated —
// no spec change required.
func TestEnvtestDeletedOwnedChildRecreated(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	if err := testClient.Delete(context.Background(), envtestCSIDriver(t)); err != nil {
		t.Fatal(err)
	}
	pollObj(t, func(ctx context.Context) bool { return envtestCSIDriver(t) != nil })
}

// TestEnvtestExternalDriftNoForce (no-force contract
// on the real API): the SSA path repairs drift only while field ownership is
// intact, and NEVER reclaims stolen fields:
//
// 1. a third party ADDS a foreign annotation via plain Update — a field
// path the operator's SSA does not own. The next reconcile applies
// without conflict: our fields stay desired, the foreign annotation
// SURVIVES (untouched) and no conflict surfaces;
// 2. a third party STEALS an operator-owned field via plain Update (the
// driver image). The Update reassigns that path's field manager, so the
// next no-force SSA conflicts. The bounded retry (the field-manager
// verification) re-applies on fresh state but EXHAUSTS — a stolen field
// cannot converge without force — so the profile settles at TERMINAL
// ForeignResourceConflict (the no-force field-ownership branch)
// and the stolen image is NOT force-repaired. Reconciling the steal is
// an admin decision. Contrast leg 1 / the benign-concurrency
// cases: a plain RV conflict converges within the bound.
func TestEnvtestExternalDriftNoForce(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)

	d := envtestDeployment(t)
	c := driverContainer(d)
	if c == nil {
		t.Fatal("driver container nfs-csi-ngxstorage not found in rendered Deployment")
	}
	if c.Image != testImageRef {
		t.Fatalf("precondition: driver image = %q, want %q", c.Image, testImageRef)
	}

	// (1) foreign annotation via plain Update: survives the no-force apply.
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations["external.example/added-by-user"] = "keep-me"
	if err := testClient.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	profileForeignConflict := func() bool {
		cr := &v1alpha1.Quartet{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, cr); err != nil {
			return false
		}
		for _, cond := range cr.Status.Profiles[v1alpha1.ProfileNFS].Conditions {
			if cond.Type == v1alpha1.ConditionReady && cond.Reason == v1alpha1.ReasonForeignResourceConflict {
				return true
			}
		}
		return false
	}
	pollObj(t, func(ctx context.Context) bool {
		got := envtestDeployment(t)
		if got == nil || got.Annotations["external.example/added-by-user"] != "keep-me" {
			return false
		}
		gc := driverContainer(got)
		return gc != nil && gc.Image == testImageRef && !profileForeignConflict()
	})
	t.Log("leg 1: annotation-only drift repaired under no-force; foreign field preserved")

	// (2) steal an operator-owned field via plain Update.
	evil := "evil.example/attacker@sha256:" + strings.Repeat("e", 64)
	got := envtestDeployment(t)
	if gc := driverContainer(got); gc != nil {
		gc.Image = evil
		if err := testClient.Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	// The object is still ours (UID+annotation intact), but the stolen field
	// is owned by another manager, so the bounded retry EXHAUSTS (the field-
	// manager verification fails to converge) and the profile settles at
	// TERMINAL ForeignResourceConflict — the no-force field-ownership branch.
	// The stolen value is NOT force-repaired; reconciling is an admin call.
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		for _, cond := range q.Status.Conditions {
			if cond.Type == v1alpha1.ConditionReady && cond.Reason == v1alpha1.ReasonForeignResourceConflict {
				return true
			}
		}
		return false
	})
	pollObj(t, func(ctx context.Context) bool {
		g := envtestDeployment(t)
		c := driverContainer(g)
		return g != nil && c != nil && c.Image == evil
	})
}

// TestEnvtestIdempotentSteadyState (idempotency on the real API): in
// steady state neither the operand (resourceVersion) nor the status churns.
func TestEnvtestIdempotentSteadyState(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	time.Sleep(1500 * time.Millisecond) // let any in-flight watch-triggered reconciles settle
	d := envtestDeployment(t)
	r1 := d.ResourceVersion
	s1 := &v1alpha1.Quartet{}
	_ = testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, s1)
	status1 := s1.Status

	time.Sleep(3 * time.Second)
	d2 := envtestDeployment(t)
	if d2.ResourceVersion != r1 {
		t.Fatalf("steady state churned the Deployment (resourceVersion %s -> %s)", r1, d2.ResourceVersion)
	}
	s2 := &v1alpha1.Quartet{}
	_ = testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, s2)
	if got := s2.Status; got.ObservedGeneration != status1.ObservedGeneration ||
		len(got.Conditions) != len(status1.Conditions) {
		t.Fatalf("steady state churned the CR status: %v -> %v", status1, got)
	}
}

// TestEnvtestForeignConflict (foreign conflict on the real API): a
// user-owned CSIDriver of the same name blocks the profile with
// ForeignResourceConflict and is left byte-for-byte untouched.
func TestEnvtestForeignConflict(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	// Remove any previous singleton BEFORE seeding the foreign object:
	// ensureSingleton would create the singleton first, and the manager
	// reconciles it immediately — the operator would own the CSIDriver
	// before the foreign one exists.
	existing := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, existing); err == nil {
		if err := testClient.Delete(ctx, existing); err != nil {
			t.Fatalf("delete previous singleton: %v", err)
		}
		waitForDeletion(t, ctx)
	}

	foreignCSIDriver := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs.csi.ngxstorage.com", Labels: map[string]string{"managed-by": "user"}},
		Spec:       storagev1.CSIDriverSpec{PodInfoOnMount: ptrBool(true)},
	}
	if err := testClient.Create(ctx, foreignCSIDriver); err != nil {
		t.Fatalf("seed foreign CSIDriver: %v", err)
	}
	defer func() {
		// Test artifact cleanup so later singleton tests see a pristine API.
		if err := testClient.Delete(context.Background(), foreignCSIDriver); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("cleanup foreign CSIDriver: %v", err)
		}
	}()

	if err := testClient.Create(ctx, validCR()); err != nil {
		t.Fatalf("create singleton: %v", err)
	}
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonForeignResourceConflict {
		t.Fatalf("aggregate reason = %q, want ForeignResourceConflict", c.Reason)
	}
	got := &storagev1.CSIDriver{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "nfs.csi.ngxstorage.com"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["managed-by"] != "user" {
		t.Fatal("foreign CSIDriver was modified (no adoption)")
	}
	if _, has := got.Annotations[renderer.OwnerUIDAnnotation]; has {
		t.Fatal("foreign CSIDriver gained the ownership annotation")
	}
	if got.Spec.PodInfoOnMount == nil || !*got.Spec.PodInfoOnMount {
		t.Fatal("foreign CSIDriver spec was overwritten")
	}
}

// TestEnvtestEnabledFCProfileIsolated (profile isolation on the real API):
// an enabled fc profile renders behind the COMMITTED installer-owned RBAC
// set (the suite applies every rendered profile's committed k8s set as the
// admin installer would) — fc's operands apply on the real API past the
// RBACDrift gate, fc's profile condition is its own, and the NFS profile
// stays independently gated. The invalid-profile negative at the renderer
// dispatch is TestProfileContractUnknownProfile.
func TestEnvtestEnabledFCProfileIsolated(t *testing.T) {
	requireEnvtest(t)
	// Provision the committed fc install set for THIS test only (the shared
	// suite plane is nfs-only): the RBACDrift gate must see a healthy fc
	// install before it can release fc's operands.
	if err := applyCommittedInstallSet(testClient, v1alpha1.ProfileFC); err != nil {
		t.Fatalf("provision committed fc install set: %v", err)
	}
	ensureSingleton(t, func(q *v1alpha1.Quartet) { q.Spec.Profiles.FC = fcEnabledSpec() })
	// envtest (no kubelet): nfs settles at WorkloadNotReady; fc renders its
	// operands and gates at its own workload surface. The isolation
	// assertion is that fc's condition never rewrites nfs's.
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return len(q.Status.Profiles) == 2 &&
			hasCondition(q.Status.Profiles[v1alpha1.ProfileFC].Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if c := condition(t, q.Status.Profiles[v1alpha1.ProfileFC].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason == v1alpha1.ReasonRenderError {
		t.Fatalf("fc Ready = (%v %q) — a wired profile on the committed install set never reports RenderError", c.Status, c.Reason)
	}
	// fc's operands applied past the RBACDrift gate on the real API — this
	// is the envtest proof that the COMMITTED fc install set is the
	// provisioning surface (parity-pinned by TestInstallRBACParity).
	fcDriver, _ := renderer.ProvisionerIdentity(v1alpha1.ProfileFC)
	cd := &storagev1.CSIDriver{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: fcDriver}, cd); err != nil {
		t.Fatalf("enabled fc must create its CSIDriver behind the committed install set: %v", err)
	}
	if c := condition(t, q.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("nfs must stay independently gated (WorkloadNotReady) alongside fc: (%v %q)", c.Status, c.Reason)
	}
}

// TestEnvtestDisableRemovesRetainsData (disable on the real API):
// nfs.enabled=false removes every operand, keeps the namespace and the
// customer Secret.
func TestEnvtestDisableRemovesRetainsData(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	ctx := context.Background()
	// A distinct customer secret name (earlier retention tests may have left
	// the contract-named one behind by design).
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "disable-retention-secret", Namespace: envtestNFSNS}}
	if err := testClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	q := &v1alpha1.Quartet{}
	_ = testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q)
	q.Spec.Profiles.NFS.Enabled = false
	if err := testClient.Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	pollObj(t, func(ctx context.Context) bool {
		return envtestDeployment(t) == nil && envtestCSIDriver(t) == nil
	})
	ns := &corev1.Namespace{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: envtestNFSNS}, ns); err != nil {
		t.Fatalf("disable removed the namespace: %v", err)
	}
	got := &corev1.Secret{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "disable-retention-secret", Namespace: envtestNFSNS}, got); err != nil {
		t.Fatalf("disable removed the customer Secret: %v", err)
	}
}

// TestEnvtestDeleteRetention (delete on the real API): deleting the CR
// runs finalizer cleanup — owned operands gone, namespace/customer data kept,
// CR reaped.
func TestEnvtestDeleteRetention(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	ctx := context.Background()
	// Distinct customer object names (earlier retention tests may have left
	// same-named objects behind by design).
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "delete-retention-secret", Namespace: envtestNFSNS}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "delete-data-pvc", Namespace: envtestNFSNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "delete-data-pv"},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/quartet-test/delete-data-pv"},
			},
		},
	}
	for _, o := range []client.Object{secret, pvc, pv} {
		if err := testClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctx, q); err != nil {
		t.Fatal(err)
	}
	waitForDeletion(t, ctx)

	pollObj(t, func(ctx context.Context) bool { return envtestDeployment(t) == nil && envtestCSIDriver(t) == nil })
	ns := &corev1.Namespace{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: envtestNFSNS}, ns); err != nil {
		t.Fatalf("delete removed the namespace: %v", err)
	}
	pvcGot := &corev1.PersistentVolumeClaim{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "delete-data-pvc", Namespace: envtestNFSNS}, pvcGot); err != nil {
		t.Fatalf("delete removed customer PVC: %v", err)
	}
	pvGot := &corev1.PersistentVolume{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "delete-data-pv"}, pvGot); err != nil {
		t.Fatalf("delete removed customer PV: %v", err)
	}
	got := &corev1.Secret{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "delete-retention-secret", Namespace: envtestNFSNS}, got); err != nil {
		t.Fatalf("delete removed the customer Secret: %v", err)
	}
}

// TestEnvtestWorkloadReadinessTransitions (on the real API,
// SIMULATED kubelet): operand apply settles at WorkloadNotReady; patching
// the workload statuses to converged moves the profile to Ready; breaking
// the rollout again moves it back. envtest has no kubelet, so this proves
// the API behavior and the transition logic — real readiness needs a live
// cluster (pod Ready is not backend I/O proof).
func TestEnvtestWorkloadReadinessTransitions(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)

	convergeWorkloads(t)
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	})

	// Rollback: the Deployment controller regresses the rollout — the gate
	// must close again.
	d := envtestDeployment(t)
	d.Status.AvailableReplicas = 0
	if err := testClient.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady); c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("regressed rollout reason = %q, want WorkloadNotReady", c.Reason)
	}
}

// TestEnvtestPodWaitingImagePullFailed: a pod in the operand
// namespace stuck in ImagePullBackOff is surfaced as WorkloadNotReady with
// reason ImagePullFailed; recovery of the pod returns the gate to Ready.
func TestEnvtestPodWaitingImagePullFailed(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)
	convergeWorkloads(t)
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	})

	// Owner-chain: the probe only counts
	// pods owned through the ReplicaSet→Deployment UID chain. This pod
	// mimics a real one: the rendered selector labels AND a controller
	// ownerRef to a ReplicaSet owned by the rendered Deployment — it is
	// the workload's own pod that is stuck pulling.
	d := envtestDeployment(t)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nfs-csi-ngxstorage-controller-podtest",
			Namespace: envtestNFSNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name,
				UID: d.UID, Controller: ptr.To(true),
			}},
		},
		// The probe only reads UID + ownerReferences; the spec exists so the
		// API's ReplicaSet validation (selector/template/containers) passes.
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "nfs-csi-ngxstorage"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "nfs-csi-ngxstorage"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nfs-csi-ngxstorage", Image: testImageRef}}},
			},
		},
	}
	if err := testClient.Create(context.Background(), rs); err != nil {
		t.Fatalf("create test ReplicaSet: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nfs-csi-ngxstorage-controller-000001",
			Namespace: envtestNFSNS,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "nfs-csi-ngxstorage",
				"app.kubernetes.io/component": "controller",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name,
				UID: rs.UID, Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "nfs-csi-ngxstorage",
				Image: testImageRef,
			}},
		},
	}
	if err := testClient.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "nfs-csi-ngxstorage",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
		},
	}}
	if err := testClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})
	q := &v1alpha1.Quartet{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	c := condition(t, q.Status.Conditions, v1alpha1.ConditionReady)
	if c.Reason != v1alpha1.ReasonWorkloadNotReady || !strings.Contains(c.Message, "ImagePullFailed") {
		t.Fatalf("ImagePullBackOff pod must surface WorkloadNotReady/ImagePullFailed, got (%q %q)", c.Reason, c.Message)
	}

	// Pod recovers: the gate reopens.
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "nfs-csi-ngxstorage",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if err := testClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	})
}

// TestEnvtestCleanupFinalizedOperandRetainsFinalizer (on the
// real API): a delete ACCEPT is not a deletion. An owned operand carrying a
// finalizer is not physically gone after delete; cleanup must keep the CR
// finalizer, report CleanupFailed and requeue — then succeed on replay once
// the external finalizer is released.
func TestEnvtestCleanupFinalizedOperandRetainsFinalizer(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollWorkloadGate(t)

	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-coordination-config", Namespace: envtestNFSNS}}
	got := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, got); err != nil {
		t.Fatalf("expected operand ConfigMap missing: %v", err)
	}
	got.Finalizers = append(got.Finalizers, "external.example.com/keep")
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	q := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctx, q); err != nil {
		t.Fatal(err)
	}
	// Poll for the CleanupFailed reason specifically: the pre-deletion
	// steady state (WorkloadNotReady) is also Ready=False and must not
	// satisfy the assertion early.
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		for _, c := range q.Status.Conditions {
			if c.Type == v1alpha1.ConditionReady {
				return c.Reason == v1alpha1.ReasonCleanupFailed
			}
		}
		return false
	})
	stuck := &v1alpha1.Quartet{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, stuck); err != nil {
		t.Fatalf("finalized operand must KEEP the CR alive, got %v", err)
	}
	if stuck.DeletionTimestamp.IsZero() {
		t.Fatal("CR must be terminating with the finalizer retained")
	}

	// Replay: release the external finalizer and wait for the cleanup
	// requeue (30s) to finish the deletion, so later tests see a pristine
	// API. The finalizer release is metadata-only on the ConfigMap and
	// does NOT bump the CR's spec generation — only the requeue drives it.
	got = &corev1.ConfigMap{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, got); err != nil {
		t.Fatal(err)
	}
	got.Finalizers = nil
	if err := testClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond,
		retryableRequeueAfter+15*time.Second, true, func(ctx context.Context) (bool, error) {
			q := &v1alpha1.Quartet{}
			if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
				return client.IgnoreNotFound(err) == nil, nil
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("CR not reaped after finalizer release and cleanup replay: %v", err)
	}
}

func fcEnabledSpec() v1alpha1.ProfileSpec {
	return v1alpha1.ProfileSpec{
		Enabled:   true,
		SecretRef: &v1alpha1.SecretRef{Name: "fc-csi-ngxstorage-config"},
		Image:     &v1alpha1.ImageSpec{Reference: "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:" + strings.Repeat("d", 64)},
	}
}

func ptrBool(v bool) *bool { return &v }
