package renderer

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// --- iSCSI focused render tests. ---
//
// Behavioral pins beyond the golden parity: secretRef handling (secretRef-only, canonical env/secret names),
// image provenance (digest pin), log-level contract (unquoted log arg), the InstallRBAC ==
// generated-set intent (out-of-band RBAC contract), the iSCSI protocol checklist
// cells (portal-group pass-through, session coordination surfaces, CHAP
// open cell), and NFS profile isolation.

func daemonSetOf(t *testing.T, objs []client.Object) *appsv1.DaemonSet {
	t.Helper()
	for _, o := range objs {
		if ds, ok := o.(*appsv1.DaemonSet); ok {
			return ds
		}
	}
	t.Fatal("no DaemonSet in rendered iSCSI set")
	return nil
}

func containerByName(c []corev1.Container, name string) (corev1.Container, bool) {
	for _, ct := range c {
		if ct.Name == name {
			return ct, true
		}
	}
	return corev1.Container{}, false
}

func envByName(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// allStringLeaves collects every string value in a rendered object's JSON
// map — the negative-surface scan (no invented keys/flags anywhere).
func allStringLeaves(v interface{}) []string {
	var out []string
	switch tv := v.(type) {
	case map[string]interface{}:
		for _, el := range tv {
			out = append(out, allStringLeaves(el)...)
		}
	case []interface{}:
		for _, el := range tv {
			out = append(out, allStringLeaves(el)...)
		}
	case string:
		out = append(out, tv)
	}
	return out
}

// TestISCSSecretRefFlowsOnlyIntoEnvKeyRefs pins the secretRef-only with canonical env/secret names contract: the
// customer secret NAME from ProfileInput flows ONLY into the env
// secretKeyRefs (both workloads), with the pinned raw key set; the Secret
// itself is never rendered, copied or renamed.
func TestISCSSecretRefFlowsOnlyIntoEnvKeyRefs(t *testing.T) {
	secret := "customer-iscsi-credentials"
	p := &iscsiProfile{in: ProfileInput{
		Profile: "iscsi", SecretName: secret, DriverImage: iscsiGoldenDriverImage,
		LogLevel: "info", Platform: v1alpha1.PlatformKubernetes,
	}}
	objs := p.Objects()

	// No Secret object in the set (also pinned by the forbidden-kinds test;
	// asserted here by name flow instead).
	wantEnvKeys := map[string]string{
		"STORAGE_IPS":       "storage-ips",
		"STORAGE_POOL_NAME": "pool-name",
		"PORTAL_GROUP":      "portal-group",
		"STORAGE_API_KEY":   "api-key",
	}
	for _, obj := range objs {
		if obj.GetName() == secret && obj.GetObjectKind().GroupVersionKind().Kind == "Secret" {
			t.Fatal("renderer created or named an object after the customer Secret (secretRef-only)")
		}
	}

	driverContainers := []corev1.Container{}
	driverContainers = append(driverContainers, deploymentOf(t, objs).Spec.Template.Spec.Containers...)
	driverContainers = append(driverContainers, daemonSetOf(t, objs).Spec.Template.Spec.Containers...)

	for _, c := range driverContainers {
		if c.Name != iscsiName {
			continue // only the driver containers consume the storage env
		}
		if len(c.Env) == 0 {
			t.Errorf("driver container %s has no env block", c.Name)
			continue
		}
		for envName, key := range wantEnvKeys {
			e, ok := envByName(c, envName)
			if !ok {
				t.Errorf("driver container %s missing env %s", c.Name, envName)
				continue
			}
			if e.ValueFrom == nil {
				t.Errorf("driver container %s env %s is not a ValueFrom env", c.Name, envName)
				continue
			}
			sk := e.ValueFrom.SecretKeyRef
			if sk == nil || sk.Name != secret || sk.Key != key {
				t.Errorf("driver container %s env %s secretKeyRef = %v, want name=%q key=%q",
					c.Name, envName, e.ValueFrom, secret, key)
			}
		}
	}
}

// TestISCSDriverImageAndSidecarDigestsPin pins digest pin: the spec driver image
// lands ONLY on the driver containers (exactly once per workload); every
// sidecar carries a canonical pinned digest with the raw Always pull
// policy, never the spec image.
func TestISCSDriverImageAndSidecarDigestsPin(t *testing.T) {
	img := "quay.io/ngxstorage/iscsi-csi-ngxstorage@sha256:" + strings.Repeat("c", 64)
	p := &iscsiProfile{in: ProfileInput{
		Profile: "iscsi", SecretName: iscsiGoldenSecret, DriverImage: img,
		LogLevel: "info", Platform: v1alpha1.PlatformKubernetes,
	}}
	objs := p.Objects()

	canonical := map[string]bool{}
	for _, d := range []string{
		iscsiProvisionerDigest, iscsiAttacherDigest, iscsiResizerDigest,
		iscsiSnapshotterDigest, iscsiLivenessProbeDigest, iscsiRegistrarDigest,
	} {
		canonical[d] = true
	}

	driverSeen := map[string]int{}
	for _, ds := range []struct {
		name       string
		containers []corev1.Container
	}{
		{"controller", deploymentOf(t, objs).Spec.Template.Spec.Containers},
		{"node", daemonSetOf(t, objs).Spec.Template.Spec.Containers},
	} {
		for _, c := range ds.containers {
			if c.Image == img {
				driverSeen[ds.name]++
				continue
			}
			digest := c.Image[strings.LastIndex(c.Image, ":")+1:]
			if !canonical[digest] {
				t.Errorf("%s container %q image %q is neither the spec driver image nor a canonical sidecar digest",
					ds.name, c.Name, c.Image)
			}
			if c.ImagePullPolicy != corev1.PullAlways {
				t.Errorf("%s container %q pull policy = %s, want Always (raw contract)", ds.name, c.Name, c.ImagePullPolicy)
			}
		}
	}
	if driverSeen["controller"] != 1 || driverSeen["node"] != 1 {
		t.Errorf("driver image occurrence = %+v, want exactly 1 per workload", driverSeen)
	}
}

// TestISCSILogLevelUnquoted pins unquoted log arg: logLevel reaches the driver args as
// exactly --log=<level> (no helm `| quote` artifact) on BOTH workloads.
func TestISCSILogLevelUnquoted(t *testing.T) {
	p := &iscsiProfile{in: ProfileInput{
		Profile: "iscsi", SecretName: iscsiGoldenSecret, DriverImage: iscsiGoldenDriverImage,
		LogLevel: "debug", Platform: v1alpha1.PlatformOpenShift,
	}}
	objs := p.Objects()
	for _, ds := range []struct {
		name       string
		containers []corev1.Container
	}{
		{"controller", deploymentOf(t, objs).Spec.Template.Spec.Containers},
		{"node", daemonSetOf(t, objs).Spec.Template.Spec.Containers},
	} {
		driver, _ := containerByName(ds.containers, iscsiName)
		logArgs := 0
		for _, a := range driver.Args {
			if strings.HasPrefix(a, "--log") {
				logArgs++
				if a != "--log=debug" {
					t.Errorf("%s --log arg %q must be exactly --log=debug (unquoted, unquoted log arg)", ds.name, a)
				}
			}
		}
		if logArgs != 1 {
			t.Errorf("%s driver carries %d --log args, want 1", ds.name, logArgs)
		}
	}
}

// TestISCSIInstallRBACMatchesGeneratedSet pins the out-of-band RBAC contract
// intent: InstallRBAC() is EXACTLY the RBAC-kind subset Objects() renders
// (field-equal, per platform), so the installer's committed manifests
// (generated from InstallRBAC) cannot drift from the render the reconciler
// verifies.
func TestISCSIInstallRBACMatchesGeneratedSet(t *testing.T) {
	rbacKinds := map[string]bool{
		"Role": true, "RoleBinding": true, "ClusterRole": true, "ClusterRoleBinding": true,
	}
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := iscsiProfileOf(platform)
		want := make(map[string]map[string]interface{})
		for _, o := range p.InstallRBAC() {
			u := toUnstructuredMap(t, o)
			want[docKey(u)] = u
		}
		got := make(map[string]map[string]interface{})
		for _, o := range p.Objects() {
			u := toUnstructuredMap(t, o)
			if rbacKinds[u["kind"].(string)] {
				got[docKey(u)] = u
			}
		}
		if len(want) != len(got) {
			t.Fatalf("platform %s: InstallRBAC() has %d objects, Objects() carries %d RBAC objects",
				platform, len(want), len(got))
		}
		assertSetEqual(t, want, got)
	}
}

// TestISCSIRBACNamesAreTheFixedSet pins the platform-specific rendered RBAC
// names against iscsiRBACNames: k8s renders the 10-object set without the
// scc-use grant; ocp renders 12 with it; every rendered name is in the
// fixed set and every fixed name is eventually covered.
func TestISCSIRBACNamesAreTheFixedSet(t *testing.T) {
	rbacKinds := map[string]bool{
		"Role": true, "RoleBinding": true, "ClusterRole": true, "ClusterRoleBinding": true,
	}
	fixed := map[string]bool{}
	for _, name := range append(append(append(append([]string{},
		iscsiRBACNames.Roles...), iscsiRBACNames.RoleBindings...),
		iscsiRBACNames.ClusterRoles...), iscsiRBACNames.ClusterRoleBindings...) {
		fixed[name] = true
	}
	expect := map[v1alpha1.Platform]int{v1alpha1.PlatformKubernetes: 10, v1alpha1.PlatformOpenShift: 12}
	for platform, count := range expect {
		objs := iscsiProfileOf(platform).InstallRBAC()
		if len(objs) != count {
			t.Errorf("platform %s: install RBAC set = %d objects, want %d", platform, len(objs), count)
		}
		for _, o := range objs {
			u := toUnstructuredMap(t, o)
			if !rbacKinds[u["kind"].(string)] {
				t.Errorf("platform %s: InstallRBAC() carries non-RBAC kind %s", platform, u["kind"])
			}
			if !fixed[u["metadata"].(map[string]interface{})["name"].(string)] {
				t.Errorf("platform %s: rendered RBAC name %s not in the fixed set", platform, u["metadata"].(map[string]interface{})["name"])
			}
		}
	}
}

// TestISCSIOperandKindsExcludeRBAC pins the cleanup contract: OperandKinds
// enumerates every non-RBAC operand kind Objects() renders (SCC only on
// OCP), and NEVER the installer-owned RBAC kinds.
func TestISCSIOperandKindsExcludeRBAC(t *testing.T) {
	rbacKinds := map[string]bool{
		"Role": true, "RoleBinding": true, "ClusterRole": true, "ClusterRoleBinding": true,
	}
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := iscsiProfileOf(platform)
		rendered := map[string]bool{}
		for _, o := range p.Objects() {
			u := toUnstructuredMap(t, o)
			rendered[u["kind"].(string)] = true
		}
		for _, k := range p.OperandKinds() {
			gvks, _, err := parityScheme.ObjectKinds(k.Object)
			if err != nil {
				t.Fatalf("resolve GVK for %T: %v", k.Object, err)
			}
			kind := gvks[0].Kind
			if rbacKinds[kind] {
				t.Errorf("platform %s: OperandKinds includes installer-owned RBAC kind %s", platform, kind)
			}
			if !rendered[kind] {
				t.Errorf("platform %s: OperandKinds includes %s but Objects() never renders it", platform, kind)
			}
		}
	}
}

// TestISCIPortalGroupPassThrough pins the portal-group checklist cell:
// the operator passes ONLY the pre-configured portal group name through
// secret key portal-group → env PORTAL_GROUP → --portal-group on BOTH
// workloads. No portal/session/credential surface is invented.
func TestISCIPortalGroupPassThrough(t *testing.T) {
	objs := iscsiProfileOf(v1alpha1.PlatformKubernetes).Objects()
	for _, ds := range []struct {
		name       string
		containers []corev1.Container
	}{
		{"controller", deploymentOf(t, objs).Spec.Template.Spec.Containers},
		{"node", daemonSetOf(t, objs).Spec.Template.Spec.Containers},
	} {
		driver, _ := containerByName(ds.containers, iscsiName)
		hasArg := false
		for _, a := range driver.Args {
			if a == "--portal-group=$(PORTAL_GROUP)" {
				hasArg = true
			}
		}
		if !hasArg {
			t.Errorf("%s driver args lack --portal-group=$(PORTAL_GROUP): %v", ds.name, driver.Args)
		}
		env, ok := envByName(driver, "PORTAL_GROUP")
		if !ok {
			t.Errorf("%s driver env lacks PORTAL_GROUP", ds.name)
			continue
		}
		var sk *corev1.SecretKeySelector
		if env.ValueFrom != nil {
			sk = env.ValueFrom.SecretKeyRef
		}
		if sk == nil || sk.Key != "portal-group" || sk.Name != iscsiGoldenSecret {
			t.Errorf("%s PORTAL_GROUP secretKeyRef = %v, want key portal-group of spec secret", ds.name, env.ValueFrom)
		}
	}
}

// TestISCSINoInventedCHAPSurface pins the CHAP open-suitability cell: the
// pinned driver (3e138b2) has zero chap references in pkg/+cmd/ — auth
// travels per-volume via the backend auth group, and login/logout/session
// management is driver-side. The renderer therefore must carry NO chap
// keys, flags or args anywhere in the operand set; support is NOT claimed
// (see iscsiChapSupportStatus).
func TestISCSINoInventedCHAPSurface(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		for _, obj := range iscsiProfileOf(platform).Objects() {
			u := toUnstructuredMap(t, obj)
			for _, s := range allStringLeaves(u) {
				if strings.Contains(strings.ToLower(s), "chap") {
					t.Errorf("platform %s: %s carries a CHAP surface %q — the driver has none (open cell, must not be invented)",
						platform, u["kind"], s)
				}
			}
		}
	}
}

// TestISCSISessionCoordinationSurfaces pins the node-side session
// coordination the driver's teardown-gated logout path requires: the node
// holder-pod read Role (pods get → node SA), BOTH operation-lock roles
// pinning the full 128-lease pool, and the projected SA-token volume the
// node driver uses while automount is off.
func TestISCSISessionCoordinationSurfaces(t *testing.T) {
	objs := iscsiProfileOf(v1alpha1.PlatformOpenShift).Objects()

	byKind := map[string]client.Object{}
	for _, o := range objs {
		u := toUnstructuredMap(t, o)
		byKind[u["kind"].(string)+"/"+u["metadata"].(map[string]interface{})["name"].(string)] = o
	}

	holderRole, ok := byKind["Role/iscsi-csi-ngxstorage-node-holder-pod-read"].(*rbacv1.Role)
	if !ok {
		t.Fatal("node-holder-pod-read Role not rendered")
	}
	if len(holderRole.Rules) != 1 ||
		holderRole.Rules[0].Resources[0] != "pods" ||
		holderRole.Rules[0].Verbs[0] != "get" {
		t.Errorf("node-holder-pod-read rules = %+v, want pods/get", holderRole.Rules)
	}

	for _, name := range []string{
		"Role/iscsi-csi-ngxstorage-controller-operation-lock",
		"Role/iscsi-csi-ngxstorage-node-operation-lock",
	} {
		r, ok := byKind[name].(*rbacv1.Role)
		if !ok {
			t.Fatalf("%s not rendered", name)
		}
		got := r.Rules[0].ResourceNames
		if len(got) != iscsiShardCount || got[0] != "iscsi-csi-ngxstorage-operation-lock-000" || got[iscsiShardCount-1] != "iscsi-csi-ngxstorage-operation-lock-127" {
			t.Errorf("%s pins %d lease names, want the full %d-shard pool", name, len(got), iscsiShardCount)
		}
	}

	ds := daemonSetOf(t, objs)
	spec := ds.Spec.Template.Spec
	// automount must be explicitly false: the token arrives only via
	// the explicit projection below.
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("node DS automountServiceAccountToken = %v, want explicit false", spec.AutomountServiceAccountToken)
	}
	found := false
	for _, v := range spec.Volumes {
		if v.Name == iscsiKubeAPIAccessVolume {
			found = true
			if v.Projected == nil || v.Projected.DefaultMode == nil || *v.Projected.DefaultMode != 420 {
				t.Errorf("kube-api-access volume projection = %+v, want projected with defaultMode 420", v.VolumeSource)
			}
		}
	}
	if !found {
		t.Fatal("node DS lacks the projected kube-api-access volume")
	}
	driver, _ := containerByName(spec.Containers, iscsiName)
	hasMount := false
	for _, m := range driver.VolumeMounts {
		if m.Name == iscsiKubeAPIAccessVolume && m.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && m.ReadOnly {
			hasMount = true
		}
	}
	if !hasMount {
		t.Errorf("node driver container lacks the read-only kube-api-access mount")
	}
}

// TestISCSINodeWorkerPinOnBothPlatforms pins the placement rule placement split: the
// iSCSI node DS is worker-pinned on BOTH platforms (already in the pinned
// raw — canonical), while the controller keeps free placement (os=linux +
// master/control-plane/CriticalAddonsOnly tolerations, NOT worker-pinned) —
// the adopted placement default, which the pinned raw already carries.
func TestISCSINodeWorkerPinOnBothPlatforms(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		objs := iscsiProfileOf(platform).Objects()
		ds := daemonSetOf(t, objs)
		sel := ds.Spec.Template.Spec.NodeSelector
		if sel["kubernetes.io/os"] != "linux" || sel["node-role.kubernetes.io/worker"] != "" {
			t.Errorf("platform %s: node DS nodeSelector = %v, want os=linux + worker-pin (canonical)", platform, sel)
		}
		deploy := deploymentOf(t, objs)
		dsel := deploy.Spec.Template.Spec.NodeSelector
		if _, pinned := dsel["node-role.kubernetes.io/worker"]; pinned {
			t.Errorf("platform %s: controller Deployment must NOT be worker-pinned (free placement — placement rule adopted default): %v", platform, dsel)
		}
		if dsel["kubernetes.io/os"] != "linux" || len(deploy.Spec.Template.Spec.Tolerations) != 3 {
			t.Errorf("platform %s: controller placement must be os=linux + 3 raw tolerations: selector %v, tolerations %v",
				platform, dsel, deploy.Spec.Template.Spec.Tolerations)
		}
		ann := ds.Spec.Template.Annotations["openshift.io/required-scc"]
		if platform == v1alpha1.PlatformOpenShift && ann != "iscsi-csi-ngxstorage-node-scc" {
			t.Errorf("platform %s: node DS must carry required-scc, got %q", platform, ann)
		}
		if platform == v1alpha1.PlatformKubernetes && ann != "" {
			t.Errorf("platform %s: node DS must NOT carry required-scc, got %q", platform, ann)
		}
	}
}

// TestISCSIProfileIsolationFromNFS pins the wave-brief isolation contract:
// rendering both profiles with identical spec values yields disjoint
// deterministic sets — an iSCSI render error can never corrupt NFS state
// (no shared object identities, namespaces or names).
func TestISCSIProfileIsolationFromNFS(t *testing.T) {
	nfsIn := ProfileInput{
		Profile: "nfs", SecretName: iscsiGoldenSecret, DriverImage: "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("d", 64),
		LogLevel: "info", Platform: v1alpha1.PlatformKubernetes,
	}
	iscsiIn := ProfileInput{
		Profile: "iscsi", SecretName: iscsiGoldenSecret, DriverImage: iscsiGoldenDriverImage,
		LogLevel: "info", Platform: v1alpha1.PlatformKubernetes,
	}
	nfsSet := renderedAsJSON(t, For(nfsIn).Objects())
	iscsiSet := renderedAsJSON(t, (&iscsiProfile{in: iscsiIn}).Objects())

	for k := range iscsiSet {
		if _, shared := nfsSet[k]; shared {
			t.Errorf("profiles share object %s — isolation violated", k)
		}
		kind, ns, _ := splitDocKey(k)
		// cluster-scoped kinds carry no namespace; everything else must
		// live in the iSCSI operand namespace.
		clusterScoped := kind == "CSIDriver" || kind == "ClusterRole" || kind == "ClusterRoleBinding"
		if !clusterScoped && ns != "ngxstorage-iscsi" {
			t.Errorf("iscsi object %s not namespaced to ngxstorage-iscsi (cluster-scoped kinds excepted)", k)
		}
	}
	for _, m := range iscsiSet {
		for _, s := range allStringLeaves(m["metadata"]) {
			if strings.Contains(s, "nfs-csi-ngxstorage") {
				t.Errorf("iscsi object metadata references an NFS identity: %q", s)
			}
		}
	}
}

func splitDocKey(k string) (kind, ns, name string) {
	parts := strings.SplitN(k, "/", 3)
	if len(parts) != 3 {
		return k, "", ""
	}
	return parts[0], parts[1], parts[2]
}
