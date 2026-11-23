package renderer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// --- iSCSI golden parity. ---
//
// The golden fixtures (testdata/iscsi-{k8s,ocp}-golden.yaml) are FIELD-LEVEL
// MIRRORS of the pinned upstream iSCSI RAW renders — NOT byte-identical to
// them, and NOT produced from this renderer. Provenance (full record in
// each golden header):
//
//	Source commit : ContainerStorage/CSI/Driver/iSCSI @
//	 3e138b2dc56490b1c60d16298a85be227ec89cae (v2.0.0),
//	 deploy/manifest byte-identical to pinned snapshot 71eba6d
//	Tool : kustomize v5.7.1, pinned checkout-external via
//	 `git archive` (dirty worktree state excluded)
//	Transforms : SC/VSC dropped, volumeattributesclasses ClusterRole
//	 rule dropped, env/secret-name set canonicalized to
//	 the helm set (POOL_NAME -> STORAGE_POOL_NAME, API_KEY ->
//	 STORAGE_API_KEY, args $(...) refs and secretKeyRef names
//	 follow; PORTAL_GROUP stays unprefixed per the helm chart),
//	 driver digest -> parity reference, worker-pin/
//	 required-scc asserted+ensured (already in raw); unquoted log arg and coordination
//	 retained
//	Open cells : fsGroupPolicy File and health-port parity mirror the RAW contract (helm divergences stay
//	 open, the earlier manifest notes); placement rule controller placement is the adopted
//	 default (free) that the raw already carries
//
// Regeneration independence: `make golden-verify` (hack/verify-golden.sh +
// hack/golden-verify) re-renders the pinned raw checkout-external with
// kustomize v5.7.1, applies ONLY these approved transforms, and
// field-compares against the fixture WITHOUT touching this package's
// renderer code.
//
// Registry dispatch is NOT exercised here: the parity tests construct the
// profile directly (iscsiProfileOf) so the render contract stays pinned
// even if the dispatch wiring changes; TestProfileWorkloadTargets pins
// For() dispatch for every rendered profile.

// iscsiGoldenSecret is the canonical env/secret names helm-canonical secret NAME the golden's
// secretKeyRefs carry (Integrations/Helm iscsi chart @ f9e3b35b, same pin
// the FC golden uses for its helm reference). Test input feeds it as
// ProfileInput.SecretName so renderer and golden compare like-for-like;
// the renderer never invents this name (secretRef-only).
const iscsiGoldenSecret = "iscsi-csi-ngxstorage-config"

// iscsiGoldenDriverImage is the digest parity reference substituted into the
// golden (the golden otherwise carries the raw pinned digest 0988d151…).
var iscsiGoldenDriverImage = "quay.io/ngxstorage/iscsi-csi-ngxstorage@sha256:" + strings.Repeat("a", 64)

// iscsiGoldenInput is the exact spec projection used to render the parity
// set, so the renderer and golden are compared like-for-like.
func iscsiGoldenInput(platform v1alpha1.Platform) ProfileInput {
	return ProfileInput{
		Profile:     "iscsi",
		SecretName:  iscsiGoldenSecret,
		DriverImage: iscsiGoldenDriverImage,
		LogLevel:    "info",
		Platform:    platform,
	}
}

// iscsiProfileOf constructs the iSCSI renderer directly (bypassing For —
// registry dispatch is the profile-integration worker's surface).
func iscsiProfileOf(platform v1alpha1.Platform) *iscsiProfile {
	return &iscsiProfile{in: iscsiGoldenInput(platform)}
}

// pruneISCSIZeros extends the deterministic normalization (pruneZeros, shared with
// NFS in parity_test.go — untouched here) with ONE iSCSI-only case: the
// pinned raw DaemonSet carries NO updateStrategy, but encoding/json
// omitempty does not apply to the non-pointer DaemonSetSpec.UpdateStrategy
// field, so the typed render emits the zero-value shell
// `updateStrategy: {}`. Exactly that shell is pruned; the API default
// (RollingUpdate, maxUnavailable 1) then applies — identical to what the
// raw manifest receives on admission.
func pruneISCSIZeros(kind string, m map[string]interface{}) map[string]interface{} {
	if kind != "DaemonSet" {
		return m
	}
	spec, _ := m["spec"].(map[string]interface{})
	if spec == nil {
		return m
	}
	if us, ok := spec["updateStrategy"].(map[string]interface{}); ok && len(us) == 0 {
		delete(spec, "updateStrategy")
	}
	return m
}

func pruneISCSI(t *testing.T, got map[string]map[string]interface{}) map[string]map[string]interface{} {
	t.Helper()
	for k, m := range got {
		got[k] = pruneISCSIZeros(strings.SplitN(k, "/", 2)[0], m)
	}
	return got
}

// TestISCSIRenderParityK8s pins the iSCSI Kubernetes operand set against
// the pinned raw-render golden (regeneration independence / normalized comparison).
func TestISCSIRenderParityK8s(t *testing.T) {
	p := iscsiProfileOf(v1alpha1.PlatformKubernetes)
	want := loadGolden(t, filepath.Join("testdata", "iscsi-k8s-golden.yaml"))
	got := pruneISCSI(t, renderedAsJSON(t, p.Objects()))
	assertSetEqual(t, want, got)
}

// TestISCSIRenderParityOCP pins the iSCSI OpenShift operand set (SCC,
// scc-use RBAC, required-scc annotation) against the pinned raw-render
// golden (regeneration independence / normalized comparison).
func TestISCSIRenderParityOCP(t *testing.T) {
	p := iscsiProfileOf(v1alpha1.PlatformOpenShift)
	want := loadGolden(t, filepath.Join("testdata", "iscsi-ocp-golden.yaml"))
	got := pruneISCSI(t, renderedAsJSON(t, p.Objects()))
	assertSetEqual(t, want, got)
}

// TestISCSIEmitsNoForbiddenResources asserts the owner-matrix negatives on
// both platforms (no Secret (secretRef-only), no SC/VSC, helm-only test Pod absent).
func TestISCSIEmitsNoForbiddenResources(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		for _, obj := range iscsiProfileOf(platform).Objects() {
			gvks, _, err := parityScheme.ObjectKinds(obj)
			if err != nil {
				t.Fatalf("resolve GVK for %T: %v", obj, err)
			}
			if forbiddenKinds[gvks[0].Kind] {
				t.Errorf("platform %s: iSCSI renderer emitted forbidden kind %s %q (secretRef-only, no SC/VSC, helm-only)",
					platform, gvks[0].Kind, obj.GetName())
			}
		}
	}
}

// TestISCSDocumentCounts pins the operand document counts: raw k8s 147 /
// ocp 150 minus the two dropped SC/VSC = 145 / 148.
func TestISCSDocumentCounts(t *testing.T) {
	if p := iscsiProfileOf(v1alpha1.PlatformKubernetes); len(p.Objects()) != 145 {
		t.Errorf("k8s operand count = %d, want 145 (raw 147 - dropped SC/VSC)", len(p.Objects()))
	}
	if p := iscsiProfileOf(v1alpha1.PlatformOpenShift); len(p.Objects()) != 148 {
		t.Errorf("ocp operand count = %d, want 148 (raw 150 - dropped SC/VSC)", len(p.Objects()))
	}
}
