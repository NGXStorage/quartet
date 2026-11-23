#!/usr/bin/env python3
"""Retirement closure checker.

Read-only check that every object the retirement DELETE+PROTECTED sets
reference (RBAC roleRef/subjects, pod template refs, SA secrets) resolves
to a known object. Run in one of two modes:

  --fixture objects.json : offline; objects keyed by
                           "apiVersion/kind/namespace/name" (namespace "-"
                           for cluster-scoped). Used by the negative tests.
  --cluster <k8s|ocp2>   : live; fetches each allowlisted object with
                           kubectl/oc (--context for ocp2). Any fetch
                           error (403/timeout/missing scope) is
                           FETCH-ERROR and FAILS the check — an
                           unverified reference set is never "closed".

Reference resolution rules (real Kubernetes schemas):
  - RoleBinding/ClusterRoleBinding: roleRef and subjects are TOP LEVEL,
    not under .spec (the earlier bug that made REF-MISS vacuous).
  - roleRef kind ClusterRole -> cluster scope; Role -> the BINDING's
    namespace (explicit contract; no cross-namespace match).
  - subjects: kind ServiceAccount with the subject's OWN namespace; if
    absent, the binding's namespace (k8s defaulting contract).
  - pod templates: containers AND initContainers — env/envFrom
    secretRef/configMapRef, env valueFrom secretKeyRef/configMapKeyRef,
    volumes secret/configMap/projected (secretSource+configMapSource),
    persistentVolumeClaim claimName; serviceAccountName;
    imagePullSecrets.
  - ServiceAccount: .secrets[] in the SA's namespace.
The known set is DELETE ∪ PROTECTED ∪ proven-external rows from the
canonical execution manifest (apiVersion/kind/ns/name columns).

Exit 0 = closure OK; exit 1 = REF-MISS or FETCH-ERROR (fail-closed).
"""
import json, subprocess, sys

CORE = "v1"
RBAC = "rbac.authorization.k8s.io/v1"
COORD = "coordination.k8s.io/v1"
STORAGE = "storage.k8s.io/v1"
SCC = "security.openshift.io/v1"
APPS = "apps/v1"
API_BY_KIND = {
    "Secret": CORE, "ConfigMap": CORE, "Service": CORE,
    "ServiceAccount": CORE, "Pod": CORE, "PersistentVolumeClaim": CORE,
    "Role": RBAC, "RoleBinding": RBAC, "ClusterRole": RBAC,
    "ClusterRoleBinding": RBAC, "Lease": COORD, "CSIDriver": STORAGE,
    "StorageClass": STORAGE, "SecurityContextConstraints": SCC,
    "Deployment": APPS, "DaemonSet": APPS, "StatefulSet": APPS,
    "ReplicaSet": APPS,
}
WORKLOADS = ("Deployment", "DaemonSet", "StatefulSet", "ReplicaSet", "Pod")

_KINDS = {
    "deployment": "Deployment", "daemonset": "DaemonSet",
    "statefulset": "StatefulSet", "replicaset": "ReplicaSet", "pod": "Pod",
    "rolebinding": "RoleBinding", "clusterrolebinding": "ClusterRoleBinding",
    "clusterrole": "ClusterRole", "role": "Role",
    "serviceaccount": "ServiceAccount", "secret": "Secret",
    "configmap": "ConfigMap", "persistentvolumeclaim": "PersistentVolumeClaim",
    "lease": "Lease", "csidriver": "CSIDriver", "sc": "StorageClass",
    "scc": "SecurityContextConstraints", "service": "Service",
}
KIND_CANON = {k.lower(): k for k in _KINDS.values()} | {k: v for k, v in _KINDS.items()}

def canon_kind(kind):
 # Manifest/inventory kinds are kubectl-style lowercase; the reference
 # logic below matches canonical casing.
    return KIND_CANON.get(kind.lower(), kind)

def key(av, kind, ns, name):
 # "::" separator: apiVersion itself contains "/" (group/version), so a
 # "/" join is ambiguous. No apiVersion/kind/namespace/name can contain
 # "::".
    return f"{av}::{kind}::{ns}::{name}"

SEP = "::"

CANON_HEADER = ["role", "apiVersion", "kind", "namespace", "name", "uid",
                "resourceVersion", "semanticSha256", "snapshotId"]
ROLES = ("DELETE", "PROTECTED", "PLATFORM-MANAGED", "NEIGHBOR")


def load_allow(path):
 # Strict canonical manifest schema (canonical manifest schema): the 9-column header, full rows
 # and known roles — unknown/malformed rows are never silently skipped.
 # retire: objects we act on — their references must be extracted.
 # resolve: the whole inventory — a reference may point at a surviving
 # object (PROTECTED / PLATFORM-MANAGED / NEIGHBOR), not only a
 # restored one; only objects ABSENT from the inventory are misses.
    retire, resolve = set(), set()
    with open(path) as f:
        header = f.readline().rstrip("\n").split("\t")
        if header != CANON_HEADER:
            sys.exit(f"manifest header is not the canonical 9-column schema: {header!r}")
        for n, line in enumerate(f, start=2):
            if line.rstrip("\n") == "":
                continue
            p = line.rstrip("\n").split("\t")
            if len(p) != 9 or any(x == "" for x in p):
                sys.exit(f"manifest line {n} malformed: {p!r}")
            role, av, kind, ns, name = p[0], p[1], canon_kind(p[2]), p[3], p[4]
            if role not in ROLES:
                sys.exit(f"manifest line {n} has unknown role {role!r}")
            ident = key(av, kind, ns, name)
            if role in ("DELETE", "PROTECTED"):
                retire.add(ident)
            resolve.add(ident)
    return retire, resolve

def extract_refs(obj, kind, ns):
    """Yield (apiVersion, refkind, refns, refname) for one object."""
    kind = canon_kind(kind)
    refs = []
    if kind in ("RoleBinding", "ClusterRoleBinding"):
        rr = obj.get("roleRef") or {}
        if rr.get("kind") == "ClusterRole":
            refs.append((RBAC, "ClusterRole", "-", rr.get("name")))
        elif rr.get("kind") == "Role":
            refs.append((RBAC, "Role", ns, rr.get("name")))  # binding's ns
        for s in obj.get("subjects") or []:
            sname = s.get("name")
            if not sname:
                continue
            if s.get("kind") == "ServiceAccount":
                refs.append((CORE, "ServiceAccount", s.get("namespace") or ns, sname))
            elif s.get("kind") == "Group":
                pass  # built-in groups are external by contract
            else:
                refs.append((None, s.get("kind"), s.get("namespace") or ns, sname))
    if kind in WORKLOADS:
        if kind == "Pod":
            ps = obj.get("spec") or {}
        else:
            ps = (((obj.get("spec") or {}).get("template") or {}).get("spec")) or {}
        if ps.get("serviceAccountName"):
            refs.append((CORE, "ServiceAccount", ns, ps["serviceAccountName"]))
        for sec in ps.get("imagePullSecrets") or []:
            refs.append((CORE, "Secret", ns, sec.get("name")))
        for c in list(ps.get("containers") or []) + list(ps.get("initContainers") or []):
            for ef in c.get("envFrom") or []:
                if "secretRef" in ef:
                    refs.append((CORE, "Secret", ns, ef["secretRef"].get("name")))
                if "configMapRef" in ef:
                    refs.append((CORE, "ConfigMap", ns, ef["configMapRef"].get("name")))
            for e in c.get("env") or []:
                vf = e.get("valueFrom") or {}
                if vf.get("secretKeyRef"):
                    refs.append((CORE, "Secret", vf["secretKeyRef"].get("namespace") or ns,
                                 vf["secretKeyRef"].get("name")))
                if vf.get("configMapKeyRef"):
                    refs.append((CORE, "ConfigMap", vf["configMapKeyRef"].get("namespace") or ns,
                                 vf["configMapKeyRef"].get("name")))
        for v in ps.get("volumes") or []:
            if v.get("secret"):
                refs.append((CORE, "Secret", ns, v["secret"].get("secretName")))
            if v.get("configMap"):
                refs.append((CORE, "ConfigMap", ns, v["configMap"].get("name")))
            if v.get("projected"):
                for src in v["projected"].get("sources") or []:
                    if src.get("secret"):
                        refs.append((CORE, "Secret", ns, src["secret"].get("name")))
                    if src.get("configMap"):
                        refs.append((CORE, "ConfigMap", ns, src["configMap"].get("name")))
            if v.get("persistentVolumeClaim"):
                refs.append((CORE, "PersistentVolumeClaim", ns,
                             v["persistentVolumeClaim"].get("claimName")))
    if kind == "ServiceAccount":
        for s in obj.get("secrets") or []:
            refs.append((CORE, "Secret", ns, s.get("name")))
    return [r for r in refs if r[3]]

def main():
    args = sys.argv[1:]
    mode, fixture, cluster, ctx = "fixture", None, None, None
    allow = None
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--fixture":
            i += 1; fixture = args[i]
        elif a == "--cluster":
            i += 1; cluster = args[i]; mode = "cluster"
        elif a == "--context":
            i += 1; ctx = args[i]
        elif a == "--allow":
            i += 1; allow = args[i]
        else:
            sys.exit(f"unknown arg {a}")
        i += 1
    if not allow:
        sys.exit("usage: closure-check.py --allow <manifest.tsv> (--fixture objects.json | --cluster k8s|ocp2 [--context c])")
    retire, resolve = load_allow(allow)

    objects = {}
    if mode == "fixture":
        objects = json.load(open(fixture))
 # fixture keys are identity strings; values carry kind+namespace
 # metadata for ref extraction: store as (kind, ns, obj)
        parsed = {}
        for k, v in objects.items():
            av, kind, ns, name = k.split(SEP, 3)
            parsed[(kind, ns, name)] = (av, v)
        fetch_err = 0
        checked = 0
        misses = []
 # canonical manifest schema: fixture mode is fail-closed like live mode — EVERY retire root
 # must be present in the fixture; a missing root is a FETCH-ERROR,
 # not a silent skip (empty fixture + populated retire set must fail).
        fixture_ids = set()
        for (kind, ns, name), (av, obj) in parsed.items():
            fixture_ids.add(key(av, canon_kind(kind), ns, name))
 # `retire` already carries canon-normalized identities.
        for missing in sorted(retire - fixture_ids):
            fetch_err += 1
            misses.append(f"FETCH-ERROR missing-from-fixture: {missing.replace(SEP, '/')}")
        for (kind, ns, name), (av, obj) in parsed.items():
            if key(av, canon_kind(kind), ns, name) not in retire:
                continue  # only the retirement set's references are checked
            for r in extract_refs(obj, kind, ns):
                checked += 1
                if r[0] is None:
                    misses.append(f"REF-UNKNOWN-KIND {kind}/{ns}/{name} -> subject kind {r[1]}/{r[3]}")
                    continue
                if key(r[0], r[1], r[2], r[3]) not in resolve:
                    misses.append(f"REF-MISS {kind}/{ns}/{name} -> {r[0]}/{r[1]}/{r[2]}/{r[3]}")
        print(f"objects-expected: {len(retire)}, objects-present: {len(parsed)}, "
              f"fetch-errors: {fetch_err}, refs-checked: {checked}")
        for m in misses:
            print(m)
        print("closure:", "OK" if not misses else f"FAIL {len(misses)}")
        sys.exit(1 if misses else 0)

    binp = "kubectl" if cluster == "k8s" else "oc"
    argv = [binp] + (["--context", ctx] if ctx else [])
    checked = 0
    misses = []
    fetch_err = 0
    for k in retire:
        av, kind, ns, name = k.split(SEP, 3)
        cmd = argv + ["get", kind] + (["-n", ns] if ns != "-" else []) + [name, "-o", "json"]
        o = subprocess.run(cmd, capture_output=True, text=True)
        if o.returncode != 0:
            fetch_err += 1
            misses.append(f"FETCH-ERROR {kind}/{ns}/{name}: {o.stderr.strip()[:120]}")
            continue
        obj = json.loads(o.stdout)
        for r in extract_refs(obj, kind, ns):
            checked += 1
            if r[0] is None:
                misses.append(f"REF-UNKNOWN-KIND {kind}/{ns}/{name} -> subject kind {r[1]}/{r[3]}")
                continue
            if key(r[0], r[1], r[2], r[3]) not in resolve:
                misses.append(f"REF-MISS {kind}/{ns}/{name} -> {r[0]}/{r[1]}/{r[2]}/{r[3]}")
    print(f"objects-checked: {len(retire)}, refs-checked: {checked}")
    for m in misses:
        print(m)
    print("closure:", "OK" if not misses else f"FAIL {len(misses)}")
    sys.exit(1 if misses else 0)

if __name__ == "__main__":
    main()
