package controller

import (
	"context"
	"fmt"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	authenticationv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
	"sync"
	"testing"
	"time"
)

var (
	rbacAPIOnce sync.Once
	rbacAPIEnv  *envtest.Environment
	rbacAPIErr  error
	rbacAdmin   client.Client
	// WithWatch (not just Client): the informer-watch test opens real
	// informer-style WATCH streams through the token-only SA client.
	rbacSA client.WithWatch
)

func repoFile(t *testing.T, path string) []byte {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// startRBACAPI lazily boots the dedicated RBAC-authorized control plane.
// In optional mode (no assets) it SKIPs with the shared machine-verified
// skip counter; in required mode a startup failure FAILS.
func startRBACAPI(t *testing.T) {
	t.Helper()
	required := envtestRequiredMode()
	rbacAPIOnce.Do(func() {
		if os.Getenv("KUBEBUILDER_ASSETS") == "" {
			rbacAPIErr = errRBACAPIBlocked
			return
		}
		scheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(scheme)

		// envtest's kube-apiserver runs --authorization-mode=RBAC by
		// DEFAULT (controlplane/apiserver.go). The bootstrap admin user
		// passes every check only via the built-in system:masters rule;
		// any other principal — including the manager SA — is judged by
		// the ClusterRole/Binding we install.
		//
		// The Quartet CRD is installed: the plane
		// hosts a REAL manager reconciling the singleton through the
		// token-only SA, so the API type must exist.
		env := &envtest.Environment{
			Scheme:                scheme,
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
		}
		cfg, err := env.Start()
		if err != nil {
			rbacAPIErr = err
			return
		}
		rbacAPIEnv = env
		rbacAPIErr = bootstrapRBAC(t, cfg)
		if rbacAPIErr != nil {
			_ = env.Stop()
			rbacAPIEnv = nil
		}
	})
	if rbacAPIErr != nil {
		if required {
			t.Fatalf("RBAC envtest required but unavailable: %v", rbacAPIErr)
		}
		envtestSkipsMu.Lock()
		envtestSkips++
		envtestSkipsMu.Unlock()
		t.Skipf("SKIP: RBAC envtest requires KUBEBUILDER_ASSETS: %v", rbacAPIErr)
	}
}

// sarDiagnose asks the API server (as admin, on behalf of the manager SA)
// why a request is or isn't allowed, and prints the exact RBAC reasoning.
// Used only in failure branches to make a denied/allowed assertion
// self-explaining.
func sarDiagnose(t *testing.T, ctx context.Context, ns, verb, group, resource, name string) {
	t.Helper()
	// Two probes: the verb actually requested, and `escalate` — the API
	// server additionally requires escalate when a created Role grants
	// permissions beyond the requester's own.
	for _, probeVerb := range []string{verb, "escalate"} {
		sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
			User: "system:serviceaccount:quartet-system:quartet-manager",
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb: probeVerb, Group: group, Resource: resource, Namespace: ns, Name: name,
			},
		}}
		if err := rbacAdmin.Create(ctx, sar); err != nil {
			t.Logf("SAR diagnostic failed: %v", err)
			return
		}
		t.Logf("SAR verb=%s %s/%s: allowed=%v denied=%q", probeVerb, group, resource, sar.Status.Allowed, sar.Status.Reason)
	}
}

var errRBACAPIBlocked = &rbacAPIBlocked{}

type rbacAPIBlocked struct{}

func (*rbacAPIBlocked) Error() string {
	return "RBAC envtest assets unavailable (KUBEBUILDER_ASSETS empty — run `make test-envtest`)"
}

// rbacSACfg is the token-only SA rest.Config — kept at package level so the
// real token-only manager can be built over it.
var rbacSACfg *rest.Config

// bootstrapRBAC applies the generated config/rbac artifacts plus the
// manager namespace/SA, and builds the two clients: admin (bootstrap,
// carries the Quartet scheme so it can create the singleton) and the
// manager SA via TokenRequest (the test subject).
func bootstrapRBAC(t *testing.T, cfg *rest.Config) error {
	adminScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(adminScheme))
	utilruntime.Must(v1alpha1.AddToScheme(adminScheme))
	admin, err := client.New(cfg, client.Options{Scheme: adminScheme})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "quartet-system"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "quartet-manager", Namespace: "quartet-system"}}
	if err := admin.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(repoFile(t, "config/rbac/role.yaml"), &role); err != nil {
		return err
	}
	if err := admin.Create(ctx, &role); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	var binding rbacv1.ClusterRoleBinding
	if err := yaml.Unmarshal(repoFile(t, "config/rbac/role_binding.yaml"), &binding); err != nil {
		return err
	}
	if err := admin.Create(ctx, &binding); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	req := &authenticationv1.TokenRequest{}
	if err := admin.SubResource("token").Create(ctx, sa, req); err != nil {
		return err
	}
	// client-go IGNORES BearerToken when a client certificate is present
	// (cert auth wins) — the admin cfg carries one, so the SA client must be
	// built WITHOUT the certs and authenticate by token alone.
	saCfg := &rest.Config{
		Host: cfg.Host,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: cfg.CAData,
		},
		BearerToken: req.Status.Token,
	}
	// envtest's returned config carries QPS 1000; a hand-built cfg falls to
	// client-go's 5 QPS default and starves the reconciler's 135-object
	// apply (dedicated RBAC plane).
	saCfg.QPS = 1000
	saCfg.Burst = 1000
	rbacSACfg = saCfg
	// NewWithWatch (not New): the informer-watch test opens real
	// informer-style WATCH streams through the token-only SA client, so the
	// watch surface must be
	// declared, not asserted after the fact.
	rbacSA, err = client.NewWithWatch(saCfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		return err
	}
	rbacAdmin = admin
	return nil
}

// TestEnvtestRBACAuthorizesUsageGate (dynamic half): the manager SA,
// authorized by the GENERATED role.yaml alone, must run exactly the three
// lists apply.go performs in the usage gate — including
// storage.k8s.io/volumeattachments, which a mismatched rule listed under
// the core group (a rule the API server never matches → Forbidden →
// fail-safe wedge
// on every disable/delete). The negative control proves the plane actually
// enforces RBAC.
func TestEnvtestRBACAuthorizesUsageGate(t *testing.T) {
	startRBACAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	if err := rbacSA.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ngxstorage-nfs"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("manager SA cannot create the operand namespace: %v", err)
	}

	for name, list := range map[string]client.ObjectList{
		"core/persistentvolumeclaims":      &corev1.PersistentVolumeClaimList{},
		"core/persistentvolumes":           &corev1.PersistentVolumeList{},
		"storage.k8s.io/volumeattachments": &storagev1.VolumeAttachmentList{},
	} {
		err := rbacSA.List(ctx, list, client.InNamespace("ngxstorage-nfs"))
		if apierrors.IsForbidden(err) {
			t.Fatalf("manager SA is FORBIDDEN listing %s — role.yaml group/resource mismatch: %v", name, err)
		}
		if err != nil {
			t.Fatalf("list %s: %v", name, err)
		}
	}

	// Negative control: the SA must NOT be able to list nodes (no rule) —
	// if this passes, authorization is AllowAll and the positive checks
	// above prove nothing.
	err := rbacSA.List(ctx, &corev1.NodeList{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("negative control failed: manager SA listed nodes without a rule (RBAC enforcement not active?): %v", err)
	}
}

// seedInstallRBAC applies the kubernetes install set (renderer.InstallRBAC)
// on the RBAC plane as ADMIN — the installer's out-of-band step. Objects the
// plane already has (from a previous test) are reset first so every test
// starts from a known-healthy install.
func seedInstallRBAC(t *testing.T, ctx context.Context) {
	t.Helper()
	const ns = "ngxstorage-nfs"
	if err := rbacAdmin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create operand namespace: %v", err)
	}
	profile := renderer.For(renderer.ProfileInput{
		Profile: v1alpha1.ProfileNFS, Platform: v1alpha1.PlatformKubernetes,
	})
	if profile == nil {
		t.Fatal("NFS renderer missing")
	}
	for _, o := range profile.InstallRBAC() {
		_ = rbacAdmin.Delete(ctx, o.DeepCopyObject().(client.Object))
		if err := rbacAdmin.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("seed install RBAC %s: %v", kindName(o), err)
		}
	}
}

// TestEnvtestRBACOperandOutOfBandContract (dynamic half):
// with the generated role.yaml as the SA's only grant —
// - READS are ALLOWED and unrestricted (informer cache, cleanup and the
// RBACDrift health gate):
// a GET of the pinned install objects AND of a foreign rbac name
// authorize — the informer cache, cleanup enumeration and the reconcile-
// start health verification issue unfiltered/named reads that a
// resourceNames pin would Forbidden;
// - EVERY mutating verb on the four RBAC kinds is DENIED — including on
// the pinned install names: the manager holds no create/patch/delete on
// any RBAC object, so it can neither self-provision nor repair drift.
// Denials classify PermissionDenied (retryable).
func TestEnvtestRBACOperandOutOfBandContract(t *testing.T) {
	startRBACAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	seedInstallRBAC(t, ctx)
	const ns = "ngxstorage-nfs"

	// --- PASS side (unrestricted reads + health gate): reads authorize. ---
	cr := &rbacv1.ClusterRole{}
	if err := rbacSA.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, cr); err != nil {
		t.Fatalf("SA read of the pinned install ClusterRole must be ALLOWED (the RBACDrift health gate verifies by pinned get): %v", err)
	}
	t.Log("PASS: SA read of the pinned install ClusterRole allowed")

	seededForeign := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "quartet-foreign-admin", Namespace: ns},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}},
	}
	if err := rbacAdmin.Create(ctx, seededForeign); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("seed foreign Role: %v", err)
	}
	defer func() {
		_ = rbacAdmin.Delete(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: seededForeign.Name, Namespace: ns}})
	}()
	probe := &rbacv1.Role{}
	if err := rbacSA.Get(ctx, types.NamespacedName{Name: seededForeign.Name, Namespace: ns}, probe); err != nil {
		t.Fatalf("SA read of a FOREIGN Role name must be ALLOWED — reads are unrestricted because the informer cache and cleanup enumerate unfiltered: %v", err)
	}
	t.Log("PASS (unrestricted-read contract): SA read of a foreign Role name allowed")

	// --- DENIED side: every mutating verb on every RBAC kind. ---
	patchBytes := []byte(`{"metadata":{"labels":{"quartet-rbac-test":"x"}}}`)
	err := rbacSA.Patch(ctx, cr, client.RawPatch(types.MergePatchType, patchBytes))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("SA patch of the pinned install ClusterRole must be DENIED (no mutating RBAC verbs — out-of-band contract): got %v", err)
	}
	reason, retryable := classifyApplyError(err)
	if reason != v1alpha1.ReasonPermissionDenied || !retryable {
		t.Fatalf("denial must classify (PermissionDenied, retryable), got (%q %v)", reason, retryable)
	}
	t.Log("DENIED + classified PermissionDenied: SA patch of pinned install ClusterRole")

	if err := rbacSA.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller-operation-lock", Namespace: ns}}); !apierrors.IsForbidden(err) {
		t.Fatalf("SA delete of the pinned install RoleBinding must be DENIED: got %v", err)
	}
	t.Log("DENIED: SA delete of pinned install RoleBinding")

	if err := rbacSA.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "quartet-sa-created-crb"},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "nfs-csi-ngxstorage-controller"}}); !apierrors.IsForbidden(err) {
		t.Fatalf("SA create of a ClusterRoleBinding (even to an operand role) must be DENIED: got %v", err)
	}
	t.Log("DENIED: SA create of a binding to the operand role")

	if err := rbacSA.Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "quartet-sa-created-cr"}}); !apierrors.IsForbidden(err) {
		t.Fatalf("SA create of any ClusterRole must be DENIED: got %v", err)
	}
	t.Log("DENIED: SA create of a ClusterRole")
}

// TestEnvtestRBACEscalationChainDenied (the escalation
// regression the previous model left open): a resourceNames pin bounds a
// role's NAME, never its CONTENT. The old self-provisioning surface (pinned
// delete + unrestricted create/escalate + pinned bind) let the manager
// DELETE the pinned operand ClusterRole, CREATE the SAME name with
// privileged content, and BIND through the pinned binding names — escalating
// privileges and corrupting the existing operand binding. With the out-of-
// band model the manager holds NO mutating RBAC verb at all, so EVERY link
// of that chain is denied, proven here against the real API-server RBAC
// layer with the token-only generated role.yaml as the SA's sole grant:
//
//	(a) DELETE of the pinned operand ClusterRole → DENIED;
//	(b) CREATE of the same name with privileged content (wildcard verbs)
//	 → DENIED — content is irrelevant, the verb is gone;
//	(c) CREATE of a RoleBinding with a FOREIGN subject onto the operand
//	 role → DENIED;
//	(d) PATCH of the operand RoleBinding (subject swap) → DENIED.
//
// Branch (e) — the positive path (admin-applied install set verifies
// healthy, operands apply, Ready path PASSes) — is on the main envtest
// plane, where the manager runs: TestEnvtestRBACDriftGatesOperandApply
// (drift half) and the suite bootstrap + TestEnvtestReconcileValid
// (healthy half). This test never attempts privilege escalation on a live
// cluster — isolated envtest authorization plane only.
func TestEnvtestRBACEscalationChainDenied(t *testing.T) {
	startRBACAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()

	const ns = "ngxstorage-nfs"
	seedInstallRBAC(t, ctx)

	// (a) DELETE the pinned operand ClusterRole.
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"}}
	err := rbacSA.Delete(ctx, cr)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("(a) SA delete of the pinned operand ClusterRole must be DENIED — chain step 1 is closed: got %v", err)
	}
	reason, retryable := classifyApplyError(err)
	if reason != v1alpha1.ReasonPermissionDenied || !retryable {
		t.Fatalf("(a) denial must classify (PermissionDenied, retryable), got (%q %v)", reason, retryable)
	}
	t.Log("DENIED (a): delete of pinned operand ClusterRole")

	// (b) CREATE the same name with privileged content. Admin removes the
	// original first — even in the deleted state the create must fail.
	if err := rbacAdmin.Delete(ctx, cr); err != nil {
		t.Fatalf("admin reset operand ClusterRole: %v", err)
	}
	privileged := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"*"}},
			{APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"}},
		},
	}
	err = rbacSA.Create(ctx, privileged)
	if !apierrors.IsForbidden(err) {
		sarDiagnose(t, ctx, "", "create", "rbac.authorization.k8s.io", "clusterroles", privileged.Name)
		t.Fatalf("(b) SA create of the pinned name with privileged content must be DENIED — chain step 2 is closed (content is irrelevant, the verb is gone): got %v", err)
	}
	t.Log("DENIED (b): create of the pinned name with privileged (wildcard/impersonate) content")

	// restore the healthy install for the remaining branches
	_ = rbacAdmin.Create(ctx, cr)

	// (c) CREATE a RoleBinding with a FOREIGN subject onto the operand role.
	evilRB := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "quartet-evil-rb", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "nfs-csi-ngxstorage-leader-election"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "quartet-manager", Namespace: "quartet-system"}},
	}
	err = rbacSA.Create(ctx, evilRB)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("(c) SA create of a foreign-subject RoleBinding onto the operand role must be DENIED — chain step 3 is closed: got %v", err)
	}
	t.Log("DENIED (c): foreign-subject RoleBinding create onto the operand role")

	// (d) PATCH the operand RoleBinding (subject swap).
	operandRB := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "nfs-csi-ngxstorage-controller-operation-lock", Namespace: ns}}
	if err := rbacSA.Get(ctx, types.NamespacedName{Name: operandRB.Name, Namespace: ns}, operandRB); err != nil {
		t.Fatalf("read operand RoleBinding: %v", err)
	}
	patchBytes := []byte(`{"subjects":[{"kind":"ServiceAccount","name":"quartet-manager","namespace":"quartet-system"}]}`)
	err = rbacSA.Patch(ctx, operandRB, client.RawPatch(types.MergePatchType, patchBytes))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("(d) SA patch of the operand RoleBinding (subject change) must be DENIED — bindings are install-owned: got %v", err)
	}
	t.Log("DENIED (d): operand RoleBinding patch (subject swap)")

	// Negative control: the plane enforces RBAC — the SA still READS.
	probe := &rbacv1.ClusterRole{}
	if err := rbacSA.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-controller"}, probe); err != nil {
		t.Fatalf("negative control: SA read of the operand ClusterRole must still be ALLOWED (health gate): %v", err)
	}
	t.Log("PASS (e, RBAC-plane half): read-only surface intact — the health gate's pinned GET authorizes")
}

// TestEnvtestRBACAuthorizesInformerWatchAndCleanup (dynamic half): the
// manager's informer cache (quartet_controller.go watchedOperands) and the
// cleanup enumeration (apply.go deleteOwnedProfile fresh.List) issue
// UNFILTERED list/watch on the four RBAC types. The earlier name-pinned
// READS were Forbidden for exactly these requests against the real RBAC
// layer, wedging cache sync and cleanup on a real manager SA. With the
// generated role.yaml as the SA's only grant:
// - (a) informer-style cluster-wide List (no fieldSelector) authorizes on
// all four types;
// - (b) Watch authorizes on all four types and DELIVERS a real event
// seeded by admin (a 403 surfaces synchronously at watch start);
// - (c) the cleanup-style List (InNamespace for the namespaced kinds,
// cluster-wide for the cluster kinds) authorizes;
// - (d) a named GET authorizes;
// - the negative control (listing nodes, no rule) stays DENIED — the
// plane genuinely enforces RBAC.
func TestEnvtestRBACAuthorizesInformerWatchAndCleanup(t *testing.T) {
	startRBACAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*suiteTimeout)
	defer cancel()

	const ns = "ngxstorage-nfs"
	if err := rbacSA.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create operand namespace: %v", err)
	}

	kinds := []struct {
		name string
		list client.ObjectList
		seed func()
		key  types.NamespacedName
	}{
		{"roles", &rbacv1.RoleList{}, func() {
			_ = rbacAdmin.Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-role", Namespace: ns}})
		}, types.NamespacedName{Name: "quartet-rbac-watch-probe-role", Namespace: ns}},
		{"rolebindings", &rbacv1.RoleBindingList{}, func() {
			_ = rbacAdmin.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-rb", Namespace: ns}})
		}, types.NamespacedName{Name: "quartet-rbac-watch-probe-rb", Namespace: ns}},
		{"clusterroles", &rbacv1.ClusterRoleList{}, func() {
			_ = rbacAdmin.Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-cr"}})
		}, types.NamespacedName{Name: "quartet-rbac-watch-probe-cr"}},
		{"clusterrolebindings", &rbacv1.ClusterRoleBindingList{}, func() {
			_ = rbacAdmin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-crb"}})
		}, types.NamespacedName{Name: "quartet-rbac-watch-probe-crb"}},
	}
	for _, k := range kinds {
		del := watchProbeDeleteTarget(k.list)
		t.Cleanup(func() {
			_ = rbacAdmin.Delete(context.Background(), del)
		})
	}

	// (a)+(c): informer-style UNFILTERED list and cleanup-style list.
	for _, k := range kinds {
		if err := rbacSA.List(ctx, k.list); apierrors.IsForbidden(err) {
			t.Fatalf("manager SA is FORBIDDEN on the informer-style cluster-wide list of %s — role.yaml read narrowing breaks cache sync: %v", k.name, err)
		} else if err != nil {
			t.Fatalf("list %s: %v", k.name, err)
		}
		if err := rbacSA.List(ctx, k.list, client.InNamespace(ns)); apierrors.IsForbidden(err) {
			t.Fatalf("manager SA is FORBIDDEN on the cleanup-style namespaced list of %s (apply.go fresh.List) — role.yaml read narrowing breaks cleanup: %v", k.name, err)
		} else if err != nil {
			t.Fatalf("namespaced list %s: %v", k.name, err)
		}
	}
	t.Log("PASS: informer-style unfiltered list + cleanup-style list authorized on all four RBAC types")

	// (b): watch authorizes and delivers.
	for _, k := range kinds {
		if !assertSAWatchAuthorized(t, ctx, k.list, k.seed) {
			t.Fatalf("watch of %s: no event received", k.name)
		}
	}
	t.Log("PASS: watch authorized on all four RBAC types — each delivered a real seeded event")

	// (d): named GET.
	got := &rbacv1.Role{}
	if err := rbacSA.Get(ctx, kinds[0].key, got); err != nil {
		t.Fatalf("named GET of the operand probe Role must be authorized (cleanup verifyAndDeleteOwned re-reads): %v", err)
	}
	t.Log("PASS: named GET authorized")

	// Negative control: the SA must NOT list nodes (no rule).
	if err := rbacSA.List(ctx, &corev1.NodeList{}); !apierrors.IsForbidden(err) {
		t.Fatalf("negative control failed: manager SA listed nodes without a rule (RBAC enforcement not active?): %v", err)
	}
}

// assertSAWatchAuthorized opens a token-only WATCH on the given list type and
// reports whether a real event arrived after admin seeded an object. A RBAC
// denial surfaces synchronously as the watch start error (403), so the very
// fact of opening the watch is the authorization proof; the received event
// additionally proves the stream is functional, as an informer's cache sync
// requires.
func assertSAWatchAuthorized(t *testing.T, ctx context.Context, list client.ObjectList, seed func()) bool {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w, err := rbacSA.Watch(wctx, list)
	if apierrors.IsForbidden(err) {
		t.Fatalf("manager SA is FORBIDDEN watching %s — role.yaml read narrowing breaks informer cache sync: %v", listKindName(list), err)
	}
	if err != nil {
		t.Fatalf("watch start for %s: %v", listKindName(list), err)
	}
	defer w.Stop()
	resultCh := w.ResultChan()
	seed()
	// watch.Event is a value type in apimachinery v0.33: the channel's ok
	// flag alone tells us whether an event arrived before the watch closed.
	select {
	case _, ok := <-resultCh:
		return ok
	case <-time.After(8 * time.Second):
		return false
	}
}

func listKindName(list client.ObjectList) string {
	return fmt.Sprintf("%T", list)
}

// watchProbeDeleteTarget builds the admin cleanup target matching a list type.
func watchProbeDeleteTarget(list client.ObjectList) client.Object {
	const ns = "ngxstorage-nfs"
	switch list.(type) {
	case *rbacv1.RoleList:
		return &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-role", Namespace: ns}}
	case *rbacv1.RoleBindingList:
		return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-rb", Namespace: ns}}
	case *rbacv1.ClusterRoleList:
		return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-cr"}}
	case *rbacv1.ClusterRoleBindingList:
		return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "quartet-rbac-watch-probe-crb"}}
	}
	return nil
}
