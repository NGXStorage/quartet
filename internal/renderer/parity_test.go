package renderer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
)

// --- Golden parity (regeneration independence / the normalized comparison rule). ---
//
// The golden fixtures (testdata/nfs-{k8s,ocp}-golden.yaml) are a FIELD-LEVEL
// MIRROR of the pinned raw NFS renders (source commit
// recorded in each golden header) with ONLY the approved
// transforms layered on — they are NOT byte-identical to the raw render:
//
// StorageClass + VolumeSnapshotClass dropped (renderer never creates)
// volumeattributesclasses ClusterRole rule dropped ClusterRole rule dropped; provisioner+resizer
// --feature-gates=VolumeAttributesClass=true -> =false (helm canonical)
// OCP node DaemonSet worker-pin + openshift.io/required-scc kept
// --log=info stays UNQUOTED (renderer writes spec.logLevel raw)
// driver digest -> parity reference (sidecars stay renderer constants)
//
// This test compares the renderer output document-by-document, field-by-field
// against that golden set. It is the "renderer must not deviate" pin.
//
// Regeneration independence: the golden must not silently
// drift from the raw source it mirrors. `make golden-verify` re-renders the
// upstream raw manifests with kustomize, applies ONLY these approved
// transforms, and field-compares against the golden — WITHOUT going through
// this package's renderer code, so a shared renderer+golden bug cannot hide.

const goldenSecret = "nfs-csi-ngxstorage-config"

// goldenDriverImage is the digest parity reference substituted into the golden
// (the golden otherwise carries the raw pinned digest).
var goldenDriverImage = "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("a", 64)

// goldenInput is the exact spec projection used to render the parity set, so
// the renderer and golden are compared like-for-like.
func goldenInput(platform v1alpha1.Platform) ProfileInput {
	return ProfileInput{
		Profile:     "nfs",
		SecretName:  goldenSecret,
		DriverImage: goldenDriverImage,
		LogLevel:    "info",
		Platform:    platform,
	}
}

// loadGolden parses a multi-doc golden YAML file into kind/name-keyed JSON
// maps.
func loadGolden(t *testing.T, path string) map[string]map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	docs := splitYAMLDocs(string(raw))
	out := make(map[string]map[string]interface{}, len(docs))
	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" || trimmed == "---" {
			continue
		}
		b, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			t.Fatalf("golden yaml->json %s: %v", path, err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("golden unmarshal %s: %v", path, err)
		}
		if _, hasKind := m["kind"]; !hasKind {
			// A leading comment-only block (generated-manifest headers)
			// splits as a document with no kind — skip it, not index it.
			continue
		}
		out[docKey(m)] = m
	}
	return out
}

func splitYAMLDocs(s string) []string {
	var docs []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line == "---" {
			docs = append(docs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	docs = append(docs, cur.String())
	return docs
}

func docKey(m map[string]interface{}) string {
	kind, _ := m["kind"].(string)
	md, _ := m["metadata"].(map[string]interface{})
	name, _ := md["name"].(string)
	ns, _ := md["namespace"].(string)
	return fmt.Sprintf("%s/%s/%s", kind, ns, name)
}

// parityScheme resolves GVKs for the typed operand objects before they are
// marshaled (typed literals carry empty TypeMeta, which would drop `kind`
// from the JSON and break doc-keying).
var parityScheme = newParityScheme()

func newParityScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

// renderedAsJSON converts the renderer output into the same kind/name-keyed
// JSON maps as the golden.
func renderedAsJSON(t *testing.T, objs []client.Object) map[string]map[string]interface{} {
	t.Helper()
	out := make(map[string]map[string]interface{}, len(objs))
	for _, obj := range objs {
		u := toUnstructuredMap(t, obj)
		out[docKey(u)] = u
	}
	return out
}

func toUnstructuredMap(t *testing.T, obj client.Object) map[string]interface{} {
	t.Helper()
	gvks, _, err := parityScheme.ObjectKinds(obj)
	if err != nil {
		t.Fatalf("resolve GVK for %T: %v", obj, err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("render marshal %T: %v", obj, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("render unmarshal %T: %v", obj, err)
	}
	kind, _ := m["kind"].(string)
	return pruneZeros(kind, m).(map[string]interface{})
}

// pruneZeros implements the normalized comparison deterministic normalization between the
// Go-typed render and the kustomize/helm golden. Typed structs serialize a
// few ZERO-VALUE fields that the cluster renderer omits; exactly those are
// pruned (and only those):
//
// - any null value (metadata.creationTimestamp: null)
// - top-level status: {} (Deployment/DaemonSet/Service typed status)
// - Lease.spec: {} (LeaseSpec is a value type; golden leases have none)
// - container resources: {} (the resource-undefined contract: resources undefined in all four sets)
// - DaemonSet updateStrategy: {} (DaemonSetUpdateStrategy is a value type,
// so an unset strategy serializes as {}; raw FC sources omit the field
// and kube applies the same default — the kube-defaulting behavior)
//
// Everything else — including deliberate empty maps like `emptyDir: {}` and
// the pod `securityContext: {}`, and explicit empty lists like the SCC's
// `groups: []` — is preserved as real golden content.
func pruneZeros(kind string, v interface{}) interface{} {
	return pruneZerosAt(kind, "", v)
}

// pruneDrop signals "delete this key" from the recursive prune.
var pruneDrop = struct{}{}

func pruneZerosAt(kind, path string, v interface{}) interface{} {
	switch tv := v.(type) {
	case map[string]interface{}:
		for k, val := range tv {
			child := k
			if path != "" {
				child = path + "." + k
			}
			if k == "status" && path == "" {
				if _, isMap := val.(map[string]interface{}); isMap {
					delete(tv, k) // typed status zero value
					continue
				}
			}
			if kind == "Lease" && k == "spec" && path == "" {
				delete(tv, k) // LeaseSpec zero value
				continue
			}
			if kind == "DaemonSet" && k == "updateStrategy" && path == "spec" {
				if m, isMap := val.(map[string]interface{}); isMap && len(m) == 0 {
					delete(tv, k) // DaemonSetUpdateStrategy zero value (field unset)
					continue
				}
			}
			if strings.Contains(path, ".containers[]") && k == "resources" {
				if m, isMap := val.(map[string]interface{}); isMap && len(m) == 0 {
					delete(tv, k)
					continue
				}
			}
			r := pruneZerosAt(kind, child, val)
			if r == pruneDrop {
				delete(tv, k)
			} else {
				tv[k] = r
			}
		}
		return tv
	case []interface{}:
		for i, el := range tv {
			r := pruneZerosAt(kind, path+"[]", el)
			if r == pruneDrop {
				tv[i] = nil
			} else {
				tv[i] = r
			}
		}
		return tv
	default:
		if v == nil {
			return pruneDrop
		}
		return v
	}
}

// TestRenderParityK8s pins the NFS Kubernetes operand set against the pinned
// golden.
func TestRenderParityK8s(t *testing.T) {
	p := For(goldenInput(v1alpha1.PlatformKubernetes))
	if p == nil {
		t.Fatal("nfs profile must render on kubernetes")
	}
	want := loadGolden(t, filepath.Join("testdata", "nfs-k8s-golden.yaml"))
	got := renderedAsJSON(t, p.Objects())
	assertSetEqual(t, want, got)
}

// TestRenderParityOCP pins the NFS OpenShift operand set (SCC, scc-use RBAC,
// worker-pin, required-scc) against the pinned golden.
func TestRenderParityOCP(t *testing.T) {
	p := For(goldenInput(v1alpha1.PlatformOpenShift))
	if p == nil {
		t.Fatal("nfs profile must render on openshift")
	}
	want := loadGolden(t, filepath.Join("testdata", "nfs-ocp-golden.yaml"))
	got := renderedAsJSON(t, p.Objects())
	assertSetEqual(t, want, got)
}

// assertSetEqual compares two document sets field-by-field, reporting the
// count of field-groups compared and every discrepancy.
func assertSetEqual(t *testing.T, want, got map[string]map[string]interface{}) {
	t.Helper()
	missing := diffKeys(want, got)
	extra := diffKeys(got, want)
	for _, k := range missing {
		t.Errorf("renderer MISSING golden document %s", k)
	}
	for _, k := range extra {
		t.Errorf("renderer EMITTED non-golden document %s", k)
	}
	if len(missing)+len(extra) > 0 {
		return
	}
	groups := 0
	for k, w := range want {
		groups += compareJSON(t, w, got[k], k, 0)
	}
	t.Logf("parity OK: %d documents, %d field-groups compared, 0 diffs", len(want), groups)
}

func diffKeys(want, got map[string]map[string]interface{}) []string {
	var out []string
	for k := range want {
		if _, ok := got[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// compareJSON recursively compares want (golden) against got (rendered).
// Lists of name-keyed objects (containers, env, volumes, mounts, rules
// subjects) compare as maps; scalar lists compare as sets — because kustomize
// sorts render lists and the renderer emits canonical order, neither side's
// ordering is meaningful for parity. Returns the number of compared groups.
func compareJSON(t *testing.T, want, got interface{}, path string, depth int) int {
	groups := 0
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !ok {
			t.Errorf("%s: golden is object, rendered is %T", path, got)
			return 0
		}
		for k, wv := range w {
			if gv, ok := g[k]; ok {
				groups += compareJSON(t, wv, gv, path+"."+k, depth+1)
			} else {
				t.Errorf("%s.%s: present in golden, absent in rendered", path, k)
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				t.Errorf("%s.%s: rendered has extra field absent from golden", path, k)
			}
		}
	case []interface{}:
		groups += compareList(t, w, got, path)
	default:
		if !jsonEqual(want, got) {
			t.Errorf("%s: golden=%v rendered=%v", path, want, got)
		}
		groups++
	}
	return groups
}

func compareList(t *testing.T, want []interface{}, got interface{}, path string) int {
	groups := 0
	g, ok := got.([]interface{})
	if !ok {
		t.Errorf("%s: golden is list, rendered is %T", path, got)
		return 0
	}
	// Name-keyed object lists compare as maps keyed by name.
	if len(want) > 0 && isNameKeyed(want) {
		wm, gm := nameIndex(want), nameIndex(g)
		for k, wv := range wm {
			if gv, ok := gm[k]; ok {
				groups += compareJSON(t, wv, gv, fmt.Sprintf("%s[%s]", path, k), 0)
			} else {
				t.Errorf("%s[%s]: golden name absent in rendered", path, k)
			}
		}
		for k := range gm {
			if _, ok := wm[k]; !ok {
				t.Errorf("%s[%s]: rendered name absent in golden", path, k)
			}
		}
		return groups
	}
	// Scalar / non-name lists compare as sets.
	if len(want) != len(g) {
		t.Errorf("%s: golden len=%d rendered len=%d (golden=%v rendered=%v)", path, len(want), len(g), want, g)
		return 0
	}
	wset, gset := asSet(want), asSet(g)
	for _, v := range wset {
		if !containsSet(gset, v) {
			t.Errorf("%s: golden element %v not in rendered", path, v)
		}
	}
	for _, v := range gset {
		if !containsSet(wset, v) {
			t.Errorf("%s: rendered element %v not in golden", path, v)
		}
	}
	return len(want) + 1
}

func isNameKeyed(list []interface{}) bool {
	for _, e := range list {
		m, ok := e.(map[string]interface{})
		if !ok {
			return false
		}
		if _, hasName := m["name"]; !hasName {
			return false
		}
	}
	return true
}

func nameIndex(list []interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]interface{}); ok {
			if n, ok := m["name"].(string); ok {
				out[n] = e
			}
		}
	}
	return out
}

func asSet(list []interface{}) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, canonicalJSON(e))
	}
	return out
}

func containsSet(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func canonicalJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonEqual(a, b interface{}) bool {
	return canonicalJSON(a) == canonicalJSON(b)
}

// --- "Must-not-render" pins (regeneration independence / the must-not-render matrix). ---

// forbiddenKinds are resource kinds the Quartet NFS renderer must NEVER emit
// (the must-not-render matrix: no Secret (secretRef-only), no SC/VSC, helm-only test Pod).
var forbiddenKinds = map[string]bool{
	"StorageClass":        true,
	"VolumeSnapshotClass": true,
	"Secret":              true,
	"Pod":                 true, // helm test-connection hook
}

// TestRenderEmitsNoForbiddenResources asserts the owner-matrix negatives on
// both platforms.
func TestRenderEmitsNoForbiddenResources(t *testing.T) {
	for _, platform := range []v1alpha1.Platform{v1alpha1.PlatformKubernetes, v1alpha1.PlatformOpenShift} {
		p := For(goldenInput(platform))
		for _, obj := range p.Objects() {
			gvks, _, err := parityScheme.ObjectKinds(obj)
			if err != nil {
				t.Fatalf("resolve GVK for %T: %v", obj, err)
			}
			kind := gvks[0].Kind
			if forbiddenKinds[kind] {
				t.Errorf("platform %s: renderer emitted forbidden kind %s %q (secretRef-only, no SC/VSC, helm-only)", platform, kind, obj.GetName())
			}
		}
	}
}

// TestRenderSpecFieldsPropagate pins digest pin, canonical env/secret names, unquoted log arg spec projection:
// - the driver image reference lands ONLY on the driver containers (digest pin:
// sidecars stay renderer constants);
// - the secretRef name lands ONLY in env secretKeyRefs (secretRef-only, canonical env/secret names);
// - logLevel lands ONLY in --log=<level>, unquoted (unquoted log arg).
func TestRenderSpecFieldsPropagate(t *testing.T) {
	img := "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("b", 64)
	secret := "custom-secret-name"
	p := For(ProfileInput{
		Profile: "nfs", SecretName: secret, DriverImage: img, LogLevel: "debug",
		Platform: v1alpha1.PlatformKubernetes,
	})
	deploy := deploymentOf(t, p.Objects())

	canonical := map[string]bool{}
	for _, d := range sidecarDigests() {
		canonical[d] = true
	}
	driverSeen := 0
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Image == img {
			driverSeen++
			for _, a := range c.Args {
				if strings.HasPrefix(a, "--log") && a != "--log=debug" {
					t.Errorf("unquoted log arg violated: --log arg %q must be exactly --log=debug (unquoted)", a)
				}
			}
		} else {
			// digest pin: every non-driver container carries a canonical sidecar
			// digest, never the spec image.
			digest := c.Image[strings.LastIndex(c.Image, ":")+1:]
			if !canonical[digest] {
				t.Errorf("container %q image %q is neither the spec driver image nor a canonical sidecar digest", c.Name, c.Image)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if sk := e.ValueFrom.SecretKeyRef; sk != nil && sk.Name != secret {
				t.Errorf("container %q env %q secretKeyRef name %q != spec %q", c.Name, e.Name, sk.Name, secret)
			}
		}
	}
	if driverSeen != 1 {
		t.Fatalf("driver image present %d times in controller Deployment, want exactly 1", driverSeen)
	}
}

func sidecarDigests() []string {
	return []string{
		nfsProvisionerDigest, nfsAttacherDigest, nfsResizerDigest,
		nfsSnapshotterDigest, nfsLivenessProbeDigest, nfsRegistrarDigest,
	}
}

func deploymentOf(t *testing.T, objs []client.Object) *appsv1.Deployment {
	t.Helper()
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok {
			return d
		}
	}
	t.Fatal("no Deployment in rendered set")
	return nil
}

func TestRenderDocumentCounts(t *testing.T) {
	// k8s = 141 operand docs (no SCC/scc-use RBAC); ocp = +3 (SCC, scc-use
	// Role, scc-use RoleBinding) = 144. Matches the pinned doc counts minus the
	// dropped SC/VSC (143-2 and 146-2).
	if p := For(goldenInput(v1alpha1.PlatformKubernetes)); len(p.Objects()) != 141 {
		t.Errorf("k8s operand count = %d, want 141", len(p.Objects()))
	}
	if p := For(goldenInput(v1alpha1.PlatformOpenShift)); len(p.Objects()) != 144 {
		t.Errorf("ocp operand count = %d, want 144", len(p.Objects()))
	}
}

// TestSCCIsOpenShiftOnly pins the SCC/scc-use RBAC to OCP and the absence of
// SCC on plain Kubernetes (normalized SCC comparison).
func TestSCCIsOpenShiftOnly(t *testing.T) {
	hasSCC := func(platform v1alpha1.Platform) bool {
		p := For(goldenInput(platform))
		for _, obj := range p.Objects() {
			if obj.GetObjectKind().GroupVersionKind().Kind == "SecurityContextConstraints" {
				return true
			}
		}
		return false
	}
	if hasSCC(v1alpha1.PlatformKubernetes) {
		t.Error("SCC must not render on kubernetes")
	}
	if !hasSCC(v1alpha1.PlatformOpenShift) {
		t.Error("SCC must render on openshift")
	}
}

// TestNodeWorkerPinOnlyOnOCP pins placement rule placement: OCP node DS is worker-pinned
// + carries required-scc; Kubernetes node DS is os-only with no required-scc.
func TestNodeWorkerPinOnlyOnOCP(t *testing.T) {
	dsNodeSelector := func(platform v1alpha1.Platform) (map[string]string, map[string]string) {
		p := For(goldenInput(platform))
		for _, obj := range p.Objects() {
			if ds, ok := obj.(*appsv1.DaemonSet); ok {
				return ds.Spec.Template.Spec.NodeSelector, ds.Spec.Template.Annotations
			}
		}
		return nil, nil
	}
	ocpSel, ocpAnn := dsNodeSelector(v1alpha1.PlatformOpenShift)
	if _, pinned := ocpSel["node-role.kubernetes.io/worker"]; !pinned {
		t.Errorf("ocp node DS must be worker-pinned, got selector %v", ocpSel)
	}
	if ocpAnn["openshift.io/required-scc"] != "nfs-csi-ngxstorage-node-scc" {
		t.Errorf("ocp node DS must carry required-scc, got %v", ocpAnn)
	}
	k8sSel, k8sAnn := dsNodeSelector(v1alpha1.PlatformKubernetes)
	if _, pinned := k8sSel["node-role.kubernetes.io/worker"]; pinned {
		t.Errorf("k8s node DS must NOT be worker-pinned, got %v", k8sSel)
	}
	if _, has := k8sAnn["openshift.io/required-scc"]; has {
		t.Errorf("k8s node DS must NOT carry required-scc, got %v", k8sAnn)
	}
	if k8sSel["kubernetes.io/os"] != "linux" {
		t.Errorf("k8s node DS must be os=linux, got %v", k8sSel)
	}
}
