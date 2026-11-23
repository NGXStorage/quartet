package v1alpha1

import (
	"fmt"
	"regexp"
)

// dnsSubdomain is the RFC 1123 DNS SUBDOMAIN pattern, mirroring the CRD
// schema pattern for secretRef.name. This is the REAL Kubernetes Secret
// metadata.name contract: dots are allowed and the limit is 253 chars
// (checked in ValidateSpec). The earlier DNS-label pattern wrongly rejected
// legal names such as "my.secret.name"; it is fixed to the API server's
// own IsDNS1123Subdomain contract, not something stricter.
var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// ociDigestRef mirrors the CRD schema pattern for image.reference:
// <registry>[:port]/<path...>@sha256:<64 hex>. The registry host and
// the repository path are both mandatory; a port is allowed; the sha256
// digest is required, so a bare tag is rejected (no wrong-acceptance of
// digest-less references). The path follows the OCI normalized lowercase
// component grammar; the registry host follows RFC 1123 (no underscore —
// unlike the earlier [\w]-based pattern, which wrongly accepted it).
var ociDigestRef = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?(?::[0-9]{1,5})?/[a-z0-9]+(?:[._-]*[a-z0-9]+)*(?:/[a-z0-9]+(?:[._-]*[a-z0-9]+)*)*@sha256:[0-9a-f]{64}$`)

// ValidateName rejects non-singleton Quartet CRs. This is a RECONCILIER-SIDE
// check only: the CRD admits objects with any name — the API imposes no
// singleton enforcement yet (the mechanism — CRD CEL on metadata.name, a
// webhook, ... — is an open design question). A foreign-named CR is
// API-accepted but refused here (InvalidSpec) and NEVER manages operands.
// Do not present the singleton as API-enforced; envtest proves the
// reconciler behavior, and a real API-level gate remains an open decision.
func (q *Quartet) ValidateName() error {
	if q.Name != QuartetName {
		return fmt.Errorf("singleton violation: only a Quartet named %q is reconciled, got %q", QuartetName, q.Name)
	}
	return nil
}

// ValidateSpec performs reconciler-side spec validation. The CRD schema
// already enforces enums, patterns and required-when-present fields; this
// adds the checks JSON schema cannot express (no CEL/webhook for these):
// - "required when enabled" for secretRef/image on a profile;
// - defense-in-depth re-check of enums and formats.
//
// NOTE: the operator deliberately does NOT touch the Secret API — no
// existence or key-set pre-validation is possible here. A missing Secret or
// key surfaces after operand creation as a kubelet
// CreateContainerConfigError and is reported as generic WorkloadNotReady;
// the MissingSecret/MissingSecretKey reason split is unproven and reserved
// for a future decision.
func (q *Quartet) ValidateSpec() error {
	if err := validatePlatform(q.Spec.Platform); err != nil {
		return err
	}
	switch q.Spec.LogLevel {
	case "", LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return fmt.Errorf("spec.logLevel: unsupported value %q", q.Spec.LogLevel)
	}
	for name, spec := range q.ProfileSpecs() {
		if !spec.Enabled {
			continue
		}
		// dnsSubdomain is the shape check; the 253-char bound is the real
		// K8s metadata.name limit, mirrored from the CRD MaxLength.
		if spec.SecretRef == nil || len(spec.SecretRef.Name) > 253 || !dnsSubdomain.MatchString(spec.SecretRef.Name) {
			return fmt.Errorf("spec.profiles.%s: enabled profile requires secretRef.name as a DNS subdomain (RFC 1123, <=253 chars)", name)
		}
		if spec.Image == nil || !ociDigestRef.MatchString(spec.Image.Reference) {
			return fmt.Errorf("spec.profiles.%s: enabled profile requires image.reference as registry[/path]@sha256:<64 hex>", name)
		}
	}
	return nil
}

// ValidateImmutablePlatform is the reconciler-side (defense-in-depth)
// immutability check for spec.platform. status.specPlatform records the
// value the reconciler first accepted; any later divergence is InvalidSpec.
//
// PRIMARY enforcement is now the API-level CRD CEL rule on QuartetSpec:
// it compares against oldSelf, so a status reset cannot bypass it —
// this was the documented best-effort residual of the old design. The
// reconciler-side check stays for environments without CEL enforcement
// (fake-client tests, direct storage edits). The residual it documents: an
// actor with status-write access on a non-CEL API can clear
// status.specPlatform and change spec.platform together — pinned by
// TestImmutablePlatformStatusClearResidualLimitation.
func (q *Quartet) ValidateImmutablePlatform() error {
	if q.Status.SpecPlatform != "" && q.Spec.Platform != q.Status.SpecPlatform {
		return fmt.Errorf("spec.platform is immutable: accepted %q, got %q", q.Status.SpecPlatform, q.Spec.Platform)
	}
	return nil
}

// ProfileSpecs returns the explicit profile name → spec pairs. The fixed
// three-profile switch is intentional (no generic plugin mechanism).
func (q *Quartet) ProfileSpecs() map[string]*ProfileSpec {
	return map[string]*ProfileSpec{
		ProfileNFS:   &q.Spec.Profiles.NFS,
		ProfileFC:    &q.Spec.Profiles.FC,
		ProfileISCSI: &q.Spec.Profiles.ISCSI,
	}
}

// ValidReasons returns the frozen reason set (slash-free values). Used by
// the reason-set unit test and any future schema/validation test.
func ValidReasons() map[string]bool {
	reasons := []string{
		ReasonReady,
		ReasonMissingSecret,
		ReasonMissingSecretKey,
		ReasonInvalidSpec,
		ReasonUnsupportedPlatform,
		ReasonPlatformDetectionFailed,
		ReasonNamespaceMissing,
		ReasonPermissionDenied,
		ReasonForeignResourceConflict,
		ReasonDependencyMissing,
		ReasonRenderError,
		ReasonApplyConflict,
		ReasonWorkloadNotReady,
		ReasonCleanupFailed,
	}
	out := make(map[string]bool, len(reasons))
	for _, r := range reasons {
		out[r] = true
	}
	return out
}

func validatePlatform(p Platform) error {
	switch p {
	case PlatformAuto, PlatformKubernetes, PlatformOpenShift:
		return nil
	default:
		return fmt.Errorf("spec.platform: unsupported value %q (want auto|kubernetes|openshift)", p)
	}
}
