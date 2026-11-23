#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# scenario_helpers.sh — OFFLINE TEST STUB for the two shared helper functions
# (CASE-CONTRACT.md "Ortak helper yüzeyi"). The runner's real lib replaces this
# file via `export -f` (the case body's declare -F probe finds it first); the
# SCENARIO_HELPERS override points at it for direct offline runs.
#
# Surface (identical names + roles to the real contract):
# scenario_preflight <case.json> — validate case metadata, target/context,
# risk/scope; reject BEFORE any mutation when invalid or when a required
# target/context is missing (no silent default fallback). Exports the
# shared runtime fields (RUN_ID, EVIDENCE_DIR, SCENARIO_CASE_NAME) with
# the same semantics as the real lib's preflight.
# scenario_kube <kubectl-args...> — the single cluster-call surface.
# Offline it emulates the API+operator via fake_cluster.sh; it never
# drives a real cluster from this fixture.
#
# This stub is strictly OFFLINE: a canonical live grant (ALLOW_LIVE=allow-*)
# — like the legacy ALLOW_LIVE=1 — requests a LIVE result, which this fixture
# cannot produce: every scenario_kube call is REFUSED (rc 2, gate semantics,
# mirroring the real lib's refusal code) so a granted run dies BLOCKED before
# any mutation instead of PASSING on a fake. A case run WITHOUT a grant
# therefore reports NOT RUN, never PASS.
#
# Helper source path is resolved from this file's own BASH_SOURCE root, per
# contract ("Helper çözümleme"). The case body references NO variable from
# this stub (protocol pin 3) — _sc_* names below are stub internals.
# -----------------------------------------------------------------------------

_sc_helpers_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# case dir is the parent of fixtures/
_sc_case_dir=$(cd "${_sc_helpers_dir}/.." && pwd)

# shellcheck disable=SC1091
source "${_sc_helpers_dir}/fake_cluster.sh"

# Lazy, once-only offline state bootstrap. Seeds the installer-precondition
# facts the case metadata declares (config/operands/nfs/ applied out-of-band —
# the reconciler only verifies it, apply.go:221-244; AND the operator itself
# installed: CRD + manager, so the body's readiness probe passes offline): the
# case body stays portable and only expresses the behavior under test.
_sc_ensure_fake_state() {
  if [[ -z "${FAKE_STATE_DIR:-}" ]]; then
    export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
    local run_id
    run_id="off-$(date +%s)-$$-$RANDOM"
    export SCENARIO_RUN_ID="$run_id"
    FAKE_STATE_DIR="${TMPDIR}/quartet-fake/${_sc_case_dir##*/}-${run_id}"
    export FAKE_STATE_DIR
    fake_reset
    fake_seed_install_rbac
    fake_seed_operator_install
  fi
}

# --- scenario_preflight <case.json> ------------------------------------------
scenario_preflight() {
  local case_json="${1:-}" errs=0 field v
  [[ -f "$case_json" ]] || { echo "preflight: missing case.json: $case_json" >&2; return 1; }
  jq -e . "$case_json" >/dev/null 2>&1 || { echo "preflight: case.json is not valid JSON" >&2; return 1; }

  for field in name platform profile risk scope precondition plan_ref; do
    v=$(jq -r --arg f "$field" '.[$f] // empty' "$case_json")
    if [[ -z "$v" ]]; then
      echo "preflight: case.json missing required field '$field'" >&2
      errs=1
    fi
  done
  if [[ $errs -ne 0 ]]; then return 1; fi

 # name must match the case directory (behavior-based naming, no drift).
  local want_name got_name
  want_name=${_sc_case_dir##*/}; got_name=$(jq -r '.name' "$case_json")
  if [[ "$want_name" != "$got_name" ]]; then
    echo "preflight: metadata name '$got_name' != directory '$want_name'" >&2
    return 1
  fi

  local risk scope
  risk=$(jq -r '.risk' "$case_json"); scope=$(jq -r '.scope' "$case_json")
  case "$risk" in isolated|cluster-scoped-fault|uninstall) ;; *)
    echo "preflight: invalid risk '$risk' (isolated|cluster-scoped-fault|uninstall)" >&2; return 1;; esac
  case "$scope" in namespaced|cluster-scoped) ;; *)
    echo "preflight: invalid scope '$scope' (namespaced|cluster-scoped)" >&2; return 1;; esac

 # No silent default fallback: an explicit target+context are mandatory.
  if [[ -z "${KUBE_CONTEXT:-}" ]]; then
    echo "preflight: KUBE_CONTEXT not set — no silent default context; refusing before any mutation" >&2
    return 1
  fi
  if [[ -z "${TEST_NAMESPACE:-}" ]]; then
    echo "preflight: TEST_NAMESPACE not set — no silent default namespace; refusing before any mutation" >&2
    return 1
  fi
  export TEST_NAMESPACE

 # Shared runtime fields (CASE-CONTRACT "Ortak runtime alanları"): the real
 # lib's preflight exports the same names; evidence + run identity land in a
 # per-run scratch dir under $HOME/.cache/ngxstorage in both modes.
  export SCENARIO_CASE_NAME="$got_name"
  : "${RUN_ID:=scenario-stub-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
  export RUN_ID
  : "${EVIDENCE_DIR:=$TMPDIR/scenario-stub-${got_name}-${RUN_ID}}"
  export EVIDENCE_DIR
  mkdir -p "$EVIDENCE_DIR"

 # Mode selection (stub diagnostic; the case body never reads it — pin 3).
 # This offline fixture is 'offline' unless a live grant was requested; even
 # then scenario_kube below refuses to touch a real cluster (live = runner).
  if [[ "${ALLOW_LIVE:-0}" == "1" || "${ALLOW_LIVE:-}" == allow-* ]]; then
    SCENARIO_MODE="offline-live-requested"
  else
    SCENARIO_MODE="offline"
  fi
  export SCENARIO_MODE

 # Bootstrap the fake state HERE (parent process), not lazily in
 # scenario_kube: case bodies may invoke scenario_kube inside a command
 # substitution (e.g. the create-response UID capture), and a bootstrap that
 # runs in that fork would leave the parent with a second, divergent state
 # dir. Preflight-success means the caller passed the gate, so a read-only
 # bootstrap is never a mutation.
  _sc_ensure_fake_state

  echo "preflight: ok (mode=${SCENARIO_MODE} context=${KUBE_CONTEXT} ns=${TEST_NAMESPACE} risk=${risk} scope=${scope})"
  return 0
}

# --- scenario_kube <kubectl-args...> -----------------------------------------
scenario_kube() {
  _sc_ensure_fake_state
 # Strictly offline: this stub never drives a real cluster, and a live grant
 # (canonical allow-* syntax, legacy 1) requests exactly what it cannot give.
 # rc 2 = gate refusal, the same code the real lib's scenario_kube refusals
 # use, so a granted offline run dies BLOCKED before any mutation.
  if [[ "${ALLOW_LIVE:-0}" == "1" || "${ALLOW_LIVE:-}" == allow-* ]]; then
    echo "scenario_kube: ALLOW_LIVE=${ALLOW_LIVE:-} but this offline fixture cannot touch a real cluster; refusing" >&2
    return 2
  fi
  fake_scenario_kube "$@"
}
