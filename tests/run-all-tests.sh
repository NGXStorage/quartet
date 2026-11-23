#!/usr/bin/env bash
# run-all-tests.sh — Quartet single test entry point (Phase A: offline only).
#
# local (default) offline orchestrator: run-unit-tests.sh + the offline
# entrypoint contract. No live cluster, no asset download.
# matrix envtest 8-version matrix via hack/envtest-matrix.sh, forced
# ENVTEST_MATRIX_NO_DOWNLOAD=1 -> missing asset is BLOCKED, never a
# silent download. Exit contract follows the matrix (0/1/2).
# scenario LIVE operator/driver scenarios via tests/scenario/run.sh, which
# carries its own risk/target/context gate (ALLOW_LIVE grant). It
# is NEVER part of the offline `local` default: a bare `local` run
# never touches a cluster.
# --list / --help discovery-only: print the surface, run nothing.
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"; mkdir -p "$cache"
export TMPDIR="$cache"
export GOCACHE="$cache/gocache"

sel="${1:-local}"
case "$sel" in
  --list|--help)
    [ "$#" -eq 1 ] || { echo "keşif seçimi ek argüman kabul etmez" >&2; exit 2; }
    echo "run-all-tests.sh <local|matrix|scenario|--list|--help>"
    echo "  local    offline (default): go vet/race + 7 offline suite (run-unit-tests.sh)"
    echo "  matrix   envtest 8-sürüm (ENVTEST_MATRIX_NO_DOWNLOAD=1; eksik asset BLOCKED)"
    echo "  scenario canlı operator/driver case'leri (tests/scenario/run.sh; risk-grant gate'i)"
    echo " --list|--help  yüzeyi gösterir, test/download/canlı tetiklenmez"
    exit 0 ;;
  local)
    [ "$#" -le 1 ] || { echo "local ek argüman kabul etmez" >&2; exit 2; }
    unit_rc=0; contract_rc=0
    ( cd "$root" && bash tests/run-unit-tests.sh ) || unit_rc=$?
    ( cd "$root" && bash tests/test-entrypoint-contract.sh ) || contract_rc=$?
 # FAIL(1) outranks DEGRADED/BLOCKED(2).
    if [ "$unit_rc" -eq 1 ] || [ "$contract_rc" -eq 1 ]; then exit 1; fi
    [ "$unit_rc" -eq 0 ] || exit "$unit_rc"
    exit "$contract_rc" ;;
  matrix)
    export ENVTEST_MATRIX_NO_DOWNLOAD=1
    exec bash "$root/hack/envtest-matrix.sh" "${@:2}" ;;
  scenario)
 # Live path -> the Phase B scenario runner, which owns the risk/target/
 # context gate. Explicit selection only; never reached by offline `local`.
    exec bash "$root/tests/scenario/run.sh" "${@:2}" ;;
  *)
    echo "bilinmeyen seçim: $sel (--list ile yüzeyi gör)" >&2
    exit 2 ;;
esac
