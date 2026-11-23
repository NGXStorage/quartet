package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// QuartetName is the singleton name. Concurrent creation WITH
// this name is atomic at the API level (the second create gets
// AlreadyExists), but the CRD imposes NO admission on other names: a Quartet
// named anything else is API-ACCEPTED, and only the reconciler refuses it
// (InvalidSpec — it never manages operands, see ValidateName). An API-level
// mechanism (CRD CEL on metadata.name, a validating webhook, ...) is an
// open design question — do NOT present the singleton as API-enforced.
const QuartetName = "quartet"

// Profile names. There is deliberately no generic plugin mechanism:
// profiles are explicit, fixed fields.
const (
	ProfileNFS   = "nfs"
	ProfileFC    = "fc"
	ProfileISCSI = "iscsi"
)

// Platform values for spec.platform.
// +kubebuilder:validation:Enum=auto;kubernetes;openshift
type Platform string

const (
	// PlatformAuto lets the operator detect the platform via API discovery of
	// the config.openshift.io group.
	PlatformAuto Platform = "auto"
	// PlatformKubernetes is an explicit override.
	PlatformKubernetes Platform = "kubernetes"
	// PlatformOpenShift is an explicit override.
	PlatformOpenShift Platform = "openshift"
)

// LogLevel values for spec.logLevel (written to the rendered --log
// arg without quoting).
// +kubebuilder:validation:Enum=debug;info;warn;error
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Condition types. False and Unknown are different; reasons never
// contain a slash (metav1.Condition constraint) — see Reason* constants.
const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"
)

// Reason values written into metav1.Condition.Status and k8s events
// (slash-free by construction). ValidReasons() pins the frozen reason set;
// draft additions are documented at their constants and are deliberately
// outside that helper.
const (
	ReasonReady                   = "Ready"
	ReasonMissingSecret           = "MissingSecret"
	ReasonMissingSecretKey        = "MissingSecretKey"
	ReasonInvalidSpec             = "InvalidSpec"
	ReasonUnsupportedPlatform     = "UnsupportedPlatform"
	ReasonPlatformDetectionFailed = "PlatformDetectionFailed"
	ReasonNamespaceMissing        = "NamespaceMissing"
	ReasonPermissionDenied        = "PermissionDenied"
	ReasonForeignResourceConflict = "ForeignResourceConflict"
	ReasonDependencyMissing       = "DependencyMissing"
	ReasonRenderError             = "RenderError"
	ReasonApplyConflict           = "ApplyConflict"
	ReasonWorkloadNotReady        = "WorkloadNotReady"
	ReasonCleanupFailed           = "CleanupFailed"
)

// ReasonDataInUse is a DRAFT condition reason: disable/delete is blocked
// while active (bound/in-use) PVCs, PVs or attached VolumeAttachments are
// present for the profile's provisioner identity. It is deliberately NOT
// part of ValidReasons(): that helper pins the frozen reason set, and this
// value is an addition to the v1alpha1 DRAFT surface pending API
// finalization — do not present it as a published contract.
const ReasonDataInUse = "DataInUse"

// ReasonRBACDrift is a DRAFT condition reason: an enabled profile's operand
// RBAC — the installer-provisioned set (Profile.InstallRBAC,
// config/operands/<profile>/) — is missing, carries drifted rules, or binds
// the wrong subjects. The out-of-band model gives the reconciler read-only
// RBAC, so it NEVER repairs the drift itself: operand apply is gated behind
// this condition, which is RETRYABLE (requeueAfter) until the admin
// re-applies the install manifest. Deliberately NOT part of ValidReasons():
// same draft-surface status as ReasonDataInUse — do not present it as a
// published contract.
const ReasonRBACDrift = "RBACDrift"

// ProfileSpec is one explicit CSI driver profile (nfs/fc/iscsi).
//
// SecretRef.Name and Image.Reference are format-validated by the CRD schema
// (patterns) but "required when Enabled" is enforced reconciler-side
// (InvalidSpec): this conditional requirement is not expressed as a CEL
// rule — the API-level mechanism decision stays open. The one
// API-level CEL rule in this API is the platform immutability rule on
// QuartetSpec (a closed decision, not part of that mechanism question).
type ProfileSpec struct {
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// +optional
	// Reference to a user-owned Secret. The operator never creates or deletes
	// it and has no Secret API RBAC at all: existence and key presence are
	// verified by the kubelet as CreateContainerConfigError and surface as a
	// generic WorkloadNotReady (a finer reason split is unproven).
	// Same-namespace only (no namespace field).
	SecretRef *SecretRef `json:"secretRef,omitempty"`

	// +optional
	// Driver image with a full OCI digest reference (tags tolerated nowhere;
	// sidecar digests stay renderer constants and have no spec field).
	Image *ImageSpec `json:"image,omitempty"`

	// +optional
	// ForceDisable is the explicit admin override for the active-usage gate:
	// when true, disabling this profile or deleting the CR proceeds even if
	// bound/in-use PVCs, PVs or attached VolumeAttachments are present for
	// the profile's provisioner identity. DRAFT field — the v1alpha1 surface
	// is a draft API, not a published contract: semantics may change before
	// stabilization.
	ForceDisable bool `json:"forceDisable,omitempty"`
}

// SecretRef names the user-owned per-profile Secret (canonical name
// set: nfs/fc/iscsi-csi-ngxstorage-config).
type SecretRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// Kubernetes Secret metadata.name contract: RFC 1123 DNS
	// SUBDOMAIN — dots ARE allowed and the limit is 253 chars. The earlier
	// DNS-label pattern wrongly rejected legal names such as
	// "my.secret.name"; this pattern mirrors the API server's own
	// validation exactly (no extra restriction).
	Name string `json:"name"`
}

// ImageSpec carries the digest-pinned driver image reference.
type ImageSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?(?::[0-9]{1,5})?/[a-z0-9]+(?:[._-]*[a-z0-9]+)*(?:/[a-z0-9]+(?:[._-]*[a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`
	// Full OCI digest reference: <registry>[:port]/<path...>
	// @sha256:<64 hex>. The registry host and the repository path are both
	// mandatory, a port is allowed, and the sha256 digest is required — a
	// bare tag is rejected (no wrong-acceptance of digest-less refs). The
	// path uses the OCI normalized lowercase component grammar; the registry
	// host follows RFC 1123 (underscore is not a legal hostname char, unlike
	// the earlier [\w]-based pattern).
	Reference string `json:"reference"`
}

// Profiles holds the three explicit driver profiles (additional object-store profiles are intentionally absent
// from this API surface entirely).
type Profiles struct {
	// +optional
	NFS ProfileSpec `json:"nfs,omitempty"`
	// +optional
	FC ProfileSpec `json:"fc,omitempty"`
	// +optional
	ISCSI ProfileSpec `json:"iscsi,omitempty"`
}

// QuartetSpec defines the desired state of Quartet (v1alpha1 draft surface).
// Placement/resources/QoS fields intentionally have no place here yet.
//
// Platform immutability at the API level: spec.platform is enforced by the
// CRD CEL rule below, not only by the reconciler. This closes the
// status-reset / pre-first-reconcile bypass of the reconciler-side check —
// the rule compares against oldSelf (the stored spec), which a status write
// cannot influence. CRD CEL (x-kubernetes-validations) is GA on the
// supported Kubernetes versions; on create oldSelf equals self, so creation
// is unaffected. The reconciler-side ValidateImmutablePlatform stays as
// defense-in-depth for environments without CEL enforcement (fake-client
// tests, direct storage edits).
// +kubebuilder:validation:XValidation:rule="self.platform == oldSelf.platform",message="spec.platform is immutable; delete the CR after draining usage, then create it again"
type QuartetSpec struct {
	// Platform selection. Immutable — enforced at the API level by
	// the CRD CEL rule on QuartetSpec and re-checked
	// reconciler-side via status.specPlatform as defense-in-depth.
	// +kubebuilder:validation:Enum=auto;kubernetes;openshift
	// +kubebuilder:default=auto
	Platform Platform `json:"platform"`

	// LogLevel for the rendered operands; written unquoted. Mutable.
	// +kubebuilder:validation:Enum=debug;info;warn;error
	// +kubebuilder:default=info
	// +optional
	LogLevel LogLevel `json:"logLevel,omitempty"`

	Profiles Profiles `json:"profiles"`
}

// ProfileStatus is the per-profile status block (profiles[].conditions
// carry Ready/Progressing/Degraded). Only enabled profiles appear in
// status.profiles; disabled profiles produce no operand and make no claim.
type ProfileStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// QuartetStatus defines the observed state of Quartet.
type QuartetStatus struct {
	// ObservedGeneration is the Quartet .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SpecPlatform records the declared spec.platform value the reconciler
	// first accepted; any later change to spec.platform is an InvalidSpec
	// (platform immutability). Primary enforcement is the API-level CRD CEL
	// rule on QuartetSpec; this field-backed check is defense-in-depth and
	// is BYPASSABLE by clearing the status (documented residual, pinned by
	// TestImmutablePlatformStatusClearResidualLimitation).
	// +optional
	SpecPlatform Platform `json:"specPlatform,omitempty"`

	// ResolvedPlatform is the effective platform after detection
	// (meaningful when spec.platform=auto).
	// +optional
	ResolvedPlatform Platform `json:"resolvedPlatform,omitempty"`

	// Aggregate conditions; Ready reflects the whole singleton.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Per-profile status for enabled profiles, keyed by profile name
	// (nfs/fc/iscsi).
	// +optional
	Profiles map[string]ProfileStatus `json:"profiles,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
//
// Quartet is the cluster-scoped singleton reconciling the chosen CSI driver
// profiles. The singleton is enforced RECONCILIER-SIDE ONLY: the CRD
// accepts objects with other names; the reconciler refuses them with
// InvalidSpec and never manages operands for them (see QuartetName and
// ValidateName). The API-level enforcement mechanism is an open design
// question — do not present the singleton as API-enforced.
type Quartet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   QuartetSpec   `json:"spec,omitempty"`
	Status QuartetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// QuartetList contains a list of Quartet.
type QuartetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Quartet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Quartet{}, &QuartetList{})
}
