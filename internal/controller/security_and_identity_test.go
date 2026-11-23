package controller

import (
	"context"
	"fmt"
	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"path/filepath"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"
	"strings"
	"sync/atomic"
	"testing"
)

// -------------------------------------------------------------------
// SCC spec/metadata drift predicate + repair
// -------------------------------------------------------------------

func sccFixture(mutate func(u *unstructured.Unstructured)) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "security.openshift.io/v1",
		"kind":       "SecurityContextConstraints",
		"metadata": map[string]interface{}{
			"name":            "nfs-csi-ngxstorage-node-scc",
			"uid":             "uid-scc",
			"resourceVersion": "100",
			"annotations":     map[string]interface{}{renderer.OwnerUIDAnnotation: "cr-1"},
		},
		"allowPrivilegedContainer": true,
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
	}}
	if mutate != nil {
		mutate(u)
	}
	return u
}

// TestSCCWatchPredicateSpecDrift: the SCC watch predicate — selected by
// predicateForObject for the unstructured SCC — fires on a spec/grant edit
// under the SAME owner annotation (the old ownership-only gate let it
// through), on metadata/owner-annotation drift, and NOT on
// resourceVersion/managedFields churn.
func TestSCCWatchPredicateSpecDrift(t *testing.T) {
	p := predicateForObject(sccFixture(nil))

	if !p.Update(event.UpdateEvent{ObjectOld: sccFixture(nil), ObjectNew: sccFixture(func(u *unstructured.Unstructured) {
		unstructured.SetNestedField(u.Object, false, "allowPrivilegedContainer")
	})}) {
		t.Fatal("SCC spec drift under the same owner annotation must trigger a reconcile")
	}

	if !p.Update(event.UpdateEvent{ObjectOld: sccFixture(nil), ObjectNew: sccFixture(func(u *unstructured.Unstructured) {
		unstructured.SetNestedField(u.Object, map[string]interface{}{"other": "x"}, "metadata", "annotations")
	})}) {
		t.Fatal("SCC metadata (owner annotation) drift must trigger a reconcile")
	}

	if p.Update(event.UpdateEvent{ObjectOld: sccFixture(nil), ObjectNew: sccFixture(func(u *unstructured.Unstructured) {
		unstructured.SetNestedField(u.Object, "101", "metadata", "resourceVersion")
		unstructured.SetNestedField(u.Object, []interface{}{map[string]interface{}{"manager": "quartet-operator", "operation": "Apply"}}, "metadata", "managedFields")
	})}) {
		t.Fatal("resourceVersion/managedFields churn must not trigger a reconcile")
	}
}

// TestSCCWatchPredicateSelection: predicateForObject picks the drift
// gate for the SCC GVK and keeps the conservative ownership-only gate for
// any other unstructured operand.
func TestSCCWatchPredicateSelection(t *testing.T) {
	pSCC := predicateForObject(sccFixture(nil))
	other := &unstructured.Unstructured{}
	other.SetGroupVersionKind(renderer.SCCGVK.GroupVersion().WithKind("SomethingElse"))
	pOther := predicateForObject(other)

	if !pSCC.Update(event.UpdateEvent{
		ObjectOld: sccFixture(nil),
		ObjectNew: sccFixture(func(u *unstructured.Unstructured) {
			unstructured.SetNestedField(u.Object, false, "allowPrivilegedContainer")
		}),
	}) {
		t.Fatal("SCC-GVK object must select the drift predicate")
	}
	if pOther.Update(event.UpdateEvent{
		ObjectOld: sccFixture(nil),
		ObjectNew: sccFixture(func(u *unstructured.Unstructured) {
			unstructured.SetNestedField(u.Object, false, "allowPrivilegedContainer")
		}),
	}) {
		t.Fatal("non-SCC unstructured object must keep the ownership-only gate")
	}
}

// TestSCCDriftRepairedOnReconcile: on the fake-client OpenShift env
// (unstructured SCC — envtest cannot simulate OCP API groups), a grant edit
// made under the SAME owner annotation is repaired on the next reconcile:
// the field returns to the rendered desired value, the ownership annotation
// survives, and the profile settles at Ready.
func TestSCCDriftRepairedOnReconcile(t *testing.T) {
	ctx := context.Background()
	te := newOCPTestEnv(t, validCR())
	// The profile requeues on the workload-readiness gate (the fake env has
	// no kubelet); the operand APPLY — including the SCC — runs first, so
	// settle is not required to observe it.
	te.reconcile(t, v1alpha1.QuartetName)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(renderer.SCCGVK)
	if err := te.client.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-node-scc", Namespace: "ngxstorage-nfs"}, got); err != nil {
		t.Fatalf("applied SCC missing: %v", err)
	}
	// Human/admin edit under the SAME annotation (the drift the old gate
	// missed): flip a grant field and replace another one.
	unstructured.SetNestedField(got.Object, false, "allowPrivilegedContainer")
	unstructured.SetNestedField(got.Object, []interface{}{"other-cap"}, "allowedCapabilities")
	if err := te.client.Update(ctx, got); err != nil {
		t.Fatalf("drift the SCC: %v", err)
	}

	// The drift reconcile repairs the SCC during operand apply (the
	// readiness gate may still requeue the profile — that is orthogonal).
	te.reconcile(t, v1alpha1.QuartetName)
	if err := te.client.Get(ctx, types.NamespacedName{Name: "nfs-csi-ngxstorage-node-scc", Namespace: "ngxstorage-nfs"}, got); err != nil {
		t.Fatal(err)
	}
	privileged, _, _ := unstructured.NestedBool(got.Object, "allowPrivilegedContainer")
	if !privileged {
		t.Fatalf("drifted allowPrivilegedContainer must be repaired to true, got %v", privileged)
	}
	caps, _, _ := unstructured.NestedStringSlice(got.Object, "allowedCapabilities")
	want := []string{"SYS_ADMIN", "SYS_CHROOT"}
	if !reflect.DeepEqual(caps, want) {
		t.Fatalf("drifted allowedCapabilities must be repaired to %v, got %v", want, caps)
	}
	// Ownership survives the repair (annotation intact — no adoption signal,
	// no ForeignResourceConflict).
	ann := got.GetAnnotations()[renderer.OwnerUIDAnnotation]
	if ann != string(te.get(t, v1alpha1.QuartetName).UID) {
		t.Fatalf("repair must keep the owner annotation (want %q), got %q",
			te.get(t, v1alpha1.QuartetName).UID, ann)
	}
}

// -------------------------------------------------------------------
// role.yaml resourceNames pin vs the renderer's fixed name set
// -------------------------------------------------------------------

type roleYAMLRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
	Verbs         []string `json:"verbs"`
}

// roleYAMLDoc is the minimal shape of config/rbac/role.yaml.
type roleYAMLDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Rules []roleYAMLRule `json:"rules"`
}

func parseRoleYAML(t *testing.T) roleYAMLDoc {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("read config/rbac/role.yaml: %v", err)
	}
	var doc roleYAMLDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse role.yaml: %v", err)
	}
	return doc
}

// TestRoleYAMLOutOfBandRBACContract: the generated
// role.yaml enforces the OUT-OF-BAND operand RBAC contract — a static pin
// against the marker-generated artifact, with the dynamic (real API-server
// authorization) half in the envtest RBAC suite
// (TestEnvtestRBACOperandOutOfBandContract /
// TestEnvtestRBACEscalationChainDenied). For each of the four RBAC kinds:
//
// - the union of verbs across ALL rules touching that kind is EXACTLY
// {get, list, watch} — reads for the informer cache + cleanup
// enumeration (unrestricted) and the pinned-name GET the RBACDrift
// health gate performs at reconcile start;
// - NO resourceNames pin anywhere: a pin bounds a role's NAME, never its
// CONTENT, so any name pin on a mutating verb (create/escalate/bind/
// patch/delete) was an open privilege-escalation chain;
// - NO mutating verb (create/escalate/bind/patch/delete) on any RBAC kind —
// operand RBAC is provisioned out-of-band by the installer
// (config/operands/nfs/), and the reconciler only verifies it.
//
// It also pins the secret-absence contract: no rule grants any verb on
// secrets, on any apiGroup.
func TestRoleYAMLOutOfBandRBACContract(t *testing.T) {
	doc := parseRoleYAML(t)
	if doc.Kind != "ClusterRole" || doc.Metadata.Name != "quartet-manager-role" {
		t.Fatalf("role.yaml = %s/%s, want ClusterRole quartet-manager-role", doc.Kind, doc.Metadata.Name)
	}

	wantResources := []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}
	for _, res := range wantResources {
		t.Run(res, func(t *testing.T) {
			verbSet := map[string]bool{}
			sawRule := false
			for i := range doc.Rules {
				r := &doc.Rules[i]
				matches := false
				for _, g := range r.APIGroups {
					if g == "rbac.authorization.k8s.io" {
						matches = true
						break
					}
				}
				if !matches {
					continue
				}
				for _, rr := range r.Resources {
					if rr == res {
						sawRule = true
						for _, v := range r.Verbs {
							verbSet[v] = true
						}
						break
					}
				}
			}
			if !sawRule {
				t.Fatalf("no rbac.authorization.k8s.io rule for %q in role.yaml", res)
			}
			// Reads present (informer/cleanup + RBACDrift gate).
			for _, want := range []string{"get", "list", "watch"} {
				if !verbSet[want] {
					t.Errorf("%q must grant read verb %q (informer cache, cleanup enumeration, RBACDrift health gate)", res, want)
				}
			}
			// NO mutating verb — out-of-band contract.
			for _, verb := range []string{"create", "delete", "update", "patch", "bind", "escalate"} {
				if verbSet[verb] {
					t.Errorf("%q must NOT grant mutating verb %q — operand RBAC is installer-provisioned out-of-band (a name pin never bounds role CONTENT)", res, verb)
				}
			}
		})
	}

	// No resourceNames pin anywhere in the RBAC rules (the escalation chain
	// that motivated the model).
	for i := range doc.Rules {
		r := &doc.Rules[i]
		isRBAC := false
		for _, g := range r.APIGroups {
			if g == "rbac.authorization.k8s.io" {
				isRBAC = true
			}
		}
		if isRBAC && len(r.ResourceNames) > 0 {
			t.Errorf("RBAC rule carries resourceNames %v — no name pin may remain (create/escalate ignore requestInfo.Name; bind/patch/delete are gone)", r.ResourceNames)
		}
	}

	// No secrets grant on any apiGroup.
	for i := range doc.Rules {
		for _, rr := range doc.Rules[i].Resources {
			if rr == "secrets" || rr == "secrets/*" {
				t.Fatalf("role.yaml grants a verb on secrets (the operator must have no Secret API access): %v", doc.Rules[i].Verbs)
			}
		}
	}
}

// TestRendererIdentityContract: every RENDERED profile contributes a
// fixed, non-empty provisioner identity and RBAC name set — a missing
// identity would let the disable/delete usage gate silently ignore that
// profile's live volumes. The union is COLLISION-FREE: a ClusterRole
// name shared by two profiles would make one profile's drift gate and RBAC
// watch pin the other's object. The pinned names exactly cover what the
// renderer installs per platform (k8s set ⊆ pinned; the platform-agnostic
// pin is the ocp superset — the nfsRBACNames rationale). Unknown profile
// names contribute nothing — the invalid-profile negative (For returns nil
// for them; the reconciler reports RenderError, never disabled-or-Ready).
func TestRendererIdentityContract(t *testing.T) {
	empty := func(n renderer.OperandRBAC) bool {
		return len(n.Roles)+len(n.RoleBindings)+len(n.ClusterRoles)+len(n.ClusterRoleBindings) == 0
	}
	hasName := func(set []string, name string) bool {
		for _, s := range set {
			if s == name {
				return true
			}
		}
		return false
	}
	claimed := map[string]string{} // scoped identity key → owning profile
	record := func(prof, kind, ns, name string) {
		key := kind + "/" + ns + "/" + name
		if owner, dup := claimed[key]; dup {
			t.Errorf("install RBAC identity %s claimed by both %s and %s — one profile's drift gate would pin the other's object", key, owner, prof)
		}
		claimed[key] = prof
	}

	for _, prof := range renderer.ProfileNames() {
		p := renderer.For(renderer.ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
		if p == nil {
			// Not rendered: it must claim nothing.
			if n := renderer.OperandRBACNames(prof); !empty(n) {
				t.Fatalf("unrendered profile %s must contribute no RBAC names: %+v", prof, n)
			}
			if driver, prefix := renderer.ProvisionerIdentity(prof); driver != "" || prefix != "" {
				t.Fatalf("unrendered profile %s must contribute no provisioner identity: %q %q", prof, driver, prefix)
			}
			continue
		}
		driver, prefix := renderer.ProvisionerIdentity(prof)
		if driver == "" || prefix == "" {
			t.Fatalf("rendered profile %s must contribute a provisioner identity — the usage gate would silently ignore its volumes", prof)
		}
		names := renderer.OperandRBACNames(prof)
		if empty(names) {
			t.Fatalf("rendered profile %s must contribute a fixed RBAC name set", prof)
		}
		// Pinned names cover every RBAC object the renderer installs, per
		// platform; the collision scan records the same identities.
		ns := renderer.OperandNamespace(prof)
		for _, plat := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
			pr := renderer.For(renderer.ProfileInput{Profile: prof, Platform: plat})
			if pr == nil {
				t.Fatalf("profile %s renders on kubernetes but not %s", prof, plat)
			}
			for _, o := range pr.InstallRBAC() {
				switch o.(type) {
				case *rbacv1.Role:
					if !hasName(names.Roles, o.GetName()) {
						t.Errorf("%s/%s: InstallRBAC Role %q not pinned by OperandRBACNames", prof, plat, o.GetName())
					}
				case *rbacv1.RoleBinding:
					if !hasName(names.RoleBindings, o.GetName()) {
						t.Errorf("%s/%s: InstallRBAC RoleBinding %q not pinned by OperandRBACNames", prof, plat, o.GetName())
					}
				case *rbacv1.ClusterRole:
					if !hasName(names.ClusterRoles, o.GetName()) {
						t.Errorf("%s/%s: InstallRBAC ClusterRole %q not pinned by OperandRBACNames", prof, plat, o.GetName())
					}
				case *rbacv1.ClusterRoleBinding:
					if !hasName(names.ClusterRoleBindings, o.GetName()) {
						t.Errorf("%s/%s: InstallRBAC ClusterRoleBinding %q not pinned by OperandRBACNames", prof, plat, o.GetName())
					}
				default:
					t.Errorf("%s/%s: InstallRBAC emitted a non-RBAC object %T", prof, plat, o)
				}
			}
		}
		for _, name := range names.Roles {
			record(prof, "Role", ns, name)
		}
		for _, name := range names.RoleBindings {
			record(prof, "RoleBinding", ns, name)
		}
		for _, name := range names.ClusterRoles {
			record(prof, "ClusterRole", "", name)
		}
		for _, name := range names.ClusterRoleBindings {
			record(prof, "ClusterRoleBinding", "", name)
		}
	}

	// Unknown profile names contribute nothing (the invalid-profile
	// negative — For returns nil for them).
	for _, bogus := range []string{"", "bogus", "nfs2"} {
		if n := renderer.OperandRBACNames(bogus); !empty(n) {
			t.Fatalf("unknown profile %q must contribute no RBAC names: %+v", bogus, n)
		}
		if driver, prefix := renderer.ProvisionerIdentity(bogus); driver != "" || prefix != "" {
			t.Fatalf("unknown profile %q must contribute no provisioner identity: %q %q", bogus, driver, prefix)
		}
	}
}

// TestRBACMarkersCoverVolumeAttachmentGroup (static half): the generated
// role.yaml grants volumeattachments ONLY under storage.k8s.io (the group
// apply.go actually queries) and keeps PVC/PV in core. The dynamic half —
// the manager SA's real API authorization — is the envtest RBAC suite.
func TestRBACMarkersCoverVolumeAttachmentGroup(t *testing.T) {
	doc := parseRoleYAML(t)
	coreVA, storageVA, corePVC, corePV := false, false, false, false
	for _, r := range doc.Rules {
		for i := range r.APIGroups {
			for _, res := range r.Resources {
				switch {
				case res == "volumeattachments" && r.APIGroups[i] == "":
					coreVA = true
				case res == "volumeattachments" && r.APIGroups[i] == "storage.k8s.io":
					storageVA = true
				case res == "persistentvolumeclaims" && r.APIGroups[i] == "":
					corePVC = true
				case res == "persistentvolumes" && r.APIGroups[i] == "":
					corePV = true
				}
			}
		}
	}
	if coreVA {
		t.Fatal("volumeattachments must NOT be granted under the core group")
	}
	if !storageVA {
		t.Fatal("volumeattachments must be granted under storage.k8s.io")
	}
	if !corePVC || !corePV {
		t.Fatal("PVC/PV must stay in the core group")
	}
}

// A ReplicaSet List failure inside the owner-chain
// readiness probe must NEVER be read as "the Deployment owns no pods" —
// swallowing it would exclude every Deployment pod from the blocked-pod
// scan, so a missing RBAC grant, a timeout or an API 5xx could surface as
// Ready. The probe surfaces the failure as a retryable not-ready state
// (PermissionDenied for 403, DependencyMissing otherwise) and only
// converges once the API path works and the owner chain resolves.
func TestWorkloadGateReplicaSetListFailureNeverReady(t *testing.T) {
	var injected int32
	var failing int32 = 1
	te := newInterceptedEnv(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.ReplicaSetList); ok && atomic.LoadInt32(&failing) == 1 {
				atomic.AddInt32(&injected, 1)
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "apps", Resource: "replicasets"}, "",
					fmt.Errorf("injected RBAC denial"))
			}
			return c.List(ctx, list, opts...)
		},
	}, validCR())

	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	te.reconcile(t, v1alpha1.QuartetName)

	// A blocked pod that only the owner chain would surface: without the
	// ReplicaSet UID map the probe would skip it entirely.
	blockedPod(t, te.client, "nfs-csi-ngxstorage-controller-000001", "CreateContainerConfigError")
	res := te.reconcile(t, v1alpha1.QuartetName)
	if atomic.LoadInt32(&injected) == 0 {
		t.Fatal("injection never fired — the failure path was not exercised")
	}
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("ReplicaSet list failure must be retryable, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status == metav1.ConditionTrue {
		t.Fatalf("probe must never report Ready while the owner-chain list fails: (%v %q)", c.Status, c.Reason)
	}
	if c.Reason != v1alpha1.ReasonPermissionDenied {
		t.Fatalf("403 on the owner-chain list must surface PermissionDenied, got %q", c.Reason)
	}
	if pc := profileReadyNFS(t, te); !strings.Contains(pc.Message, "cannot list ReplicaSets") {
		t.Fatalf("profile message must name the failure: %q", pc.Message)
	}

	// Recovery: the API path heals and the SAME blocked pod — previously
	// invisible because the chain never resolved — is now detected through
	// the owner chain. If the chain were still broken the gate would
	// wrongly reopen.
	atomic.StoreInt32(&failing, 0)
	te.reconcile(t, v1alpha1.QuartetName)
	c = condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status == metav1.ConditionTrue || c.Reason != v1alpha1.ReasonWorkloadNotReady {
		t.Fatalf("after recovery the blocked pod must gate the profile via the owner chain: (%v %q)", c.Status, c.Reason)
	}
	if pc := profileReadyNFS(t, te); !strings.Contains(pc.Message, "CreateContainerConfigError") {
		t.Fatalf("owner chain must now see the blocked pod: %q", pc.Message)
	}
	// Healed pod reopens the gate.
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
		t.Fatalf("healed owner chain + healed pod must converge: (%v %q)", c.Status, c.Reason)
	}
}

// A non-RBAC failure (API 5xx) on the owner-chain list maps to
// DependencyMissing, still retryable and never Ready.
func TestWorkloadGateReplicaSetListError5xxDependencyMissing(t *testing.T) {
	var injected int32
	te := newInterceptedEnv(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.ReplicaSetList); ok {
				atomic.AddInt32(&injected, 1)
				return apierrors.NewServiceUnavailable("injected API outage")
			}
			return c.List(ctx, list, opts...)
		},
	}, validCR())

	te.reconcile(t, v1alpha1.QuartetName)
	fakeWorkloadReady(t, te.client)
	blockedPod(t, te.client, "nfs-csi-ngxstorage-controller-000001", "ImagePullBackOff")
	res := te.reconcile(t, v1alpha1.QuartetName)
	if atomic.LoadInt32(&injected) == 0 {
		t.Fatal("injection never fired — the failure path was not exercised")
	}
	if res.RequeueAfter != retryableRequeueAfter {
		t.Fatalf("5xx on the owner-chain list must be retryable, got %+v", res)
	}
	c := condition(t, te.get(t, v1alpha1.QuartetName).Status.Conditions, v1alpha1.ConditionReady)
	if c.Status == metav1.ConditionTrue {
		t.Fatalf("API outage must never surface as Ready: (%v %q)", c.Status, c.Reason)
	}
	if c.Reason != v1alpha1.ReasonDependencyMissing {
		t.Fatalf("non-RBAC list failure must surface DependencyMissing, got %q", c.Reason)
	}
}
