// Layer-1 static version audit (candidate range 1.30-1.37).
//
// This test proves only the API-surface contract of the rendered sets:
// config/default-nfs, config/default-nfs-ocp and the runtime NFS operand render
// (both platforms). It verifies that every apiVersion/kind pair used is a
// stable v1 API that exists unchanged across the whole candidate range,
// that the range floor covers the newest hard feature prerequisite (CRD
// CEL validation, GA 1.29), and that every container image is a full
// @sha256 digest reference (canonical sidecar set).
//
// What it does NOT prove: envtest/API behavior per version (layer 2,
// hack/envtest-matrix.sh), kubelet/CSI mount/I/O or OCP admission
// behavior (layer 3, live sampling only). A green static audit on one
// binary is not compatibility proof for the range; live sampling remains
// a separate gate.

package rendertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/renderer"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

// supportedRange is the candidate range fixed by user decision
// 2026-11-11. Candidate, NOT a support promise — closure requires the compatibility campaign
// layer evidence, not this constant.
const (
	rangeFloorMinor = 30
	rangeCeilMinor  = 37
)

// featureMinimums maps each hard API prerequisite of the rendered sets to
// the K8s minor in which the feature reached the maturity the manifests
// rely on. Dates are the stable/GA releases, not the beta introductions
// (H4 correction: SSA stable 1.22 — beta 1.16 does not count; CSIDriver
// GA 1.18 — alpha 1.13 does not count).
var featureMinimums = map[string]int{
	"core/v1":                                       1,  // stable since 1.0
	"apps/v1 (Deployment/DaemonSet)":                9,  // stable 1.9
	"rbac.authorization.k8s.io/v1":                  8,  // stable 1.8
	"coordination.k8s.io/v1 (Lease)":                14, // stable 1.14 (beta 1.12)
	"storage.k8s.io/v1 (CSIDriver)":                 18, // GA 1.18 (alpha 1.13)
	"events.k8s.io/v1":                              19, // stable 1.19 (beta 1.8)
	"apiextensions.k8s.io/v1 (CRD)":                 16, // stable 1.16 (beta 1.11)
	"CRD CEL validation (x-kubernetes-validations)": 29, // GA 1.29 (beta 1.25)
	"server-side apply (SSA)":                       22, // stable 1.22 (beta 1.16)
}

// digestRef is the digest pin full OCI digest reference contract.
var digestRef = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$`)

// allowedAPIKinds is the EXPLICIT stable API allow-list for the 1.30-1.37
// candidate range (API allow-list: a "/v1" suffix check alone accepted unknown groups).
// Adding a kind here is a deliberate range decision — the test fails
// loudly on anything else, including unknown group/v1 pairs and kinds not
// known to their group.
var allowedAPIKinds = map[string]map[string]bool{
	"v1":                           {"Namespace": true, "ServiceAccount": true, "Service": true, "ConfigMap": true, "Secret": true},
	"apps/v1":                      {"Deployment": true, "DaemonSet": true},
	"rbac.authorization.k8s.io/v1": {"ClusterRole": true, "ClusterRoleBinding": true, "Role": true, "RoleBinding": true},
	"storage.k8s.io/v1":            {"CSIDriver": true},
	"coordination.k8s.io/v1":       {"Lease": true},
	"apiextensions.k8s.io/v1":      {"CustomResourceDefinition": true},
	"security.openshift.io/v1":     {"SecurityContextConstraints": true}, // openshift sets only
}

// observedFeatureAPIs maps each featureMinimums entry that a RENDER must
// exercise to the apiVersion that proves it (API allow-list: the floor table must be
// tied to APIs actually present in the audited sets — core/v1, apps/v1,
// rbac/v1, Lease, CSIDriver and the CEL-bearing CRD are all render-visible.
// SSA and events/v1 are runtime behaviors, documented prerequisites only).
var observedFeatureAPIs = map[string]string{
	"core/v1":                                       "v1",
	"apps/v1 (Deployment/DaemonSet)":                "apps/v1",
	"rbac.authorization.k8s.io/v1":                  "rbac.authorization.k8s.io/v1",
	"coordination.k8s.io/v1 (Lease)":                "coordination.k8s.io/v1",
	"storage.k8s.io/v1 (CSIDriver)":                 "storage.k8s.io/v1",
	"apiextensions.k8s.io/v1 (CRD)":                 "apiextensions.k8s.io/v1",
	"CRD CEL validation (x-kubernetes-validations)": "apiextensions.k8s.io/v1",
}

// renderSet is one auditable render with a platform tag. The two raw
// kustomize compositions (config/default-nfs, config/default-nfs-ocp) carry the digest rule
// manager dev placeholder; the runtime renders must be fully digest-pinned.
// auditSet distinguishes them by set name (installRenderOf).
type renderSet struct {
	name     string
	platform v1alpha1.Platform // openshift sets may carry SCC; upstream sets may not
	docs     []map[string]interface{}
}

// TestStaticVersionRangeAudit enforces the per-render API surface contract
// for every render set of both platforms.
func TestStaticVersionRangeAudit(t *testing.T) {
	root := repoRoot(t)

	// Range floor sanity: every hard prerequisite must fit inside the
	// declared candidate range. If someone widens the floor below a feature
	// minimum, the audit fails here instead of at apply time.
	for feature, min := range featureMinimums {
		if min > rangeCeilMinor {
			t.Fatalf("feature %q requires K8s 1.%d but exceeds the range ceiling 1.%d",
				feature, min, rangeCeilMinor)
		}
		if min > rangeFloorMinor {
			t.Fatalf("range floor 1.%d is below the hard prerequisite %q (1.%d) — raise the floor",
				rangeFloorMinor, feature, min)
		}
	}

	// Every profile-selected install composition is audited (composition audit: the audit
	// must not be NFS-only while fc/iscsi compositions ship the same surface).
	installComps := []struct {
		name     string
		platform v1alpha1.Platform
		path     string
	}{
		{"config/default-nfs", v1alpha1.PlatformKubernetes, "config/default-nfs"},
		{"config/default-nfs-ocp", v1alpha1.PlatformOpenShift, "config/default-nfs-ocp"},
		{"config/default-fc", v1alpha1.PlatformKubernetes, "config/default-fc"},
		{"config/default-fc-ocp", v1alpha1.PlatformOpenShift, "config/default-fc-ocp"},
		{"config/default-iscsi", v1alpha1.PlatformKubernetes, "config/default-iscsi"},
		{"config/default-iscsi-ocp", v1alpha1.PlatformOpenShift, "config/default-iscsi-ocp"},
	}
	sets := []renderSet{}
	for _, c := range installComps {
		sets = append(sets, renderSet{
			name: c.name, platform: c.platform,
			docs: parseUnstructured(t, buildKustomizePath(t, root, c.path)),
		})
	}
	runtimeStart := len(sets)
	sets = append(sets, runtimeRenderSet(t, v1alpha1.PlatformKubernetes)...)
	sets = append(sets, runtimeRenderSet(t, v1alpha1.PlatformOpenShift)...)

	// The quartets CRD (and its CEL) must be present in EVERY install set;
	// the runtime sets carry no CRD.
	for _, s := range sets[:runtimeStart] {
		for _, v := range auditSet(s.docs, s.platform, true, s.name) {
			t.Error(v)
		}
	}
	for _, s := range sets[runtimeStart:] {
		for _, v := range auditSet(s.docs, s.platform, false, s.name) {
			t.Error(v)
		}
	}

	// (API allow-list) The floor table must be tied to APIs the renders actually use:
	// every observed feature apiVersion appears in the union of the audited sets.
	observed := map[string]bool{}
	usesCEL := false
	for _, s := range sets {
		for _, d := range s.docs {
			av, _ := d["apiVersion"].(string)
			if k, _ := d["kind"].(string); k == "CustomResourceDefinition" {
				if spec, ok := d["spec"].(map[string]interface{}); ok && specHasCEL(spec) {
					usesCEL = true
				}
			}
			if av != "" {
				observed[av] = true
			}
		}
	}
	for feature, av := range observedFeatureAPIs {
		if feature == "CRD CEL validation (x-kubernetes-validations)" {
			if !usesCEL {
				t.Errorf("feature %q not exercised: no rendered CRD uses x-kubernetes-validations", feature)
			}
			continue
		}
		if !observed[av] {
			t.Errorf("feature %q maps to %s but no audited render uses that API — the floor rationale is stale", feature, av)
		}
	}
}

// auditSet returns violations for one render (API allow-list: pure, so negative
// fixtures can drive it directly). expectsCRD requires the quartets CRD
// to be present and to exercise CEL (the load-bearing range floor).
func auditSet(docs []map[string]interface{}, platform v1alpha1.Platform, expectsCRD bool, name string) []string {
	var violations []string
	usesCEL := false
	for _, d := range docs {
		apiVersion, _ := d["apiVersion"].(string)
		kind, _ := d["kind"].(string)
		if apiVersion == "" || kind == "" {
			return []string{fmt.Sprintf("%s: document missing apiVersion/kind: %v", name, d["metadata"])}
		}
		// (1) Explicit allow-list (API allow-list): an unknown group/v1 pair or a kind
		// not known to its group is a violation, not a suffix pass.
		kinds, ok := allowedAPIKinds[apiVersion]
		if !ok {
			violations = append(violations, fmt.Sprintf("%s: %s is not an allow-listed stable API group for the 1.30-1.37 range", name, apiVersion))
			continue
		}
		if apiVersion == "security.openshift.io/v1" && platform != v1alpha1.PlatformOpenShift {
			violations = append(violations, fmt.Sprintf("%s: %s %s is an OpenShift API; this render must not carry it", name, apiVersion, kind))
			continue
		}
		if !kinds[kind] {
			violations = append(violations, fmt.Sprintf("%s: kind %s is not allow-listed under %s — add it deliberately with a range justification", name, kind, apiVersion))
			continue
		}
		// (2) CEL usage drives the range floor.
		if kind == "CustomResourceDefinition" {
			if spec, ok := d["spec"].(map[string]interface{}); ok {
				usesCEL = specHasCEL(spec)
			}
		}
		// (3) Every container image is a full digest reference.
		if spec, ok := d["spec"].(map[string]interface{}); ok {
			if tmpl, ok := spec["template"].(map[string]interface{}); ok {
				if podSpec, ok := tmpl["spec"].(map[string]interface{}); ok {
					if containers, ok := podSpec["containers"].([]interface{}); ok {
						for _, c := range containers {
							cm, _ := c.(map[string]interface{})
							image, _ := cm["image"].(string)
							if image == "" {
								continue
							}
							if installRenderOf(name) &&
								image == "quay.io/ngxstorage/quartet-operator:v0.0.0-dev" {
								// The digest rule pins this to a full digest before apply.
								continue
							}
							if !digestRef.MatchString(image) {
								violations = append(violations, fmt.Sprintf("%s: %s container image %q is not a full @sha256 digest reference (digest pin)", name, kind, image))
							}
						}
					}
				}
			}
		}
	}
	if expectsCRD && !usesCEL {
		violations = append(violations, fmt.Sprintf("%s: quartets CRD missing or no longer uses x-kubernetes-validations — the CEL floor (1.29) is load-bearing; update featureMinimums in the same change", name))
	}
	return violations
}

// installRenderOf: the two raw kustomize compositions carry the digest-pinned
// dev placeholder; the runtime renders must be fully digest-pinned.
func installRenderOf(name string) bool {
	switch name {
	case "config/default-nfs", "config/default-nfs-ocp",
		"config/default-fc", "config/default-fc-ocp",
		"config/default-iscsi", "config/default-iscsi-ocp":
		return true
	}
	return false
}

// specHasCEL reports whether a CRD spec carries x-kubernetes-validations.
func specHasCEL(spec map[string]interface{}) bool {
	if spec == nil {
		return false
	}
	versions, _ := spec["versions"].([]interface{})
	for _, v := range versions {
		vm, _ := v.(map[string]interface{})
		schema, _ := vm["schema"].(map[string]interface{})
		if schema == nil {
			continue
		}
		b, err := json.Marshal(schema)
		if err != nil {
			return false
		}
		if strings.Contains(string(b), "x-kubernetes-validations") {
			return true
		}
	}
	return false
}

// parseUnstructured renders kustomize output into unstructured maps so the
// audit does not depend on the doc struct of config_render_test.go.
func parseUnstructured(t *testing.T, rendered string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, raw := range strings.Split(rendered, "\n---\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(trimmed), &m); err != nil {
			t.Fatalf("parse render document: %v", err)
		}
		if m["kind"] == nil {
			t.Fatalf("render document has no kind: %q", trimmed)
		}
		out = append(out, m)
	}
	return out
}

// TestStaticAuditNegativeFixtures (API allow-list): the allow-list must REJECT an
// unknown group/v1 pair, a kind not known to an allow-listed group, and an
// OpenShift API carried by an upstream render — the exact cases the old
// suffix-only check accepted.
func TestStaticAuditNegativeFixtures(t *testing.T) {
	doc := func(apiVersion, kind string) map[string]interface{} {
		return map[string]interface{}{"apiVersion": apiVersion, "kind": kind,
			"metadata": map[string]interface{}{"name": "x"}}
	}
	cases := []struct {
		desc     string
		platform v1alpha1.Platform
		docs     []map[string]interface{}
	}{
		{"unknown group/v1 pair", v1alpha1.PlatformKubernetes,
			[]map[string]interface{}{doc("unregistered.invalid/v1", "DoesNotExist")}},
		{"kind not known to its group", v1alpha1.PlatformKubernetes,
			[]map[string]interface{}{doc("apps/v1", "NotARealAppKind")}},
		{"alpha API version", v1alpha1.PlatformKubernetes,
			[]map[string]interface{}{doc("storage.k8s.io/v1alpha1", "CSIDriver")}},
		{"OCP API on an upstream render", v1alpha1.PlatformKubernetes,
			[]map[string]interface{}{doc("security.openshift.io/v1", "SecurityContextConstraints")}},
	}
	for _, tc := range cases {
		v := auditSet(tc.docs, tc.platform, false, tc.desc)
		if len(v) == 0 {
			t.Errorf("%s: auditSet accepted a document that must be rejected", tc.desc)
		}
	}
	// A legitimate allow-listed kind on the right platform is clean.
	if v := auditSet([]map[string]interface{}{doc("apps/v1", "Deployment")},
		v1alpha1.PlatformKubernetes, false, "positive"); len(v) != 0 {
		t.Errorf("positive fixture produced violations: %v", v)
	}
	// SCC is allowed on the openshift platform set.
	if v := auditSet([]map[string]interface{}{doc("security.openshift.io/v1", "SecurityContextConstraints")},
		v1alpha1.PlatformOpenShift, false, "scc-ocp"); len(v) != 0 {
		t.Errorf("SCC on openshift must be allow-listed, got: %v", v)
	}
}

// buildKustomizePath renders one composition path with the pinned module.
func buildKustomizePath(t *testing.T, root, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", kustomizeModule, "build",
		root+"/"+path)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if looksLikeModuleLookupFailure(stderr.String()) {
			t.Skipf("SKIP: cannot resolve kustomize %s offline (no module cache, no network): %v — this run does NOT prove the render contract", kustomizeModule, err)
		}
		t.Fatalf("kustomize build %s failed: %v\nstderr: %s", path, err, stderr.String())
	}
	return stdout.String()
}

// runtimeRenderSet renders the runtime NFS operand set for one platform via
// the real renderer (not kustomize) and converts it to unstructured maps
// with the client-go scheme GVK.
func runtimeRenderSet(t *testing.T, platform v1alpha1.Platform) []renderSet {
	t.Helper()
	profile := renderer.For(renderer.ProfileInput{
		Profile:     v1alpha1.ProfileNFS,
		SecretName:  "quartet-audit-secret",
		DriverImage: "quay.io/ngxstorage/nfs-csi-ngxstorage-driver@sha256:" + strings.Repeat("a", 64),
		LogLevel:    "info",
		Platform:    platform,
	})
	if profile == nil {
		t.Fatalf("renderer.For returned nil for nfs/%s", platform)
	}
	docs := []map[string]interface{}{}
	for _, obj := range profile.Objects() {
		gvks, _, err := scheme.Scheme.ObjectKinds(obj)
		if err != nil || len(gvks) != 1 {
			t.Fatalf("resolve GVK for %T: %v %v", obj, gvks, err)
		}
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			t.Fatalf("convert %T to unstructured: %v", obj, err)
		}
		u["apiVersion"] = gvks[0].GroupVersion().String()
		u["kind"] = gvks[0].Kind
		docs = append(docs, u)
	}
	return []renderSet{{
		name:     "runtime-nfs-" + string(platform),
		platform: platform,
		docs:     docs,
	}}
}
