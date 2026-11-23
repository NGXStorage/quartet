#!/usr/bin/env bash
# Build-argument computation. Computes VCS_REF,
# DIRTY and the dev tag for the Dockerfile build args; it NEVER writes into
# the source checkout — the dirty snapshot is taken BEFORE any evidence
# artifact exists, and evidence goes to a caller-provided run directory.
# It never builds or pushes.
#
# Usage: hack/build-args.sh <repo-root> [evidence-dir]
# Exit 0 on clean AND dirty checkouts (dirty only changes the tag);
# nonzero only on real failures (not a repo, git failure).
set -euo pipefail

repo_root="${1:?usage: build-args.sh <repo-root> [evidence-dir]}"
evidence_dir="${2:-}"

git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || { echo "build-args: not a git work tree: $repo_root" >&2; exit 1; }

# Snapshot the source state FIRST — creating evidence afterwards inside the
# checkout would flag a clean tree as dirty (the build provenance self-contamination).
vcs_ref="$(git -C "$repo_root" rev-parse HEAD)"
short_sha="${vcs_ref:0:12}"
dirty_entries="$(git -C "$repo_root" status --porcelain)"

dirty=false
if [ -n "$dirty_entries" ]; then
    dirty=true
fi

tag="$short_sha"
if [ "$dirty" = "true" ]; then
    tag="${tag}-dirty"
fi

if [ -n "$evidence_dir" ]; then
 # Build-evidence policy: evidence inside the checkout contaminates the NEXT source
 # snapshot (a clean tree becomes dirty with the evidence file). Canonical
 # resolution (symlinks + not-yet-existing components) happens BEFORE any
 # mutation; the policy is evidence outside the checkout, full stop.
    root_real="$(cd "$repo_root" && pwd -P)"
    ev_real="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$evidence_dir")"
    case "$ev_real" in
        "$root_real"|"$root_real"/*)
            echo "build-args: evidence dir resolves INSIDE the source checkout: $ev_real — refusing" >&2
            exit 2 ;;
    esac
    mkdir -p "$ev_real"
    evidence_dir="$ev_real"
    {
        printf 'commit=%s\n' "$vcs_ref"
        printf 'shortSha=%s\n' "$short_sha"
        printf 'dirty=%s\n' "$dirty"
        printf 'tag=%s\n' "$tag"
 # NOTE: hash of the porcelain LISTING (file names), NOT a content
 # hash — The provenance check's trackedDiffSha256 (git diff HEAD content) and the
 # untracked content manifest are different records and must not be
 # confused with this one.
        printf 'dirtyFileListSha256=%s\n' "$(printf '%s' "$dirty_entries" | sha256sum | cut -d' ' -f1)"
    } > "$evidence_dir/build-args.txt"
fi

printf 'VCS_REF=%s\nDIRTY=%s\nTAG=%s\n' "$vcs_ref" "$dirty" "$tag"
