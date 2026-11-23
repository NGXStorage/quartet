#!/usr/bin/env bash
# =============================================================================
# Offline contract — dependency-recovery
#
# What this proves (hermetically, against THIS case's stub fixtures only):
# the correct-stub case body runs to completion against the stub and reports
# NOT RUN (never PASS — no live result was produced), with every
# source-bound check green, and its EXIT-trap cleanup removed what
# the run created (no leftover objects in the run's fake store);
# the snapshot-controller-absence case-controller-absence cell rides the probe the source
# says it does (snapshotter sidecar unavailable -> ImagePullBackOff ->
# WorkloadNotReady "ImagePullFailed", apply.go:421-423; heal on clear);
# sabotage: stub reports an unproven reason: the stub emits the unproven MissingSecret reason -> the
# case FAILs (Secret API untouched discipline bites: validation.go:45-50);
# sabotage: stub never heals: the stub never heals after the Secret exists -> the
# case FAILs (auto-heal is asserted, not assumed);
# T5 preflight rejects a missing KUBE_CONTEXT BEFORE any cluster mutation
# (no silent default context; no objects created);
# T6 event dedupe: exactly one Warning WorkloadNotReady + one Normal Ready
# per condition signature across the whole run (controller:336-379);
# T7 the renderer's Secret KEY SET is the heal precondition
# (internal/renderer/nfs.go:476-479: storage-ips/pool-name/api-key/
# data-ip): a `data:{}` Secret does NOT converge — presence alone is
# not readiness ("heal on Secret presence" is not auto-heal);
# T8 the offline fixture REFUSES a canonical live grant (allow-*) with
# rc 2 before any mutation -> the case is BLOCKED, never PASS;
# T9 the pinned atomic cleanup surface (CASE-CONTRACT "Ortak atomik
# cleanup helper"): create binds the response UID (stdout +
# ownership.tsv), a swapped-UID object is refused (rc 2) and survives,
# a registered UID deletes + postcondition-verifies, absence is
# idempotent, namespace misuse and gate refusals register nothing.
#
# Sabotage never edits the committed fixtures: it copies them to scratch and
# flips the behavior there.
# =============================================================================
set -u
contract_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export TMPDIR="$HOME/.cache/ngxstorage"
SCRATCH="$TMPDIR/quartet-contract/dependency-recovery-$$"
mkdir -p "$SCRATCH"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "CONTRACT OK  : $1"; }
fail() { FAIL=$((FAIL + 1)); echo "CONTRACT FAIL: $1"; }

CTX="offline-stub-context"
NS="quartet-dep-recovery-contract"

run_case() { # <helpers-dir> [extra env pairs...] -> writes case stdout to $SCRATCH/case-out.txt
  local helpers="$1"; shift
  env KUBE_CONTEXT="$CTX" TEST_NAMESPACE="$NS" ALLOW_LIVE=0 \
      SCENARIO_WAIT_SECONDS=12 SCENARIO_WAIT_INTERVAL=1 \
      SCENARIO_HELPERS="${helpers}/fixtures/scenario_helpers.sh" \
      "$@" bash "${contract_dir}/case.sh" > "$SCRATCH/case-out.txt" 2>&1
  local rc=$?
  echo "$rc" > "$SCRATCH/case-rc.txt"
  grep -E '^RESULT:' "$SCRATCH/case-out.txt" | tail -1
}

snapshot_dirs() { ls -1 "$TMPDIR/quartet-fake" 2>/dev/null | sort > "$1"; }
run_state_dir() { # <marker-file> -> the run's own fake-state dir
  local cand
  while IFS= read -r cand; do
    [[ -z "$cand" ]] && continue
    grep -qx "$cand" "$1" 2>/dev/null && continue
    echo "$TMPDIR/quartet-fake/$cand"; return
  done < <(ls -1d "$TMPDIR"/quartet-fake/dependency-recovery-off-* 2>/dev/null | xargs -n1 basename 2>/dev/null)
}

# --- T1: correct stub -> NOT RUN, all checks green ---------------------------
# Snapshot existing fake-state dirs so T6 can isolate T1's own run dir.
snapshot_dirs "$SCRATCH/.t1marker"
OUT=$(run_case "$contract_dir")
RC=$(cat "$SCRATCH/case-rc.txt")
T1STATE=$(run_state_dir "$SCRATCH/.t1marker")
if [[ "$RC" == "3" && "$OUT" == *"RESULT: NOT RUN"* ]] && ! grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T1 case body runs offline and reports NOT RUN with all checks green"
else
  fail "T1 expected exit 3 + 'RESULT: NOT RUN' + no CHECK FAIL (got rc=$RC: $OUT)"
fi
# Pin 8: the EXIT trap must have removed what the run created — the singleton
# CR, the backend Secret and the TEST_NAMESPACE are gone from the live obj
# store; the operator-created operand namespace stays (namespace ownership: never deleted).
if [[ -n "$T1STATE" ]]; then
  T1_LEFTOVER=$(ls "$T1STATE/obj/quartet/_/" 2>/dev/null | wc -l)
  T1_LEFTOVER=$(( T1_LEFTOVER + $(ls "$T1STATE/obj/secret/ngxstorage-nfs/" 2>/dev/null | wc -l) ))
  T1_LEFTOVER=$(( T1_LEFTOVER + $(ls "$T1STATE/obj/namespace/_/" 2>/dev/null | grep -c "^$NS\.json$" || true) ))
  if [[ "$T1_LEFTOVER" == "0" && -f "$T1STATE/obj/namespace/_/ngxstorage-nfs.json" ]]; then
    pass "T1 EXIT-trap cleanup removed every case-created object (operand namespace kept, namespace ownership)"
  else
    fail "T1 cleanup leftovers: quartet=$T1STATE/obj/quartet/_ secret+ns=$T1STATE (leftover_count=$T1_LEFTOVER)"
  fi
else
  fail "T1 could not locate the run's fake state to verify cleanup"
fi

# --- T2: snapshot-controller cell (probe path apply.go:421-423) ---------------
source "${contract_dir}/fixtures/scenario_helpers.sh"
export TEST_NAMESPACE="$NS"
export FAKE_STATE_DIR="${SCRATCH}/t2state"
fake_reset; fake_seed_install_rbac; fake_seed_operator_install
scenario_kube create -f <(jq -n --arg ns "$NS" '{apiVersion:"v1",kind:"Namespace",metadata:{name:$ns}}')
# Quartet manifest is cluster-scoped (real CRD spec.scope: Cluster): no namespace.
scenario_kube create -f <(jq -n --arg s "nfs-backend-secret" --arg img "$(fc_driver_image)" \
  '{apiVersion:"quartet.ngxstorage.com/v1alpha1",kind:"Quartet",metadata:{name:"quartet"},
    spec:{platform:"auto",profiles:{nfs:{enabled:true,secretRef:{name:$s},image:{reference:$img}},
      fc:{enabled:false},iscsi:{enabled:false}}}}')
# Baseline needs the FULL renderer key set (nfs.go:476-479), not presence.
fake_complete_backend_secret "nfs-backend-secret" "ngxstorage-nfs"
J=$(scenario_kube get quartet quartet -o json)  # cluster-scoped: no -n
if [[ "$(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason)"')" == "True Ready" ]]; then
  pass "T2 baseline converged Ready before the snapshot fault (full-key backend Secret)"
else
  fail "T2 baseline not Ready: $(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason) :: \(.message)"')"
fi
fake_set_image_unavailable
J=$(scenario_kube get quartet quartet -o json)  # cluster-scoped: no -n
MSG=$(echo "$J" | jq -r '.status.profiles.nfs.conditions[]? | select(.type=="Ready") | .message')
if [[ "$(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason)"')" == "False WorkloadNotReady" ]] \
   && [[ "$MSG" == *"ImagePullFailed"* ]]; then
  pass "T2 snapshot controller absent -> WorkloadNotReady 'ImagePullFailed' (apply.go:421-423)"
else
  fail "T2 snapshot fault cell wrong: $(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason) :: \(.message)"')"
fi
fake_clear_image_unavailable
J=$(scenario_kube get quartet quartet -o json)  # cluster-scoped: no -n
if [[ "$(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason)"')" == "True Ready" ]]; then
  pass "T2 heals to Ready when the snapshotter image becomes available"
else
  fail "T2 no heal after clearing the snapshot fault"
fi
# Unexport T2's state so the case.sh subprocesses below bootstrap their own.
unset FAKE_STATE_DIR TEST_NAMESPACE SCENARIO_RUN_ID

# --- T7: the renderer KEY SET is the heal precondition (nfs.go:476-479) -------
export TEST_NAMESPACE="$NS"
export FAKE_STATE_DIR="${SCRATCH}/t7state"
fake_reset; fake_seed_install_rbac; fake_seed_operator_install
scenario_kube create -f <(jq -n --arg ns "$NS" '{apiVersion:"v1",kind:"Namespace",metadata:{name:$ns}}')
# Quartet manifest is cluster-scoped (real CRD spec.scope: Cluster): no namespace.
scenario_kube create -f <(jq -n --arg s "nfs-backend-secret" --arg img "$(fc_driver_image)" \
  '{apiVersion:"quartet.ngxstorage.com/v1alpha1",kind:"Quartet",metadata:{name:"quartet"},
    spec:{platform:"auto",profiles:{nfs:{enabled:true,secretRef:{name:$s},image:{reference:$img}},
      fc:{enabled:false},iscsi:{enabled:false}}}}')
scenario_kube create -f <(jq -n '{apiVersion:"v1",kind:"Secret",metadata:{name:"nfs-backend-secret",namespace:"ngxstorage-nfs"},data:{}}')
J=$(scenario_kube get quartet quartet -o json)  # cluster-scoped: no -n
R7=$(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason)"')
M7=$(echo "$J" | jq -r '.status.profiles.nfs.conditions[]? | select(.type=="Ready") | .message')
if [[ "$R7" == "False WorkloadNotReady" && "$M7" == *"CreateContainerConfigError"* ]]; then
  pass "T7 data:{} Secret does NOT heal — all four renderer keys are required (nfs.go:476-479)"
else
  fail "T7 empty-data Secret unexpectedly converged: $R7 :: $M7"
fi
fake_complete_backend_secret "nfs-backend-secret" "ngxstorage-nfs"
# The kubelet model re-derives on the next read (watch trigger).
J=$(scenario_kube get quartet quartet -o json)  # cluster-scoped: no -n
if [[ "$(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason)"')" == "True Ready" ]]; then
  pass "T7 completes the key set -> heals to Ready"
else
  fail "T7 no heal after completing the renderer key set: $(echo "$J" | jq -r '.status.conditions[]? | select(.type=="Ready") | "\(.status) \(.reason) :: \(.message)"')"
fi
unset FAKE_STATE_DIR TEST_NAMESPACE SCENARIO_RUN_ID

# Sabotage copies live under <dir>/dependency-recovery/fixtures/ so the
# preflight name==directory check still passes (the flipped behavior, not the
# metadata, is what the case must catch).
sabotage_dir() { # <label> -> sets SAB_DIR, applies $2 sed to fake_cluster.sh
  SAB_DIR="$SCRATCH/sab/$1/dependency-recovery"
  mkdir -p "$SAB_DIR/fixtures"
  cp "${contract_dir}/fixtures/fake_cluster.sh" "${contract_dir}/fixtures/scenario_helpers.sh" \
     "${contract_dir}/fixtures/scenario_atomic_cleanup.sh" "$SAB_DIR/fixtures/"
  sed -i "$2" "$SAB_DIR/fixtures/fake_cluster.sh"
}

# --- T3: sabotage — stub promises MissingSecret (unproven reason) ------------
sabotage_dir reason 's/FC_PROF_REASON="WorkloadNotReady"/FC_PROF_REASON="MissingSecret"/g'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T3 sabotage (MissingSecret emitted) makes the case FAIL"
else
  fail "T3 sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T4: sabotage — stub never heals -----------------------------------------
sabotage_dir noheal 's/fc_exists "secret" "$op_ns" "$secret"/false/'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T4 sabotage (no heal after Secret exists) makes the case FAIL"
else
  fail "T4 sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T5: preflight rejects missing context BEFORE mutation -------------------
touch "$SCRATCH/marker"
env -u KUBE_CONTEXT TEST_NAMESPACE="$NS" ALLOW_LIVE=0 \
    SCENARIO_HELPERS="${contract_dir}/fixtures/scenario_helpers.sh" \
    bash "${contract_dir}/case.sh" > "$SCRATCH/case-out-nctx.txt" 2>&1
RC=$?
OUTLINE=$(grep -E '^RESULT:' "$SCRATCH/case-out-nctx.txt" | tail -1)
NEW_STATE=$(find "$TMPDIR/quartet-fake" -maxdepth 2 -name obj -newer "$SCRATCH/marker" 2>/dev/null | wc -l)
if [[ "$RC" == "2" && "$OUTLINE" == *"RESULT: BLOCKED"* && "$NEW_STATE" == "0" ]]; then
  pass "T5 missing KUBE_CONTEXT -> BLOCKED before any cluster mutation"
else
  fail "T5 expected rc=2 BLOCKED + zero created state (rc=$RC, out=$OUTLINE, new_state=$NEW_STATE)"
fi

# --- T8: canonical live grant on the offline fixture -> refused, BLOCKED -----
# Preflight bootstraps the SEED (the precondition: installed operator + RBAC);
# the refusal must keep every CASE object out — no quartet/secret/csidriver/...
seed_only_state() { # <state-dir> -> rc 0 iff obj/ holds only the seeded precondition kinds
  local extra
  extra=$(ls "$1/obj" 2>/dev/null | grep -vx 'clusterrole\|role\|rolebinding\|clusterrolebinding\|crd' | wc -l)
  [[ "$extra" == "0" ]]
}
snapshot_dirs "$SCRATCH/.t8marker"
env KUBE_CONTEXT="$CTX" TEST_NAMESPACE="$NS" ALLOW_LIVE=allow-cluster-scoped-fault \
    SCENARIO_HELPERS="${contract_dir}/fixtures/scenario_helpers.sh" \
    bash "${contract_dir}/case.sh" > "$SCRATCH/case-out-grant.txt" 2>&1
RC=$?
T8STATE=$(run_state_dir "$SCRATCH/.t8marker")
if [[ "$RC" == "2" ]] && grep -q "offline fixture cannot touch a real cluster" "$SCRATCH/case-out-grant.txt" \
   && grep -q "RESULT: BLOCKED" "$SCRATCH/case-out-grant.txt" \
   && [[ -n "${T8STATE:-}" ]] && seed_only_state "$T8STATE"; then
  pass "T8 ALLOW_LIVE=allow-* refused by the offline stub (BLOCKED, zero case mutation, never PASS)"
else
  fail "T8 expected rc=2 BLOCKED + refusal message + seed-only state (rc=$RC, state=${T8STATE:-none})"
fi

# --- T6: event dedupe across the T1 run --------------------------------------
# Isolate T1's state dir: the new quartet-fake/<case>-off-* entry created after
# the marker (sabotage runs share the same basename, so don't rely on head -1).
if [[ -n "$T1STATE" && -f "$T1STATE/events.tsv" ]]; then
 # Dedupe contract (controller:336-379): an event fires only when the
 # signature transitions — never two adjacent identical events. The case's
 # cleanup (secret removal) legitimately transitions back to blocked, so the
 # assertion is adjacency-based, plus the two required transitions present.
  ADJ_DUPES=$(awk -F'\t' '$1 FS $2 == prev { c++ } { prev = $1 FS $2 } END { print c+0 }' "$T1STATE/events.tsv")
  W=$(grep -c $'^Warning\tWorkloadNotReady\t' "$T1STATE/events.tsv" || true)
  R=$(grep -c $'^Normal\tReady\t' "$T1STATE/events.tsv" || true)
  if [[ "$ADJ_DUPES" == "0" && "$W" -ge 1 && "$R" -ge 1 ]]; then
    pass "T6 no adjacent duplicate events; both transitions emitted (controller:336-379)"
  else
    fail "T6 event dedupe broken (adjacent_dupes=$ADJ_DUPES Warning=$W NormalReady=$R)"
  fi
else
  fail "T6 could not locate the T1 fake state to inspect events"
fi

# --- T9: the pinned atomic cleanup surface (CASE-CONTRACT "Ortak atomik
# cleanup helper") validated on THIS fixture — exact names/argv/returns,
# the ownership.tsv evidence rows, swap refusal, absence idempotency,
# namespace-misuse refusal and gate refusal registering nothing.
source "${contract_dir}/fixtures/scenario_atomic_cleanup.sh"
export TEST_NAMESPACE="$NS"
export FAKE_STATE_DIR="${SCRATCH}/t9-state"
export EVIDENCE_DIR="${SCRATCH}/t9-evidence"
export RUN_ID="contract-t9"
fake_reset
mkdir -p "$EVIDENCE_DIR"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"t9-cm","namespace":"%s"},"data":{"payload":"owned by this run"}}' \
  "$NS" > "$SCRATCH/t9-cm.json"
T9_RC=0; T9_UID="$(scenario_create_tracked configmap t9-cm --namespace "$NS" "$SCRATCH/t9-cm.json")" || T9_RC=$?
T9_STORED_UID="$(jq -r '.metadata.uid // empty' "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json" 2>/dev/null)"
if [[ "$T9_RC" == "0" && "$T9_UID" == "$T9_STORED_UID" && -n "$T9_UID" ]] \
   && grep -qF "$(printf 'CREATE\tconfigmap\t%s\tt9-cm\t%s\tcreated' "$NS" "$T9_UID")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T9a create binds the create-response UID (stdout + ownership.tsv CREATE row)"
else
  fail "T9a create/registration wrong (rc=$T9_RC uid=$T9_UID stored=$T9_STORED_UID)"
fi
# swap the object under ownership: the live UID no longer matches the
# registered one -> SAHİPLENME YOK, the replacement is never deleted.
jq '.metadata.uid = "t9-swapped-uid"' "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json" \
  > "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json.t" \
  && mv "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json.t" "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json"
T9_RC=0; scenario_delete_owned configmap t9-cm --namespace "$NS" "$T9_UID" >/dev/null 2>&1 || T9_RC=$?
if [[ "$T9_RC" == "2" ]] \
   && [[ "$(jq -r '.metadata.uid' "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json")" == "t9-swapped-uid" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt9-cm\t%s\trefused-uid-mismatch' "$NS" "$T9_UID")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T9b swapped-UID object: delete is a refusal (rc 2), the replacement survives untouched"
else
  fail "T9b swap refusal wrong (rc=$T9_RC)"
fi
# clean registered delete + postcondition NotFound + evidence row.
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"t9-cm2","namespace":"%s"},"data":{"payload":"owned by this run"}}' \
  "$NS" > "$SCRATCH/t9-cm2.json"
T9_RC=0; T9_UID2="$(scenario_create_tracked configmap t9-cm2 --namespace "$NS" "$SCRATCH/t9-cm2.json")" || T9_RC=$?
T9_RC2=0; scenario_delete_owned configmap t9-cm2 --namespace "$NS" "$T9_UID2" || T9_RC2=$?
if [[ "$T9_RC" == "0" && "$T9_RC2" == "0" ]] \
   && [[ ! -f "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm2.json" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt9-cm2\t%s\tdeleted' "$NS" "$T9_UID2")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T9c registered UID: UID-preconditioned delete verifies NotFound (postcondition) and records it"
else
  fail "T9c clean delete wrong (create_rc=$T9_RC delete_rc=$T9_RC2)"
fi
# idempotent absence: deleting an already-gone obligation is rc 0, not a failure.
T9_RC=0; scenario_delete_owned configmap t9-cm2 --namespace "$NS" "$T9_UID2" >/dev/null 2>&1 || T9_RC=$?
if [[ "$T9_RC" == "0" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt9-cm2\t%s\tabsent' "$NS" "$T9_UID2")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T9d delete of an already-absent obligation is idempotent rc 0 (outcome=absent)"
else
  fail "T9d absence idempotency wrong (rc=$T9_RC)"
fi
# namespace binding: a namespaced kind without --namespace is refused BEFORE
# dispatch (never a silent current-namespace default).
T9_RC=0; scenario_create_tracked configmap t9-cm3 "$SCRATCH/t9-cm.json" >/dev/null 2>&1 || T9_RC=$?
if [[ "$T9_RC" == "2" && ! -f "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm3.json" ]] \
   && [[ "$(grep -c "t9-cm3" "$EVIDENCE_DIR/ownership.tsv" 2>/dev/null || true)" == "0" ]]; then
  pass "T9e missing --namespace on a namespaced kind is a refusal (rc 2), zero mutation"
else
  fail "T9e namespace-misuse refusal wrong (rc=$T9_RC)"
fi
# gate refusal: the offline fixture under a live grant refuses every call —
# a refusal is never a success and registers nothing.
T9_LINES_BEFORE="$(wc -l < "$EVIDENCE_DIR/ownership.tsv")"
T9_RC=0; ALLOW_LIVE=allow-cluster-scoped-fault scenario_create_tracked configmap t9-cm4 --namespace "$NS" "$SCRATCH/t9-cm.json" >/dev/null 2>&1 || T9_RC=$?
T9_RC2=0; ALLOW_LIVE=allow-cluster-scoped-fault scenario_delete_owned configmap t9-cm --namespace "$NS" "$T9_UID" >/dev/null 2>&1 || T9_RC2=$?
if [[ "$T9_RC" == "2" && "$T9_RC2" == "2" ]] \
   && [[ "$(wc -l < "$EVIDENCE_DIR/ownership.tsv")" == "$T9_LINES_BEFORE" ]] \
   && [[ "$(jq -r '.metadata.uid' "$FAKE_STATE_DIR/obj/configmap/$NS/t9-cm.json")" == "t9-swapped-uid" ]]; then
  pass "T9f gate refusal (rc 2) registers nothing and deletes nothing — never a success"
else
  fail "T9f refusal handling wrong (create_rc=$T9_RC delete_rc=$T9_RC2)"
fi
unset FAKE_STATE_DIR EVIDENCE_DIR TEST_NAMESPACE RUN_ID SCENARIO_RUN_ID

echo "----------------------------------------"
echo "CONTRACT RESULT: $PASS passed, $FAIL failed"
(( FAIL == 0 ))
