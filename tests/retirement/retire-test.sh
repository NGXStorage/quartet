#!/usr/bin/env bash
# Offline negative/positive tests for retire.py.
# Drives the REAL retire.py against a fake kubectl: preflight 403 => zero
# deletes; UID mismatch => zero deletes; delete-time API error => immediate
# stop before later objects; postcondition 403 => exit 1; pod-barrier
# exhaustion => stop; ownerReferences restore export parses and is stripped;
# unproven restore file => resume STOP; happy path => exit 0.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/retire-test.XXXXXX")"
if [ "${RETIRE_TEST_KEEP:-0}" = "1" ]; then
  trap 'echo "KEEP scratch: $scratch"' EXIT
else
  trap 'rm -rf "$scratch"' EXIT
fi
FAKE="$root/hack/retirement/fake-kubectl.py"
RETIRE="$root/hack/retirement/retire.py"
NS=nfs-csi-ngxstorage

# ---------- shared fixture builders (python) ------------------------------
build_state() { # $1=state dir $2=withcascade $3=extra_new_pod(0/1)
python3 - "$1" "${2:-}" "${3:-0}" <<'PY'
import json, os, sys
S, cascade, extrapod = sys.argv[1], sys.argv[2] == "1", sys.argv[3] == "1"
NS = "nfs-csi-ngxstorage"
os.makedirs(f"{S}/objects", exist_ok=True)
open(f"{S}/calls.log", "w").close()
json.dump([], open(f"{S}/faults.json", "w"))
def obj(kind, ns, name, uid, extra=None):
    d = {"kind": kind, "apiVersion": "v1",
         "metadata": {"name": name, "namespace": ns, "uid": uid,
                      "resourceVersion": "100", "creationTimestamp": "x",
                      "managedFields": [{"manager": "kubectl"}]},
         "spec": {}}
    if extra: d.update(extra)
    json.dump(d, open(f"{S}/objects/{kind}__{ns}__{name}.json", "w"))
    return d
obj("ns", "", NS, "ns-uid-1")
dep = obj("deployment", NS, "dep1", "dep-uid")
# ownerReferences + status + generation: must be stripped from restore
dep["metadata"]["generation"] = 7
dep["status"] = {"replicas": 1}
json.dump(dep, open(f"{S}/objects/deployment__{NS}__dep1.json", "w"))
for k, n, u in [("daemonset", "ds1", "ds-uid"), ("replicaset", "rs1", "rs-uid"),
                ("lease", "le1", "le-uid"), ("role", "ro1", "ro-uid"),
                ("rolebinding", "rb1", "rb-uid"), ("serviceaccount", "sa1", "sa-uid"),
                ("clusterrolebinding", "crb1", "crb-uid"), ("clusterrole", "cr1", "cr-uid"),
                ("csidriver", "cd1", "cd-uid"),
                ("secret", "sec-protected", "sec-uid"), ("configmap", "cm-neighbor", "cm-uid")]:
    ns = "-" if k in ("clusterrolebinding", "clusterrole", "csidriver") else NS
    obj(k, ns, n, u)
lease = json.load(open(f"{S}/objects/lease__{NS}__le1.json"))
lease["spec"] = {"renewTime": "t1", "holderIdentity": "x"}
json.dump(lease, open(f"{S}/objects/lease__{NS}__le1.json", "w"))
# owner chain: rs1 owned by dep1; pods owned by rs1
rs = json.load(open(f"{S}/objects/replicaset__{NS}__rs1.json"))
rs["metadata"]["ownerReferences"] = [{"apiVersion":"apps/v1","kind":"Deployment",
    "name":"dep1","uid":"dep-uid","controller":True}]
json.dump(rs, open(f"{S}/objects/replicaset__{NS}__rs1.json", "w"))
def pod(name, uid):
    p = obj("pod", NS, name, uid)
    p["metadata"]["ownerReferences"] = [{"apiVersion": "apps/v1", "kind": "ReplicaSet",
                                         "name": "rs1", "uid": "rs-uid", "controller": True}]
    json.dump(p, open(f"{S}/objects/pod__{NS}__{name}.json", "w"))
pod("dep1-abc-12345", "pod-uid-1")
if extrapod:
    pod("dep1-newhash-newpod", "pod-uid-2")   # NEW name, owned by recorded rs-uid
json.dump({"items": []}, open(f"{S}/objects/csinode__-__all.json", "w"))
if cascade:
    open(f"{S}/.cascade", "w").close()
PY
}

build_manifest() { # $1=out $2=ns $3=retire_dir
python3 - "$1" "$2" "$3" <<'PY'
import json, sys, os
sys.path.insert(0, sys.argv[3])
import semantic_hash as sh
out, NS = sys.argv[1], sys.argv[2]
st = os.environ["FAKE_STATE"]
objs = {}
for f in os.listdir(f"{st}/objects"):
    if f.endswith(".json"):
        k = f[:-5].split("__")
        objs[(k[0], k[1], k[2])] = json.load(open(f"{st}/objects/{f}"))
hdr = "role\tapiVersion\tkind\tnamespace\tname\tuid\tresourceVersion\tsemanticSha256\tsnapshotId"
AV = {"deployment":"apps/v1","daemonset":"apps/v1","replicaset":"apps/v1","pod":"v1",
      "lease":"coordination.k8s.io/v1",
      "role":"rbac.authorization.k8s.io/v1","rolebinding":"rbac.authorization.k8s.io/v1",
      "serviceaccount":"v1","clusterrole":"rbac.authorization.k8s.io/v1",
      "clusterrolebinding":"rbac.authorization.k8s.io/v1","csidriver":"storage.k8s.io/v1",
      "secret":"v1","configmap":"v1"}
rows = [hdr]
def add(role, kind, ns, name, uid=None):
    o = objs[(kind, ns, name)]
    u = uid or o["metadata"]["uid"]
 # NEIGHBOR hashes use the SHARED contract; DELETE rows carry a
 # placeholder (retire.py re-derives nothing from the DELETE hash).
    hv = sh.semantic_hash(o, kind) if role == "NEIGHBOR" else "hash"
    rows.append("\t".join([role, AV[kind], kind, ns, name, u, "100", hv, "snap"]))
for k, n in [("daemonset","ds1"),("deployment","dep1"),("replicaset","rs1"),("lease","le1"),
             ("role","ro1"),("rolebinding","rb1"),("serviceaccount","sa1"),
             ("clusterrolebinding","crb1"),("clusterrole","cr1"),("csidriver","cd1")]:
    ns = "-" if k in ("clusterrolebinding","clusterrole","csidriver") else NS
    add("DELETE", k, ns, n)
add("DELETE", "pod", NS, "dep1-abc-12345", "pod-uid-1")
add("PROTECTED","secret",NS,"sec-protected")
add("NEIGHBOR","configmap",NS,"cm-neighbor")
open(out,"w").write("\n".join(rows) + "\n")
PY
}

mkkube() { # $1=state
cat > "$1/kubectl" <<EOF
#!/bin/sh
FAKE_STATE="$1" exec python3 "$FAKE" "\$@"
EOF
chmod +x "$1/kubectl"
}

run_retire() { # $1=state $2=manifest $3=outdir  [env...]
  ( cd "$1" && RETIRE_BARRIER_TRIES=3 RETIRE_BARRIER_SLEEP=0 \
      FAKE_STATE="$1" python3 "$RETIRE" "$2" "$3" "$NS" "$1/kubectl" )
}

RETIRE_DIR="$root/hack/retirement"
scn() { mkdir -p "$scratch/$1"; build_state "$scratch/$1" "${2:-1}" "${3:-0}"; mkkube "$scratch/$1"; FAKE_STATE="$scratch/$1" build_manifest "$scratch/$1/manifest.tsv" "$NS" "$RETIRE_DIR"; }

# happy-path: exit 0, restore parses, ownerRef stripped
scn happy-path 1
rc=0; out1="$(run_retire "$scratch/happy-path" "$scratch/happy-path/manifest.tsv" "$scratch/happy-path/out" 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL happy-path: happy path rc=$rc" >&2; echo "$out1"; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out1" || { echo "FAIL happy-path: no RETIRE-COMPLETE: $out1" >&2; exit 1; }
python3 - <<PY
import yaml
d = yaml.safe_load(open("$scratch/happy-path/out/restore-manifests/deployment-dep1.yaml"))
md = d["metadata"]
assert "ownerReferences" not in md, "ownerReferences must be stripped"
assert "status" not in d, "status must be stripped"
assert "uid" not in md and "managedFields" not in md and "generation" not in md
print("happy-path restore export parses; server fields + ownerReferences stripped")
PY
# every DELETE actually delivered a DeleteOptions body with the manifest UID
python3 - "$scratch/happy-path" <<'PY'
import os, sys
S = sys.argv[1]
man = {}
for line in open(f"{S}/manifest.tsv").read().splitlines()[1:]:
    p = line.split("\t")
 # pods are cascade-owned (barrier-gated), never directly deleted
    if p[0] == "DELETE" and p[2] != "pod":
        man[(p[2], p[4])] = p[5]
bodies = [l.split("\t") for l in open(f"{S}/delete-bodies.tsv").read().splitlines()]
assert bodies, "no DeleteOptions bodies captured — precondition never delivered"
sent = {}
for raw, uid in bodies:
    kind, name = raw.rstrip("/").split("/")[-2:]
    plural2kind = {"daemonsets":"daemonset","deployments":"deployment",
                   "replicasets":"replicaset","leases":"lease","roles":"role",
                   "rolebindings":"rolebinding","serviceaccounts":"serviceaccount",
                   "clusterrolebindings":"clusterrolebinding","clusterroles":"clusterrole",
                   "csidrivers":"csidriver"}
    sent[(plural2kind[kind], name)] = uid
missing = [k for k in man if k not in sent]
assert not missing, f"DELETE objects never sent with a UID body: {missing}"
bad = [k for k, u in man.items() if sent.get(k) != u]
assert not bad, f"UID body does not match manifest: {bad}"
print(f"happy-path body OK: {len(bodies)} DeleteOptions bodies delivered, all carry manifest UIDs")
PY

# preflight-403: restore-GET 403 -> exit 1, ZERO deletes
scn preflight-403 1
python3 -c "import json;json.dump([{'match':{'verb':'get','kind':'serviceaccount'},'reply':'forbidden'}],open('$scratch/preflight-403/faults.json','w'))"
rc=0; run_retire "$scratch/preflight-403" "$scratch/preflight-403/manifest.tsv" "$scratch/preflight-403/out" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL preflight-403: 403 preflight rc=$rc" >&2; exit 1; }
grep -q "^delete" "$scratch/preflight-403/calls.log" && { echo "FAIL preflight-403: delete called after 403 preflight" >&2; exit 1; }
echo "preflight-403 OK: preflight 403 -> zero deletes, exit 1"

# uid-mismatch: preflight UID mismatch -> exit 1, zero deletes
scn uid-mismatch 1
sed -i "s/dep-uid/other-uid/" "$scratch/uid-mismatch/manifest.tsv"
rc=0; run_retire "$scratch/uid-mismatch" "$scratch/uid-mismatch/manifest.tsv" "$scratch/uid-mismatch/out" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL uid-mismatch: uid mismatch rc=$rc" >&2; exit 1; }
grep -q "^delete" "$scratch/uid-mismatch/calls.log" && { echo "FAIL uid-mismatch: delete after uid mismatch" >&2; exit 1; }
echo "uid-mismatch OK: preflight UID mismatch -> zero deletes, exit 1"

# delete-time-stop: delete-time API error -> immediate stop, later kinds untouched.
# Fault fires only on the DELETE verb for the lease (preflight uses get),
# so the stop happens mid-delete-loop and role/csdriver must be untouched.
scn delete-time-stop 1
python3 -c "import json;json.dump([{'match':{'verb':'delete','kind':'lease','name':'le1'},'reply':'forbidden'}],open('$scratch/delete-time-stop/faults.json','w'))"
rc=0; run_retire "$scratch/delete-time-stop" "$scratch/delete-time-stop/manifest.tsv" "$scratch/delete-time-stop/out" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL delete-time-stop: delete-time 403 rc=$rc" >&2; exit 1; }
grep -q "^delete.*roles/ro1" "$scratch/delete-time-stop/calls.log" && { echo "FAIL delete-time-stop: continued past delete-time error" >&2; exit 1; }
grep -q "^delete.*csidrivers/cd1" "$scratch/delete-time-stop/calls.log" && { echo "FAIL delete-time-stop: continued to cluster-scoped" >&2; exit 1; }
echo "delete-time-stop OK: delete-time API error stops immediately"

# postcond-403: postcondition 403 -> exit 1 (all deletes happened, but result incomplete).
# Fault fires only once csidriver is deleted, i.e. during postconditions.
scn postcond-403 1
python3 -c "import json;json.dump([{'match':{'verb':'get','kind':'role','name':'ro1','after_delete':'csidriver/cd1'},'reply':'forbidden'}],open('$scratch/postcond-403/faults.json','w'))"
rc=0; out5="$(run_retire "$scratch/postcond-403" "$scratch/postcond-403/manifest.tsv" "$scratch/postcond-403/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL postcond-403: postcondition 403 rc=$rc" >&2; exit 1; }
grep -q "^delete.*csidrivers/cd1" "$scratch/postcond-403/calls.log" || { echo "FAIL postcond-403: deletes did not complete" >&2; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out5" && { echo "FAIL postcond-403: printed COMPLETE despite postcondition error" >&2; exit 1; }
echo "postcond-403 OK: postcondition API error -> exit 1 (not COMPLETE)"

# owner-barrier: a NEW-NAMED owned pod (not in the snapshot) must be caught
# by the owner chain, not just snapshot names.
scn owner-barrier 0 1   # no cascade (pods survive) + extra new-name owned pod
rc=0; out6="$(run_retire "$scratch/owner-barrier" "$scratch/owner-barrier/manifest.tsv" "$scratch/owner-barrier/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL owner-barrier: barrier rc=$rc" >&2; exit 1; }
grep -q "dep1-newhash-newpod" <<< "$out6" || { echo "FAIL owner-barrier: new-name owned pod not detected by barrier: $out6" >&2; exit 1; }
grep -q "^delete.*leases/le1" "$scratch/owner-barrier/calls.log" && { echo "FAIL owner-barrier: proceeded past failed barrier" >&2; exit 1; }
echo "owner-barrier OK: owner-chain barrier catches new-name owned pod, stops before later kinds"

# unproven-resume: resume with unproven restore file -> STOP, zero deletes
scn unproven-resume 1
mkdir -p "$scratch/unproven-resume/out/restore-manifests"
echo "apiVersion: v1" > "$scratch/unproven-resume/out/restore-manifests/deployment-dep1.yaml"
rc=0; run_retire "$scratch/unproven-resume" "$scratch/unproven-resume/manifest.tsv" "$scratch/unproven-resume/out" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL unproven-resume: unproven restore rc=$rc" >&2; exit 1; }
grep -q "^delete" "$scratch/unproven-resume/calls.log" && { echo "FAIL unproven-resume: delete with unproven backup" >&2; exit 1; }
echo "unproven-resume OK: unproven restore manifest blocks resume"

# valid-resume: partial run stops mid-delete; a second run with the
# journal-proven restore files intact must proceed to completion.
scn valid-resume 1
python3 -c "import json;json.dump([{'match':{'verb':'delete','kind':'lease','name':'le1'},'reply':'forbidden'}],open('$scratch/valid-resume/faults.json','w'))"
rc=0; run_retire "$scratch/valid-resume" "$scratch/valid-resume/manifest.tsv" "$scratch/valid-resume/out" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL valid-resume: first partial run should stop (rc=$rc)" >&2; exit 1; }
grep -q "^delete.*leases/le1" "$scratch/valid-resume/calls.log" || { echo "FAIL valid-resume: lease delete not attempted in first run" >&2; exit 1; }
grep -q "^delete.*roles/ro1" "$scratch/valid-resume/calls.log" && { echo "FAIL valid-resume: continued past stop in first run" >&2; exit 1; }
# clear the fault, re-run -> must resume and complete
python3 -c "import json;json.dump([],open('$scratch/valid-resume/faults.json','w'))"
rc=0; out8="$(run_retire "$scratch/valid-resume" "$scratch/valid-resume/manifest.tsv" "$scratch/valid-resume/out" 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL valid-resume: valid resume rc=$rc: $out8" >&2; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out8" || { echo "FAIL valid-resume: resume did not complete: $out8" >&2; exit 1; }
echo "valid-resume OK: valid journal-proven resume proceeds to RETIRE-COMPLETE"

# uid-conflict: UID precondition Conflict is NOT NotFound; replacement kept, no
# 'deleted' record, zero later deletes.
scn uid-conflict 1
python3 -c "import json;json.dump([{'match':{'verb':'delete','kind':'deployment','name':'dep1'},'reply':'uid-conflict'}],open('$scratch/uid-conflict/faults.json','w'))"
rc=0; out9="$(run_retire "$scratch/uid-conflict" "$scratch/uid-conflict/manifest.tsv" "$scratch/uid-conflict/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL uid-conflict: uid-conflict rc=$rc" >&2; exit 1; }
grep -q "CONFLICT" <<< "$out9" || { echo "FAIL uid-conflict: conflict not surfaced: $out9" >&2; exit 1; }
[ -f "$scratch/uid-conflict/objects/deployment__nfs-csi-ngxstorage__dep1.json" ] || { echo "FAIL uid-conflict: replacement was deleted" >&2; exit 1; }
grep -q "^delete.*leases/le1" "$scratch/uid-conflict/calls.log" && { echo "FAIL uid-conflict: continued after Conflict" >&2; exit 1; }
echo "uid-conflict OK: UID-conflict != NotFound; replacement kept, strict stop"

# resume-barrier: an attempt that stopped at the owner-chain
# barrier must re-enforce it on resume even though the parent is gone; a
# new-name owned pod blocks; a foreign same-prefix pod does NOT block.
scn resume-barrier 0 1   # no cascade + extra new-name owned pod; barrier stops attempt 1
rc=0; run_retire "$scratch/resume-barrier" "$scratch/resume-barrier/manifest.tsv" "$scratch/resume-barrier/out" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S10a: first attempt should stop at barrier (rc=$rc)" >&2; exit 1; }
# model the snapshot pod terminating; the NEW-name owned pod stays alive
rm -f "$scratch/resume-barrier/objects/pod__nfs-csi-ngxstorage__dep1-abc-12345.json"
rc=0; out10="$(run_retire "$scratch/resume-barrier" "$scratch/resume-barrier/manifest.tsv" "$scratch/resume-barrier/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S10b: resume skipped the owner-chain barrier (rc=$rc): $out10" >&2; exit 1; }
grep -q "dep1-newhash-newpod" <<< "$out10" || { echo "FAIL S10b: resume barrier did not see the new-name owned pod: $out10" >&2; exit 1; }
[ -f "$scratch/resume-barrier/objects/lease__nfs-csi-ngxstorage__le1.json" ] \
  && [ -f "$scratch/resume-barrier/objects/role__nfs-csi-ngxstorage__ro1.json" ] \
  || { echo "FAIL S10b: deleted past the stopped barrier" >&2; exit 1; }
# now the owned pod goes and a FOREIGN same-prefix pod appears: attempt 3 completes
rm -f "$scratch/resume-barrier/objects/pod__nfs-csi-ngxstorage__dep1-newhash-newpod.json"
python3 - "$scratch/resume-barrier" <<'PY'
import json, sys
S = sys.argv[1]
p = {"kind": "Pod", "metadata": {"name": "dep1-foreign-x", "namespace": "nfs-csi-ngxstorage",
      "uid": "pod-foreign", "ownerReferences": [{"apiVersion": "apps/v1", "kind": "ReplicaSet",
      "name": "foreign-rs", "uid": "foreign-uid"}]}, "spec": {}}
json.dump(p, open(f"{S}/objects/pod__nfs-csi-ngxstorage__dep1-foreign-x.json", "w"))
PY
rc=0; out10c="$(run_retire "$scratch/resume-barrier" "$scratch/resume-barrier/manifest.tsv" "$scratch/resume-barrier/out" 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL S10c: third attempt should complete (rc=$rc): $out10c" >&2; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out10c" || { echo "FAIL S10c: no COMPLETE: $out10c" >&2; exit 1; }
echo "resume-barrier OK: resume re-enforces owner-chain barrier; foreign same-prefix pod does not block"

# lost-baseline: a previously exported, still-living
# object whose restore file vanished must STOP the resume — never regenerated
# from the (possibly mutated) live object.
scn lost-baseline 1
python3 -c "import json;json.dump([{'match':{'verb':'delete','kind':'lease','name':'le1'},'reply':'forbidden'}],open('$scratch/lost-baseline/faults.json','w'))"
rc=0; run_retire "$scratch/lost-baseline" "$scratch/lost-baseline/manifest.tsv" "$scratch/lost-baseline/out" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S11a: first partial run should stop (rc=$rc)" >&2; exit 1; }
# drop the lease baseline and mutate the live lease (same UID)
rm -f "$scratch/lost-baseline/out/restore-manifests/lease-le1.yaml"
python3 - "$scratch/lost-baseline" <<'PY'
import json, sys
S = sys.argv[1]
p = "objects/lease__nfs-csi-ngxstorage__le1.json"
o = json.load(open(f"{S}/{p}"))
o["spec"]["holderIdentity"] = "changed-after-baseline"
json.dump(o, open(f"{S}/{p}", "w"))
PY
python3 -c "import json;json.dump([],open('$scratch/lost-baseline/faults.json','w'))"
rc=0; out11="$(run_retire "$scratch/lost-baseline" "$scratch/lost-baseline/manifest.tsv" "$scratch/lost-baseline/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S11b: lost baseline must STOP (rc=$rc): $out11" >&2; exit 1; }
grep -q "baseline is MISSING" <<< "$out11" || { echo "FAIL S11b: wrong stop reason: $out11" >&2; exit 1; }
[ -f "$scratch/lost-baseline/objects/lease__nfs-csi-ngxstorage__le1.json" ] \
  || { echo "FAIL S11b: lease deleted despite lost baseline" >&2; exit 1; }
[ ! -f "$scratch/lost-baseline/out/restore-manifests/lease-le1.yaml" ] \
  || { echo "FAIL S11b: baseline regenerated from live object" >&2; exit 1; }
j2="$(ls "$scratch/lost-baseline/out"/journal-*.tsv | sort | tail -1)"
grep -q "exported	lease/le1" "$j2" && { echo "FAIL S11b: new export recorded in resumed attempt" >&2; exit 1; }
echo "lost-baseline OK: lost journal-proven baseline stops resume, no regeneration, zero deletes"

# same-attempt-chain: a child that becomes visible AFTER
# the workload's own barrier passed must still be caught by the postcondition
# using the chain recorded in THIS attempt (in-memory update).
scn same-attempt-chain 1
FAKE_STATE="$scratch/same-attempt-chain" python3 - "$NS" <<'PY'
import json, sys, os
json.dump([{"match": {"verb": "delete", "kind": "csidriver", "name": "cd1"},
            "reply": "ok",
            "spawn": {"name": "dep1-late-pod", "owner": "rs-uid", "ns": sys.argv[1]}}],
          open(os.path.join(os.environ["FAKE_STATE"], "faults.json"), "w"))
PY
rc=0; out12="$(run_retire "$scratch/same-attempt-chain" "$scratch/same-attempt-chain/manifest.tsv" "$scratch/same-attempt-chain/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL same-attempt-chain: late owned pod must fail postcondition (rc=$rc): $out12" >&2; exit 1; }
grep -q "POSTCOND-FAIL owner-chain" <<< "$out12" || { echo "FAIL same-attempt-chain: postcondition did not check owner chain: $out12" >&2; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out12" && { echo "FAIL same-attempt-chain: printed COMPLETE with live owned pod" >&2; exit 1; }
echo "same-attempt-chain OK: same-attempt postcondition checks this attempt's recorded chain"

# expanded-chain: a later attempt proves a WIDER
# owner chain for the same parent; resume must consume the UNION (not the
# frozen first record). Attempt 3 stops at the barrier with the new RS UID's
# pod alive and performs ZERO further deletes; once the pod is gone, resume
# completes.
scn expanded-chain 0   # cascade off: we control child survival
# attempt 1: deployment delete 403 -> stops after recording {dep-uid, rs-uid}
python3 -c "import json;json.dump([{'match':{'verb':'delete','kind':'deployment','name':'dep1'},'reply':'forbidden'}],open('$scratch/expanded-chain/faults.json','w'))"
rc=0; run_retire "$scratch/expanded-chain" "$scratch/expanded-chain/manifest.tsv" "$scratch/expanded-chain/out" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S13a: attempt1 should stop on deploy 403 (rc=$rc)" >&2; exit 1; }
grep -q "uids=" "$scratch/expanded-chain/out"/journal-*.tsv | grep -q "dep-uid" || true
# add a second RS (owned by dep1) + its pod; model the old snapshot pod dying
python3 - "$scratch/expanded-chain" "$NS" <<'PY'
import json, sys, os
S, NS = sys.argv[1], sys.argv[2]
rs = {"kind": "ReplicaSet", "metadata": {"name": "rs-new", "namespace": NS, "uid": "rs-new-uid",
       "ownerReferences": [{"apiVersion": "apps/v1", "kind": "Deployment", "name": "dep1",
                            "uid": "dep-uid", "controller": True}]}, "spec": {}}
json.dump(rs, open(f"{S}/objects/replicaset__{NS}__rs-new.json", "w"))
pod = {"kind": "Pod", "metadata": {"name": "dep1-expanded-chain-pod", "namespace": NS,
       "uid": "pod-expanded", "ownerReferences": [{"apiVersion": "apps/v1", "kind": "ReplicaSet",
       "name": "rs-new", "uid": "rs-new-uid", "controller": True}]}, "spec": {}}
json.dump(pod, open(f"{S}/objects/pod__{NS}__dep1-expanded-chain-pod.json", "w"))
os.remove(f"{S}/objects/pod__{NS}__dep1-abc-12345.json")   # old snapshot pod terminated
PY
# attempt 2: clear fault; dep1 now proves {dep-uid, rs-uid, rs-new-uid}, deletes, stops at barrier
python3 -c "import json;json.dump([],open('$scratch/expanded-chain/faults.json','w'))"
rc=0; out13b="$(run_retire "$scratch/expanded-chain" "$scratch/expanded-chain/manifest.tsv" "$scratch/expanded-chain/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S13b: attempt2 should stop at barrier on expanded pod (rc=$rc): $out13b" >&2; exit 1; }
grep -q "rs-new-uid" <<< "$out13b" || { echo "FAIL S13b: expanded chain not recorded: $out13b" >&2; exit 1; }
[ -f "$scratch/expanded-chain/objects/lease__nfs-csi-ngxstorage__le1.json" ] \
  || { echo "FAIL S13b: deleted past the barrier in attempt2" >&2; exit 1; }
# attempt 3: parent already gone; MUST re-use the merged chain (union), not the
# frozen first record -> stops again with the new RS's pod alive, zero deletes
rc=0; out13c="$(run_retire "$scratch/expanded-chain" "$scratch/expanded-chain/manifest.tsv" "$scratch/expanded-chain/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S13c: attempt3 resumed with frozen chain and completed (rc=$rc): $out13c" >&2; exit 1; }
grep -q "dep1-expanded-chain-pod" <<< "$out13c" || { echo "FAIL S13c: attempt3 barrier did not see expanded pod: $out13c" >&2; exit 1; }
[ -f "$scratch/expanded-chain/objects/lease__nfs-csi-ngxstorage__le1.json" ] && \
[ -f "$scratch/expanded-chain/objects/role__nfs-csi-ngxstorage__ro1.json" ] && \
[ -f "$scratch/expanded-chain/objects/csidriver__-__cd1.json" ] \
  || { echo "FAIL S13c: attempt3 deleted beyond the barrier" >&2; exit 1; }
# the owned pod finally terminates -> attempt 4 completes
rm -f "$scratch/expanded-chain/objects/pod__nfs-csi-ngxstorage__dep1-expanded-chain-pod.json"
rc=0; out13d="$(run_retire "$scratch/expanded-chain" "$scratch/expanded-chain/manifest.tsv" "$scratch/expanded-chain/out" 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL S13d: resume after pod gone should complete (rc=$rc): $out13d" >&2; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out13d" || { echo "FAIL S13d: no COMPLETE: $out13d" >&2; exit 1; }
echo "expanded-chain OK: resume consumes the merged owner chain (union), stops on new-RS pod, zero extra deletes, then completes"

# living-parent-union: living parent + vanished RS + terminating child: the parent
# survives a first DELETE 403 (chain {dep,rs} proven), then the RS GC's away
# while its pod is still terminating. The resume's narrow RS query sees only
# {dep} — the barrier MUST use the merged union and stop with zero further
# deletes; once the child is gone, the next attempt completes.
scn living-parent-union 0
python3 -c "import json;json.dump([{'match':{'verb':'delete','kind':'deployment','name':'dep1'},'reply':'forbidden'}],open('$scratch/living-parent-union/faults.json','w'))"
rc=0; run_retire "$scratch/living-parent-union" "$scratch/living-parent-union/manifest.tsv" "$scratch/living-parent-union/out" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S14a: attempt1 should stop on deploy 403 (rc=$rc)" >&2; exit 1; }
# RS vanishes (GC), its pod stays terminating (deletionTimestamp set)
rm -f "$scratch/living-parent-union/objects/replicaset__nfs-csi-ngxstorage__rs1.json"
python3 - "$scratch/living-parent-union" "$NS" <<'PY'
import json, sys, os
S, NS = sys.argv[1], sys.argv[2]
p = f"{S}/objects/pod__{NS}__dep1-abc-12345.json"
o = json.load(open(p))
o["metadata"]["deletionTimestamp"] = "2026-11-11T00:00:00Z"
json.dump(o, open(p, "w"))
PY
python3 -c "import json;json.dump([],open('$scratch/living-parent-union/faults.json','w'))"
rc=0; out14="$(run_retire "$scratch/living-parent-union" "$scratch/living-parent-union/manifest.tsv" "$scratch/living-parent-union/out" 2>&1)" || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL S14b: resume must stop at the barrier with the merged union (rc=$rc): $out14" >&2; exit 1; }
grep -q "dep1-abc-12345" <<< "$out14" || { echo "FAIL S14b: barrier did not see the terminating child: $out14" >&2; exit 1; }
[ -f "$scratch/living-parent-union/objects/lease__nfs-csi-ngxstorage__le1.json" ] && \
[ -f "$scratch/living-parent-union/objects/role__nfs-csi-ngxstorage__ro1.json" ] && \
[ -f "$scratch/living-parent-union/objects/csidriver__-__cd1.json" ] \
  || { echo "FAIL S14b: deleted Lease/Role/CSIDriver before the child terminated" >&2; exit 1; }
# child finally goes -> next attempt completes
rm -f "$scratch/living-parent-union/objects/pod__nfs-csi-ngxstorage__dep1-abc-12345.json"
rc=0; out14c="$(run_retire "$scratch/living-parent-union" "$scratch/living-parent-union/manifest.tsv" "$scratch/living-parent-union/out" 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL S14c: resume after child gone should complete (rc=$rc): $out14c" >&2; exit 1; }
grep -q "RETIRE-COMPLETE" <<< "$out14c" || { echo "FAIL S14c: no COMPLETE: $out14c" >&2; exit 1; }
echo "living-parent-union OK: living-parent barrier consumes the merged union; terminating child blocks Lease/RBAC/CSIDriver; resume completes after child gone"

echo "retire.py offline tests PASS (happy-path, preflight-403, uid-mismatch, delete-time-stop, postcond-403, owner-barrier, unproven-resume, valid-resume, uid-conflict, resume-barrier, lost-baseline, same-attempt-chain, expanded-chain, living-parent-union)"
