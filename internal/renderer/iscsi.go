package renderer

// iscsi.go renders the iSCSI profile's deterministic operand set.
//
// Canonical source: the pinned raw renders of the iSCSI driver repository's
// deploy/manifest (kustomize v5.7.1). The golden-parity fixtures
// testdata/iscsi-{k8s,ocp}-golden.yaml are field-level mirrors of exactly
// those raw renders with ONLY the approved transforms layered on; the
// golden headers record the source pins.
//
// Source-bound contract values (the raw render is the pinned contract):
// - Secret env canonical set: STORAGE_IPS / STORAGE_POOL_NAME /
// PORTAL_GROUP / STORAGE_API_KEY over the secret keys storage-ips /
// pool-name / portal-group / api-key; the secret default name is
// iscsi-csi-ngxstorage-config. PORTAL_GROUP stays UNPREFIXED (the FC
// precedent is FC_TARGET_NAMES), so it is the only env name the
// canonical set keeps raw. The golden mirrors that set via the
// POOL_NAME -> STORAGE_POOL_NAME and API_KEY -> STORAGE_API_KEY
// transforms; parity with the raw key names is NOT the contract here.
// The secret NAME the renderer emits is only ever p.in.SecretName
// (customer-owned); the golden reference name is a parity constant.
// - fsGroupPolicy mirrors the raw contract value File (the Helm chart
// renders None; neither source is declared wrong without live proof).
// - Controller placement = free placement (os=linux + master /
// control-plane / CriticalAddonsOnly tolerations), which the pinned
// raw already carries (no transform); the node DaemonSet worker-pin
// is likewise already in the raw.
// - livenessprobe keeps the raw --health-port flag form and the node
// driver keeps the raw /registration mount.
// - Raw deltas vs NFS, mirrored as-is: sidecars pull Always; the
// controller driver has NO container livenessProbe (sidecar + health
// Service cover it); the node DaemonSet has NO explicit updateStrategy;
// the node health port is 9812; the driver args reference
// $(CSI_ENDPOINT) via an env value; the node driver mounts the
// projected kube-api-access SA-token volume (holder/session read path).
//
// Protocol ownership: the operator passes ONLY the pre-configured portal
// group name through secret → env → --portal-group (the driver's
// cmd/csi-driver flag contract). Login/logout, session sharing and
// teardown-before-last-consumer gating are DRIVER responsibilities
// (pkg/iscsi session lifecycle per IQN/portal; the node driver registers
// the initiator IQN on the backend auth group before login) and the
// renderer never models them. CHAP: see iscsiChapSupportStatus.

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

// iscsiProfile renders the pinned iSCSI raw operand set with the approved
// transforms applied. Explicit struct — no plugin mechanism (Profile
// contract, profile.go). Registry dispatch (For, ProvisionerIdentity,
// OperandRBACNames) is live in profile.go; this file exposes the constants
// the wiring consumes.
type iscsiProfile struct{ in ProfileInput }

// WorkloadTargets implements Profile.WorkloadTargets: the names Objects()
// renders for THIS profile's controller Deployment and node DaemonSet
// (iscsiName + "-controller" / "-node").
func (p *iscsiProfile) WorkloadTargets() (string, string) {
	return iscsiName + "-controller", iscsiName + "-node"
}

// --- Canonical identity constants. Mirrors of the pinned raw renders;
// --- the renderer must not deviate (golden parity).

const (
	iscsiName        = "iscsi-csi-ngxstorage"     // driver + lease/coordination prefix
	iscsiDriverName  = "iscsi.csi.ngxstorage.com" // CSIDriver name
	iscsiSidecarRepo = "quay.io/ngxstorage/csi-sidecars"

	// Sidecar digests are renderer CONSTANTS (the spec carries the driver
	// image only). Values are the canonical set pinned from the raw
	// renders — byte-identical to the NFS/FC constants (cross-driver
	// mirror digests, provenance: pinned raw renders, not an assertion).
	iscsiProvisionerDigest   = "a4b0b1a37605b7b04a293e136edf7006ec1786a8eb3f4e5a945f81d667dcc371"
	iscsiAttacherDigest      = "b9dc9a714a484ccdeeb6f86d88d4db9b7a5ecfc5a55da6db3a60bb3fa33c278a"
	iscsiResizerDigest       = "ea1d25e23479000c7e8eeb92d827df66258df4e482ca054c5e7ce3fc0f5c41a5"
	iscsiSnapshotterDigest   = "42af0929bcd60a43499825c078a60ff0534e08af8fbeb283aa391be40feb9f3e"
	iscsiLivenessProbeDigest = "06da0d5b8908072f2e4522692aee8dc119fba7247a9658497e1153992cd777e9"
	iscsiRegistrarDigest     = "f9de845b170155199f2a2a3f9531cf13d78e31235e9db6b6582a8b0db0a50dad"

	iscsiLeasePrefix = "iscsi-csi-ngxstorage"
	iscsiShardCount  = 128

	// Health ports (raw contract — iSCSI differs from NFS on the NODE side:
	// node driver 9812 vs NFS 9809; controller 9808 is shared).
	iscsiControllerHealthPort = 9808
	iscsiNodeHealthPort       = 9812

	// Projected SA-token volume name pinned in the raw node manifest
	// (deploy/manifest/base/iscsi-csi-ngxstorage-node.yaml). The iSCSI node
	// driver reads the API (holder Pod tracking for session teardown, via
	// the node-holder-pod-read Role) while automountServiceAccountToken is
	// false — so the token arrives through this explicit projection.
	iscsiKubeAPIAccessVolume = "kube-api-access-ngxstorage"
)

// iscsiVolumeHandlePrefix is the CSI volumeID prefix the driver embeds in
// every PV it creates: iSCSI driver pkg/driver/controller.go
// (volumeIDPrefix = "iscsi.csi.ngxstorage.com."; volumeID = prefix + LUN
// id). The manifest never sets --driver-prefix, so the canonical identity
// holds. Consumed by ProvisionerIdentity's usage gate (integration wiring).
const iscsiVolumeHandlePrefix = iscsiDriverName + "."

// iscsiChapSupportStatus records the open CHAP suitability question.
// Verified against the pinned driver source: pkg/ + cmd/ contain ZERO
// chap references (non-test). Auth is carried per-volume by the BACKEND
// AUTH GROUP: the node driver registers its initiator IQN on the auth
// group before login (pkg/driver/node.go) and logins are authorized
// backend-side for registered IQNs only; --portal-group selects a
// pre-configured portal group name.
// Consequence: the operator Secret carries NO CHAP credential keys, the
// driver exposes NO CHAP flags, and no renderer surface may invent one —
// live suitability proof is still required before any CHAP support claim.
const iscsiChapSupportStatus = "OPEN: no CHAP keys or flags in the driver (auth via backend auth groups, per-volume); not modeled by the operator; live proof required"

func iscsiSidecar(image string) string { return iscsiSidecarRepo + "/" + image }

// iscsiRBACNames is the fixed iSCSI operand RBAC name set (mirrors
// nfsRBACNames). Every name is produced by Objects() — the scc-use
// role/binding exist only on OpenShift, but the RBAC resourceNames pin is
// platform-agnostic (see the nfsRBACNames rationale). Keep in lockstep
// with the Objects() builders; the controller tests pin role.yaml against
// it, and the install-set parity test covers the joined OperandRBACNames.
var iscsiRBACNames = OperandRBAC{
	Roles: []string{
		"iscsi-csi-ngxstorage-controller-operation-lock",
		"iscsi-csi-ngxstorage-leader-election",
		"iscsi-csi-ngxstorage-node-holder-pod-read",
		"iscsi-csi-ngxstorage-node-operation-lock",
		"iscsi-csi-ngxstorage-node-scc-use",
	},
	RoleBindings: []string{
		"iscsi-csi-ngxstorage-controller-operation-lock",
		"iscsi-csi-ngxstorage-leader-election",
		"iscsi-csi-ngxstorage-node-holder-pod-read",
		"iscsi-csi-ngxstorage-node-operation-lock",
		"iscsi-csi-ngxstorage-node-scc-use",
	},
	ClusterRoles:        []string{"iscsi-csi-ngxstorage-controller"},
	ClusterRoleBindings: []string{"iscsi-csi-ngxstorage-controller"},
}

func (p *iscsiProfile) Name() string      { return p.in.Profile }
func (p *iscsiProfile) Namespace() string { return "ngxstorage-" + p.in.Profile }

func (p *iscsiProfile) appLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      iscsiName,
		"app.kubernetes.io/component": component,
	}
}

// Objects returns the deterministic iSCSI operand set in the reconciler's
// apply order: RBAC/SA → platform policy (SCC, OCP only) → coordination
// (CM+Lease) → CSIDriver → Service → Deployment → DaemonSet. The Namespace
// is NOT here (the reconciler ensures it); Secret/SC/VSC/test-connection
// are absent by contract.
func (p *iscsiProfile) Objects() []client.Object {
	ns := p.Namespace()
	ocp := p.in.Platform == v1alpha1.PlatformOpenShift

	objs := make([]client.Object, 0, 14+iscsiShardCount+4)

	// --- RBAC / ServiceAccounts. The RBAC kinds are installer-owned
	// --- (out-of-band provisioning — see InstallRBAC); the SAs remain
	// --- operator-applied.
	objs = append(objs,
		&corev1.ServiceAccount{ObjectMeta: meta("iscsi-csi-ngxstorage-controller", ns)},
		&corev1.ServiceAccount{ObjectMeta: meta("iscsi-csi-ngxstorage-node", ns)},
	)
	objs = append(objs, p.installRBAC()...)

	// --- Platform policy: SCC + use grant, OCP ONLY. The
	// --- scc-use Role/RoleBinding are part of installRBAC() (installer-
	// --- owned); the SCC itself stays operator-applied.
	if ocp {
		objs = append(objs, p.scc(ns))
	}

	// --- Coordination: immutable CM + 128-lease pool.
	objs = append(objs, p.coordinationConfigMap(ns))
	objs = append(objs, iscsiLeasePool(ns)...)

	// --- Driver plumbing. ---
	objs = append(objs, p.csiDriver(), p.healthService(ns), p.controllerDeployment(ns), p.nodeDaemonSet(ns))
	return objs
}

// installRBAC is the iSCSI operand RBAC set (Profile.InstallRBAC) in PINNED
// RAW RENDER ORDER (03 render doc sequence: Role ×4, ClusterRole,
// RoleBinding ×4, ClusterRoleBinding; OCP appends the scc-use grant): any
// reordering is a golden regression. Node-side RBAC exists for iSCSI
// (operation-lock + holder-pod-read) because node mutations (login/logout,
// multipath) happen on the node — the NFS node has none.
func (p *iscsiProfile) installRBAC() []client.Object {
	ns := p.Namespace()
	objs := []client.Object{
		p.controllerOperationLockRole(ns),
		p.leaderElectionRole(ns),
		p.nodeHolderPodReadRole(ns),
		p.nodeOperationLockRole(ns),
		p.clusterRole(),
		p.controllerOperationLockRoleBinding(ns),
		p.leaderElectionRoleBinding(ns),
		p.nodeHolderPodReadRoleBinding(ns),
		p.nodeOperationLockRoleBinding(ns),
		p.clusterRoleBinding(ns),
	}
	if p.in.Platform == v1alpha1.PlatformOpenShift {
		objs = append(objs, p.sccUseRole(ns), p.sccUseRoleBinding(ns))
	}
	return objs
}

// InstallRBAC implements Profile.InstallRBAC — the installer-owned RBAC
// set (out-of-band provisioning). The reconciler only reads and verifies
// it by pinned name; the committed manifests are generated from exactly
// this function (hack/generate-install-rbac → config/operands/iscsi/).
func (p *iscsiProfile) InstallRBAC() []client.Object { return p.installRBAC() }

// OperandKinds lists the operand kinds for owned-object enumeration
// (disable/delete cleanup). The SCC kind is OCP-only, carried unstructured
// like the NFS one. The four RBAC kinds are ABSENT: operand RBAC is
// installer-owned out-of-band.
func (p *iscsiProfile) OperandKinds() []OperandKind {
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

// --- RBAC builders (names per iscsiRBACNames; rules per the pinned raw). ---

// clusterRole is the iSCSI controller ClusterRole. The raw-only
// `volumeattributesclasses` rule is intentionally dropped: the pinned raw
// render carries 13 rules, leaving 12 — the same set as the NFS renderer.
func (p *iscsiProfile) clusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "iscsi-csi-ngxstorage-controller"},
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
			{APIGroups: []string{""}, Resources: []string{"pods"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses", "csinodes"},
				Verbs: []string{"get", "list", "watch"}},
			// NOTE: the raw `volumeattributesclasses` rule is intentionally
			// ABSENT (the driver does not use volume attribute classes).
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

func operationLockRule() rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		APIGroups:     []string{"coordination.k8s.io"},
		Resources:     []string{"leases"},
		ResourceNames: iscsiLeasePoolNames(),
		Verbs:         []string{"get", "update", "patch"},
	}
}

func (p *iscsiProfile) controllerOperationLockRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("iscsi-csi-ngxstorage-controller-operation-lock", ns),
		Rules:      []rbacv1.PolicyRule{operationLockRule()},
	}
}

// nodeOperationLockRole is the iSCSI node-side twin of the NFS
// controller-only lock: node mutations (login/logout, multipath
// flush) take per-shard operation locks on the node.
func (p *iscsiProfile) nodeOperationLockRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("iscsi-csi-ngxstorage-node-operation-lock", ns),
		Rules:      []rbacv1.PolicyRule{operationLockRule()},
	}
}

func (p *iscsiProfile) leaderElectionRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("iscsi-csi-ngxstorage-leader-election", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
}

// nodeHolderPodReadRole grants the node driver read access to Pods: the
// session-teardown path must see the last consumer before logout (driver
// responsibility — pkg/driver; the renderer only carries the RBAC).
func (p *iscsiProfile) nodeHolderPodReadRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("iscsi-csi-ngxstorage-node-holder-pod-read", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get"},
		}},
	}
}

func roleBinding(name, ns, roleKind, subjectSA string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta(name, ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: roleKind,
		},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: subjectSA, Namespace: ns,
		}},
	}
}

func (p *iscsiProfile) controllerOperationLockRoleBinding(ns string) *rbacv1.RoleBinding {
	return roleBinding("iscsi-csi-ngxstorage-controller-operation-lock", ns, "iscsi-csi-ngxstorage-controller-operation-lock", "iscsi-csi-ngxstorage-controller")
}

func (p *iscsiProfile) leaderElectionRoleBinding(ns string) *rbacv1.RoleBinding {
	return roleBinding("iscsi-csi-ngxstorage-leader-election", ns, "iscsi-csi-ngxstorage-leader-election", "iscsi-csi-ngxstorage-controller")
}

func (p *iscsiProfile) nodeHolderPodReadRoleBinding(ns string) *rbacv1.RoleBinding {
	return roleBinding("iscsi-csi-ngxstorage-node-holder-pod-read", ns, "iscsi-csi-ngxstorage-node-holder-pod-read", "iscsi-csi-ngxstorage-node")
}

func (p *iscsiProfile) nodeOperationLockRoleBinding(ns string) *rbacv1.RoleBinding {
	return roleBinding("iscsi-csi-ngxstorage-node-operation-lock", ns, "iscsi-csi-ngxstorage-node-operation-lock", "iscsi-csi-ngxstorage-node")
}

func (p *iscsiProfile) clusterRoleBinding(ns string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "iscsi-csi-ngxstorage-controller"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "iscsi-csi-ngxstorage-controller"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: "iscsi-csi-ngxstorage-controller", Namespace: ns,
		}},
	}
}

// --- OCP-only platform policy. ---

func (p *iscsiProfile) sccUseRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("iscsi-csi-ngxstorage-node-scc-use", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"security.openshift.io"},
			Resources:     []string{"securitycontextconstraints"},
			ResourceNames: []string{"iscsi-csi-ngxstorage-node-scc"},
			Verbs:         []string{"use"},
		}},
	}
}

func (p *iscsiProfile) sccUseRoleBinding(ns string) *rbacv1.RoleBinding {
	return roleBinding("iscsi-csi-ngxstorage-node-scc-use", ns, "iscsi-csi-ngxstorage-node-scc-use", "iscsi-csi-ngxstorage-node")
}

// scc renders the OCP node SCC. The pinned raw SCC field set is
// byte-identical to the NFS one (re-verified against the pinned raw
// render), only the name differs.
func (p *iscsiProfile) scc(ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "security.openshift.io/v1",
		"kind":       "SecurityContextConstraints",
		"metadata": map[string]interface{}{
			"name":      "iscsi-csi-ngxstorage-node-scc",
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

func (p *iscsiProfile) coordinationConfigMap(ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      iscsiName + "-coordination-config",
			Namespace: ns,
			Labels:    p.appLabels("coordination-config"),
		},
		Immutable: ptr.To(true),
		Data: map[string]string{
			"lease-prefix": iscsiLeasePrefix,
			"shard-count":  fmt.Sprintf("%d", iscsiShardCount),
		},
	}
}

func iscsiLeasePoolNames() []string {
	names := make([]string, iscsiShardCount)
	for i := range names {
		names[i] = fmt.Sprintf("%s-operation-lock-%03d", iscsiLeasePrefix, i)
	}
	return names
}

func iscsiLeasePool(ns string) []client.Object {
	labels := map[string]string{
		"app.kubernetes.io/name":      iscsiName,
		"app.kubernetes.io/component": "operation-lock",
	}
	out := make([]client.Object, iscsiShardCount)
	for i, name := range iscsiLeasePoolNames() {
		out[i] = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		}
	}
	return out
}

// --- Driver plumbing. ---

// csiDriver renders the CSIDriver. fsGroupPolicy mirrors the RAW contract
// value File (the Helm chart renders None; the canonical decision waits
// on live proof, so neither source is declared wrong here).
func (p *iscsiProfile) csiDriver() *storagev1.CSIDriver {
	return &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: iscsiDriverName},
		Spec: storagev1.CSIDriverSpec{
			AttachRequired: ptr.To(true),
			FSGroupPolicy:  ptr.To(storagev1.FileFSGroupPolicy),
			PodInfoOnMount: ptr.To(false),
			VolumeLifecycleModes: []storagev1.VolumeLifecycleMode{
				storagev1.VolumeLifecyclePersistent,
			},
		},
	}
}

func (p *iscsiProfile) healthService(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "iscsi-csi-ngxstorage-controller-health",
			Namespace: ns,
			Labels:    p.appLabels("controller-health"),
		},
		Spec: corev1.ServiceSpec{
			Ports:    []corev1.ServicePort{{Name: "healthz", Port: iscsiControllerHealthPort, TargetPort: intstrFromInt(iscsiControllerHealthPort)}},
			Selector: p.appLabels("controller"),
		},
	}
}

// storageSecretEnv renders the secretKeyRef env block from the spec
// secret name. Canonical set (see file header):
// storage-ips/pool-name/portal-group/api-key →
// STORAGE_IPS/STORAGE_POOL_NAME/PORTAL_GROUP/STORAGE_API_KEY (PORTAL_GROUP
// stays unprefixed in the canonical set, FC precedent FC_TARGET_NAMES).
// The Secret itself is customer-owned: only the name from ProfileInput
// flows in.
func (p *iscsiProfile) storageSecretEnv() []corev1.EnvVar {
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
		ref("PORTAL_GROUP", "portal-group"),
		ref("STORAGE_API_KEY", "api-key"),
	}
}

// sharedCoordEnv is the raw contract env set present on BOTH workloads'
// driver containers (CSI endpoint inline value + coordination pair +
// downward-API pod identity). The raw uses an env-valued CSI_ENDPOINT
// that the args reference via $(CSI_ENDPOINT).
func sharedCoordEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "CSI_ENDPOINT", Value: "unix:///csi/csi.sock"},
		{Name: "NGX_COORDINATION_LEASE_PREFIX", Value: iscsiLeasePrefix},
		{Name: "NGX_COORDINATION_SHARD_COUNT", Value: fmt.Sprintf("%d", iscsiShardCount)},
		fieldRefEnv("POD_NAMESPACE", "metadata.namespace"),
		fieldRefEnv("POD_NAME", "metadata.name"),
		fieldRefEnv("POD_UID", "metadata.uid"),
	}
}

func (p *iscsiProfile) controllerDeployment(ns string) *apps.Deployment {
	labels := p.appLabels("controller")
	socketMount := []corev1.VolumeMount{{Name: "socket-dir", MountPath: "/csi"}}
	// Driver flag contract (iSCSI driver cmd/csi-driver):
	// role/endpoint/ip/pool-name/portal-group/api-key/log — controller role
	// carries NO --nodeid. --portal-group passes the pre-configured portal
	// group name only.
	driverArgs := []string{
		"--role=controller",
		"--endpoint=$(CSI_ENDPOINT)",
		"--ip=$(STORAGE_IPS)",
		"--pool-name=$(STORAGE_POOL_NAME)",
		"--portal-group=$(PORTAL_GROUP)",
		"--api-key=$(STORAGE_API_KEY)",
		// Log level written UNQUOTED.
		"--log=" + p.in.LogLevel,
	}

	return &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "iscsi-csi-ngxstorage-controller", Namespace: ns, Labels: labels},
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
					ServiceAccountName: "iscsi-csi-ngxstorage-controller",
					PriorityClassName:  "system-cluster-critical",
					// Controller placement = free placement (os=linux +
					// master/control-plane/CriticalAddonsOnly tolerations),
					// which the pinned raw already carries (no golden
					// transform). The raw iSCSI controller pod carries NO
					// pod-level securityContext (unlike NFS) — mirrored as
					// absent.
					NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
					Tolerations: []corev1.Toleration{
						{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
					},
					Volumes: []corev1.Volume{{Name: "socket-dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					Containers: []corev1.Container{
						p.provisionerSidecar(socketMount),
						p.attacherSidecar(socketMount),
						p.resizerSidecar(socketMount),
						p.snapshotterSidecar(socketMount),
						p.livenessProbeSidecar(fmt.Sprintf("%d", iscsiControllerHealthPort), socketMount),
						corev1.Container{
							Name:  iscsiName,
							Image: p.in.DriverImage, // driver image from spec
							// Raw contract: EVERY iSCSI container (sidecars
							// included) pulls Always — differs from NFS
							// (IfNotPresent sidecars).
							ImagePullPolicy: pullAlways,
							Args:            driverArgs,
							Env:             append(append([]corev1.EnvVar{}, sharedCoordEnv()...), p.storageSecretEnv()...),
							// Raw contract: the controller driver carries
							// NO container-level livenessProbe (unlike NFS)
							// — the liveness-probe sidecar (--health-port)
							// and the health Service cover it.
							Ports:           []corev1.ContainerPort{{ContainerPort: iscsiControllerHealthPort, Protocol: corev1.ProtocolTCP}},
							SecurityContext: restrictedSC(),
							VolumeMounts:    socketMount,
						},
					},
				},
			},
		},
	}
}

// Sidecars (canonical digests, Always pull per raw contract). The raw
// iSCSI sidecars carry NO --feature-gates argument at all (unlike NFS raw),
// so the volumeattributesclasses closure only drops the ClusterRole rule
// here.
func (p *iscsiProfile) provisionerSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-provisioner", iscsiSidecar("csi-provisioner@sha256:"+iscsiProvisionerDigest),
		append(p.leaderElectionArgs(), "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *iscsiProfile) attacherSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-attacher", iscsiSidecar("csi-attacher@sha256:"+iscsiAttacherDigest),
		append(p.leaderElectionArgs(), "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *iscsiProfile) resizerSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-resizer", iscsiSidecar("csi-resizer@sha256:"+iscsiResizerDigest),
		append(p.leaderElectionArgs(), "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *iscsiProfile) snapshotterSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-snapshotter", iscsiSidecar("csi-snapshotter@sha256:"+iscsiSnapshotterDigest),
		append(p.leaderElectionArgs(), "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

// livenessProbeSidecar keeps the RAW --health-port flag form (the Helm
// chart's --http-endpoint/--probe-timeout equivalence is unproven).
func (p *iscsiProfile) livenessProbeSidecar(port string, mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("liveness-probe", iscsiSidecar("livenessprobe@sha256:"+iscsiLivenessProbeDigest),
		[]string{"--csi-address=/csi/csi.sock", "--health-port=" + port},
		nil, mounts)
}

// leaderElectionArgs is the shared sidecar flag set (raw contract,
// identical to the NFS/FC set).
func (p *iscsiProfile) leaderElectionArgs() []string {
	return []string{
		"--csi-address=/csi/csi.sock",
		"--leader-election",
		"--leader-election-namespace=$(POD_NAMESPACE)",
		"--timeout=300s",
	}
}

func (p *iscsiProfile) sidecar(name, image string, args []string, env []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	c := corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: pullAlways,
		Args:            args,
		SecurityContext: restrictedSC(),
		VolumeMounts:    mounts,
	}
	if env != nil {
		c.Env = env
	}
	return c
}

// kubeAPIAccessVolume is the projected SA-token volume pinned in the raw
// node manifest (see iscsiKubeAPIAccessVolume).
func kubeAPIAccessVolume() corev1.Volume {
	return corev1.Volume{
		Name: iscsiKubeAPIAccessVolume,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(int32(420)),
				Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						ExpirationSeconds: ptr.To(int64(3600)),
						Path:              "token",
					}},
					{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
						Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
					}},
					{DownwardAPI: &corev1.DownwardAPIProjection{
						Items: []corev1.DownwardAPIVolumeFile{{
							Path:     "namespace",
							FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
						}},
					}},
				},
			},
		},
	}
}

func (p *iscsiProfile) nodeDaemonSet(ns string) *apps.DaemonSet {
	labels := p.appLabels("node")
	registrarMounts := []corev1.VolumeMount{
		{Name: "socket-dir", MountPath: "/csi"},
		{Name: "registration-dir", MountPath: "/registration"},
	}
	driverMounts := []corev1.VolumeMount{
		{Name: "socket-dir", MountPath: "/csi"},
		{Name: "registration-dir", MountPath: "/registration"},
		{Name: iscsiKubeAPIAccessVolume, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
		{Name: "kubelet-dir", MountPath: "/var/lib/kubelet", MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "host-root", MountPath: "/host", MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "host-dev", MountPath: "/dev"},
	}

	// Raw contract: node DS is worker-pinned on BOTH platforms (already
	// in the pinned raw render). OCP-only delta: the required-scc template
	// annotation.
	nodeSelector := map[string]string{
		"kubernetes.io/os":               "linux",
		"node-role.kubernetes.io/worker": "",
	}
	var templateAnnotations map[string]string
	if p.in.Platform == v1alpha1.PlatformOpenShift {
		templateAnnotations = map[string]string{
			"openshift.io/required-scc": "iscsi-csi-ngxstorage-node-scc",
		}
	}

	// Driver flag contract: node role adds --nodeid=$(KUBE_NODE_NAME).
	driverArgs := []string{
		"--role=node",
		"--endpoint=$(CSI_ENDPOINT)",
		"--ip=$(STORAGE_IPS)",
		"--pool-name=$(STORAGE_POOL_NAME)",
		"--portal-group=$(PORTAL_GROUP)",
		"--api-key=$(STORAGE_API_KEY)",
		"--nodeid=$(KUBE_NODE_NAME)",
		"--log=" + p.in.LogLevel, // unquoted
	}
	driverEnv := append([]corev1.EnvVar{
		fieldRefEnv("KUBE_NODE_NAME", "spec.nodeName"),
	}, append(sharedCoordEnv(), append(p.storageSecretEnv(),
		corev1.EnvVar{Name: "NGXSTORAGE_CSI_LOG_DIR", Value: "/host/var/log/ngxstorage/iscsi"})...)...)

	return &apps.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "iscsi-csi-ngxstorage-node", Namespace: ns, Labels: labels},
		Spec: apps.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			// Raw contract: the node DaemonSet carries NO explicit
			// updateStrategy (unlike NFS) — the API default (RollingUpdate,
			// maxUnavailable 1) applies; mirrored as absent.
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: templateAnnotations},
				Spec: corev1.PodSpec{
					ServiceAccountName:           "iscsi-csi-ngxstorage-node",
					AutomountServiceAccountToken: ptr.To(false),
					PriorityClassName:            "system-node-critical",
					NodeSelector:                 nodeSelector,
					Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					HostNetwork:                  true,
					HostPID:                      true,
					HostIPC:                      true,
					Volumes: append([]corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/" + iscsiDriverName, Type: ptr.To(corev1.HostPathDirectoryOrCreate)},
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
					}, kubeAPIAccessVolume()),
					Containers: []corev1.Container{
						{
							Name:            "node-driver-registrar",
							Image:           iscsiSidecar("csi-node-driver-registrar@" + "sha256:" + iscsiRegistrarDigest),
							ImagePullPolicy: pullAlways,
							Args: []string{
								"--csi-address=/csi/csi.sock",
								"--kubelet-registration-path=/var/lib/kubelet/plugins/" + iscsiDriverName + "/csi.sock",
								"--v=2",
							},
							SecurityContext: restrictedSC(),
							VolumeMounts:    registrarMounts,
						},
						p.livenessProbeSidecar(fmt.Sprintf("%d", iscsiNodeHealthPort), []corev1.VolumeMount{{Name: "socket-dir", MountPath: "/csi"}}),
						corev1.Container{
							Name:            iscsiName,
							Image:           p.in.DriverImage, // driver image from spec
							ImagePullPolicy: pullAlways,
							Args:            driverArgs,
							Env:             driverEnv,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: allowEscalation,
								Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN", "SYS_CHROOT"}},
								Privileged:               ptr.To(true),
								ReadOnlyRootFilesystem:   readOnlyRoot,
							},
							VolumeMounts:  driverMounts,
							LivenessProbe: healthProbe(iscsiNodeHealthPort),
						},
					},
				},
			},
		},
	}
}
