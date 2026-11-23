#!/usr/bin/env bash
# =============================================================================
# Case body — singleton-rejection (offline scenario contract, NFS-applicable)
#
# Behavior under test (bound to source):
# 1. A different-named CR is API-ACCEPTED — the CRD imposes NO name
# enforcement (the only CEL rule is platform immutability), so create
# succeeds (api/v1alpha1/quartet_types.go:7-15, config/crd/bases/
# quartet.ngxstorage.com_quartets.yaml:301-304).
# 2. ...and is refused by the RECONCILIER: ValidateName -> InvalidSpec,
# TERMINAL (reported once, never requeued), the rejected CR records no
# specPlatform, keeps no profile claims, gets no finalizer, and NEVER
# manages operands (api/v1alpha1/validation.go:25-37,
# internal/controller/quartet_controller.go:109-114,318-328).
# 3. Parallel start: the true singleton `quartet` manages the operands and
# reaches Ready (observed by bounded poll of the real status); the
# foreign-named CR stays InvalidSpec beside it — single ownership
# (internal/controller/quartet_controller_test.go
# TestSingletonRaceSimulated).
# 4. Concurrent same-name create is API-atomic -> AlreadyExists
# (quartet_types.go:7-9).
#
# The foreign-named CR is API-created — it is this case's own canary and,
# like every object created here, a cleanup obligation registered with its
# create-response UID.
#
# Protocol (tests/scenario/CASE-CONTRACT.md "Protocol pins", 2026-11-12):
# - ONLY the contracted helper surface: scenario_preflight + scenario_kube,
# plus the pinned atomic cleanup helpers scenario_create_tracked /
# scenario_delete_owned (CASE-CONTRACT "Ortak atomik cleanup helper";
# real owner tests/scenario/lib/ — this body's fixtures carry the TEST
# STUB of the exact same contract when the loaded lib lacks them).
# This body references NO offline-stub-private variable. Helper
# source path resolves from this file's OWN BASH_SOURCE root: the live
# runner's `export -f` lib wins the declare -F probe; otherwise the
# SCENARIO_HELPERS override or this case's offline stub fixture.
# - The body's FIRST cluster call is a read-only live-adapter readiness
# probe: `get crd quartets.quartet.ngxstorage.com` (CRD name bound to
# config/crd/bases/quartet.ngxstorage.com_quartets.yaml:7). Unready
# adapter -> NOT RUN (3) BEFORE any resource exists. Refused
# surface -> BLOCKED (2).
# - Exit protocol: 0=PASS 1=FAIL 2=BLOCKED 3=SKIP/NOT RUN
# 4=FAIL_CLEANUP. Default outcome is FAIL; rc 0 only on an explicit green
# completion under a live grant with a ready adapter. Offline
# (no canonical live grant) ends NOT RUN even with every check green.
# - Ownership: every object THIS run creates is registered with the
# UID from its create response; an EXIT trap UID-precondition-deletes on
# EVERY exit path; cleanup failure escalates to FAIL_CLEANUP (4, never 5).
# The operator NEVER deletes namespaces (namespace ownership); a pre-existing operand
# namespace is adopted as-is and never re-stamped, so the ownership check
# honors that (apply.go:502-507).
# =============================================================================
set -u

case_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
case_name=${case_dir##*/}
# Machine policy: scratch only under $HOME/.cache/ngxstorage, never /tmp.
export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
mkdir -p "$TMPDIR"

SCRATCH="$TMPDIR/quartet-case/${case_name}-$$"
mkdir -p "$SCRATCH"

CHECKS=0; FAILS=0
CLEANUP_OK=1
MUTATED=0
CLEANUP_RAN=0
RESTORE_PENDING=0
LIVE=0
LIVE_ADAPTER=0
NS_CREATED=0
OPNS_PREEXISTED=0
FOREIGN_CREATED=0
RESTORE_FILE=""
# pin-8 cleanup registry: $EVIDENCE_DIR/ownership.tsv (8-column evidence log
# written by scenario_create_tracked / scenario_delete_owned — the pinned
# atomic cleanup helpers, CASE-CONTRACT "Ortak atomik cleanup helper").
IDENTITY_FILE="$SCRATCH/identity.tsv"
: > "$IDENTITY_FILE"

# --- helper surface (contracted; pin 3) ---------------------------------------
if ! declare -F scenario_preflight >/dev/null 2>&1; then
 # live runner injects scenario_preflight/scenario_kube via `export -f`;
 # only fall back to this case's offline stub when it was not provided.
  source "${SCENARIO_HELPERS:-${case_dir}/fixtures/scenario_helpers.sh}"
fi
if ! declare -F scenario_create_tracked >/dev/null 2>&1; then
 # Pinned atomic cleanup surface (CASE-CONTRACT.md "Ortak atomik cleanup
 # helper"): the real owner is tests/scenario/lib/; when the loaded surface
 # does not carry it yet, this case's fixture stub defines the exact pinned
 # names/returns so the offline contract validates the CALLS.
  source "${case_dir}/fixtures/scenario_atomic_cleanup.sh"
fi

# --- ownership registry + EXIT trap -----------------------------------
identity() { printf '%s\t%s\n' "$1" "$2" >> "$IDENTITY_FILE"; }

create_owned() { # <kind> <name> [--namespace <ns>] <manifest> — pin-8 create:
 # the shared helper binds THIS run's ownership to the create-response UID
 # (emitted on stdout, registered in ownership.tsv). rc 2 = gate refusal,
 # rc 1 = API error — neither adopts nor succeeds. Sets CREATE_UID on rc 0.
  local rc=0
  CREATE_UID="$(scenario_create_tracked "$@")" || rc=$?
  if [[ "$rc" == "2" ]]; then
    echo "setup blocked: create REFUSED by the scenario surface (rc 2 = refusal, never a success)" >&2
  elif [[ "$rc" == "1" ]]; then
    echo "setup blocked: create API-rejected (rc 1) — refusing to adopt a same-name object" >&2
  elif [[ -z "${CREATE_UID:-}" ]]; then
    echo "setup blocked: create returned rc 0 without a UID — cannot bind ownership (fail-closed)" >&2
    rc=2
  else
    MUTATED=1
  fi
  return "$rc"
}

obligation_done() { # <kind> <ns-or-dash> <name> <uid> — this run ALREADY
# deleted this obligation in-flow. The registry is evidence, not a gate:
# rows are never consumed, so the EXIT trap must not re-assert the captured
# UID against a name the operator may legitimately own afterwards.
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" >> "$SCRATCH/satisfied.tsv"
}

cleanup_owned() {
  local rc=0 reg action kind ns name uid outcome run_id ts r attempt nsarg satrow
  if [[ "$RESTORE_PENDING" == "1" ]]; then
    RESTORE_PENDING=0
    if [[ -n "$RESTORE_FILE" && -f "$RESTORE_FILE" ]]; then
      scenario_kube replace -f "$RESTORE_FILE" </dev/null || rc=1
    fi
  fi
  reg="$EVIDENCE_DIR/ownership.tsv"
  [[ -f "$reg" ]] || return "$rc"
 # LIFO: workloads/CRs before the namespace that holds them. Only rows the
 # pinned helper verified as `created` (uid bound from the create response)
 # are obligations. Every cleanup deletion goes through the shared UID-
 # preconditioned helper — a swapped UID is a refusal (rc 2), never a
 # delete. rc 2 is retried a bounded number of times: on a live cluster the
 # case-created CR's finalizer is reaped asynchronously by the operator, and
 # the helper's postcondition (NotFound) may only hold after that pass — a
 # genuinely replaced object refuses identically on every retry and is
 # never touched.
  while IFS=$'\t' read -r action kind ns name uid outcome run_id ts; do
    [[ "$action" == "CREATE" && "$outcome" == "created" && -n "${uid:-}" ]] || continue
    satrow="$(printf '%s\t%s\t%s\t%s' "$kind" "$ns" "$name" "$uid")"
    grep -qxF "$satrow" "$SCRATCH/satisfied.tsv" 2>/dev/null && continue
    r=0; attempt=0; nsarg=""
    [[ "$ns" != "-" ]] && nsarg="--namespace $ns"
    while :; do
      scenario_delete_owned "$kind" "$name" $nsarg "$uid" || r=$?
      [[ "$r" == "0" ]] && break
      if [[ "$r" != "2" ]] || (( attempt >= ${SCENARIO_CLEANUP_RETRIES:-3} )); then
        break
      fi
      attempt=$((attempt + 1))
      sleep "${SCENARIO_CLEANUP_INTERVAL:-2}"
    done
    if [[ "$r" != "0" ]]; then
      if [[ "$r" == "2" ]]; then
 # rc 2 means the helper refused (or could not verify). UIDs are
 # immutable: a DIFFERENT live UID at that name proves the object this
 # run created is gone — the replacement is NOT ours to adopt or
 # delete, so the obligation itself is met.
        live="$(scenario_kube get "$kind" "$name" $nsarg -o json </dev/null 2>/dev/null | jq -r '.metadata.uid // empty')"
        if [[ -n "$live" && "$live" != "$uid" ]]; then
          echo "cleanup: $kind/$name — captured UID $uid gone from that name (live $live); replacement preserved, obligation met" >&2
          continue
        fi
      fi
      echo "cleanup: $kind/$name not removed (rc=$r — $([[ "$r" == "2" ]] && echo 'ownership lost/unverified: replaced/foreign object preserved' || echo 'API error')); reported separately" >&2
      rc=1
    fi
  done < <(tac "$reg")
  return "$rc"
}

write_evidence() { # <exit-code>
  local code="$1" evdir rev ident
  evdir="${EVIDENCE_DIR:-$SCRATCH}"
  mkdir -p "$evdir"
  rev="${SOURCE_REV:-$(git -C "$case_dir" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
  ident=$(jq -Rn '[inputs | select(length > 0) | split("\t") | {(.[0]): (.[1] // "")}] | add // {}' "$IDENTITY_FILE")
  jq -n \
    --arg case "$case_name" \
    --arg run "${RUN_ID:-offline-direct}" \
    --argjson live "$LIVE" \
    --arg ctx "${KUBE_CONTEXT:-unset}" \
    --arg ns "${TEST_NAMESPACE:-unset}" \
    --arg rev "$rev" \
    --arg sha "${CASE_SHA:-}" \
    --argjson checks "$CHECKS" --argjson fails "$FAILS" \
    --argjson cleanup "$CLEANUP_OK" \
    --argjson code "$code" \
    --argjson identity "$ident" \
    '{case:$case, run_id:$run, live_grant:$live, target_context:$ctx,
      namespace:$ns, source_revision:$rev, case_sha:$sha,
      operand_identity:$identity, checks:$checks, failed:$fails,
      cleanup_ok:$cleanup, exit_code:$code}' > "$evdir/evidence.json"
 # Credential hygiene (contract "Ortak runtime alanları"): only UIDs/digests
 # ever reach evidence; the backend Secret is not written here at all.
}

on_exit() {
  local code=$? cleanup_label
 # Run the ownership cleanup exactly once, on EVERY exit path: both
 # CRs (the rejected foreign canary too — it was created by THIS run), the
 # backend Secret, and the TEST_NAMESPACE only if this run created it.
  if [[ "$MUTATED" == "1" && "$CLEANUP_RAN" != "1" ]]; then
    CLEANUP_RAN=1
    cleanup_owned || CLEANUP_OK=0
  fi
  if [[ "$CLEANUP_OK" == "0" && ( "$code" == "0" || "$code" == "3" ) ]]; then
    code=4   # FAIL_CLEANUP: a failed cleanup never becomes success (4, not 5)
  fi
  write_evidence "$code"
  cleanup_label="$([[ $CLEANUP_OK -eq 1 ]] && echo ok || echo failed)"
  case "$code" in
    0) echo "RESULT: PASS (live; checks=$CHECKS failed=$FAILS cleanup=$cleanup_label)" ;;
    1) echo "RESULT: FAIL (checks=$CHECKS failed=$FAILS cleanup=$cleanup_label)" ;;
    2) echo "RESULT: BLOCKED (cleanup=$cleanup_label)" ;;
    3) echo "RESULT: NOT RUN (no live result; checks=$CHECKS failed=$FAILS cleanup=$cleanup_label)" ;;
    4) echo "RESULT: FAIL_CLEANUP (cleanup failed; checks=$CHECKS failed=$FAILS)" ;;
    *) echo "RESULT: FAIL (unexpected shell exit $code — fail-closed, never PASS)" ;;
  esac
  exit "$code"
}
trap on_exit EXIT

# --- contract: the case body calls the shared preflight itself ---------------
if ! scenario_preflight "${case_dir}/case.json"; then
  exit 2
fi
# Only the canonical risk grant (ALLOW_LIVE=allow-*) is a live consent;
# anything else keeps this run offline (NOT RUN, never PASS).
[[ "${ALLOW_LIVE:-}" == allow-* ]] && LIVE=1

# --- live-adapter readiness probe: ZERO mutation before this ---------
CRD_NAME="quartets.quartet.ngxstorage.com"
probe_err="$SCRATCH/probe.err"
if crd_json=$(scenario_kube get crd "$CRD_NAME" -o json </dev/null 2>"$probe_err"); then
  LIVE_ADAPTER=1
  identity adapter_probe "ready"
else
  probe_rc=$?
  [[ -s "$probe_err" ]] && cat "$probe_err" >&2
  if [[ "$probe_rc" == "2" ]]; then
    echo "scenario surface refused the probe (offline fixture under a live grant, or gate drift)"
    identity adapter_probe "refused"
    exit 2
  fi
  echo "live adapter not ready: $CRD_NAME not found — NOT RUN before any resource is created"
  identity adapter_probe "absent"
  exit 3
fi

check() { # <label> <predicate...>
  CHECKS=$((CHECKS + 1))
  local label="$1"; shift
  if "$@"; then
    echo "CHECK OK: ${label}"
  else
    echo "CHECK FAIL: ${label}"
    FAILS=$((FAILS + 1))
  fi
}

# --- portable readers (parametrized by CR name) --------------------------------
# Quartet is CLUSTER-SCOPED (config/crd/bases/quartet.ngxstorage.com_quartets.yaml
# spec.scope: Cluster): the readers emit NO namespace for it (namespace belongs
# to genuinely namespaced objects only — Secret/PVC/Deployment keep theirs).
cr_json() { scenario_kube get quartet "$1" -o json </dev/null; }
cond_reason() { echo "$1" | jq -r --arg t "$3" '
  (.status.conditions // [])[]? | select(.type==$t) | "\(.status) \(.reason)"' | head -1; }
cond_message() { echo "$1" | jq -r --arg t "$3" '
  (.status.conditions // [])[]? | select(.type==$t) | .message' | head -1; }
wait_for() { # <seconds> <predicate...> — bounded poll (pin 4: real observation)
  local deadline=$(( $(date +%s) + $1 )) local interval="${SCENARIO_WAIT_INTERVAL:-2}"
  shift
  while :; do "$@" && return 0; (( $(date +%s) >= deadline )) && return 1; sleep "$interval"; done
}
obj_absent() { if scenario_kube get "$1" "$2" ${3:+-n "$3"} -o json </dev/null >/dev/null 2>&1; then return 1; else return 0; fi; }

NFS_NS="ngxstorage-nfs"      # operand namespace operand namespace (renderer/nfs.go:29)
SECRET_NAME="nfs-backend-secret"
FOREIGN="quartet-extra"
SINGLETON="quartet"

# Approved NFS driver image (digest pin: the spec carries the DRIVER image only):
# hack/golden-verify/main.go:36 (rawDriverDigest), pinned to the raw render
# inventory (the pinned raw-render notes).
DRIVER_IMAGE="quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:809528ed53c084730c1f645c6b8f4d8b04dfaac2a4ca3fbee81059606e39243b"

# --- setup -------------------------------------------------------------------
# TEST_NAMESPACE: adopted if the campaign already created it; only THIS run's
# creation becomes THIS run's cleanup obligation.
if scenario_kube get namespace "$TEST_NAMESPACE" -o json </dev/null >/dev/null 2>&1; then
  identity test_namespace_created 0
else
  printf '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"%s"}}' "$TEST_NAMESPACE" > "$SCRATCH/ns.json"
  create_owned namespace "$TEST_NAMESPACE" "$SCRATCH/ns.json" || exit 2
  NS_UID="$CREATE_UID"
  NS_CREATED=1
  identity test_namespace_created 1
  identity test_namespace_uid "$NS_UID"
fi

mkcr() { # <name> -> writes manifest to $SCRATCH/cr-<name>.json
 # Cluster-scoped manifest: NO metadata.namespace (the real CRD scope is
 # Cluster; a namespace field on a cluster object would be the inverse schema).
  jq -n --arg name "$1" --arg sec "$SECRET_NAME" --arg img "$DRIVER_IMAGE" \
    '{apiVersion:"quartet.ngxstorage.com/v1alpha1",kind:"Quartet",
      metadata:{name:$name},
      spec:{platform:"auto",logLevel:"info",
        profiles:{nfs:{enabled:true,secretRef:{name:$sec},image:{reference:$img}},
          fc:{enabled:false},iscsi:{enabled:false}}}}' > "$SCRATCH/cr-$1.json"
}

# --- phase 1: foreign-named CR is API-accepted, reconciler-refused ------------
mkcr "$FOREIGN"
create_owned quartet "$FOREIGN" "$SCRATCH/cr-$FOREIGN.json" || exit 2
FOREIGN_CREATED=1
FOREIGN_UID="$CREATE_UID"
identity foreign_cr_uid "$FOREIGN_UID"

# predicates (in case-shell scope so check() records their return code)
p_api_accepted() { [[ "$FOREIGN_CREATED" == "1" ]]; }
p_rej_reason()   { [[ "$(cond_reason "$1" agg Ready)" == "False InvalidSpec" ]]; }
# Message bound verbatim to validation.go:34 (ValidateName error text).
p_rej_message()  { cond_message "$1" agg Ready | grep -q "singleton violation: only a Quartet named \"quartet\" is reconciled, got \"${FOREIGN}\""; }
p_rej_nospecplat(){ [[ "$(echo "$1" | jq -r '.status.specPlatform // ""')" == "" ]]; }
p_rej_noprofiles(){ [[ "$(echo "$1" | jq -r '.status.profiles // null')" == "null" ]]; }
p_rej_nofinal()  { [[ "$(echo "$1" | jq -r '.metadata.finalizers // [] | length')" == "0" ]]; }

p_rejected() { local j; j=$(cr_json "$FOREIGN") || return 1; p_rej_reason "$j"; }
if wait_for "${SCENARIO_WAIT_SECONDS:-120}" p_rejected; then
  J=$(cr_json "$FOREIGN")
  check "foreign-named CR is API-accepted (create rc=0, no CRD name enforcement)" p_api_accepted
  check "aggregate Ready=False InvalidSpec (reconciler-side rejection)" p_rej_reason "$J"
  check "message carries the singleton-violation wording verbatim (validation.go:34)" p_rej_message "$J"
  check "rejected CR records no accepted specPlatform (failedStatus, controller:318-328)" p_rej_nospecplat "$J"
  check "rejected CR keeps no profile claims (profiles cleared, controller:323)" p_rej_noprofiles "$J"
  check "rejected CR gets no finalizer (validate precedes finalizer, controller:111 vs 131)" p_rej_nofinal "$J"
  check "never manages operands: operand namespace NOT created (namespace ownership only after validate)" \
    obj_absent namespace "$NFS_NS"
else
  J=$(cr_json "$FOREIGN" 2>/dev/null || echo '{}')
  echo "  observed aggregate Ready: $(cond_reason "$J" agg Ready) (msg: $(cond_message "$J" agg Ready))"
  check "foreign-named CR reconciler-refused with InvalidSpec" false
fi

# --- phase 2: parallel start -> single ownership ------------------------------
# Record whether the operand namespace predates this case: the operator ADOPTS
# an existing namespace as-is and never re-stamps it (namespace ownership, apply.go:502-507),
# so the ownership assertion must honor that.
obj_absent namespace "$NFS_NS" || OPNS_PREEXISTED=1

mkcr "$SINGLETON"
create_owned quartet "$SINGLETON" "$SCRATCH/cr-$SINGLETON.json" || exit 2
SINGLETON_UID="$CREATE_UID"
identity singleton_uid "$SINGLETON_UID"

# The backend Secret (all four renderer keys, nfs.go:476-479) is created in the
# OPERAND namespace after the operator's namespace ownership ensureNamespace ran — a real API
# rejects an object in a namespace that does not exist yet. Values are
# synthetic; only the UID reaches evidence.
mk_secret() {
  jq -n --arg n "$SECRET_NAME" --arg ns "$NFS_NS" \
    '{apiVersion:"v1",kind:"Secret",type:"Opaque",
      metadata:{name:$n,namespace:$ns},
      data:{"storage-ips":"MTI3LjAuMC4x","pool-name":"cXVhcnRldC1wb29s",
            "api-key":"cXVhcnRldC1zeW50aGV0aWMta2V5","data-ip":"MTI3LjAuMC4y"}}' > "$1"
}
p_operand_ns_ready() { scenario_kube get namespace "$NFS_NS" -o json </dev/null >/dev/null 2>&1; }
if wait_for "${SCENARIO_WAIT_SECONDS:-60}" p_operand_ns_ready; then
  mk_secret "$SCRATCH/secret.json"
  create_owned secret "$SECRET_NAME" --namespace "$NFS_NS" "$SCRATCH/secret.json" || exit 2
  SECRET_UID="$CREATE_UID"
  identity backend_secret_uid "$SECRET_UID"
else
  J=$(cr_json "$SINGLETON" 2>/dev/null || echo '{}')
  echo "  operand namespace never appeared: $(cond_reason "$J" agg Ready)"
  check "operator created the operand namespace before healing was possible (namespace ownership)" false
fi

p_singleton_ready() { local j; j=$(cr_json "$SINGLETON") || return 1
  [[ "$(cond_reason "$j" agg Ready)" == "True Ready" ]]; }
p_singleton_reason() { [[ "$(cond_reason "$1" agg Ready)" == "True Ready" ]]; }
p_foreign_still_rejected() { [[ "$(cond_reason "$1" agg Ready)" == "False InvalidSpec" ]]; }
p_ns_owned_by_singleton() {
  local a
  a=$(scenario_kube get namespace "$NFS_NS" -o json </dev/null 2>/dev/null | jq -r '.metadata.annotations["quartet.ngxstorage.com/owner-uid"] // ""')
  if [[ "$OPNS_PREEXISTED" == "1" ]]; then
 # Adopted as-is (apply.go:505-507): the stamp is either absent (never ours)
 # or this singleton's — never the rejected foreign CR's.
    [[ -z "$a" || "$a" == "$SINGLETON_UID" ]]
  else
    [[ "$a" == "$SINGLETON_UID" ]]
  fi
}
if wait_for "${SCENARIO_WAIT_SECONDS:-120}" p_singleton_ready; then
  J=$(cr_json "$SINGLETON"); F=$(cr_json "$FOREIGN")
  check "singleton owns the operands and reaches Ready (single ownership)" p_singleton_reason "$J"
  check "foreign CR beside the singleton is STILL InvalidSpec (no second owner)" p_foreign_still_rejected "$F"
  check "foreign CR STILL has no finalizer / never managed operands" p_rej_nofinal "$F"
  check "operand namespace is owned by the singleton UID, not the foreign CR" p_ns_owned_by_singleton
else
  J=$(cr_json "$SINGLETON" 2>/dev/null || echo '{}')
  echo "  observed singleton aggregate Ready: $(cond_reason "$J" agg Ready)"
  check "singleton reaches Ready under parallel start" false
fi

# --- phase 3: same-name concurrent create is API-atomic -----------------------
mkcr "$SINGLETON"
DUP_RC=0
scenario_kube create -f "$SCRATCH/cr-$SINGLETON.json" </dev/null 2>"$SCRATCH/dup.err" || DUP_RC=$?
check "second same-name create rejected with AlreadyExists (API-atomic, quartet_types.go:7-9)" \
  bash -c 'test "$1" -ne 0 && grep -q "AlreadyExists" "$2"' _ "$DUP_RC" "$SCRATCH/dup.err"

# --- verdict ------------------------------------------------------------------
# Cleanup itself runs in the EXIT trap and is reported separately; a
# failed cleanup escalates this run to FAIL_CLEANUP (4, not 5).
if (( FAILS > 0 )); then
  exit 1
fi
if (( LIVE == 0 )); then
 # Offline fixtures never produce a live result: NOT RUN, never PASS.
  exit 3
fi
exit 0
