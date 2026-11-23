#!/usr/bin/env bash
# verify-golden: re-render the upstream raw manifests (NFS, FC, iSCSI) with
# the SAME pinned kustomize version, apply ONLY the approved transforms,
# and field-compare against the Quartet golden fixtures (regeneration
# independence — the golden must not silently drift from the
# raw source it mirrors, and must not share a code path with the renderer
# under test).
#
# Sources:
# NFS working tree of $NFS_DIR (unchanged behavior). For a strict
# pinned-source comparison: git -C "$NFS_DIR" archive <commit>
# deploy/manifest | tar -x -C <tmpdir>, then point NFS_DIR at the
# extracted tree (the golden source commit is in the golden header).
# FC checkout-external from the CLEAN driver commit recorded in the
# golden headers (git archive — any dirty worktree state is
# excluded): FC_DIR @ FC_PIN.
# iSCSI same model: ISCSI_DIR @ ISCSI_PIN.
# The raw source is NEVER derived from the golden; the golden is the
# transformed raw, and this script re-derives it independently.
#
# Exit 0 = all six platform/profile comparisons match; exit 1 = diffs
# reported; exit 2 = setup error (missing source, missing pin).
set -euo pipefail

export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
export GOCACHE="${GOCACHE:-$TMPDIR/gocache}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVER_ROOT="$REPO_ROOT/../../CSI/Driver"
NFS_DIR="${NFS_DIR:-$DRIVER_ROOT/NFS}"
FC_DIR="${FC_DIR:-$DRIVER_ROOT/FC}"
FC_PIN="${FC_PIN:-7a8454afa04e10905181d2d24e37813680be92b8}"
ISCSI_DIR="${ISCSI_DIR:-$DRIVER_ROOT/iSCSI}"
ISCSI_PIN="${ISCSI_PIN:-3e138b2dc56490b1c60d16298a85be227ec89cae}"
KUSTOMIZE_VERSION="sigs.k8s.io/kustomize/kustomize/v5@v5.7.1"
KUSTOMIZE_SHORT="$(echo "$KUSTOMIZE_VERSION" | grep -o 'v5\..*')"

WORK="$TMPDIR/quartet-golden-verify"
mkdir -p "$WORK"

# render <root> <platform> <out>: kustomize build of <root>/deploy/manifest/
# <platform-dir>; the raw layout dirs are named kubernetes/openshift while
# the golden/verifier platform keys are k8s/ocp.
render() {
  local root="$1" platform="$2" out="$3"
  local pdir
  case "$platform" in
    k8s) pdir="kubernetes" ;;
    ocp) pdir="openshift" ;;
  esac
  if [[ ! -d "$root/deploy/manifest" ]]; then
    echo "error: manifest source not found at $root/deploy/manifest" >&2
    exit 2
  fi
  echo "rendering $platform raw from $root (kustomize $KUSTOMIZE_SHORT)"
  go run "$KUSTOMIZE_VERSION" build "$root/deploy/manifest/$pdir" > "$out"
}

# render_pinned <dir> <pin> <profile>: checkout-external git archive of the
# clean driver commit (once), then render both platforms from it.
render_pinned() {
  local dir="$1" pin="$2" profile="$3"
  if [[ ! -d "$dir" ]]; then
    echo "error: driver checkout not found at $dir (set ${profile^^}_DIR)" >&2
    exit 2
  fi
  if ! git -C "$dir" cat-file -e "${pin}^{commit}" 2>/dev/null; then
    echo "error: pinned commit $pin not present in $dir" >&2
    exit 2
  fi
  local tree="$WORK/$profile-pin"
  if [[ ! -d "$tree/deploy/manifest" ]]; then
    mkdir -p "$tree"
    git -C "$dir" archive "$pin" deploy/manifest | tar -x -C "$tree"
  fi
  render "$tree" k8s "$WORK/$profile-k8s.yaml"
  render "$tree" ocp "$WORK/$profile-ocp.yaml"
}

echo "== NFS (working tree, behavior unchanged)"
render "$NFS_DIR" k8s "$WORK/nfs-k8s.yaml"
render "$NFS_DIR" ocp "$WORK/nfs-ocp.yaml"

echo "== FC (checkout-external, pinned $FC_PIN)"
render_pinned "$FC_DIR" "$FC_PIN" fc

echo "== iSCSI (checkout-external, pinned $ISCSI_PIN)"
render_pinned "$ISCSI_DIR" "$ISCSI_PIN" iscsi

cd "$REPO_ROOT"
for profile in nfs fc iscsi; do
  for platform in k8s ocp; do
    go run ./hack/golden-verify "$profile" "$platform" \
      "$WORK/$profile-$platform.yaml" "internal/renderer/testdata/$profile-$platform-golden.yaml"
  done
done
echo "verify-golden: OK (nfs+fc+iscsi, both platforms)"
