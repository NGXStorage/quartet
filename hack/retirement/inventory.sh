#!/usr/bin/env bash
# Retirement discovery inventory (read-only; runs ON a node with the
# cluster client). Produces the 7-column discovery TSV consumed by
# hack/retirement/make-execution-manifest.py:
#
# role kind namespace name uid resourceVersion semanticSha256
#
# Roles:
# ALLOWLIST namespaced objects matching the retirement name prefixes in
# NS, plus every explicitly requested cluster-scoped object.
# NEIGHBOR other namespaced objects (postcondition baseline).
# CONSUMER-* widened consumer scan hits (PV/PVC/VA/SC/RB subjects).
# INVENTORY-ERROR any fetch that failed (403/timeout/missing scope) —
# the manifest generator refuses to publish over these.
#
# Semantic hash = sha256 of the object with server fields
# (uid/resourceVersion/managedFields/creationTimestamp/selfLink/status)
# removed; Secret data/stringData are REMOVED and flagged (data-excluded)
# — credential values never enter the inventory.
#
# Usage:
# inventory.sh <kubectl|oc> [--context C] <namespace> <kind/name> [...]
# kind/name cluster-scoped retirement targets (e.g.
# csidriver/nfs.csi.ngxstorage.com, clusterrole/nfs-csi-ngxstorage-controller)
# Namespaced kinds are scanned from the fixed list below.
set -euo pipefail
BIN="${1:?kubectl|oc}"
shift
CTX=""
if [ "${1:-}" = "--context" ]; then CTX="$2"; shift 2; fi
NS="${1:?namespace required}"
shift

ARGS=("$BIN")
[ -n "$CTX" ] && ARGS+=("--context" "$CTX")

INV_NS="$NS" INV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" \
python3 - "${ARGS[@]}" "$@" <<'PYEOF'
import json, subprocess, sys, os
argv = sys.argv[1:]
NS = os.environ["INV_NS"]
args = argv[:1] + ([argv[1], argv[2]] if argv[1] == "--context" else [])
# cluster-scoped kind/name pairs: passed after the namespace
raw = [x for x in argv if "/" in x]

# semantic hash: the SAME semantic-hash contract retire.py postconditions compare
# against (and the test manifest builders use) — one implementation.
sys.path.insert(0, os.environ["INV_DIR"])
import semantic_hash as sh

PREFIXES = ("nfs-csi-ngxstorage", "external-attacher-leader-nfs",
            "external-resizer-nfs", "external-snapshotter-leader-nfs")
DRIVER = "nfs.csi.ngxstorage.com"
KINDS_NS = ["configmap", "secret", "serviceaccount", "service", "deployment",
            "daemonset", "statefulset", "job", "replicaset", "role",
            "rolebinding", "lease", "pod"]

def h(o, kind):
    return sh.semantic_hash(o, kind)

rows = []

def err(kind, ns, name, msg):
    rows.append(["INVENTORY-ERROR", kind, ns, name, "-", "-", msg[:100]])

def get(kind, ns, name):
    a = args + ["get", kind] + (["-n", ns] if ns != "-" else []) + [name, "-o", "json"]
    o = subprocess.run(a, capture_output=True, text=True)
    if o.returncode != 0:
        return None, o.stderr.strip()
    return json.loads(o.stdout), None

def listk(kind, ns):
    """ns: '-' = cluster-scoped (no flag), '-A' = all namespaces
    (namespaced kinds), else explicit namespace."""
    a = args + ["get", kind]
    if ns == "-A":
        a += ["-A"]
    elif ns != "-":
        a += ["-n", ns]
    a += ["-o", "json"]
    o = subprocess.run(a, capture_output=True, text=True)
    if o.returncode != 0:
        return None, o.stderr.strip()
    return json.loads(o.stdout).get("items", []), None

print("role\tkind\tnamespace\tname\tuid\tresourceVersion\tsemanticSha256")
all_ns = {}
for k in KINDS_NS:
    items, e = listk(k, NS)
    if items is None:
        err(k, NS, "-", e)
        continue
    for it in items:
        all_ns[(k, it["metadata"]["name"])] = it
for (k, name), it in sorted(all_ns.items()):
    uid = it["metadata"]["uid"]
    rv = it["metadata"]["resourceVersion"]
    is_nfs = any(name.startswith(p) for p in PREFIXES)
    hv = h(it, k)  # shared contract; secret suffix handled inside
    rows.append(["ALLOWLIST" if is_nfs else "NEIGHBOR", k, NS, name, uid, rv, hv])

for spec in raw:
    k, name = spec.split("/", 1)
    it, e = get(k, "-", name)
    if it is None:
        err(k, "-", name, e)
        continue
    rows.append(["ALLOWLIST", k, "-", name, it["metadata"]["uid"],
                 it["metadata"]["resourceVersion"], h(it, k)])

# widened consumer scan (consumer scan scope + real schema real-schema fix):
# - PV: CSI spec.csi.driver == DRIVER, static spec.nfs, Pending/
# terminating states included (state is informational, hit is a hit).
# - VolumeAttachment: real field is spec.attacher (NOT spec.driverName).
# - StorageClass: real field is top-level .provisioner (NOT .spec).
# - PVC in ALL namespaces: bound to an nfs-driver PV, or selecting an
# nfs-provisioner SC; Pending flagged.
# - RBAC subjects referencing the operand ServiceAccounts: RoleBindings
# in ALL namespaces + ClusterRoleBindings (cluster grants).
# A hit is BLOCKING unless the manifest generator proves the consumer is
# itself in the DELETE set (internal self-reference).
items, e = listk("pv", "-")
if items is None:
    err("pv", "-", "-", e)
else:
    for it in items:
        spec = it.get("spec", {})
        if spec.get("csi", {}).get("driver") == DRIVER or "nfs" in spec:
            rows.append(["CONSUMER-PV", spec.get("csi", {}).get("driver") or "spec.nfs",
                         "-", it["metadata"]["name"], it["metadata"]["uid"],
                         it["metadata"]["resourceVersion"], h(it, "pv")])
    pv_by_name = {it["metadata"]["name"]: it for it in items}

# SC map: name -> provisioner (top-level field)
sc_prov = {}
items, e = listk("sc", "-")
if items is None:
    err("sc", "-", "-", e)
else:
    for it in items:
        prov = it.get("provisioner")
        sc_prov[it["metadata"]["name"]] = prov
        if prov == DRIVER:
            rows.append(["CONSUMER-SC", DRIVER, "-",
                         it["metadata"]["name"], it["metadata"]["uid"],
                         it["metadata"]["resourceVersion"], h(it, "sc")])

items, e = listk("volumeattachment", "-")
if items is None:
    err("volumeattachment", "-", "-", e)
else:
    for it in items:
 # REAL schema: spec.attacher holds the driver name.
        if it.get("spec", {}).get("attacher") == DRIVER:
            rows.append(["CONSUMER-VA", DRIVER, "-",
                         it["metadata"]["name"], it["metadata"]["uid"],
                         it["metadata"]["resourceVersion"], h(it, "volumeattachment")])

# PVC across ALL namespaces
items, e = listk("pvc", "-A")
if items is None:
    err("pvc", "-A", "-", e)
else:
    for it in items:
        spec = it.get("spec", {})
        pvname = spec.get("volumeName")
        ph = it.get("status", {}).get("phase", "")
        hit = None
        if pvname and pvname in pv_by_name:
            pvs = pv_by_name[pvname].get("spec", {})
            if pvs.get("csi", {}).get("driver") == DRIVER or "nfs" in pvs:
                hit = "bound-pv:" + pvname
        if hit is None and sc_prov.get(spec.get("storageClassName")) == DRIVER:
            hit = "sc:" + str(spec.get("storageClassName"))
        if hit:
            rows.append(["CONSUMER-PVC" + ("-PENDING" if ph == "Pending" else ""),
                         hit, it["metadata"]["namespace"],
                         it["metadata"]["name"], it["metadata"]["uid"],
                         it["metadata"]["resourceVersion"], h(it, "pvc")])

# RBAC subjects referencing operand SAs — ALL namespaces + cluster grants
items, e = listk("rolebinding", "-A")
if items is None:
    err("rolebinding", "-A", "-", e)
else:
    for it in items:
        for s in it.get("subjects", []):
            if s.get("kind") == "ServiceAccount" and \
               s.get("name", "").startswith("nfs-csi-ngxstorage"):
                rows.append(["CONSUMER-RB", it["metadata"]["name"],
                             it["metadata"].get("namespace", "-"),
                             "subject:" + s["name"], it["metadata"]["uid"],
                             it["metadata"]["resourceVersion"], "-"])
                break
items, e = listk("clusterrolebinding", "-")
if items is None:
    err("clusterrolebinding", "-", "-", e)
else:
    for it in items:
        for s in it.get("subjects", []):
            if s.get("kind") == "ServiceAccount" and \
               s.get("name", "").startswith("nfs-csi-ngxstorage"):
                rows.append(["CONSUMER-CRB", it["metadata"]["name"], "-",
                             "subject:" + s["name"], it["metadata"]["uid"],
                             it["metadata"]["resourceVersion"], "-"])
                break

for r in rows:
    print("\t".join(str(x) for x in r))
PYEOF
