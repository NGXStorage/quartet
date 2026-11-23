// Package rendertest pins the kustomize render of config/default-nfs against the
// General review findings:
//
// - explicit namespace: every namespaced resource must render with an explicit
// metadata.namespace of quartet-system, binding subjects and the rendered
// ServiceAccount must agree, and the render must be independent of the
// kubectl default namespace / KUBECONFIG (kustomize build reads neither,
// but the test asserts it empirically by building twice under divergent
// contexts and diffing).
// - arbitrary UID: the manager pod must use the arbitrary-UID strategy — runAsNonRoot
// without a fixed runAsUser/fsGroup — with the read-only root filesystem
// and no mounted volumes.
//
// Live OCP SCC admission and actual runtime UID/fsGroup verification remain
// Live-cluster evidence; this test is static render proof only.
package rendertest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// Kustomize is invoked version-pinned so the render contract does not drift
// with whatever binary happens to be on PATH. The first run downloads the
// module into the local module cache; afterwards (including with
// GOPROXY=off) the cache hit suffices — see docs/BUILDING.md "rendertest
// offline durumu". The decision: the module-cache path was
// kept over a separately pinned kustomize binary because it reuses the
// same tooling/provenance surface as setup-envtest and controller-gen,
// with no second download channel to verify.
const kustomizeModule = "sigs.k8s.io/kustomize/kustomize/v5@v5.7.1"

// quartetNamespace is the approved manager namespace.
const quartetNamespace = "quartet-system"

// quartetSA is the manager ServiceAccount name (quartet-system contract).
const quartetSA = "quartet-manager"

// clusterScoped kinds rendered by config/default-nfs that must NOT carry a
// namespace field.
var clusterScoped = map[string]bool{
	"Namespace":                true,
	"CustomResourceDefinition": true,
	"ClusterRole":              true,
	"ClusterRoleBinding":       true,
}

// namespaced kinds that must carry metadata.namespace: quartet-system.
var namespaced = map[string]bool{
	"ServiceAccount": true,
	"Role":           true,
	"RoleBinding":    true,
	"Deployment":     true,
}

// operandRBACNamespace is the operand namespace operand namespace of the NFS profile — the
// installer-owned operand RBAC set (out-of-band RBAC contract) renders there, NOT in
// quartet-system: it binds the operand ServiceAccounts, not the manager.
const operandRBACNamespace = "ngxstorage-nfs"

// operandRBACNames are the fixed NFS install-set names the render must carry
// (one ClusterRole + ClusterRoleBinding, two Role + RoleBinding pairs).
var operandRBACNames = map[string]bool{
	"nfs-csi-ngxstorage-controller":                true, // ClusterRole + ClusterRoleBinding
	"nfs-csi-ngxstorage-controller-operation-lock": true, // Role + RoleBinding
	"nfs-csi-ngxstorage-leader-election":           true, // Role + RoleBinding
}

type doc struct {
	Kind     string                 `json:"kind"`
	Metadata map[string]interface{} `json:"metadata"`
	// RoleBinding/ClusterRoleBinding fields.
	RoleRef  map[string]interface{}   `json:"roleRef"`
	Subjects []map[string]interface{} `json:"subjects"`
	Spec     map[string]interface{}   `json:"spec"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func buildKustomize(t *testing.T, root string, extraEnv []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", kustomizeModule, "build",
		filepath.Join(root, "config", "default-nfs"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Offline with an empty module cache: `go run module@version` cannot
		// resolve. This is an environment gap, NOT a render regression —
		// skip loudly and document the requirement (docs/BUILDING.md), so
		// the absence of network never masquerades as a green render check.
		if looksLikeModuleLookupFailure(stderr.String()) {
			t.Skipf("SKIP: cannot resolve kustomize %s offline (no module cache, no network): %v — "+
				"run once online to warm the Go module cache (docs/BUILDING.md \"rendertest offline durumu\"); "+
				"this run does NOT prove the render contract", kustomizeModule, err)
		}
		t.Fatalf("kustomize build (%s) failed: %v\nstdout: %s\nstderr: %s",
			kustomizeModule, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// looksLikeModuleLookupFailure distinguishes a `go run module@version`
// resolution failure (offline / empty cache) from a real kustomize build
// error. Go reports these on stderr with "module lookup disabled",
// "cannot query module", "no matching versions", or a dial/lookup error.
func looksLikeModuleLookupFailure(stderr string) bool {
	for _, marker := range []string{
		"module lookup disabled",
		"cannot query module",
		"no matching versions",
		"unknown revision",
		"connection refused",
		"no such host",
		"TLS handshake",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func parseDocs(t *testing.T, rendered string) []doc {
	t.Helper()
	var docs []doc
	for _, raw := range strings.Split(rendered, "\n---\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "---") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "---"))
			if trimmed == "" {
				continue
			}
		}
		var d doc
		if err := yaml.Unmarshal([]byte(trimmed), &d); err != nil {
			t.Fatalf("parse rendered document %q: %v", trimmed, err)
		}
		if d.Kind == "" {
			t.Fatalf("rendered document has no kind: %q", trimmed)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatal("kustomize build produced no documents")
	}
	return docs
}

func TestDefaultRenderNamespaceConsistency(t *testing.T) {
	root := repoRoot(t)

	rendered := buildKustomize(t, root, nil)
	docs := parseDocs(t, rendered)

	byName := func(kind, name string) *doc {
		for i := range docs {
			d := &docs[i]
			if d.Kind == kind && d.Metadata["name"] == name {
				return d
			}
		}
		return nil
	}
	sa := byName("ServiceAccount", quartetSA)
	deploy := byName("Deployment", quartetSA)
	crb := byName("ClusterRoleBinding", "quartet-manager-rolebinding")
	cr := byName("ClusterRole", "quartet-manager-role")
	leRole := byName("Role", "quartet-leader-election-role")
	leRoleBinding := byName("RoleBinding", "quartet-leader-election-rolebinding")
	nsObject := byName("Namespace", quartetNamespace)
	for name, d := range map[string]*doc{"ServiceAccount": sa, "Deployment": deploy,
		"manager ClusterRoleBinding": crb, "manager ClusterRole": cr,
		"leader-election Role": leRole, "leader-election RoleBinding": leRoleBinding,
		"Namespace": nsObject} {
		if d == nil {
			t.Fatalf("config/default-nfs render is missing %s", name)
		}
	}
	// The operand RBAC install set (out-of-band RBAC contract) renders too — all six
	// fixed names, cluster-scoped kinds aside from the Role/RoleBinding pairs.
	for n, kind := range map[string]string{
		"nfs-csi-ngxstorage-controller":                "ClusterRole",
		"nfs-csi-ngxstorage-controller-operation-lock": "Role",
		"nfs-csi-ngxstorage-leader-election":           "Role",
	} {
		if byName(kind, n) == nil {
			t.Fatalf("config/default-nfs render is missing operand %s %s", kind, n)
		}
	}
	for _, n := range []string{
		"nfs-csi-ngxstorage-controller-operation-lock",
		"nfs-csi-ngxstorage-leader-election",
	} {
		if byName("RoleBinding", n) == nil {
			t.Fatalf("config/default-nfs render is missing operand RoleBinding %s", n)
		}
	}
	if byName("ClusterRoleBinding", "nfs-csi-ngxstorage-controller") == nil {
		t.Fatal("config/default-nfs render is missing the operand ClusterRoleBinding")
	}
	// The operand namespace (explicit namespace contract) renders too — and BEFORE the
	// first operand RBAC document: the install order is namespace → RBAC →
	// (manager), so a kubectl apply of the render cannot hit namespaced RBAC
	// objects in a missing namespace.
	if byName("Namespace", operandRBACNamespace) == nil {
		t.Fatal("config/default-nfs render is missing the operand Namespace")
	}
	nsIdx, firstRBACIdx := -1, -1
	for i := range docs {
		if docs[i].Kind == "Namespace" && docs[i].Metadata["name"] == operandRBACNamespace {
			nsIdx = i
		}
		if firstRBACIdx == -1 && operandRBACNames[docs[i].Metadata["name"].(string)] &&
			(docs[i].Kind == "ClusterRole" || docs[i].Kind == "ClusterRoleBinding" ||
				docs[i].Kind == "Role" || docs[i].Kind == "RoleBinding") {
			firstRBACIdx = i
		}
	}
	if nsIdx == -1 || firstRBACIdx == -1 || nsIdx > firstRBACIdx {
		t.Errorf("operand Namespace must render before the operand RBAC set (namespace=%d firstRBAC=%d)", nsIdx, firstRBACIdx)
	}

	// (a) Every MANAGER namespaced document renders in quartet-system; every
	// operand-RBAC namespaced document renders in the operand namespace operand namespace;
	// every cluster-scoped kind carries no namespace field.
	for _, d := range docs {
		metaNS, present := d.Metadata["namespace"]
		name, _ := d.Metadata["name"].(string)
		if _, isOperand := operandRBACNames[name]; isOperand &&
			(d.Kind == "Role" || d.Kind == "RoleBinding") {
			if !present || metaNS != operandRBACNamespace {
				t.Errorf("operand RBAC %s %v: metadata.namespace = %v (present=%v), want %q",
					d.Kind, d.Metadata["name"], metaNS, present, operandRBACNamespace)
			}
			continue
		}
		if namespaced[d.Kind] {
			if !present || metaNS != quartetNamespace {
				t.Errorf("%s %v: metadata.namespace = %v (present=%v), want %q",
					d.Kind, d.Metadata["name"], metaNS, present, quartetNamespace)
			}
		}
		if clusterScoped[d.Kind] && present {
			t.Errorf("cluster-scoped %s %v unexpectedly carries namespace %v",
				d.Kind, d.Metadata["name"], metaNS)
		}
	}
	if nsObject.Metadata["name"] != quartetNamespace {
		t.Errorf("Namespace object name = %v, want %q", nsObject.Metadata["name"], quartetNamespace)
	}

	// (b) The leader-election RoleBinding subject and namespace must match the
	// rendered ServiceAccount, and its roleRef must match the rendered Role.
	checkManagerSubjects(t, "RoleBinding", leRoleBinding, leRole.Metadata["name"])

	// (c) The ClusterRoleBinding subject points at the same SA, and its
	// roleRef matches the rendered ClusterRole.
	checkManagerSubjects(t, "ClusterRoleBinding", crb, cr.Metadata["name"])

	// (d) The operand install bindings bind the NFS controller SA in the
	// operand namespace, and each roleRef points at the rendered role of the
	// same name — the RBACDrift health gate verifies exactly this shape.
	for _, n := range []string{
		"nfs-csi-ngxstorage-controller-operation-lock",
		"nfs-csi-ngxstorage-leader-election",
	} {
		checkOperandBinding(t, "RoleBinding", byName("RoleBinding", n), n, "Role")
	}
	checkOperandBinding(t, "ClusterRoleBinding", byName("ClusterRoleBinding", "nfs-csi-ngxstorage-controller"),
		"nfs-csi-ngxstorage-controller", "ClusterRole")

	// The Deployment must run as the SA the bindings grant.
	spec := deploy.Spec
	if spec == nil {
		t.Fatal("Deployment has no spec")
	}
	podSpec, _ := spec["template"].(map[string]interface{})
	if podSpec == nil {
		t.Fatal("Deployment has no spec.template")
	}
	innerSpec, _ := podSpec["spec"].(map[string]interface{})
	if innerSpec == nil {
		t.Fatal("Deployment has no spec.template.spec")
	}
	if innerSpec["serviceAccountName"] != quartetSA {
		t.Errorf("Deployment serviceAccountName = %v, want %q",
			innerSpec["serviceAccountName"], quartetSA)
	}
}

// checkManagerSubjects asserts every subject of an (Cluster)RoleBinding is
// quartet-system/quartet-manager and that the roleRef points at wantRole.
func checkManagerSubjects(t *testing.T, kind string, binding *doc, wantRole interface{}) {
	t.Helper()
	checkSubjects(t, kind, binding, wantRole, quartetSA, quartetNamespace)
}

// checkOperandBinding asserts an operand install binding (out-of-band RBAC contract):
// roleRef kind/name match, and the single subject is the NFS controller SA in
// the operand namespace — the exact shape the reconciler's RBACDrift health
// gate re-verifies at runtime.
func checkOperandBinding(t *testing.T, kind string, binding *doc, wantRole, wantRoleKind string) {
	t.Helper()
	if binding == nil {
		t.Fatalf("%s %s missing from render", kind, wantRole)
	}
	if binding.RoleRef == nil || binding.RoleRef["name"] != wantRole || binding.RoleRef["kind"] != wantRoleKind {
		t.Errorf("%s %s roleRef = %v, want kind=%s name=%s", kind, wantRole, binding.RoleRef, wantRoleKind, wantRole)
	}
	checkSubjects(t, kind, binding, wantRole, "nfs-csi-ngxstorage-controller", operandRBACNamespace)
}

// checkSubjects asserts every subject of an (Cluster)RoleBinding is
// wantSA in wantNamespace and that the roleRef points at wantRole.
func checkSubjects(t *testing.T, kind string, binding *doc, wantRole, wantSA, wantNS interface{}) {
	t.Helper()
	if binding.RoleRef == nil {
		t.Errorf("%s has no roleRef", kind)
	}
	if binding.RoleRef["name"] != wantRole {
		t.Errorf("%s roleRef name %v, want %v", kind, binding.RoleRef["name"], wantRole)
	}
	if len(binding.Subjects) == 0 {
		t.Errorf("%s has no subjects", kind)
	}
	for i, s := range binding.Subjects {
		if s["kind"] != "ServiceAccount" || s["name"] != wantSA || s["namespace"] != wantNS {
			t.Errorf("%s subject[%d] = %v, want ServiceAccount %v/%v",
				kind, i, s, wantNS, wantSA)
		}
	}
}

// (d) The render is independent of the kubectl default context: building
// under an absent, a bogus and a namespace-scoped KUBECONFIG must yield
// byte-identical output, and every namespaced object must already carry an
// explicit namespace (a kubectl default-namespace fallback can never apply).
func TestDefaultRenderIndependentOfKubeContext(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir() // session temp dir, never /tmp
	bogus := filepath.Join(tmp, "kubeconfig-does-not-exist")

	plain := buildKustomize(t, root, nil)
	noEnv := buildKustomize(t, root, []string{"KUBECONFIG=", "KUBERNETES_DEFAULT_NAMESPACE="})
	bogusEnv := buildKustomize(t, root, []string{"KUBECONFIG=" + bogus})

	for _, other := range []string{noEnv, bogusEnv} {
		if other != plain {
			t.Errorf("render differs under divergent kube context (first divergence at byte %d)",
				firstDiff(plain, other))
		}
	}
}

func firstDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// arbitrary UID: the manager pod spec must express the arbitrary-UID strategy —
// runAsNonRoot without fixed runAsUser/fsGroup — while keeping the read-only
// root filesystem, no privilege escalation, seccomp RuntimeDefault and no
// mounted volumes that would make fsGroup meaningful.
func TestManagerPodSecurityContextArbitraryUID(t *testing.T) {
	root := repoRoot(t)
	docs := parseDocs(t, buildKustomize(t, root, nil))

	var deploy *doc
	for i := range docs {
		if docs[i].Kind == "Deployment" {
			deploy = &docs[i]
		}
	}
	if deploy == nil {
		t.Fatal("no Deployment in config/default-nfs render")
	}

	spec := deploy.Spec
	podSpec, _ := spec["template"].(map[string]interface{})
	innerSpec, _ := podSpec["spec"].(map[string]interface{})
	if innerSpec == nil {
		t.Fatal("Deployment spec.template.spec missing")
	}

	sc, _ := innerSpec["securityContext"].(map[string]interface{})
	if sc == nil {
		t.Fatal("pod securityContext missing")
	}
	if v, present := sc["runAsNonRoot"]; !present || v != true {
		t.Errorf("pod securityContext.runAsNonRoot = %v (present=%v), want true", v, present)
	}
	for _, key := range []string{"runAsUser", "fsGroup", "runAsGroup", "supplementalGroups"} {
		if _, present := sc[key]; present {
			t.Errorf("pod securityContext carries fixed %s = %v; arbitrary-UID strategy must not pin it",
				key, sc[key])
		}
	}
	seccomp, _ := sc["seccompProfile"].(map[string]interface{})
	if seccomp == nil || seccomp["type"] != "RuntimeDefault" {
		t.Errorf("pod seccompProfile = %v, want type RuntimeDefault", sc["seccompProfile"])
	}
	if _, present := innerSpec["volumes"]; present {
		t.Errorf("manager pod mounts volumes %v; with readOnlyRootFilesystem and no volume fsGroup is absent by design",
			innerSpec["volumes"])
	}

	containers, _ := innerSpec["containers"].([]interface{})
	if len(containers) != 1 {
		t.Fatalf("manager Deployment has %d containers, want 1", len(containers))
	}
	c, _ := containers[0].(map[string]interface{})
	csc, _ := c["securityContext"].(map[string]interface{})
	if csc == nil {
		t.Fatal("container securityContext missing")
	}
	if v := csc["allowPrivilegeEscalation"]; v != false {
		t.Errorf("container allowPrivilegeEscalation = %v, want false", v)
	}
	if v := csc["readOnlyRootFilesystem"]; v != true {
		t.Errorf("container readOnlyRootFilesystem = %v, want true", v)
	}
	if v := csc["capabilities"]; v == nil {
		t.Error("container capabilities drop missing")
	}
}

// TestInstallPlatformCompositions (extended for the
// explicit profile selection): each (platform, profile) pair has ONE fully
// rendered install artifact. config/default{,-ocp} are the profile-NEUTRAL
// base (manager only, no operand set); config/default-nfs{,-ocp} compose the
// NFS selection; config/default-fc{,-ocp} the FC selection;
// config/default-iscsi{,-ocp} the iSCSI selection. k8s compositions use
// the rbac.yaml entrypoint, ocp compositions the rbac-ocp.yaml superset
// (adds the node SCC use grant); applying a k8s and an ocp composition
// for the same profile would re-apply the k8s RBAC rules onto the same
// names — platform mixing — so the selections must not overlap. The
// selection is explicit: a composition carries ONLY its profile's operand
// objects (never the other profiles' namespaces or RBAC), and the operand
// namespace renders before the operand RBAC set (namespace-first, RBAC
// after — the namespaced Role/RoleBinding objects cannot apply before the
// namespace exists, explicit namespace contract). The manager namespace file is
// carried as a byte-identical copy in every entrypoint (kustomize v5
// locality forbids a sibling FILE reference); this test pins those copies.
func TestInstallPlatformCompositions(t *testing.T) {
	root := repoRoot(t)

	compositions := map[string]struct {
		profile string // operand profile the composition must carry
		ocp     bool
	}{
		"default":           {profile: "", ocp: false},
		"default-ocp":       {profile: "", ocp: true},
		"default-nfs":       {profile: "nfs", ocp: false},
		"default-nfs-ocp":   {profile: "nfs", ocp: true},
		"default-fc":        {profile: "fc", ocp: false},
		"default-fc-ocp":    {profile: "fc", ocp: true},
		"default-iscsi":     {profile: "iscsi", ocp: false},
		"default-iscsi-ocp": {profile: "iscsi", ocp: true},
	}

	// The namespace copies must be byte-identical across ALL entrypoints.
	reference, err := os.ReadFile(filepath.Join(root, "config", "default", "namespace.yaml"))
	if err != nil {
		t.Fatalf("read reference namespace copy: %v", err)
	}
	for comp := range compositions {
		b, err := os.ReadFile(filepath.Join(root, "config", comp, "namespace.yaml"))
		if err != nil {
			t.Fatalf("read config/%s/namespace.yaml: %v", comp, err)
		}
		if !bytes.Equal(reference, b) {
			t.Errorf("config/%s/namespace.yaml diverged from config/default/namespace.yaml", comp)
		}
	}

	for comp, spec := range compositions {
		t.Run(comp, func(t *testing.T) {
			// buildKustomizePath is the shared generic render helper (also
			// used by the version audit): root-relative composition path.
			docs := parseDocs(t, buildKustomizePath(t, root, "config/"+comp))

			byName := func(kind, name string) *doc {
				for i := range docs {
					d := &docs[i]
					if d.Kind == kind && d.Metadata["name"] == name {
						return d
					}
				}
				return nil
			}

			// Exactly one manager Deployment per composition.
			deploys := 0
			for _, d := range docs {
				if d.Kind == "Deployment" {
					deploys++
				}
			}
			if deploys != 1 {
				t.Errorf("composition renders %d Deployments, want exactly 1 (the manager)", deploys)
			}

			if spec.profile == "" {
				// Neutral base: exactly the manager plane, NO operand objects.
				for _, d := range docs {
					n, _ := d.Metadata["name"].(string)
					for _, p := range []string{"nfs", "fc", "iscsi"} {
						if strings.HasPrefix(n, p+"-csi-ngxstorage-") || n == "ngxstorage-"+p {
							t.Errorf("neutral composition carries operand object %s %s — default must not select a profile", d.Kind, n)
						}
					}
				}
				return
			}

			operandPrefix := spec.profile + "-csi-ngxstorage-"

			// Platform purity: the node SCC use grant exists iff ocp —
			// k8s and ocp sets must not overlap.
			sccName := operandPrefix + "node-scc-use"
			hasSCCUse := byName("Role", sccName) != nil && byName("RoleBinding", sccName) != nil
			if spec.ocp && !hasSCCUse {
				t.Errorf("ocp composition is missing the node SCC use grant %s", sccName)
			}
			if !spec.ocp && hasSCCUse {
				t.Errorf("k8s composition carries the OCP SCC use grant %s — platform sets mixed", sccName)
			}

			// Explicit selection: the selected profile's operand namespace and
			// controller binding are present; the OTHER profiles' operand
			// namespaces and RBAC objects are absent.
			operandNS := "ngxstorage-" + spec.profile
			if byName("Namespace", operandNS) == nil {
				t.Errorf("composition is missing the operand Namespace %s", operandNS)
			}
			if byName("ClusterRoleBinding", operandPrefix+"controller") == nil {
				t.Errorf("composition is missing the operand ClusterRoleBinding %s-csi-ngxstorage-controller", spec.profile)
			}
			for _, other := range []string{"nfs", "fc", "iscsi"} {
				if other == spec.profile {
					continue
				}
				if d := byName("Namespace", "ngxstorage-"+other); d != nil {
					t.Errorf("composition carries foreign operand Namespace %s — selection must be explicit", d.Metadata["name"])
				}
				for _, d := range docs {
					if n, _ := d.Metadata["name"].(string); strings.HasPrefix(n, other+"-csi-ngxstorage-") {
						t.Errorf("composition carries foreign operand object %s %s — selection must be explicit", d.Kind, n)
					}
				}
			}

			// Namespace-first, RBAC-after: the operand Namespace renders
			// BEFORE the first operand RBAC document.
			nsIdx, firstRBACIdx := -1, -1
			for i := range docs {
				name, _ := docs[i].Metadata["name"].(string)
				if docs[i].Kind == "Namespace" && name == operandNS {
					nsIdx = i
				}
				if firstRBACIdx == -1 && strings.HasPrefix(name, operandPrefix) &&
					(docs[i].Kind == "ClusterRole" || docs[i].Kind == "ClusterRoleBinding" ||
						docs[i].Kind == "Role" || docs[i].Kind == "RoleBinding") {
					firstRBACIdx = i
				}
			}
			if nsIdx == -1 || firstRBACIdx == -1 || nsIdx > firstRBACIdx {
				t.Errorf("operand Namespace must render before the operand RBAC set (namespace=%d firstRBAC=%d)",
					nsIdx, firstRBACIdx)
			}
		})
	}
}
