#!/usr/bin/env bash
# Offline TEST STUB for the scenario_preflight / scenario_kube helper surface
# (tests/scenario/CASE-CONTRACT.md, "Ortak helper yüzeyi").
#
# The contract says case workers build the TEST STUB of these two functions in
# their OWN fixtures and must not wait for the runner worker's lib/. case.sh
# sources this file only when the runner has NOT already defined the
# functions (declare -F), so a live tests/scenario/lib/ drops in unmodified.
# Every runtime name the case body reads after preflight (RUN_ID,
# EVIDENCE_DIR, SCENARIO_FAKE_STORE, SOURCE_REV, CASE_SHA) is defined here AND
# in the real lib/common.sh preflight (protocol pin 3: no stub-only variable
# may leave the case unbound under the real helper).
#
# Offline semantics enforced here (contract "Canlı gate sınırı"):
# * no silent default target — missing KUBE_CONTEXT/TEST_NAMESPACE rejects
# the case BEFORE any mutation (BLOCKED, rc 2);
# * only the fake target "offline-fake" (local store under the scratch root)
# is ever accepted; any other context is a LIVE target and is rejected
# before the first mutation — cluster-scoped-fault cases are never
# automatic, and this worker's live cell stays NOT RUN (a separate
# serial live worker owns live I/O);
# * scenario_kube rejects any verb that is not a real kubectl verb BEFORE
# dispatch (protocol pin 4): the offline model refuses to invent an
# operator RPC — there is no kubectl "reconcile" verb, and the real
# operator is async, observed via status/observedGeneration polls;
# * scratch only under $HOME/.cache/ngxstorage (machine rule: no /tmp).

set -u

_offline_fixture_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The real kubectl CLI verb surface. Anything outside this set is NOT a
# kubectl verb — a real `kubectl <verb>` would fail identically, so the stub
# refuses dispatch instead of silently running a synthetic command.
KUBECTL_VERBS=" annotate apply auth can-i certificate cluster-info completion config cp create delete diff edit explain get kustomize label logs option patch plugin port-forward proxy replace scale set taint top unset version wait "

_offline_scratch_root() {
  local base="${SCENARIO_SCRATCH_ROOT:-$HOME/.cache/ngxstorage}"
  case "$base" in
    /tmp* | /var/tmp*)
      echo "scratch root $base violates the no-/tmp machine rule" >&2
      return 1 ;;
  esac
  mkdir -p -- "$base" || return 1
  printf '%s' "$base"
}

scenario_preflight() {
  local case_json="${1:?scenario_preflight: case.json path required}"
  local rc=2
  [[ -f "$case_json" ]] || { echo "PREFLIGHT-FAIL: case.json not found: $case_json" >&2; return 1; }

 # Runtime fields (contract "Ortak runtime alanları"). KUBE_BIN has a
 # default; context/namespace NEVER do.
  KUBE_BIN="${KUBE_BIN:-kubectl}"
  RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
  export KUBE_BIN RUN_ID

 # case.json metadata validation (discovery does not execute the case).
  local meta
  if ! meta="$(python3 - "$case_json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
missing = [k for k in ("name", "platform", "profile", "risk", "scope",
                       "precondition", "plan_ref") if not d.get(k)]
if missing:
    raise SystemExit("case.json missing fields: %s" % ",".join(missing))
if d["risk"] not in ("isolated", "cluster-scoped-fault", "uninstall"):
    raise SystemExit("case.json unknown risk: %s" % d["risk"])
if d["scope"] not in ("namespaced", "cluster-scoped"):
    raise SystemExit("case.json unknown scope: %s" % d["scope"])
print("%s|%s|%s" % (d["name"], d["risk"], d["scope"]))
PY
)"; then
    echo "PREFLIGHT-FAIL: $meta" >&2
    return 1
  fi
  local name risk scope
  IFS='|' read -r name risk scope <<<"$meta"

  if [[ -z "${KUBE_CONTEXT:-}" || -z "${TEST_NAMESPACE:-}" ]]; then
    echo "PREFLIGHT-REJECT: KUBE_CONTEXT/TEST_NAMESPACE missing — no silent default target; mutation rejected before anything (BLOCKED)" >&2
    return 2
  fi

  if [[ "$KUBE_CONTEXT" != "offline-fake" ]]; then
 # Live target. cluster-scoped-fault/uninstall never auto-engage; a grant
 # is required BEFORE the mutation — and even a grant cannot route this
 # case to a live cell from this worker: the live singleton is owned by
 # the serial live worker (NOT RUN, never PASS here).
    if [[ "${ALLOW_LIVE:-}" != "allow-$risk" ]]; then
      echo "PREFLIGHT-REJECT: live target $KUBE_CONTEXT, risk=$risk has no explicit grant (ALLOW_LIVE=allow-$risk); mutation rejected before anything (BLOCKED)" >&2
      return 2
    fi
    echo "PREFLIGHT-REJECT: live cell NOT RUN — live I/O belongs to the serial live worker; an isolated namespace does not make this mutation safe on the live singleton (BLOCKED)" >&2
    return 2
  fi

 # Offline fake target: fresh store under the scratch root, never /tmp.
  local root store evidence
  if ! root="$(_offline_scratch_root)"; then
    echo "PREFLIGHT-FAIL: $root" >&2
    return 2
  fi
  export TMPDIR="$root"
  store="${SCENARIO_FAKE_STORE:-}"
  if [[ -n "$store" ]]; then
    case "$(cd "$(dirname "$store")" 2>/dev/null && pwd)" in
      "$root" | "$root"/*) ;;  # pre-seeded store must still live under scratch
      *) echo "PREFLIGHT-FAIL: SCENARIO_FAKE_STORE outside scratch root refused" >&2; return 2 ;;
    esac
  else
    store="$root/scenario-$name-$RUN_ID/store"
  fi
  mkdir -p -- "$store"
  EVIDENCE_DIR="${EVIDENCE_DIR:-$root/scenario-$name-$RUN_ID/evidence}"
  mkdir -p -- "$EVIDENCE_DIR"
  export SCENARIO_FAKE_STORE="$store" EVIDENCE_DIR

 # Source/runner revision binding for the evidence (contract "Sonuç sözleşmesi").
  export SOURCE_REV="$(git -C "$_offline_fixture_dir" rev-parse HEAD 2>/dev/null || echo nogit)"
  export CASE_SHA="$(find "$_offline_fixture_dir/.." -maxdepth 1 -type f \( -name 'case.sh' -o -name 'case.json' \) -exec sha256sum {} + 2>/dev/null | sha256sum | cut -d' ' -f1)"

  echo "PREFLIGHT-OK name=$name risk=$risk scope=$scope context=$KUBE_CONTEXT namespace=$TEST_NAMESPACE run=$RUN_ID"
  return 0
}

scenario_kube() {
 # The single cluster-call surface. Offline it drives the fake store.
 # Refusal-vs-API-error discipline (CASE-CONTRACT "Ortak atomik cleanup
 # helper" final paragraph): every refusal path here returns 2 — the pinned
 # cleanup helpers propagate that split, so a refusal is never read as a
 # success; rc 1 is reserved for the API surface itself.
  if [[ -z "${SCENARIO_FAKE_STORE:-}" ]]; then
    echo "SCENARIO_KUBE-REJECT: no offline store (scenario_preflight not run) — refusal before dispatch (rc 2)" >&2
    return 2
  fi
 # Runtime verb guard: reject BEFORE dispatch anything that is not a
 # real kubectl verb — never after a mutation, never as a silent fallback.
  local verb="${1:-}"
  if [[ ! "$KUBECTL_VERBS" == *" $verb "* ]]; then
    echo "SCENARIO_KUBE-REJECT: unsupported kubectl verb '$verb' — refused before dispatch; the live operator is async and is observed via status/observedGeneration, never commanded through a synthetic RPC" >&2
    return 2
  fi
  local root
  root="$(_offline_scratch_root)" || { echo "SCENARIO_KUBE-REJECT: bad scratch root (rc 2)" >&2; return 2; }
  case "$SCENARIO_FAKE_STORE" in
    "$root" | "$root"/*) ;;
    *) echo "SCENARIO_KUBE-REJECT: store outside scratch root refused (rc 2)" >&2; return 2 ;;
  esac
  python3 "$_offline_fixture_dir/fake_kubectl.py" --store "$SCENARIO_FAKE_STORE" "$@"
}
