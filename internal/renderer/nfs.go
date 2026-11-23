package renderer

import (
	"fmt"
	"reflect"

	apps "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nfsProfile renders the canonical NFS operand set pinned from the raw
// driver manifests with the approved transforms applied. Explicit struct —
// no plugin mechanism.
type nfsProfile struct{ in ProfileInput }

// namespace returns the per-profile operand namespace.
func (p *nfsProfile) Name() string      { return p.in.Profile }
func (p *nfsProfile) Namespace() string { return "ngxstorage-" + p.in.Profile }

// WorkloadTargets implements Profile.WorkloadTargets: the names Objects()
// renders for THIS profile's controller Deployment and node DaemonSet
// (nfsName + "-controller" / "-node").
func (p *nfsProfile) WorkloadTargets() (string, string) {
	return nfsName + "-controller", nfsName + "-node"
}

// --- Canonical constants pinned from the raw NFS manifests; the renderer
// --- must not deviate (golden parity). ---

const (
	nfsName        = "nfs-csi-ngxstorage"     // driver + lease/coordination prefix
	nfsDriverName  = "nfs.csi.ngxstorage.com" // CSIDriver name
	nfsSidecarRepo = "quay.io/ngxstorage/csi-sidecars"

	// Sidecar digests are renderer CONSTANTS (the spec carries the driver
	// image only). Values are the canonical set pinned from the raw
	// renders (identical across NFS/FC/iSCSI).
	nfsProvisionerDigest   = "a4b0b1a37605b7b04a293e136edf7006ec1786a8eb3f4e5a945f81d667dcc371"
	nfsAttacherDigest      = "b9dc9a714a484ccdeeb6f86d88d4db9b7a5ecfc5a55da6db3a60bb3fa33c278a"
	nfsResizerDigest       = "ea1d25e23479000c7e8eeb92d827df66258df4e482ca054c5e7ce3fc0f5c41a5"
	nfsSnapshotterDigest   = "42af0929bcd60a43499825c078a60ff0534e08af8fbeb283aa391be40feb9f3e"
	nfsLivenessProbeDigest = "06da0d5b8908072f2e4522692aee8dc119fba7247a9658497e1153992cd777e9"
	nfsRegistrarDigest     = "f9de845b170155199f2a2a3f9531cf13d78e31235e9db6b6582a8b0db0a50dad"

	nfsLeasePrefix = "nfs-csi-ngxstorage"
	nfsShardCount  = 128
)

func sidecar(image string) string { return nfsSidecarRepo + "/" + image }

// nfsRBACNames is the fixed NFS operand RBAC name set (owner-chain union). Every name is
// produced by Objects() — the scc-use role/binding exist only on OpenShift,
// but the RBAC resourceNames pin is platform-agnostic: listing an extra
// harmless name costs nothing, while the manager RBAC must work when the
// profile later flips platforms. Keep this in lockstep with the Objects()
// builders; the controller tests pin role.yaml against it.
var nfsRBACNames = OperandRBAC{
	Roles: []string{
		"nfs-csi-ngxstorage-controller-operation-lock",
		"nfs-csi-ngxstorage-leader-election",
		"nfs-csi-ngxstorage-node-scc-use",
	},
	RoleBindings: []string{
		"nfs-csi-ngxstorage-controller-operation-lock",
		"nfs-csi-ngxstorage-leader-election",
		"nfs-csi-ngxstorage-node-scc-use",
	},
	ClusterRoles:        []string{"nfs-csi-ngxstorage-controller"},
	ClusterRoleBindings: []string{"nfs-csi-ngxstorage-controller"},
}

func (p *nfsProfile) appLabels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      nfsName,
		"app.kubernetes.io/component": component,
	}
}

// Objects returns the deterministic NFS operand set in the reconciler's
// apply order: RBAC/SA → platform policy (SCC, OCP only) → coordination
// (CM+Lease) → CSIDriver → Service → Deployment → DaemonSet. The Namespace
// is NOT here (the reconciler ensures it); Secret/SC/VSC/test-connection
// are absent by contract.
func (p *nfsProfile) Objects() []client.Object {
	ns := p.Namespace()
	ocp := p.in.Platform == v1alpha1.PlatformOpenShift

	objs := make([]client.Object, 0, 14+nfsShardCount+4)

	// --- RBAC / ServiceAccounts. The RBAC kinds are installer-owned
	// --- (out-of-band provisioning — see InstallRBAC); the SAs remain
	// --- operator-applied.
	objs = append(objs,
		&corev1.ServiceAccount{ObjectMeta: meta("nfs-csi-ngxstorage-controller", ns)},
		&corev1.ServiceAccount{ObjectMeta: meta("nfs-csi-ngxstorage-node", ns)},
	)
	objs = append(objs, p.installRBAC()...)

	// --- Platform policy: SCC + use grant, OCP ONLY. The
	// --- scc-use Role/RoleBinding are part of installRBAC() (installer-
	// --- owned); the SCC itself stays operator-applied (security.
	// --- openshift.io is NOT an rbac.authorization.k8s.io kind).
	if ocp {
		objs = append(objs, p.scc(ns))
	}

	// --- Coordination: immutable CM + 128-lease pool (state never
	// --- rewritten; the driver owns lease contents after creation).
	objs = append(objs, p.coordinationConfigMap(ns))
	objs = append(objs, nfsLeasePool(ns)...)

	// --- Driver plumbing. ---
	objs = append(objs, p.csiDriver(), p.healthService(ns), p.controllerDeployment(ns), p.nodeDaemonSet(ns))
	return objs
}

// installRBAC is the NFS operand RBAC set (Profile.InstallRBAC), in Objects()
// order: the controller ClusterRole, the Role/RoleBinding set for the
// operand namespace, and on OpenShift the node SCC use grant. The ORDER must
// match the golden parity set, so any reordering is a golden regression.
func (p *nfsProfile) installRBAC() []client.Object {
	ns := p.Namespace()
	objs := []client.Object{
		p.clusterRole(),
		p.operationLockRole(ns),
		p.leaderElectionRole(ns),
		p.operationLockRoleBinding(ns),
		p.leaderElectionRoleBinding(ns),
		p.clusterRoleBinding(ns),
	}
	if p.in.Platform == v1alpha1.PlatformOpenShift {
		objs = append(objs, p.sccUseRole(ns), p.sccUseRoleBinding(ns))
	}
	return objs
}

// InstallRBAC implements Profile.InstallRBAC — the installer-owned RBAC set
// (out-of-band provisioning). The reconciler only reads and verifies this
// set by pinned name; it never creates, patches or deletes it. The committed
// manifests are generated from exactly this function
// (hack/generate-install-rbac → config/operands/nfs/), and the golden
// parity test pins the committed files against it.
//
// FC/iSCSI renderers are implemented; their install sets live in
// config/operands/<profile>/ and the parity test covers them. The
// nil-renderer guard stays fail-closed for any future profile without one.
func (p *nfsProfile) InstallRBAC() []client.Object { return p.installRBAC() }

// OperandKinds lists the operand kinds for owned-object enumeration
// (disable/delete cleanup). The SCC kind is OCP-only; it is carried
// unstructured and only enumerated when the platform is OpenShift. The four
// RBAC kinds are ABSENT: operand RBAC is installer-owned out-of-band
// (InstallRBAC), so disable/delete cleanup never enumerates or deletes it —
// removing the operand RBAC set is the installer's uninstall step, not the
// reconciler's.
func operandKind(obj client.Object, list func() client.ObjectList, items func(client.ObjectList) []client.Object, clusterScoped bool) OperandKind {
	return OperandKind{Object: obj, ClusterScoped: clusterScoped, NewList: list, Items: items}
}

func (p *nfsProfile) OperandKinds() []OperandKind {
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

// kind builds an OperandKind from empty object + list instances. The Items
// accessor is shared: every k8s list type carries its elements in an
// `Items []T` field, so one reflection helper serves all kinds (no
// per-type boilerplate, no untyped assertion at the call site).
func kind(obj client.Object, listTemplate runtime.Object, clusterScoped bool) OperandKind {
	return OperandKind{
		Object:        obj,
		ClusterScoped: clusterScoped,
		NewList: func() client.ObjectList {
			return listTemplate.DeepCopyObject().(client.ObjectList)
		},
		Items: itemsFromList,
	}
}

// itemsFromList extracts the element objects from any populated k8s list by
// reading its `Items` field (all list types in client-go expose Items []T
// where T is a pointer implementing client.Object).
func itemsFromList(list client.ObjectList) []client.Object {
	v := reflect.ValueOf(list).Elem().FieldByName("Items")
	if !v.IsValid() || v.Kind() != reflect.Slice {
		return nil
	}
	out := make([]client.Object, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		el := v.Index(i)
		if el.Kind() != reflect.Ptr {
			el = el.Addr()
		}
		if obj, ok := el.Interface().(client.Object); ok {
			out = append(out, obj)
		}
	}
	return out
}

func sccUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func meta(name, ns string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns}
}

// clusterRole is the NFS controller ClusterRole. The raw-only
// `volumeattributesclasses` rule is intentionally dropped, leaving 12
// rules with the core `pods` rule retained.
func (p *nfsProfile) clusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"},
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
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattachments"},
				Verbs: []string{"get", "list", "watch", "patch"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattachments/status"},
				Verbs: []string{"patch"}},
			// NOTE: the raw `volumeattributesclasses` rule is intentionally
			// ABSENT (the driver does not use volume attribute classes).
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshotclasses", "volumesnapshots"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshotcontents"},
				Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshotcontents/status"},
				Verbs: []string{"update", "patch"}},
		},
	}
}

func (p *nfsProfile) operationLockRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("nfs-csi-ngxstorage-controller-operation-lock", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"coordination.k8s.io"},
			Resources:     []string{"leases"},
			ResourceNames: leasePoolNames(),
			Verbs:         []string{"get", "update", "patch"},
		}},
	}
}

func (p *nfsProfile) leaderElectionRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("nfs-csi-ngxstorage-leader-election", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
}

func (p *nfsProfile) operationLockRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("nfs-csi-ngxstorage-controller-operation-lock", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "nfs-csi-ngxstorage-controller-operation-lock"},
		Subjects: controllerSubject(ns),
	}
}

func (p *nfsProfile) leaderElectionRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("nfs-csi-ngxstorage-leader-election", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "nfs-csi-ngxstorage-leader-election"},
		Subjects: controllerSubject(ns),
	}
}

func (p *nfsProfile) clusterRoleBinding(ns string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "nfs-csi-ngxstorage-controller"},
		Subjects: controllerSubject(ns),
	}
}

func controllerSubject(ns string) []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: "nfs-csi-ngxstorage-controller", Namespace: ns,
	}}
}

func nodeSubject(ns string) []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind: rbacv1.ServiceAccountKind, Name: "nfs-csi-ngxstorage-node", Namespace: ns,
	}}
}

// --- OCP-only platform policy. ---

func (p *nfsProfile) sccUseRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: meta("nfs-csi-ngxstorage-node-scc-use", ns),
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"security.openshift.io"},
			Resources:     []string{"securitycontextconstraints"},
			ResourceNames: []string{"nfs-csi-ngxstorage-node-scc"},
			Verbs:         []string{"use"},
		}},
	}
}

func (p *nfsProfile) sccUseRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: meta("nfs-csi-ngxstorage-node-scc-use", ns),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "Role", Name: "nfs-csi-ngxstorage-node-scc-use"},
		Subjects: nodeSubject(ns),
	}
}

// SCCGVK is the OpenShift node SCC's GVK. envtest cannot simulate OpenShift
// API groups, so the SCC is carried as unstructured: the fake-client OCP
// unit tests register this GVK, and the controller's conditional SCC watch
// resolves it from the scheme.
var SCCGVK = schema.GroupVersionKind{
	Group: "security.openshift.io", Version: "v1", Kind: "SecurityContextConstraints",
}

func (p *nfsProfile) scc(ns string) *unstructured.Unstructured {
	// The SCC's grant fields are TOP-LEVEL (not under spec), so build the
	// Object map directly rather than through the nested-field helpers.
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "security.openshift.io/v1",
		"kind":       "SecurityContextConstraints",
		"metadata": map[string]interface{}{
			"name":      "nfs-csi-ngxstorage-node-scc",
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
	return u
}

// --- Coordination. ---

func (p *nfsProfile) coordinationConfigMap(ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nfsName + "-coordination-config",
			Namespace: ns,
			Labels:    p.appLabels("coordination-config"),
		},
		Immutable: ptr.To(true),
		Data: map[string]string{
			"lease-prefix": nfsLeasePrefix,
			"shard-count":  "128",
		},
	}
}

func leasePoolNames() []string {
	names := make([]string, nfsShardCount)
	for i := range names {
		names[i] = fmt.Sprintf("%s-operation-lock-%03d", nfsLeasePrefix, i)
	}
	return names
}

func nfsLeasePool(ns string) []client.Object {
	labels := map[string]string{
		"app.kubernetes.io/name":      nfsName,
		"app.kubernetes.io/component": "operation-lock",
	}
	out := make([]client.Object, nfsShardCount)
	for i, name := range leasePoolNames() {
		out[i] = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		}
	}
	return out
}

// --- Driver plumbing. ---

func (p *nfsProfile) csiDriver() *storagev1.CSIDriver {
	return &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: nfsDriverName},
		Spec: storagev1.CSIDriverSpec{
			AttachRequired: ptr.To(true),
			// Canonical raw contract value (NFS = None).
			FSGroupPolicy:        ptr.To(storagev1.NoneFSGroupPolicy),
			PodInfoOnMount:       ptr.To(false),
			VolumeLifecycleModes: []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent},
		},
	}
}

func (p *nfsProfile) healthService(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nfs-csi-ngxstorage-controller-health",
			Namespace: ns,
			Labels:    p.appLabels("controller-health"),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "healthz", Port: 9808, TargetPort: intstrFromInt(9808)}},
			Selector: map[string]string{
				"app.kubernetes.io/name":      nfsName,
				"app.kubernetes.io/component": "controller",
			},
		},
	}
}

func intstrFromInt(i int) intstr.IntOrString { return intstr.FromInt(i) }

// storageSecretEnv renders the secretKeyRef env block from the spec
// secret name. Key set pinned from the raw manifests:
// storage-ips/pool-name/api-key/data-ip.
func (p *nfsProfile) storageSecretEnv() []corev1.EnvVar {
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
		ref("STORAGE_API_KEY", "api-key"),
		ref("STORAGE_DATA_IP", "data-ip"),
	}
}

var (
	dropAllCaps     = []corev1.Capability{"ALL"}
	noEscalation    = ptr.To(false)
	allowEscalation = ptr.To(true)
	readOnlyRoot    = ptr.To(true)
	ifNotPresent    = corev1.PullIfNotPresent
	pullAlways      = corev1.PullAlways
)

func restrictedSC() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: noEscalation,
		Capabilities:             &corev1.Capabilities{Drop: dropAllCaps},
		ReadOnlyRootFilesystem:   readOnlyRoot,
	}
}

func fieldRefEnv(name, path string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}}
}

func (p *nfsProfile) controllerDeployment(ns string) *apps.Deployment {
	labels := p.appLabels("controller")
	socketMount := []corev1.VolumeMount{{Name: "socket-dir", MountPath: "/csi"}}
	coordEnv := []corev1.EnvVar{
		{Name: "NGX_COORDINATION_LEASE_PREFIX", Value: nfsLeasePrefix},
		{Name: "NGX_COORDINATION_SHARD_COUNT", Value: "128"},
		fieldRefEnv("POD_NAMESPACE", "metadata.namespace"),
		fieldRefEnv("POD_NAME", "metadata.name"),
		fieldRefEnv("POD_UID", "metadata.uid"),
	}
	driverArgs := []string{
		"--endpoint=unix:///csi/csi.sock",
		"--role=controller",
		"--ip=$(STORAGE_IPS)",
		"--pool-name=$(STORAGE_POOL_NAME)",
		"--api-key=$(STORAGE_API_KEY)",
		"--data-ip=$(STORAGE_DATA_IP)",
		// Log level written UNQUOTED (the Helm `| quote` defect is fixed
		// here).
		"--log=" + p.in.LogLevel,
	}

	return &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-controller", Namespace: ns, Labels: labels},
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
					ServiceAccountName: "nfs-csi-ngxstorage-controller",
					PriorityClassName:  "system-cluster-critical",
					// Controller placement = raw behavior (os=linux +
					// master/control-plane/CriticalAddonsOnly tolerations).
					NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
					Tolerations: []corev1.Toleration{
						{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
					},
					// Raw manifest carries an explicit empty pod securityContext.
					SecurityContext: &corev1.PodSecurityContext{},
					Volumes:         []corev1.Volume{{Name: "socket-dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					Containers: []corev1.Container{
						p.provisionerSidecar(socketMount),
						p.attacherSidecar(socketMount),
						p.resizerSidecar(socketMount),
						p.snapshotterSidecar(socketMount),
						p.livenessProbeSidecar("9808", socketMount),
						corev1.Container{
							Name:            nfsName,
							Image:           p.in.DriverImage, // driver image from spec
							ImagePullPolicy: pullAlways,
							Args:            driverArgs,
							Env:             append(append([]corev1.EnvVar{}, coordEnv...), p.storageSecretEnv()...),
							SecurityContext: restrictedSC(),
							VolumeMounts:    socketMount,
							LivenessProbe:   healthProbe(9808),
						},
					},
				},
			},
		},
	}
}

func (p *nfsProfile) provisionerSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-provisioner", sidecar("csi-provisioner@"+"sha256:"+nfsProvisionerDigest),
		append(p.leaderElectionArgs(), "--feature-gates=VolumeAttributesClass=false", "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *nfsProfile) attacherSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-attacher", sidecar("csi-attacher@"+"sha256:"+nfsAttacherDigest),
		append(p.leaderElectionArgs(), "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *nfsProfile) resizerSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-resizer", sidecar("csi-resizer@"+"sha256:"+nfsResizerDigest),
		append(p.leaderElectionArgs(), "--feature-gates=VolumeAttributesClass=false", "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *nfsProfile) snapshotterSidecar(mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("external-snapshotter", sidecar("csi-snapshotter@"+"sha256:"+nfsSnapshotterDigest),
		append(p.leaderElectionArgs(), "--v=2"),
		[]corev1.EnvVar{fieldRefEnv("POD_NAMESPACE", "metadata.namespace")}, mounts)
}

func (p *nfsProfile) livenessProbeSidecar(port string, mounts []corev1.VolumeMount) corev1.Container {
	return p.sidecar("liveness-probe", sidecar("livenessprobe@"+"sha256:"+nfsLivenessProbeDigest),
		[]string{"--csi-address=/csi/csi.sock", "--http-endpoint=0.0.0.0:" + port, "--probe-timeout=3s", "--v=2"},
		nil, mounts)
}

// leaderElectionArgs is the shared sidecar flag set. The VAC feature gate
// is rendered CLOSED (=false) — the Helm canonical value.
func (p *nfsProfile) leaderElectionArgs() []string {
	return []string{
		"--csi-address=/csi/csi.sock",
		"--leader-election",
		"--leader-election-namespace=$(POD_NAMESPACE)",
		"--timeout=300s",
	}
}

func (p *nfsProfile) sidecar(name, image string, args []string, env []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	c := corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: ifNotPresent,
		Args:            args,
		SecurityContext: restrictedSC(),
		VolumeMounts:    mounts,
	}
	if env != nil {
		c.Env = env
	}
	return c
}

func healthProbe(port int) *corev1.Probe {
	return &corev1.Probe{
		FailureThreshold:    5,
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstrFromInt(port)},
		},
	}
}

func (p *nfsProfile) nodeDaemonSet(ns string) *apps.DaemonSet {
	labels := p.appLabels("node")
	registrarMounts := []corev1.VolumeMount{
		{Name: "socket-dir", MountPath: "/csi"},
		{Name: "registration-dir", MountPath: "/registration"},
	}
	driverMounts := []corev1.VolumeMount{
		{Name: "socket-dir", MountPath: "/csi"},
		{Name: "kubelet-dir", MountPath: "/var/lib/kubelet", MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "host-root", MountPath: "/host", MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "host-dev", MountPath: "/dev"},
	}

	nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
	var templateAnnotations map[string]string
	if p.in.Platform == v1alpha1.PlatformOpenShift {
		// OCP node DS is worker-pinned and carries the required-scc
		// annotation; k8s stays os-only.
		nodeSelector["node-role.kubernetes.io/worker"] = ""
		templateAnnotations = map[string]string{
			"openshift.io/required-scc": "nfs-csi-ngxstorage-node-scc",
		}
	}

	return &apps.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-csi-ngxstorage-node", Namespace: ns, Labels: labels},
		Spec: apps.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy: apps.DaemonSetUpdateStrategy{
				Type:          apps.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &apps.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstrFromInt(1))},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: templateAnnotations},
				Spec: corev1.PodSpec{
					ServiceAccountName:           "nfs-csi-ngxstorage-node",
					AutomountServiceAccountToken: ptr.To(false),
					PriorityClassName:            "system-node-critical",
					NodeSelector:                 nodeSelector,
					Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					HostNetwork:                  true,
					HostPID:                      true,
					HostIPC:                      true,
					Volumes: []corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/" + nfsDriverName, Type: ptr.To(corev1.HostPathDirectoryOrCreate)},
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
					},
					Containers: []corev1.Container{
						{
							Name:            "node-driver-registrar",
							Image:           sidecar("csi-node-driver-registrar@" + "sha256:" + nfsRegistrarDigest),
							ImagePullPolicy: ifNotPresent,
							Args: []string{
								"--csi-address=/csi/csi.sock",
								"--kubelet-registration-path=/var/lib/kubelet/plugins/" + nfsDriverName + "/csi.sock",
								"--v=2",
							},
							SecurityContext: restrictedSC(),
							VolumeMounts:    registrarMounts,
						},
						p.livenessProbeSidecar("9809", []corev1.VolumeMount{{Name: "socket-dir", MountPath: "/csi"}}),
						{
							Name:            nfsName,
							Image:           p.in.DriverImage, // driver image from spec
							ImagePullPolicy: pullAlways,
							Args: []string{
								"--endpoint=unix:///csi/csi.sock",
								"--role=node",
								"--nodeid=$(KUBE_NODE_NAME)",
								"--ip=$(STORAGE_IPS)",
								"--pool-name=$(STORAGE_POOL_NAME)",
								"--api-key=$(STORAGE_API_KEY)",
								"--data-ip=$(STORAGE_DATA_IP)",
								"--log=" + p.in.LogLevel, // unquoted
							},
							Env: append([]corev1.EnvVar{
								fieldRefEnv("KUBE_NODE_NAME", "spec.nodeName"),
							}, append(p.storageSecretEnv(),
								corev1.EnvVar{Name: "NGXSTORAGE_CSI_LOG_DIR", Value: "/host/var/log/ngxstorage/nfs"})...),
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: allowEscalation,
								Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN", "SYS_CHROOT"}},
								Privileged:               ptr.To(true),
								ReadOnlyRootFilesystem:   readOnlyRoot,
							},
							VolumeMounts:  driverMounts,
							LivenessProbe: healthProbe(9809),
						},
					},
				},
			},
		},
	}
}
