#!/usr/bin/env bash
# ============================================================================
# Offline acceptance battery for active-volume-delete-protection.
#
# Proves, with the fake API store + operator simulation in fixtures/
# (CASE-CONTRACT.md: case workers build their own TEST STUB surface):
# * the block: OBSERVED driver-identity usage (PV Bound + claimRef, PVC
# Bound, VA attached — created spec-only and watched in API state) ⇒
# delete keeps the CR terminating with the finalizer retained and
# surfaces DataInUse on the aggregate; operands/test data preserved
# (happy path, exit 0); the finalizer is removed exactly once, only
# after a clean scan, and the CR is then GC'd;
# * the async contract: the body sends only real kubectl verbs
# (get/create/patch/delete) and observes status + observedGeneration —
# the synthetic reconcile RPC is gone from the body (static proof), and
# an injected one is rejected at PRE-FLIGHT, before any mutation, and
# again by the helper at dispatch time;
# * fail-closed shell: the default result is FAIL; an injected unbound
# variable or unguarded error after the mutation is exit 1 (never 0),
# with restore+cleanup still run on that exit path;
# * ownership: only this run's create-response UIDs are ever deleted —
# stale same-name objects block the run (never adopted), pre-existing
# foreign objects survive untouched, the canonical singleton is a
# precondition (never created here);
# * fail-safe scan errors block with the source-bound reason and report
# evidence + remaining objects (BLOCKED, exit 2);
# * controlled recovery happens only after the usage clears — never via a
# forceDisable or finalizer break set by the case (event-log proof);
# * every WRONG behavior injected into the fake operator makes the case
# FAIL (the negative bites).
#
# The battery seeds the target state (singleton, node, real-foreign
# objects) directly into the fake store — that is pre-existing cluster
# state, not case mutation; zero events exist before the case runs.
#
# Machine rules honored: scratch only under $HOME/.cache/ngxstorage (fresh
# dir per test — nothing is ever deleted or pruned), no live cluster I/O,
# the live cell stays NOT RUN (serial live worker only).
set -u

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CASE_NAME="active-volume-delete-protection"
BASE="$HOME/.cache/ngxstorage/quartet-scenario-offline-tests/$CASE_NAME/$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$BASE"
PASS=0; FAIL=0

# --- helpers ----------------------------------------------------------------

report() { # report <test> <ok:1/0> [detail]
  if [[ "$2" -eq 1 ]]; then
    printf '  OK   %s\n' "$1"; PASS=$((PASS + 1))
  else
    printf '  FAIL %s%s\n' "$1" "${3:+ — $3}"; FAIL=$((FAIL + 1))
  fi
}

run_case() { # run_case <test-name> [env pairs...]
 # defaults first, caller pairs after — env applies left-to-right, so a
 # test may override KUBE_CONTEXT/TEST_NAMESPACE/ALLOW_LIVE
  local name="$1"; shift
  env \
    SCENARIO_SCRATCH_ROOT="$BASE/$name" \
    KUBE_CONTEXT=offline-fake \
    TEST_NAMESPACE=workload-ns \
    RUN_ID="offline-$name" \
    SCENARIO_MAX_POLLS=25 \
    SCENARIO_POLL_INTERVAL=0 \
    "$@" \
    bash "$CASE_DIR/case.sh" >"$BASE/$name.out" 2>&1
  echo "$?"
}

run_case_from() { # run_case_from <run-dir> <test-name> [env pairs...]
  local dir="$1/$CASE_NAME" name="$2"; shift 2
  env \
    SCENARIO_SCRATCH_ROOT="$BASE/$name" \
    KUBE_CONTEXT=offline-fake \
    TEST_NAMESPACE=workload-ns \
    RUN_ID="offline-$name" \
    SCENARIO_MAX_POLLS=25 \
    SCENARIO_POLL_INTERVAL=0 \
    "$@" \
    bash "$dir/case.sh" >"$BASE/$name.out" 2>&1
  echo "$?"
}

evidence_dir() { ls -d "$BASE/$1"/scenario-*/evidence 2>/dev/null | head -1; }
store_dir() { ls -d "$BASE/$1"/scenario-*/store 2>/dev/null | head -1; }
events() { cat "$(store_dir "$1")/_events.jsonl" 2>/dev/null; }

# The stub preflight derives the store path from case name + RUN_ID, which
# run_case pins to "offline-<test>" — the exact path the battery seeds.
target_store() { mkdir -p "$BASE/$1/scenario-$CASE_NAME-offline-$1/store"; printf '%s' "$BASE/$1/scenario-$CASE_NAME-offline-$1/store"; }

# Pre-existing target state, written straight into the store (NOT case
# mutation): the canonical singleton, one node, and real-foreign objects a
# correct run must never touch.
seed_target() { # seed_target <store-dir>
  python3 - "$1" <<'PY'
import json, os, sys, uuid
store, test_ns = sys.argv[1], "workload-ns"

def put(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    json.dump(obj, open(path, "w"), indent=1, sort_keys=True)

put(store + "/cluster/node/test-node.json", {
    "apiVersion": "v1", "kind": "Node",
    "metadata": {"name": "test-node", "uid": "node-uid-test", "resourceVersion": "1"},
})
put(store + "/cluster/quartet/quartet.json", {
    "apiVersion": "quartet.ngxstorage.com/v1alpha1", "kind": "Quartet",
    "metadata": {"name": "quartet",
                 "uid": str(uuid.uuid5(uuid.NAMESPACE_URL, "quartet://singleton-target")),
                 "generation": "1", "resourceVersion": "1"},
    "spec": {"platform": "auto", "logLevel": "info",
             "profiles": {"nfs": {"enabled": True, "forceDisable": False,
                                  "secretRef": {"name": "nfs-csi-ngxstorage-config"},
                                  "image": {"reference": "quay.io/ngxstorage/nfs-csi-ngxstorage"}}}},
})
# real-foreign: a DIFFERENT driver's Bound PV — the gate must ignore it and
# nothing may ever touch it
put(store + "/cluster/persistentvolume/external-foreign-pv.json", {
    "apiVersion": "v1", "kind": "PersistentVolume",
    "metadata": {"name": "external-foreign-pv", "uid": "foreign-pv-uid-1", "resourceVersion": "1"},
    "spec": {"capacity": {"storage": "2Gi"}, "accessModes": ["ReadWriteMany"],
             "csi": {"driver": "other.example/external", "volumeHandle": "other.example/external.abc"},
             "claimRef": {"kind": "PersistentVolumeClaim", "namespace": "external-ns", "name": "external-pvc"}},
    "status": {"phase": "Bound"},
})
# installer-owned RBAC (out-of-band provisioned; NEVER operator-owned):
# the recovery postcondition must verify it survives the owned-set cleanup.
put(store + "/namespaced/ngxstorage-nfs/role/nfs-csi-ngxstorage-leader-election.json", {
    "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
    "metadata": {"name": "nfs-csi-ngxstorage-leader-election", "namespace": "ngxstorage-nfs",
                 "uid": "installer-role-uid-1", "resourceVersion": "1"},
    "rules": [{"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get", "list", "watch"]}],
})
put(store + "/cluster/clusterrole/nfs-csi-ngxstorage-controller.json", {
    "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
    "metadata": {"name": "nfs-csi-ngxstorage-controller",
                 "uid": "installer-clusterrole-uid-1", "resourceVersion": "1"},
    "rules": [{"apiGroups": [""], "resources": ["persistentvolumes"], "verbs": ["get", "list", "watch"]}],
})
put(store + "/namespaced/workload-ns/configmap/preexisting-foreign-cm.json", {
    "apiVersion": "v1", "kind": "ConfigMap",
    "metadata": {"name": "preexisting-foreign-cm", "namespace": test_ns,
                 "uid": "foreign-cm-uid-1", "resourceVersion": "1"},
    "data": {"payload": "pre-existing, not this run's"},
})
PY
}

# Copy the case dir into <run-dir>/<case-name>/ and insert <line> after the
# first line containing <anchor> — a fault injected into the REAL body.
inject_fault() { # inject_fault <run-dir> <anchor> <line>
  local run_dir="$1" anchor="$2" line="$3"
  cp -r "$CASE_DIR" "$run_dir/$CASE_NAME"
  awk -v anchor="$anchor" -v line="$line" \
      '{print} index($0, anchor) {print line}' "$CASE_DIR/case.sh" >"$run_dir/$CASE_NAME/case.sh"
}

# --- battery ----------------------------------------------------------------

echo "== $CASE_NAME offline battery (scratch: $BASE)"

# T1: happy path — observed usage ⇒ blocked termination (finalizer kept,
# aggregate DataInUse), retention, controlled recovery: finalizer
# removed exactly once after a clean scan, CR reaped, PASS.
seed_target "$(target_store T1-happy)"
rc="$(run_case T1-happy)"
ev="$(evidence_dir T1-happy)"
ok=1
[[ $rc -eq 0 ]] || ok=0
grep -q "RESULT=PASS block-reason=DataInUse" "$ev/result.txt" 2>/dev/null || ok=0
grep -q '"finalizer": "quartet.ngxstorage.com/quartet"' "$ev/identity.json" 2>/dev/null || ok=0
grep -q '"quartet_uid"' "$ev/identity.json" 2>/dev/null || ok=0
grep -q '"usage_observed"' "$ev/identity.json" 2>/dev/null || ok=0
grep -q '"marker_canary_sha256"' "$ev/identity.json" 2>/dev/null || ok=0
grep -q '"node_name"' "$ev/identity.json" 2>/dev/null || ok=0
grep -q "source_rev" "$ev/identity.json" 2>/dev/null || ok=0
# force is never an automatic path: no case actor may patch forceDisable,
# and no finalizer may be broken by the case (apply.go:1086-1092, 1258-1277)
events T1-happy | grep '"actor": "case"' | grep -qi "forceDisable" && ok=0
events T1-happy | grep '"actor": "case"' | grep '"op": "patch"' | grep -qi "finalizer" && ok=0
# usage was OBSERVED, not assumed: create manifests carry no pre-set status
mdir="$(dirname "$(store_dir T1-happy)")/manifests"
grep -r '"status"' "$mdir" >/dev/null 2>&1 && ok=0
grep -q '"attacher": "nfs.csi.ngxstorage.com"' "$mdir/data-va.json" 2>/dev/null || ok=0
grep -q '"selector"' "$mdir/foreign-deployment.json" 2>/dev/null || ok=0
grep -q '"template"' "$mdir/foreign-deployment.json" 2>/dev/null || ok=0
# cleanup hygiene: no case-owned litter, real-foreign objects survived, the
# operator never deleted the canary (the CASE's own cleanup removed it)
grep -q '"owner": "case-owned"' "$ev/remaining-objects.json" 2>/dev/null && ok=0
grep -q '"name": "external-foreign-pv".*"owner": "foreign"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
grep -q '"name": "preexisting-foreign-cm".*"owner": "foreign"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
events T1-happy | grep '"op": "delete-owned"' | grep -q "foreign-canary-deployment" && ok=0
events T1-happy | grep '"actor": "case"' | grep '"op": "delete"' | grep -q "foreign-canary-deployment" || ok=0
grep -q "CLEANUP=OK" "$ev/cleanup.txt" 2>/dev/null || ok=0
grep -q "usage OBSERVED in API state" "$ev/run.log" 2>/dev/null || ok=0
grep -q "full runtime-owned set typed-absent" "$ev/run.log" 2>/dev/null || ok=0
# the operator removed the finalizer exactly ONCE, only after clean usage
n_rm_finalizer="$(events T1-happy | grep '"actor": "operator"' | grep -c '"op": "remove-finalizer"')"
[[ "$n_rm_finalizer" == "1" ]] || ok=0
# the CR was fully reaped — nothing of kind quartet remains
grep -q '"kind": "quartet"' "$ev/remaining-objects.json" 2>/dev/null && ok=0
report "T1-happy (observed usage + blocked termination + finalizer once after clean scan + reaped + canary, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T2: fail-safe scan error (network) — DependencyMissing bound to
# apply.go:1252; recovery can't verify ⇒ BLOCKED with evidence +
# remaining objects (apply.go:1247-1259).
seed_target "$(target_store T2-scan-network-error)"
rc="$(run_case T2-scan-network-error FAKE_API_ERROR=pv-list)"
ev="$(evidence_dir T2-scan-network-error)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "RESULT=BLOCKED" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "DependencyMissing" "$ev/run.log" 2>/dev/null || ok=0
grep -q '"kind": "quartet".*"owner": "target"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
grep -q '"name": "nfs-csi-ngxstorage-controller", "namespace": "ngxstorage-nfs", "uid": [^,]*, "owner": "operator-owned"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
grep -q '"owner": "case-owned"' "$ev/remaining-objects.json" 2>/dev/null && ok=0
grep -q "CLEANUP=OK" "$ev/cleanup.txt" 2>/dev/null || ok=0
report "T2-scan-network-error (BLOCKED rc=2, DependencyMissing, CR+operands remain, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T3: fail-safe scan 403 — PermissionDenied bound to apply.go:1253-1254.
seed_target "$(target_store T3-scan-403)"
rc="$(run_case T3-scan-403 FAKE_API_ERROR=pvc-list-403)"
ev="$(evidence_dir T3-scan-403)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "RESULT=BLOCKED" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "PermissionDenied" "$ev/run.log" 2>/dev/null || ok=0
grep -q '"kind": "quartet"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
report "T3-scan-403 (BLOCKED rc=2, PermissionDenied, CR remains, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T4: WRONG early success — operator removes the finalizer while usage is
# present; the CR is GC'd. The case MUST FAIL.
seed_target "$(target_store T4-flaw-premature-finalizer)"
rc="$(run_case T4-flaw-premature-finalizer FAKE_OPERATOR_FLAW=premature-finalizer-removal)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "ASSERT-FAIL.*premature finalizer removal" "$BASE/T4-flaw-premature-finalizer.out" 2>/dev/null || ok=0
report "T4-flaw-premature-finalizer (injected wrong success FAILs the case, rc=$rc)" $ok

# T5: WRONG cleanup — no UID ownership check: foreign canary deleted.
seed_target "$(target_store T5-flaw-unverified-delete)"
rc="$(run_case T5-flaw-unverified-delete FAKE_OPERATOR_FLAW=unverified-delete)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "ASSERT-FAIL.*foreign canary" "$BASE/T5-flaw-unverified-delete.out" 2>/dev/null || ok=0
report "T5-flaw-unverified-delete (UID-ownership violation FAILs the case, rc=$rc)" $ok

# T6: WRONG cleanup — customer data untouched violated: customer test data deleted.
seed_target "$(target_store T6-flaw-data-theft)"
rc="$(run_case T6-flaw-data-theft FAKE_OPERATOR_FLAW=data-theft)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "ASSERT-FAIL.*test data" "$BASE/T6-flaw-data-theft.out" 2>/dev/null || ok=0
report "T6-flaw-data-theft (test-data theft FAILs the case, rc=$rc)" $ok

# T7: live target without a grant — rejected BEFORE any mutation; zero
# objects created (contract "Canlı gate sınırı").
rc="$(run_case T7-live-no-grant KUBE_CONTEXT=prod-live TEST_NAMESPACE=real-ns)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "PREFLIGHT-REJECT" "$BASE/T7-live-no-grant.out" 2>/dev/null || ok=0
store_objs="$(find "$BASE/T7-live-no-grant" -path '*/store/*' -name '*.json' 2>/dev/null | grep -v manifests | wc -l)"
[[ "$store_objs" -eq 0 ]] || ok=0
report "T7-live-no-grant (live mutation rejected before anything, rc=$rc)" $ok

# T8: live target WITH a grant still stays NOT RUN here — the live cell
# belongs to the serial live worker; an isolated namespace is not safety.
rc="$(run_case T8-live-grant-still-not-run KUBE_CONTEXT=prod-live TEST_NAMESPACE=real-ns ALLOW_LIVE=allow-cluster-scoped-fault)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "live cell NOT RUN" "$BASE/T8-live-grant-still-not-run.out" 2>/dev/null || ok=0
report "T8-live-grant-still-not-run (BLOCKED rc=2, NOT RUN, rc=$rc)" $ok

# T9: no silent default target — missing TEST_NAMESPACE rejects first.
rc="$(env SCENARIO_SCRATCH_ROOT="$BASE/T9-no-default" KUBE_CONTEXT=offline-fake TEST_NAMESPACE= RUN_ID=offline-T9 bash "$CASE_DIR/case.sh" >"$BASE/T9-no-default.out" 2>&1; echo $?)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "no silent default target" "$BASE/T9-no-default.out" 2>/dev/null || ok=0
report "T9-no-default-target (BLOCKED rc=2, rc=$rc)" $ok

# T10: unknown argument rejected before anything.
rc="$(bash "$CASE_DIR/case.sh" --bogus >"$BASE/T10-unknown-arg.out" 2>&1; echo $?)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "CASE-REJECT" "$BASE/T10-unknown-arg.out" 2>/dev/null || ok=0
report "T10-unknown-argument (BLOCKED rc=2, rc=$rc)" $ok

# T11: stale same-name object NOT owned by this run — the case must BLOCK,
# never adopt, and leave the stale object byte-identical.
seed_target "$(target_store T11-stale-collision)"
python3 - "$(store_dir T11-stale-collision)" <<'PY'
import json, os, sys
store = sys.argv[1]
stale = {"apiVersion": "v1", "kind": "PersistentVolume",
         "metadata": {"name": "scenario-data-pv", "uid": "stale-uid-1", "resourceVersion": "1",
                      "labels": {"quartet-scenario.test/case": "active-volume-delete-protection",
                                 "quartet-scenario.test/run": "offline-crashed-earlier"}},
         "spec": {"capacity": {"storage": "1Gi"}, "accessModes": ["ReadWriteOnce"],
                  "csi": {"driver": "nfs.csi.ngxstorage.com",
                          "volumeHandle": "nfs.csi.ngxstorage.com.scenario-data-pv"}},
         "status": {"phase": "Bound"}}
d = os.path.join(store, "cluster", "persistentvolume")
os.makedirs(d, exist_ok=True)
json.dump(stale, open(os.path.join(d, "scenario-data-pv.json"), "w"))
PY
rc="$(run_case T11-stale-collision)"
ev="$(evidence_dir T11-stale-collision)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "RESULT=BLOCKED" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "refusing to adopt" "$BASE/T11-stale-collision.out" 2>/dev/null || ok=0
grep -q '"uid": "stale-uid-1"' "$(store_dir T11-stale-collision)/cluster/persistentvolume/scenario-data-pv.json" 2>/dev/null || ok=0
find "$(store_dir T11-stale-collision)" -name 'scenario-data-pvc.json' | grep -q . && ok=0
find "$(store_dir T11-stale-collision)" -name 'foreign-canary-*.json' | grep -q . && ok=0
events T11-stale-collision | grep '"actor": "case"' | grep -q . && ok=0
report "T11-stale-collision (unowned same-name object BLOCKs, never adopted, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T12: static proof — the body sends only real kubectl verbs; the synthetic
# reconcile RPC is gone from the case source, and the fake adapter
# refuses the verb behaviorally (no handler, nonzero rc).
verbs="$(grep -E 'scenario_kube[[:space:]]+[a-z][a-z-]*' "$CASE_DIR/case.sh" \
  | grep -vE '^[[:space:]]*#' | grep -oE 'scenario_kube[[:space:]]+[a-z][a-z-]*' | awk '{print $2}' | sort -u)"
ok=1
while IFS= read -r v; do
  [[ -n "$v" ]] || continue
  case " get create patch delete " in
    *" $v "*) ;;
    *) ok=0 ;;
  esac
done <<<"$verbs"
grep -qE '^[[:space:]]*scenario_kube reconcile' "$CASE_DIR/case.sh" && ok=0
fake_probe_store="$(target_store T12-static-verb-surface)"
probe_rc=0
python3 "$CASE_DIR/fixtures/fake_kubectl.py" --store "$fake_probe_store" reconcile --max-passes 1 \
  >"$BASE/T12-static-verb-surface.out" 2>&1 || probe_rc=$?
[[ $probe_rc -ne 0 ]] || ok=0
grep -q "unsupported verb" "$BASE/T12-static-verb-surface.out" || ok=0
report "T12-static-verb-surface (body verbs = create/delete/get/patch only)" $ok "verbs=[$(echo $verbs | tr '\n' ' ')]"

# T13: the injected reconcile verb is detected at PRE-FLIGHT — before any
# mutation — on the REAL case shell (fault injected into a copy).
run_dir="$BASE/T13-injected-reconcile/run"
mkdir -p "$run_dir"
seed_target "$(target_store T13-injected-reconcile)"
before="$(sha256sum "$(store_dir T13-injected-reconcile)/cluster/quartet/quartet.json")"
inject_fault "$run_dir" 'ev "pre-flight verb surface verified' '  scenario_kube reconcile --max-passes 1'
rc="$(run_case_from "$run_dir" T13-injected-reconcile)"
ev="$(evidence_dir T13-injected-reconcile)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "PREFLIGHT-VERB-REJECT" "$BASE/T13-injected-reconcile.out" 2>/dev/null || ok=0
grep -q "ASSERT-FAIL.*PREFLIGHT-VERB-REJECT" "$ev/result.txt" 2>/dev/null || ok=0
find "$(store_dir T13-injected-reconcile)" -name 'scenario-data-*.json' | grep -q . && ok=0
find "$(store_dir T13-injected-reconcile)" -name 'foreign-canary-*.json' | grep -q . && ok=0
[[ "$(sha256sum "$(store_dir T13-injected-reconcile)/cluster/quartet/quartet.json")" == "$before" ]] || ok=0
report "T13-injected-reconcile (unsupported verb rejected at pre-flight, zero mutation, rc=$rc)" $ok

# T14: the helper's runtime guard — even a call that bypasses the self-scan
# is refused BEFORE dispatch, with the store untouched.
run_dir="$BASE/T14-runtime-verb-guard"
mkdir -p "$run_dir"
seed_target "$(target_store T14-runtime-verb-guard)"
before="$(find "$(store_dir T14-runtime-verb-guard)" -type f | sort | xargs sha256sum | sha256sum)"
env SCENARIO_SCRATCH_ROOT="$BASE/T14-runtime-verb-guard" KUBE_CONTEXT=offline-fake TEST_NAMESPACE=workload-ns \
  bash -c '
    source "'"$CASE_DIR"'/fixtures/offline_helpers.sh"
    export SCENARIO_FAKE_STORE="'"$(store_dir T14-runtime-verb-guard)"'"
    scenario_kube reconcile --max-passes 1
  ' >"$BASE/T14-runtime-verb-guard.out" 2>&1
rc=$?
after="$(find "$(store_dir T14-runtime-verb-guard)" -type f | sort | xargs sha256sum | sha256sum)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "unsupported kubectl verb" "$BASE/T14-runtime-verb-guard.out" 2>/dev/null || ok=0
[[ "$before" == "$after" ]] || ok=0
report "T14-runtime-verb-guard (helper refuses dispatch of a non-kubectl verb: refusal rc=2, store untouched, rc=$rc)" $ok

# T15: unbound variable injected AFTER the mutation — fail-closed: exit 1
# (never 0), result FAIL, and restore+cleanup still ran (pin 6/8).
run_dir="$BASE/T15-unbound-after-mutation/run"
mkdir -p "$run_dir"
seed_target "$(target_store T15-unbound-after-mutation)"
inject_fault "$run_dir" 'ev "seeded: usage' 'echo "$UNBOUND_INJECTION_XYZ"'
rc="$(run_case_from "$run_dir" T15-unbound-after-mutation)"
ev="$(evidence_dir T15-unbound-after-mutation)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "UNBOUND_INJECTION_XYZ" "$BASE/T15-unbound-after-mutation.out" 2>/dev/null || ok=0
grep -q "RESULT=FAIL" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
find "$(store_dir T15-unbound-after-mutation)" -name 'scenario-data-*.json' | grep -q . && ok=0
find "$(store_dir T15-unbound-after-mutation)" -name 'foreign-canary-*.json' | grep -q . && ok=0
grep -q "CLEANUP=OK" "$ev/cleanup.txt" 2>/dev/null || ok=0
report "T15-unbound-after-mutation (unexpected unbound var → FAIL not PASS, cleanup ran, rc=$rc)" $ok

# T16: unguarded shell error injected after the mutation — same contract.
run_dir="$BASE/T16-unguarded-error/run"
mkdir -p "$run_dir"
seed_target "$(target_store T16-unguarded-error)"
inject_fault "$run_dir" 'ev "seeded: usage' 'false'
rc="$(run_case_from "$run_dir" T16-unguarded-error)"
ev="$(evidence_dir T16-unguarded-error)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "RESULT=FAIL" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
find "$(store_dir T16-unguarded-error)" -name 'scenario-data-*.json' | grep -q . && ok=0
grep -q "CLEANUP=OK" "$ev/cleanup.txt" 2>/dev/null || ok=0
report "T16-unguarded-error (unexpected shell error → FAIL not PASS, cleanup ran, rc=$rc)" $ok

# T17: default-FAIL proof — the explicit final finish removed: the run
# completes every step but still exits 1 with a fail-closed result line.
run_dir="$BASE/T17-default-fail-tail/run"
mkdir -p "$run_dir"
seed_target "$(target_store T17-default-fail-tail)"
cp -r "$CASE_DIR" "$run_dir/$CASE_NAME"
grep -v '^finish $RC_PASS' "$CASE_DIR/case.sh" >"$run_dir/$CASE_NAME/case.sh"
rc="$(run_case_from "$run_dir" T17-default-fail-tail)"
ev="$(evidence_dir T17-default-fail-tail)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "RESULT=FAIL unexpected exit" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
find "$(store_dir T17-default-fail-tail)" -name 'foreign-canary-*.json' | grep -q . && ok=0
grep -q "CLEANUP=OK" "$ev/cleanup.txt" 2>/dev/null || ok=0
report "T17-default-fail-tail (rc0 only on explicit successful completion, rc=$rc)" $ok

# T18: the pinned atomic cleanup surface on this fixture (CASE-CONTRACT
# "Ortak atomik cleanup helper"): create binds the response UID
# (stdout + ownership.tsv CREATE row), a swapped-UID object is refused
# (rc 2) and survives, a registered UID deletes + postcondition-
# verifies, absence is idempotent, namespace misuse registers nothing.
seed_target "$(target_store T18-atomic-surface)"
rc18="$(env SCENARIO_SCRATCH_ROOT="$BASE/T18-atomic-surface" KUBE_CONTEXT=offline-fake TEST_NAMESPACE=workload-ns \
  EVIDENCE_DIR="$BASE/T18-atomic-surface/evidence" SCENARIO_FAKE_STORE="$(store_dir T18-atomic-surface)" RUN_ID="offline-T18" \
  bash -c '
    set -u
    source "$1/fixtures/offline_helpers.sh"
    source "$1/fixtures/scenario_atomic_cleanup.sh"
    mkdir -p "$EVIDENCE_DIR"; : > "$EVIDENCE_DIR/ownership.tsv"
    reg="$EVIDENCE_DIR/ownership.tsv"
    printf "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"t18-cm\",\"namespace\":\"workload-ns\"},\"data\":{\"p\":\"owned\"}}" > "$EVIDENCE_DIR/t18-cm.json"
    rc=0; uid1="$(scenario_create_tracked configmap t18-cm --namespace workload-ns "$EVIDENCE_DIR/t18-cm.json")" || rc=$?
    [[ "$rc" == "0" && -n "$uid1" ]] || exit 10
    stored="$(python3 -c "import json; print(json.load(open(\"$SCENARIO_FAKE_STORE/namespaced/workload-ns/configmap/t18-cm.json\"))[\"metadata\"][\"uid\"])")"
    [[ "$uid1" == "$stored" ]] || exit 11
    grep -qF "$(printf "CREATE\tconfigmap\tworkload-ns\tt18-cm\t%s\tcreated" "$uid1")" "$reg" || exit 12
    python3 - "$SCENARIO_FAKE_STORE/namespaced/workload-ns/configmap/t18-cm.json" <<PY
import json, sys
d = json.load(open(sys.argv[1]))
d["metadata"]["uid"] = "t18-swapped-uid"
json.dump(d, open(sys.argv[1], "w"), indent=1, sort_keys=True)
PY
    rc=0; scenario_delete_owned configmap t18-cm --namespace workload-ns "$uid1" >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "2" ]] || exit 13
    grep -q "t18-swapped-uid" "$SCENARIO_FAKE_STORE/namespaced/workload-ns/configmap/t18-cm.json" || exit 14
    grep -qF "$(printf "DELETE\tconfigmap\tworkload-ns\tt18-cm\t%s\trefused-uid-mismatch" "$uid1")" "$reg" || exit 15
    printf "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"t18-cm2\",\"namespace\":\"workload-ns\"},\"data\":{\"p\":\"owned\"}}" > "$EVIDENCE_DIR/t18-cm2.json"
    rc=0; uid2="$(scenario_create_tracked configmap t18-cm2 --namespace workload-ns "$EVIDENCE_DIR/t18-cm2.json")" || rc=$?
    [[ "$rc" == "0" ]] || exit 16
    rc=0; scenario_delete_owned configmap t18-cm2 --namespace workload-ns "$uid2" || rc=$?
    [[ "$rc" == "0" ]] || exit 17
    [[ ! -f "$SCENARIO_FAKE_STORE/namespaced/workload-ns/configmap/t18-cm2.json" ]] || exit 18
    grep -qF "$(printf "DELETE\tconfigmap\tworkload-ns\tt18-cm2\t%s\tdeleted" "$uid2")" "$reg" || exit 19
    rc=0; scenario_delete_owned configmap t18-cm2 --namespace workload-ns "$uid2" >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "0" ]] || exit 20
    grep -qF "$(printf "DELETE\tconfigmap\tworkload-ns\tt18-cm2\t%s\tabsent" "$uid2")" "$reg" || exit 21
    rc=0; scenario_create_tracked configmap t18-cm3 "$EVIDENCE_DIR/t18-cm.json" >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "2" ]] || exit 22
    [[ ! -f "$SCENARIO_FAKE_STORE/namespaced/workload-ns/configmap/t18-cm3.json" ]] || exit 23
  ' _ "$CASE_DIR")"
ok=1
[[ "$rc18" -eq 0 ]] || ok=0
report "T18-atomic-surface (pinned helpers: response-UID binding, swap refusal rc=2, postcondition delete, absence idempotency, ns-misuse refusal)" $ok "subrc=$rc18"

# T19: postcondition negative — delete ACCEPTED but the object never goes
# away (sabotaged fake): the deletion is UNVERIFIED and never claimed —
# rc 2 (refusal), ownership.tsv records postcondition-failed.
run_dir="$BASE/T19-postcondition/run"
mkdir -p "$run_dir/fixtures"
cp "$CASE_DIR/fixtures/offline_helpers.sh" "$CASE_DIR/fixtures/scenario_atomic_cleanup.sh" "$run_dir/fixtures/"
sed '0,/os.replace(p, p + ".removed")/s//pass  # sabotaged: delete accepted but the object is never removed/' \
  "$CASE_DIR/fixtures/fake_kubectl.py" > "$run_dir/fixtures/fake_kubectl.py"
grep -q "sabotaged" "$run_dir/fixtures/fake_kubectl.py" || { echo "T19 sabotage not applied — test setup bug"; exit 1; }
seed_target "$(target_store T19-postcondition)"
rc19="$(env SCENARIO_SCRATCH_ROOT="$BASE/T19-postcondition" KUBE_CONTEXT=offline-fake TEST_NAMESPACE=workload-ns \
  EVIDENCE_DIR="$BASE/T19-postcondition/evidence" SCENARIO_FAKE_STORE="$(store_dir T19-postcondition)" RUN_ID="offline-T19" \
  bash -c '
    set -u
    source "$1/fixtures/offline_helpers.sh"
    source "$1/fixtures/scenario_atomic_cleanup.sh"
    mkdir -p "$EVIDENCE_DIR"; : > "$EVIDENCE_DIR/ownership.tsv"
    printf "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"t19-cm\",\"namespace\":\"workload-ns\"},\"data\":{\"p\":\"owned\"}}" > "$EVIDENCE_DIR/t19-cm.json"
    rc=0; uid1="$(scenario_create_tracked configmap t19-cm --namespace workload-ns "$EVIDENCE_DIR/t19-cm.json")" || rc=$?
    [[ "$rc" == "0" ]] || exit 10
    rc=0; scenario_delete_owned configmap t19-cm --namespace workload-ns "$uid1" >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "2" ]] || exit 11
    grep -qF "$(printf "DELETE\tconfigmap\tworkload-ns\tt19-cm\t%s\tpostcondition-failed" "$uid1")" "$EVIDENCE_DIR/ownership.tsv" || exit 12
    [[ -f "$SCENARIO_FAKE_STORE/namespaced/workload-ns/configmap/t19-cm.json" ]] || exit 13
  ' _ "$run_dir")"
ok=1
[[ "$rc19" -eq 0 ]] || ok=0
report "T19-postcondition (delete accepted but object never removed: UNVERIFIED = refusal rc 2, never claimed)" $ok "subrc=$rc19"

# T20: gate refusal — a call the scenario surface refuses (store outside the
# scratch root) is rc 2 for BOTH helpers: nothing registered, nothing
# deleted, never a success.
rc20="$(env SCENARIO_SCRATCH_ROOT="$BASE/T20-refusal" KUBE_CONTEXT=offline-fake TEST_NAMESPACE=workload-ns \
  EVIDENCE_DIR="$BASE/T20-refusal/evidence" RUN_ID="offline-T20" \
  bash -c '
    set -u
    source "$1/fixtures/offline_helpers.sh"
    source "$1/fixtures/scenario_atomic_cleanup.sh"
    mkdir -p "$EVIDENCE_DIR"; : > "$EVIDENCE_DIR/ownership.tsv"
    export SCENARIO_FAKE_STORE="/etc/scenario-store-outside-scratch"
    printf "{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\",\"metadata\":{\"name\":\"t20-cm\",\"namespace\":\"workload-ns\"},\"data\":{\"p\":\"owned\"}}" > "$EVIDENCE_DIR/t20-cm.json"
    rc=0; scenario_create_tracked configmap t20-cm --namespace workload-ns "$EVIDENCE_DIR/t20-cm.json" >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "2" ]] || exit 10
    rc=0; scenario_delete_owned configmap t20-cm --namespace workload-ns t20-uid >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "2" ]] || exit 11
    [[ "$(wc -l < "$EVIDENCE_DIR/ownership.tsv")" == "0" ]] || exit 12
  ' _ "$CASE_DIR")"
ok=1
[[ "$rc20" -eq 0 ]] || ok=0
report "T20-gate-refusal (refusal rc 2 registers nothing and deletes nothing — never a success)" $ok "subrc=$rc20"

# --- summary -----------------------------------------------------------------

# T21: E2BIG file transport — a single cluster-wide list JSON >=1MiB must go
# through the FILE transport (real subprocess exec); evidence completes and
# the gate assertion is reached. Env/argv transport would E2BIG here.
seed_target "$(target_store T21-e2big)"
python3 - "$(target_store T21-e2big)" <<'PY'
import json, os, sys
store = sys.argv[1]
os.makedirs(store + "/cluster/node", exist_ok=True)
pad = "x" * 900
for i in range(1400):
    json.dump({"apiVersion": "v1", "kind": "Node",
               "metadata": {"name": "bulk-node-%04d" % i, "uid": "bulk-uid-%04d" % i,
                            "resourceVersion": "1", "annotations": {"pad": pad}},
               "status": {"conditions": [{"type": "Ready", "status": "True"}]}},
              open(store + "/cluster/node/bulk-node-%04d.json" % i, "w"))
PY
rc="$(run_case T21-e2big)"
ev="$(evidence_dir T21-e2big)"
ok=1
[[ $rc -eq 0 ]] || ok=0
grep -q "RESULT=PASS block-reason=DataInUse" "$ev/result.txt" 2>/dev/null || ok=0
grep -q '"kind": "node"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
sz=$(wc -c <"$ev/lists/node.json" 2>/dev/null || echo 0)
[[ $sz -ge 1048576 ]] || ok=0
grep -q '"error"' "$ev/remaining-objects.json" 2>/dev/null && ok=0
report "T21-e2big (>=1MiB node list via file transport, evidence complete, gate reached, rc=$rc, list=${sz}B)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T22: evidence GET failure — a failed list GET during evidence collection
# must surface as an explicit error entry, never a silent empty list/PASS.
seed_target "$(target_store T22-evid-getfail)"
rd22="$BASE/T22-evid-getfail-run"; mkdir -p "$rd22"
inject_fault "$rd22" 'kube_get_json() {' 'kube_get_json() { case " $* " in *" lease "*) return 1;; esac; scenario_kube get "$@" -o json 2>/dev/null; }'
rc="$(run_case_from "$rd22" T22-evid-getfail)"
ev="$(evidence_dir T22-evid-getfail)"
ok=1
[[ $rc -eq 0 ]] || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null || ok=0
grep -q '"kind": "lease".*"error": "GET_FAILED_RC_' "$ev/remaining-objects.json" 2>/dev/null || ok=0
grep -q '"kind": "persistentvolume"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
report "T22-evid-getfail (lease GET rc1 -> explicit error entry, other kinds collected, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T23: malformed list JSON — parse failure recorded, never silent empty.
seed_target "$(target_store T23-evid-badjson)"
rd23="$BASE/T23-evid-badjson-run"; mkdir -p "$rd23"
inject_fault "$rd23" 'kube_get_json() {' 'kube_get_json() { case " $* " in *" lease "*) echo "{malformed"; return 0;; esac; scenario_kube get "$@" -o json 2>/dev/null; }'
rc="$(run_case_from "$rd23" T23-evid-badjson)"
ev="$(evidence_dir T23-evid-badjson)"
ok=1
[[ $rc -eq 0 ]] || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null || ok=0
grep -q '"kind": "lease".*"error": "LIST_PARSE_FAIL"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
grep -q '"kind": "persistentvolume"' "$ev/remaining-objects.json" 2>/dev/null || ok=0
report "T23-evid-badjson (malformed lease JSON -> LIST_PARSE_FAIL entry, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"


# --- recovery-postcondition reliability injections (review acceptance) ------
# API error on an absence GET must NEVER produce PASS; leftover owned objects
# must FAIL; real NotFound positive control is T1 (full-set assertion below).

# inject a kube_absent redefinition that simulates an API-error classification
# for one target kind (the GET itself would return 403/5xx/timeout).
inject_absence_fault() { # <run-dir> <match-var> <stderr-text>
  local rd="$1" mvar="$2" etext="$3"
  python3 - "$rd/$CASE_NAME/case.sh" "$mvar" "$etext" <<'PY'
import sys
p, mvar, etext = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(p).read()
anchor = "# --- typed recovery postconditions (API error is NEVER absence) ------------"
fault = (
    "kube_absent() { local errf=\"$EVIDENCE_DIR/absent-check.err\" rc=0 reason; "
    "case \"$*\" in *\"" + "$" + mvar + "\"*) "
    "printf '%s\\n' '" + etext + "' >\"$errf\"; return 3;; esac; "
    "scenario_kube get \"$@\" >/dev/null 2>\"$errf\" </dev/null || rc=$?; "
    "if [[ $rc -eq 0 ]]; then return 1; fi; "
    "if [[ $rc -eq 2 ]]; then return 3; fi; "
    "reason=\"$(sed -nE 's/.*Error from server \\(([A-Za-z]+)\\).*/\\1/p' \"$errf\" 2>/dev/null | head -n 1)\"; "
    "if [[ \"$reason\" == \"NotFound\" ]]; then return 0; fi; return 3; }"
)
assert anchor in s
s = s.replace(anchor, anchor + "\n" + fault, 1)
open(p, "w").write(s)
PY
}

# make the copied fake operator skip deleting one kind (leftover simulation)
inject_fake_skip() { # <run-dir> <kind>
  python3 - "$1/$CASE_NAME/fixtures/fake_kubectl.py" "$2" <<'PY'
import sys
p, kind = sys.argv[1], sys.argv[2]
s = open(p).read()
old = "    for kind, cluster in OPERAND_KINDS:\n        ns = None if cluster else OPERAND_NS"
new = "    for kind, cluster in OPERAND_KINDS:\n        if kind == %r: continue\n        ns = None if cluster else OPERAND_NS" % kind
assert old in s
open(p, "w").write(s.replace(old, new, 1))
PY
}

# seed an operator-owned object that the (faulted) operator will leave behind
seed_owned_leftover() { # <store-dir> <kind> <name>
  python3 - "$1" "$2" "$3" <<'PY'
import json, os, sys, uuid
store, kind, name = sys.argv[1], sys.argv[2], sys.argv[3]
owner = str(uuid.uuid5(uuid.NAMESPACE_URL, "quartet://singleton-target"))
ns = "ngxstorage-nfs"
obj = {"apiVersion": "apps/v1", "kind": kind,
       "metadata": {"name": name, "namespace": ns,
                    "uid": "leftover-%s-uid" % kind, "resourceVersion": "1",
                    "annotations": {"quartet.ngxstorage.com/owner-uid": owner}}}
if kind == "Pod":
    obj["apiVersion"] = "v1"
    obj["spec"] = {"containers": [{"name": "c", "image": "busybox"}]}
    obj["status"] = {"phase": "Running"}
else:
    obj["spec"] = {"selector": {"matchLabels": {"app": "x"}},
                   "template": {"metadata": {"labels": {"app": "x"}},
                                "spec": {"containers": [{"name": "c", "image": "busybox"}]}}}
d = store + "/namespaced/" + ns + "/" + kind.lower()
os.makedirs(d, exist_ok=True)
json.dump(obj, open(d + "/" + name + ".json", "w"), indent=1, sort_keys=True)
PY
}

# T24: recovery Deployment absence GET -> 403 => BLOCKED, never PASS
seed_target "$(target_store T24-recovery-deploy-403)"
rd24="$BASE/T24-run"; mkdir -p "$rd24"
inject_fault "$rd24" '# --- typed recovery postconditions (API error is NEVER absence) ------------' '# (fault marker)'
inject_absence_fault "$rd24" DEPLOYMENT_NAME 'Error from server (Forbidden): deployments.apps "nfs-csi-ngxstorage-controller" is forbidden (injected)'
rc="$(run_case_from "$rd24" T24-recovery-deploy-403)"
ev="$(evidence_dir T24-recovery-deploy-403)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "RESULT=BLOCKED" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
grep -qi "API error" "$ev/result.txt" 2>/dev/null || ok=0
report "T24-recovery-deploy-403 (absence GET 403 -> BLOCKED, no PASS, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T25: recovery CSIDriver absence GET -> 5xx => BLOCKED, never PASS
seed_target "$(target_store T25-recovery-csidriver-5xx)"
rd25="$BASE/T25-run"; mkdir -p "$rd25"
inject_fault "$rd25" '# --- typed recovery postconditions (API error is NEVER absence) ------------' '# (fault marker)'
inject_absence_fault "$rd25" CSIDRIVER_NAME 'Error from server (InternalError): an error on the server (injected)'
rc="$(run_case_from "$rd25" T25-recovery-csidriver-5xx)"
ev="$(evidence_dir T25-recovery-csidriver-5xx)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "RESULT=BLOCKED" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
report "T25-recovery-csidriver-5xx (absence GET 5xx -> BLOCKED, no PASS, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T25b: recovery Deployment absence GET -> timeout => BLOCKED, never PASS
seed_target "$(target_store T25b-recovery-deploy-timeout)"
rd25b="$BASE/T25b-run"; mkdir -p "$rd25b"
inject_fault "$rd25b" '# --- typed recovery postconditions (API error is NEVER absence) ------------' '# (fault marker)'
inject_absence_fault "$rd25b" DEPLOYMENT_NAME 'Unable to connect to the server: dial tcp: i/o timeout (injected)'
rc="$(run_case_from "$rd25b" T25b-recovery-deploy-timeout)"
ev="$(evidence_dir T25b-recovery-deploy-timeout)"
ok=1
[[ $rc -eq 2 ]] || ok=0
grep -q "RESULT=BLOCKED" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
report "T25b-recovery-deploy-timeout (absence GET timeout -> BLOCKED, no PASS, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T26: leftover owned DaemonSet => FAIL (full-set predicate, not just Deploy)
seed_target "$(target_store T26-leftover-ds)"
seed_owned_leftover "$(target_store T26-leftover-ds)" DaemonSet nfs-csi-ngxstorage-node
rd26="$BASE/T26-run"; mkdir -p "$rd26"
inject_fault "$rd26" '# --- typed recovery postconditions (API error is NEVER absence) ------------' '# (fault marker)'
inject_fake_skip "$rd26" daemonset
rc="$(run_case_from "$rd26" T26-leftover-ds)"
ev="$(evidence_dir T26-leftover-ds)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "DaemonSet survived" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
report "T26-leftover-ds (owned DS survives -> FAIL, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

# T27: leftover owned pod => FAIL (pod-list predicate)
seed_target "$(target_store T27-leftover-pod)"
seed_owned_leftover "$(target_store T27-leftover-pod)" Pod leftover-owned-pod
rd27="$BASE/T27-run"; mkdir -p "$rd27"
inject_fault "$rd27" '# --- typed recovery postconditions (API error is NEVER absence) ------------' '# (fault marker)'
inject_fake_skip "$rd27" pod
rc="$(run_case_from "$rd27" T27-leftover-pod)"
ev="$(evidence_dir T27-leftover-pod)"
ok=1
[[ $rc -eq 1 ]] || ok=0
grep -q "owned pod survived" "$ev/result.txt" 2>/dev/null || ok=0
grep -q "RESULT=PASS" "$ev/result.txt" 2>/dev/null && ok=0
report "T27-leftover-pod (owned pod survives -> FAIL, rc=$rc)" $ok "$(head -1 "$ev/result.txt" 2>/dev/null)"

echo "== summary: PASS=$PASS FAIL=$FAIL (out/evidence under $BASE)"
[[ $FAIL -eq 0 ]]
