#!/usr/bin/env bash
# ============================================================================
# active-volume-delete-protection — active-volume-delete fault: CR delete with active usage
# (tests/scenario/CASE-CONTRACT.md case body)
#
# Behavior under test, pinned to source:
# * deleting the singleton Quartet CR while active driver-identity usage
# exists MUST keep the CR terminating with the finalizer
# quartet.ngxstorage.com/quartet retained (apply.go:31), surface
# DataInUse on the AGGREGATE conditions (setBlockedTermination,
# apply.go:1275-1295; gate apply.go:1324-1328; reason mapping
# apply.go:1247-1259 incl. :1255; const
# api/v1alpha1/quartet_types.go:87, DRAFT surface) and requeue
# (retryableRequeueAfter, quartet_controller.go:45).
# * usage is OBSERVED, never assumed: the case creates API-valid
# usage objects — PV/PVC/VolumeAttachment with SPEC ONLY (no pre-set
# Bound/attached status; the API server ignores status on create for
# subresource-enabled types) — then bounded-polls the real API state
# (PV Bound + claimRef, PVC Bound, VA attached) under driver identity
# (spec.csi.driver == nfs.csi.ngxstorage.com or the handle prefix —
# pvCarriesDriverIdentity, apply.go:1207-1216) before testing the gate.
# * the operator is ASYNC: this body speaks only the contracted
# kubectl verb surface (get/create/patch/delete) and observes the
# operator through status + observedGeneration with bounded polls. It
# never commands a reconcile pass — a synthetic reconcile RPC is not a
# kubectl verb and does not exist in this model. Unsupported verbs are
# rejected at PRE-FLIGHT (static self-scan of this file, before any
# mutation) and again by the helper at dispatch time.
# * operand + test data are preserved while usage exists — cleanup is
# cancelled on the block (apply.go:1324-1328 `return`s before
# deleteOwnedProfile); customer data: Secret/PV/PVC/backend/namespace
# are never touched (apply.go:97-98).
# * usage clears → controlled recovery WITHOUT any override: the retryable
# requeue replays the gate; only after a CLEAN scan are the UID-owned
# operands deleted (deleteOwnedProfile, apply.go:939-990; ownership
# annotation quartet.ngxstorage.com/owner-uid,
# internal/renderer/profile.go:34; UID+RV precondition,
# verifyAndDeleteOwned, apply.go:1030-1072) and THEN the finalizer is
# removed (apply.go:1329-1342) — the API server only then GCs the CR.
# * forceDisable is an explicit admin override (apply.go:1086-1092) and a
# manual finalizer break is an admin procedure with documented orphan
# consequences (apply.go:1258-1277) — neither is an automatic recovery
# path, and this case never sets either.
#
# Volume-data integrity boundary (honest scope): the marker ConfigMap is a
# NAMESPACED canary proving customer data untouched ("the operator never touches customer
# objects in TEST_NAMESPACE"). It is NOT evidence of volume I/O integrity —
# data inside the volume is proven by the live I/O cells (serial live
# worker, NOT RUN here).
#
# Ownership: the case deletes ONLY objects it created in THIS run,
# each under the UID captured from its create response — through the SHARED
# pinned helpers scenario_create_tracked / scenario_delete_owned (CASE-
# CONTRACT "Ortak atomik cleanup helper"; real owner tests/scenario/lib/,
# fixture TEST STUB of the same contract when the lib lacks them). Atomic
# precondition: get → live-UID match → delete --field-selector=metadata.uid →
# postcondition get must be NotFound. The canonical singleton Quartet CR is
# a PRECONDITION — absent, the case BLOCKs with zero mutation; it is never
# created or adopted here, and its UID is re-verified immediately before the
# delete mutation. There is no label-based sweep of any other run's objects.
#
# Post-PASS boundary (honest scope): the mutation under test DELETES the
# singleton, and a clean recovery leaves the target without it — the
# installer (or the next lifecycle cell) re-provisions it. This case never
# re-creates the singleton on its own.
#
# Fail-closed: the default result is FAIL; rc0 only on explicit
# successful completion. ERR/EXIT traps run restore+cleanup on every exit
# path — an unbound variable or crash cannot become exit 0/PASS. Cleanup
# removes this run's own usage objects, which is what unblocks the gate if
# the run dies mid-termination (the finalizer itself is NEVER broken by the
# case — admin procedure only).
#
# Risk (case.json): cluster-scoped-fault, cluster-scoped scope. The mutation
# deletes the live singleton Quartet CR and the gate scans cluster-wide; an
# isolated namespace does NOT make it safe on the target.
#
# Exit codes: 0 PASS, 1 FAIL, 2 BLOCKED, 3 SKIP (unused), 4 FAIL_CLEANUP.
# Offline by construction: helper stubs come from fixtures/ (contract: case
# workers build their own TEST STUB surface); the live runner's lib/ wins
# when already defined (declare -F).
set -u -E

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CASE_NAME="active-volume-delete-protection"
FINALIZER="quartet.ngxstorage.com/quartet"  # internal/controller/apply.go:31
OPERAND_NS="ngxstorage-nfs"             # internal/renderer/nfs.go:29
DRIVER_NAME="nfs.csi.ngxstorage.com"    # internal/renderer/nfs.go:35
HANDLE_PREFIX="${DRIVER_NAME}."         # internal/renderer/profile.go:145-150
DEPLOYMENT_NAME="nfs-csi-ngxstorage-controller"  # internal/renderer/nfs.go:527
CSIDRIVER_NAME="${DRIVER_NAME}"         # internal/renderer/nfs.go:429-432
DAEMONSET_NAME="nfs-csi-ngxstorage-node"        # internal/renderer/nfs.go
SERVICE_NAME="nfs-csi-ngxstorage-controller-health"
SA_CONTROLLER_NAME="nfs-csi-ngxstorage-controller"
SA_NODE_NAME="nfs-csi-ngxstorage-node"
COORDINATION_CM_NAME="nfs-csi-ngxstorage-coordination-config"
SCC_NAME="nfs-csi-ngxstorage-node-scc"          # OCP surface; probed at baseline
INSTALL_ROLE_NAME="nfs-csi-ngxstorage-leader-election"      # installer-owned
INSTALL_CLUSTERROLE_NAME="nfs-csi-ngxstorage-controller"    # installer-owned
PV_NAME="scenario-data-pv"
PVC_NAME="scenario-data-pvc"
VA_NAME="scenario-data-va"
MARKER_NAME="scenario-data-marker"
CANARY_DEP_NAME="foreign-canary-deployment"
CANARY_CM_NAME="foreign-configmap"
export CASE_NAME

RC_PASS=0; RC_FAIL=1; RC_BLOCKED=2; RC_SKIP=3; RC_FAIL_CLEANUP=4

# The ONLY kubectl verbs this case body sends (pin 4: contracted surface).
# The async operator is observed via status/observedGeneration polls, never
# commanded through a synthetic RPC.
KUBE_VERBS=" get create patch delete "

# Fail-closed state: result is FAIL until explicit success.
FINAL_RC=$RC_FAIL
CLEANUP_RC=0
MUTATIONS_MADE=0
CLEANED=0
RESULT_WRITTEN=0
REGISTRY=""
q_uid=""

MAX_POLLS="${SCENARIO_MAX_POLLS:-30}"
POLL_INTERVAL="${SCENARIO_POLL_INTERVAL:-2}"

KINDS="node deployment daemonset service configmap serviceaccount lease persistentvolumeclaim quartet csidriver persistentvolume volumeattachment"

if [[ $# -gt 0 ]]; then
  echo "CASE-REJECT: unknown argument(s): $* — rejected before anything (BLOCKED)" >&2
  exit $RC_BLOCKED
fi

if ! declare -F scenario_preflight >/dev/null 2>&1; then
 # helper source path resolves from this case's BASH_SOURCE root (contract)
  source "$CASE_DIR/fixtures/offline_helpers.sh"
fi
if ! declare -F scenario_create_tracked >/dev/null 2>&1; then
 # Pinned atomic cleanup surface (CASE-CONTRACT.md "Ortak atomik cleanup
 # helper"): the real owner is tests/scenario/lib/; when the loaded surface
 # does not carry it yet, this case's fixture stub defines the exact pinned
 # names/returns so the offline battery validates the CALLS.
  source "$CASE_DIR/fixtures/scenario_atomic_cleanup.sh"
fi

log() { printf '[%s] %s\n' "$CASE_NAME" "$*"; }
ev()  { [[ -n "${EVIDENCE_DIR:-}" ]] && printf '%s\n' "$*" >>"$EVIDENCE_DIR/run.log"; }

write_result_once() { # write_result_once <RESULT-line>
  [[ "${RESULT_WRITTEN:-0}" -eq 1 ]] && return 0
  if [[ -n "${EVIDENCE_DIR:-}" ]]; then
    mkdir -p -- "$EVIDENCE_DIR"
    printf '%s\n' "$1" >>"$EVIDENCE_DIR/result.txt"
  fi
  RESULT_WRITTEN=1
  log "$1"
}

finish() { # finish <rc> <RESULT-line>
  FINAL_RC=$1
  write_result_once "$2"
  exit "$1"
}

note_fail() { finish $RC_FAIL "ASSERT-FAIL: $1"; }

# --- readers over the real API surface (stdout of the helper) -------------
kube_get_json() { scenario_kube get "$@" -o json 2>/dev/null; }

obj_uid() {
  python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("metadata",{}).get("uid",""))
except ValueError:
    pass'
}

marker_digest() {
  python3 -c 'import json,sys,hashlib
try:
    d=json.load(sys.stdin)
except ValueError:
    sys.exit(0)
print(hashlib.sha256((d.get("data") or {}).get("payload","").encode()).hexdigest())'
}

cond_reason() { # cond_reason <quartet-json> <aggregate|profile> <type>
  local json="$1"
  COND_JSON="$json" python3 - "$2" "$3" <<'PY'
import json, os, sys
try:
    d = json.loads(os.environ.get("COND_JSON") or "{}")
except ValueError:
    d = {}
scope, ctype = sys.argv[1], sys.argv[2]
conds = []
if scope == "profile":
    conds = ((d.get("status", {}).get("profiles", {}) or {}).get("nfs", {}) or {}).get("conditions", []) or []
else:
    conds = d.get("status", {}).get("conditions", []) or []
for c in conds:
    if c.get("type") == ctype:
        print(c.get("reason", "")); break
else:
    print("")
PY
}

cond_status() { # cond_status <quartet-json> <aggregate|profile> <type>
  local json="$1"
  COND_JSON="$json" python3 - "$2" "$3" <<'PY'
import json, os, sys
try:
    d = json.loads(os.environ.get("COND_JSON") or "{}")
except ValueError:
    d = {}
scope, ctype = sys.argv[1], sys.argv[2]
conds = []
if scope == "profile":
    conds = ((d.get("status", {}).get("profiles", {}) or {}).get("nfs", {}) or {}).get("conditions", []) or []
else:
    conds = d.get("status", {}).get("conditions", []) or []
for c in conds:
    if c.get("type") == ctype:
        print(c.get("status", "")); break
else:
    print("")
PY
}

has_finalizer() { # has_finalizer <quartet-json> <finalizer> → prints 1/0
  local json="$1" fin="$2"
  COND_JSON="$json" FIN="$fin" python3 - <<'PY'
import json, os
try:
    d = json.loads(os.environ.get("COND_JSON") or "{}")
except ValueError:
    d = {}
fns = (d.get("metadata", {}).get("finalizers") or [])
print(1 if os.environ["FIN"] in fns else 0)
PY
}

is_terminating() { # is_terminating <quartet-json> → prints 1/0
  local json="$1"
  COND_JSON="$json" python3 - <<'PY'
import json, os
try:
    d = json.loads(os.environ.get("COND_JSON") or "{}")
except ValueError:
    d = {}
print(1 if "deletionTimestamp" in (d.get("metadata", {}) or {}) else 0)
PY
}

gen_caught_up() { # observedGeneration >= generation — the async-operator gate
  Q="${QUARTET_JSON:-}" python3 -c '
import json, os, sys
try:
    d = json.loads(os.environ.get("Q") or "{}")
except ValueError:
    sys.exit(1)
try:
    gen = int(d.get("metadata", {}).get("generation") or 0)
    og = int((d.get("status") or {}).get("observedGeneration") or 0)
except (TypeError, ValueError):
    sys.exit(1)
sys.exit(0 if og >= gen else 1)'
}

gen_info() {
  Q="${QUARTET_JSON:-}" python3 -c '
import json, os
try:
    d = json.loads(os.environ.get("Q") or "{}")
except ValueError:
    d = {}
print("%s/%s" % ((d.get("status") or {}).get("observedGeneration"),
                 d.get("metadata", {}).get("generation")))'
}

fetch_quartet() { QUARTET_JSON="$(scenario_kube get quartet quartet -o json 2>/dev/null || true)"; }

# --- bounded polls of the REAL API state (the operator is async) -----------
poll_quartet() { # poll_quartet <max> <predicate-fn over QUARTET_JSON>
  local i
  for ((i = 1; i <= $1; i++)); do
    fetch_quartet
    if [[ -n "$QUARTET_JSON" ]] && gen_caught_up && "$2"; then
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  return 1
}

poll_raw() { # poll_raw <max> <predicate-fn> — the predicate observes itself
  local i
  for ((i = 1; i <= $1; i++)); do
    if "$2"; then
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  return 1
}

pred_agg_ready() { [[ "$(cond_reason "${QUARTET_JSON:-}" aggregate Ready)" == "Ready" ]]; }

# Real usage observation: PV/PVC/VA by driver identity, in API STATE,
# never assumed from the create JSON (the API ignores status on create).
pred_usage_live() {
  local pv pvc va
  pv="$(kube_get_json persistentvolume "$PV_NAME" || true)"
  pvc="$(kube_get_json persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" || true)"
  va="$(kube_get_json volumeattachment "$VA_NAME" || true)"
  PV="$pv" PVC="$pvc" VA="$va" DRIVER="$DRIVER_NAME" PREFIX="$HANDLE_PREFIX" \
  PVNAME="$PV_NAME" PVCNAME="$PVC_NAME" python3 - <<'PY'
import json, os, sys
def load(v):
    try:
        return json.loads(v) if v else {}
    except ValueError:
        return {}
pv, pvc, va = load(os.environ["PV"]), load(os.environ["PVC"]), load(os.environ["VA"])
driver, prefix = os.environ["DRIVER"], os.environ["PREFIX"]
pvname, pvcname = os.environ["PVNAME"], os.environ["PVCNAME"]
csi = (pv.get("spec") or {}).get("csi") or {}
identity = (csi.get("driver") == driver
            or str(csi.get("volumeHandle", "")).startswith(prefix))
claim = (pv.get("spec") or {}).get("claimRef") or {}
ok = (
    identity
    and pv.get("status", {}).get("phase") == "Bound"
    and claim.get("name") == pvcname
    and pvc.get("status", {}).get("phase") == "Bound"
    and (pvc.get("spec") or {}).get("volumeName") == pvname
    and va.get("status", {}).get("attached") is True
    and ((va.get("spec") or {}).get("source") or {}).get("persistentVolumeName") == pvname
)
sys.exit(0 if ok else 1)
PY
}

# --- live vs offline usage mode -------------------------------------------
# Offline (KUBE_CONTEXT=offline-fake): synthetic spec-only PV/PVC/VA; the fake
# store simulates binding/attachment. LIVE: test-owned SC -> PVC -> real
# backend provision -> mount Pod on a worker -> real driver-created VA. No
# synthetic fallback live; setup failure is BLOCKED, never a fabricated PASS.
# Usage mode is an EXPLICIT runtime grant, never inferred from the context
# name: SCENARIO_USAGE_MODE=live selects real provisioning; the default
# (synthetic) keeps the offline fake-store and the recording integration
# fixture models. Synthetic usage can never PASS on a real cluster (a fake
# static handle does not attach), so a live PASS requires the explicit mode.
LIVE_USAGE=0
if [[ "${SCENARIO_USAGE_MODE:-synthetic}" == "live" ]]; then LIVE_USAGE=1; fi

kube_absent() { # kube_absent <get-args...> — typed NotFound ONLY (rc 0);
 # present rc 1; API error/refusal rc 3 — 403/5xx/timeout is NEVER absence.
  local errf="$EVIDENCE_DIR/absent-check.err" rc=0 reason
  scenario_kube get "$@" >/dev/null 2>"$errf" </dev/null || rc=$?
  if [[ $rc -eq 0 ]]; then return 1; fi
  if [[ $rc -eq 2 ]]; then return 3; fi
  reason="$(sed -nE 's/.*Error from server \(([A-Za-z]+)\).*/\1/p' "$errf" 2>/dev/null | head -n 1)"
  if [[ "$reason" == "NotFound" ]]; then return 0; fi
  return 3
}


# --- typed recovery postconditions (API error is NEVER absence) ------------
assert_owned_absent() { # <desc> <get-args...>
  local desc="$1"; shift
  local rc=0
  kube_absent "$@" || rc=$?
  case "$rc" in
    0) return 0 ;;
    1) note_fail "$desc survived clean recovery (typed present) — deleteOwnedProfile must remove UID-owned operands only after a CLEAN scan (apply.go:939-990)" ;;
    *) write_remaining_evidence
       finish $RC_BLOCKED "RESULT=BLOCKED $desc absence GET failed (API error/refusal) during recovery postcondition — API error is NEVER absence; evidence written" ;;
  esac
}
assert_foreign_present() { # <desc> <get-args...>
  local desc="$1"; shift
  local rc=0
  kube_absent "$@" || rc=$?
  case "$rc" in
    1) return 0 ;;
    0) note_fail "$desc deleted during UID-ownership cleanup — the operator deletes ONLY owner-uid-matching operands (apply.go:961-963, 1030-1072)" ;;
    *) write_remaining_evidence
       finish $RC_BLOCKED "RESULT=BLOCKED $desc presence GET failed (API error/refusal) — protection cannot be verified; evidence written" ;;
  esac
}
# Full runtime-owned set absence + installer-owned protection. SCC/RBAC are
# checked only when their kind was observable at baseline (platform surface).
assert_owned_set_absent() {
  assert_owned_absent "operator Deployment" deployment "$DEPLOYMENT_NAME" --namespace "$OPERAND_NS"
  assert_owned_absent "operator DaemonSet" daemonset "$DAEMONSET_NAME" --namespace "$OPERAND_NS"
  assert_owned_absent "CSIDriver" csidriver "$CSIDRIVER_NAME"
  assert_owned_absent "controller Service" service "$SERVICE_NAME" --namespace "$OPERAND_NS"
  assert_owned_absent "controller ServiceAccount" serviceaccount "$SA_CONTROLLER_NAME" --namespace "$OPERAND_NS"
  assert_owned_absent "node ServiceAccount" serviceaccount "$SA_NODE_NAME" --namespace "$OPERAND_NS"
  assert_owned_absent "coordination ConfigMap" configmap "$COORDINATION_CM_NAME" --namespace "$OPERAND_NS"
  if [[ "${SCC_OBSERVED:-0}" == "1" ]]; then
    assert_owned_absent "node SCC" scc "$SCC_NAME"
  fi
  local pj lj prc
  pj="$(kube_get_json pods --namespace "$OPERAND_NS")" || {
    write_remaining_evidence
    finish $RC_BLOCKED "RESULT=BLOCKED operand-namespace pod list GET failed during recovery postcondition — API error is never absence"; }
  prc=0
  printf '%s' "$pj" | CANARY="$CANARY_DEP_NAME" python3 -c '
import json,os,sys
try: d=json.load(sys.stdin)
except ValueError: sys.exit(2)
canary=os.environ["CANARY"]
for p in d.get("items",[]):
    owners=(p.get("metadata",{}).get("ownerReferences") or [])
    if any(o.get("kind")=="ReplicaSet" and str(o.get("name","")).startswith(canary+"-") for o in owners):
        continue
    sys.exit(1)
sys.exit(0)' || prc=$?
  if [[ $prc -eq 1 ]]; then note_fail "owned pod survived clean recovery (typed present) — full runtime-owned set must be absent"; fi
  if [[ $prc -ne 0 ]]; then write_remaining_evidence; finish $RC_BLOCKED "RESULT=BLOCKED pod list parse failure during recovery postcondition"; fi
  lj="$(kube_get_json leases --namespace "$OPERAND_NS")" || {
    write_remaining_evidence
    finish $RC_BLOCKED "RESULT=BLOCKED lease list GET failed during recovery postcondition — API error is never absence"; }
  prc=0
  printf '%s' "$lj" | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except ValueError: sys.exit(2)
locks=[l for l in d.get("items",[]) if "operation-lock" in l["metadata"]["name"]]
sys.exit(1 if locks else 0)' || prc=$?
  if [[ $prc -eq 1 ]]; then note_fail "operation-lock lease survived clean recovery — full runtime-owned set must be absent"; fi
  if [[ $prc -ne 0 ]]; then write_remaining_evidence; finish $RC_BLOCKED "RESULT=BLOCKED lease list parse failure during recovery postcondition"; fi
  if [[ "${RBAC_OBSERVED:-0}" == "1" ]]; then
    assert_foreign_present "installer Role" role "$INSTALL_ROLE_NAME" --namespace "$OPERAND_NS"
    assert_foreign_present "installer ClusterRole" clusterrole "$INSTALL_CLUSTERROLE_NAME"
  fi
}
# Platform surface probe (fail-closed): observable = present OR typed NotFound
# (kind exists); kind-absent on this platform = skip; API error/refusal = BLOCK.
probe_surface() { # <varname> <get-args...>
  local __var="$1"; shift
  local errf="$EVIDENCE_DIR/surface-probe.err" rc=0
  scenario_kube get "$@" -o json >/dev/null 2>"$errf" </dev/null || rc=$?
  if [[ $rc -eq 0 ]]; then printf -v "$__var" 1; return 0; fi
  if [[ $rc -eq 2 ]]; then finish $RC_BLOCKED "RESULT=BLOCKED surface probe refusal ($*) — fail-closed"; fi
  if grep -q "Error from server (NotFound)" "$errf"; then printf -v "$__var" 1; return 0; fi
  if grep -qE "doesn't have a resource type|no matches for kind|unknown kind" "$errf"; then printf -v "$__var" 0; return 0; fi
  finish $RC_BLOCKED "RESULT=BLOCKED surface probe API error ($*) — cannot establish platform surface; fail-closed"
}

live_pvc_bound() {
  local j
  j="$(kube_get_json persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" || true)"
  [[ -n "$j" ]] || return 1
  printf '%s' "$j" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",{}).get("phase",""))
except ValueError: pass' 2>/dev/null | grep -q '^Bound$'
}

live_pod_ready() {
  local j
  j="$(kube_get_json pod "$POD_NAME" --namespace "$TEST_NAMESPACE" || true)"
  [[ -n "$j" ]] || return 1
  printf '%s' "$j" | python3 -c '
import json,sys
try: p=json.load(sys.stdin)
except ValueError: sys.exit(1)
if p["metadata"].get("deletionTimestamp"): sys.exit(1)
cs=p.get("status",{}).get("conditions",[])
sys.exit(0 if next((c["status"] for c in cs if c["type"]=="ContainersReady"),"False")=="True" else 1)' 2>/dev/null
}

live_va_observed() {
  local j
  j="$(kube_get_json volumeattachments || true)"
  [[ -n "$j" ]] || return 1
  LIVE_VA_NAME=""
  LIVE_VA_UID=""
  local out
  out="$(printf '%s' "$j" | PVNAME="$PV_NAME" DRIVER="$DRIVER_NAME" python3 -c '
import json,os,sys
try: d=json.load(sys.stdin)
except ValueError: sys.exit(0)
for v in d.get("items",[]):
    sp=v.get("spec",{})
    if sp.get("attacher")==os.environ["DRIVER"] and (sp.get("source") or {}).get("persistentVolumeName")==os.environ["PVNAME"]:
        print(v["metadata"]["name"], v["metadata"]["uid"]); break' 2>/dev/null)"
  LIVE_VA_NAME="${out%% *}"
  LIVE_VA_UID="${out##* }"
  [[ -n "$LIVE_VA_NAME" && "$LIVE_VA_NAME" != "$out" ]]
}

live_pod_gone() { kube_absent pod "$POD_NAME" --namespace "$TEST_NAMESPACE"; }
live_pv_gone() { kube_absent persistentvolume "$PV_NAME"; }
live_va_gone() { kube_absent volumeattachment "$VA_NAME"; }
live_pvva_gone() { live_pv_gone && live_va_gone; }

# --- UID-ownership registry — the SHARED atomic cleanup surface -----
# Every create goes through scenario_create_tracked (pinned; CASE-CONTRACT
# "Ortak atomik cleanup helper"), every deletion through scenario_delete_owned:
# the run's obligations are exactly the ownership.tsv CREATE rows the helper
# verified with outcome=created (uid bound from the create response). No
# label sweep, no name guess — a swapped UID is a refusal (rc 2), never a
# delete; a refusal is never a success.
create_tracked() { # create_tracked <kind> <name> [--namespace <ns>] <manifest-file>
  local rc=0 uid base
  uid="$(scenario_create_tracked "$@" 2>"$EVIDENCE_DIR/create.err")" || rc=$?
  [[ -s "$EVIDENCE_DIR/create.err" ]] && cat "$EVIDENCE_DIR/create.err" >&2
  base="$(basename "${@: -1}")"
  if [[ "$rc" == "2" ]]; then
    finish $RC_BLOCKED \
      "RESULT=BLOCKED create of $base REFUSED by the scenario surface (rc 2 = refusal, never a success) — no adoption, zero further mutation, evidence written"
  elif [[ "$rc" == "1" ]]; then
    if grep -q "AlreadyExists" "$EVIDENCE_DIR/create.err" 2>/dev/null; then
      finish $RC_BLOCKED \
        "RESULT=BLOCKED create of $base rejected (AlreadyExists) — a same-name object not owned by this run exists; refusing to adopt it (pin 8); zero further mutation, evidence written"
    fi
    finish $RC_BLOCKED \
      "RESULT=BLOCKED create of $base API-rejected (rc 1) — precondition/seed unmet; refusing to continue without clean ownership (pin 8); zero further mutation, evidence written"
  elif [[ -z "$uid" ]]; then
    finish $RC_FAIL \
      "ASSERT-FAIL: create of $base returned rc 0 without a metadata.uid — ownership cannot be bound (fail-closed)"
  else
    MUTATIONS_MADE=1
    ev "created+owned: $1/${2}/$base uid=$uid"
  fi
}

tracked_uid() { # tracked_uid <kind> <ns-or-empty> <name> -> this run's UID
  local nsreg
  nsreg="${2:--}"
  awk -F'\t' -v k="$1" -v ns="$nsreg" -v n="$3" \
    '$1=="CREATE" && $6=="created" && $2==k && $3==ns && $4==n {u=$5} END {print u}' \
    "${REGISTRY:-/dev/null}" 2>/dev/null
}

delete_owned() { # <kind> <ns-or-empty> <name> <recorded-uid> — atomic (pinned)
  local nsarg=""
  [[ -n "$2" ]] && nsarg="--namespace $2"
  scenario_delete_owned "$1" "$3" $nsarg "$4"
}

case_cleanup() {
 # Restore + cleanup on every exit path (pins 7/8): remove ONLY this run's
 # own objects by recorded create-response UID. This is what unblocks the
 # usage gate if the run dies mid-termination — the FINALIZER itself is
 # never broken by this case (admin procedure, apply.go:1258-1277).
  CLEANED=1
  if [[ -n "${REGISTRY:-}" && -f "${REGISTRY:-}" ]]; then
    local k ns nm uid r attempt nsarg live
    while IFS=$'\t' read -r act k ns nm uid outcome run_id ts; do
      [[ "$act" == "CREATE" && "$outcome" == "created" && -n "${uid:-}" ]] || continue
      r=0; attempt=0; nsarg=""
      [[ "$ns" != "-" ]] && nsarg="--namespace $ns"
      while :; do
        delete_owned "$k" "$([[ "$ns" == "-" ]] && echo "" || echo "$ns")" "$nm" "$uid" || r=$?
        [[ "$r" == "0" ]] && break
 # rc 2 can be a transient postcondition (a live finalizer reaping the
 # case-created CR asynchronously) — retry bounded; rc 1 (API error)
 # is not retryable. A genuinely replaced object refuses identically
 # on every retry and is never touched.
        if [[ "$r" != "2" ]] || (( attempt >= ${SCENARIO_CLEANUP_RETRIES:-3} )); then
          break
        fi
        attempt=$((attempt + 1))
        sleep "${SCENARIO_CLEANUP_INTERVAL:-2}"
      done
      if [[ "$r" != "0" ]]; then
        if [[ "$r" == "2" ]]; then
          live="$(kube_get_json "$k" "$nm" $nsarg || true)"
          live="$(obj_uid <<<"$live")"
          if [[ -n "$live" && "$live" != "$uid" ]]; then
            log "cleanup: $k/${ns:-(cluster)}/$nm — captured UID $uid gone from that name (live $live); replacement preserved, obligation met"
            continue
          fi
        fi
        CLEANUP_RC=1
        log "cleanup: $k/${ns:-(cluster)}/$nm not cleanly removed (rc=$r) — reported separately (FAIL_CLEANUP if the run otherwise passed)"
      fi
    done < <(tac "$REGISTRY")
  fi
  if [[ "${LIVE_USAGE:-0}" -eq 1 && -n "${PV_NAME:-}" && -n "${VA_NAME:-}" ]]; then
 # live residue guarantee: after the registry cleanup (PVC deleted), the
 # driver must have completed PV/VA removal; verify typed absence, bounded.
    local lrc=0
    poll_raw "${SCENARIO_CLEANUP_POLLS:-30}" live_pvva_gone || lrc=1
    if [[ $lrc -ne 0 ]]; then
      CLEANUP_RC=1
      log "cleanup: live PV/VA still present after budget — reported (FAIL_CLEANUP if run otherwise passed)"
    fi
  fi
  write_remaining_evidence
  if [[ -n "${EVIDENCE_DIR:-}" ]]; then
    mkdir -p -- "$EVIDENCE_DIR"
    echo "CLEANUP=$([[ $CLEANUP_RC -eq 0 ]] && echo OK || echo FAILED)" >>"$EVIDENCE_DIR/cleanup.txt"
  fi
  if [[ $CLEANUP_RC -ne 0 && $FINAL_RC -eq $RC_PASS ]]; then
    FINAL_RC=$RC_FAIL_CLEANUP
    write_result_once "RESULT=FAIL_CLEANUP cleanup failed — no final success (exit 4, pin 7)"
  fi
}

on_unexpected_error() { # ERR trap: unguarded shell failure is fail-closed
  log "UNEXPECTED shell error (line ${1:-?}: ${BASH_COMMAND:-?}) — fail-closed, the result stays FAIL (pin 6)"
  write_result_once "RESULT=FAIL unexpected shell error — fail-closed default; no successful completion reached (pin 6)"
  exit $RC_FAIL
}

on_exit() { # EXIT trap: restore/cleanup on EVERY exit path (pin 8)
  if [[ "${MUTATIONS_MADE:-0}" -eq 1 && "${CLEANED:-0}" -eq 0 ]]; then
    case_cleanup
  fi
  if [[ "${RESULT_WRITTEN:-0}" -eq 0 ]]; then
    case "${FINAL_RC:-1}" in
      $RC_BLOCKED) write_result_once "RESULT=BLOCKED unexpected exit before an explicit decision — fail-closed (pin 6)" ;;
      *)           write_result_once "RESULT=FAIL unexpected exit — fail-closed default: rc0 only on explicit successful completion" ;;
    esac
  fi
  exit "$FINAL_RC"
}
trap on_unexpected_error ERR
trap on_exit EXIT

write_remaining_evidence() {
  [[ -n "${EVIDENCE_DIR:-}" ]] || return 0
  mkdir -p -- "$EVIDENCE_DIR/lists"
  : >"$EVIDENCE_DIR/remaining-objects.json"
  local kind list_json ns listfile rc
 # File transport (E2BIG fix): cluster-wide list JSON can exceed the
 # per-env/argv size limit (MAX_ARG_STRLEN). The raw list is written to an
 # evidence file and python reads the FILE — never env/argv. A failed GET
 # is recorded as an explicit error entry; 403/5xx/timeout never becomes an
 # empty list or a silent PASS (absence is only a typed NotFound).
  for kind in $KINDS; do
    case "$kind" in
      node|quartet|csidriver|persistentvolume|volumeattachment)
        listfile="$EVIDENCE_DIR/lists/$kind.json"
        if list_json="$(kube_get_json "$kind")"; then
          printf '%s' "$list_json" >"$listfile"
          LIST_FILE="$listfile" QUID="${q_uid:-}" REG="${REGISTRY:-}" CASE_NAME="$CASE_NAME" python3 - "$kind" <<'PY' >>"$EVIDENCE_DIR/remaining-objects.json"
import json, os, sys
kind = sys.argv[1]
try:
    d = json.load(open(os.environ["LIST_FILE"]))
except (ValueError, OSError) as e:
    print(json.dumps({"kind": kind, "error": "LIST_PARSE_FAIL", "detail": str(e)[:120]}))
    sys.exit(0)
registry = set()
reg = os.environ.get("REG")
if reg and os.path.exists(reg):
    for ln in open(reg):
        parts = ln.rstrip("\n").split("\t")
 # ownership.tsv row: action kind namespace name uid outcome run_id ts
 # (namespace "-" for cluster-scoped kinds); only verified CREATE rows
 # are case obligations.
        if len(parts) == 8 and parts[0] == "CREATE" and parts[5] == "created":
            registry.add((parts[1], "" if parts[2] == "-" else parts[2],
                          parts[3], parts[4]))
q_uid = os.environ.get("QUID", "")
for o in d.get("items", []):
    md = o.get("metadata", {})
    anns = md.get("annotations", {}) or {}
    if (kind, md.get("namespace", ""), md.get("name", ""), md.get("uid", "")) in registry:
        owner = "case-owned"
    elif kind == "quartet":
        owner = "target"
    elif anns.get("quartet.ngxstorage.com/owner-uid") == q_uid:
        owner = "operator-owned"
    else:
        owner = "foreign"
    print(json.dumps({"kind": kind, "name": md.get("name"),
                      "namespace": md.get("namespace", ""),
                      "uid": md.get("uid", ""), "owner": owner}))
PY
        else
          rc=$?
          printf '{"kind": "%s", "error": "GET_FAILED_RC_%s"}\n' "$kind" "$rc" >>"$EVIDENCE_DIR/remaining-objects.json"
        fi
        ;;
      *)
        for ns in "$TEST_NAMESPACE" "$OPERAND_NS"; do
          listfile="$EVIDENCE_DIR/lists/$kind.$ns.json"
          if list_json="$(kube_get_json "$kind" --namespace "$ns")"; then
            printf '%s' "$list_json" >"$listfile"
            LIST_FILE="$listfile" QUID="${q_uid:-}" REG="${REGISTRY:-}" python3 - "$kind" <<'PY' >>"$EVIDENCE_DIR/remaining-objects.json"
import json, os, sys
kind = sys.argv[1]
try:
    d = json.load(open(os.environ["LIST_FILE"]))
except (ValueError, OSError) as e:
    print(json.dumps({"kind": kind, "error": "LIST_PARSE_FAIL", "detail": str(e)[:120]}))
    sys.exit(0)
registry = set()
reg = os.environ.get("REG")
if reg and os.path.exists(reg):
    for ln in open(reg):
        parts = ln.rstrip("\n").split("\t")
        if len(parts) == 8 and parts[0] == "CREATE" and parts[5] == "created":
            registry.add((parts[1], "" if parts[2] == "-" else parts[2],
                          parts[3], parts[4]))
q_uid = os.environ.get("QUID", "")
for o in d.get("items", []):
    md = o.get("metadata", {})
    anns = md.get("annotations", {}) or {}
    if (kind, md.get("namespace", ""), md.get("name", ""), md.get("uid", "")) in registry:
        owner = "case-owned"
    elif anns.get("quartet.ngxstorage.com/owner-uid") == q_uid:
        owner = "operator-owned"
    else:
        owner = "foreign"
    print(json.dumps({"kind": kind, "name": md.get("name"),
                      "namespace": md.get("namespace", ""),
                      "uid": md.get("uid", ""), "owner": owner}))
PY
          else
            rc=$?
            printf '{"kind": "%s", "namespace": "%s", "error": "GET_FAILED_RC_%s"}\n' "$kind" "$ns" "$rc" >>"$EVIDENCE_DIR/remaining-objects.json"
          fi
        done
        ;;
    esac
  done
}

# ============================================================================
preflight_rc=0
scenario_preflight "$CASE_DIR/case.json" || preflight_rc=$?
if [[ $preflight_rc -ne 0 ]]; then
  if [[ $preflight_rc -eq 2 ]]; then
    finish $RC_BLOCKED "RESULT=BLOCKED preflight rejected (rc=2) — mutation rejected before anything"
  fi
  finish $RC_FAIL "RESULT=FAIL preflight failed (rc=$preflight_rc) — fail-closed, no mutation performed"
fi

# --- PRE-FLIGHT verb-surface check — before ANY cluster call --------
# Static self-scan: every scenario_kube call in this body must use a declared
# real-kubectl verb. Detected here (and again by the helper at dispatch time),
# never discovered after a mutation.
verbs_found="$(grep -E 'scenario_kube[[:space:]]+[a-z][a-z-]*' "$CASE_DIR/case.sh" \
  | grep -vE '^[[:space:]]*#' \
  | grep -oE 'scenario_kube[[:space:]]+[a-z][a-z-]*' \
  | awk '{print $2}' | sort -u)"
while IFS= read -r v; do
  [[ -n "$v" ]] || continue
  if [[ ! "$KUBE_VERBS" == *" $v "* ]]; then
    note_fail "PREFLIGHT-VERB-REJECT: this body calls the helper with verb '$v', outside the declared kubectl verb surface ($KUBE_VERBS) — rejected BEFORE any mutation; no such kubectl verb exists, the async operator is observed, not commanded"
  fi
done <<<"$verbs_found"
ev "pre-flight verb surface verified: $KUBE_VERBS"

log "run=$RUN_ID store=${SCENARIO_FAKE_STORE:-n/a} evidence=$EVIDENCE_DIR"
if [[ -z "${SCENARIO_FAKE_STORE:-}" || -z "${EVIDENCE_DIR:-}" ]]; then
  finish $RC_FAIL "RESULT=FAIL preflight ran but did not bind SCENARIO_FAKE_STORE/EVIDENCE_DIR — fail-closed, no mutation performed"
fi
REGISTRY="$EVIDENCE_DIR/ownership.tsv"
: >"$REGISTRY"
MANIFESTS_DIR="$(dirname "$SCENARIO_FAKE_STORE")/manifests"
mkdir -p -- "$MANIFESTS_DIR"

# ----------------------------------------------------------------------------
# 1) preconditions (zero mutation): the canonical singleton + an attachable
# node must already exist on the target. This case NEVER creates the
# singleton — effect-based classification.
# ----------------------------------------------------------------------------
q="$(kube_get_json quartet quartet || true)"
if [[ -z "$q" ]]; then
  finish $RC_BLOCKED "RESULT=BLOCKED precondition unmet: the canonical singleton Quartet CR is absent on the target — this case never creates or adopts it (pin 1); zero mutation performed"
fi
q_uid="$(obj_uid <<<"$q")"

node_name="$(scenario_kube get nodes -o json 2>/dev/null | python3 -c 'import json,sys
try:
    items = (json.load(sys.stdin).get("items") or [])
except ValueError:
    items = []
print(items[0]["metadata"]["name"] if items else "")' || true)"
if [[ -z "$node_name" ]]; then
  finish $RC_BLOCKED "RESULT=BLOCKED precondition unmet: no attachable node on the target — a VolumeAttachment needs a real spec.nodeName (pin 4); zero mutation performed"
fi
ev "preconditions met: singleton quartet uid=$q_uid, node=$node_name"

# ----------------------------------------------------------------------------
# 2) seed API-valid usage objects + marker canary + canaries (spec-only —
# no pre-set Bound/attached status; create responses bind the UIDs)
# ----------------------------------------------------------------------------
LABELS="$(SC_CASE="$CASE_NAME" SC_RUN="$RUN_ID" python3 -c 'import json, os; print(json.dumps({"quartet-scenario.test/case": os.environ["SC_CASE"], "quartet-scenario.test/run": os.environ["SC_RUN"]}))')"
if [[ $LIVE_USAGE -eq 0 ]]; then
python3 - "$MANIFESTS_DIR" "$RUN_ID" "$LABELS" "$TEST_NAMESPACE" "$OPERAND_NS" "$DRIVER_NAME" "$HANDLE_PREFIX" "$node_name" "$PV_NAME" "$PVC_NAME" "$VA_NAME" "$MARKER_NAME" "$CANARY_DEP_NAME" "$CANARY_CM_NAME" <<'PY'
import json, sys
(out, run_id, labels, test_ns, operand_ns, driver, prefix,
 node_name, pv_name, pvc_name, va_name, marker_name,
 canary_dep_name, canary_cm_name) = sys.argv[1:15]
labels = json.loads(labels)

def w(fn, obj):
    with open(f"{out}/{fn}", "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)

# Driver-identity PV (spec ONLY — the binding controller sets claimRef and
# Bound status; identity rules: apply.go:1207-1216).
w("data-pv.json", {
    "apiVersion": "v1", "kind": "PersistentVolume",
    "metadata": {"name": pv_name, "labels": labels},
    "spec": {"capacity": {"storage": "1Gi"},
             "accessModes": ["ReadWriteOnce"],
             "csi": {"driver": driver, "volumeHandle": prefix + pv_name}},
})

# PVC in TEST_NAMESPACE naming the PV (spec ONLY — no pre-set Bound status).
w("data-pvc.json", {
    "apiVersion": "v1", "kind": "PersistentVolumeClaim",
    "metadata": {"name": pvc_name, "namespace": test_ns, "labels": labels},
    "spec": {"volumeName": pv_name, "accessModes": ["ReadWriteOnce"],
             "resources": {"requests": {"storage": "1Gi"}}},
})

# VolumeAttachment (spec ONLY — status.attached comes from the attacher,
# never from the create JSON; spec.attacher is REQUIRED by the API).
w("data-va.json", {
    "apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment",
    "metadata": {"name": va_name, "labels": labels},
    "spec": {"attacher": driver, "nodeName": node_name,
             "source": {"persistentVolumeName": pv_name}},
})

# Marker: NAMESPACED canary proving customer data untouched (the operator never touches customer
# objects in TEST_NAMESPACE). NOT volume-data-integrity evidence — that is
# the live I/O cells' job (serial live worker, NOT RUN here).
w("data-marker.json", {
    "apiVersion": "v1", "kind": "ConfigMap",
    "metadata": {"name": marker_name, "namespace": test_ns, "labels": labels},
    "data": {"payload": f"scenario test data marker run={run_id} — operator must never touch this (customer data untouched)"},
})

# FOREIGN canaries (to the OPERATOR: not this CR's owner-uid): API-valid
# Deployment with selector + template, plus a plain ConfigMap. They are
# still THIS run's objects, so they go into the UID registry and the case
# owns their cleanup.
w("foreign-deployment.json", {
    "apiVersion": "apps/v1", "kind": "Deployment",
    "metadata": {"name": canary_dep_name, "namespace": operand_ns, "labels": labels,
                 "annotations": {"quartet.ngxstorage.com/owner-uid": "foreign-owner-uid"}},
    "spec": {"replicas": 1,
             "selector": {"matchLabels": {"app": "foreign-canary"}},
             "template": {"metadata": {"labels": {"app": "foreign-canary"}},
                          "spec": {"containers": [{"name": "canary",
                                                   "image": "registry.k8s.io/pause:3.10"}]}}},
})
w("foreign-configmap.json", {
    "apiVersion": "v1", "kind": "ConfigMap",
    "metadata": {"name": canary_cm_name, "namespace": test_ns, "labels": labels},
    "data": {"payload": "not ours"},
})
PY
create_tracked persistentvolume "$PV_NAME" "$MANIFESTS_DIR/data-pv.json"
create_tracked persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/data-pvc.json"
create_tracked volumeattachment "$VA_NAME" "$MANIFESTS_DIR/data-va.json"
create_tracked configmap "$MARKER_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/data-marker.json"
create_tracked deployment "$CANARY_DEP_NAME" --namespace "$OPERAND_NS" "$MANIFESTS_DIR/foreign-deployment.json"
create_tracked configmap "$CANARY_CM_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/foreign-configmap.json"
ev "seeded: usage $PV_NAME/$PVC_NAME/$VA_NAME + marker canary + canaries (UIDs bound from create responses)"
else
 # LIVE: real provisioned usage. SC/PVC/Pod/marker/canaries are case-created
 # (CREATE rows); PV and VA are driver/controller-created — their UIDs are
 # OBSERVED through the claim/ref chain (OBSERVE rows) and cleanup verifies
 # the same chain; a foreign replacement is preserved, never adopted.
  SC_NAME="scenario-data-sc"
  POD_NAME="scenario-data-pod"
  POD_IMAGE="${SCENARIO_TEST_POD_IMAGE:-docker.io/library/busybox:1.36}"
  POD_PULL="${SCENARIO_TEST_POD_PULL_POLICY:-IfNotPresent}"
  node_name="$(scenario_kube get nodes -o json 2>/dev/null | python3 -c 'import json,sys
try:
    items=(json.load(sys.stdin).get("items") or [])
except ValueError:
    items=[]
workers=[n for n in items if not ({"node-role.kubernetes.io/control-plane","node-role.kubernetes.io/master"} & set((n["metadata"].get("labels") or {}).keys()))]
pick=workers or items
print(pick[0]["metadata"]["name"] if pick else "")' || true)"
  [[ -n "$node_name" ]] || finish $RC_BLOCKED "RESULT=BLOCKED live setup: no schedulable worker node — zero mutation"
  python3 - "$MANIFESTS_DIR" "$RUN_ID" "$LABELS" "$TEST_NAMESPACE" "$DRIVER_NAME" "$SC_NAME" "$PVC_NAME" "$POD_NAME" "$POD_IMAGE" "$POD_PULL" "$node_name" "$MARKER_NAME" "$CANARY_DEP_NAME" "$CANARY_CM_NAME" "$OPERAND_NS" <<'PY'
import json, sys
(out, run_id, labels, test_ns, driver, sc_name, pvc_name, pod_name, pod_image, pod_pull,
 node_name, marker_name, canary_dep_name, canary_cm_name, operand_ns) = sys.argv[1:16]
labels = json.loads(labels)

def w(fn, obj):
    with open(f"{out}/{fn}", "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)

w("data-sc.json", {
    "apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
    "metadata": {"name": sc_name, "labels": labels},
    "provisioner": driver,
    "parameters": {"blocksize": "128k", "compression": "on",
                    "deduplication": "on", "permissions": "111111111"},
    "reclaimPolicy": "Delete", "volumeBindingMode": "Immediate",
})
w("data-pvc.json", {
    "apiVersion": "v1", "kind": "PersistentVolumeClaim",
    "metadata": {"name": pvc_name, "namespace": test_ns, "labels": labels},
    "spec": {"accessModes": ["ReadWriteOnce"], "storageClassName": sc_name,
             "resources": {"requests": {"storage": "1Gi"}}},
})
w("data-pod.json", {
    "apiVersion": "v1", "kind": "Pod",
    "metadata": {"name": pod_name, "namespace": test_ns, "labels": labels},
    "spec": {"nodeSelector": {"kubernetes.io/hostname": node_name},
             "containers": [{"name": "writer", "image": pod_image,
                              "imagePullPolicy": pod_pull,
                              "command": ["sh", "-c", "sleep 3600"],
                              "volumeMounts": [{"name": "data", "mountPath": "/data"}]}],
             "volumes": [{"name": "data",
                           "persistentVolumeClaim": {"claimName": pvc_name}}]},
})
w("data-marker.json", {
    "apiVersion": "v1", "kind": "ConfigMap",
    "metadata": {"name": marker_name, "namespace": test_ns, "labels": labels},
    "data": {"payload": f"scenario test data marker run={run_id} — operator must never touch this (customer data untouched)"},
})
w("foreign-deployment.json", {
    "apiVersion": "apps/v1", "kind": "Deployment",
    "metadata": {"name": canary_dep_name, "namespace": operand_ns, "labels": labels,
                 "annotations": {"quartet.ngxstorage.com/owner-uid": "foreign-owner-uid"}},
    "spec": {"replicas": 1,
             "selector": {"matchLabels": {"app": "foreign-canary"}},
             "template": {"metadata": {"labels": {"app": "foreign-canary"}},
                          "spec": {"containers": [{"name": "canary",
                                                    "image": "registry.k8s.io/pause:3.10"}]}}},
})
w("foreign-configmap.json", {
    "apiVersion": "v1", "kind": "ConfigMap",
    "metadata": {"name": canary_cm_name, "namespace": test_ns, "labels": labels},
    "data": {"payload": "not ours"},
})
PY
  create_tracked storageclass "$SC_NAME" "$MANIFESTS_DIR/data-sc.json"
  create_tracked persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/data-pvc.json"
  if ! poll_raw "$MAX_POLLS" live_pvc_bound; then
    write_remaining_evidence
    finish $RC_BLOCKED "RESULT=BLOCKED live setup: PVC never Bound within $MAX_POLLS polls — real provisioning failed; no synthetic fallback, no status fabrication"
  fi
  PV_NAME="$(kube_get_json persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" | python3 -c 'import json,sys; print(json.load(sys.stdin)["spec"]["volumeName"])')"
  pv_json="$(kube_get_json persistentvolume "$PV_NAME")"
  PV_UID="$(obj_uid <<<"$pv_json")"
  VOLUME_HANDLE="$(printf '%s' "$pv_json" | python3 -c 'import json,sys; print((json.load(sys.stdin)["spec"].get("csi") or {}).get("volumeHandle",""))')"
  [[ -n "$PV_UID" && -n "$VOLUME_HANDLE" ]] || finish $RC_BLOCKED "RESULT=BLOCKED live setup: PV identity (UID/volumeHandle) unobservable — refusing to assume usage"
  printf 'OBSERVE\tpersistentvolume\t-\t%s\t%s\tobserved\t%s\t%s\n' "$PV_NAME" "$PV_UID" "$RUN_ID" "$(date -u +%Y%m%dT%H%M%SZ)" >>"$REGISTRY"
  create_tracked pod "$POD_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/data-pod.json"
  if ! poll_raw "$MAX_POLLS" live_pod_ready; then
    write_remaining_evidence
    finish $RC_BLOCKED "RESULT=BLOCKED live setup: mount pod never Ready within $MAX_POLLS polls"
  fi
  if ! poll_raw "$MAX_POLLS" live_va_observed; then
    write_remaining_evidence
    finish $RC_BLOCKED "RESULT=BLOCKED live setup: no driver VolumeAttachment observed within $MAX_POLLS polls"
  fi
  VA_NAME="$LIVE_VA_NAME"
  VA_UID="$LIVE_VA_UID"
  printf 'OBSERVE\tvolumeattachment\t-\t%s\t%s\tobserved\t%s\t%s\n' "$VA_NAME" "$VA_UID" "$RUN_ID" "$(date -u +%Y%m%dT%H%M%SZ)" >>"$REGISTRY"
  create_tracked configmap "$MARKER_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/data-marker.json"
  create_tracked deployment "$CANARY_DEP_NAME" --namespace "$OPERAND_NS" "$MANIFESTS_DIR/foreign-deployment.json"
  create_tracked configmap "$CANARY_CM_NAME" --namespace "$TEST_NAMESPACE" "$MANIFESTS_DIR/foreign-configmap.json"
  ev "live seeded: SC/$SC_NAME PVC/$PVC_NAME -> PV/$PV_NAME uid=$PV_UID handle=$VOLUME_HANDLE pod/$POD_NAME VA/$VA_NAME uid=$VA_UID (driver-created, UIDs observed via ref chain)"
fi

# ----------------------------------------------------------------------------
# 3) OBSERVE the active usage in the real API state — never assume it
# ----------------------------------------------------------------------------
if ! poll_raw "$MAX_POLLS" pred_usage_live; then
  write_remaining_evidence
  finish $RC_BLOCKED "RESULT=BLOCKED active usage never became observable in the real API state within $MAX_POLLS bounded polls (PV Bound + claimRef, PVC Bound, VA attached, by driver identity $DRIVER_NAME) — the gate cannot be tested without observed usage; refusing to assume it (pin 4); evidence written"
fi
ev "usage OBSERVED in API state: $PV_NAME Bound (claimRef→$PVC_NAME), $PVC_NAME Bound, $VA_NAME attached — driver identity $DRIVER_NAME / $HANDLE_PREFIX"

# ----------------------------------------------------------------------------
# 4) baseline: enabled profile converges (observedGeneration-bound poll) and
# carries the finalizer; identity bound to evidence
# ----------------------------------------------------------------------------
if ! poll_quartet "$MAX_POLLS" pred_agg_ready; then
  write_remaining_evidence
  finish $RC_BLOCKED "RESULT=BLOCKED enabled profile did not converge to aggregate Ready within $MAX_POLLS bounded polls (observedGeneration-bound) — baseline inconclusive; evidence written"
fi
fetch_quartet
[[ "$(has_finalizer "$QUARTET_JSON" "$FINALIZER")" == "1" ]] || note_fail "finalizer $FINALIZER missing at baseline — the controller ensures it on first sight (apply.go:31)"

dep="$(kube_get_json deployment "$DEPLOYMENT_NAME" --namespace "$OPERAND_NS" || true)"
dep_uid="$(obj_uid <<<"$dep")"
[[ -n "$dep_uid" ]] || note_fail "operator Deployment missing at baseline — enabled profile must apply its operands"
kube_get_json csidriver "$CSIDRIVER_NAME" >/dev/null || note_fail "CSIDriver missing at baseline"
SCC_OBSERVED=0
RBAC_OBSERVED=0
probe_surface SCC_OBSERVED scc "$SCC_NAME"
probe_surface RBAC_OBSERVED role "$INSTALL_ROLE_NAME" --namespace "$OPERAND_NS"
ev "platform surface probed: SCC_OBSERVED=$SCC_OBSERVED RBAC_OBSERVED=$RBAC_OBSERVED"
marker="$(kube_get_json configmap "$MARKER_NAME" --namespace "$TEST_NAMESPACE" || true)"
marker_sha="$(marker_digest <<<"$marker")"
pv_now="$(kube_get_json persistentvolume "$PV_NAME" || true)"
pvc_now="$(kube_get_json persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" || true)"
va_now="$(kube_get_json volumeattachment "$VA_NAME" || true)"

ID_LIVE="$LIVE_USAGE" ID_HANDLE="${VOLUME_HANDLE:-}" ID_Q_UID="$q_uid" ID_FINALIZER="$FINALIZER" ID_DEP="$dep" ID_MARKER_SHA="$marker_sha" \
ID_NODE="$node_name" ID_DRIVER="$DRIVER_NAME" ID_PREFIX="$HANDLE_PREFIX" \
ID_PV="$pv_now" ID_PVC="$pvc_now" ID_VA="$va_now" \
python3 - <<'PY'
import json, os
def uid_of(v):
    try:
        return json.loads(v).get("metadata", {}).get("uid", "")
    except ValueError:
        return ""
json.dump({
  "run_id": os.environ["RUN_ID"],
  "kube_context": os.environ["KUBE_CONTEXT"],
  "test_namespace": os.environ["TEST_NAMESPACE"],
  "case": os.environ["CASE_NAME"],
  "node_name": os.environ.get("ID_NODE", ""),
  "source_rev": os.environ.get("SOURCE_REV", ""),
  "case_files_sha256": os.environ.get("CASE_SHA", ""),
  "quartet_uid": os.environ["ID_Q_UID"],
  "finalizer": os.environ["ID_FINALIZER"],
  "operand_deployment_uid": uid_of(os.environ.get("ID_DEP", "")),
  "marker_canary_sha256": os.environ.get("ID_MARKER_SHA", ""),
  "live_usage": os.environ.get("ID_LIVE", "0"),
  "volume_handle": os.environ.get("ID_HANDLE", ""),
  "usage_observed": {
     "driver": os.environ["ID_DRIVER"], "handle_prefix": os.environ["ID_PREFIX"],
     "pv_uid": uid_of(os.environ.get("ID_PV", "")),
     "pvc_uid": uid_of(os.environ.get("ID_PVC", "")),
     "va_uid": uid_of(os.environ.get("ID_VA", "")),
  },
}, open(os.environ["EVIDENCE_DIR"] + "/identity.json", "w"), indent=1, sort_keys=True)
PY
ev "identity bound: quartet_uid=$q_uid finalizer=$FINALIZER deployment_uid=$dep_uid marker_canary_sha256=$marker_sha usage uids recorded"

# ----------------------------------------------------------------------------
# 5) MUTATION: delete the singleton CR (active-volume-delete fault). Expect a BLOCKED termination —
# finalizer kept, CR terminating, DataInUse on the aggregate. Never GC.
# ----------------------------------------------------------------------------
# Re-verify the precondition target immediately before the mutation: the
# singleton was captured at step 1, and a swapped UID means the mutation
# would delete an object this run never owned (pin 1: precondition, never
# adopted — a refusal/swap is never a success).
q_now="$(kube_get_json quartet quartet || true)"
[[ "$(obj_uid <<<"$q_now")" == "$q_uid" ]] || note_fail "singleton quartet UID changed before the delete mutation — the precondition target was swapped under the case; refusing to delete a replaced object (pin 1)"
rc=0
scenario_kube delete quartet quartet || rc=$?
if [[ "$rc" == "2" ]]; then
  finish $RC_BLOCKED "RESULT=BLOCKED delete mutation REFUSED by the scenario surface (rc 2 = refusal, never a success) — zero mutation, evidence written"
fi
if [[ "$rc" != "0" ]]; then
  note_fail "delete mutation failed (API rejected the deletion itself, rc=$rc)"
fi
ev "mutation applied: delete quartet quartet (uid $q_uid)"

# bounded poll of the real API state: a vanished CR or a Ready aggregate is
# premature success; a gate reason is the expected block; silence past the
# budget is inconclusive.
blocked_reason=""
for ((pass_n = 1; pass_n <= MAX_POLLS; pass_n++)); do
  fetch_quartet
  if [[ -z "$QUARTET_JSON" ]]; then
    note_fail "premature finalizer removal: CR deleted while observed usage still present — handleTermination must KEEP $FINALIZER and surface the gate reason (apply.go:1297-1342, 1275-1295; finalizer apply.go:31)"
  fi
  r=""
  if gen_caught_up; then
    r="$(cond_reason "$QUARTET_JSON" aggregate Ready)"
  fi
  case "$r" in
    Ready)
      note_fail "premature success: aggregate reported Ready while observed usage still present — expected a DataInUse block (apply.go:1324-1328, :1255; reason const quartet_types.go:87)" ;;
    DataInUse|PermissionDenied|DependencyMissing)
      blocked_reason="$r"; break ;;
  esac
  ev "block-observe pass $pass_n: aggregate Ready reason=${r:-<not-observed>} observedGeneration=$(gen_info) finalizer=$(has_finalizer "$QUARTET_JSON" "$FINALIZER")"
  sleep "$POLL_INTERVAL"
done
if [[ -z "$blocked_reason" ]]; then
  write_remaining_evidence
  finish $RC_BLOCKED "RESULT=BLOCKED no usage-gate block surfaced within $MAX_POLLS bounded polls — inconclusive within budget (the gate is async); evidence + remaining objects written"
fi
log "deletion blocked as expected: reason=$blocked_reason (retryable requeue, quartet_controller.go:45)"

# ----------------------------------------------------------------------------
# 6) blocked-termination evidence: terminating + finalizer kept + aggregate
# surface + retention (operands, usage, marker canary, canaries, identity)
# ----------------------------------------------------------------------------
# CR state checks run over the already-fetched JSON (no new API call — the
# operator is async and any fetch could advance a pass); then object
# survival, usage data FIRST: a stolen/destroyed usage set lets a later
# terminating pass legitimately proceed, so the customer-data retention properties
# must be asserted before that transition is observed.
[[ "$(is_terminating "$QUARTET_JSON")" == "1" ]] || note_fail "CR must carry deletionTimestamp while the gate blocks (terminating, not gone)"
[[ "$(has_finalizer "$QUARTET_JSON" "$FINALIZER")" == "1" ]] || note_fail "finalizer $FINALIZER removed while usage present — setBlockedTermination must keep it (apply.go:1275-1295)"
[[ "$(cond_status "$QUARTET_JSON" aggregate Degraded)" == "True" ]] || note_fail "aggregate Degraded must be True while deletion is blocked (apply.go:1281)"
agg_ready="$(cond_reason "$QUARTET_JSON" aggregate Ready)"
[[ "$agg_ready" == "$blocked_reason" ]] || note_fail "aggregate Ready reason=$agg_ready, want $blocked_reason (apply.go:1278-1281)"
[[ "$(obj_uid <<<"$QUARTET_JSON")" == "$q_uid" ]] || note_fail "singleton quartet UID changed — wrong operand identity"

kube_get_json persistentvolume "$PV_NAME" >/dev/null || note_fail "test data removed while usage present: PV $PV_NAME (customer data untouched, apply.go:97-98)"
kube_get_json persistentvolumeclaim "$PVC_NAME" --namespace "$TEST_NAMESPACE" >/dev/null || note_fail "test data removed while usage present: PVC $PVC_NAME (customer data untouched, apply.go:97-98)"
kube_get_json volumeattachment "$VA_NAME" >/dev/null || note_fail "test data removed while usage present: VA $VA_NAME (customer data untouched, apply.go:97-98)"
m="$(kube_get_json configmap "$MARKER_NAME" --namespace "$TEST_NAMESPACE" || true)"
[[ "$(marker_digest <<<"$m")" == "$marker_sha" ]] || note_fail "marker canary payload changed while usage present — customer data: objects in TEST_NAMESPACE are never touched (apply.go:97-98)"
dep_now="$(kube_get_json deployment "$DEPLOYMENT_NAME" --namespace "$OPERAND_NS" || true)"
printf '%s' "$dep_now" | grep -q "\"$dep_uid\"" || note_fail "operator Deployment removed while deletion blocked — gate must cancel cleanup (apply.go:1324-1328)"
kube_get_json csidriver "$CSIDRIVER_NAME" >/dev/null || note_fail "CSIDriver removed while deletion blocked — gate must cancel cleanup (apply.go:1324-1328)"
kube_get_json deployment "$CANARY_DEP_NAME" --namespace "$OPERAND_NS" >/dev/null || note_fail "foreign canary Deployment touched by the operator (UID ownership, apply.go:961-963)"
kube_get_json configmap "$CANARY_CM_NAME" --namespace "$TEST_NAMESPACE" >/dev/null || note_fail "foreign canary ConfigMap touched by the operator (label/namespace ownership)"
ev "blocked-termination verified: CR terminating, finalizer kept, aggregate Degraded=True/$blocked_reason, observed usage + marker canary (sha256 $marker_sha) + operands (deployment uid $dep_uid) + canaries intact"

# ----------------------------------------------------------------------------
# 7) controlled recovery WITHOUT override: usage clears → replay removes the
# finalizer only after a CLEAN scan, then the API GCs the CR. forceDisable
# (apply.go:1086-1092) and a manual finalizer break (apply.go:1258-1277)
# are admin procedures — never set by this case.
# ----------------------------------------------------------------------------
if [[ $LIVE_USAGE -eq 1 ]]; then
 # LIVE: pod first, then PVC; wait for the DRIVER to complete PV/VA/backend
 # cleanup before the gate may pass (Released-PV drain: a Released PV does
 # not block the gate, so disabling before cleanup finishes can orphan the
 # PV finalizer — the runner waits for full cleanup instead). No finalizer
 # break; a stuck SAME-UID object is deleted under UID precondition only
 # after the bounded natural-cleanup wait; a foreign replacement is preserved.
  u="$(tracked_uid pod "$TEST_NAMESPACE" "$POD_NAME")"
  [[ -n "$u" ]] || note_fail "registry lost the create-response UID for pod/$POD_NAME (pin 8)"
  delete_owned pod "$TEST_NAMESPACE" "$POD_NAME" "$u" || note_fail "live pod delete failed (refusal/API error — never a success)"
  poll_raw "$MAX_POLLS" live_pod_gone || note_fail "live pod not gone within budget (typed absence required)"
  u="$(tracked_uid persistentvolumeclaim "$TEST_NAMESPACE" "$PVC_NAME")"
  [[ -n "$u" ]] || note_fail "registry lost the create-response UID for pvc/$PVC_NAME (pin 8)"
  delete_owned persistentvolumeclaim "$TEST_NAMESPACE" "$PVC_NAME" "$u" || note_fail "live PVC delete failed"
  if ! poll_raw "$((MAX_POLLS * 2))" live_pvva_gone; then
    for pair in "persistentvolume:$PV_NAME:$PV_UID" "volumeattachment:$VA_NAME:$VA_UID"; do
      k="${pair%%:*}"; rest="${pair#*:}"; n="${rest%%:*}"; uid="${rest##*:}"
      live="$(kube_get_json "$k" "$n" || true)"
      [[ -n "$live" ]] || continue
      luid="$(obj_uid <<<"$live")"
      if [[ "$luid" == "$uid" ]]; then
        delete_owned "$k" "" "$n" "$uid" || note_fail "stuck $k/$n cleanup failed (UID-preconditioned)"
      else
        note_fail "foreign replacement at $k/$n (live $luid != observed $uid) — preserved; usage cannot be cleared safely"
      fi
    done
    if ! poll_raw "$MAX_POLLS" live_pvva_gone; then
      write_remaining_evidence
      finish $RC_BLOCKED "RESULT=BLOCKED live usage cleanup incomplete within budget — gate cannot be tested on assumed absence"
    fi
  fi
  u="$(tracked_uid configmap "$TEST_NAMESPACE" "$MARKER_NAME")"
  [[ -n "$u" ]] || note_fail "registry lost marker UID (pin 8)"
  delete_owned configmap "$TEST_NAMESPACE" "$MARKER_NAME" "$u" || note_fail "live marker delete failed"
  ev "live usage cleared: pod+PVC deleted; driver completed PV/VA/backend cleanup (observed UID chain verified, typed absence)"
else
clear_kinds=(volumeattachment persistentvolumeclaim persistentvolume configmap)
clear_names=("$VA_NAME" "$PVC_NAME" "$PV_NAME" "$MARKER_NAME")
clear_nses=("" "$TEST_NAMESPACE" "" "$TEST_NAMESPACE")
for i in "${!clear_kinds[@]}"; do
  k="${clear_kinds[$i]}"; n="${clear_names[$i]}"; ns="${clear_nses[$i]}"
  u="$(tracked_uid "$k" "$ns" "$n")"
  [[ -n "$u" ]] || note_fail "registry lost the create-response UID for $k/$n — refusing to continue without clean ownership (pin 8)"
  delete_owned "$k" "$ns" "$n" "$u" || note_fail "clearing own usage failed for $k/$n (refusal rc 2 / API error rc 1 — a refusal is never a success) — refusing to continue without clean ownership (pin 8)"
done
ev "usage cleared: own PV/PVC/VA/marker removed under UID-ownership precondition (this run's registry only)"
fi

pred_reaped() { kube_absent quartet quartet; }  # typed NotFound only; API error keeps polling -> BLOCKED, never "reaped"
if ! poll_raw "$MAX_POLLS" pred_reaped; then
  write_remaining_evidence
  finish $RC_BLOCKED "RESULT=BLOCKED controlled recovery incomplete after usage cleared — finalizer still pending within $MAX_POLLS bounded polls (fail-safe: unknown/errored scan state stays blocked, apply.go:1247-1259, 1324-1328); evidence + remaining objects written"
fi
assert_owned_set_absent
assert_foreign_present "foreign canary Deployment" deployment "$CANARY_DEP_NAME" --namespace "$OPERAND_NS"
ev "controlled recovery verified: finalizer removed only after a CLEAN scan; CR GC'd; full runtime-owned set typed-absent; canary + installer RBAC untouched by the operator"

# Post-PASS boundary: the singleton is gone BY DESIGN (the mutation under
# test); re-provisioning it belongs to the installer/next lifecycle cell —
# this case never re-creates the canonical singleton source.
finish $RC_PASS "RESULT=PASS block-reason=$blocked_reason finalizer=$FINALIZER recovery=clean-scan-only cr=reaped canary=operator-untouched ownership=uid-registry verbs='get create patch delete'"
