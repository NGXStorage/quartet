#!/usr/bin/env bash
# =============================================================================
# Offline contract — singleton-rejection
#
# correct-stub case body runs offline -> NOT RUN (never PASS), all checks green, and
# the EXIT-trap cleanup removed BOTH CRs (the rejected foreign canary
# included), the secret and the created namespace;
# sabotage: stub adopts the foreign object: stub skips ValidateName (accepts the foreign-named CR) ->
# the rejection/never-manages-operands invariants make the case FAIL;
# sabotage: stub reports an unproven reason: stub records an accepted specPlatform on rejection
# (failedStatus must NOT advance it) -> the case FAILs;
# sabotage: stub never heals: stub allows a duplicate same-name create (must be
# API-atomic AlreadyExists) -> the case FAILs;
# T5 preflight rejects missing KUBE_CONTEXT BEFORE any mutation;
# T6 ALLOW_LIVE=1 is refused by the offline stub (never touches a real
# cluster) -> BLOCKED;
# T7 a canonical live grant (ALLOW_LIVE=allow-*) is refused the same way —
# zero mutation, BLOCKED, never PASS;
# T8 the pinned atomic cleanup surface (CASE-CONTRACT "Ortak atomik
# cleanup helper"): response-UID binding, swap refusal (rc 2, object
# survives), postcondition-verified delete, absence idempotency,
# namespace misuse and gate refusals register nothing.
# Sabotage copies the fixtures to scratch; the committed fixtures never change.
# =============================================================================
set -u
contract_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export TMPDIR="$HOME/.cache/ngxstorage"
SCRATCH="$TMPDIR/quartet-contract/singleton-rejection-$$"
mkdir -p "$SCRATCH"

PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "CONTRACT OK  : $1"; }
fail() { FAIL=$((FAIL + 1)); echo "CONTRACT FAIL: $1"; }

CTX="offline-stub-context"
NS="quartet-singleton-rejection-contract"

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
run_state_dir() { # <marker-file>
  local cand
  while IFS= read -r cand; do
    [[ -z "$cand" ]] && continue
    grep -qx "$cand" "$1" 2>/dev/null && continue
    echo "$TMPDIR/quartet-fake/$cand"; return
  done < <(ls -1d "$TMPDIR"/quartet-fake/singleton-rejection-off-* 2>/dev/null | xargs -n1 basename 2>/dev/null)
}

seed_only_state() { # <state-dir> -> rc 0 iff obj/ holds only the seeded precondition kinds
  local extra
  extra=$(ls "$1/obj" 2>/dev/null | grep -vx 'clusterrole\|role\|rolebinding\|clusterrolebinding\|crd' | wc -l)
  [[ "$extra" == "0" ]]
}

sabotage_dir() { # <label> <python-transform-expr operating on 'src'>
  SAB_DIR="$SCRATCH/sab/$1/singleton-rejection"
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

# --- T1: NOT RUN + all checks green + trap cleanup ------------------------------
snapshot_dirs "$SCRATCH/.t1marker"
OUT=$(run_case "$contract_dir"); RC=$(cat "$SCRATCH/case-rc.txt")
T1STATE=$(run_state_dir "$SCRATCH/.t1marker")
if [[ "$RC" == "3" && "$OUT" == *"RESULT: NOT RUN"* ]] && ! grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T1 case body runs offline and reports NOT RUN with all checks green"
else
  fail "T1 expected exit 3 + 'RESULT: NOT RUN' + no CHECK FAIL (got rc=$RC: $OUT)"
fi
if [[ -n "${T1STATE:-}" ]]; then
  T1_LEFT=$(ls "$T1STATE/obj/quartet/_/" 2>/dev/null | wc -l)
  T1_LEFT=$(( T1_LEFT + $(ls "$T1STATE/obj/secret/ngxstorage-nfs/" 2>/dev/null | wc -l) ))
  T1_LEFT=$(( T1_LEFT + $(ls "$T1STATE/obj/namespace/_/" 2>/dev/null | grep -c "^$NS\.json$" || true) ))
  if [[ "$T1_LEFT" == "0" ]]; then
    pass "T1 EXIT-trap cleanup removed both CRs (foreign canary included), secret, created ns"
  else
    fail "T1 cleanup leftovers (count=$T1_LEFT) in $T1STATE"
  fi
else
  fail "T1 could not locate the run's fake state to verify cleanup"
fi

# --- T2: accept the foreign name (skip ValidateName) --------------------------
sabotage_dir acceptname 'src.replace("if [[ \"$name\" != \"$(fc_singleton_name)\" ]]; then", "if false; then", 1)'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T2 sabotage (foreign-named CR accepted/reconciled) makes the case FAIL"
else
  fail "T2 accept-name sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T3: record specPlatform on rejection --------------------------------------
sabotage_dir recspecplatform 'src.replace("specPlatform: \"\", resolvedPlatform: \"\", profiles: null,", "specPlatform: \"auto\", resolvedPlatform: \"kubernetes\", profiles: null,", 1)'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T3 sabotage (specPlatform recorded on rejection) makes the case FAIL"
else
  fail "T3 specPlatform sabotage did not bite (rc=$RC: $OUT)"
fi

# --- T4: allow duplicate same-name create --------------------------------------
sabotage_dir dupcreate 'src.replace("if fc_exists \"$klc\" \"$ns\" \"$name\"; then", "if false; then", 1)'
OUT=$(run_case "$SAB_DIR"); RC=$(cat "$SCRATCH/case-rc.txt")
if [[ "$RC" == "1" ]] && grep -q "CHECK FAIL" "$SCRATCH/case-out.txt"; then
  pass "T4 sabotage (duplicate same-name create accepted) makes the case FAIL"
else
  fail "T4 duplicate-create sabotage did not bite (rc=$RC: $OUT)"
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
# refusal must keep every CASE object out — no quartet/secret/...
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

# --- T8: the pinned atomic cleanup surface (CASE-CONTRACT "Ortak atomik
# cleanup helper") validated on THIS fixture — the same battery the
# case body relies on: create binds the response UID, a swapped-UID
# object is refused (rc 2) and survives, a registered UID deletes and
# postcondition-verifies, absence is idempotent, namespace misuse and
# gate refusals register nothing.
source "${contract_dir}/fixtures/scenario_helpers.sh"
source "${contract_dir}/fixtures/scenario_atomic_cleanup.sh"
export TEST_NAMESPACE="$NS"
export FAKE_STATE_DIR="${SCRATCH}/t8-state"
export EVIDENCE_DIR="${SCRATCH}/t8-evidence"
export RUN_ID="contract-t8"
fake_reset
mkdir -p "$EVIDENCE_DIR"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"t8-cm","namespace":"%s"},"data":{"payload":"owned by this run"}}' \
  "$NS" > "$SCRATCH/t8-cm.json"
T8_RC=0; T8_UID="$(scenario_create_tracked configmap t8-cm --namespace "$NS" "$SCRATCH/t8-cm.json")" || T8_RC=$?
T8_STORED_UID="$(jq -r '.metadata.uid // empty' "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json" 2>/dev/null)"
if [[ "$T8_RC" == "0" && "$T8_UID" == "$T8_STORED_UID" && -n "$T8_UID" ]] \
   && grep -qF "$(printf 'CREATE\tconfigmap\t%s\tt8-cm\t%s\tcreated' "$NS" "$T8_UID")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T8a create binds the create-response UID (stdout + ownership.tsv CREATE row)"
else
  fail "T8a create/registration wrong (rc=$T8_RC uid=$T8_UID stored=$T8_STORED_UID)"
fi
jq '.metadata.uid = "t8-swapped-uid"' "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json" \
  > "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json.t" \
  && mv "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json.t" "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json"
T8_RC=0; scenario_delete_owned configmap t8-cm --namespace "$NS" "$T8_UID" >/dev/null 2>&1 || T8_RC=$?
if [[ "$T8_RC" == "2" ]] \
   && [[ "$(jq -r '.metadata.uid' "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json")" == "t8-swapped-uid" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt8-cm\t%s\trefused-uid-mismatch' "$NS" "$T8_UID")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T8b swapped-UID object: delete is a refusal (rc 2), the replacement survives untouched"
else
  fail "T8b swap refusal wrong (rc=$T8_RC)"
fi
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"t8-cm2","namespace":"%s"},"data":{"payload":"owned by this run"}}' \
  "$NS" > "$SCRATCH/t8-cm2.json"
T8_RC=0; T8_UID2="$(scenario_create_tracked configmap t8-cm2 --namespace "$NS" "$SCRATCH/t8-cm2.json")" || T8_RC=$?
T8_RC2=0; scenario_delete_owned configmap t8-cm2 --namespace "$NS" "$T8_UID2" || T8_RC2=$?
if [[ "$T8_RC" == "0" && "$T8_RC2" == "0" ]] \
   && [[ ! -f "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm2.json" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt8-cm2\t%s\tdeleted' "$NS" "$T8_UID2")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T8c registered UID: UID-preconditioned delete verifies NotFound (postcondition) and records it"
else
  fail "T8c clean delete wrong (create_rc=$T8_RC delete_rc=$T8_RC2)"
fi
T8_RC=0; scenario_delete_owned configmap t8-cm2 --namespace "$NS" "$T8_UID2" >/dev/null 2>&1 || T8_RC=$?
if [[ "$T8_RC" == "0" ]] \
   && grep -qF "$(printf 'DELETE\tconfigmap\t%s\tt8-cm2\t%s\tabsent' "$NS" "$T8_UID2")" "$EVIDENCE_DIR/ownership.tsv"; then
  pass "T8d delete of an already-absent obligation is idempotent rc 0 (outcome=absent)"
else
  fail "T8d absence idempotency wrong (rc=$T8_RC)"
fi
T8_RC=0; scenario_create_tracked configmap t8-cm3 "$SCRATCH/t8-cm.json" >/dev/null 2>&1 || T8_RC=$?
if [[ "$T8_RC" == "2" && ! -f "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm3.json" ]] \
   && [[ "$(grep -c "t8-cm3" "$EVIDENCE_DIR/ownership.tsv" 2>/dev/null || true)" == "0" ]]; then
  pass "T8e missing --namespace on a namespaced kind is a refusal (rc 2), zero mutation"
else
  fail "T8e namespace-misuse refusal wrong (rc=$T8_RC)"
fi
T8_LINES_BEFORE="$(wc -l < "$EVIDENCE_DIR/ownership.tsv")"
T8_RC=0; ALLOW_LIVE=allow-cluster-scoped-fault scenario_create_tracked configmap t8-cm4 --namespace "$NS" "$SCRATCH/t8-cm.json" >/dev/null 2>&1 || T8_RC=$?
T8_RC2=0; ALLOW_LIVE=allow-cluster-scoped-fault scenario_delete_owned configmap t8-cm --namespace "$NS" "$T8_UID" >/dev/null 2>&1 || T8_RC2=$?
if [[ "$T8_RC" == "2" && "$T8_RC2" == "2" ]] \
   && [[ "$(wc -l < "$EVIDENCE_DIR/ownership.tsv")" == "$T8_LINES_BEFORE" ]] \
   && [[ "$(jq -r '.metadata.uid' "$FAKE_STATE_DIR/obj/configmap/$NS/t8-cm.json")" == "t8-swapped-uid" ]]; then
  pass "T8f gate refusal (rc 2) registers nothing and deletes nothing — never a success"
else
  fail "T8f refusal handling wrong (create_rc=$T8_RC delete_rc=$T8_RC2)"
fi
unset FAKE_STATE_DIR EVIDENCE_DIR TEST_NAMESPACE RUN_ID SCENARIO_RUN_ID

echo "----------------------------------------"
echo "CONTRACT RESULT: $PASS passed, $FAIL failed"
(( FAIL == 0 ))
