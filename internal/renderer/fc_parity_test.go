package renderer

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// --- FC golden parity. ---
//
// Provenance (recorded in the golden headers, testdata/fc-*-golden.yaml):
//
//	raw : ContainerStorage/CSI/Driver/FC @ 7a8454afa04e10905181d2d24e37813680be92b8
//	 (branch master, 2026-11-09), deploy/manifest/{kubernetes,openshift},
//	 pinned checkout-external via git archive (the user-dirty tests/ tree
//	 is EXCLUDED — the reference is the clean tracked commit), rendered
//	 with kustomize v5.7.1: 147/150 docs -> 145/148 after the SC/VSC drop.
//	helm: Integrations/Helm @ f9e3b35b325c3615a91b1cfa05569b4e46f07e8a
//	 (2026-11-03), helm v3.21.3, Helm/fc/{kubernetes,openshift} — semantic
//	 diff reference only, NOT mirrored (17-item divergence table
//	 re-verified at the pinned commit 253d7da; earlier notes superseded).
//
//	Approved transform allow-list (raw -> golden): StorageClass/
//	VolumeSnapshotClass dropped; canonical env/secret names driver env names canonicalized to the
//	helm set (POOL_NAME -> STORAGE_POOL_NAME, API_KEY -> STORAGE_API_KEY,
//	args $(...) refs follow) and the secretKeyRef name to the spec-parity
//	name fc-csi-ngxstorage-config; placement rule controller placement canonicalized
//	to the adopted free-placement default (raw worker-pin stripped from
//	the controller Deployment only; node DS worker-pin stays — placement rule
//	canonical); --log stays UNQUOTED (raw already is); driver digest
//	digest -> parity reference. Everything else is source-bound raw
//	adoption — see the golden headers for the recorded divergences
//	(fsGroupPolicy File / fsGroupPolicy File open, --health-port flags, controller
//	containerPort without probe, /registration driver mount, projected
//	token volume, IfNotPresent, kustomize SCC namespace artifact).
//
// fcProfile is constructed DIRECTLY here: the parity contract stays pinned
// even if the registry dispatch wiring changes; the dispatch itself
// (For / ProvisionerIdentity / OperandRBACNames) is pinned by
// TestFCRegistryWiringContract.

const fcGoldenSecret = "fc-csi-ngxstorage-config"

// fcGoldenDriverImage is the digest parity reference substituted into the
// golden (the golden otherwise carries the raw pinned digest 3d6542d7...).
var fcGoldenDriverImage = "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:" + strings.Repeat("a", 64)

// fcGoldenInput is the exact spec projection used to render the parity set,
// so the renderer and golden are compared like-for-like.
func fcGoldenInput(platform v1alpha1.Platform) ProfileInput {
	return ProfileInput{
		Profile:     "fc",
		SecretName:  fcGoldenSecret,
		DriverImage: fcGoldenDriverImage,
		LogLevel:    "info",
		Platform:    platform,
	}
}

// TestFCRenderParityK8s pins the FC Kubernetes operand set against the
// pinned-clean raw reference (regeneration independence / normalized comparison). Missing and extra documents are
// both rejected (assertSetEqual), then every field-group is compared.
func TestFCRenderParityK8s(t *testing.T) {
	p := &fcProfile{in: fcGoldenInput(v1alpha1.PlatformKubernetes)}
	want := loadGolden(t, filepath.Join("testdata", "fc-k8s-golden.yaml"))
	got := renderedAsJSON(t, p.Objects())
	assertSetEqual(t, want, got)
}

// TestFCRenderParityOCP pins the FC OpenShift operand set (SCC, scc-use
// RBAC, required-scc annotation) against the pinned-clean raw reference.
func TestFCRenderParityOCP(t *testing.T) {
	p := &fcProfile{in: fcGoldenInput(v1alpha1.PlatformOpenShift)}
	want := loadGolden(t, filepath.Join("testdata", "fc-ocp-golden.yaml"))
	got := renderedAsJSON(t, p.Objects())
	assertSetEqual(t, want, got)
}

// TestFCRegistryWiringContract pins the registry dispatch contract
// (converted from the pre-wiring phase pin "For(fc) must return nil", which
// the profile-integration worker superseded when the dispatch landed — per
// the brief, phase expectations convert to support tests, they are not just
// deleted). The dispatch, provisioner identity and RBAC names must agree
// with this renderer: the usage gate must not skip fc because of a missing
// identity, and the installer set must be exactly fcRBACNames.
func TestFCRegistryWiringContract(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := For(fcGoldenInput(platform))
		if p == nil {
			t.Fatalf("platform %s: For(fc) must dispatch to the fc renderer once wired", platform)
		}
		if p.Name() != v1alpha1.ProfileFC || p.Namespace() != OperandNamespace(v1alpha1.ProfileFC) {
			t.Errorf("For(fc) identity = %s/%s, want %s/%s", p.Name(), p.Namespace(), v1alpha1.ProfileFC, OperandNamespace(v1alpha1.ProfileFC))
		}
	}
	dn, vhp := ProvisionerIdentity(v1alpha1.ProfileFC)
	if dn != fcDriverName || vhp != fcDriverName+"." {
		t.Errorf("ProvisionerIdentity(fc) = %q/%q, want %q/%q (pinned from the driver source, not invented)", dn, vhp, fcDriverName, fcDriverName+".")
	}
	if rb := OperandRBACNames(v1alpha1.ProfileFC); !reflect.DeepEqual(rb, fcRBACNames) {
		t.Errorf("OperandRBACNames(fc) = %+v, want fcRBACNames %+v", rb, fcRBACNames)
	}
}
