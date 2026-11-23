// golden-verify re-renders the upstream raw manifests (NFS, FC, iSCSI) with
// kustomize and field-compares the result against the Quartet golden
// fixtures (internal/renderer/testdata/{nfs,fc,iscsi}-{k8s,ocp}-golden.yaml).
//
// Usage:
//
//	go run ./hack/golden-verify <nfs|fc|iscsi> <k8s|ocp> <raw-render.yaml> <golden.yaml>
//	go run ./hack/golden-verify emit <nfs|fc|iscsi> <k8s|ocp> <raw-render.yaml> <out.yaml>
//
// The golden is NOT byte-identical to the raw render: it is a field-level
// mirror of the raw with ONLY the approved transforms applied (the
// allow-list per profile is the profileSpec table below and is documented
// in each golden's header):
//
//	nfs SC/VSC dropped, VAC rule dropped + gate args closed,
//
// OCP node worker-pin/required-scc ensured, digest swap.
//
//	fc SC/VSC dropped, env/secret-name set -> Helm canonical, controller
//
// free placement (raw worker-pin stripped from the controller
// Deployment only), digest swap.
//
//	iscsi SC/VSC dropped, VAC rule dropped (no gate args in raw), env/secret-
//
// name set -> Helm canonical, OCP node worker-pin/required-scc
// ensured, digest swap.
//
// Exit 0 = identical (regeneration independent of the renderer code path);
// exit 1 = diffs reported. The script path (hack/verify-golden.sh) pins the
// kustomize version and the raw source tree (checkout-external from the
// clean driver commits recorded in the golden headers) for CI/packaging use.
// The emit mode writes the transformed raw as YAML — a regeneration aid;
// the golden's provenance header is maintained by hand and the verify mode
// is the proof.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// profileSpec is the approved transform allow-list for ONE profile's raw
// render. The values are pinned, not invented: the raw driver images are
// the digests the pinned raw manifests carry, the parity images are the
// references the golden headers name, and every other cell is documented
// in the golden headers.
type profileSpec struct {
	// base is the "<profile>-csi-ngxstorage" name prefix of the pinned raw.
	base string
	// rawDriverImage / goldenDriverImage are the digest-swap pair.
	rawDriverImage    string
	goldenDriverImage string
	// dropVACRule: drop the raw-only volumeattributesclasses
	// ClusterRole rule (never present in the FC raw).
	dropVACRule bool
	// rewriteVACGate: close --feature-gates=VolumeAttributesClass=true
	// to =false (NFS raw only; FC/iSCSI raw sidecars carry no such arg).
	rewriteVACGate bool
	// envRenames: raw env name -> golden env name; args $(...) refs
	// follow. Empty for NFS (its raw set is already canonical).
	envRenames map[string]string
	// rawSecretName / paritySecretName: secretKeyRef name swap.
	rawSecretName    string
	paritySecretName string
	// freeController: strip the worker-pin from the controller Deployment
	// ONLY (FC raw carries it; the canonical default is free placement,
	// NFS/iSCSI raw behavior). The node DS worker-pin stays — canonical
	// for all three drivers.
	freeController bool
}

var profiles = map[string]profileSpec{
	"nfs": {
		base:              "nfs-csi-ngxstorage",
		rawDriverImage:    "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:809528ed53c084730c1f645c6b8f4d8b04dfaac2a4ca3fbee81059606e39243b",
		goldenDriverImage: "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + strings.Repeat("a", 64),
		dropVACRule:       true,
		rewriteVACGate:    true,
	},
	"fc": {
		base:              "fc-csi-ngxstorage",
		rawDriverImage:    "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:3d6542d7d65d679aaf6e2b03625011700baee082ae09b11959244f9874310604",
		goldenDriverImage: "quay.io/ngxstorage/fc-csi-ngxstorage@sha256:" + strings.Repeat("a", 64),
		envRenames:        map[string]string{"POOL_NAME": "STORAGE_POOL_NAME", "API_KEY": "STORAGE_API_KEY"},
		rawSecretName:     "fc-csi-ngxstorage-secret",
		paritySecretName:  "fc-csi-ngxstorage-config",
		freeController:    true,
	},
	"iscsi": {
		base:              "iscsi-csi-ngxstorage",
		rawDriverImage:    "quay.io/ngxstorage/iscsi-csi-ngxstorage@sha256:0988d15112dabb97f713e875d77ab62c98ae7a4d40aaa2544a057cf349fc1c87",
		goldenDriverImage: "quay.io/ngxstorage/iscsi-csi-ngxstorage@sha256:" + strings.Repeat("a", 64),
		dropVACRule:       true,
		// rewriteVACGate stays false: the pinned raw sidecars carry no
		// --feature-gates arg, so the VAC closure drops the rule only.
		envRenames:       map[string]string{"POOL_NAME": "STORAGE_POOL_NAME", "API_KEY": "STORAGE_API_KEY"},
		rawSecretName:    "iscsi-csi-ngxstorage-secret",
		paritySecretName: "iscsi-csi-ngxstorage-config",
	},
}

var droppedKinds = map[string]bool{"StorageClass": true, "VolumeSnapshotClass": true}

func main() {
	args := os.Args[1:]
	mode := "verify"
	if len(args) > 0 && args[0] == "emit" {
		mode = "emit"
		args = args[1:]
	}
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: golden-verify <nfs|fc|iscsi> <k8s|ocp> <raw.yaml> <golden.yaml>\n"+
			"       golden-verify emit <nfs|fc|iscsi> <k8s|ocp> <raw.yaml> <out.yaml>")
		os.Exit(2)
	}
	profile, platform, inPath, outPath := args[0], args[1], args[2], args[3]
	spec, ok := profiles[profile]
	if !ok {
		fmt.Fprintf(os.Stderr, "profile must be nfs, fc or iscsi (got %q)\n", profile)
		os.Exit(2)
	}
	ocp := platform == "ocp"
	if platform != "k8s" && !ocp {
		fmt.Fprintln(os.Stderr, "platform must be k8s or ocp")
		os.Exit(2)
	}
	in, err := os.ReadFile(inPath)
	if err != nil {
		fatal("read raw render: %v", err)
	}

	// transform applies ONLY the approved transforms for this profile and
	// returns kind/ns/name-keyed JSON maps in document order.
	docs, order := transform(string(in), spec, ocp)

	if mode == "emit" {
		var buf strings.Builder
		for i, key := range order {
			if i > 0 {
				buf.WriteString("---\n")
			}
			b, err := json.Marshal(docs[key])
			if err != nil {
				fatal("emit json: %v", err)
			}
			y, err := yaml.JSONToYAML(b)
			if err != nil {
				fatal("emit yaml: %v", err)
			}
			buf.Write(y)
		}
		if err := os.WriteFile(outPath, []byte(buf.String()), 0o644); err != nil {
			fatal("write %s: %v", outPath, err)
		}
		fmt.Printf("emit %s/%s: %d docs written to %s\n", profile, platform, len(order), outPath)
		return
	}

	golden, err := os.ReadFile(outPath)
	if err != nil {
		fatal("read golden: %v", err)
	}
	goldenDocs := loadGolden(string(golden))

	groups := compare(docs, goldenDocs)
	fmt.Printf("profile=%s platform=%s: raw %d docs (after approved transforms) vs golden %d docs; %d field-groups compared\n",
		profile, platform, len(docs), len(goldenDocs), groups)
	if diffCount == 0 {
		fmt.Printf("golden-verify OK (%s/%s): field-level mirror matches (regeneration independent of the renderer)\n",
			profile, platform)
		return
	}
	fmt.Printf("golden-verify MISMATCH (%s/%s): %d diff(s) reported above\n", profile, platform, diffCount)
	os.Exit(1)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func transform(raw string, spec profileSpec, ocp bool) (map[string]map[string]interface{}, []string) {
	out := map[string]map[string]interface{}{}
	var order []string
	for _, doc := range splitYAMLDocs(raw) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" || trimmed == "---" {
			continue
		}
		b, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			fatal("raw yaml->json: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err != nil {
			fatal("raw unmarshal: %v", err)
		}
		kind, _ := m["kind"].(string)
		if droppedKinds[kind] {
			continue // SC/VSC are absent by contract
		}
		applyTransforms(m, kind, ocp, spec)
		key := docKey(m)
		out[key] = pruneZeros(kind, m).(map[string]interface{})
		order = append(order, key)
	}
	return out, order
}

func applyTransforms(m map[string]interface{}, kind string, ocp bool, spec profileSpec) {
	// Drop the raw-only volumeattributesclasses ClusterRole rule.
	if spec.dropVACRule && kind == "ClusterRole" {
		md, _ := m["metadata"].(map[string]interface{})
		if name, _ := md["name"].(string); name == spec.base+"-controller" {
			rules, _ := m["rules"].([]interface{})
			kept := make([]interface{}, 0, len(rules))
			for _, r := range rules {
				rm, _ := r.(map[string]interface{})
				res, _ := rm["resources"].([]interface{})
				hasVAC := false
				for _, x := range res {
					if x == "volumeattributesclasses" {
						hasVAC = true
					}
				}
				if !hasVAC {
					kept = append(kept, r)
				}
			}
			m["rules"] = kept
		}
	}
	// Driver digest -> parity reference; VAC gate closed;
	// env names + secretKeyRef names canonicalized, args $(...) follow.
	for _, c := range containersOf(m) {
		args, _ := c["args"].([]interface{})
		for i, a := range args {
			s, _ := a.(string)
			if spec.rewriteVACGate && s == "--feature-gates=VolumeAttributesClass=true" {
				s = "--feature-gates=VolumeAttributesClass=false"
			}
			for raw, golden := range spec.envRenames {
				s = strings.ReplaceAll(s, "$("+raw+")", "$("+golden+")")
			}
			args[i] = s
		}
		if img, _ := c["image"].(string); img == spec.rawDriverImage {
			c["image"] = spec.goldenDriverImage
		}
		env, _ := c["env"].([]interface{})
		for _, e := range env {
			em, _ := e.(map[string]interface{})
			if em == nil {
				continue
			}
			if raw, _ := em["name"].(string); spec.envRenames[raw] != "" {
				em["name"] = spec.envRenames[raw]
			}
			if spec.rawSecretName != "" {
				if vf, _ := em["valueFrom"].(map[string]interface{}); vf != nil {
					if sk, _ := vf["secretKeyRef"].(map[string]interface{}); sk != nil {
						if name, _ := sk["name"].(string); name == spec.rawSecretName {
							sk["name"] = spec.paritySecretName
						}
					}
				}
			}
		}
	}
	// FC controller free placement — strip the raw worker-pin from the
	// controller Deployment only (os=linux and the tolerations stay).
	if spec.freeController && kind == "Deployment" {
		if nodeName(m) == spec.base+"-controller" {
			sel := podNodeSelector(m)
			if sel != nil {
				delete(sel, "node-role.kubernetes.io/worker")
			}
		}
	}
	// OCP node DaemonSet worker-pin + required-scc (idempotent ensure;
	// all three profiles — the pinned raw already carries it for FC/iSCSI).
	if ocp && kind == "DaemonSet" {
		if nodeName(m) != spec.base+"-node" {
			return
		}
		pspec := podSpec(m)
		sel := podNodeSelectorFrom(pspec)
		if sel == nil {
			sel = map[string]interface{}{}
			pspec["nodeSelector"] = sel
		}
		sel["node-role.kubernetes.io/worker"] = ""
		// required-scc sits on the pod template METADATA, not the template.
		templ := template(m)
		tmeta, _ := templ["metadata"].(map[string]interface{})
		if tmeta == nil {
			tmeta = map[string]interface{}{}
			templ["metadata"] = tmeta
		}
		ann, _ := tmeta["annotations"].(map[string]interface{})
		if ann == nil {
			ann = map[string]interface{}{}
			tmeta["annotations"] = ann
		}
		ann["openshift.io/required-scc"] = spec.base + "-node-scc"
	}
}

func nodeName(m map[string]interface{}) string {
	md, _ := m["metadata"].(map[string]interface{})
	name, _ := md["name"].(string)
	return name
}

func template(m map[string]interface{}) map[string]interface{} {
	spec, _ := m["spec"].(map[string]interface{})
	templ, _ := spec["template"].(map[string]interface{})
	return templ
}

func podSpec(m map[string]interface{}) map[string]interface{} {
	templ := template(m)
	pspec, _ := templ["spec"].(map[string]interface{})
	return pspec
}

func podNodeSelector(m map[string]interface{}) map[string]interface{} {
	return podNodeSelectorFrom(podSpec(m))
}

func podNodeSelectorFrom(pspec map[string]interface{}) map[string]interface{} {
	sel, _ := pspec["nodeSelector"].(map[string]interface{})
	return sel
}

func containersOf(m map[string]interface{}) []map[string]interface{} {
	pspec := podSpec(m)
	raw, _ := pspec["containers"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, c := range raw {
		if cm, ok := c.(map[string]interface{}); ok {
			out = append(out, cm)
		}
	}
	return out
}

// --- Golden loading + comparison (same semantics as parity_test.go). ---

func loadGolden(raw string) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for _, doc := range splitYAMLDocs(raw) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" || trimmed == "---" {
			continue
		}
		b, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			fatal("golden yaml->json: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err != nil {
			fatal("golden unmarshal: %v", err)
		}
		// A segment that parses to NO object is the provenance header (or a
		// bare trailing separator), not an operand — skip it so the header
		// convention never fabricates a phantom "//" document.
		if len(m) == 0 {
			continue
		}
		out[docKey(m)] = pruneZeros(docKind(m), m).(map[string]interface{})
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

func docKind(m map[string]interface{}) string {
	k, _ := m["kind"].(string)
	return k
}

// pruneZeros mirrors parity_test.go exactly: nulls, top-level status{},
// Lease.spec{}, empty container resources{}.
func pruneZeros(kind string, v interface{}) interface{} {
	return pruneZerosAt(kind, "", v)
}

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
					delete(tv, k)
					continue
				}
			}
			if kind == "Lease" && k == "spec" && path == "" {
				delete(tv, k)
				continue
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
			if r := pruneZerosAt(kind, path+"[]", el); r == pruneDrop {
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

var diffCount int

func compare(want, got map[string]map[string]interface{}) int {
	groups := 0
	var missing, extra []string
	for k := range want {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	// missing = in want (raw) but not in got (golden); extra = the reverse.
	for _, k := range missing {
		addDiff(fmt.Sprintf("doc %s: raw-only (golden does not expect it)", k))
	}
	for _, k := range extra {
		addDiff(fmt.Sprintf("doc %s: golden-only (raw render lost it)", k))
	}
	for k, w := range want {
		if g, ok := got[k]; ok {
			groups += compareJSON(w, g, k, 0)
		}
	}
	return groups
}

// compareJSON mirrors parity_test.go: name-keyed lists compare as maps,
// scalar lists as sets. Returns field-groups compared; records diffs.
func compareJSON(want, got interface{}, path string, depth int) int {
	groups := 0
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !ok {
			addDiff(fmt.Sprintf("%s: raw is object, golden is %T", path, got))
			return 0
		}
		for k, wv := range w {
			if gv, ok := g[k]; ok {
				groups += compareJSON(wv, gv, path+"."+k, depth+1)
			} else {
				addDiff(fmt.Sprintf("%s.%s: present only in raw (golden lacks it)", path, k))
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				addDiff(fmt.Sprintf("%s.%s: present only in golden (raw lacks it)", path, k))
			}
		}
	case []interface{}:
		groups += compareList(w, got, path)
	default:
		if canonical(want) != canonical(got) {
			addDiff(fmt.Sprintf("%s: raw=%v golden=%v", path, want, got))
		}
		groups++
	}
	return groups
}

func compareList(want []interface{}, got interface{}, path string) int {
	groups := 0
	g, ok := got.([]interface{})
	if !ok {
		addDiff(fmt.Sprintf("%s: raw is list, golden is %T", path, got))
		return 0
	}
	if len(want) > 0 && isNameKeyed(want) {
		wm, gm := nameIndex(want), nameIndex(g)
		for k, wv := range wm {
			if gv, ok := gm[k]; ok {
				groups += compareJSON(wv, gv, fmt.Sprintf("%s[%s]", path, k), 0)
			} else {
				addDiff(fmt.Sprintf("%s[%s]: golden name absent in raw", path, k))
			}
		}
		for k := range gm {
			if _, ok := wm[k]; !ok {
				addDiff(fmt.Sprintf("%s[%s]: raw name absent in golden", path, k))
			}
		}
		return groups
	}
	if len(want) != len(g) {
		addDiff(fmt.Sprintf("%s: raw len=%d golden len=%d", path, len(want), len(g)))
		return 0
	}
	wset, gset := setOf(want), setOf(g)
	for v := range wset {
		if !gset[v] {
			addDiff(fmt.Sprintf("%s: golden lacks element %s", path, v))
		}
	}
	for v := range gset {
		if !wset[v] {
			addDiff(fmt.Sprintf("%s: raw lacks element %s", path, v))
		}
	}
	return len(want) + 1
}

func addDiff(s string) {
	diffCount++
	fmt.Printf("  %s\n", s)
}

func isNameKeyed(list []interface{}) bool {
	for _, e := range list {
		m, ok := e.(map[string]interface{})
		if !ok {
			return false
		}
		if _, has := m["name"]; !has {
			return false
		}
	}
	return true
}

func nameIndex(list []interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, e := range list {
		if m, ok := e.(map[string]interface{}); ok {
			if n, ok := m["name"].(string); ok {
				out[n] = e
			}
		}
	}
	return out
}

func setOf(list []interface{}) map[string]bool {
	out := map[string]bool{}
	for _, e := range list {
		out[canonical(e)] = true
	}
	return out
}

func canonical(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
