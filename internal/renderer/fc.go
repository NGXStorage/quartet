package renderer

import (
	"fmt"

	apps "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fcProfile renders the canonical FC operand set pinned from the CLEAN
// driver commit (deploy/manifest/{kubernetes,openshift}, kustomize v5.7.1 —
// see the golden headers in testdata/fc-*-golden.yaml) with the approved
// transforms applied and everything else SOURCE-BOUND to the raw render.
// Explicit struct — no plugin mechanism.
//
// Registry wiring (For / ProvisionerIdentity / OperandRBACNames) is live in
// profile.go — the golden-parity tests still construct the profile
// explicitly, and the registry contract tests pin the dispatch.
type fcProfile struct{ in ProfileInput }

func (p *fcProfile) Name() string      { return p.in.Profile }
func (p *fcProfile) Namespace() string { return "ngxstorage-" + p.in.Profile }

// WorkloadTargets implements Profile.WorkloadTargets: the names Objects()
// renders for THIS profile's controller Deployment and node DaemonSet
// (fcName + "-controller" / "-node").
func (p *fcProfile) WorkloadTargets() (string, string) {
	return fcName + "-controller", fcName + "-node"
}

// --- Canonical constants pinned from the raw FC manifests; the renderer
// --- must not deviate (golden parity). ---

const (
	fcName        = "fc-csi-ngxstorage"     // driver container + lease/coordination prefix
	fcDriverName  = "fc.csi.ngxstorage.com" // CSIDriver name
	fcSidecarRepo = "quay.io/ngxstorage/csi-sidecars"

	// Sidecar digests are renderer CONSTANTS (the spec carries the driver
	// image only). Values are the canonical set pinned from the raw
	// renders (identical across NFS/FC/iSCSI).
	fcProvisionerDigest   = "a4b0b1a37605b7b04a293e136edf7006ec1786a8eb3f4e5a945f81d667dcc371"
	fcAttacherDigest      = "b9dc9a714a484ccdeeb6f86d88d4db9b7a5ecfc5a55da6db3a60bb3fa33c278a"
	fcResizerDigest       = "ea1d25e23479000c7e8eeb92d827df66258df4e482ca054c5e7ce3fc0f5c41a5"
	fcSnapshotterDigest   = "42af0929bcd60a43499825c078a60ff0534e08af8fbeb283aa391be40feb9f3e"
	fcLivenessProbeDigest = "06da0d5b8908072f2e4522692aee8dc119fba7247a9658497e1153992cd777e9"
	fcRegistrarDigest     = "f9de845b170155199f2a2a3f9531cf13d78e31235e9db6b6582a8b0db0a50dad"

	fcLeasePrefix = "fc-csi-ngxstorage"
	fcShardCount  = 128

	// Source-bound raw ports: the controller driver exposes containerPort
	// 9808 (no probe — helm adds one, recorded divergence); the node driver
	// carries livenessProbe on 9811 (raw).
	fcControllerHealthPort = 9808
	fcNodeHealthPort       = 9811
)

func fcSidecar(image, digest string) string {
	return fcSidecarRepo + "/" + image + "@sha256:" + digest
}

// fcRBACNames is the fixed FC operand RBAC name set. Every name is produced
// by Objects() — the scc-use role/binding exist only on OpenShift, but the
// RBAC resourceNames pin is platform-agnostic (see nfsRBACNames rationale).
// Keep in lockstep with the Objects() builders; the install-manifest parity
// pins the committed set against it.
var fcRBACNames = OperandRBAC{
	Roles: []string{
		"fc-csi-ngxstorage-controller-operation-lock",
		"fc-csi-ngxstorage-node-operation-lock",
		"fc-csi-ngxstorage-leader-election",
		"fc-csi-ngxstorage-node-holder-pod-read",
		"fc-csi-ngxstorage-node-scc-use",
	},
	RoleBindings: []string{
		"fc-csi-ngxstorage-controller-operation-lock",
		"fc-csi-ngxstorage-node-operation-lock",
		"fc-csi-ngxstorage-leader-election",
		"fc-csi-ngxstorage-node-holder-pod-read",
		"fc-csi-ngxstorage-node-scc-use",
	},
	ClusterRoles:        []string{"fc-csi-ngxstorage-controller"},
	ClusterRoleBindings: []string{"fc-csi-ngxstorage-controller"},
}

func (p *fcProfile) appLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      fcName,
		"app.kubernetes.io/component": component,
	}
}

// Objects returns the deterministic FC operand set in the reconciler's
// apply order: RBAC/SA → platform policy (SCC, OCP only) → coordination
// (CM+Lease) → CSIDriver → Service → Deployment → DaemonSet. The Namespace
// is NOT here (the reconciler ensures it); Secret/SC/VSC/test-connection
// are absent by contract.
func (p *fcProfile) Objects() []client.Object {
	ns := p.Namespace()
	ocp := p.in.Platform == v1alpha1.PlatformOpenShift

	objs := make([]client.Object, 0, 14+fcShardCount+4)

	// --- RBAC / ServiceAccounts. The RBAC kinds are installer-owned
	// --- (out-of-band provisioning — see InstallRBAC); the SAs remain
	// --- operator-applied.
	objs = append(objs,
		&corev1.ServiceAccount{ObjectMeta: meta("fc-csi-ngxstorage-controller", ns)},
		&corev1.ServiceAccount{ObjectMeta: meta("fc-csi-ngxstorage-node", ns)},
	)
	objs = append(objs, p.installRBAC()...)

	// --- Platform policy: SCC + use grant, OCP ONLY.
	if ocp {
		objs = append(objs, p.scc(ns))
	}

	// --- Coordination: immutable CM + 128-lease pool. ---
	objs = append(objs, p.coordinationConfigMap(ns))
	objs = append(objs, fcLeasePool(ns)...)

	// --- Driver plumbing. ---
	objs = append(objs, p.csiDriver(), p.healthService(ns), p.controllerDeployment(ns), p.nodeDaemonSet(ns))
	return objs
}

// installRBAC is the FC operand RBAC set (Profile.InstallRBAC): the
// controller ClusterRole, the four namespaced Roles (controller-operation-
// lock, node-operation-lock, leader-election, node-holder-pod-read), and on
// OpenShift the node SCC use grant. Roles precede their bindings; the order
// is the installer-apply order the committed manifest generation follows.
func (p *fcProfile) installRBAC() []client.Object {
	ns := p.Namespace()
	ocp := p.in.Platform == v1alpha1.PlatformOpenShift
	objs := []client.Object{
		p.clusterRole(),
		p.controllerOperationLockRole(ns),
		p.nodeOperationLockRole(ns),
		p.leaderElectionRole(ns),
		p.nodeHolderPodReadRole(ns),
	}
	if ocp {
		objs = append(objs, p.sccUseRole(ns))
	}
	objs = append(objs,
		p.controllerOperationLockRoleBinding(ns),
		p.nodeOperationLockRoleBinding(ns),
		p.leaderElectionRoleBinding(ns),
		p.nodeHolderPodReadRoleBinding(ns),
	)
	if ocp {
		objs = append(objs, p.sccUseRoleBinding(ns))
	}
	objs = append(objs, p.clusterRoleBinding(ns))
	return objs
}

// InstallRBAC implements Profile.InstallRBAC — the installer-owned RBAC set
// (out-of-band provisioning). The committed manifests are generated from
// exactly this function (hack/generate-install-rbac → config/operands/fc/);
// the renderer is wired into the registry and the install set is committed
// and parity-pinned.
func (p *fcProfile) InstallRBAC() []client.Object { return p.installRBAC() }

// OperandKinds lists the FC operand kinds for owned-object enumeration
// (disable/delete cleanup). The SCC kind is OCP-only and carried
// unstructured (normalized comparison, envtest cannot serve security.openshift.io). The
// four RBAC kinds are ABSENT: operand RBAC is installer-owned out-of-band
// (InstallRBAC), so cleanup never enumerates or deletes it.
func (p *fcProfile) OperandKinds() []OperandKind {
	kinds := []OperandKind{
		kind(&corev1.ServiceAccount{}, &corev1.ServiceAccountList{}, false),
		kind(&corev1.ConfigMap{}, &corev1.ConfigMapList{}, false),
		kind(&coordinationv1.Lease{}, &coordinationv1.LeaseList{}, false),
		kind(&corev1.Service{}, &corev1.ServiceList{}, false),
		kind(&apps.Deployment{}, &apps.DeploymentList{}, false),
		kind(&apps.DaemonSet{}, &apps.DaemonSetList{}, false),
		kind(&storagev1.CSIDriver{}, &storagev1.CSIDriverList{}, true),
	}
	if p.in.Platform == v1alpha1.PlatformOpenShift {
		sccList := &unstructured.UnstructuredList{}
		sccList.SetGroupVersionKind(SCCGVK)
		kinds = append(kinds, kind(sccUnstructured(SCCGVK), sccList, true))
	}
	return kinds
}

// clusterRole is the FC controller ClusterRole — the 12-rule set from the
// pinned raw render. It never carried a volumeattributesclasses rule
// (unlike NFS raw), so no such transform applies here.
func (p *fcProfile) clusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "fc-csi-ngxstorage-controller"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"persistentvolumes"},
				Verbs: []string{"get", "list", "watch", "create", "delete", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"},
				Verbs: []string{"get", "list", "watch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims/status"},
				Verbs: []string{"patch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"events"},
				Verbs: []string{"list", "watch", "create", "update", "patch"}},
			// Source-bound raw note: external-resizer needs pods
			// get/list/watch for NodeExpand target resolution.
			{APIGroups: []string{""}, Resources: []string{"pods"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses", "csinodes"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattachments"},
				Verbs: []string{"get", "list", "watch", "patch"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattachments/status"},
				Verbs: []string{"patch"}},
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshotclasses", "volumesnapshots"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshotcontents"},
				Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshotcontents/status"},
				Verbs: []string{"update", "patch"}},
		},
	}
}

func (p *fcProfile) controllerOperationLockRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("fc-csi-ngxstorage-controller-operation-lock", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"coordination.k8s.io"},
			Resources:     []string{"leases"},
			ResourceNames: fcLeasePoolNames(),
			Verbs:         []string{"get", "update", "patch"},
		}},
	}
}

func (p *fcProfile) nodeOperationLockRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("fc-csi-ngxstorage-node-operation-lock", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"coordination.k8s.io"},
			Resources:     []string{"leases"},
			ResourceNames: fcLeasePoolNames(),
			Verbs:         []string{"get", "update", "patch"},
		}},
	}
}

func (p *fcProfile) leaderElectionRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("fc-csi-ngxstorage-leader-election", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
}

// nodeHolderPodReadRole is the FC-specific exception grant: the node driver
// may only GET the 11.1 destructive-holder Pod (source-bound raw comment).
func (p *fcProfile) nodeHolderPodReadRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("fc-csi-ngxstorage-node-holder-pod-read", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get"},
		}},
	}
}

func (p *fcProfile) controllerOperationLockRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("fc-csi-ngxstorage-controller-operation-lock", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "fc-csi-ngxstorage-controller-operation-lock"},
		Subjects: fcControllerSubject(ns),
	}
}

func (p *fcProfile) nodeOperationLockRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("fc-csi-ngxstorage-node-operation-lock", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "fc-csi-ngxstorage-node-operation-lock"},
		Subjects: fcNodeSubject(ns),
	}
}

func (p *fcProfile) leaderElectionRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("fc-csi-ngxstorage-leader-election", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "fc-csi-ngxstorage-leader-election"},
		Subjects: fcControllerSubject(ns),
	}
}

func (p *fcProfile) nodeHolderPodReadRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("fc-csi-ngxstorage-node-holder-pod-read", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "fc-csi-ngxstorage-node-holder-pod-read"},
		Subjects: fcNodeSubject(ns),
	}
}

func (p *fcProfile) clusterRoleBinding(ns string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fc-csi-ngxstorage-controller"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "fc-csi-ngxstorage-controller"},
		Subjects: fcControllerSubject(ns),
	}
}

func fcControllerSubject(ns string) []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: "fc-csi-ngxstorage-controller", Namespace: ns,
	}}
}

func fcNodeSubject(ns string) []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: "fc-csi-ngxstorage-node", Namespace: ns,
	}}
}

// --- OCP-only platform policy. ---

func (p *fcProfile) sccUseRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("fc-csi-ngxstorage-node-scc-use", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"security.openshift.io"},
			Resources:     []string{"securitycontextconstraints"},
			ResourceNames: []string{"fc-csi-ngxstorage-node-scc"},
			Verbs:         []string{"use"},
		}},
	}
}

func (p *fcProfile) sccUseRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("fc-csi-ngxstorage-node-scc-use", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "fc-csi-ngxstorage-node-scc-use"},
		Subjects: fcNodeSubject(ns),
	}
}

func (p *fcProfile) scc(ns string) *unstructured.Unstructured {
	// Source-bound to the pinned raw SCC. The grant fields are
	// TOP-LEVEL (not under spec), so build the Object map directly. The
	// metadata.namespace is the kustomize render artifact (SCC is cluster-
	// scoped; the API ignores it) — kept for 1:1 golden parity, same as NFS.
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "security.openshift.io/v1",
		"kind":       "SecurityContextConstraints",
		"metadata": map[string]interface{}{
			"name":      "fc-csi-ngxstorage-node-scc",
			"namespace": ns,
		},
		"allowHostDirVolumePlugin": true,
		"allowHostIPC":             true,
		"allowHostNetwork":         true,
		"allowHostPID":             true,
		"allowHostPorts":           false,
		"allowPrivilegeEscalation": true,
		"allowPrivilegedContainer": true,
		"allowedCapabilities":      []interface{}{"SYS_ADMIN", "SYS_CHROOT"},
		"fsGroup":                  map[string]interface{}{"type": "RunAsAny"},
		"groups":                   []interface{}{},
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
		"seLinuxContext":           map[string]interface{}{"type": "RunAsAny"},
		"supplementalGroups":       map[string]interface{}{"type": "RunAsAny"},
		"users":                    []interface{}{},
		"volumes":                  []interface{}{"hostPath", "downwardAPI", "projected", "emptyDir"},
	}}
}

// --- Coordination. ---

func (p *fcProfile) coordinationConfigMap(ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fcName + "-coordination-config",
			Namespace: ns,
			Labels:    p.appLabels("coordination-config"),
		},
		Immutable: ptr.To(true),
		Data: map[string]string{
			"lease-prefix": fcLeasePrefix,
			"shard-count":  "128",
		},
	}
}

func fcLeasePoolNames() []string {
	names := make([]string, fcShardCount)
	for i := range names {
		names[i] = fmt.Sprintf("%s-operation-lock-%03d", fcLeasePrefix, i)
	}
	return names
}

func fcLeasePool(ns string) []client.Object {
	labels := map[string]string{
		"app.kubernetes.io/name":      fcName,
		"app.kubernetes.io/component": "operation-lock",
	}
	out := make([]client.Object, fcShardCount)
	for i, name := range fcLeasePoolNames() {
		out[i] = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		}
	}
	return out
}

// --- Driver plumbing. ---

func (p *fcProfile) csiDriver() *storagev1.CSIDriver {
	return &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: fcDriverName},
		Spec: storagev1.CSIDriverSpec{
			AttachRequired: ptr.To(true),
			// The raw contract value is File; the Helm chart's None is a
			// recorded divergence — the chart-side fix awaits a decision
			// plus live fsGroup proof on both platforms. Do not "fix"
			// toward Helm without that proof.
			FSGroupPolicy:        ptr.To(storagev1.FileFSGroupPolicy),
			PodInfoOnMount:       ptr.To(false),
			VolumeLifecycleModes: []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent},
		},
	}
}

func (p *fcProfile) healthService(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fc-csi-ngxstorage-controller-health",
			Namespace: ns,
			Labels:    p.appLabels("controller-health"),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "healthz", Port: fcControllerHealthPort, TargetPort: intstrFromInt(fcControllerHealthPort)}},
			Selector: map[string]string{
				"app.kubernetes.io/name":      fcName,
				"app.kubernetes.io/component": "controller",
			},
		},
	}
}

// storageSecretEnv renders the canonical (Helm-set) env block from the
// spec secret name: STORAGE_* env names over the raw key set
// (storage-ips, pool-name, fc-target-names, api-key). FC-specific vs NFS:
// fc-target-names carries the pre-zoned one-target-per-LUN list the driver
// consumes; the operator only wires it, it never manages zoning.
func (p *fcProfile) storageSecretEnv() []corev1.EnvVar {
	ref := func(env, key string) corev1.EnvVar {
		return corev1.EnvVar{
			Name: env,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: p.in.SecretName},
					Key:                  key,
				},
			},
		}
	}
	return []corev1.EnvVar{
		ref("STORAGE_IPS", "storage-ips"),
		ref("STORAGE_POOL_NAME", "pool-name"),
		ref("FC_TARGET_NAMES", "fc-target-names"),
		ref("STORAGE_API_KEY", "api-key"),
	}
}

// coordinationDriverEnv is the shared coordination + pod-identity env block
// the driver containers carry in both workloads (source-bound raw).
func (p *fcProfile) coordinationDriverEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NGX_COORDINATION_LEASE_PREFIX", Value: fcLeasePrefix},
		{Name: "NGX_COORDINATION_SHARD_COUNT", Value: "128"},
		fieldRefEnv("POD_NAMESPACE", "metadata.namespace"),
		fieldRefEnv("POD_NAME", "metadata.name"),
		fieldRefEnv("POD_UID", "metadata.uid"),
	}
}

func (p *fcProfile) controllerDeployment(ns string) *apps.Deployment {
	labels := p.appLabels("controller")
	socketMount := []corev1.VolumeMount{{Name: "socket-dir", MountPath: "/csi"}}
	driverEnv := append([]corev1.EnvVar{
		{Name: "CSI_ENDPOINT", Value: "unix:///csi/csi.sock"},
	}, p.coordinationDriverEnv()...)
	driverEnv = append(driverEnv, p.storageSecretEnv()...)
	driverArgs := []string{
		"--role=controller",
		"--endpoint=$(CSI_ENDPOINT)",
		"--ip=$(STORAGE_IPS)",
		"--pool-name=$(STORAGE_POOL_NAME)",
		"--fc-target-names=$(FC_TARGET_NAMES)",
		"--api-key=$(STORAGE_API_KEY)",
		// Log level written UNQUOTED (spec.logLevel as-is).
		"--log=" + p.in.LogLevel,
	}

	return &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fc-csi-ngxstorage-controller", Namespace: ns, Labels: labels},
		Spec: apps.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: apps.DeploymentStrategy{
				Type: apps.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &apps.RollingUpdateDeployment{
					MaxSurge:       ptr.To(intstrFromInt(0)),
					MaxUnavailable: ptr.To(intstrFromInt(1)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "fc-csi-ngxstorage-controller",
					PriorityClassName:  "system-cluster-critical",
					// Controller placement: free placement (os=linux + the
					// master/control-plane/CriticalAddonsOnly tolerations),
					// matching the NFS/iSCSI raw behavior. The FC raw
					// worker-pin is a recorded divergence superseded by
					// that default; live scheduling acceptance on FC
					// clusters remains an open acceptance gate. The node
					// DS keeps the worker-pin (raw behavior of all three
					// drivers).
					NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
					Tolerations: []corev1.Toleration{
						{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
					},
					// Source-bound: raw carries NO pod securityContext
					// (helm's explicit empty one is a recorded divergence).
					Volumes: []corev1.Volume{{Name: "socket-dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					Containers: []corev1.Container{
						p.provisionerSidecar(socketMount),
						p.attacherSidecar(socketMount),
						p.resizerSidecar(socketMount),
						p.snapshotterSidecar(socketMount),
						p.livenessProbeSidecar(fcControllerHealthPort, socketMount),
						corev1.Container{
							Name:  fcName,
							Image: p.in.DriverImage, // driver image from spec
							// Source-bound: raw FC pins IfNotPresent on
							// EVERY container (NFS uses Always for the
							// driver — recorded, not copied).
							ImagePullPolicy: ifNotPresent,
							Args:            driverArgs,
							Env:             driverEnv,
							// Source-bound raw security patch: all six
							// controller containers are restricted.
							SecurityContext: restrictedSC(),
							// Source-bound: raw exposes the health port
							// WITHOUT a driver livenessProbe in the
							// controller (Helm adds the probe — recorded
							// divergence).
							Ports:        []corev1.ContainerPort{{ContainerPort: fcControllerHealthPort, Protocol: corev1.ProtocolTCP}},
							VolumeMounts: socketMount,
						},
					},
				},
			},
		},
	}
}

// --- Controller sidecars (source-bound raw args: NO
// --- --feature-gates=VolumeAttributesClass — helm adds it, recorded
// --- divergence; the FC sidecar digests predate that gate in this set). ---

func (p *fcProfile) provisionerSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.fcSidecar("external-provisioner", fcSidecar("csi-provisioner", fcProvisionerDigest),
		p.leaderElectionArgs(),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *fcProfile) attacherSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.fcSidecar("external-attacher", fcSidecar("csi-attacher", fcAttacherDigest),
		p.leaderElectionArgs(),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *fcProfile) resizerSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.fcSidecar("external-resizer", fcSidecar("csi-resizer", fcResizerDigest),
		p.leaderElectionArgs(),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *fcProfile) snapshotterSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.fcSidecar("external-snapshotter", fcSidecar("csi-snapshotter", fcSnapshotterDigest),
		p.leaderElectionArgs(),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

// livenessProbeSidecar uses the raw FC flag style --health-port=<port>
// (helm's --http-endpoint/--probe-timeout/--v=2 set is a recorded divergence;
// flag equivalence is an open live-proof cell).
func (p *fcProfile) livenessProbeSidecar(port int, mounts []corev1.VolumeMount) corev1.Container {
	return p.fcSidecar("liveness-probe", fcSidecar("livenessprobe", fcLivenessProbeDigest),
		[]string{"--csi-address=/csi/csi.sock", fmt.Sprintf("--health-port=%d", port)},
		nil, mounts)
}

func (p *fcProfile) leaderElectionArgs() []string {
	return []string{
		"--csi-address=/csi/csi.sock",
		"--leader-election",
		"--leader-election-namespace=$(POD_NAMESPACE)",
		"--timeout=300s",
		"--v=2",
	}
}

func (p *fcProfile) fcSidecar(name, image string, args []string, env []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	c := corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: ifNotPresent,
		Args:            args,
		SecurityContext: restrictedSC(), // raw security patch covers sidecars too
		VolumeMounts:    mounts,
	}
	if env != nil {
		c.Env = env
	}
	return c
}

func (p *fcProfile) nodeDaemonSet(ns string) *apps.DaemonSet {
	labels := p.appLabels("node")
	registrarMounts := []corev1.VolumeMount{
		{Name: "socket-dir", MountPath: "/csi"},
		{Name: "registration-dir", MountPath: "/registration"},
	}
	driverMounts := []corev1.VolumeMount{
		{Name: "socket-dir", MountPath: "/csi"},
		// Source-bound raw: the node driver container mounts the
		// registration dir too (Helm lacks it — recorded divergence,
		// closed by the pinned raw).
		{Name: "registration-dir", MountPath: "/registration"},
		// automountServiceAccountToken is false pod-wide; the projected
		// token volume is mounted READ-ONLY on the driver container only.
		{Name: "kube-api-access-ngxstorage", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
		{Name: "kubelet-dir", MountPath: "/var/lib/kubelet", MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "host-root", MountPath: "/host", MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "host-dev", MountPath: "/dev"},
	}
	nodeDriverEnv := append([]corev1.EnvVar{
		{Name: "CSI_ENDPOINT", Value: "unix:///csi/csi.sock"},
		fieldRefEnv("KUBE_NODE_NAME", "spec.nodeName"),
	}, p.coordinationDriverEnv()...)
	nodeDriverEnv = append(nodeDriverEnv, p.storageSecretEnv()...)
	nodeDriverEnv = append(nodeDriverEnv,
		corev1.EnvVar{Name: "NGXSTORAGE_CSI_LOG_DIR", Value: "/host/var/log/ngxstorage/fc"})
	nodeDriverArgs := []string{
		"--role=node",
		"--endpoint=$(CSI_ENDPOINT)",
		"--ip=$(STORAGE_IPS)",
		"--pool-name=$(STORAGE_POOL_NAME)",
		"--fc-target-names=$(FC_TARGET_NAMES)",
		"--api-key=$(STORAGE_API_KEY)",
		"--nodeid=$(KUBE_NODE_NAME)",
		"--log=" + p.in.LogLevel, // unquoted
	}

	var templateAnnotations map[string]string
	if p.in.Platform == v1alpha1.PlatformOpenShift {
		// The OCP node DS carries the required-scc annotation.
		templateAnnotations = map[string]string{
			"openshift.io/required-scc": "fc-csi-ngxstorage-node-scc",
		}
	}

	return &apps.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "fc-csi-ngxstorage-node", Namespace: ns, Labels: labels},
		Spec: apps.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			// Source-bound: raw declares NO updateStrategy (Helm's explicit
			// RollingUpdate maxUnavailable=1 equals the kube default —
			// recorded divergence).
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: templateAnnotations},
				Spec: corev1.PodSpec{
					ServiceAccountName:           "fc-csi-ngxstorage-node",
					AutomountServiceAccountToken: ptr.To(false),
					PriorityClassName:            "system-node-critical",
					// Node DS worker-pin is the canonical raw behavior —
					// present on BOTH platforms for FC.
					NodeSelector: map[string]string{
						"kubernetes.io/os":               "linux",
						"node-role.kubernetes.io/worker": "",
					},
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					HostNetwork: true,
					HostPID:     true,
					HostIPC:     true,
					Volumes: []corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/" + fcDriverName, Type: ptr.To(corev1.HostPathDirectoryOrCreate)},
							},
						},
						{
							Name: "registration-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins_registry", Type: ptr.To(corev1.HostPathDirectory)}},
						},
						{
							Name: "kubelet-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet", Type: ptr.To(corev1.HostPathDirectory)}},
						},
						{
							Name: "host-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/", Type: ptr.To(corev1.HostPathDirectory)}},
						},
						{
							Name: "host-dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/dev", Type: ptr.To(corev1.HostPathDirectory)}},
						},
						fcProjectedTokenVolume(),
					},
					Containers: []corev1.Container{
						{
							Name:            "node-driver-registrar",
							Image:           fcSidecar("csi-node-driver-registrar", fcRegistrarDigest),
							ImagePullPolicy: ifNotPresent,
							Args: []string{
								"--csi-address=/csi/csi.sock",
								"--kubelet-registration-path=/var/lib/kubelet/plugins/" + fcDriverName + "/csi.sock",
								"--v=2",
							},
							SecurityContext: restrictedSC(),
							VolumeMounts:    registrarMounts,
						},
						p.livenessProbeSidecar(fcNodeHealthPort, []corev1.VolumeMount{{Name: "socket-dir", MountPath: "/csi"}}),
						{
							Name:            fcName,
							Image:           p.in.DriverImage, // driver image from spec
							ImagePullPolicy: ifNotPresent,
							Args:            nodeDriverArgs,
							Env:             nodeDriverEnv,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: allowEscalation,
								Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN", "SYS_CHROOT"}},
								Privileged:               ptr.To(true),
								ReadOnlyRootFilesystem:   readOnlyRoot,
							},
							VolumeMounts:  driverMounts,
							LivenessProbe: healthProbe(fcNodeHealthPort),
						},
					},
				},
			},
		},
	}
}

// fcProjectedTokenVolume is the source-bound raw projected serviceaccount
// token volume: pod automount is off, so the driver container reads the API
// token from this read-only mount only.
func fcProjectedTokenVolume() corev1.Volume {
	return corev1.Volume{
		Name: "kube-api-access-ngxstorage",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(int32(420)),
				Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path:              "token",
						ExpirationSeconds: ptr.To(int64(3600)),
					}},
					{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
						Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
					}},
					{DownwardAPI: &corev1.DownwardAPIProjection{
						Items: []corev1.DownwardAPIVolumeFile{{
							Path: "namespace",
							FieldRef: &corev1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.namespace",
							}},
						}},
					},
				},
			},
		},
	}
}
