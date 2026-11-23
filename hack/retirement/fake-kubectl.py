#!/usr/bin/env python3
"""Fake `kubectl` for offline retire/inventory tests (offline fixture review).

State lives in $FAKE_STATE:
  objects/<kind>__<ns>__<name>.json   live objects
  pods.txt                            one pod name per line (pod barrier)
  faults.json                         [{match:{verb,kind?,name?}, reply:"forbidden"|"timeout"|"notfound"|"internal"}]
  calls.log                           argv of every invocation (assert zero-delete etc.)

Reply semantics:
  forbidden -> rc=1, stderr "Error from server (Forbidden): ..."
  timeout   -> rc=1, stderr "error: timed out waiting for ..."
  notfound  -> rc=1, stderr 'Error from server (NotFound): ...'
  internal  -> rc=1, stderr "error: server gave HTTP response 500"
"""
import os, sys, json, glob

S = os.environ["FAKE_STATE"]
objs = os.path.join(S, "objects")
os.makedirs(objs, exist_ok=True)
with open(os.path.join(S, "calls.log"), "a") as f:
    f.write("\t".join(sys.argv[1:]) + "\n")
# global call counter (faults may require a minimum call count to fire,
# so a fault can target only the delete-time or postcondition phase)
ctr_path = os.path.join(S, ".callctr")
ctr = int(open(ctr_path).read()) if os.path.exists(ctr_path) else 0
ctr += 1
open(ctr_path, "w").write(str(ctr))

def key(kind, ns, name):
    return f"{kind}__{ns}__{name}"

def load(kind, ns, name):
    p = os.path.join(objs, key(kind, ns, name) + ".json")
    if not os.path.exists(p):
        return None
    return json.load(open(p))

def save(kind, ns, name, obj):
    json.dump(obj, open(os.path.join(objs, key(kind, ns, name) + ".json"), "w"))

def reply(code, stderr, stdout=""):
    sys.stdout.write(stdout)
    sys.stderr.write(stderr)
    sys.exit(code)

args = sys.argv[1:]
# strip global flags we don't model
a = [x for x in args]

# fault injection lookup
faults = []
fp = os.path.join(S, "faults.json")
if os.path.exists(fp):
    faults = json.load(open(fp))

deleted_file = os.path.join(S, "deleted.txt")
deleted = set(open(deleted_file).read().split()) if os.path.exists(deleted_file) else set()

def match_fault(verb, kind=None, name=None):
    for fl in faults:
        m = fl.get("match", {})
        if m.get("verb") and m["verb"] != verb:
            continue
        if m.get("kind") and m["kind"] != kind:
            continue
        if m.get("name") and m["name"] != name:
            continue
        if m.get("after_delete") and m["after_delete"] not in deleted:
            continue
        return fl
    return None

if a and a[0] == "version":
    reply(0, "", json.dumps({"serverVersion": {"gitVersion": "v1.36.4"}}))

if a and a[0] == "get":
    rest = a[1:]
 # parse [-n NS] / [-A] and trailing -o json / -o jsonpath=...
    ns = ""
    allns = False
    out = ""
    items = []
    i = 0
    kindarg = None
    namearg = None
    while i < len(rest):
        x = rest[i]
        if x == "-n":
            ns = rest[i+1]; i += 2; continue
        if x == "-A":
            allns = True; i += 1; continue
        if x == "-o":
            out = rest[i+1]; i += 2; continue
        if x.startswith("-o="):
            out = x; i += 1; continue
        if kindarg is None:
            kindarg = x
        else:
            namearg = x
        i += 1

 # namespace object is treated as kind "ns"
    if kindarg == "ns":
        kindarg, namearg = "ns", ns or namearg
        ns = ""

 # objects are stored under singular kind; normalize plural list/get names
    plural2kind = {"daemonsets": "daemonset", "deployments": "deployment",
                   "replicasets": "replicaset", "leases": "lease", "roles": "role",
                   "rolebindings": "rolebinding", "serviceaccounts": "serviceaccount",
                   "clusterrolebindings": "clusterrolebinding", "clusterroles": "clusterrole",
                   "csidrivers": "csidriver", "namespaces": "ns", "secrets": "secret",
                   "configmaps": "configmap", "pods": "pod", "persistentvolumes": "pv",
                   "persistentvolumeclaims": "pvc", "storageclasses": "sc",
                   "volumeattachments": "volumeattachment", "csinodes": "csinode"}
    kindarg = plural2kind.get(kindarg, kindarg)

    f = match_fault("get", kindarg, namearg)
    fr = f["reply"] if f else None
    if fr == "forbidden":
        reply(1, "Error from server (Forbidden): kubectl get forbidden\n")
    if fr == "timeout":
        reply(1, "error: timed out waiting for the condition\n")
    if fr == "internal":
        reply(1, "error: server gave HTTP response with error 500\n")

    cluster_scoped = {"ns", "csidriver", "clusterrole", "clusterrolebinding",
                      "sc", "volumeattachment", "csinode", "pv",
                      "clusterrolebinding"}

    if kindarg in ("pods", "pod"):
        kindarg = "pod"
 # pods are real objects (with ownerReferences) so the owner-chain
 # barrier can be exercised
        if namearg is not None:
            obj = load("pod", ns, namearg)
            if obj is None:
                reply(1, f'Error from server (NotFound): pods "{namearg}" not found\n')
            reply(0, "", json.dumps(obj))
        pats = glob.glob(os.path.join(objs, f"pod__{ns}__*.json")) if not allns \
            else glob.glob(os.path.join(objs, "pod__*__*.json"))
        items = [json.load(open(p)) for p in pats]
        if out and "jsonpath" in out:
            reply(0, "", "\n".join(i["metadata"]["name"] for i in items) + ("\n" if items else ""))
        reply(0, "", json.dumps({"items": items}))

    if kindarg == "csinode":
        p = os.path.join(objs, "csinode__-__all.json")
        reply(0, "", open(p).read() if os.path.exists(p) else '{"items":[]}')

    if namearg is not None:
        obj = load(kindarg, ns, namearg)
        if obj is None and ns == "":
            obj = load(kindarg, "-", namearg)  # cluster-scoped stored under "-"
        if fr == "notfound" or obj is None:
            reply(1, f'Error from server (NotFound): {kindarg}s "{namearg}" not found\n')
        reply(0, "", json.dumps(obj))

 # list (cluster-scoped or -A)
    if allns or kindarg in cluster_scoped:
        pats = []
        if allns:
            pats = glob.glob(os.path.join(objs, f"{kindarg}__*__*.json"))
        else:
            pats = glob.glob(os.path.join(objs, f"{kindarg}__-__*.json"))
        items = [json.load(open(p)) for p in pats]
        reply(0, "", json.dumps({"items": items}))
 # namespaced list
    pats = glob.glob(os.path.join(objs, f"{kindarg}__{ns}__*.json"))
    items = [json.load(open(p)) for p in pats]
    reply(0, "", json.dumps({"items": items}))

if a and a[0] == "delete":
    raw = None
    for x in a:
        if x.startswith("--raw="):
            raw = x[len("--raw="):]
    body = sys.stdin.read()
 # UID precondition: the UID precondition must actually arrive on stdin. An empty or
 # malformed DeleteOptions body, or one missing preconditions.uid, is
 # rejected like a strict client would — the fake never deletes
 # precondition-less.
    try:
        parsed = json.loads(body) if body.strip() else None
        pre = (parsed or {}).get("preconditions", {}).get("uid")
    except Exception:
        parsed, pre = None, None
    if not isinstance(parsed, dict) or not pre:
        reply(2, "STRICT CLIENT: missing DeleteOptions UID body\n")
    with open(os.path.join(S, "delete-bodies.tsv"), "a") as f:
        f.write(f"{raw}\t{pre}\n")
 # raw = /apis/<group>/<version>/namespaces/<ns>/<plural>/<name> or cluster path
    parts = raw.strip("/").split("/")
    if "namespaces" in parts:
        ns = parts[parts.index("namespaces") + 1]
        plural = parts[-2]
        name = parts[-1]
    else:
        ns = "-"
        plural = parts[-2]
        name = parts[-1]
    plural2kind = {v: k for k, v in {
        "daemonsets": "daemonset", "deployments": "deployment",
        "replicasets": "replicaset", "leases": "lease", "roles": "role",
        "rolebindings": "rolebinding", "serviceaccounts": "serviceaccount",
        "clusterrolebindings": "clusterrolebinding", "clusterroles": "clusterrole",
        "csidrivers": "csidriver", "pods": "pod"}.items()}
    kind = plural2kind.get(plural, plural.rstrip("s"))
    fl = match_fault("delete", kind, name)
    fr = fl["reply"] if fl else None
    obj = load(kind, ns, name)
    if fr == "forbidden":
        reply(1, "Error from server (Forbidden): deletes forbidden\n")
    if fr == "uid-conflict":
        reply(1, 'Error from server (Conflict): the server object was not found'
                 ' (uid precondition failed)\n')
    if obj is None:
        reply(1, f'Error from server (NotFound): {kind}s "{name}" not found\n')
    if pre and obj.get("metadata", {}).get("uid") != pre:
        reply(1, 'Error from server (Conflict): the server object was not found'
                 ' (uid precondition failed)\n')
    os.remove(os.path.join(objs, key(kind, ns, name) + ".json"))
    with open(deleted_file, "a") as fh:
        fh.write(f"{kind}/{name}\n")
 # optional cascade: model GC walking the ownerReference UID chain
    if os.path.exists(os.path.join(S, ".cascade")) and kind in ("daemonset", "deployment"):
        owners = {obj["metadata"]["uid"]}
        if kind == "deployment":
            for p in glob.glob(os.path.join(objs, f"replicaset__{ns}__*.json")):
                rso = json.load(open(p))
                for orr in rso["metadata"].get("ownerReferences", []):
                    if orr.get("uid") in owners:
                        owners.add(rso["metadata"]["uid"])
        for p in glob.glob(os.path.join(objs, f"pod__{ns}__*.json")):
            po = json.load(open(p))
            for orr in po["metadata"].get("ownerReferences", []):
                if orr.get("uid") in owners:
                    os.remove(p)
                    break
 # spawn-late-pod: model a child becoming visible AFTER the workload's own
 # barrier passed (late GC / still-terminating child) for the postcondition
    if fl and fl.get("spawn"):
        sp = fl["spawn"]
        pod = {"kind": "Pod", "metadata": {"name": sp["name"], "namespace": sp.get("ns", ns),
                "uid": "pod-" + sp["name"], "ownerReferences": [{"apiVersion": "apps/v1",
                "kind": "ReplicaSet", "name": "x", "uid": sp["owner"]}]}, "spec": {}}
        json.dump(pod, open(os.path.join(objs, key("pod", sp.get("ns", ns), sp["name"]) + ".json"), "w"))
    reply(0, "", '{"kind":"Status","status":"Success"}\n')

reply(2, f"fake-kubectl: unhandled argv {a}\n")
