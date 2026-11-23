#!/usr/bin/env bash
# =============================================================================
# Case body — foreign-resource-conflict (offline scenario contract, NFS-applicable)
#
# Behavior under test (adoption-or-nothing, bound to source):
# 1. Pre-existing FOREIGN operand (CSIDriver nfs.csi.ngxstorage.com) without
# this CR's owner-uid annotation -> ownership gate -> ForeignResourceConflict,
# TERMINAL (no requeue; waits for a spec/watch trigger). No adoption, no
# force, no modification: the foreign object stays byte-identical (UID,
# labels, spec, no ownership annotation gained).
# internal/controller/apply.go:831-900 (ownershipGate/foreignConflict),
# apply.go:140-147 (terminal classification), apply.go:897 (message),
# proven by internal/controller/operand_test.go:513-545 +
# operand_envtest_test.go:503-560.
# 2. Impact bound: apply stops at the FIRST failure (CSIDriver) — earlier
# owned objects (SA/ConfigMap/leases) remain, later ones (Service/
# Deployment/DaemonSet) are never created. apply.go:114-118.
# 3. Installer-provisioned RBAC: the manager holds ONLY get/list/watch and
# never creates/patches/deletes RBAC (quartet_controller.go:47-57). A
# drifted/foreign pinned ClusterRole -> verifyOperandRBAC -> RBACDrift
# (retryable), the operator is NOT self-repaired, and the message binds
# verbatim to apply.go:242. apply.go:221-244.
#
# The planted foreign CSIDriver and the tampered ClusterRole state are THIS
# case's own canaries: pin 8 makes them cleanup obligations — the canary is
# deleted by its create-response UID, and the tamper is RESTORED (not deleted:
# installer-owned) from the captured original on every exit path.
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
# UID from its create response; an EXIT trap restores + UID-precondition-
# deletes on EVERY exit path (including early errors AFTER the foreign-
# RBAC mutation); cleanup failure escalates to FAIL_CLEANUP (4, never 5).
# Real foreign objects are preserved: the UID precondition refuses to
# delete anything that is no longer the captured object.
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
 # ever reach evidence; the backend Secret is not written here at all.
}

on_exit() {
  local code=$? cleanup_label
 # Run the ownership cleanup exactly once, on EVERY exit path —
 # including an early error after the foreign-RBAC mutation: the tamper is
 # restored from the captured original before anything else is deleted.
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

# --- portable readers ---------------------------------------------------------
cr_json() { scenario_kube get quartet "quartet" -o json </dev/null; }
cond_reason() { echo "$1" | jq -r --arg t "$3" --arg s "$2" '
  (if $s=="agg" then .status.conditions else .status.profiles.nfs.conditions end)[]?
  | select(.type==$t) | "\(.status) \(.reason)"' | head -1; }
cond_message() { echo "$1" | jq -r --arg t "$3" --arg s "$2" '
  (if $s=="agg" then .status.conditions else .status.profiles.nfs.conditions end)[]?
  | select(.type==$t) | .message' | head -1; }
wait_for() { # <seconds> <predicate...> — bounded poll (pin 4: real observation)
  local deadline=$(( $(date +%s) + $1 )) local interval="${SCENARIO_WAIT_INTERVAL:-2}"
  shift
  while :; do "$@" && return 0; (( $(date +%s) >= deadline )) && return 1; sleep "$interval"; done
}
obj_digest() { printf '%s' "$1" | jq -S . | sha256sum | cut -d' ' -f1; }
# Normalized digest for cross-write comparisons (content as $1): volatile
# server-managed fields (resourceVersion/managedFields/creationTimestamp) are
# not identity.
obj_norm_digest() { printf '%s' "$1" | jq -S 'del(.metadata.resourceVersion, .metadata.managedFields, .metadata.creationTimestamp)' | sha256sum | cut -d' ' -f1; }
# Same normalization, reading a file.
file_norm_digest() { jq -S 'del(.metadata.resourceVersion, .metadata.managedFields, .metadata.creationTimestamp)' "$1" 2>/dev/null | sha256sum | cut -d' ' -f1; }
obj_exists() { scenario_kube get "$1" "$2" ${3:+-n "$3"} -o json </dev/null >/dev/null 2>&1; }
owner_uid_of() { scenario_kube get "$1" "$2" ${3:+-n "$3"} -o json </dev/null 2>/dev/null | jq -r '.metadata.annotations["quartet.ngxstorage.com/owner-uid"] // ""'; }

NFS_NS="ngxstorage-nfs"          # operand namespace operand namespace (renderer/nfs.go:29)
CSIDRIVER="nfs.csi.ngxstorage.com"  # renderer/nfs.go:35
SVC="nfs-csi-ngxstorage-controller-health"    # renderer/nfs.go:445
CLUSTERROLE="nfs-csi-ngxstorage-controller"   # renderer/nfs.go:334
SECRET_NAME="nfs-backend-secret"

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

# --- phase 1: foreign CSIDriver -> ForeignResourceConflict (terminal) ---------
# Seed the foreign object BEFORE the singleton CR exists: once the CR is in
# place a settle would let the operator own the name first (the envtest does
# the same, internal/controller/operand_envtest_test.go:505-515). This planted
# object is the case's own canary: cleanup deletes it by its UID only.
jq -n --arg n "$CSIDRIVER" '{apiVersion:"storage.k8s.io/v1",kind:"CSIDriver",
  metadata:{name:$n,labels:{"managed-by":"user","foreign-marker":"phase1"}},
  spec:{podInfoOnMount:true}}' > "$SCRATCH/foreign.json"
create_owned csidriver "$CSIDRIVER" "$SCRATCH/foreign.json" || exit 2
CANARY_UID="$CREATE_UID"
identity foreign_csidriver_uid "$CANARY_UID"

# Cluster-scoped manifest: NO metadata.namespace (real CRD spec.scope: Cluster).
jq -n --arg sec "$SECRET_NAME" --arg img "$DRIVER_IMAGE" \
  '{apiVersion:"quartet.ngxstorage.com/v1alpha1",kind:"Quartet",
    metadata:{name:"quartet"},
    spec:{platform:"auto",logLevel:"info",
      profiles:{nfs:{enabled:true,secretRef:{name:$sec},image:{reference:$img}},
        fc:{enabled:false},iscsi:{enabled:false}}}}' > "$SCRATCH/cr.json"
create_owned quartet "quartet" "$SCRATCH/cr.json" || exit 2
CR_UID="$CREATE_UID"
identity quartet_uid "$CR_UID"

FUID=$(scenario_kube get csidriver "$CSIDRIVER" -o json </dev/null | jq -r '.metadata.uid')
FDIGEST=$(obj_digest "$(scenario_kube get csidriver "$CSIDRIVER" -o json </dev/null)")

# predicates
p_frc_state() { local j; j=$(cr_json) || return 1
  [[ "$(cond_reason "$j" agg Ready)" == "False ForeignResourceConflict" ]] &&
  [[ "$(cond_reason "$j" profile Ready)" == "False ForeignResourceConflict" ]]; }
p_degraded_frc() { [[ "$(cond_reason "$1" agg Degraded)" == "True ForeignResourceConflict" ]]; }
p_finalizer() { echo "$1" | jq -e '.metadata.finalizers | index("quartet.ngxstorage.com/quartet")' >/dev/null; }
p_adoption_msg() { cond_message "$1" profile Ready | grep -q "adoption is not attempted"; }
p_fuid_kept() { [[ "$(scenario_kube get csidriver "$CSIDRIVER" -o json </dev/null | jq -r '.metadata.uid')" == "$FUID" ]]; }
p_fdigest_kept() { [[ "$(obj_digest "$(scenario_kube get csidriver "$CSIDRIVER" -o json </dev/null)")" == "$FDIGEST" ]]; }
p_fno_owner_ann() { [[ -z "$(owner_uid_of csidriver "$CSIDRIVER")" ]]; }
p_no_later_objs() { ! obj_exists service "$SVC" "$NFS_NS" && ! obj_exists deployment nfs-csi-ngxstorage-controller "$NFS_NS" && ! obj_exists daemonset nfs-csi-ngxstorage-node "$NFS_NS"; }
p_partial_apply() { [[ "$(owner_uid_of serviceaccount nfs-csi-ngxstorage-controller "$NFS_NS")" == "$CR_UID" ]] && [[ "$(owner_uid_of configmap nfs-csi-ngxstorage-coordination-config "$NFS_NS")" == "$CR_UID" ]]; }

if wait_for "${SCENARIO_WAIT_SECONDS:-120}" p_frc_state; then
  J=$(cr_json)
  check "aggregate Ready=False ForeignResourceConflict" p_frc_state
  check "Degraded=True ForeignResourceConflict (conflict explicit)" p_degraded_frc "$J"
  check "message carries 'adoption is not attempted' verbatim (apply.go:897)" p_adoption_msg "$J"
  check "cleanup finalizer added BEFORE operand mutation (customer data untouched, controller:129-137)" p_finalizer "$J"
  check "foreign CSIDriver UID preserved (not replaced/adopted)" p_fuid_kept
  check "foreign CSIDriver digest preserved (labels/spec untouched)" p_fdigest_kept
  check "foreign CSIDriver did NOT gain the ownership annotation" p_fno_owner_ann
  check "impact bound: Service/Deployment/DaemonSet never created (stopped at CSIDriver)" p_no_later_objs
  check "partial apply recorded, not rolled back: SA+ConfigMap owned (apply.go:114-118)" p_partial_apply
else
  J=$(cr_json 2>/dev/null || echo '{}')
  echo "  observed aggregate Ready: $(cond_reason "$J" agg Ready) (msg: $(cond_message "$J" agg Ready))"
  check "foreign CSIDriver -> terminal ForeignResourceConflict" false
fi

# Remove the case's own canary (by its create-response UID) -> the terminal
# conflict clears on the watch trigger. On success the case flow has fulfilled
# this cleanup obligation; the trap no longer re-asserts the canary UID (the
# operator legitimately owns the CSIDriver name from that point on).
rc=0
scenario_delete_owned csidriver "$CSIDRIVER" "$CANARY_UID" || rc=$?
if [[ "$rc" == "2" ]]; then
 # The helper's postcondition can read the name the OPERATOR re-adopted on
 # this very watch trigger (removing the canary is what clears the terminal
 # conflict). The UID precondition could only have removed the canary
 # itself, so a different live UID proves the canary is gone — the helper's
 # refusal is a verification artifact, not a surviving canary.
  live_uid="$(scenario_kube get csidriver "$CSIDRIVER" -o json </dev/null 2>/dev/null | jq -r '.metadata.uid // empty')"
  [[ -n "$live_uid" && "$live_uid" != "$CANARY_UID" ]] && rc=0
fi
if [[ "$rc" == "0" ]]; then
 # The case flow fulfilled this obligation; the EXIT trap must not re-assert
 # the canary UID against the name the operator adopts from here on.
  obligation_done csidriver "-" "$CSIDRIVER" "$CANARY_UID"
else
  echo "canary removal failed (rc=$rc — $([[ "$rc" == "2" ]] && echo 'ownership lost/unverified: object preserved' || echo 'API error'); retried by the cleanup trap)"
fi

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
  J=$(cr_json 2>/dev/null || echo '{}')
  echo "  operand namespace never appeared: $(cond_reason "$J" agg Ready)"
  check "operator created the operand namespace before healing was possible (namespace ownership)" false
fi

p_ready() { local j; j=$(cr_json) || return 1; [[ "$(cond_reason "$j" agg Ready)" == "True Ready" ]]; }
if wait_for "${SCENARIO_WAIT_SECONDS:-120}" p_ready; then
  check "terminal conflict clears when the foreign object is removed (watch trigger)" true
else
  check "terminal conflict clears after foreign removal" false
fi

# --- phase 2: installer-provisioned RBAC drift -> RBACDrift (read-only) -------
# The tampered ClusterRole is a case canary mutation: the captured original is
# the restore obligation on EVERY exit path, never a deletion (the
# installer owns it).
CR_ROLE_ORIG="$SCRATCH/clusterrole-orig.json"
if ! scenario_kube get clusterrole "$CLUSTERROLE" -o json </dev/null > "$CR_ROLE_ORIG"; then
  echo "setup blocked: installer-provisioned ClusterRole absent (precondition not met)"
  exit 2
fi
CR_UID_RB=$(jq -r '.metadata.uid' "$CR_ROLE_ORIG")
CR_ROLE_ORIG_DIGEST=$(file_norm_digest "$CR_ROLE_ORIG")
jq 'del(.metadata.resourceVersion)' "$CR_ROLE_ORIG" > "$SCRATCH/clusterrole-restore.json"
RESTORE_FILE="$SCRATCH/clusterrole-restore.json"
RESTORE_PENDING=1
MUTATED=1
identity tampered_clusterrole_uid "$CR_UID_RB"

SVC_BEFORE=$(obj_digest "$(scenario_kube get service "$SVC" -n "$NFS_NS" -o json </dev/null 2>/dev/null)")
jq '.rules += [{apiGroups:[""],resources:["secrets"],verbs:["get"]}] | .metadata.labels += {"managed-by":"attacker"} | del(.metadata.resourceVersion)' \
  "$CR_ROLE_ORIG" > "$SCRATCH/clusterrole-tampered.json"
scenario_kube replace -f "$SCRATCH/clusterrole-tampered.json" </dev/null || exit 2
CR_TAMPERED=$(obj_digest "$(scenario_kube get clusterrole "$CLUSTERROLE" -o json </dev/null)")

p_rbd() { local j; j=$(cr_json) || return 1
  [[ "$(cond_reason "$j" profile Ready)" == "False RBACDrift" ]]; }
# Wording bound VERBATIM to apply.go:242 (verifyOperandRBAC drift message).
p_rbd_msg() { local m; m=$(cond_message "$1" profile Ready)
  [[ "$m" == *"re-apply the install manifest config/operands/nfs/"* && "$m" == *"NOT created or modified"* ]]; }
p_degraded_rbd() { [[ "$(cond_reason "$1" agg Degraded)" == "True RBACDrift" ]]; }
p_crb_uid_kept() { [[ "$(scenario_kube get clusterrole "$CLUSTERROLE" -o json </dev/null | jq -r '.metadata.uid')" == "$CR_UID_RB" ]]; }
p_crb_digest_kept() { [[ "$(obj_digest "$(scenario_kube get clusterrole "$CLUSTERROLE" -o json </dev/null)")" == "$CR_TAMPERED" ]]; }
p_svc_frozen() { [[ "$(obj_digest "$(scenario_kube get service "$SVC" -n "$NFS_NS" -o json </dev/null 2>/dev/null)")" == "$SVC_BEFORE" ]]; }

if wait_for "${SCENARIO_WAIT_SECONDS:-120}" p_rbd; then
  J=$(cr_json)
  check "drifted RBAC -> profile Ready=False RBACDrift (apply.go:221-244)" p_rbd
  check "aggregate Degraded=True RBACDrift (impact explicit)" p_degraded_rbd "$J"
  check "message instructs re-apply config/operands/nfs/ + 'NOT created or modified' (apply.go:242)" p_rbd_msg "$J"
  check "foreign ClusterRole UID preserved (operator never writes RBAC)" p_crb_uid_kept
  check "foreign ClusterRole digest preserved (NOT self-repaired)" p_crb_digest_kept
  check "owned operand frozen while gated (Service unchanged)" p_svc_frozen
else
  J=$(cr_json 2>/dev/null || echo '{}')
  echo "  observed profile nfs Ready: $(cond_reason "$J" profile Ready) (msg: $(cond_message "$J" profile Ready))"
  check "drifted RBAC -> RBACDrift gate" false
fi

# --- phase 3: admin restores the manifest -> gate reopens ---------------------
# The restore IS the admin action the RBACDrift message instructs; doing it is
# how this case clears its own canary mutation.
p_restore_verified() { [[ "$(obj_norm_digest "$(scenario_kube get clusterrole "$CLUSTERROLE" -o json </dev/null 2>/dev/null)")" == "$CR_ROLE_ORIG_DIGEST" ]]; }
if scenario_kube replace -f "$RESTORE_FILE" </dev/null && wait_for "${SCENARIO_WAIT_SECONDS:-30}" p_restore_verified; then
  RESTORE_PENDING=0   # trap need not re-restore an already-verified manifest
  check "manifest restored byte-equivalent (volatile fields aside)" true
else
  check "manifest restored byte-equivalent (volatile fields aside)" false
fi
if wait_for "${SCENARIO_WAIT_SECONDS:-120}" p_ready; then
  check "gate reopens to Ready once the manifest is re-applied" true
else
  check "gate reopens after manifest restore" false
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
