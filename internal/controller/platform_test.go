package controller

import (
	"context"
	"errors"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"
)

// fakeDiscovery implements platformDiscovery for unit tests. envtest cannot
// simulate an OpenShift API group, so the OCP branch is covered here and
// exercised against the real discovery client in suite_test.go.
type fakeDiscovery struct {
	groups metav1.APIGroupList
	err    error
}

func (f fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return &f.groups, f.err
}

func k8sGroups() metav1.APIGroupList {
	return metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "v1"}, {Name: "apps"}}}
}

func ocpGroups() metav1.APIGroupList {
	g := k8sGroups()
	g.Groups = append(g.Groups, metav1.APIGroup{Name: openShiftConfigGroup})
	return g
}

func TestDetectPlatform(t *testing.T) {
	forbiddenErr := apierrors.NewForbidden(schema.GroupResource{Group: "meta.k8s.io"}, "", errors.New("RBAC denied"))
	networkErr := errors.New("connection refused")

	cases := []struct {
		name      string
		discovery fakeDiscovery
		declared  v1alpha1.Platform
		want      v1alpha1.Platform
		wantErr   error
	}{
		{
			name:      "auto on plain kubernetes",
			discovery: fakeDiscovery{groups: k8sGroups()},
			declared:  v1alpha1.PlatformAuto,
			want:      v1alpha1.PlatformKubernetes,
		},
		{
			name:      "auto on openshift",
			discovery: fakeDiscovery{groups: ocpGroups()},
			declared:  v1alpha1.PlatformAuto,
			want:      v1alpha1.PlatformOpenShift,
		},
		{
			name:      "explicit kubernetes matching discovery",
			discovery: fakeDiscovery{groups: k8sGroups()},
			declared:  v1alpha1.PlatformKubernetes,
			want:      v1alpha1.PlatformKubernetes,
		},
		{
			name:      "explicit openshift matching discovery",
			discovery: fakeDiscovery{groups: ocpGroups()},
			declared:  v1alpha1.PlatformOpenShift,
			want:      v1alpha1.PlatformOpenShift,
		},
		{
			name:      "override conflict — no silent fallback",
			discovery: fakeDiscovery{groups: k8sGroups()},
			declared:  v1alpha1.PlatformOpenShift,
			wantErr:   ErrPlatformConflict,
		},
		{
			name:      "override conflict reverse direction",
			discovery: fakeDiscovery{groups: ocpGroups()},
			declared:  v1alpha1.PlatformKubernetes,
			wantErr:   ErrPlatformConflict,
		},
		{
			name:      "forbidden discovery is a permission error",
			discovery: fakeDiscovery{err: forbiddenErr},
			declared:  v1alpha1.PlatformAuto,
			wantErr:   ErrDiscoveryPermission,
		},
		{
			name:      "network discovery error is a dependency error",
			discovery: fakeDiscovery{err: networkErr},
			declared:  v1alpha1.PlatformAuto,
			wantErr:   ErrDiscoveryFailure,
		},
		{
			name:      "declared value outside enum",
			discovery: fakeDiscovery{groups: k8sGroups()},
			declared:  v1alpha1.Platform("windows"),
			wantErr:   ErrUnsupportedPlatform,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectPlatform(tc.discovery, tc.declared)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("DetectPlatform() err=%v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectPlatform() unexpected err=%v", err)
			}
			if got != tc.want {
				t.Fatalf("DetectPlatform() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetectionReasonMapping pins the rule that API absence and
// permission/network failures map to DISTINCT condition reasons.
func TestDetectionReasonMapping(t *testing.T) {
	cases := []struct {
		err    error
		reason string
		retry  bool
	}{
		{ErrPlatformConflict, v1alpha1.ReasonPlatformDetectionFailed, true},
		{ErrDiscoveryPermission, v1alpha1.ReasonPermissionDenied, true},
		{ErrDiscoveryFailure, v1alpha1.ReasonDependencyMissing, true},
		{ErrUnsupportedPlatform, v1alpha1.ReasonUnsupportedPlatform, false},
	}
	for _, tc := range cases {
		reason, retry := detectionReason(tc.err)
		if reason != tc.reason || retry != tc.retry {
			t.Fatalf("detectionReason(%v) = (%q, %v), want (%q, %v)", tc.err, reason, retry, tc.reason, tc.retry)
		}
	}
}

// TestImmutablePlatformStatusClearResidualLimitation pins the
// RECONCILIER-SIDE residual of the platform immutability check: in an API
// without CEL enforcement (the fake client), an actor that clears
// status.specPlatform and then changes spec.platform bypasses the
// status-backed check — the reconciler re-accepts the new platform and
// re-records it. On the real API (K8s >= 1.25, where CEL validation is
// available) the CRD CEL rule compares against oldSelf and rejects the change
// regardless of any status write — proven by
// TestEnvtestPlatformImmutableAtAPI. This test documents the exact boundary
// of the defense-in-depth check instead of pretending it is airtight.
func TestImmutablePlatformStatusClearResidualLimitation(t *testing.T) {
	te := newTestEnv(t, validCR())
	te.reconcile(t, v1alpha1.QuartetName)

	q := te.get(t, v1alpha1.QuartetName)
	if q.Status.SpecPlatform != v1alpha1.PlatformAuto {
		t.Fatalf("precondition: specPlatform = %q, want auto (first acceptance)", q.Status.SpecPlatform)
	}
	// The bypassing external writes (Quartet has the status subresource:
	// status and spec are separate API writes even for one actor).
	q.Status.SpecPlatform = ""
	if err := te.client.Status().Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	q = te.get(t, v1alpha1.QuartetName)
	q.Spec.Platform = v1alpha1.PlatformKubernetes
	if err := te.client.Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)

	got := te.get(t, v1alpha1.QuartetName)
	if got.Status.SpecPlatform != v1alpha1.PlatformKubernetes {
		t.Fatalf("reconciler re-accepted the platform but did not re-record it: specPlatform=%q", got.Status.SpecPlatform)
	}
	if c := condition(t, got.Status.Conditions, v1alpha1.ConditionReady); c.Reason == v1alpha1.ReasonInvalidSpec {
		t.Fatalf("the re-recorded platform must not stay InvalidSpec: %v", c)
	}
	// The check still fires when the status record is intact (it was just
	// re-recorded): flipping again without clearing is InvalidSpec.
	got.Spec.Platform = v1alpha1.PlatformOpenShift
	if err := te.client.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	te.reconcile(t, v1alpha1.QuartetName)
	c2 := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c2.Status != metav1.ConditionFalse || c2.Reason != v1alpha1.ReasonInvalidSpec {
		t.Fatalf("intact status record must still reject a platform change: (%v %q)", c2.Status, c2.Reason)
	}
}

// assertNoOperands pins the strengthened singleton claim: a CR that the
// reconciler refuses (foreign name) never MANAGES operands — not just
// "no CSIDriver", but no operand kind of any profile anywhere, and no
// operand namespace.
//
// The four RBAC kinds are NOT listed here: operand RBAC
// is installer-owned out-of-band and the test envs pre-seed a healthy install
// set that exists independently of any CR — its presence is expected, not a
// management claim.
func assertNoOperands(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	for _, k := range []client.Object{
		&corev1.ServiceAccount{}, &corev1.ConfigMap{}, &coordinationv1.Lease{},
		&corev1.Service{}, &appsv1.Deployment{}, &appsv1.DaemonSet{},
		&storagev1.CSIDriver{},
	} {
		list := listFor(t, k)
		opts := []client.ListOption{}
		if !isClusterScopedObj(k) {
			opts = append(opts, client.InNamespace(gateNS))
		}
		if err := c.List(ctx, list, opts...); err != nil {
			t.Fatalf("list %T: %v", k, err)
		}
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(list)
		if err != nil {
			t.Fatalf("unstructured %T: %v", list, err)
		}
		items, _, _ := unstructured.NestedSlice(u, "items")
		if len(items) != 0 {
			t.Fatalf("non-singleton CR managed operands: %d %T present", len(items), k)
		}
	}
	if ns := getNamespace(c, gateNS); ns != nil {
		t.Fatalf("non-singleton CR created the operand namespace %q", gateNS)
	}
}

// TestEnabledFCAndISCSIProfilesIsolated: enabled FC AND iSCSI profiles
// render behind their seeded installer-owned RBAC sets, appear in
// status.profiles with their OWN conditions, create their OWN operands,
// and do not affect the NFS profile's state (profile isolation). The
// invalid-profile dispatch negative is covered by
// TestProfileContractUnknownProfile and the fresh-cluster negative by
// TestEnabledProfileWithoutInstallSetGatesBeforeOperands.
func TestEnabledFCAndISCSIProfilesIsolated(t *testing.T) {
	q := validCR()
	// ProfileSpecs() returns a throwaway map of pointers — assign through
	// the real spec fields.
	q.Spec.Profiles.FC = profileSpecFor(v1alpha1.ProfileFC)
	q.Spec.Profiles.ISCSI = profileSpecFor(v1alpha1.ProfileISCSI)
	te := newTestEnv(t, q)
	convergeReadiness(t, te)
	got := te.get(t, v1alpha1.QuartetName)

	for _, prof := range []string{v1alpha1.ProfileFC, v1alpha1.ProfileISCSI} {
		ps, ok := got.Status.Profiles[prof]
		if !ok {
			t.Fatalf("enabled %s must appear in status.profiles (a claim, not a silent no-op)", prof)
		}
		// Its OWN condition — the fake env gates workloads; a wired
		// renderer never surfaces RenderError, and never NFS's state.
		if c := condition(t, ps.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse ||
			c.Reason == v1alpha1.ReasonRenderError || c.Reason == v1alpha1.ReasonDataInUse {
			t.Fatalf("%s Ready = (%v %q), want (False <own gate reason>)", prof, c.Status, c.Reason)
		}
		// Its OWN operands rendered behind the seeded install set.
		driverName, _ := renderer.ProvisionerIdentity(prof)
		cd := &storagev1.CSIDriver{}
		if err := te.client.Get(context.Background(), types.NamespacedName{Name: driverName}, cd); err != nil {
			t.Fatalf("enabled %s must create its own CSIDriver: %v", prof, err)
		}
	}
	// NFS is unaffected by the other profiles' gating (it converged).
	if c := condition(t, got.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("nfs must stay Ready despite fc/iscsi being gated: (%v %q)", c.Status, c.Reason)
	}
	// The aggregate honestly surfaces the FIRST blocked profile in the
	// fixed order (nfs is Ready here, so fc's gate reason wins).
	agg := condition(t, got.Status.Conditions, v1alpha1.ConditionReady)
	if agg.Status != metav1.ConditionFalse || !strings.Contains(agg.Message, v1alpha1.ProfileFC) {
		t.Fatalf("aggregate must name the failed profile honestly: (%v %q) %s", agg.Status, agg.Reason, agg.Message)
	}
}

// TestEnabledProfileWithoutInstallSetGatesBeforeOperands: on a cluster
// where the admin has NOT applied profile P's installer-owned RBAC set,
// enabling P gates at RBACDrift BEFORE any operand write — P creates no
// CSIDriver and no Deployment, and the other profile is unaffected. This
// is the out-of-band provisioning model working as designed, not an
// absence of support.
func TestEnabledProfileWithoutInstallSetGatesBeforeOperands(t *testing.T) {
	q := validCR()
	fcSpec := profileSpecFor(v1alpha1.ProfileFC)
	q.Spec.Profiles.FC = fcSpec
	te := newTestEnv(t, q)
	removeInstallRBACFor(t, te, v1alpha1.ProfileFC)
	convergeReadiness(t, te)
	got := te.get(t, v1alpha1.QuartetName)

	fc := got.Status.Profiles[v1alpha1.ProfileFC]
	if c := condition(t, fc.Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonRBACDrift {
		t.Fatalf("fc without its install set = (%v %q), want (False RBACDrift)", c.Status, c.Reason)
	}
	if c := condition(t, fc.Conditions, v1alpha1.ConditionDegraded); c.Status != metav1.ConditionTrue {
		t.Fatal("fc Degraded must be True behind an unapplied install set")
	}
	// The gate is BEFORE any operand write: nothing created for fc.
	fcDriver, _ := renderer.ProvisionerIdentity(v1alpha1.ProfileFC)
	cd := &storagev1.CSIDriver{}
	if err := te.client.Get(context.Background(), types.NamespacedName{Name: fcDriver}, cd); !apierrors.IsNotFound(err) {
		t.Fatalf("fc without its install set must create no CSIDriver (err=%v)", err)
	}
	dep := &appsv1.Deployment{}
	if err := te.client.Get(context.Background(),
		types.NamespacedName{Name: "fc-csi-ngxstorage-controller", Namespace: renderer.OperandNamespace(v1alpha1.ProfileFC)}, dep); !apierrors.IsNotFound(err) {
		t.Fatalf("fc without its install set must create no Deployment (err=%v)", err)
	}
	// NFS unaffected.
	if c := condition(t, got.Status.Profiles[v1alpha1.ProfileNFS].Conditions, v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("nfs must stay Ready despite fc's RBACDrift: (%v %q)", c.Status, c.Reason)
	}
}

// TestEnvtestPlatformImmutableAtAPI: creating with a platform passes
// (on create oldSelf == self); changing spec.platform is rejected by the
// API server — BOTH with the status record intact AND after
// status.specPlatform was cleared. The CR content is never modified.
func TestEnvtestPlatformImmutableAtAPI(t *testing.T) {
	requireEnvtest(t)
	ensureSingleton(t, nil)
	pollStatus(t, v1alpha1.QuartetName, func(q *v1alpha1.Quartet) bool {
		return q.Status.SpecPlatform == v1alpha1.PlatformAuto &&
			hasCondition(q.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	})

	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	getSingleton := func() *v1alpha1.Quartet {
		t.Helper()
		q := &v1alpha1.Quartet{}
		if err := testClient.Get(ctx, types.NamespacedName{Name: v1alpha1.QuartetName}, q); err != nil {
			t.Fatalf("get singleton: %v", err)
		}
		return q
	}

	// 1. Status record intact: the API rejects the platform change.
	q := getSingleton()
	q.Spec.Platform = v1alpha1.PlatformOpenShift
	if err := testClient.Update(ctx, q); err == nil {
		t.Fatal("spec.platform change must be rejected by the API (CEL x-kubernetes-validations)")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("rejection must cite the immutability rule, got: %v", err)
	}

	// 2. Status reset attempt: clearing status.specPlatform must not
	// re-arm the change (the rule reads oldSelf, not status).
	q = getSingleton()
	q.Status.SpecPlatform = ""
	if err := testClient.Status().Update(ctx, q); err != nil {
		t.Fatalf("clear status.specPlatform: %v", err)
	}
	q = getSingleton()
	q.Spec.Platform = v1alpha1.PlatformKubernetes
	if err := testClient.Update(ctx, q); err == nil {
		t.Fatal("spec.platform change after a status reset must STILL be rejected by the API")
	}

	// The stored spec never changed, and the reconciler never saw an
	// InvalidSpec state (the rejected updates never reached it).
	got := getSingleton()
	if got.Spec.Platform != v1alpha1.PlatformAuto {
		t.Fatalf("stored spec.platform = %q, want the unchanged auto", got.Spec.Platform)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == v1alpha1.ConditionReady && c.Reason == v1alpha1.ReasonInvalidSpec {
			t.Fatalf("reconciler recorded an InvalidSpec hold it should never have seen: %v", c)
		}
	}
}
