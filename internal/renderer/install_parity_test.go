package renderer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// Golden parity of the COMMITTED operand RBAC install manifests against the
// renderer (out-of-band RBAC contract): hack/generate-install-rbac writes one
// entrypoint pair per rendered profile —
// config/operands/<profile>/{k8s/rbac.yaml, ocp/rbac-ocp.yaml} plus
// {k8s,ocp}/namespace.yaml — from InstallRBAC(); this test parses the
// committed files and field-compares them against a fresh render, so a
// hand-edit of the manifests or a renderer change that forgets to
// regenerate fails the suite. The comparison reuses the pinned golden
// machinery (docKey + pruneZeros) so both sides are normalized identically.
//
// The suite is driven by ProfileNames() in fixed order and covers exactly
// the profiles renderer.For returns: a profile without a renderer skips
// its cells with an explicit note (no placeholder install surface
// exists to pin), and every rendered profile — nfs today, fc/iscsi at
// convergence — is parity-checked the same way.
func TestInstallRBACParity(t *testing.T) {
	platforms := []struct {
		platform v1alpha1.Platform
		overlay  string
		rbacFile string
	}{
		{v1alpha1.PlatformKubernetes, "k8s", "rbac.yaml"},
		{v1alpha1.PlatformOpenShift, "ocp", "rbac-ocp.yaml"},
	}
	for _, prof := range ProfileNames() {
		for _, tc := range platforms {
			t.Run(prof+"-"+string(tc.platform), func(t *testing.T) {
				p := For(ProfileInput{Profile: prof, Platform: tc.platform})
				if p == nil {
					t.Skipf("profile %s has no renderer in this phase — the install set generates when it lands (hack/generate-install-rbac)", prof)
				}
				objs := p.InstallRBAC()
				if len(objs) == 0 {
					t.Fatalf("%s install set is empty on %s", prof, tc.platform)
				}
				path := filepath.Join("..", "..", "config", "operands", prof, tc.overlay, tc.rbacFile)
				committed := loadGolden(t, path)
				rendered := renderedAsJSON(t, objs)
				// renderedAsJSON is keyed by kind/namespace/name: a
				// collapsed key pair (two InstallRBAC objects sharing one
				// identity) must fail loudly, not hide behind the map.
				if len(rendered) != len(objs) {
					t.Fatalf("%s/%s: InstallRBAC() emits %d objects under only %d unique kind/namespace/name identities — duplicate RBAC identity", prof, tc.platform, len(objs), len(rendered))
				}
				if len(committed) != len(rendered) {
					t.Fatalf("%s/%s: committed set carries %d documents, InstallRBAC() renders %d", prof, tc.platform, len(committed), len(rendered))
				}
				assertSetEqual(t, committed, rendered)
			})
		}
	}
}

// TestInstallNamespaceParity (explicit namespace contract): the committed operand
// namespace manifest of every rendered profile is generated from the
// renderer's Profile.Namespace() and is SHARED by both platforms (k8s and
// ocp install into the same operand namespace "ngxstorage-<profile>"). The
// install entrypoints (config/operands/<profile>/{k8s,ocp}) list it FIRST
// — the namespaced Role/RoleBinding objects in the RBAC set cannot apply
// before it exists — so this test also pins that order in both
// entrypoint kustomizations, per rendered profile.
func TestInstallNamespaceParity(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, prof := range ProfileNames() {
		t.Run(prof, func(t *testing.T) {
			p := For(ProfileInput{Profile: prof, Platform: v1alpha1.PlatformKubernetes})
			if p == nil {
				t.Skipf("profile %s has no renderer in this phase — no install namespace to pin", prof)
			}
			// kustomize v5 locality: each overlay carries its OWN
			// namespace.yaml copy (a shared ../ file is forbidden). Both
			// must be byte-identical and match the renderer's
			// Profile.Namespace() for BOTH platforms.
			overlayNS := map[string]string{
				"k8s": filepath.Join(root, "config", "operands", prof, "k8s", "namespace.yaml"),
				"ocp": filepath.Join(root, "config", "operands", prof, "ocp", "namespace.yaml"),
			}
			// Load in FIXED order first: a map-range loop over this set is
			// a coin flip on platform visit order, and comparing ocp
			// against an unread k8sRaw used to fail ~half the runs
			// (envtest-matrix run 2026-11-11 caught it on 1.33.0 while
			// 1.30.0/1.37.0 passed the same bytes).
			raws := map[string][]byte{}
			for _, platform := range []string{"k8s", "ocp"} {
				raw, err := os.ReadFile(overlayNS[platform])
				if err != nil {
					t.Fatalf("read committed namespace manifest %s: %v", overlayNS[platform], err)
				}
				raws[platform] = raw
			}
			if !bytes.Equal(raws["k8s"], raws["ocp"]) {
				t.Fatalf("%s: ocp/namespace.yaml must be byte-identical to k8s/namespace.yaml (same operand namespace)", prof)
			}
			var ns corev1.Namespace
			for platform, raw := range raws {
				ns = corev1.Namespace{}
				if err := yaml.Unmarshal(raw, &ns); err != nil {
					t.Fatalf("%s: parse committed namespace manifest %s: %v", prof, overlayNS[platform], err)
				}
				for _, plat := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
					pr := For(ProfileInput{Profile: prof, Platform: plat})
					if pr == nil {
						t.Fatalf("%s profile missing for %s", prof, plat)
					}
					if ns.Name != pr.Namespace() {
						t.Errorf("%s: %s copy: committed namespace = %q, renderer Profile.Namespace() = %q", prof, platform, ns.Name, pr.Namespace())
					}
				}
				if len(ns.Spec.Finalizers) != 0 {
					t.Errorf("%s: install-set namespace (%s) must not pin finalizers: %v", prof, platform, ns.Spec.Finalizers)
				}
			}

			// Entrypoint order: namespace.yaml (local) before the platform
			// RBAC file.
			for _, tc := range []struct {
				overlay  string
				rbacFile string
			}{
				{"k8s", "rbac.yaml"},
				{"ocp", "rbac-ocp.yaml"},
			} {
				overlay := filepath.Join(root, "config", "operands", prof, tc.overlay, "kustomization.yaml")
				overlayRaw, err := os.ReadFile(overlay)
				if err != nil {
					t.Fatalf("%s: read entrypoint %s: %v", prof, overlay, err)
				}
				idx := func(s, want string) int {
					for i, line := range strings.Split(s, "\n") {
						if strings.TrimSpace(line) == want {
							return i
						}
					}
					return -1
				}
				s := string(overlayRaw)
				if a, b := idx(s, "- namespace.yaml"), idx(s, "- "+tc.rbacFile); a == -1 || b == -1 || a > b {
					t.Errorf("%s: %s entrypoint must list namespace.yaml BEFORE %s (deterministic install order)", prof, tc.overlay, tc.rbacFile)
				}
			}
			// The parent kustomization selects the k8s entrypoint.
			parent := filepath.Join(root, "config", "operands", prof, "kustomization.yaml")
			parentRaw, err := os.ReadFile(parent)
			if err != nil {
				t.Fatalf("%s: read parent kustomization: %v", prof, err)
			}
			idx := func(s, want string) int {
				for i, line := range strings.Split(s, "\n") {
					if strings.TrimSpace(line) == want {
						return i
					}
				}
				return -1
			}
			if idx(string(parentRaw), "- k8s") == -1 {
				t.Errorf("%s: parent kustomization must select the k8s entrypoint", prof)
			}
		})
	}
}

// TestInstallRBACIsObjectsSubset pins the split contract per rendered
// profile: the install set is exactly the RBAC-kind subset of Objects()
// (same order for parity with the pinned golden), and nothing else leaks
// into it — the runtime apply set is Objects() MINUS this, so the two
// together must still cover the full set. The RBAC set is independent of
// the spec projection, so the zero ProfileInput except profile+platform
// is exact (the same input hack/install-rbac generates from).
func TestInstallRBACIsObjectsSubset(t *testing.T) {
	isRBAC := func(o client.Object) bool {
		switch o.(type) {
		case *rbacv1.Role, *rbacv1.RoleBinding, *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding:
			return true
		}
		return false
	}
	for _, prof := range ProfileNames() {
		for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
			t.Run(prof+"-"+string(platform), func(t *testing.T) {
				p := For(ProfileInput{Profile: prof, Platform: platform})
				if p == nil {
					t.Skipf("profile %s has no renderer in this phase", prof)
				}
				want := make(map[string]int, 0)
				got := 0
				for _, o := range p.Objects() {
					if isRBAC(o) {
						got++
						want[o.GetName()] = want[o.GetName()] + 1
					}
				}
				if got != len(p.InstallRBAC()) {
					t.Fatalf("%s/%s: Objects() carries %d RBAC objects, InstallRBAC() returns %d", prof, platform, got, len(p.InstallRBAC()))
				}
				for _, o := range p.InstallRBAC() {
					if want[o.GetName()] == 0 {
						t.Fatalf("%s/%s: InstallRBAC() emitted %q not present in Objects()", prof, platform, o.GetName())
					}
				}
			})
		}
	}
}
