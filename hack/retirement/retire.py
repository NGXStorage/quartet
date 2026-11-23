#!/usr/bin/env python3
"""Retirement executor (k8s-only scope)
.

Usage: retire.py <manifest.tsv> <out-dir> <namespace> <kubectl|oc> [context]

Phases:
0. Server identity (version, client) is locked into the attempt journal.
1. PREFLIGHT (zero mutations): every DELETE row is GET'd with the FULL
   BIN+context; only a genuine API NotFound (kube_errors.is_notfound) is
   tolerated. UID and namespace must match the manifest. Restore manifests
   are exported via semantic YAML parse + structural server-field strip +
   re-parse validation. Restore files are IMMUTABLE and journal-proven:
   an existing file is accepted only when its hash matches the hash the
   FIRST journal recorded for that object (full-hash comparison) AND the
   file parses with matching kind/name — for objects that are already
   gone the file is still validated this way (baseline restore). A baseline restore
   file that disappeared is a STOP, never a silent pass.
2. DELETE (strict single pass): the DeleteOptions body carrying
   preconditions.uid is delivered via stdin (UID precondition). The FIRST UID mismatch,
   Conflict, Forbidden, timeout or transport error exits nonzero at once —
   Conflict is never confused with NotFound (error classification). After each workload the
   owner-chain POD BARRIER (owner-chain barrier) records the ReplicaSet UIDs owned by the
   parent BEFORE deleting it, then requires every pod whose ownerReference
   UID is in that recorded chain to be actually gone (new names included)
   before any later kind proceeds.
3. POSTCONDITIONS (fail-closed): only genuine NotFound counts as deleted;
   namespace UID must equal the FIRST-attempt baseline; NEIGHBOR semantic
   hashes must match the manifest using the SHARED semantic-hash contract;
   PROTECTED rows must exist with unchanged UID; the CSINode query must
   SUCCEED and show zero nfs registrations. Any failure exits nonzero.

Resume: journals are append-only per attempt; restore manifests are never
rewritten. There is NO "adopt current UID" path — replacement is always a
STOP (the 2026-11-11 manual lease deletion remains a documented, separate
exception).

Scope: k8s-only (no scc/sc/service in ORDER; oc wiring unexercised).
"""
import hashlib, json, os, subprocess, sys, datetime, re

MANIFEST, OUT, NS = sys.argv[1], sys.argv[2], sys.argv[3]
BIN = [sys.argv[4]] + (["--context", sys.argv[5]] if len(sys.argv) > 5 else [])

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import importlib.util
import yaml
import semantic_hash as sh
import kube_errors as ke

_spec = importlib.util.spec_from_file_location(
    "gen", os.path.join(HERE, "make-execution-manifest.py"))
gen = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gen)

ORDER = ["daemonset", "deployment", "replicaset", "lease", "role",
         "rolebinding", "serviceaccount", "clusterrolebinding", "clusterrole",
         "csidriver"]
PATH = {
    "daemonset": f"/apis/apps/v1/namespaces/{{ns}}/daemonsets",
    "deployment": f"/apis/apps/v1/namespaces/{{ns}}/deployments",
    "replicaset": f"/apis/apps/v1/namespaces/{{ns}}/replicasets",
    "lease": f"/apis/coordination.k8s.io/v1/namespaces/{{ns}}/leases",
    "role": f"/apis/rbac.authorization.k8s.io/v1/namespaces/{{ns}}/roles",
    "rolebinding": f"/apis/rbac.authorization.k8s.io/v1/namespaces/{{ns}}/rolebindings",
    "serviceaccount": f"/api/v1/namespaces/{{ns}}/serviceaccounts",
    "clusterrolebinding": "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings",
    "clusterrole": "/apis/rbac.authorization.k8s.io/v1/clusterroles",
    "csidriver": "/apis/storage.k8s.io/v1/csidrivers",
}
CLUSTER_SCOPED = ("clusterrolebinding", "clusterrole", "csidriver")
DRIVER = "nfs.csi.ngxstorage.com"

os.makedirs(f"{OUT}/restore-manifests", exist_ok=True)
attempt = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S-") + str(os.getpid())
journal = open(f"{OUT}/journal-{attempt}.tsv", "a")


def ts():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%FT%TZ")


def logj(phase, obj, detail):
    journal.write(f"{ts()}\t{phase}\t{obj}\t{detail}\n")
    journal.flush()
    print(f"[{phase}] {obj} {detail}" if detail else f"[{phase}] {obj}")


def die(phase, obj, detail):
    logj(phase, obj, detail + "  -> STOP (fail-closed)")
    sys.exit(1)


def kubectl(args, timeout=60, stdin=None):
    """Uniform BIN+context for EVERY query (uniform query); stdin is delivered for real
    (UID precondition) — delete bodies must actually reach the API."""
    try:
        o = subprocess.run(BIN + args, input=stdin, capture_output=True,
                           text=True, timeout=timeout)
        return o.returncode, o.stdout, o.stderr
    except subprocess.TimeoutExpired:
        return 124, "", "timeout"


def get_obj(kind, name):
    """GET with fail-closed classification: only a genuine NotFound returns
    None; Conflict/Forbidden/timeout are API errors (SystemExit)."""
    a = ["get", kind] + (["-n", NS] if kind not in CLUSTER_SCOPED else []) + [name, "-o", "json"]
    rc, out, err = kubectl(a)
    if rc == 0:
        return json.loads(out)
    if ke.is_notfound(rc, err):
        return None
    raise SystemExit(f"GET API error for {kind}/{name} (rc={rc}): {err.strip()[:120]} — fail-closed")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def journal_rows():
    for fn in sorted(os.listdir(OUT)):
        if not (fn.startswith("journal-") and fn.endswith(".tsv")):
            continue
        for line in open(f"{OUT}/{fn}"):
            yield line.rstrip("\n").split("\t")


def read_journals():
    """ident -> full sha256 recorded by the FIRST 'exported' journal entry."""
    proven = {}
    for p in journal_rows():
        if len(p) == 4 and p[1] == "exported" and "hash=" in p[3]:
            h = p[3].split("hash=", 1)[1].split(" ")[0]
            if p[2] not in proven:
                proven[p[2]] = h
    return proven


def read_owner_chains(parent_uids):
    """owner-chain union: consume the UNION of every proven 'owner-chain' journal record,
    not just the first — a parent can legally see a wider owner chain in a
    later attempt (e.g. a new ReplicaSet before its delete failed). Each
    record carries the verified parent UID; a record whose parent does not
    match the manifest UID for that ident is evidence tampering and is
    reported as a violation (fail-closed). Returns (chains, violations).
    Legacy records without a parent= field cannot be verified -> violation."""
    chains, violations = {}, []
    for p in journal_rows():
        if len(p) != 4 or p[1] != "owner-chain":
            continue
        ident, detail = p[2], p[3]
        m_parent = re.search(r"parent=([\w.-]+)", detail)
        m_uids = re.search(r"uids=(.*)$", detail)
        if not m_parent or not m_uids:
            violations.append(f"{ident}: owner-chain record without verifiable parent/uids: {detail!r}")
            continue
        rec_parent = m_parent.group(1)
        if parent_uids.get(ident) != rec_parent:
            violations.append(f"{ident}: owner-chain parent uid {rec_parent} != manifest uid {parent_uids.get(ident)}")
            continue
        chains.setdefault(ident, set()).update(u for u in m_uids.group(1).split(",") if u)
    return chains, violations


def ns_baseline_uid():
    """The FIRST attempt's namespace UID baseline, journaled once."""
    for fn in sorted(os.listdir(OUT)):
        if not (fn.startswith("journal-") and fn.endswith(".tsv")):
            continue
        for line in open(f"{OUT}/{fn}"):
            p = line.rstrip("\n").split("\t")
            if len(p) == 4 and p[1] == "start" and p[2] == "namespace" and p[3].startswith("uid="):
                return p[3].split("uid=")[1]
    return None


# ---------------------------------------------------------------- manifest
viol = gen.validate_manifest(MANIFEST)
for v in viol:
    print("manifest violation:", v)
if viol:
    die("preflight", "manifest", "canonical validation failed")

rows = []
with open(MANIFEST) as f:
    hdr = f.readline().rstrip("\n").split("\t")
    for line in f:
        p = line.rstrip("\n").split("\t")
        if len(p) != 9:
            die("preflight", "manifest", f"malformed row not tolerated: {p!r}")
        rows.append(dict(zip(hdr, p)))
delete = [r for r in rows if r["role"] == "DELETE" and r["kind"] != "pod"]
cascaded = [r for r in rows if r["role"] == "DELETE" and r["kind"] == "pod"]
for r in delete:
    if r["kind"] not in ORDER:
        die("preflight", f"{r['kind']}/{r['name']}", "kind outside k8s-only ORDER scope")
    if r["kind"] not in CLUSTER_SCOPED and r["namespace"] != NS:
        die("preflight", f"{r['kind']}/{r['name']}",
            f"manifest namespace {r['namespace']} != target {NS}")

# ------------------------------------------------- phase 0: lock identity
rc, out, err = kubectl(["version", "--output=json"])
if rc != 0:
    die("preflight", "server", f"cannot lock server identity: {err.strip()[:120]}")
logj("start", "server", f"gitVersion={json.loads(out).get('serverVersion', {}).get('gitVersion', '?')} bin={' '.join(BIN)}")

rc, out, err = kubectl(["get", "ns", NS, "-o", "json"])
if rc != 0:
    die("preflight", f"ns/{NS}", f"namespace GET failed: {err.strip()[:120]}")
ns_uid_now = json.loads(out)["metadata"]["uid"]
base_uid = ns_baseline_uid()
if base_uid is None:
    base_uid = ns_uid_now
    logj("start", "namespace", f"uid={base_uid}")
elif ns_uid_now != base_uid:
    die("preflight", f"ns/{NS}", f"namespace UID changed since first attempt ({base_uid} -> {ns_uid_now})")

proven = read_journals()

# owner-chain union: consume the union of every proven owner-chain record, verified against
# the manifest parent UIDs (a mismatched parent is evidence tampering).
chains, chain_violations = read_owner_chains({f"{r['kind']}/{r['name']}": r["uid"] for r in delete})
for v in chain_violations:
    die("preflight", "owner-chain", v)
owner_chains = chains


def record_owner_chain(ident, parent_uid, new_uids):
    """Single writer for owner chains (owner-chain union): merge WITHOUT losing child UIDs
    already proven for the same verified parent, append the union to the
    append-only journal, and update the in-memory state in the SAME call so
    this attempt's postcondition sees it. R4: RETURNS the merged set —
    memory, journal, the immediate barrier and the final guard must all
    consume the same union, never a narrower per-query set."""
    merged = owner_chains.get(ident, set()) | set(new_uids)
    owner_chains[ident] = merged
    logj("owner-chain", ident, f"parent={parent_uid} uids={','.join(sorted(merged))}")
    return merged


def validate_existing_export(ident, kind, name):
    """baseline restore: an existing restore file must parse, carry a matching metadata.name,
    and hash-match the FIRST journal's FULL recorded hash — required whether
    the object is still present or already gone. (The export strips the uid,
    so identity is name + journal hash, not uid.)"""
    safe = f"{OUT}/restore-manifests/{kind}-{name}.yaml"
    if ident not in proven:
        return False
    if not os.path.exists(safe):
        return False
    try:
        doc = yaml.safe_load(open(safe))
    except Exception:
        return False
    if not isinstance(doc, dict) or (doc.get("metadata") or {}).get("name") != name:
        return False
    return proven[ident] == sha256_file(safe)


# ------------------------------------------- phase 1: preflight, zero deletes
current = {}
for r in sorted(delete, key=lambda x: (ORDER.index(x["kind"]), x["name"])):
    kind, name, uid = r["kind"], r["name"], r["uid"]
    ident = f"{kind}/{name}"
    it = get_obj(kind, name)
    if it is None:
 # already gone (resume): the restore baseline for it must still be
 # proven, else the object may have been lost without a backup.
        if not validate_existing_export(ident, kind, name):
            die("preflight", ident, "already absent but restore baseline missing/unproven — STOP")
        logj("preflight", ident, "already absent before any mutation; baseline proven (resume)")
        current[ident] = None
        continue
    meta = it["metadata"]
    if meta["uid"] != uid:
        die("preflight", ident, f"UID mismatch manifest={uid} current={meta['uid']} — replacement; zero deletes performed")
    if kind not in CLUSTER_SCOPED and meta.get("namespace", NS) != NS:
        die("preflight", ident, "namespace mismatch")
    safe = f"{OUT}/restore-manifests/{kind}-{name}.yaml"
    if ident in proven:
 # R2: a journal-proven baseline exists for this object. It is
 # IMMUTABLE: a missing file means the baseline was lost — never
 # regenerate it from the (possibly mutated) live object.
        if not os.path.exists(safe):
            die("preflight", ident, "journal-proven restore baseline is MISSING — STOP (no regeneration from the live object)")
        if sha256_file(safe) != proven[ident]:
            die("preflight", ident, "restore baseline hash mismatch — STOP")
        logj("preflight", ident, "restore-exists (journal-proven, immutable)")
    elif os.path.exists(safe):
        die("preflight", ident, "restore file exists but has NO journal export record — unproven; STOP")
    else:
 # first-ever export for this object
        doc = it
        md = doc.get("metadata", {})
        for f in sh._SERVER_FIELDS + ("ownerReferences",):
            md.pop(f, None)
 # ownerReferences are stripped for ALL kinds (documented restore
 # policy): an orphaned historical RS restored under a new Deployment
 # UID would become a second active workload. Rollback applies the
 # parent workload; historical RS restores are opt-in, never automatic.
        doc.pop("status", None)
        y = yaml.safe_dump(doc, sort_keys=False, default_flow_style=False)
        yaml.safe_load(y)  # re-parse validation
        with open(safe, "w") as f2:
            f2.write(y)
        logj("exported", ident, f"hash={sha256_file(safe)} uid={uid}")
    current[ident] = uid

logj("preflight-done", "restore-set", f"{len(delete)} objects validated, zero mutations so far")

# ------------------------------------------- phase 2: strict deletion
def owned_uids(workload_uid, kind):
    """Record the owner chain BEFORE the parent is deleted (owner-chain barrier): GC removes
    the relationship data afterwards, so RS UIDs must be captured first."""
    uids = {workload_uid}
    if kind == "deployment":
        rc, out, err = kubectl(["get", "replicasets", "-n", NS, "-o", "json"])
        if rc != 0:
            die("owner-chain", f"deployment/{workload_uid}", f"RS GET API error: {err.strip()[:120]}")
        for rs in json.loads(out).get("items", []):
            for orr in rs["metadata"].get("ownerReferences", []):
                if orr.get("uid") == workload_uid:
                    uids.add(rs["metadata"]["uid"])
    return uids


def live_owned_pods(owner_uids):
    """Pods whose ownerReference UID chain matches the recorded owners.
    Owner-UID chain (not name prefix) is the ownership proof, so a NEW
    pod name created after the snapshot is still caught (owner-chain barrier)."""
    rc, out, err = kubectl(["get", "pods", "-n", NS, "-o", "json"])
    if rc != 0:
        die("pod-barrier", "pods", f"pod GET API error: {err.strip()[:120]}")
    live = []
    for p in json.loads(out).get("items", []):
        ors = {o.get("uid") for o in p["metadata"].get("ownerReferences", [])}
        if ors & owner_uids:
            live.append(p["metadata"]["name"])
    return live


def enforce_barrier(ident, chain):
    """Wait until every pod owned by the recorded chain is gone (owner-chain barrier/owner-chain barrier)."""
    tries = int(os.environ.get("RETIRE_BARRIER_TRIES", "150"))
    sleep_s = os.environ.get("RETIRE_BARRIER_SLEEP", "2")
    for _ in range(tries):
        still = live_owned_pods(chain)
        if not still:
            break
        subprocess.run(["sleep", sleep_s])
    else:
        die("pod-barrier", ident, f"owner-chain pods still alive after workload delete: {sorted(still)}")
    logj("pod-barrier-ok", ident, "all owner-chain pods terminated (recorded RS/DS UID chain)")


WORKLOADS = ("deployment", "daemonset")
for r in sorted(delete, key=lambda x: (ORDER.index(x["kind"]), x["name"])):
    kind, name, uid = r["kind"], r["name"], r["uid"]
    ident = f"{kind}/{name}"
    is_workload = kind in WORKLOADS

    def absent_barrier():
 # owner-chain barrier: a workload whose parent is already gone (resume / traced-gone)
 # still owes its pod barrier. Enforce it from the journal-proven
 # chain; with no recorded chain we cannot prove pods are gone -> STOP.
        if not is_workload:
            return
        chain = owner_chains.get(ident)
        if chain is None:
            die("delete", ident, "workload absent but no journal-proven owner chain — cannot prove owned pods gone; STOP before further deletes")
        enforce_barrier(ident, chain)

    if current[ident] is None:
        logj("delete", ident, "already absent (resume)")
        absent_barrier()
        continue
    it = get_obj(kind, name)
    if it is None:
        logj("delete", ident, "traced-gone between preflight and delete")
        absent_barrier()
        continue
    cur_uid = it["metadata"]["uid"]
    if cur_uid != uid:
        die("delete", ident, f"UID mismatch at delete time manifest={uid} current={cur_uid}")

    chain = None
    if is_workload:
 # R4: use the MERGED union (this attempt's query + every previously
 # proven child for the same parent), not the narrow per-query set —
 # an RS may have GC'd away while its pod is still terminating.
        chain = record_owner_chain(ident, uid, owned_uids(uid, kind))

    raw = PATH[kind].replace("{ns}", NS) + "/" + name
    body = json.dumps({"kind": "DeleteOptions", "apiVersion": "v1",
                       "metadata": {"name": name}, "preconditions": {"uid": uid}})
    rc, out, err = kubectl(["delete", f"--raw={raw}", "-f", "-"],
                           timeout=90, stdin=body)
    if rc != 0:
        if ke.is_notfound(rc, err):
            logj("delete", ident, "traced-gone at delete time")
            absent_barrier()
            continue
        if ke.is_conflict(rc, err):
            die("delete", ident, f"UID precondition CONFLICT — replacement kept, no further deletes: {err.strip()[:140]}")
        die("delete", ident, f"delete API error (rc={rc}): {err.strip()[:160]}")
    logj("deleted", ident, f"uid={uid}")

    if is_workload:
        enforce_barrier(ident, chain)

# ------------------------------------------- phase 3: fail-closed postconditions
fails = 0


def pc_fail(obj, detail):
    global fails
    fails += 1
    print(f"POSTCOND-FAIL {obj}: {detail}")


# owner-chain barrier: every journal-proven owner chain must be pod-free at the end — the
# snapshot pod names alone do not cover pods created after the snapshot.
for ident, chain in sorted(owner_chains.items()):
    still = live_owned_pods(chain)
    if still:
        pc_fail(f"owner-chain {ident}", f"owned pods still alive: {sorted(still)}")


for r in delete + cascaded:
    kind, name = r["kind"], r["name"]
    if kind == "pod":
        a = ["get", "pod", "-n", NS, name, "-o", "json"]
    else:
        a = ["get", kind] + (["-n", NS] if kind not in CLUSTER_SCOPED else []) + [name, "-o", "json"]
    rc, out, err = kubectl(a)
    if rc == 0:
        pc_fail(f"{kind}/{name}", "still present")
    elif not ke.is_notfound(rc, err):
        pc_fail(f"{kind}/{name}", f"NOT a NotFound (rc={rc}): {err.strip()[:120]}")

rc, out, err = kubectl(["get", "ns", NS, "-o", "json"])
if rc != 0 or json.loads(out)["metadata"]["uid"] != base_uid:
    pc_fail(f"ns/{NS}", "UID changed or namespace GET failed")

# NEIGHBOR re-hash with the SHARED semantic-hash contract (semantic hash)
for r in rows:
    if r["role"] != "NEIGHBOR":
        continue
    kind, name = r["kind"], r["name"]
    it = get_obj(kind, name)
    if it is None:
        pc_fail(f"neighbor {kind}/{name}", "disappeared — outside retirement scope")
        continue
    if sh.semantic_hash(it, kind) != r["semanticSha256"]:
        pc_fail(f"neighbor {kind}/{name}", "semantic hash changed (shared contract, renewTime-normalized)")
    if it["metadata"]["uid"] != r["uid"]:
        pc_fail(f"neighbor {kind}/{name}", "UID replaced")

for r in rows:
    if r["role"] != "PROTECTED":
        continue
    kind, name = r["kind"], r["name"]
    it = get_obj(kind, name)
    if it is None:
        pc_fail(f"protected {kind}/{name}", "MISSING — protected set violated")
    elif it["metadata"]["uid"] != r["uid"]:
        pc_fail(f"protected {kind}/{name}", "UID changed")

rc, out, err = kubectl(["get", "csinode", "-A", "-o", "json"])
if rc != 0:
    pc_fail("csinode", f"query failed (rc={rc}): {err.strip()[:120]}")
else:
    reg = sum(1 for item in json.loads(out).get("items", [])
              for d in item.get("spec", {}).get("drivers", [])
              if d.get("name") == DRIVER)
    if reg != 0:
        pc_fail("csinode", f"{reg} nfs registrations remain")

logj("postcondition", "overall", "COMPLETE" if fails == 0 else f"INCOMPLETE fails={fails}")
if fails:
    sys.exit(1)
print("RETIRE-COMPLETE")
sys.exit(0)
