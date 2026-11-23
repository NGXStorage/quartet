#!/usr/bin/env bash
# =============================================================================
# Offline contract — foreign-resource-conflict
#
# correct-stub case body runs offline -> NOT RUN (never PASS), all checks green, and
# the EXIT-trap cleanup left nothing behind (canary, CR, secret,
# created namespace);
# sabotage: stub adopts the foreign object: stub ADOPTS the foreign CSIDriver (ownership gate no-ops)
# -> the byte-identical/UID/annotation invariants make the case FAIL;
# sabotage: stub reports an unproven reason: stub SELF-REPAIRS the drifted RBAC (forbidden — read-only)
# -> the case never observes RBACDrift and FAILs;
# sabotage: stub never heals: stub reports ApplyConflict instead of ForeignResourceConflict
# (wrong reason, wrong terminal class) -> the case FAILs;
# T5 preflight rejects missing KUBE_CONTEXT BEFORE any mutation;
# T6 ALLOW_LIVE=1 is refused by the offline stub (never touches a real
# cluster) -> BLOCKED;
# T7 a canonical live grant (ALLOW_LIVE=allow-*) is refused the same way —
# zero mutation, BLOCKED, never PASS;
# T8 pin 8 (cleanup on EVERY exit path): with the RBACDrift gate sabotaged
# the case FAILs mid-phase-2 — the EXIT trap must still RESTORE the
# tampered installer ClusterRole and delete the planted canaries;
# T9 pin 7 (cleanup failure is 4, not 5): with quartet deletion sabotaged
# the trap cannot clean -> FAIL_CLEANUP (rc 4), never NOT RUN/PASS;
# T10 the pinned atomic cleanup surface (CASE-CONTRACT "Ortak atomik
# cleanup helper"): response-UID binding, swap refusal (rc 2, object
# survives), postcondition-verified delete, absence idempotency,
# namespace misuse and gate refusals register nothing.
# Sabotage copies the fixtures to scratch and flips behavior there; the
# committed fixtures are never modified.
# =============================================================================
set -u
contract_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export TMPDIR="$HOME/.cache/ngxstorage"
SCRATCH="$TMPDIR/quartet-contract/foreign-resource-conflict-$$"
mkdir -p "$SCRATCH"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "CONTRACT OK  : $1"; }
fail() { FAIL=$((FAIL + 1)); echo "CONTRACT FAIL: $1"; }

CTX="offline-stub-context"
NS="quartet-foreign-conflict-contract"

run_case() { # <helpers-dir> [extra env pairs...]
  local helpers="$1"; shift
  env KUBE_CONTEXT="$CTX" TEST_NAMESPACE="$NS" ALLOW_LIVE=0 \
      SCENARIO_WAIT_SECONDS=10 SCENARIO_WAIT_INTERVAL=1 \
      SCENARIO_HELPERS="${helpers}/fixtures/scenario_helpers.sh" \
      "$@" bash "${contract_dir}/case.sh" > "$SCRATCH/case-out.txt" 2>&1
  local rc=$?
  echo "$rc" > "$SCRATCH/case-rc.txt"
  grep -E '^RESULT:' "$SCRATCH/case-out.txt" | tail -1
}

snapshot_dirs() { ls -1 "$TMPDIR/quartet-fake" 2>/dev/null | sort > "$1"; }
# The newest fake-state dir created AFTER the marker — the run's own dir.
run_state_dir() { # <marker-file>
  local cand
  while IFS= read -r cand; do
    [[ -z "$cand" ]] && continue
    grep -qx "$cand" "$1" 2>/dev/null && continue
    echo "$TMPDIR/quartet-fake/$cand"; return
  done < <(ls -1d "$TMPDIR"/quartet-fake/foreign-resource-conflict-off-* 2>/dev/null | xargs -n1 basename 2>/dev/null)
}

# Content identity for the T8 restore comparison: volatile server-managed
# fields AND the uid are stripped — the contract's golden store and the run's
# store stamp DIFFERENT fake uids by construction; what must match is the
# manifest content (rules/labels), i.e. that the tamper was undone.
norm_digest() { jq -S 'del(.metadata.resourceVersion, .metadata.uid, .metadata.managedFields, .metadata.creationTimestamp)' | sha256sum | cut -d' ' -f1; }

seed_only_state() { # <state-dir> -> rc 0 iff obj/ holds only the seeded precondition kinds
  local extra
  extra=$(ls "$1/obj" 2>/dev/null | grep -vx 'clusterrole\|role\|rolebinding\|clusterrolebinding\|crd' | wc -l)
  [[ "$extra" == "0" ]]
}

sabotage_dir() { # <label> <python-transform-expr operating on 'src'>
  SAB_DIR="$SCRATCH/sab/$1/foreign-resource-conflict"
  mkdir -p "$SAB_DIR/fixtures"
  cp "${contract_dir}/fixtures/fake_cluster.sh" "${contract_dir}/fixtures/scenario_helpers.sh" \
     "${contract_dir}/fixtures/scenario_atomic_cleanup.sh" "$SAB_DIR/fixtures/"
  printf '%s\n' "$2" > "$SAB_DIR/transform.py"
  python3 - "$SAB_DIR/fixtures/fake_cluster.sh" "$SAB_DIR/transform.py" <<'PY'
import sys
path, tpath = sys.argv[1], sys.argv[2]
src = open(path).read()
new = eval(open(tpath).read())  # transform expr operating on 'src'
assert new != src, "sabotage flip did not apply"
open(path, "w").write(new)
PY
}

# Golden digest of the healthy installer ClusterRole (the restore target).
GOLDEN_STATE="$SCRATCH/goldenstate"
source "${contract_dir}/fixtures/scenario_helpers.sh"
export FAKE_STATE_DIR="$GOLDEN_STATE"
fake_reset; fake_seed_install_rbac
GOLDEN_CR_DIGEST=$(fc_get clusterrole "" nfs-csi-ngxstorage-controller | norm_digest)
unset FAKE_STATE_DIR SCENARIO_RUN_ID

# --- T1: NOT RUN + all checks green + trap cleanup left nothing ---------------
snapshot_dirs "$SCRATCH/.t1marker"
OUT=$(run_case "$contract_dir"); RC=$(cat "$SCRATCH/case-rc.txt")
T1STATE=$(run_state_dir "$SCRATCH/.t1marker")
if [[ "$RC" == "3" && "$OUT" == *"RESULT: NOT RUN"* ]] && ! grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T1 case body runs offline and reports NOT RUN with all checks green"
else
  fail "T1 expected exit 3 + 'RESULT: NOT RUN' + no CHECK FAIL (got rc=$RC: $OUT)"
fi
if [[ -n "${T1STATE:-}" ]]; then
 # Case artifacts only: CR, backend secret, created namespace. The
 # operator-adopted CSIDriver name (and other operands) legitimately remain
 # in the fake store — the fake models no finalizer cleanup; in the real API
 # the CR deletion's finalizer owns those operands, not this case.
  T1_LEFT=$(ls "$T1STATE/obj/quartet/_/" 2>/dev/null | wc -l)
  T1_LEFT=$(( T1_LEFT + $(ls "$T1STATE/obj/secret/ngxstorage-nfs/" 2>/dev/null | wc -l) ))
  T1_LEFT=$(( T1_LEFT + $(ls "$T1STATE/obj/namespace/_/" 2>/dev/null | grep -c "^$NS\.json$" || true) ))
 # The planted canary itself must be gone: any CSIDriver left must NOT carry
 # the canary's foreign-marker label (the operator adopted the name in-flow).
  T1_CANARY_MARK=$(jq -r '.metadata.labels["foreign-marker"] // ""' \
    "$T1STATE/obj/csidriver/_/nfs.csi.ngxstorage.com.json" 2>/dev/null)
  if [[ "$T1_LEFT" == "0" && -z "$T1_CANARY_MARK" \
        && -f "$T1STATE/obj/clusterrole/_/nfs-csi-ngxstorage-controller.json" ]]; then
    pass "T1 EXIT-trap cleanup removed CR/canary/secret/created-ns; installer RBAC untouched"
  else
    fail "T1 cleanup leftovers (count=$T1_LEFT, canary_marker=${T1_CANARY_MARK:-<none>}) in $T1STATE"
  fi
else
  fail "T1 could not locate the run's fake state to verify cleanup"
fi

# --- T2: adoption sabotage -----------------------------------------------------
sabotage_dir adopt 'src.replace("[[ \"$owner\" == \"$uid\" ]] || return 10", "[[ true ]]")'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T2 sabotage (foreign object adopted) makes the case FAIL"
else
  fail "T2 adoption sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T3: RBAC self-repair sabotage --------------------------------------------
sabotage_dir autorepair 'src.replace("  local uid=\"$1\" secret=\"$2\" op_ns", "  local uid=\"$1\" secret=\"$2\" op_ns\n  fake_seed_install_rbac", 1)'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T3 sabotage (RBAC self-repaired) makes the case FAIL"
else
  fail "T3 self-repair sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T4: wrong-reason sabotage -------------------------------------------------
sabotage_dir wrongreason 'src.replace("FC_PROF_REASON=\"ForeignResourceConflict\"", "FC_PROF_REASON=\"ApplyConflict\"", 1)'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T4 sabotage (ApplyConflict instead of ForeignResourceConflict) makes the case FAIL"
else
  fail "T4 wrong-reason sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T5: preflight rejects missing context -------------------------------------
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

# --- T6: legacy live refusal ----------------------------------------------------
env KUBE_CONTEXT="$CTX" TEST_NAMESPACE="$NS" ALLOW_LIVE=1 \
    SCENARIO_HELPERS="${contract_dir}/fixtures/scenario_helpers.sh" \
    bash "${contract_dir}/case.sh" > "$SCRATCH/case-out-live.txt" 2>&1
RC=$?
if [[ "$RC" == "2" ]] && grep -q "offline fixture cannot touch a real cluster" "$SCRATCH/case-out-live.txt"; then
  pass "T6 ALLOW_LIVE=1 refused by the offline stub (BLOCKED, never a real cluster)"
else
  fail "T6 expected live refusal rc=2 (rc=$RC)"
fi

# --- T7: canonical live grant refusal -------------------------------------------
# Preflight bootstraps the SEED (precondition: installed operator + RBAC); the
# refusal must keep every CASE object out — no quartet/csidriver/secret/...
snapshot_dirs "$SCRATCH/.t7marker"
env KUBE_CONTEXT="$CTX" TEST_NAMESPACE="$NS" ALLOW_LIVE=allow-cluster-scoped-fault \
    SCENARIO_HELPERS="${contract_dir}/fixtures/scenario_helpers.sh" \
    bash "${contract_dir}/case.sh" > "$SCRATCH/case-out-grant.txt" 2>&1
RC=$?
T7STATE=$(run_state_dir "$SCRATCH/.t7marker")
if [[ "$RC" == "2" ]] && grep -q "offline fixture cannot touch a real cluster" "$SCRATCH/case-out-grant.txt" \
   && grep -q "RESULT: BLOCKED" "$SCRATCH/case-out-grant.txt" \
   && [[ -n "${T7STATE:-}" ]] && seed_only_state "$T7STATE"; then
  pass "T7 ALLOW_LIVE=allow-* refused by the offline stub (BLOCKED, zero case mutation, never PASS)"
else
  fail "T7 expected rc=2 BLOCKED + refusal message + seed-only state (rc=$RC, state=${T7STATE:-none})"
fi

# --- T8: trap restores the tamper + deletes canaries on the FAIL path ----------
snapshot_dirs "$SCRATCH/.t8marker"
sabotage_dir rbdflip 'src.replace("FC_PROF_REASON=\"RBACDrift\"", "FC_PROF_REASON=\"WorkloadNotReady\"")'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
T8STATE=$(run_state_dir "$SCRATCH/.t8marker")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T8 sabotage (RBACDrift gate skipped) makes the case FAIL on the body path"
else
  fail "T8 expected rc=1 + CHECK FAIL (rc=$RC: $OUT)"
fi
if [[ -n "${T8STATE:-}" ]]; then
  T8_CR_DIGEST=$(jq -S . "$T8STATE/obj/clusterrole/_/nfs-csi-ngxstorage-controller.json" 2>/dev/null | norm_digest)
  if [[ "$T8_CR_DIGEST" == "$GOLDEN_CR_DIGEST" ]]; then
    pass "T8 EXIT trap RESTORED the tampered installer ClusterRole on the FAIL path"
  else
    fail "T8 ClusterRole not restored (digest $T8_CR_DIGEST != golden $GOLDEN_CR_DIGEST)"
  fi
 # Canaries only: the planted CSIDriver must not carry its foreign-marker
 # label any more (deleted in-flow; the operator may own the name), CR,
 # secret and the created namespace must be gone.
  T8_LEFT=0
  T8_CANARY_MARK=$(jq -r '.metadata.labels["foreign-marker"] // ""' \
    "$T8STATE/obj/csidriver/_/nfs.csi.ngxstorage.com.json" 2>/dev/null)
  [[ -n "$T8_CANARY_MARK" ]] && T8_LEFT=1
  [[ -n "$(ls "$T8STATE/obj/quartet/_/" 2>/dev/null)" ]] && T8_LEFT=1
  [[ -n "$(ls "$T8STATE/obj/secret/ngxstorage-nfs/" 2>/dev/null)" ]] && T8_LEFT=1
  [[ -f "$T8STATE/obj/namespace/_/$NS.json" ]] && T8_LEFT=1
  if [[ "$T8_LEFT" == "0" ]]; then
    pass "T8 EXIT trap deleted every case-created canary on the FAIL path"
  else
    fail "T8 canary leftovers in $T8STATE (count=$T8_LEFT)"
  fi
else
  fail "T8 could not locate the run's fake state"
fi

# --- T9: cleanup failure escalates to FAIL_CLEANUP (4), never 5/3 ---------------
snapshot_dirs "$SCRATCH/.t9marker"
# Sabotage the delete branch itself: fc_delete's rc 1 would read as
# "not found" and be swallowed by --ignore-not-found, so the refusal has to
# come AFTER that handling — i.e. from the verb handler, like a real API
# error (forbidden/unavailable), which cleanup must report as failed.
sabotage_dir quartetdelete 'src.replace("      local kind=\"\" name=\"\" ns=\"\" klc ignore_nf=0", "      if [[ \"$*\" == *--raw=*quartets* ]]; then echo \"Error from server (ServiceUnavailable): delete refused\" >&2; return 1; fi\n      local kind=\"\" name=\"\" ns=\"\" klc ignore_nf=0", 1)'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "4" ]] && grep -q "RESULT: FAIL_CLEANUP" "$SCRATCH/case-out.txt"; then
  pass "T9 cleanup failure -> FAIL_CLEANUP rc=4 (the old exit-5 path is gone)"
else
  fail "T9 expected rc=4 + FAIL_CLEANUP (rc=$RC: $OUT)"
fi

# --- T10: the pinned atomic cleanup surface (CASE-CONTRACT "Ortak atomik
# cleanup helper") validated on THIS fixture — the same battery the
# case body relies on: create binds the response UID, a swapped-UID
# object is refused (rc 2) and survives, a registered UID deletes and
# postcondition-verifies, absence is idempotent, namespace misuse and
# gate refusals register nothing.
source "${contract_dir}/fixtures/scenario_atomic_cleanup.sh"
export TEST_NAMESPACE="$NS"
export FAKE_STATE_DIR="${SCRATCH}/t10-state"
export EVIDENCE_DIR="${SCRATCH}/t10-evidence"
export RUN_ID="contract-t10"
fake_reset
mkdir -p "$EVIDENCE_DIR"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"t10-cm","namespace":"%s"},"data":{"payload":"owned by this run"}}' \
  "$NS" > "$SCRATCH/t10-cm.json"
T10_RC=0; T10_UID="$(scenario_create_tracked configmap t10-cm --namespace "$NS" "$SCRATCH/t10-cm.json")" || T10_RC=$?
T10_STORED_UID="$(jq -r '.metadata.uid // empty' "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json" 2>/dev/null)"
if [[ "$T10_RC" == "0" && "$T10_UID" == "$T10_STORED_UID" && -n "$T10_UID" ]] \
   && grep -qF "$(printf 'CREATE\tconfigmap\t%s\tt10-cm\t%s\tcreated' "$NS" "$T10_UID")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T10a create binds the create-response UID (stdout + ownership.tsv CREATE row)"
else
  fail "T10a create/registration wrong (rc=$T10_RC uid=$T10_UID stored=$T10_STORED_UID)"
fi
jq '.metadata.uid = "t10-swapped-uid"' "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json" \
  > "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json.t" \
  && mv "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json.t" "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json"
T10_RC=0; scenario_delete_owned configmap t10-cm --namespace "$NS" "$T10_UID" >/dev/null 2>&1 || T10_RC=$?
if [[ "$T10_RC" == "2" ]] \
   && [[ "$(jq -r '.metadata.uid' "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json")" == "t10-swapped-uid" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt10-cm\t%s\trefused-uid-mismatch' "$NS" "$T10_UID")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T10b swapped-UID object: delete is a refusal (rc 2), the replacement survives untouched"
else
  fail "T10b swap refusal wrong (rc=$T10_RC)"
fi
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"t10-cm2","namespace":"%s"},"data":{"payload":"owned by this run"}}' \
  "$NS" > "$SCRATCH/t10-cm2.json"
T10_RC=0; T10_UID2="$(scenario_create_tracked configmap t10-cm2 --namespace "$NS" "$SCRATCH/t10-cm2.json")" || T10_RC=$?
T10_RC2=0; scenario_delete_owned configmap t10-cm2 --namespace "$NS" "$T10_UID2" || T10_RC2=$?
if [[ "$T10_RC" == "0" && "$T10_RC2" == "0" ]] \
   && [[ ! -f "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm2.json" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt10-cm2\t%s\tdeleted' "$NS" "$T10_UID2")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T10c registered UID: UID-preconditioned delete verifies NotFound (postcondition) and records it"
else
  fail "T10c clean delete wrong (create_rc=$T10_RC delete_rc=$T10_RC2)"
fi
T10_RC=0; scenario_delete_owned configmap t10-cm2 --namespace "$NS" "$T10_UID2" >/dev/null 2>&1 || T10_RC=$?
if [[ "$T10_RC" == "0" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt10-cm2\t%s\tabsent' "$NS" "$T10_UID2")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T10d delete of an already-absent obligation is idempotent rc 0 (outcome=absent)"
else
  fail "T10d absence idempotency wrong (rc=$T10_RC)"
fi
T10_RC=0; scenario_create_tracked configmap t10-cm3 "$SCRATCH/t10-cm.json" >/dev/null 2>&1 || T10_RC=$?
if [[ "$T10_RC" == "2" && ! -f "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm3.json" ]] \
   && [[ "$(grep -c "t10-cm3" "$EVIDENCE_DIR/ownership.tsv" 2>/dev/null || true)" == "0" ]]; then
  pass "T10e missing --namespace on a namespaced kind is a refusal (rc 2), zero mutation"
else
  fail "T10e namespace-misuse refusal wrong (rc=$T10_RC)"
fi
T10_LINES_BEFORE="$(wc -l < "$EVIDENCE_DIR/ownership.tsv")"
T10_RC=0; ALLOW_LIVE=allow-cluster-scoped-fault scenario_create_tracked configmap t10-cm4 --namespace "$NS" "$SCRATCH/t10-cm.json" >/dev/null 2>&1 || T10_RC=$?
T10_RC2=0; ALLOW_LIVE=allow-cluster-scoped-fault scenario_delete_owned configmap t10-cm --namespace "$NS" "$T10_UID" >/dev/null 2>&1 || T10_RC2=$?
if [[ "$T10_RC" == "2" && "$T10_RC2" == "2" ]] \
   && [[ "$(wc -l < "$EVIDENCE_DIR/ownership.tsv")" == "$T10_LINES_BEFORE" ]] \
   && [[ "$(jq -r '.metadata.uid' "$FAKE_STATE_DIR/obj/configmap/$NS/t10-cm.json")" == "t10-swapped-uid" ]]; then
  pass "T10f gate refusal (rc 2) registers nothing and deletes nothing — never a success"
else
  fail "T10f refusal handling wrong (create_rc=$T10_RC delete_rc=$T10_RC2)"
fi
unset FAKE_STATE_DIR EVIDENCE_DIR TEST_NAMESPACE RUN_ID SCENARIO_RUN_ID

echo "----------------------------------------"
echo "CONTRACT RESULT: $PASS passed, $FAIL failed"
(( FAIL == 0 ))
