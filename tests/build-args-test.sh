#!/usr/bin/env bash
# Offline acceptance for hack/build-args.sh (build provenance): the computed commit, DIRTY
# flag, tag and build args are asserted against a CLEAN and a SYNTHETIC
# DIRTY scratch checkout; no docker build or push is performed.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/build-args-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
export TMPDIR="$scratch/tmp"
mkdir -p "$TMPDIR"

gitconfig() { git config user.name t && git config user.email t@example.com; }

# --- clean checkout -------------------------------------------------------
clean="$scratch/clean"; mkdir -p "$clean"
git init -q "$clean"; (cd "$clean" && gitconfig && printf 'x\n' > a.txt && git add a.txt && git commit -qm init)
out="$(bash "$root/hack/build-args.sh" "$clean" "$scratch/ev-clean")"
ref="$(git -C "$clean" rev-parse HEAD)"; short="${ref:0:12}"
grep -q "VCS_REF=$ref" <<< "$out"
grep -q "DIRTY=false" <<< "$out"
grep -q "TAG=$short" <<< "$out"

# --- synthetic dirty checkout ---------------------------------------------
dirty="$scratch/dirty"; mkdir -p "$dirty"
git init -q "$dirty"; (cd "$dirty" && gitconfig && printf 'x\n' > a.txt && git add a.txt && git commit -qm init && printf 'changed\n' >> a.txt && printf 'new\n' > untracked.txt)
out="$(bash "$root/hack/build-args.sh" "$dirty" "$scratch/ev-dirty")"
ref="$(git -C "$dirty" rev-parse HEAD)"; short="${ref:0:12}"
grep -q "VCS_REF=$ref" <<< "$out"
grep -q "DIRTY=true" <<< "$out"
grep -q "TAG=$short-dirty" <<< "$out"

# --- run isolation: in-checkout evidence is REJECTED (policy: evidence outside) -------
# Writing evidence into the checkout contaminates the NEXT source snapshot;
# the helper refuses with exit 2 instead of silently doing it.
rc=0
bash "$root/hack/build-args.sh" "$clean" "$clean/in-evidence" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || { echo "FAIL: in-checkout evidence dir must be rejected (rc=$rc)" >&2; exit 1; }

# --- evidence content carries the listing hash, never the raw listing -----
grep -q '^dirtyFileListSha256=' "$scratch/ev-dirty/build-args.txt"
grep -q '^dirtyFileListSha256=' "$scratch/ev-clean/build-args.txt"
grep -q 'untracked.txt' "$scratch/ev-dirty/build-args.txt" \
    && { echo "FAIL: raw listing content leaked into evidence" >&2; exit 1; } || true

echo "build-args offline acceptance: clean/dirty/evidence-isolation PASS"
