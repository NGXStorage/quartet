#!/usr/bin/env python3
"""Canonical execution manifest generator + validator.

Splits the discovery inventory (role/kind/ns/name/uid/rv/semanticHash TSV)
into the canonical execution manifest:

    role  apiVersion  kind  namespace  name  uid  resourceVersion
    semanticSha256  snapshotId

Roles:
  DELETE     — exactly what retirement removes (UID-preconditioned).
  PROTECTED  — kept in the cluster on purpose (Secret
               nfs-csi-ngxstorage-config, ConfigMap
               nfs-csi-ngxstorage-coordination-config): the restore
               boundary; never deleted, never exported.
  NEIGHBOR   — not ours (content-hash baseline for the postcondition).
  PLATFORM-MANAGED — OCP auto-managed (per-SA dockercfg secrets): kept,
               recreated by OCP with the SA; never in DELETE.
  CONSUMER-* — consumers found by the widened scan (must be empty).
  INVENTORY-ERROR — 403/timeout/missing scope; a manifest carrying one of
               these is invalid (never "empty").

The old discovery TSVs are NOT edited in place: the generator writes
SUPERSEDED.tsv recording their sha256 and points at the new manifest.
validate_manifest() is imported by tests/retirement/execution-manifest-test.sh and
enforces: PROTECTED∩DELETE empty, no Namespace/Secret-data in DELETE,
no duplicate (kind,ns,name), every row complete with UID+RV+hash,
CONSUMER/INVENTORY-ERROR rows absent.
"""
import hashlib, os, sys

API_BY_KIND = {
    "secret": "v1", "configmap": "v1", "serviceaccount": "v1", "service": "v1",
    "pod": "v1", "replicaset": "apps/v1", "deployment": "apps/v1",
    "daemonset": "apps/v1", "statefulset": "apps/v1", "job": "batch/v1",
    "role": "rbac.authorization.k8s.io/v1",
    "rolebinding": "rbac.authorization.k8s.io/v1",
    "clusterrole": "rbac.authorization.k8s.io/v1",
    "clusterrolebinding": "rbac.authorization.k8s.io/v1",
    "lease": "coordination.k8s.io/v1", "csidriver": "storage.k8s.io/v1",
    "sc": "storage.k8s.io/v1", "scc": "security.openshift.io/v1",
}

PROTECTED_NAMES = {
    ("secret", "nfs-csi-ngxstorage-config"),
    ("configmap", "nfs-csi-ngxstorage-coordination-config"),
}

# OpenShift auto-creates an image-pull dockercfg Secret for every
# ServiceAccount and deletes/recreates it with the SA. Name contract:
# <sa-name>-dockercfg-<5 lowercase alphanumeric>. These are
# PLATFORM-MANAGED: kept out of DELETE, recreated by OCP when the SA
# comes back.
import re
DOCKERCFG = re.compile(r"^[a-z0-9-]+-dockercfg-[a-z0-9]{5}$")

HEADER = ["role", "apiVersion", "kind", "namespace", "name", "uid",
          "resourceVersion", "semanticSha256", "snapshotId"]


def file_sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


# Discovery roles the inventory producer may emit. Anything else is a
# schema violation (strict schema: unknown roles must fail, never pass through).
INVENTORY_ROLES = ("ALLOWLIST", "NEIGHBOR")
INVENTORY_BAD = ("CONSUMER", "INVENTORY-ERROR")


def load_inventory(path):
    """Strict (strict schema): any malformed row, unknown role, scan error, or found
    consumer aborts BEFORE anything is written. A flawed scan can never
    become a 'valid empty manifest'."""
 # First pass: collect the uids the scan classified ALLOWLIST (the
 # retirement set). A CONSUMER-* row whose uid is IN that set is a
 # self-referential internal cross-reference (e.g. the stack's own
 # RoleBinding -> its SA) and is retired with everything else. A
 # CONSUMER-* row whose uid is NOT in the set is a genuine external
 # consumer and must block manifest production (strict schema fail-closed).
    all_rows = []
    allow_uids = set()
    with open(path) as f:
        header = f.readline().rstrip("\n")
        want = ["role", "kind", "namespace", "name", "uid", "resourceVersion", "semanticSha256"]
        if header.split("\t") != want:
            sys.exit(f"inventory header changed: {header!r} — regenerate the inventory first")
        for n, line in enumerate(f, start=2):
            if line.rstrip("\n") == "":
                continue
            p = line.rstrip("\n").split("\t")
            if len(p) != 7 or any(x == "" for x in p):
                sys.exit(f"inventory line {n} malformed ({len(p)} fields, empty cells): {p!r}")
            role = p[0]
            if role not in INVENTORY_ROLES + INVENTORY_BAD and not role.startswith(INVENTORY_BAD):
                sys.exit(f"inventory line {n} has unknown role {role!r}")
            if role == "ALLOWLIST":
                allow_uids.add(p[4])
            all_rows.append((n, p))

    rows = []
    for n, p in all_rows:
        role = p[0]
        if role.startswith(("CONSUMER", "INVENTORY-ERROR")):
            if role.startswith("INVENTORY-ERROR"):
                sys.exit(f"inventory line {n} carries {role} {p[1:]!r} — the scan FAILED; "
                         f"a manifest MUST NOT be produced from a flawed scan")
 # CONSUMER-*: block unless it is itself part of the retirement set.
            if p[4] not in allow_uids:
                sys.exit(f"inventory line {n} carries an EXTERNAL consumer {role} "
                         f"{p[1:]!r} (uid not in the DELETE set) — a manifest MUST NOT "
                         f"be produced while a live consumer exists")
 # Internal self-reference: it is retired with the allowlist set,
 # so carry it as NEIGHBOR-equivalent metadata, never as a row
 # the closure/validator would misread.
            continue
        rows.append(p)
    return rows


def build_manifest(inventory_path, out_path):
    rows = load_inventory(inventory_path)  # fail-closed before any write
    snap = file_sha256(inventory_path)
    out_rows = []
    for role, kind, ns, name, uid, rv, hv in rows:
        if kind.lower() not in API_BY_KIND:
            sys.exit(f"unknown kind in inventory: {kind!r} — extend API_BY_KIND deliberately")
        if (kind.lower(), name) in PROTECTED_NAMES and role == "ALLOWLIST":
            role = "PROTECTED"
        elif role == "ALLOWLIST" and kind.lower() == "secret" and DOCKERCFG.match(name):
            role = "PLATFORM-MANAGED"
        elif role == "ALLOWLIST":
            role = "DELETE"
        out_rows.append([role, API_BY_KIND[kind.lower()], kind, ns, name,
                         uid, rv, hv, snap])
 # Only after the whole scan validated do we publish the file.
    with open(out_path, "w") as f:
        f.write("\t".join(HEADER) + "\n")
        for r in out_rows:
            f.write("\t".join(r) + "\n")
    return out_rows


def validate_manifest(path):
    """Return a list of violations (empty = valid)."""
    v = []
    seen = {}
    with open(path) as f:
        header = f.readline().rstrip("\n").split("\t")
        if header != HEADER:
            return [f"header mismatch: {header}"]
        for n, line in enumerate(f, start=2):
            p = line.rstrip("\n").split("\t")
            if len(p) != 9:
                v.append(f"line {n}: incomplete row ({len(p)} fields)")
                continue
            role, av, kind, ns, name, uid, rv, hv, snap = p
            if role not in ("DELETE", "PROTECTED", "PLATFORM-MANAGED", "NEIGHBOR"):
                v.append(f"line {n}: unknown role {role!r}")
            if not uid or not rv or not hv or not snap:
                v.append(f"line {n}: missing uid/rv/hash/snapshot")
            if role == "DELETE":
                if kind.lower() == "namespace":
                    v.append(f"line {n}: Namespace in DELETE set — namespaces are never retired")
                if kind.lower() == "secret":
                    v.append(f"line {n}: Secret {name} in DELETE set — runtime secrets are protected")
 # Protected-name policy is validated INDEPENDENTLY of the
 # generator's relabel (strict schema): a protected object must never
 # reach DELETE even if a duplicate PROTECTED row is absent.
                if (kind.lower(), name) in PROTECTED_NAMES:
                    v.append(f"line {n}: protected name {kind}/{name} in DELETE set — "
                             f"it is the restore boundary and must be PROTECTED")
            key = (av, kind, ns, name)
            if key in seen:
                v.append(f"line {n}: duplicate identity {key} (first at line {seen[key]})")
            seen[key] = n
    delete = set()
    protected = set()
    with open(path) as f:
        f.readline()
        for line in f:
            p = line.rstrip("\n").split("\t")
            if len(p) == 9:
                if p[0] == "DELETE":
                    delete.add((p[1], p[2], p[3], p[4]))
                elif p[0] == "PROTECTED":
                    protected.add((p[1], p[2], p[3], p[4]))
    overlap = delete & protected
    if overlap:
        v.append(f"PROTECTED∩DELETE not empty: {sorted(overlap)}")
    return v


def supersede(old_dir, out_path):
    lines = ["superseded\tsha256\treplaced-by"]
    for name in sorted(os.listdir(old_dir)):
        if name.startswith("k8s-") or name.startswith("ocp2-"):
            lines.append(f"{name}\t{file_sha256(os.path.join(old_dir, name))}\t"
                         f"{os.path.basename(out_path)} (manifests/ + SUPERSEDED.tsv)")
    with open(os.path.join(old_dir, "SUPERSEDED.tsv"), "w") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    inv, out = sys.argv[1], sys.argv[2]
    rows = build_manifest(inv, out)
    violations = validate_manifest(out)
    for x in violations:
        print("VIOLATION:", x)
    n_delete = sum(1 for r in rows if r[0] == "DELETE")
    n_prot = sum(1 for r in rows if r[0] == "PROTECTED")
    n_plat = sum(1 for r in rows if r[0] == "PLATFORM-MANAGED")
    print(f"manifest {os.path.basename(out)}: DELETE={n_delete} PROTECTED={n_prot} "
          f"PLATFORM-MANAGED={n_plat} NEIGHBOR={len(rows)-n_delete-n_prot-n_plat} "
          f"violations={len(violations)}")
    sys.exit(1 if violations else 0)
