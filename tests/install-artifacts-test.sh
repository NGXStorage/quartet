#!/usr/bin/env bash
# Offline acceptance for hack/install-artifacts.sh (run-id and isolation review): path
# policy (run dir must resolve outside the source checkout on EVERY input
# path, before any mutation), run separation, overwrite policy, and the
# digest contract. Uses a synthetic digest; no cluster, no push.
# Negative (pre-render) cases run against a mock checkout containing only
# the script; positive render cases run against the real repo.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/install-artifacts-test.XXXXXX")"
mkdir -p "$scratch/tmp"
trap 'rm -rf "$scratch"' EXIT
export KUBEBUILDER_ASSETS="${KUBEBUILDER_ASSETS:-$cache/kubebuilder-assets/k8s/1.37.0-linux-amd64}"
digest="quay.io/ngxstorage/quartet-operator@sha256:$(printf 'c%.0s' {1..64})"
# The script requires an EXPLICIT profile (no default; NFS is not the
# default). The generic contract cases below select nfs explicitly; the
# selection-contract cases override or unset it.
export QUARTET_INSTALL_PROFILE=nfs

# --- run-id policy: default TMPDIR resolving inside the checkout -> rc=2, zero mutation
checkout="$scratch/mock-checkout"
mkdir -p "$checkout/hack" "$checkout/cache"
cp "$root/hack/install-artifacts.sh" "$checkout/hack/"
before_tree="$(cd "$checkout" && find . -type f | sort | sha256sum | cut -d' ' -f1)"
rc=0
TMPDIR="$checkout/cache" bash "$checkout/hack/install-artifacts.sh" k8s "$digest" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: in-checkout TMPDIR must be rejected rc=2 (got $rc)" >&2; exit 1; }
after_tree="$(cd "$checkout" && find . -type f | sort | sha256sum | cut -d' ' -f1)"
[ "$before_tree" = "$after_tree" ] || { echo "FAIL: in-checkout TMPDIR left source mutations" >&2; exit 1; }

# --- run-id policy: nonexistent explicit in-checkout run dir -> rc=2, NOTHING created
rc=0
TMPDIR="$scratch/tmp" bash "$checkout/hack/install-artifacts.sh" k8s "$digest" "$checkout/run-inside" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: in-checkout run dir must be rejected rc=2 (got $rc)" >&2; exit 1; }
[ ! -e "$checkout/run-inside" ] || { echo "FAIL: rejected run dir was created" >&2; exit 1; }

# --- valid outside run dir: fresh per-run separation for two invocations
line_of() { sed -n "s|^install-artifacts: .*artifact ready at \\(.*\\)$|\\1|p" <<< "$1"; }
rd1="$(line_of "$(TMPDIR="$scratch/tmp" bash "$root/hack/install-artifacts.sh" k8s "$digest")")"
rd2="$(line_of "$(TMPDIR="$scratch/tmp" bash "$root/hack/install-artifacts.sh" k8s "$digest")")"
[ -n "$rd1" ] && [ -n "$rd2" ] || { echo "FAIL: could not read the final artifact path from output" >&2; exit 1; }
run1="$(dirname "$rd1")"; run2="$(dirname "$rd2")"
[ "$run1" != "$run2" ] || { echo "FAIL: two runs shared a run dir" >&2; exit 1; }
[ -s "$rd1" ] && [ -s "$rd2" ]
[ "$(grep -c 'v0.0.0-dev' "$run1/phase3-manager.yaml" || true)" = "0" ] \
    || { echo "FAIL: canonical phase3 apply file still carries the dev image" >&2; exit 1; }

# --- QUARTET_INSTALL_IMAGE_PULL_POLICY: imported-image (Never) path -------
rd_never="$(line_of "$(TMPDIR="$scratch/tmp" QUARTET_INSTALL_IMAGE_PULL_POLICY=Never bash "$root/hack/install-artifacts.sh" k8s "$digest")")"
run_never="$(dirname "$rd_never")"
grep -q "imagePullPolicy: Never" "$run_never/phase3-manager.yaml" \
    || { echo "FAIL: phase3 missing imagePullPolicy Never" >&2; exit 1; }
grep -q "imagePullPolicy: Never" "$rd_never" \
    || { echo "FAIL: final artifact missing imagePullPolicy Never" >&2; exit 1; }
grep -q "$digest" "$run_never/phase3-manager.yaml" \
    || { echo "FAIL: pullPolicy patch lost the pinned digest" >&2; exit 1; }
# default (unset) must NOT add the field — rendered default stays IfNotPresent
grep -q "imagePullPolicy" "$run1/phase3-manager.yaml" \
    && { echo "FAIL: unset policy must leave imagePullPolicy absent" >&2; exit 1; } || true
# invalid value rejected rc=2 BEFORE any render/mutation (review
# note: the RC=2 contract must hold even when the kustomize module lookup
# would fail offline) — assert zero new run dirs were created.
dirs_before="$(ls "$scratch/tmp" | grep -c '^quartet-install-' || true)"
rc=0
TMPDIR="$scratch/tmp" QUARTET_INSTALL_IMAGE_PULL_POLICY=Booger bash "$root/hack/install-artifacts.sh" k8s "$digest" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: invalid pull policy must be rejected rc=2 (got $rc)" >&2; exit 1; }
dirs_after="$(ls "$scratch/tmp" | grep -c '^quartet-install-' || true)"
[ "$dirs_before" = "$dirs_after" ] || { echo "FAIL: invalid pull policy created a run dir before rejecting" >&2; exit 1; }

# --- explicit non-empty run dir rejected without QUARTET_INSTALL_OVERWRITE=1
rc=0
TMPDIR="$scratch/tmp" bash "$root/hack/install-artifacts.sh" k8s "$digest" "$run1" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: non-empty run dir must be rejected (got $rc)" >&2; exit 1; }
# explicit overwrite accepted
TMPDIR="$scratch/tmp" QUARTET_INSTALL_OVERWRITE=1 bash "$root/hack/install-artifacts.sh" k8s "$digest" "$run1" >/dev/null \
    || { echo "FAIL: explicit overwrite was rejected" >&2; exit 1; }

# --- digest contract negatives (checked before any render)
for bad in "quay.io/ngxstorage/quartet-operator:v1" "" "quay.io/x@sha256:abc"; do
    rc=0
    TMPDIR="$scratch/tmp" bash "$checkout/hack/install-artifacts.sh" k8s "$bad" >/dev/null 2>&1 || rc=$?
    [ "$rc" -eq 2 ] || { echo "FAIL: digest $bad must be rejected rc=2 (got $rc)" >&2; exit 1; }
done

# --- invalid QUARTET_INSTALL_PROFILE rejected rc=2 BEFORE any render/dir
# (same RC=2-before-mutation surface as the pull-policy contract)
dirs_before="$(ls "$scratch/tmp" | grep -c '^quartet-install-' || true)"
rc=0
TMPDIR="$scratch/tmp" QUARTET_INSTALL_PROFILE=bogus bash "$root/hack/install-artifacts.sh" k8s "$digest" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: invalid profile must be rejected rc=2 (got $rc)" >&2; exit 1; }
dirs_after="$(ls "$scratch/tmp" | grep -c '^quartet-install-' || true)"
[ "$dirs_before" = "$dirs_after" ] || { echo "FAIL: invalid profile created a run dir before rejecting" >&2; exit 1; }

# --- unset QUARTET_INSTALL_PROFILE rejected rc=2 (no default profile; NFS
# is not the default selection)
rc=0
TMPDIR="$scratch/tmp" env -u QUARTET_INSTALL_PROFILE bash "$root/hack/install-artifacts.sh" k8s "$digest" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: unset profile must be rejected rc=2 (got $rc)" >&2; exit 1; }

# --- profile selection: ONE full-artifact path per profile x platform
# (the documented selection surface). Each run must compose the selected
# profile's operand set — namespace-first, RBAC after — carry ONLY that
# profile (explicit selection, never a silent union), keep the manager
# image pinned, preserve the semantic artifact (raw==final multiset), and
# verify the canonical phase files against their recorded hashes.
other_profiles() {
    case "$1" in
        nfs) echo "fc iscsi" ;;
        fc)  echo "nfs iscsi" ;;
        iscsi) echo "nfs fc" ;;
    esac
}
for platform in k8s ocp; do
    for profile in nfs fc iscsi; do
        rd="$(line_of "$(TMPDIR="$scratch/tmp" QUARTET_INSTALL_PROFILE="$profile" \
            bash "$root/hack/install-artifacts.sh" "$platform" "$digest")")"
        [ -n "$rd" ] || { echo "FAIL: $platform/$profile artifact path not reported (selection must render)" >&2; exit 1; }
        run="$(dirname "$rd")"
 # composition: the selected operand namespace is present
        grep -q "^  name: ngxstorage-$profile$" "$rd" \
            || { echo "FAIL: $platform/$profile artifact missing operand namespace ngxstorage-$profile" >&2; exit 1; }
 # explicit selection: the other two profiles' operand sets are absent
        for o in $(other_profiles "$profile"); do
            grep -q "ngxstorage-$o" "$rd" \
                && { echo "FAIL: $platform/$profile artifact carries $o operand objects (selection must be explicit)" >&2; exit 1; }
        done
 # namespace-first, RBAC-after: the operand namespace renders before
 # the first operand RBAC object
        ns_line="$(grep -n "^  name: ngxstorage-$profile$" "$rd" | head -1 | cut -d: -f1)"
        rbac_line="$(grep -n "^  name: ${profile}-csi-ngxstorage-controller$" "$rd" | head -1 | cut -d: -f1)"
        [ -n "$ns_line" ] && [ -n "$rbac_line" ] && [ "$ns_line" -lt "$rbac_line" ] \
            || { echo "FAIL: $platform/$profile artifact does not render the operand namespace before the RBAC set" >&2; exit 1; }
 # SA/binding shape: the operand controller ClusterRoleBinding exists
 # and at least one binding subjects the operand controller SA in
 # the operand namespace (the RBACDrift gate verifies this shape)
        subj_err="$(python3 - "$rd" "$profile" <<'PY' 2>&1
import sys, yaml
path, profile = sys.argv[1], sys.argv[2]
sa, ns = f"{profile}-csi-ngxstorage-controller", f"ngxstorage-{profile}"
crb = binding = False
for d in yaml.safe_load_all(open(path)):
    if not d:
        continue
    kind = d.get("kind")
    name = (d.get("metadata") or {}).get("name")
    if kind == "ClusterRoleBinding" and name == sa:
        crb = True
    if kind in ("RoleBinding", "ClusterRoleBinding"):
        for s in d.get("subjects") or []:
            if s.get("kind") == "ServiceAccount" and s.get("name") == sa and s.get("namespace") == ns:
                binding = True
if not crb:
    sys.exit(f"FAIL: no ClusterRoleBinding {sa}")
if not binding:
    sys.exit(f"FAIL: no binding subjects SA {sa} in {ns}")
PY
)" || { echo "$subj_err" >&2; exit 1; }
 # image pin preserved in the canonical phase3 file AND the final artifact
        grep -q "$digest" "$run/phase3-manager.yaml" \
            || { echo "FAIL: $platform/$profile phase3 lost the pinned digest" >&2; exit 1; }
        grep -q "$digest" "$rd" \
            || { echo "FAIL: $platform/$profile final artifact lost the pinned digest" >&2; exit 1; }
        [ "$(grep -c 'v0.0.0-dev' "$rd" || true)" = "0" ] \
            || { echo "FAIL: $platform/$profile final artifact still carries the dev image" >&2; exit 1; }
 # platform purity: the node SCC use grant exists iff ocp
        if [ "$platform" = ocp ]; then
            grep -q "${profile}-csi-ngxstorage-node-scc-use" "$rd" \
                || { echo "FAIL: ocp/$profile artifact missing the node SCC use grant" >&2; exit 1; }
        else
            grep -q "${profile}-csi-ngxstorage-node-scc-use" "$rd" \
                && { echo "FAIL: k8s/$profile artifact carries the OCP SCC use grant (platform sets mixed)" >&2; exit 1; }
        fi
 # semantic artifact preservation: the canonical apply files still
 # match their recorded hashes (nothing mutated after finalize)
        ( cd "$run" && sha256sum -c --quiet artifacts.sha256 ) \
            || { echo "FAIL: $platform/$profile canonical phase files diverge from artifacts.sha256" >&2; exit 1; }
    done
done

echo "install-artifacts offline acceptance: path policy (zero mutation), run separation, overwrite policy, digest contract, profile selection (6 platform x profile artifacts: composition, explicit selection, namespace-first, SA/binding shape, image pin, platform purity, semantic preservation) PASS"
