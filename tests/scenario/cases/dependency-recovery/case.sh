#!/usr/bin/env bash
# =============================================================================
# Case body — dependency-recovery (offline scenario contract, NFS-applicable)
#
# Behavior under test (bound to source; see EXPECTATIONS below):
# 1. Operand namespace absent -> the operator CREATES it (namespace ownership adopted
# default; it never deletes namespaces): ensureNamespace,
# internal/controller/apply.go:502-526.
# 2. Backend Secret/key missing -> SAFE STATE + OPEN ERROR, and the reason
# is WorkloadNotReady carrying the kubelet waiting reason AS-IS
# (CreateContainerConfigError) with the explicit "does not read Secrets"
# disclosure. Secret API untouched(a): the operator never touches the Secret API, so the
# MissingSecret/MissingSecretKey split is UNPROVEN and must NOT appear —
# api/v1alpha1/validation.go:45-50, internal/controller/apply.go:416-420,
# pinned by internal/controller/reconciliation_lifecycle_test.go:270-286.
# The failure is RETRYABLE (30s requeue):
# internal/controller/quartet_controller.go:45,172-179.
# 3. AUTO-HEAL: once the referenced Secret carries the keys the renderer's
# secretKeyRef env block actually resolves (storage-ips/pool-name/
# api-key/data-ip — internal/renderer/nfs.go:476-479, env wiring
# nfs.go:565), the pod watch (the operator has no Secret watch — Secret API untouched(a))
# drives a reconcile whose probeWorkloadReadiness passes and the profile
# goes Ready with the explicit "not a backend I/O proof" wording —
# internal/controller/apply.go:433, quartet_controller.go:297-302.
# Recovery is observed by BOUNDED POLL of the real status subresource
# (observedGeneration + conditions), never by a synthetic reconcile RPC.
#
# Protocol (tests/scenario/CASE-CONTRACT.md "Protocol pins", 2026-11-12):
# - ONLY the contracted helper surface: scenario_preflight + scenario_kube,
# plus the pinned atomic cleanup helpers scenario_create_tracked /
# scenario_delete_owned (CASE-CONTRACT "Ortak atomik cleanup helper";
# real owner tests/scenario/lib/ — this body's fixtures carry the TEST
# STUB of the exact same names/returns when the loaded lib lacks them).
# This body references NO offline-stub-private variable. Helper
# source path resolves from this file's OWN BASH_SOURCE root: the live
# runner's `export -f` lib wins the declare -F probe; otherwise the
# SCENARIO_HELPERS override or this case's offline stub fixture.
# - The body's FIRST cluster call is a read-only live-adapter readiness
# probe: `get crd quartets.quartet.ngxstorage.com` (CRD name bound to
# config/crd/bases/quartet.ngxstorage.com_quartets.yaml:7). Unready
# adapter -> NOT RUN (3) BEFORE any resource exists. Refused
# surface (offline fixture under a live grant, gate drift) -> BLOCKED (2).
# - Exit protocol: 0=PASS 1=FAIL 2=BLOCKED 3=SKIP/NOT RUN
# 4=FAIL_CLEANUP. Default outcome is FAIL; rc 0 only on an explicit green
# completion under a live grant with a ready adapter. Offline
# (no canonical live grant) ends NOT RUN even with every check green.
# - Ownership: every object THIS run creates is registered with the
# UID from its create response; an EXIT trap restores + UID-precondition-
# deletes on EVERY exit path; cleanup failure escalates to FAIL_CLEANUP
# (4, never 5). The customer backend Secret is secretRef-only-untouchable by the
# operator; THIS case's setup Secret is the case's own artifact and IS
# cleaned by UID.
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
RESTORE_FILE=""
# pin-8 cleanup registry: $EVIDENCE_DIR/ownership.tsv (kind \t ns \t name \t uid),
# written by scenario_create_tracked, consumed by scenario_delete_owned.
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
 # delete, so the obligation itself is met (e.g. the operator re-
 # adopting a name this run legitimately cleared in-flow).
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
 # ever reach evidence; backend Secret VALUES are never written here.
}

on_exit() {
  local code=$? cleanup_label
 # Run the ownership cleanup exactly once, on EVERY exit path.
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
# The operator must be installed (case.json precondition: CRD + manager).
# Read-only check of the CRD name bound to config/crd/bases/
# quartet.ngxstorage.com_quartets.yaml:7.
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

# --- portable readers (scenario_kube is the only cluster surface) ------------
cr_json() { # <cr-name> — Quartet is cluster-scoped: NO -n (real CRD spec.scope)
  scenario_kube get quartet "$1" -o json </dev/null
}
cond_reason() { # <json> <scope: agg|profile> <ConditionType> -> "STATUS REASON"
  echo "$1" | jq -r --arg t "$3" --arg s "$2" '
    (if $s=="agg" then .status.conditions else .status.profiles.nfs.conditions end)[]?
    | select(.type==$t) | "\(.status) \(.reason)"' | head -1
}
cond_message() { # <json> <scope> <ConditionType>
  echo "$1" | jq -r --arg t "$3" --arg s "$2" '
    (if $s=="agg" then .status.conditions else .status.profiles.nfs.conditions end)[]?
    | select(.type==$t) | .message' | head -1
}
wait_for() { # <seconds> <predicate...> — bounded poll (pin 4: real observation)
  local deadline=$(( $(date +%s) + $1 )); local interval="${SCENARIO_WAIT_INTERVAL:-2}"
  shift
  while :; do
    "$@" && return 0
    (( $(date +%s) >= deadline )) && return 1
    sleep "$interval"
  done
}
obj_digest() { printf '%s' "$1" | jq -S . | sha256sum | cut -d' ' -f1; }

# Approved NFS driver image (digest pin: the spec carries the DRIVER image only):
# hack/golden-verify/main.go:36 (rawDriverDigest), pinned to the raw render
# inventory (the pinned raw-render notes). Sidecar images
# are renderer constants (internal/renderer/nfs.go:36-46) — they never enter
# the spec, so nothing invented goes into the manifest.
DRIVER_IMAGE="quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:809528ed53c084730c1f645c6b8f4d8b04dfaac2a4ca3fbee81059606e39243b"

NFS_NS="ngxstorage-nfs"        # operand namespace operand namespace (renderer/nfs.go:29)
SECRET_NAME="nfs-backend-secret"
SINGLETON="quartet"
CR_UID=""
PHASE1_REASON=""
SECRET_UID=""
SECRET_DIGEST=""

# --- setup -------------------------------------------------------------------
# TEST_NAMESPACE: adopted if the campaign already created it; only THIS run's
# creation becomes THIS run's cleanup obligation (pin 8 — never delete a
# namespace we did not create).
if scenario_kube get namespace "$TEST_NAMESPACE" -o json </dev/null >/dev/null 2>&1; then
  identity test_namespace_created 0
else
  printf '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"%s"}}' \
    "$TEST_NAMESPACE" > "$SCRATCH/test-ns.json"
  create_owned namespace "$TEST_NAMESPACE" "$SCRATCH/test-ns.json" || exit 2
  NS_UID="$CREATE_UID"
  NS_CREATED=1
  identity test_namespace_created 1
  identity test_namespace_uid "$NS_UID"
fi

# Singleton CR: nfs enabled, secretRef points at a Secret that does NOT exist
# yet (the dependency-recovery fault fault). Image is the approved digest reference above.
# Cluster-scoped manifest: NO metadata.namespace (real CRD spec.scope: Cluster).
jq -n --arg sec "$SECRET_NAME" --arg img "$DRIVER_IMAGE" \
  '{apiVersion:"quartet.ngxstorage.com/v1alpha1",kind:"Quartet",
    metadata:{name:"quartet"},
    spec:{platform:"auto",logLevel:"info",
      profiles:{
        nfs:{enabled:true,secretRef:{name:$sec},image:{reference:$img}},
        fc:{enabled:false},iscsi:{enabled:false}}}}' > "$SCRATCH/cr.json"
create_owned quartet "$SINGLETON" "$SCRATCH/cr.json" || exit 2
CR_UID="$CREATE_UID"
identity quartet_uid "$CR_UID"

# --- phase 1: safe state + open error, retryable -----------------------------
phase1_holds() {
  local j; j=$(cr_json "$SINGLETON") || return 1
  [[ "$(cond_reason "$j" agg Ready)" == "False WorkloadNotReady" ]] || return 1
  [[ "$(cond_reason "$j" agg Degraded)" == "True WorkloadNotReady" ]] || return 1
  [[ "$(cond_reason "$j" profile Ready)" == "False WorkloadNotReady" ]] || return 1
  PHASE1_REASON=$(cond_message "$j" profile Ready)
  echo "$PHASE1_REASON" | grep -q "CreateContainerConfigError" &&
  echo "$PHASE1_REASON" | grep -q "does not read Secrets"
}
# rc-based predicates (check records their return code, not their output)
p_agg_wnr()    { [[ "$(cond_reason "$1" agg Ready)" == "False WorkloadNotReady" ]]; }
p_degraded()   { [[ "$(cond_reason "$1" agg Degraded)" == "True WorkloadNotReady" ]]; }
p_profile_wnr(){ [[ "$(cond_reason "$1" profile Ready)" == "False WorkloadNotReady" ]]; }
p_d29a_msg()   { [[ "$PHASE1_REASON" == *"CreateContainerConfigError"* && "$PHASE1_REASON" == *"does not read Secrets"* ]]; }
p_no_missing_reasons() { [[ "$1" != *'"MissingSecret"*' && "$1" != *'"MissingSecretKey"*' ]]; }
p_observed()   { [[ "$(echo "$1" | jq -r '.status.observedGeneration')" == "1" && "$(echo "$1" | jq -r '.status.specPlatform')" == "auto" ]]; }
p_ns_exists()  { [[ -n "$(scenario_kube get namespace "$NFS_NS" -o json </dev/null 2>/dev/null)" ]]; }
p_ns_stamp()   { local a; a=$(scenario_kube get namespace "$NFS_NS" -o json </dev/null 2>/dev/null | jq -r '.metadata.annotations["quartet.ngxstorage.com/owner-uid"] // ""'); [[ "$a" == "$CR_UID" || -z "$a" ]]; }

if wait_for "${SCENARIO_WAIT_SECONDS:-120}" phase1_holds; then
  J=$(cr_json "$SINGLETON")
  check "aggregate Ready=False WorkloadNotReady with the kubelet reason AS-IS" p_agg_wnr "$J"
  check "Degraded=True (open error surfaced)" p_degraded "$J"
  check "profile nfs Ready=False WorkloadNotReady" p_profile_wnr "$J"
  check "message carries CreateContainerConfigError + 'does not read Secrets' (Secret API untouched)" p_d29a_msg
 # Secret API untouched discipline: the unproven MissingSecret/MissingSecretKey split must
 # NOT appear anywhere in the status (validation.go:45-50).
  check "no MissingSecret/MissingSecretKey reason promised (Secret API untouched)" p_no_missing_reasons "$J"
  check "observedGeneration advanced + specPlatform recorded (auto)" p_observed "$J"
 # namespace ownership: operand namespace auto-created with the owner-uid stamp (apply.go:514-515).
  check "operand namespace ngxstorage-nfs exists (namespace ownership create-if-absent)" p_ns_exists
  check "operand namespace carries the CR's owner-uid stamp when operator-created" p_ns_stamp
else
  J=$(cr_json "$SINGLETON" 2>/dev/null || echo '{}')
  echo "  observed aggregate Ready: $(cond_reason "$J" agg Ready) (msg: $(cond_message "$J" agg Ready))"
  check "phase 1 converged to WorkloadNotReady open error" false
fi

# --- phase 2: auto-heal when the dependency is fixed --------------------------
# The renderer's secretKeyRef env block (nfs.go:476-479) resolves FOUR keys;
# the kubelet refuses the container until every referenced key resolves, so
# the setup Secret carries exactly that key set. Values are SYNTHETIC: the
# Ready bar is itself "not a backend I/O proof" (controller:297-302), and
# credentials never enter evidence (only the UID below).
# The Secret is created in the operand namespace AFTER the operator's namespace ownership
# ensureNamespace ran (bounded poll for the namespace), because a real API
# rejects an object in a namespace that does not exist yet.
mk_secret() { # <file>
  jq -n --arg n "$SECRET_NAME" --arg ns "$NFS_NS" \
    '{apiVersion:"v1",kind:"Secret",type:"Opaque",
      metadata:{name:$n,namespace:$ns},
      data:{"storage-ips":"MTI3LjAuMC4x","pool-name":"cXVhcnRldC1wb29s",
            "api-key":"cXVhcnRldC1zeW50aGV0aWMta2V5","data-ip":"MTI3LjAuMC4y"}}' > "$1"
}
p_operand_ns_ready() { scenario_kube get namespace "$NFS_NS" -o json </dev/null >/dev/null 2>&1; }
if ! wait_for "${SCENARIO_WAIT_SECONDS:-60}" p_operand_ns_ready; then
  J=$(cr_json "$SINGLETON" 2>/dev/null || echo '{}')
  echo "  operand namespace never appeared: $(cond_reason "$J" agg Ready)"
  check "operator created the operand namespace before healing was possible (namespace ownership)" false
fi

mk_secret "$SCRATCH/secret.json"
create_owned secret "$SECRET_NAME" --namespace "$NFS_NS" "$SCRATCH/secret.json" || exit 2
SECRET_UID="$CREATE_UID"
identity backend_secret_uid "$SECRET_UID"
SECRET_DIGEST=$(obj_digest "$(scenario_kube get secret "$SECRET_NAME" -n "$NFS_NS" -o json </dev/null)")

phase2_holds() {
  local j; j=$(cr_json "$SINGLETON") || return 1
  [[ "$(cond_reason "$j" agg Ready)" == "True Ready" ]] &&
  [[ "$(cond_reason "$j" agg Degraded)" == "False Ready" ]] &&
  cond_message "$j" agg Ready | grep -q "not a backend I/O proof"
}
p_ready_agg()  { [[ "$(cond_reason "$1" agg Ready)" == "True Ready" ]]; }
p_degraded_off(){ [[ "$(cond_reason "$1" agg Degraded)" == "False Ready" ]]; }
p_ready_bar()  { cond_message "$1" agg Ready | grep -q "not a backend I/O proof"; }
p_profile_ready(){ [[ "$(cond_reason "$1" profile Ready)" == "True Ready" ]]; }
p_secret_untouched() {
  [[ "$(obj_digest "$(scenario_kube get secret "$SECRET_NAME" -n "$NFS_NS" -o json </dev/null 2>/dev/null)")" == "$SECRET_DIGEST" ]]
}
if wait_for "${SCENARIO_WAIT_SECONDS:-120}" phase2_holds; then
  J=$(cr_json "$SINGLETON")
  check "healed: aggregate Ready=True reason=Ready after the full-key Secret exists" p_ready_agg "$J"
  check "Degraded cleared to False" p_degraded_off "$J"
  check "Ready message keeps the 'not a backend I/O proof' bar (the readiness bar)" p_ready_bar "$J"
  check "profile nfs Ready=True" p_profile_ready "$J"
 # Customer Secret is never mutated by the operator (secretRef-only): byte-identical.
  check "backend Secret untouched by the operator (secretRef-only)" p_secret_untouched
else
  J=$(cr_json "$SINGLETON" 2>/dev/null || echo '{}')
  echo "  observed aggregate Ready: $(cond_reason "$J" agg Ready) (msg: $(cond_message "$J" agg Ready))"
  check "auto-heal converged to Ready after dependency fix" false
fi

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
