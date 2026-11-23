package renderer

import (
	"strings"
	"testing"

	apps "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// --- Focused FC render pins. ---
//
// The golden parity tests (fc_parity_test.go) pin field-level equality
// against the clean pinned raw reference; these tests pin each operand
// CATEGORY by name so a regression fails diagnosably and so the semantic
// contract stays visible when a golden is ever regenerated.

// TestFCRenderDocumentCounts pins the operand-set size: the raw doc counts
// (k8s 147, ocp 150) minus the two dropped classes; ocp adds SCC +
// scc-use Role + scc-use RoleBinding.
func TestFCRenderDocumentCounts(t *testing.T) {
	if p := (&fcProfile{in: fcGoldenInput(v1alpha1.PlatformKubernetes)}); len(p.Objects()) != 145 {
		t.Errorf("k8s operand count = %d, want 145", len(p.Objects()))
	}
	if p := (&fcProfile{in: fcGoldenInput(v1alpha1.PlatformOpenShift)}); len(p.Objects()) != 148 {
		t.Errorf("ocp operand count = %d, want 148", len(p.Objects()))
	}
}

// TestFCEmitsNoForbiddenResources asserts the owner-matrix negatives on both
// platforms: no SC/VSC, no Secret (secretRef-only — spec secretRef feeds the env
// secretKeyRefs only), no helm-only test-connection Pod, no Namespace object
// (reconciler ensures it per namespace ownership).
func TestFCEmitsNoForbiddenResources(t *testing.T) {
	forbidden := make(map[string]bool, len(forbiddenKinds)+1)
	for k := range forbiddenKinds {
		forbidden[k] = true
	}
	forbidden["Namespace"] = true

	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := &fcProfile{in: fcGoldenInput(platform)}
		for _, obj := range p.Objects() {
			gvks, _, err := parityScheme.ObjectKinds(obj)
			if err != nil {
				t.Fatalf("resolve GVK for %T: %v", obj, err)
			}
			if forbidden[gvks[0].Kind] {
				t.Errorf("platform %s: renderer emitted forbidden kind %s %q (secretRef-only, no SC/VSC, namespace ownership, helm-only)",
					platform, gvks[0].Kind, obj.GetName())
			}
		}
	}
}

// fcEnvRefs extracts the $(VAR) references one arg string carries.
func fcEnvRefs(arg string) []string {
	var out []string
	rest := arg
	for {
		i := strings.Index(rest, "$(")
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], ")")
		if j < 0 {
			break
		}
		// rest[i:] starts at "$("; the var name runs to rest[i+j] == ')'.
		out = append(out, rest[i+2:i+j])
		rest = rest[i+j+1:]
	}
	return out
}

// fcDaemonSetOf / fcContainerByName / fcEnvByName are the FC test-local
// lookups (fc-prefixed to avoid colliding with the iSCSI worker's helpers).
func fcDaemonSetOf(t *testing.T, objs []client.Object) *apps.DaemonSet {
	t.Helper()
	for _, o := range objs {
		if d, ok := o.(*apps.DaemonSet); ok {
			return d
		}
	}
	t.Fatal("no DaemonSet in rendered set")
	return nil
}

func fcContainerByName(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, c := range containers {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

func fcEnvByName(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// TestFCProtocolArgEnvContract pins the FC driver argv/env relationship —
// the rendered argv must reference ONLY env that exists, and the env
// contract must be exactly the canonical env/secret names-canonical set (STORAGE_* names over the
// secret keys key set) plus fc-target-names. The pre-zoned one-target-per-LUN
// contract lives DRIVER-side: the renderer's whole surface for it is this
// env wiring; there is deliberately no zoning/host-masking/refresh flag.
func TestFCProtocolArgEnvContract(t *testing.T) {
	p := &fcProfile{in: fcGoldenInput(v1alpha1.PlatformKubernetes)}
	deploy := deploymentOf(t, p.Objects())
	ds := fcDaemonSetOf(t, p.Objects())

	ctrl, _ := fcContainerByName(deploy.Spec.Template.Spec.Containers, fcName)
	node, _ := fcContainerByName(ds.Spec.Template.Spec.Containers, fcName)

	// Exact flag-name sets: anything extra would be an invented protocol
	// surface (host masking, fabric zoning, service restart must NOT be
	// operator defaults).
	wantCtrlFlags := map[string]bool{"role": true, "endpoint": true, "ip": true,
		"pool-name": true, "fc-target-names": true, "api-key": true, "log": true}
	wantNodeFlags := map[string]bool{"role": true, "endpoint": true, "ip": true,
		"pool-name": true, "fc-target-names": true, "api-key": true,
		"nodeid": true, "log": true}
	checkFlags := func(t *testing.T, c corev1.Container, want map[string]bool, role string) {
		t.Helper()
		got := make(map[string]bool, len(c.Args))
		for _, a := range c.Args {
			name := strings.TrimPrefix(a, "--")
			if i := strings.Index(name, "="); i >= 0 {
				name = name[:i]
			}
			if !want[name] {
				t.Errorf("%s: unexpected driver flag --%s (invented protocol surface?)", role, name)
			}
			got[name] = true
		}
		for name := range want {
			if !got[name] {
				t.Errorf("%s: missing driver flag --%s", role, name)
			}
		}
	}
	checkFlags(t, ctrl, wantCtrlFlags, "controller")
	checkFlags(t, node, wantNodeFlags, "node")

	// Every $(VAR) referenced in args must be defined by the container env.
	envsDefined := func(c corev1.Container) map[string]bool {
		out := make(map[string]bool, len(c.Env))
		for _, e := range c.Env {
			out[e.Name] = true
		}
		return out
	}
	for _, c := range []struct {
		env  corev1.Container
		role string
	}{
		{ctrl, "controller"},
		{node, "node"},
	} {
		def := envsDefined(c.env)
		for _, a := range c.env.Args {
			for _, ref := range fcEnvRefs(a) {
				if !def[ref] {
					t.Errorf("%s: arg %q references undefined env %s", c.role, a, ref)
				}
			}
		}
	}

	// The canonical env set: env name -> secret key.
	wantEnvKeys := map[string]string{
		"STORAGE_IPS":       "storage-ips",
		"STORAGE_POOL_NAME": "pool-name",
		"FC_TARGET_NAMES":   "fc-target-names",
		"STORAGE_API_KEY":   "api-key",
	}
	for role, c := range map[string]corev1.Container{"controller": ctrl, "node": node} {
		for envName, wantKey := range wantEnvKeys {
			e, ok := fcEnvByName(c, envName)
			if !ok {
				t.Errorf("%s: missing env %s", role, envName)
				continue
			}
			sk := e.ValueFrom
			if sk == nil || sk.SecretKeyRef == nil || sk.SecretKeyRef.Key != wantKey || sk.SecretKeyRef.Name != fcGoldenSecret {
				t.Errorf("%s: env %s must be secretKeyRef %s/%s (canonical env/secret names), got %+v",
					role, envName, fcGoldenSecret, wantKey, e.ValueFrom)
			}
		}
		// --endpoint consumes the CSI_ENDPOINT literal env (source-bound raw).
		if e, _ := fcEnvByName(c, "CSI_ENDPOINT"); e.Value != "unix:///csi/csi.sock" {
			t.Errorf("%s: CSI_ENDPOINT env = %q, want unix:///csi/csi.sock", role, e.Value)
		}
	}
}

// TestFCSpecFieldsPropagate pins digest pin, canonical env/secret names, unquoted log arg on the FC workloads:
// the driver image lands ONLY on the driver containers of both workloads
// (sidecars stay canonical digests), the secretRef name lands ONLY in env
// secretKeyRefs, and logLevel lands in --log=<level> UNQUOTED.
func TestFCSpecFieldsPropagate(t *testing.T) {
	img := "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:" + strings.Repeat("c", 64)
	secret := "customer-fc-secret"
	p := &fcProfile{in: ProfileInput{
		Profile: "fc", SecretName: secret, DriverImage: img, LogLevel: "debug",
		Platform: v1alpha1.PlatformKubernetes,
	}}
	canonical := map[string]bool{}
	for _, d := range []string{
		fcProvisionerDigest, fcAttacherDigest, fcResizerDigest,
		fcSnapshotterDigest, fcLivenessProbeDigest, fcRegistrarDigest,
	} {
		canonical[d] = true
	}

	checkPod := func(t *testing.T, name string, pod corev1.PodSpec) {
		t.Helper()
		driverSeen := 0
		for _, c := range pod.Containers {
			if c.Image == img {
				driverSeen++
				for _, a := range c.Args {
					if strings.HasPrefix(a, "--log") && a != "--log=debug" {
						t.Errorf("%s: unquoted log arg violated — --log arg %q must be exactly --log=debug (unquoted)", name, a)
					}
				}
			} else {
				digest := c.Image[strings.LastIndex(c.Image, ":")+1:]
				if !canonical[digest] {
					t.Errorf("%s: container %q image %q is neither the spec driver image nor a canonical sidecar digest", name, c.Name, c.Image)
				}
			}
			for _, e := range c.Env {
				if e.ValueFrom != nil {
					if sk := e.ValueFrom.SecretKeyRef; sk != nil && sk.Name != secret {
						t.Errorf("%s: container %q env %q secretKeyRef name %q != spec %q", name, c.Name, e.Name, sk.Name, secret)
					}
				}
			}
		}
		if driverSeen != 1 {
			t.Errorf("%s: driver image present %d times, want exactly 1 (digest pin)", name, driverSeen)
		}
	}
	checkPod(t, "controller", deploymentOf(t, p.Objects()).Spec.Template.Spec)
	checkPod(t, "node", fcDaemonSetOf(t, p.Objects()).Spec.Template.Spec)
}

// TestFCInstallRBACMatchesGeneratedSet pins InstallRBAC() == exactly the RBAC
// subset of Objects(), on both platforms, and the name set == fcRBACNames —
// the installer-owned out-of-band set must never drift from the render.
func TestFCInstallRBACMatchesGeneratedSet(t *testing.T) {
	rbacKinds := map[string]bool{
		"Role": true, "RoleBinding": true, "ClusterRole": true, "ClusterRoleBinding": true,
	}
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := &fcProfile{in: fcGoldenInput(platform)}
		rendered := renderedAsJSON(t, p.Objects())
		subset := map[string]map[string]interface{}{}
		for key, doc := range rendered {
			if rbacKinds[docKeyKind(doc)] {
				subset[key] = doc
			}
		}
		want := renderedAsJSON(t, p.InstallRBAC())
		assertSetEqual(t, want, subset)

		gotNames := OperandRBAC{}
		for _, obj := range p.InstallRBAC() {
			switch obj.(type) {
			case *rbacv1.Role:
				gotNames.Roles = append(gotNames.Roles, obj.GetName())
			case *rbacv1.RoleBinding:
				gotNames.RoleBindings = append(gotNames.RoleBindings, obj.GetName())
			case *rbacv1.ClusterRole:
				gotNames.ClusterRoles = append(gotNames.ClusterRoles, obj.GetName())
			case *rbacv1.ClusterRoleBinding:
				gotNames.ClusterRoleBindings = append(gotNames.ClusterRoleBindings, obj.GetName())
			}
		}
		// The fixed pin fcRBACNames is platform-agnostic (the extra
		// harmless scc-use names are listed even on k8s — same model as
		// nfsRBACNames); the rendered per-platform set is the pin minus
		// scc-use on k8s, equal on ocp.
		wantNames := fcRBACNames
		if platform == v1alpha1.PlatformKubernetes {
			wantNames = fcRBACNamesMinus(wantNames, "fc-csi-ngxstorage-node-scc-use")
		}
		if !sameStringSets(gotNames.Roles, wantNames.Roles) ||
			!sameStringSets(gotNames.RoleBindings, wantNames.RoleBindings) ||
			!sameStringSets(gotNames.ClusterRoles, wantNames.ClusterRoles) ||
			!sameStringSets(gotNames.ClusterRoleBindings, wantNames.ClusterRoleBindings) {
			t.Errorf("platform %s: InstallRBAC names %+v != want %+v", platform, gotNames, wantNames)
		}
	}
}

func docKeyKind(doc map[string]interface{}) string {
	k, _ := doc["kind"].(string)
	return k
}

func fcRBACNamesMinus(in OperandRBAC, name string) OperandRBAC {
	out := OperandRBAC{}
	for _, r := range in.Roles {
		if r != name {
			out.Roles = append(out.Roles, r)
		}
	}
	for _, r := range in.RoleBindings {
		if r != name {
			out.RoleBindings = append(out.RoleBindings, r)
		}
	}
	out.ClusterRoles = in.ClusterRoles
	out.ClusterRoleBindings = in.ClusterRoleBindings
	return out
}

func sameStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestFCRBACRuleFidelity pins the FC-specific RBAC semantics by category:
// the controller ClusterRole's exact 12-rule set, the two operation-lock
// Roles restricted to the full 128-lease resourceNames, leader-election
// unconstrained, node-holder-pod-read pods/get ONLY, and every binding to a
// single subject.
func TestFCRBACRuleFidelity(t *testing.T) {
	p := &fcProfile{in: fcGoldenInput(v1alpha1.PlatformKubernetes)}
	// Key by kind/name — names alone collide (the controller SA and the
	// ClusterRole share "fc-csi-ngxstorage-controller").
	byKindName := map[string]client.Object{}
	for _, obj := range p.Objects() {
		gvks, _, err := parityScheme.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("resolve GVK for %T: %v", obj, err)
		}
		byKindName[gvks[0].Kind+"/"+obj.GetName()] = obj
	}

	cr, _ := byKindName["ClusterRole/fc-csi-ngxstorage-controller"].(*rbacv1.ClusterRole)
	if cr == nil || len(cr.Rules) != 12 {
		t.Fatalf("controller ClusterRole must carry exactly the 12 pinned raw rules, got %v", rulesCount(cr))
	}

	leases := fcLeasePoolNames()
	for _, name := range []string{"fc-csi-ngxstorage-controller-operation-lock", "fc-csi-ngxstorage-node-operation-lock"} {
		r, _ := byKindName["Role/"+name].(*rbacv1.Role)
		if r == nil || len(r.Rules) != 1 ||
			len(r.Rules[0].Resources) != 1 || r.Rules[0].Resources[0] != "leases" ||
			!sameStringSets(r.Rules[0].ResourceNames, leases) {
			t.Errorf("Role %s: must be a single leases rule pinned to the full %d-lease pool", name, fcShardCount)
		}
	}
	le, _ := byKindName["Role/fc-csi-ngxstorage-leader-election"].(*rbacv1.Role)
	if le == nil || le.Rules[0].ResourceNames != nil {
		t.Error("leader-election Role must NOT carry resourceNames (sidecars elect freely)")
	}
	hpr, _ := byKindName["Role/fc-csi-ngxstorage-node-holder-pod-read"].(*rbacv1.Role)
	if hpr == nil || len(hpr.Rules) != 1 || len(hpr.Rules[0].Resources) != 1 ||
		hpr.Rules[0].Resources[0] != "pods" || len(hpr.Rules[0].Verbs) != 1 || hpr.Rules[0].Verbs[0] != "get" {
		t.Errorf("node-holder-pod-read Role must be pods/get ONLY, got %+v", hpr)
	}

	// Every binding binds exactly ONE subject; controller ops -> controller
	// SA, node ops -> node SA.
	wantSubject := map[string]string{
		"fc-csi-ngxstorage-controller-operation-lock": "fc-csi-ngxstorage-controller",
		"fc-csi-ngxstorage-node-operation-lock":       "fc-csi-ngxstorage-node",
		"fc-csi-ngxstorage-leader-election":           "fc-csi-ngxstorage-controller",
		"fc-csi-ngxstorage-node-holder-pod-read":      "fc-csi-ngxstorage-node",
	}
	for name, sa := range wantSubject {
		rb, _ := byKindName["RoleBinding/"+name].(*rbacv1.RoleBinding)
		if rb == nil || len(rb.Subjects) != 1 || rb.Subjects[0].Name != sa || rb.Subjects[0].Kind != rbacv1.ServiceAccountKind {
			t.Errorf("RoleBinding %s: must bind exactly ServiceAccount %s, got %+v", name, sa, rb)
		}
	}
	crb, _ := byKindName["ClusterRoleBinding/fc-csi-ngxstorage-controller"].(*rbacv1.ClusterRoleBinding)
	if crb == nil || len(crb.Subjects) != 1 || crb.Subjects[0].Name != "fc-csi-ngxstorage-controller" {
		t.Errorf("ClusterRoleBinding must bind exactly the controller SA, got %+v", crb)
	}
}

func rulesCount(cr *rbacv1.ClusterRole) int {
	if cr == nil {
		return -1
	}
	return len(cr.Rules)
}

// TestFCCoordinationShardWiring pins the coordination category: immutable
// ConfigMap with the lease-prefix/shard-count data, the 128-lease pool, the
// driver env agreement, and the operation-lock Role resourceNames.
func TestFCCoordinationShardWiring(t *testing.T) {
	p := &fcProfile{in: fcGoldenInput(v1alpha1.PlatformKubernetes)}

	cmSeen, leaseSeen := 0, 0
	var cm *corev1.ConfigMap
	for _, obj := range p.Objects() {
		switch o := obj.(type) {
		case *corev1.ConfigMap:
			cmSeen++
			cm = o
		case *coordinationv1.Lease:
			leaseSeen++
		}
	}
	if cmSeen != 1 || leaseSeen != fcShardCount {
		t.Fatalf("coordination set: %d ConfigMaps + %d Leases rendered, want 1 + %d", cmSeen, leaseSeen, fcShardCount)
	}
	if !ptrToTrue(cm.Immutable) {
		t.Error("coordination ConfigMap must be immutable (coordination)")
	}
	if cm.Data["lease-prefix"] != fcLeasePrefix || cm.Data["shard-count"] != "128" {
		t.Errorf("coordination ConfigMap data = %+v, want lease-prefix=%s shard-count=128", cm.Data, fcLeasePrefix)
	}

	deploy := deploymentOf(t, p.Objects())
	ds := fcDaemonSetOf(t, p.Objects())
	for _, c := range []corev1.Container{
		mustFCContainer(t, deploy.Spec.Template.Spec.Containers),
		mustFCContainer(t, ds.Spec.Template.Spec.Containers),
	} {
		if e, _ := fcEnvByName(c, "NGX_COORDINATION_LEASE_PREFIX"); e.Value != fcLeasePrefix {
			t.Errorf("driver env NGX_COORDINATION_LEASE_PREFIX = %q, want %q", e.Value, fcLeasePrefix)
		}
		if e, _ := fcEnvByName(c, "NGX_COORDINATION_SHARD_COUNT"); e.Value != "128" {
			t.Errorf("driver env NGX_COORDINATION_SHARD_COUNT = %q, want 128", e.Value)
		}
	}
}

func mustFCContainer(t *testing.T, containers []corev1.Container) corev1.Container {
	t.Helper()
	c, ok := fcContainerByName(containers, fcName)
	if !ok {
		t.Fatal("no driver container in pod spec")
	}
	return c
}

func ptrToTrue(p *bool) bool { return p != nil && *p }

// TestFCSCCUseGrantOpenShiftOnly pins the platform-policy category: k8s has
// NO SCC and NO scc-use RBAC and NO required-scc annotation; ocp has all
// three, and the use grant resolves to the node SA only (the SCC's users
// list stays empty — access flows through the grant).
func TestFCSCCUseGrantOpenShiftOnly(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := &fcProfile{in: fcGoldenInput(platform)}
		hasSCC, hasSccUse, sccUseOK := false, false, false
		ds := fcDaemonSetOf(t, p.Objects())
		ann := ds.Spec.Template.Annotations["openshift.io/required-scc"]
		for _, obj := range p.Objects() {
			switch o := obj.(type) {
			case *unstructured.Unstructured:
				hasSCC = true
			case *rbacv1.Role:
				if o.GetName() == "fc-csi-ngxstorage-node-scc-use" {
					hasSccUse = true
					sccUseOK = len(o.Rules) == 1 && o.Rules[0].ResourceNames[0] == "fc-csi-ngxstorage-node-scc" && o.Rules[0].Verbs[0] == "use"
				}
			case *rbacv1.RoleBinding:
				if o.GetName() == "fc-csi-ngxstorage-node-scc-use" && len(o.Subjects) == 1 &&
					o.Subjects[0].Name == "fc-csi-ngxstorage-node" {
					sccUseOK = true
				}
			}
		}
		want := platform == v1alpha1.PlatformOpenShift
		if hasSCC != want || hasSccUse != want {
			t.Errorf("platform %s: SCC/scc-use presence = %v/%v, want %v (OCP-only, normalized comparison)", platform, hasSCC, hasSccUse, want)
		}
		if (ann == "fc-csi-ngxstorage-node-scc") != want {
			t.Errorf("platform %s: node DS required-scc annotation = %q", platform, ann)
		}
		if want && !sccUseOK {
			t.Error("platform ocp: scc-use grant must pin the node SCC and bind the node SA only")
		}
	}
}

// TestFCPlacementSecuritySourceBound pins the placement/security/host-mount
// category: placement rule placement (node DS worker-pin both platforms — canonical;
// controller free placement — the adopted default, raw worker-pin a
// recorded divergence), tolerations, priority classes, privileged ONLY on
// the node driver, the projected token volume mounted read-only on the
// driver container only, hostPath types and Bidirectional propagation,
// IfNotPresent everywhere, and CSIDriver fsGroupPolicy File (fsGroupPolicy File open —
// raw contract value).
func TestFCPlacementSecuritySourceBound(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := &fcProfile{in: fcGoldenInput(platform)}
		deploy := deploymentOf(t, p.Objects())
		ds := fcDaemonSetOf(t, p.Objects())
		cpod, npod := deploy.Spec.Template.Spec, ds.Spec.Template.Spec

		if cpod.PriorityClassName != "system-cluster-critical" || npod.PriorityClassName != "system-node-critical" {
			t.Errorf("priority classes: controller %q / node %q", cpod.PriorityClassName, npod.PriorityClassName)
		}
		// placement rule: node DS worker-pinned on BOTH platforms (canonical raw
		// behavior of all three drivers); controller FREE placement (the adopted default
		// adopted default — os=linux only, the raw worker-pin stripped).
		if len(cpod.NodeSelector) != 1 || cpod.NodeSelector["kubernetes.io/os"] != "linux" {
			t.Errorf("platform %s: controller nodeSelector = %v, want os=linux only (placement rule free placement)", platform, cpod.NodeSelector)
		}
		if _, workerPinned := npod.NodeSelector["node-role.kubernetes.io/worker"]; npod.NodeSelector["kubernetes.io/os"] != "linux" || !workerPinned {
			t.Errorf("platform %s: node DS nodeSelector = %v, want os=linux + worker-pin (canonical)", platform, npod.NodeSelector)
		}
		if len(cpod.Tolerations) != 3 || len(npod.Tolerations) != 1 {
			t.Errorf("tolerations: controller %d / node %d, want 3/1", len(cpod.Tolerations), len(npod.Tolerations))
		}

		// Node pod plumbing.
		if !npod.HostPID || !npod.HostNetwork || !npod.HostIPC || ptrToTrue(npod.AutomountServiceAccountToken) {
			t.Error("node pod must set hostPID/hostNetwork/hostIPC true and automountServiceAccountToken false")
		}
		tokenMounts := 0
		for _, c := range npod.Containers {
			for _, m := range c.VolumeMounts {
				if m.Name == "kube-api-access-ngxstorage" {
					tokenMounts++
					if !m.ReadOnly || m.MountPath != "/var/run/secrets/kubernetes.io/serviceaccount" {
						t.Error("projected token mount must be readOnly at the serviceaccount path")
					}
					if c.Name != fcName {
						t.Errorf("projected token volume may only mount on the driver container, got %q", c.Name)
					}
				}
			}
			if c.ImagePullPolicy != corev1.PullIfNotPresent {
				t.Errorf("node container %q imagePullPolicy = %q, want IfNotPresent (source-bound raw)", c.Name, c.ImagePullPolicy)
			}
		}
		if tokenMounts != 1 {
			t.Errorf("projected token volume mounted %d times, want exactly 1", tokenMounts)
		}

		// hostPath types + propagation.
		wantHostPath := map[string]struct {
			path  string
			type_ corev1.HostPathType
			prop  corev1.MountPropagationMode
		}{
			"socket-dir":       {"/var/lib/kubelet/plugins/" + fcDriverName, corev1.HostPathDirectoryOrCreate, ""},
			"registration-dir": {"/var/lib/kubelet/plugins_registry", corev1.HostPathDirectory, ""},
			"kubelet-dir":      {"/var/lib/kubelet", corev1.HostPathDirectory, corev1.MountPropagationBidirectional},
			"host-root":        {"/", corev1.HostPathDirectory, corev1.MountPropagationBidirectional},
			"host-dev":         {"/dev", corev1.HostPathDirectory, ""},
		}
		for _, v := range npod.Volumes {
			want, ok := wantHostPath[v.Name]
			if !ok {
				continue
			}
			if v.HostPath == nil || v.HostPath.Path != want.path || v.HostPath.Type == nil || *v.HostPath.Type != want.type_ {
				t.Errorf("node volume %q hostPath = %+v, want %s (%s)", v.Name, v.HostPath, want.path, want.type_)
			}
			if want.prop == "" {
				continue
			}
			mounted := false
			for _, c := range npod.Containers {
				for _, m := range c.VolumeMounts {
					if m.Name == v.Name {
						mounted = true
						if m.MountPropagation == nil || *m.MountPropagation != want.prop {
							t.Errorf("node volume %q must mount Bidirectional", v.Name)
						}
					}
				}
			}
			if !mounted {
				t.Errorf("node volume %q is never mounted", v.Name)
			}
		}

		// Privilege boundary: privileged ONLY on the node driver; every
		// controller container restricted (source-bound raw security patch).
		privCount := 0
		for _, c := range npod.Containers {
			if c.SecurityContext != nil && ptrToTrue(c.SecurityContext.Privileged) {
				privCount++
				if c.Name != fcName {
					t.Errorf("privileged container %q: only the node driver may be privileged", c.Name)
				}
				caps := c.SecurityContext.Capabilities
				if caps == nil || len(caps.Add) != 2 || caps.Add[0] != "SYS_ADMIN" || caps.Add[1] != "SYS_CHROOT" {
					t.Errorf("node driver capabilities = %+v, want [SYS_ADMIN SYS_CHROOT]", caps)
				}
			}
		}
		if privCount != 1 {
			t.Errorf("privileged containers = %d, want exactly 1 (node driver)", privCount)
		}
		for _, c := range cpod.Containers {
			sc := c.SecurityContext
			if sc == nil || ptrToTrue(sc.AllowPrivilegeEscalation) || !ptrToTrue(sc.ReadOnlyRootFilesystem) ||
				sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
				ptrToTrue(sc.Privileged) {
				t.Errorf("controller container %q must carry the raw restricted securityContext, got %+v", c.Name, sc)
			}
		}

		// CSI identity.
		var drv *storagev1.CSIDriver
		for _, obj := range p.Objects() {
			if d, ok := obj.(*storagev1.CSIDriver); ok {
				drv = d
			}
		}
		if drv == nil || drv.Name != fcDriverName ||
			!ptrToTrue(drv.Spec.AttachRequired) || drv.Spec.FSGroupPolicy == nil || *drv.Spec.FSGroupPolicy != storagev1.FileFSGroupPolicy {
			t.Errorf("CSIDriver = %+v, want %s attachRequired + fsGroupPolicy File (fsGroupPolicy File raw contract value)", drv, fcDriverName)
		}
	}
}
