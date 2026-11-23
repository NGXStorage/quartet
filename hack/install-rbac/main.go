// Command install-rbac regenerates the commit-resident operand install
// manifests (config/operands/<profile>/) from the renderer: namespace.yaml
// (the operand namespace operand namespace, shared by both platforms) and the RBAC set
// from InstallRBAC() — rbac.yaml (kubernetes) and rbac-ocp.yaml
// (openshift). One entrypoint pair per rendered profile:
// config/operands/{nfs,fc,iscsi}/{k8s,ocp}/.
//
// Out-of-band RBAC provisioning (out-of-band RBAC contract): the Quartet manager
// holds ONLY get/list/watch on the four rbac.authorization.k8s.io kinds and
// never creates, patches, deletes or binds RBAC at runtime — a resourceNames
// pin bounds a role's NAME, never its CONTENT, so the previous
// pinned-delete + unrestricted-create/escalate + pinned-bind model was an
// open privilege-escalation chain. The INSTALLER (admin) applies the set
// generated here, out-of-band, and the reconciler only verifies it by
// pinned name at reconcile start (Degraded/RBACDrift until it is healthy).
//
// Deterministic output: the same renderer input always produces byte-
// identical files, so `hack/generate-install-rbac` is idempotent (running it
// twice changes nothing — verified by the gate). The golden parity test
// (internal/renderer/install_parity_test.go) pins the committed files
// against the renderer so the manifests cannot drift from InstallRBAC().
//
// Profiles are driven by renderer.ProfileNames() in fixed order, and a
// profile joins the generated set ONLY when renderer.For returns a
// renderer for it: an unrendered profile gets no placeholder manifests
// (nothing to install from, nothing to present as supported).
package main

import (
	"fmt"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
)

// platformOutputs maps a platform to its generated install set location,
// written into the platform entrypoint overlay (config/operands/<profile>/
// {k8s,ocp}/). kustomize v5 refuses file references that escape the
// kustomization directory (locality constraint), so each overlay carries
// its OWN generated copies: namespace.yaml (byte-identical across
// platforms — the same operand namespace) plus the platform RBAC file. Both
// overlays are parity-pinned against the renderer (TestInstallRBACParity /
// TestInstallNamespaceParity), so the copies cannot drift from each other
// or from InstallRBAC().
var platformOutputs = []struct {
	Platform v1alpha1.Platform
	Overlay  string // entrypoint overlay directory name (k8s|ocp)
	RBACName string // generated RBAC manifest file name
}{
	{Platform: v1alpha1.PlatformKubernetes, Overlay: "k8s", RBACName: "rbac.yaml"},
	{Platform: v1alpha1.PlatformOpenShift, Overlay: "ocp", RBACName: "rbac-ocp.yaml"},
}

// displayName is the header casing for one profile in the generated
// manifests (the nfs text is byte-identical to the pre-generalization
// generator, so regeneration stays idempotent against the committed set).
var displayName = map[string]string{
	v1alpha1.ProfileNFS:   "NFS",
	v1alpha1.ProfileFC:    "FC",
	v1alpha1.ProfileISCSI: "iSCSI",
}

func profileDisplayName(prof string) string {
	if d, ok := displayName[prof]; ok {
		return d
	}
	return prof
}

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	for _, prof := range renderer.ProfileNames() {
		for _, out := range platformOutputs {
			// The RBAC set is independent of the spec projection (secret
			// name / driver image / log level never appear in an RBAC
			// object), so the zero ProfileInput except profile+platform is
			// exact.
			profile := renderer.For(renderer.ProfileInput{Profile: prof, Platform: out.Platform})
			if profile == nil {
				fmt.Printf("install-rbac: %s has no renderer in this phase — no install set generated (no placeholder install surface)\n", prof)
				continue
			}
			objs := profile.InstallRBAC()
			if len(objs) == 0 {
				fatalf("InstallRBAC() returned no objects for profile %s platform %s", prof, out.Platform)
			}

			// namespace.yaml (the operand namespace operand namespace) — byte-identical
			// across both overlays; kustomize locality forbids sharing the
			// file, so each entrypoint carries its own parity-pinned copy.
			nsPath := filepath.Join("config", "operands", prof, out.Overlay, "namespace.yaml")
			if err := writeNamespace(scheme, profile, nsPath); err != nil {
				fatalf("write %s: %v", nsPath, err)
			}

			var buf strings.Builder
			buf.WriteString(header(prof, out.Platform))
			for i, obj := range objs {
				if i > 0 {
					buf.WriteString("---\n")
				}
				doc, err := marshalDoc(obj, scheme)
				if err != nil {
					fatalf("marshal %T %q: %v", obj, obj.GetName(), err)
				}
				buf.WriteString(doc)
			}

			rbPath := filepath.Join("config", "operands", prof, out.Overlay, out.RBACName)
			if err := writeManifest(rbPath, buf.String()); err != nil {
				fatalf("write %s: %v", rbPath, err)
			}
			fmt.Printf("install-rbac: %s regenerated (%d RBAC objects, platform %s)\n", rbPath, len(objs), out.Platform)
		}
	}
}

// writeNamespace generates the operand namespace operand namespace manifest from the
// renderer's Profile.Namespace() (explicit namespace contract) into the given overlay
// directory. The install set's namespaced Role/RoleBinding objects target
// this namespace, so it is part of the set and its FIRST document
// (deterministic order: namespace → RBAC).
func writeNamespace(scheme *runtime.Scheme, profile renderer.Profile, relPath string) error {
	// The namespace NAME is platform-independent (k8s and ocp install into
	// the same operand namespace); the parity test pins both copies. The
	// renderer input is zero except the profile, as in the RBAC loop.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: profile.Namespace()}}
	doc, err := marshalDoc(ns, scheme)
	if err != nil {
		return fmt.Errorf("marshal namespace: %w", err)
	}
	const tmpl = `# GENERATED — do not hand-edit.
# Regenerate with: hack/generate-install-rbac
#
# Operator %s operand namespace (operand namespace) — renderer.Profile.Namespace(),
# shared by the kubernetes and openshift install sets. First document of
# every install entrypoint (config/operands/%s/{k8s,ocp}): the namespaced
# Role/RoleBinding objects in the RBAC set cannot apply before it exists.
# Parity-pinned against the renderer by internal/renderer
# (TestInstallNamespaceParity). The reconciler ALSO ensures this namespace
# at runtime (namespace ownership) but NEVER deletes it; install-set ownership and runtime
# ensure are complementary, not conflicting.
---
`
	content := fmt.Sprintf(tmpl, profileDisplayName(profile.Name()), profile.Name()) + doc
	if err := writeManifest(relPath, content); err != nil {
		return err
	}
	fmt.Printf("install-rbac: %s regenerated (operand namespace %s)\n", relPath, ns.Name)
	return nil
}

func writeManifest(relPath, content string) error {
	abs, err := filepath.Abs(relPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func header(prof string, platform v1alpha1.Platform) string {
	return fmt.Sprintf(`# GENERATED — do not hand-edit.
# Regenerate with: hack/generate-install-rbac
#
# Operator %s operand RBAC install set (platform: %s) — renderer.InstallRBAC().
# Out-of-band provisioning (out-of-band RBAC contract): the Quartet manager holds ONLY
# get/list/watch on the four rbac.authorization.k8s.io kinds and never mutates
# RBAC at runtime. The admin applies this set with the install manifest; the
# reconciler verifies it by pinned name at reconcile start (Degraded/RBACDrift
# until healthy) and gates operand apply behind it. Uninstalling this set is
# the installer's step, not the operator's.
# Parity-pinned against the renderer by internal/renderer (TestInstallRBACParity).
---
`, profileDisplayName(prof), platform)
}

// marshalDoc serializes one typed object as a YAML document with its GVK
// resolved from the scheme (typed literals carry empty TypeMeta). Map keys
// come out sorted via the JSON round-trip inside sigs.k8s.io/yaml — the
// output is byte-deterministic for byte-identical input. The zero-value
// metadata.creationTimestamp that the unstructured conversion emits is
// stripped: it is server-managed metadata, not manifest content (kubectl
// applies either way, but the docs stay install-manifest shaped).
func marshalDoc(obj client.Object, scheme *runtime.Scheme) (string, error) {
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		return "", err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return "", err
	}
	if md, ok := u["metadata"].(map[string]interface{}); ok {
		if ts, present := md["creationTimestamp"]; present && ts == nil {
			delete(md, "creationTimestamp")
		}
	}
	// The unstructured conversion emits zero-value spec/status maps
	// (e.g. Namespace) — server-managed scaffolding, not manifest content
	// (kubectl applies either way, but the docs stay install-manifest
	// shaped).
	for _, key := range []string{"spec", "status"} {
		if m, ok := u[key].(map[string]interface{}); ok && len(m) == 0 {
			delete(u, key)
		}
	}
	b, err := yaml.Marshal(u)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "install-rbac: "+format+"\n", args...)
	os.Exit(1)
}
