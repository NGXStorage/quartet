#!/usr/bin/env python3
"""Offline fake API store + operator-behavior simulation for the
active-volume-disable-protection / active-volume-delete-protection scenario
cases.

Per CASE-CONTRACT.md ("Ortak helper yüzeyi"): case workers build TEST STUBS of
scenario_preflight/scenario_kube in their OWN fixtures. This is that stub's
engine: a local JSON object store plus a simulation of the reconciler's
OBSERVABLE usage-gate contract, so the case body can prove block/retention/
recovery/cleanup offline with a fake CLI. The Go reconciler itself is covered
by internal/controller unit + envtest cells.

FAKE- vs LIVE-ADAPTER boundary (protocol pin 4): the case body speaks ONLY
the real kubectl verb surface (get/create/patch/delete) and observes the
async operator via bounded status/observedGeneration polls. There is NO
synthetic "reconcile" verb anywhere in this model — main() refuses it like
any invented verb, exactly as a real kubectl would. The offline adapter
advances its simulated controllers (binding controller, external-attacher,
the operator reconcile pass) once per observed API call; that internal
advance is the fake's stand-in for real asynchrony and is never presented to
the case as a cluster command.

API-validity mirrored from the real API server (pin 4):
  * status on create/patch of subresource-enabled types is IGNORED — pre-set
    Bound/attached in the create JSON never takes effect; only the simulated
    controllers set status;
  * VolumeAttachment requires spec.attacher, spec.nodeName and
    spec.source.persistentVolumeName (exactly one source);
  * Deployment requires spec.selector AND spec.template;
  * PersistentVolume/PersistentVolumeClaim require capacity / requested size.
  The fake rejects invalid seeds with an "Error from server (Invalid)" style
  failure instead of silently accepting them the way the old model did.

Pins to source (every decision mirrors this code so the fake drifts nowhere):
  singleton CR name "quartet" ................. api/v1alpha1/quartet_types.go:14
  apiVersion quartet.ngxstorage.com/v1alpha1 .. api/v1alpha1/groupversion_info.go:113
  finalizer quartet.ngxstorage.com/quartet .... internal/controller/apply.go:31
  owner-uid annotation ....................... internal/renderer/profile.go:34
  driver name + handle prefix ................ internal/renderer/nfs.go:35,
                                               internal/renderer/profile.go:145-150
  operand namespace ngxstorage-nfs ........... internal/renderer/nfs.go:29
  controller deployment name ................. internal/renderer/nfs.go:527
  csidriver name ............................. internal/renderer/nfs.go:429-432
  ReasonDataInUse ............................ api/v1alpha1/quartet_types.go:87
  disable gate (block, retryable) ............ internal/controller/apply.go:87-96
  cluster-wide driver-identity scan .......... internal/controller/apply.go:1129-1200
  PV identity match .......................... internal/controller/apply.go:1207-1216
  usage summary format ....................... internal/controller/apply.go:1219-1237
  reason mapping (DataInUse / PermissionDenied /
  DependencyMissing) ......................... internal/controller/apply.go:1247-1259
  UID-ownership cleanup only ................. internal/controller/apply.go:939-990
  delete preconditions UID+RV ................ internal/controller/apply.go:1030-1072
  customer data: storage never touched ........ internal/controller/apply.go:97-98
  forceDisable: explicit admin override only . internal/controller/apply.go:1086-1092
  blocked-termination keeps finalizer ........ internal/controller/apply.go:1275-1295
  finalizer removed after clean pass only .... internal/controller/apply.go:1329-1342
  profile condition surface .................. internal/controller/quartet_controller.go:273-282
  aggregate first-failure surface ............ internal/controller/quartet_controller.go:293-297

Fault injection (the offline test battery proves the case BITES):
  FAKE_OPERATOR_FLAW in {premature-success-on-disable, premature-finalizer-removal,
                         unverified-delete, data-theft}
  FAKE_API_ERROR in {pv-list, pv-list-403, pvc-list-403, va-list}, honored for
  the first FAKE_API_ERROR_UNTIL_PASS operator passes (default: forever) —
  simulates the API's response, not the client objects (same boundary
  technique as internal/controller fake-client cells).
"""

import copy
import json
import os
import sys
import time
import uuid

API_VERSION = "quartet.ngxstorage.com/v1alpha1"
QUARTET_NAME = "quartet"                    # api/v1alpha1/quartet_types.go:14
FINALIZER = "quartet.ngxstorage.com/quartet"  # internal/controller/apply.go:31
OWNER_ANNOTATION = "quartet.ngxstorage.com/owner-uid"  # internal/renderer/profile.go:34
DRIVER_NAME = "nfs.csi.ngxstorage.com"       # internal/renderer/nfs.go:35
HANDLE_PREFIX = DRIVER_NAME + "."            # internal/renderer/profile.go:145-150
OPERAND_NS = "ngxstorage-nfs"                # internal/renderer/nfs.go:29
DEPLOYMENT_NAME = "nfs-csi-ngxstorage-controller"  # internal/renderer/nfs.go:527
CSIDRIVER_NAME = DRIVER_NAME                 # internal/renderer/nfs.go:429-432

REASON_READY = "Ready"
REASON_DATA_IN_USE = "DataInUse"                   # quartet_types.go:87
REASON_PERMISSION_DENIED = "PermissionDenied"      # apply.go:1253-1254
REASON_DEPENDENCY_MISSING = "DependencyMissing"    # apply.go:1252

CLUSTER_KINDS = {"quartet", "csidriver", "persistentvolume",
                 "volumeattachment", "node",
    "scc", "clusterrole",
}
NS_KINDS = {
    "deployment", "daemonset", "service", "configmap",
    "serviceaccount", "lease", "persistentvolumeclaim",
    "pod", "role",
}
ALL_KINDS = sorted(CLUSTER_KINDS | NS_KINDS)
KIND_ALIASES = {"nodes": "node", "pods": "pod", "leases": "lease",
                "services": "service", "serviceaccounts": "serviceaccount",
                "configmaps": "configmap", "deployments": "deployment",
                "daemonsets": "daemonset", "csidrivers": "csidriver",
                "roles": "role", "clusterroles": "clusterrole", "sccs": "scc"}

# operand kinds enumerated by deleteOwnedProfile (apply.go:939-990); the RBAC
# kinds are installer-owned and NEVER enumerated (nfs.go OperandRBAC comment).
OPERAND_KINDS = [
    ("serviceaccount", False), ("configmap", False), ("lease", False),
    ("service", False), ("deployment", False), ("daemonset", False),
    ("csidriver", True),
]


def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


class NotFound(Exception):
    pass


def obj_path(store, kind, ns, name):
    if kind in CLUSTER_KINDS:
        return os.path.join(store, "cluster", kind, name + ".json")
    return os.path.join(store, "namespaced", ns, kind, name + ".json")


def read(store, kind, ns, name):
    p = obj_path(store, kind, ns, name)
    if not os.path.exists(p):
        return None
    with open(p) as f:
        return json.load(f)


def event(store, actor, op, kind, name, detail=""):
    with open(os.path.join(store, "_events.jsonl"), "a") as f:
        f.write(json.dumps({
            "ts": now(), "actor": actor, "op": op,
            "kind": kind, "name": name, "detail": detail,
        }) + "\n")


def write(store, kind, ns, name, obj, actor, op, detail=""):
    md = obj.setdefault("metadata", {})
 # the API server assigns the UID at creation — stamp it on the first
 # write so operator-created operands carry one too
    md.setdefault("uid", deterministic_uid(kind, ns, md.get("name", "")))
    md.setdefault("creationTimestamp", now())
    md["resourceVersion"] = str(int(md.get("resourceVersion", "0")) + 1)
    p = obj_path(store, kind, ns, name)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    tmp = p + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)
    os.replace(tmp, p)
    event(store, actor, op, kind, name, detail)


def require_ns(kind):
    return kind in NS_KINDS


def deterministic_uid(kind, ns, name):
    return str(uuid.uuid5(uuid.NAMESPACE_URL, "k8s://%s/%s/%s" % (kind, ns or "*", name)))


def stamp(obj, kind, ns):
    md = obj.setdefault("metadata", {})
    md.setdefault("uid", deterministic_uid(kind, ns, md.get("name", "")))
    md.setdefault("resourceVersion", "1")
    md.setdefault("creationTimestamp", now())
    return obj


def deep_merge(dst, src):
    for k, v in src.items():
        if isinstance(v, dict) and isinstance(dst.get(k), dict):
            deep_merge(dst[k], v)
        else:
            dst[k] = copy.deepcopy(v)


def read_dir(dirpath):
    if not os.path.isdir(dirpath):
        return []
    out = []
    for fn in sorted(os.listdir(dirpath)):
        if not fn.endswith(".json"):
            continue  # .json.removed tombstones are gone objects
        out.append(json.load(open(os.path.join(dirpath, fn))))
    return out


def list_kind(store, kind, ns=None):
    if kind in CLUSTER_KINDS:
        return read_dir(os.path.join(store, "cluster", kind))
    if ns:
        return read_dir(os.path.join(store, "namespaced", ns, kind))
    out = []
    nroot = os.path.join(store, "namespaced")
    for n in sorted(os.listdir(nroot) if os.path.isdir(nroot) else []):
        out.extend(read_dir(os.path.join(nroot, n, kind)))
    return out


# ---------------------------------------------------------------------------
# API-validity validation (pin 4: manifests must pass a real API/schema check)
# ---------------------------------------------------------------------------

def validate_seed(obj, kind):
    """Return an error string for an INVALID seed, else "". Mirrors the
    fields the real API server rejects at admission; anything a real kubectl
    create would refuse, this offline store refuses too."""
    spec = obj.get("spec") or {}
    if kind == "volumeattachment":
        if not spec.get("attacher"):
            return "spec.attacher: Required value"
        if not spec.get("nodeName"):
            return "spec.nodeName: Required value"
        src = spec.get("source") or {}
        if not src.get("persistentVolumeName"):
            return "source: exactly one of source.persistentVolumeClaim/source.persistentVolumeName required"
    elif kind == "deployment":
        if not spec.get("selector"):
            return "spec.selector: Required value"
        if not spec.get("template"):
            return "spec.template: Required value"
    elif kind == "persistentvolume":
        if not spec.get("capacity"):
            return "spec.capacity: Required value"
        csi = spec.get("csi") or {}
        if csi and not csi.get("driver"):
            return "spec.csi.driver: Required value"
    elif kind == "persistentvolumeclaim":
        req = ((spec.get("resources") or {}).get("requests") or {})
        if not req.get("storage"):
            return "spec.resources.requests.storage: Required value"
    return ""


# ---------------------------------------------------------------------------
# CLI surface — real kubectl verbs ONLY (get/create/patch/delete)
# ---------------------------------------------------------------------------

def cmd_create(store, args):
    files = []
    out_json = False
    flag_ns = None
    i = 0
    while i < len(args):
        if args[i] == "-f":
            files.append(args[i + 1])
            i += 2
        elif args[i] == "-o" and args[i + 1] == "json":
            out_json = True
            i += 2
        elif args[i] in ("-n", "--namespace"):
 # real kubectl: for -f creates the manifest's own metadata.namespace
 # wins; the flag only fills a document that carries none.
            flag_ns = args[i + 1]
            i += 2
        else:
            raise SystemExit("create: unsupported arg %r" % args[i])
    if not files:
        raise SystemExit("create: -f required")
    for fn in files:
        with open(fn) as f:
            obj = json.load(f)
        kind = obj.get("kind", "").lower()
        kind = KIND_ALIASES.get(kind, kind)
        if kind not in ALL_KINDS:
            raise SystemExit("create: unknown kind %r" % obj.get("kind"))
        err = validate_seed(obj, kind)
        if err:
            raise SystemExit("Error from server (Invalid): %s %r is invalid: %s"
                             % (kind, obj.get("metadata", {}).get("name"), err))
 # status subresource semantics: the API server IGNORES status on
 # create — pre-set Bound/attached never takes effect.
        obj.pop("status", None)
        md = obj["metadata"]
        ns = md.get("namespace") if require_ns(kind) else None
        if require_ns(kind) and not ns and flag_ns:
            ns = flag_ns
            md["namespace"] = flag_ns
        if require_ns(kind) and not ns:
            raise SystemExit("create: namespace-scoped kind %s needs metadata.namespace" % kind)
        name = md.get("name")
        if read(store, kind, ns, name) is not None:
            raise SystemExit("Error from server (AlreadyExists): %s %r" % (kind, name))
        actor = "case"
        write(store, kind, ns, name, stamp(obj, kind, ns), actor, "create", os.path.basename(fn))
        if out_json:
            print(json.dumps(obj, indent=1, sort_keys=True))


def cmd_get(store, args):
    if "-o" in args:
        if args[args.index("-o") + 1] != "json":
            raise SystemExit("get: only -o json supported")
        args = args[:args.index("-o")] + args[args.index("-o") + 2:]
    ns = None
    if "--namespace" in args or "-n" in args:
        flag = "--namespace" if "--namespace" in args else "-n"
        ns = args[args.index(flag) + 1]
        idx = args.index(flag)
        args = args[:idx] + args[idx + 2:]
    if not args:
        raise SystemExit("get: kind required")
    kind = KIND_ALIASES.get(args[0], args[0])
    if kind not in ALL_KINDS:
        raise SystemExit("get: unknown kind %r" % args[0])
    if len(args) > 1:
        if require_ns(kind) and not ns:
            raise SystemExit("get: --namespace required for named %s" % kind)
        obj = read(store, kind, ns, args[1])
        if obj is None:
            print("Error from server (NotFound): %s %r" % (kind, args[1]), file=sys.stderr)
            return 1
        print(json.dumps(obj, indent=1, sort_keys=True))
        return 0
    items = list_kind(store, kind, ns)
    print(json.dumps({"apiVersion": API_VERSION, "kind": kind.capitalize() + "List", "items": items},
                     indent=1, sort_keys=True))
    return 0


def cmd_patch(store, args):
    pty = None
    body = None
    i = 0
    rest = []
    while i < len(args):
        if args[i] == "--type":
            pty = args[i + 1]
            i += 2
        elif args[i] == "-p":
            body = args[i + 1]
            i += 2
        else:
            rest.append(args[i])
            i += 1
    if pty != "merge" or body is None:
        raise SystemExit("patch: only --type merge -p <json> supported")
    if body == "-":
        body = sys.stdin.read()
    ns = None
    if "--namespace" in rest or "-n" in rest:
        flag = "--namespace" if "--namespace" in rest else "-n"
        ns = rest[rest.index(flag) + 1]
        idx = rest.index(flag)
        rest = rest[:idx] + rest[idx + 2:]
    if len(rest) != 2:
        raise SystemExit("patch: need <kind> <name>")
    kind = KIND_ALIASES.get(rest[0], rest[0])
    name = rest[1]
    obj = read(store, kind, ns, name)
    if obj is None:
        print("Error from server (NotFound): %s %r" % (kind, name), file=sys.stderr)
        return 1
    patch_body = json.loads(body)
 # status subresource semantics: a main-resource patch never writes status.
    patch_body.pop("status", None)
    old_spec = json.dumps(obj.get("spec"), sort_keys=True)
    deep_merge(obj, patch_body)
 # API server: a spec change bumps metadata.generation — observedGeneration
 # is what the async-operator poll trusts.
    if json.dumps(obj.get("spec"), sort_keys=True) != old_spec:
        md = obj.setdefault("metadata", {})
        md["generation"] = str(int(md.get("generation", "1")) + 1)
    write(store, kind, ns, name, obj, "case", "patch", body[:200])
    return 0


PLURAL_TO_KIND = {
    "namespaces": "namespace", "nodes": "node",
    "persistentvolumes": "persistentvolume",
    "persistentvolumeclaims": "persistentvolumeclaim",
    "configmaps": "configmap", "secrets": "secret",
    "serviceaccounts": "serviceaccount", "services": "service",
    "pods": "pod", "leases": "lease", "deployments": "deployment",
    "daemonsets": "daemonset", "clusterroles": "clusterrole",
    "clusterrolebindings": "clusterrolebinding",
    "csidrivers": "csidriver", "volumeattachments": "volumeattachment",
    "customresourcedefinitions": "customresourcedefinition",
    "quartets": "quartet",
}


def raw_path_target(raw):
    """Resolve a DeleteOptions raw API path to (kind, ns, name), or None.
    /api/v1/<plural>/<name> | /api/v1/namespaces/<ns>/<plural>/<name> |
    /apis/<group>/<version>/[namespaces/<ns>/]<plural>/<name>."""
    if raw.startswith("/api/v1/"):
        rest = raw[len("/api/v1/"):]
    elif raw.startswith("/apis/"):
        rest = raw[len("/apis/"):].split("/", 2)
        if len(rest) < 3:
            return None
        rest = rest[2]
    else:
        return None
    seg = rest.split("/")
    if len(seg) == 2:
        plural, name = seg
        ns = None
    elif len(seg) == 4 and seg[0] == "namespaces":
        ns, plural, name = seg[1], seg[2], seg[3]
    else:
        return None
    kind = PLURAL_TO_KIND.get(plural)
    if kind is None:
        return None
    return (kind, ns, name)


def cmd_delete_raw(store, raw, body_file):
    """Real DeleteOptions.body UID precondition (CASE-CONTRACT "Gerçek
    delete gövdesi", review 2026-11-12): `delete --raw=<api-path> -f -` —
    the supported-client form. The body must be a DeleteOptions object and
    preconditions.uid is enforced SERVER-SIDE:
      * object absent                         -> NotFound (rc 1);
      * GET-then A->B replacement before the
        delete (uid mismatch)                 -> 409 Conflict (rc 1); B's
        fields are FULLY preserved, nothing removed;
      * match                                 -> deleted / terminating.
    Test arm: <store>/ctl/replace-before-delete carrying a UID swaps the
    object AFTER the caller's get and BEFORE the precondition evaluates."""
    tgt = raw_path_target(raw)
    if tgt is None:
        print("Error from server (NotFound): the server could not find the requested resource",
              file=sys.stderr)
        event(store, "case", "delete-rejected", "-", "-", "raw path not routable: %s" % raw)
        return 1
    kind, ns, name = tgt
    try:
        if body_file == "-" or body_file is None:
            body = json.loads(sys.stdin.read())
        else:
            with open(body_file) as f:
                body = json.loads(f.read())
    except Exception:
        print("error: the body of a raw delete must be a decodable DeleteOptions object",
              file=sys.stderr)
        return 1
    if body.get("kind") != "DeleteOptions":
        print("error: the body of a raw delete must be a DeleteOptions object",
              file=sys.stderr)
        return 1
    prec = ((body.get("preconditions") or {}).get("uid")) or ""

    obj = read(store, kind, ns, name)
    if obj is None:
        print("Error from server (NotFound): %s %r" % (kind, name), file=sys.stderr)
        event(store, "case", "delete", kind, name,
              "raw DeleteOptions: absent (precondition=%s)" % (prec or "-"))
        return 1
    arm = os.path.join(store, "ctl", "replace-before-delete")
    if prec and os.path.exists(arm):
        with open(arm) as f:
            repl_uid = f.read().strip()
        if repl_uid and obj["metadata"].get("uid") != repl_uid:
            obj["metadata"]["uid"] = repl_uid
            obj.setdefault("metadata", {}).setdefault("annotations", {})[
                "scenario-offline/replaced"] = "yes"
            write(store, kind, ns, name, obj, "test-arm", "replace-inject",
                  "swapped-under-delete expected=%s live=%s" % (prec, repl_uid))
    live_uid = obj["metadata"].get("uid")
    if prec and live_uid != prec:
 # 409 Conflict — the server-side precondition failed; the replacement
 # (all its fields) survives untouched.
        print("Error from server (Conflict): %s %r — UID precondition failed: "
              "the object has been modified" % (kind, name), file=sys.stderr)
        event(store, "case", "delete-rejected", kind, name,
              "uid precondition failed 409 (expected=%s live=%s) — nothing removed"
              % (prec, live_uid))
        return 1
    fns = obj["metadata"].get("finalizers") or []
    if fns:
 # API server semantics: a finalizer pins the object — deletion is
 # recorded (deletionTimestamp) but the object stays until removed.
        obj["metadata"]["deletionTimestamp"] = now()
        write(store, kind, ns, name, obj, "case", "delete-accepted",
              "finalizers=%s precondition=%s" % (",".join(fns), prec or "-"))
    else:
        p = obj_path(store, kind, ns, name)
        os.replace(p, p + ".removed")
        event(store, "case", "delete", kind, name,
              "raw DeleteOptions: no finalizers — removed (precondition=%s)" % (prec or "-"))
    return 0


def cmd_delete(store, args):
    ns = None
    if "--namespace" in args or "-n" in args:
        flag = "--namespace" if "--namespace" in args else "-n"
        ns = args[args.index(flag) + 1]
        idx = args.index(flag)
        args = args[:idx] + args[idx + 2:]
    field_sel = None
    raw = None
    body_file = None
    ignore_nf = False
    rest = []
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--field-selector":
            raise SystemExit("delete: --field-selector requires = form here")
        if a.startswith("--field-selector="):
            field_sel = a.split("=", 1)[1]
        elif a == "--raw":
            raw = args[i + 1]
            i += 2
            continue
        elif a.startswith("--raw="):
            raw = a.split("=", 1)[1]
        elif a in ("-f", "--filename"):
            body_file = args[i + 1]
            i += 2
            continue
        elif a == "--ignore-not-found":
            ignore_nf = True
        else:
            rest.append(a)
        i += 1
    if field_sel is not None:
 # Real kubectl refuses name + field-selector BEFORE the API is ever
 # reached; a field selector is not a UID precondition (it only
 # pre-filters a list). The UID precondition travels in the
 # DeleteOptions.body via --raw (cmd_delete_raw), enforced
 # server-side.
        print("error: name cannot be provided when a selector is specified",
              file=sys.stderr)
        event(store, "case", "delete-rejected",
              KIND_ALIASES.get(rest[0], rest[0]) if rest else "-",
              rest[1] if len(rest) > 1 else "-",
              "name+field-selector is not a precondition (client refused)")
        return 1
    if raw is not None:
        return cmd_delete_raw(store, raw, body_file)
    if len(rest) != 2:
        raise SystemExit("delete: need <kind> <name>")
    kind = KIND_ALIASES.get(rest[0], rest[0])
    name = rest[1]
    obj = read(store, kind, ns, name)
    if obj is None:
        if ignore_nf:
            print("No resources found")
            event(store, "case", "delete", kind, name, "absent (no precondition)")
            return 0
        print("Error from server (NotFound): %s %r" % (kind, name), file=sys.stderr)
        return 1
    fns = obj["metadata"].get("finalizers") or []
    if fns:
 # API server semantics: a finalizer pins the object — deletion is
 # recorded (deletionTimestamp) but the object stays until removed.
        obj["metadata"]["deletionTimestamp"] = now()
        write(store, kind, ns, name, obj, "case", "delete-accepted",
              "finalizers=%s" % ",".join(fns))
    else:
        p = obj_path(store, kind, ns, name)
        os.replace(p, p + ".removed")
        event(store, "case", "delete", kind, name, "no finalizers — removed (no precondition)")
    return 0


# ---------------------------------------------------------------------------
# Simulated controllers (the offline adapter's stand-in for real asynchrony):
# binding controller + external-attacher + one operator reconcile pass,
# advanced once per observed API call. NEVER surfaced to the case as a verb.
# ---------------------------------------------------------------------------

def binding_controller_sim(store):
    """k8s binding controller: a PVC naming a free PV is Bound, and the PV
    records the claimRef + phase. Status is set ONLY here — never at create."""
    for pvc in list_kind(store, "persistentvolumeclaim"):
        if pvc.get("status", {}).get("phase") == "Bound":
            continue
        vol = (pvc.get("spec") or {}).get("volumeName")
        if not vol:
            continue
        pv = read(store, "persistentvolume", None, vol)
        if pv is None:
            continue
        claimref = (pv.get("spec") or {}).get("claimRef") or {}
        if claimref and not (claimref.get("namespace") == pvc["metadata"].get("namespace")
                             and claimref.get("name") == pvc["metadata"].get("name")):
            continue  # claimed by someone else
        md = pvc["metadata"]
        pvc.setdefault("status", {})["phase"] = "Bound"
        write(store, "persistentvolumeclaim", md["namespace"], md["name"], pvc,
              "binding-controller", "bind-pvc", vol)
        pv.setdefault("status", {})["phase"] = "Bound"
        pv.setdefault("spec", {})["claimRef"] = {"kind": "PersistentVolumeClaim",
                                                 "namespace": md["namespace"],
                                                 "name": md["name"]}
        write(store, "persistentvolume", None, vol, pv,
              "binding-controller", "bind-pv", md["name"])


def attacher_sim(store):
    """external-attacher: attaches a VolumeAttachment whose spec.attacher
    matches the driver, on a real node, sourced at a driver-identity PV."""
    node_names = {n["metadata"]["name"] for n in list_kind(store, "node")}
    for va in list_kind(store, "volumeattachment"):
        if va.get("status", {}).get("attached"):
            continue
        spec = va.get("spec") or {}
        if spec.get("attacher") != DRIVER_NAME:
            continue
        src = (spec.get("source") or {}).get("persistentVolumeName")
        if not src or read(store, "persistentvolume", None, src) is None:
            continue
        if spec.get("nodeName") not in node_names:
            continue  # no such node: the attacher cannot attach
        md = va["metadata"]
        va.setdefault("status", {})["attached"] = True
        write(store, "volumeattachment", None, md["name"], va,
              "external-attacher", "attach", src)


def advance(store):
    binding_controller_sim(store)
    attacher_sim(store)
    reconcile_pass(store)


# ---------------------------------------------------------------------------
# Operator behavior simulation (one reconcile pass)
# ---------------------------------------------------------------------------

def conditions_of(d, profile):
    st = d.setdefault("status", {})
    if profile:
        st.setdefault("profiles", {}).setdefault("nfs", {}).setdefault("conditions", [])
        return st["profiles"]["nfs"]["conditions"]
    return st.setdefault("conditions", [])


def set_condition(conds, ctype, status, reason, message):
    for c in conds:
        if c["type"] == ctype:
            c.update({"status": status, "reason": reason, "message": message,
                      "lastTransitionTime": now()})
            return
    conds.append({"type": ctype, "status": status, "reason": reason,
                  "message": message, "lastTransitionTime": now()})


def surface_blocked(d, profile, reason, message):
    conds = conditions_of(d, profile)
    if profile:
 # quartet_controller.go:277-282: blocked result → Ready=False,
 # Progressing=False ("blocked until the Ready condition clears"),
 # Degraded=True with the gate reason.
        set_condition(conds, "Ready", "False", reason, message)
        set_condition(conds, "Progressing", "False", reason, "blocked until the Ready condition clears")
        set_condition(conds, "Degraded", "True", reason, message)
    else:
 # apply.go:1278-1283 (setBlockedTermination): aggregate surface,
 # finalizer retained by the caller.
        set_condition(conds, "Ready", "False", reason, message)
        set_condition(conds, "Progressing", "False", reason, "deletion blocked; re-queued")
        set_condition(conds, "Degraded", "True", reason, message)


def surface_ready(d, profile):
    conds = conditions_of(d, profile)
 # quartet_controller.go:273-275 / 301-304
    set_condition(conds, "Ready", "True", REASON_READY, "operands converged" if profile else "all profiles converged")
    set_condition(conds, "Progressing", "False", REASON_READY, "operands converged" if profile else "nothing in progress")
    set_condition(conds, "Degraded", "False", REASON_READY, "no failures observed")


def scan_error(reason, err):
 # usageGateReasonMessage, apply.go:1251-1256 (fail-safe: unknown usage ==
 # active — disable/delete cancelled until the API state is readable).
    return (REASON_DEPENDENCY_MISSING if reason == "network" else REASON_PERMISSION_DENIED,
            'profile nfs: cannot verify active driver storage usage (cluster-wide scan, operand namespace "%s"): %s — fail-safe, disable/delete cancelled until the API state is readable (the fail-safe usage gate)' % (OPERAND_NS, err))


def scan_usage(store):
    """Mirror of scanActiveVolumeUsage, apply.go:1129-1200. Returns
    (reason_or_None, message_or_None, usage_summary). reason set ⇒ fail-safe
    block; usage non-empty ⇒ DataInUse block."""
    flaw_err = os.environ.get("FAKE_API_ERROR", "")
    until = int(os.environ.get("FAKE_API_ERROR_UNTIL_PASS", "0"))  # 0 = forever
    state_path = os.path.join(store, ".scan_passes")
    try:
        with open(state_path) as f:
            passes = int(f.read() or 0)
    except OSError:
        passes = 0
    if flaw_err and (until == 0 or passes < until):
        if flaw_err in ("pv-list", "va-list"):
            r, msg = scan_error("network", "connection refused (test simulation)")
            return (r, msg, "")
        r, msg = scan_error("forbidden", "denied by test policy")
        return (r, msg, "")

 # PVs: cluster-wide list, driver-identity filtered (apply.go:1135-1158).
    driver_pvs = {}
    active_pvs = {}
    pv_names = []
    for pv in list_kind(store, "persistentvolume"):
        csi = (pv.get("spec") or {}).get("csi") or {}
 # pvCarriesDriverIdentity, apply.go:1207-1216
        if csi.get("driver") == DRIVER_NAME or (
                HANDLE_PREFIX and csi.get("volumeHandle", "").startswith(HANDLE_PREFIX)):
            md = pv.get("metadata") or {}
            driver_pvs[pv["metadata"]["name"]] = True
            bound_or_unbinding = (pv.get("status", {}).get("phase") == "Bound"
                                  or ("deletionTimestamp" in md
                                      and (pv.get("spec") or {}).get("claimRef")))
            if bound_or_unbinding:
                active_pvs[pv["metadata"]["name"]] = True
                pv_names.append(pv["metadata"]["name"])

 # PVCs: operand namespace only, Bound, bound to an active driver PV
 # (apply.go:1160-1177).
    pvc_names = []
    for pvc in list_kind(store, "persistentvolumeclaim", ns=OPERAND_NS):
        if pvc.get("status", {}).get("phase") != "Bound":
            continue
        vol = (pvc.get("spec") or {}).get("volumeName", "")
        if vol and active_pvs.get(vol):
            pvc_names.append(pvc["metadata"]["name"])

 # VolumeAttachments: attached on ANY driver-identity PV, phase-blind
 # (apply.go:1184-1197).
    va_names = []
    for va in list_kind(store, "volumeattachment"):
        src = ((va.get("spec") or {}).get("source") or {}).get("persistentVolumeName")
        if src and va.get("status", {}).get("attached") and driver_pvs.get(src):
            va_names.append(va["metadata"]["name"])

 # summarizeUsage, apply.go:1219-1237.
    parts = []
    if pvc_names:
        parts.append("%d bound PVC(s): %s" % (len(pvc_names), ", ".join(pvc_names)))
    if pv_names:
        parts.append("%d driver-produced PV(s) in use cluster-wide: %s" % (len(pv_names), ", ".join(pv_names)))
    if va_names:
        parts.append("%d attached VolumeAttachment(s): %s" % (len(va_names), ", ".join(va_names)))
    return (None, None, ", ".join(parts))


def gate_message(reason, usage, err_msg):
    if reason:
        return err_msg
 # usageGateReasonMessage, apply.go:1255
    return ("profile nfs: %s — active driver usage present (cluster-wide "
            "driver-identity scan, the fail-safe usage gate); explicit override: "
            "spec.profiles.nfs.forceDisable=true" % usage)


def apply_operands(store, q):
    """Mirror of the enabled-profile apply for the two canary operands the
    case observes (SA/CM/lease/Service/DaemonSet exist too on the real
    surface; ownership + delete semantics are identical per kind).
    stampOwnership: apply.go owner-uid annotation."""
    owner = q["metadata"]["uid"]
    dep = read(store, "deployment", OPERAND_NS, DEPLOYMENT_NAME)
    if dep is None:
        write(store, "deployment", OPERAND_NS, DEPLOYMENT_NAME, {
            "apiVersion": "apps/v1", "kind": "Deployment",
            "metadata": {"name": DEPLOYMENT_NAME, "namespace": OPERAND_NS,
                         "labels": {"app.kubernetes.io/name": "nfs-csi-ngxstorage",
                                    "app.kubernetes.io/component": "controller"},
                         "annotations": {OWNER_ANNOTATION: owner}},
            "spec": {"replicas": 1},
        }, "operator", "apply-operand", DEPLOYMENT_NAME)
    drv = read(store, "csidriver", None, CSIDRIVER_NAME)
    if drv is None:
        write(store, "csidriver", None, CSIDRIVER_NAME, {
            "apiVersion": "storage.k8s.io/v1", "kind": "CSIDriver",
            "metadata": {"name": CSIDRIVER_NAME,
                         "annotations": {OWNER_ANNOTATION: owner}},
            "spec": {"attachRequired": True},
        }, "operator", "apply-operand", CSIDRIVER_NAME)


def data_theft_sweep(store):
 # wrong behavior only: violates customer data untouched (apply.go:97-98) — customer test data
 # (PV/PVC/VA) is destroyed at the moment the gate blocks.
    for kind in ("persistentvolume", "persistentvolumeclaim", "volumeattachment"):
        for obj in list_kind(store, kind):
            ns = obj["metadata"].get("namespace") if kind in NS_KINDS else None
            p = obj_path(store, kind, ns, obj["metadata"]["name"])
            os.replace(p, p + ".removed")
            event(store, "operator", "data-theft", kind, obj["metadata"]["name"], "")


def delete_owned(store, q, flaw):
    """Mirror of deleteOwnedProfile + verifyAndDeleteOwned,
    apply.go:939-1075: enumerate operand kinds, delete ONLY objects carrying
    our UID in quartet.ngxstorage.com/owner-uid, never customer storage."""
    owner = q["metadata"]["uid"]
    for kind, cluster in OPERAND_KINDS:
        ns = None if cluster else OPERAND_NS
        for obj in list_kind(store, kind, ns):
            ann = (obj.get("metadata") or {}).get("annotations") or {}
            name = obj["metadata"]["name"]
            if flaw == "unverified-delete":
                pass  # wrong behavior: skip the UID ownership check
            elif ann.get(OWNER_ANNOTATION) != owner:
                continue  # not ours — never deleted (apply.go:961-963)
 # apply.go:1030-1072: fresh re-verify + preconditioned delete.
            p = obj_path(store, kind, ns, name)
            os.replace(p, p + ".removed")
            event(store, "operator", "delete-owned", kind, name,
                  "uid-precondition=%s" % owner)
    return None


def reconcile_pass(store):
    global _pass_count
    _pass_count += 1
    with open(os.path.join(store, ".scan_passes"), "w") as f:
        f.write(str(_pass_count))

    q = read(store, "quartet", None, QUARTET_NAME)
    if q is None:
        return
    md = q["metadata"]
 # async-operator contract: status.observedGeneration is how the case's
 # bounded poll knows the operator saw the latest spec.
    q.setdefault("status", {})["observedGeneration"] = int(md.get("generation", "1"))
    fns = md.get("finalizers") or []
    terminating = "deletionTimestamp" in md
    if not terminating and FINALIZER not in fns:
 # the controller ensures its finalizer on first sight of the CR
        fns.append(FINALIZER)
        md["finalizers"] = fns
        write(store, "quartet", None, QUARTET_NAME, q, "operator", "add-finalizer", FINALIZER)
        return

    flaw = os.environ.get("FAKE_OPERATOR_FLAW", "")
    spec = ((q.get("spec") or {}).get("profiles") or {}).get("nfs") or {}
    force = bool(spec.get("forceDisable"))
    reason, err_msg, usage = scan_usage(store)

    if terminating:
        if flaw == "premature-finalizer-removal":
 # wrong behavior: handleTermination (apply.go:1297-1342) MUST scan
 # first and keep the finalizer while usage/errors exist.
            md["finalizers"] = [f for f in fns if f != FINALIZER]
            write(store, "quartet", None, QUARTET_NAME, q, "operator",
                  "remove-finalizer", "flaw=premature-finalizer-removal")
            gc_if_empty(store, "quartet", None, QUARTET_NAME)
            return
        if not force and (reason or usage):
            r = reason or REASON_DATA_IN_USE
            if flaw == "data-theft":
                data_theft_sweep(store)
            surface_blocked(q, profile=False, reason=r, message=gate_message(r, usage, err_msg))
 # setBlockedTermination, apply.go:1275-1295: status written,
 # finalizer KEPT, caller requeues.
            write(store, "quartet", None, QUARTET_NAME, q, "operator",
                  "blocked-termination", "reason=%s" % r)
            return
        delete_owned(store, q, flaw)
        md["finalizers"] = [f for f in fns if f != FINALIZER]
        write(store, "quartet", None, QUARTET_NAME, q, "operator",
              "remove-finalizer", "clean usage; apply.go:1329-1342")
        gc_if_empty(store, "quartet", None, QUARTET_NAME)
        return

    if spec.get("enabled"):
        apply_operands(store, q)
        surface_ready(q, profile=False)
        surface_ready(q, profile=True)
        write(store, "quartet", None, QUARTET_NAME, q, "operator", "reconcile-ready", "")
        return

 # disabled profile — customer data untouched / fail-safe gate, apply.go:87-99
    if flaw == "premature-success-on-disable":
 # wrong behavior: cleanup runs without the usage scan; the profile
 # reports Ready while labeled usage still exists.
        delete_owned(store, q, flaw)
        surface_ready(q, profile=False)
        surface_ready(q, profile=True)
        write(store, "quartet", None, QUARTET_NAME, q, "operator",
              "premature-success", "flaw=premature-success-on-disable")
        return
    if not force and (reason or usage):
        r = reason or REASON_DATA_IN_USE
        if flaw == "data-theft":
            data_theft_sweep(store)
        surface_blocked(q, profile=True, reason=r, message=gate_message(r, usage, err_msg))
        surface_blocked(q, profile=False, reason=r, message=gate_message(r, usage, err_msg))
        write(store, "quartet", None, QUARTET_NAME, q, "operator",
              "disabled-gate-blocked", "reason=%s" % r)
        return
    delete_owned(store, q, flaw)
    surface_ready(q, profile=False)
    surface_ready(q, profile=True)
    write(store, "quartet", None, QUARTET_NAME, q, "operator",
          "disabled-clean-recovery", "apply.go:99-107")


def gc_if_empty(store, kind, ns, name):
    """API server GC: a terminating object with no finalizers left is
    actually removed."""
    obj = read(store, kind, ns, name)
    if obj is None:
        return
    if "deletionTimestamp" in obj["metadata"] and not (obj["metadata"].get("finalizers") or []):
        p = obj_path(store, kind, ns, name)
        os.replace(p, p + ".removed")
        event(store, "operator", "gc", kind, name, "finalizers empty")


# ---------------------------------------------------------------------------

_pass_count = 0


def main():
    argv = sys.argv[1:]
    if not argv or argv[0] != "--store":
        raise SystemExit("usage: fake_kubectl.py --store <dir> <kubectl-verb> ...")
    store = os.path.abspath(argv[1])
    verb, args = argv[2], argv[3:]
 # Real kubectl verb surface ONLY. There is no kubectl "reconcile" verb —
 # the operator is async; invented RPC verbs are refused exactly as a real
 # CLI would refuse them, before any store change.
    handlers = {
        "create": cmd_create, "get": cmd_get, "patch": cmd_patch,
        "delete": cmd_delete,
    }
    if verb not in handlers:
        raise SystemExit(
            "fake kubectl: unsupported verb %r — not a real kubectl verb; "
            "refused before any store change (the live operator is async and "
            "is observed via status/observedGeneration, never commanded)" % verb)
    rc = handlers[verb](store, args) or 0
    advance(store)
    sys.exit(rc)


if __name__ == "__main__":
    main()
