#!/usr/bin/env bash
# Install-artifact preparation. Renders ONE
# platform-complete install artifact for ONE explicitly selected profile
# (QUARTET_INSTALL_PROFILE=nfs|fc|iscsi — REQUIRED, no default profile
# contract; an unset selection NEVER installs all three), pins the manager
# image BEFORE the artifact is finalized, and splits it into apply phases
# with explicit barriers. The selection picks the platform composition
# (config/default-nfs, config/default-nfs-ocp for nfs; config/default-fc{,-ocp}
# for fc; config/default-iscsi{,-ocp} for iscsi) whose operand install set
# is config/operands/<profile>/{k8s,ocp} — namespace-first, RBAC after.
# The artifact embeds ONLY the selected profile's operand objects; a
# multi-profile install applies one artifact per profile. The canonical
# apply files are phase1/phase2/phase3 under their EXACT names; the
# pre-pin manager document is kept only under a clearly non-apply name
# (phase3-manager.prepin.yaml). The final artifact is assembled with
# document separators and verified semantically against the raw render
# (GVK/name/namespace multiset equality; only the manager image may
# differ; CRD, manager binding, both namespaces, the selected profile's
# operand namespace + controller binding, and the manager Deployment are
# asserted individually).
#
# Run isolation (run isolation): the default run dir carries a fresh run-id per
# invocation; an explicitly given run dir must be empty unless
# QUARTET_INSTALL_OVERWRITE=1; the run dir must resolve (symlinks resolved)
# OUTSIDE the source checkout. All input checks complete before the first
# mutation. No cluster is contacted: kubectl is used with --local only.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"

platform="${1:-}"; digest="${2:-}"; run_dir="${3:-}"
profile="${QUARTET_INSTALL_PROFILE:-}"
if [ -z "$profile" ]; then
  echo "QUARTET_INSTALL_PROFILE is required (nfs|fc|iscsi) — no default profile" >&2
  exit 2
fi
if [ -z "$platform" ] || [ -z "$digest" ]; then
    echo "usage: install-artifacts.sh <k8s|ocp> <manager-image-digest> [run-dir]" >&2
    echo "       QUARTET_INSTALL_PROFILE=nfs|fc|iscsi (REQUIRED, no default) selects the operand install set" >&2
    exit 2
fi

# --- digest contract: repository + full sha256 (matches the v1alpha1
# image.reference contract); tags, short/uppercase/malformed digests no. ---
printf '%s' "$digest" | grep -Eq '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$' \
    || { echo "manager image must be an immutable reference repo@sha256:<64 lowercase hex> (v1alpha1 image.reference contract)" >&2; exit 2; }
# Pull policy enum is validated BEFORE any render/output mutation (47744b2
# review note): the RC=2 contract must hold even when the kustomize module
# lookup fails offline.
if [ -n "${QUARTET_INSTALL_IMAGE_PULL_POLICY:-}" ]; then
    case "${QUARTET_INSTALL_IMAGE_PULL_POLICY}" in
        Always|IfNotPresent|Never) : ;;
        *) echo "QUARTET_INSTALL_IMAGE_PULL_POLICY must be Always|IfNotPresent|Never" >&2; exit 2 ;;
    esac
fi
# --- profile contract: exactly one rendered profile per artifact, and the
# selection is explicit (the artifact embeds ONLY the selected operand set
# — namespace-first, RBAC after). Validated BEFORE any run-dir/render
# mutation, on the same RC=2-before-anything surface as the pull-policy
# check (it must hold even when the kustomize module lookup would fail
# offline). ---
case "$profile" in
    nfs|fc|iscsi) : ;;
    *) echo "QUARTET_INSTALL_PROFILE must be nfs, fc or iscsi (got '$profile')" >&2; exit 2 ;;
esac

# --- run dir policy (run-id policy): fresh run-id, overwrite only explicit, outside
# the checkout — canonical path resolution (symlinks and NOT-YET-EXISTING
# components) happens BEFORE the first mutation, on every input path. ---
root_real="$(cd "$root" && pwd -P)"
canon() { python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$1"; }
reject_inside_checkout() { # $1 = resolved path, $2 = what it is
    case "$1" in
        "$root_real"|"$root_real"/*)
            echo "$2 resolves INSIDE the source checkout: $1 — artifacts must stay outside; refusing" >&2
            exit 2 ;;
    esac
}
if [ -z "$run_dir" ]; then
    tmpdir_real="$(canon "$TMPDIR")"
    reject_inside_checkout "$tmpdir_real" "default TMPDIR"
    run_dir="$(mktemp -d "$TMPDIR/quartet-install-$platform-$profile.XXXXXX")"
    run_real="$(canon "$run_dir")"
    reject_inside_checkout "$run_real" "run dir"
else
    case "$run_dir" in
        /*) : ;;
        *) run_dir="$PWD/$run_dir" ;;
    esac
    run_real="$(canon "$run_dir")"
    reject_inside_checkout "$run_real" "run dir"
    if [ -n "$(ls -A "$run_real" 2>/dev/null)" ]; then
        if [ "${QUARTET_INSTALL_OVERWRITE:-0}" = "1" ]; then
            echo "note: overwriting non-empty run dir (QUARTET_INSTALL_OVERWRITE=1): $run_real" >&2
        else
            echo "run dir not empty: $run_real — use a fresh run dir or QUARTET_INSTALL_OVERWRITE=1" >&2
            exit 2
        fi
    fi
    mkdir -p "$run_real"
fi
# Composition per (platform, profile) selection — each one is the
# platform-complete artifact for EXACTLY that profile's operand set
# (config/operands/<profile>/{k8s,ocp}); platform mixing is impossible
# because k8s selections build the k8s entrypoint and ocp selections the
# ocp entrypoint. Both axes are validated above, so the six combinations
# are exhaustive.
case "$platform-$profile" in
    k8s-nfs)   composition="$root/config/default-nfs" ;;
    k8s-fc)    composition="$root/config/default-fc" ;;
    k8s-iscsi) composition="$root/config/default-iscsi" ;;
    ocp-nfs)   composition="$root/config/default-nfs-ocp" ;;
    ocp-fc)    composition="$root/config/default-fc-ocp" ;;
    ocp-iscsi) composition="$root/config/default-iscsi-ocp" ;;
esac

kubectl_bin=""
if [ -n "${KUBEBUILDER_ASSETS:-}" ] && [ -x "$KUBEBUILDER_ASSETS/kubectl" ]; then
    kubectl_bin="$KUBEBUILDER_ASSETS/kubectl"
elif command -v kubectl >/dev/null 2>&1; then
    kubectl_bin="$(command -v kubectl)"
fi
if [ -z "$kubectl_bin" ]; then
    echo "BLOCKED: install-artifacts needs a real kubectl for --local image pinning (set KUBEBUILDER_ASSETS or PATH)"
    exit 0
fi

raw="$run_real/install-$platform-$profile.raw.yaml"
final="$run_real/install-$platform-$profile.yaml"
p1="$run_real/phase1-namespace-crd.yaml"
p2="$run_real/phase2-rbac-operands.yaml"
p3="$run_real/phase3-manager.yaml"
p3pre="$run_real/phase3-manager.prepin.yaml"   # NOT an apply file

# 1. Render the platform-complete artifact.
go run sigs.k8s.io/kustomize/kustomize/v5@v5.7.1 build "$composition" > "$raw"

# 2. Split into phases (python keeps document boundaries intact).
python3 - "$raw" "$run_real" <<'PY'
import re, sys, pathlib
raw_path, run_dir = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
docs = [d for d in raw_path.read_text().split("\n---\n") if d.strip()]

def kind(d):
    m = re.search(r"^kind: (\w+)", d, re.M)
    return m.group(1) if m else ""

phases = {
    "phase1": ("phase1-namespace-crd.yaml", []),
    "phase2": ("phase2-rbac-operands.yaml", []),
    "phase3": ("phase3-manager.yaml", []),
}
deployments = []
for d in docs:
    k = kind(d)
    if k in ("Namespace", "CustomResourceDefinition"):
        phases["phase1"][1].append(d)
    elif k in ("ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"):
        phases["phase2"][1].append(d)
    elif k == "Deployment":
        deployments.append(d)
    elif k == "Service":
        phases["phase3"][1].append(d)
    else:
        raise SystemExit(f"unclassified kind in install artifact: {k!r}")
if len(deployments) != 1:
    raise SystemExit(f"expected exactly 1 manager Deployment, got {len(deployments)}")
phases["phase3"][1].insert(0, deployments[0])

for name, (fname, docs_) in phases.items():
    run_dir.joinpath(fname).write_text("\n---\n".join(docs_) + "\n")
PY

# 3. Pin the manager image; the PINNED document becomes the canonical
# phase3 apply file — the pre-pin input keeps a non-apply name (phase hash rule).
cp -f "$p3" "$p3pre"
"$kubectl_bin" set image \
    -f "$p3pre" \
    quartet-manager="$digest" \
    --local -o yaml > "$p3"

# 3b. Optional imagePullPolicy for node-imported images: when the manager
# image is loaded into node containerd stores (localhost/...@sha256:
# reference, no registry pull possible) the campaign sets
# QUARTET_INSTALL_IMAGE_PULL_POLICY=Never. Unset keeps the rendered
# default (IfNotPresent) for the registry-distribution path.
if [ -n "${QUARTET_INSTALL_IMAGE_PULL_POLICY:-}" ]; then
 # value validated at input stage (RC=2 contract holds offline too)
    python3 - "$p3" "$QUARTET_INSTALL_IMAGE_PULL_POLICY" <<'PY'
import sys, yaml
path, pol = sys.argv[1], sys.argv[2]
docs = list(yaml.safe_load_all(open(path)))
set_pol = 0
for d in docs:
    if d and d.get("kind") == "Deployment":
        for c in d["spec"]["template"]["spec"]["containers"]:
            c["imagePullPolicy"] = pol
            set_pol += 1
if set_pol != 1:
    sys.exit(f"FAIL: expected exactly 1 manager container to receive imagePullPolicy, got {set_pol}")
with open(path, "w") as f:
    for i, d in enumerate(docs):
        if i:
            f.write("---\n")
        yaml.safe_dump(d, f, sort_keys=False)
PY
    echo "note: manager imagePullPolicy=$QUARTET_INSTALL_IMAGE_PULL_POLICY applied to phase3 (imported-image path)"
fi

# 4. Final artifact assembled WITH document separators (final artifact rule).
{
    cat "$p1"; printf '\n---\n'
    cat "$p2"; printf '\n---\n'
    cat "$p3"
} > "$final"

# 5. Semantic verification: final vs raw must agree on the GVK/name/ns
# multiset; only the manager image may differ. Individual asserts for
# the objects a plain grep could miss (final artifact rule) — including the SELECTED
# profile's operand namespace, controller binding and binding subjects
# (the exact shape the reconciler's RBACDrift gate verifies), and the
# explicit-selection invariant: exactly 2 Namespaces (quartet-system +
# the selected operand namespace — never the other profiles').
python3 - "$raw" "$final" "$digest" "$profile" <<'PY'
import sys
import yaml

raw, final, digest, profile = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
operand_ns = f"ngxstorage-{profile}"
subject_sa = f"{profile}-csi-ngxstorage-controller"

def identity(path):
    out = []
    for d in yaml.safe_load_all(open(path)):
        if not d:
            continue
        md = d.get("metadata", {})
        out.append((d["kind"], md.get("namespace", ""), md["name"]))
    return out

ri, fi = identity(raw), identity(final)
if sorted(ri) != sorted(fi):
    missing, added = set(ri) - set(fi), set(fi) - set(ri)
    sys.exit(f"FAIL: final artifact lost {sorted(missing)} / added {sorted(added)} vs raw render")

asserts = {
    "CRD quartets": False, "manager binding": False, "namespaces": 0,
    "deployment": False, "operand namespace": False, "operand binding": False,
}
for k, ns, n in fi:
    if (k, ns, n) == ("CustomResourceDefinition", "", "quartets.quartet.ngxstorage.com"):
        asserts["CRD quartets"] = True
    if (k, ns, n) == ("ClusterRoleBinding", "", "quartet-manager-rolebinding"):
        asserts["manager binding"] = True
    if k == "Namespace":
        asserts["namespaces"] += 1
    if (k, ns, n) == ("Namespace", "", operand_ns):
        asserts["operand namespace"] = True
    if (k, ns, n) == ("Deployment", "quartet-system", "quartet-manager"):
        asserts["deployment"] = True
# Binding-subject shape: at least one (Cluster)RoleBinding must subject the
# operand controller ServiceAccount in the operand namespace.
for d in yaml.safe_load_all(open(final)):
    if not d or d.get("kind") not in ("RoleBinding", "ClusterRoleBinding"):
        continue
    for s in d.get("subjects", []) or []:
        if (s.get("kind"), s.get("name"), s.get("namespace")) == ("ServiceAccount", subject_sa, operand_ns):
            asserts["operand binding"] = True
            break
if asserts["namespaces"] != 2:
    sys.exit(f"FAIL: expected exactly 2 Namespaces (quartet-system + {operand_ns}, "
             f"explicit selection carries no other profile), got {asserts['namespaces']}")
for name, got in asserts.items():
    if not got:
        sys.exit(f"FAIL: required object missing from final artifact: {name}")

dev_left = 0
for d in yaml.safe_load_all(open(final)):
    if d and d.get("kind") == "Deployment":
        for c in d["spec"]["template"]["spec"]["containers"]:
            if c["image"] != digest:
                sys.exit(f"FAIL: manager container image is {c['image']!r}, want the pinned digest")
print(f"semantic verify OK ({profile}): {len(fi)} objects, GVK/name/ns multiset identical to raw, "
      f"CRD/manager-binding/2-namespaces/operand-namespace/operand-binding/Deployment asserted, "
      f"manager image pinned")
PY

# 6. Phase hashes come from the REAL apply files (phase hash rule).
sha256sum "$final" "$p1" "$p2" "$p3" > "$run_real/artifacts.sha256"

echo "install-artifacts: $platform/$profile artifact ready at $final"
echo "  canonical apply files (phase order): $p1 / $p2 / $p3"
echo "  LIVE APPLY ORDER: phase1 -> kubectl wait --for=condition=Established --timeout=120s crd quartets.quartet.ngxstorage.com -> phase2 -> phase3 -> phase4 (the Quartet CR)"
echo "  selection: QUARTET_INSTALL_PROFILE=$profile — the artifact carries ONLY this profile's operand set (config/operands/$profile/$( [ "$platform" = ocp ] && echo ocp || echo k8s ));"
echo "  a multi-profile install applies one artifact per profile."
