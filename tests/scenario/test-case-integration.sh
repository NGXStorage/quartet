#!/usr/bin/env bash
# test-case-integration.sh — Phase B case-integration contract (convergence gate).
#
# Proves, WITHOUT any live cluster, that the FIVE REAL case bodies under
# cases/*/ and the REAL common lib (lib/common.sh — sourced here, never a stub)
# are wired to the real kubectl/API protocol, using a RECORDING + STRICT
# kubectl-or-API fixture generated into scratch at runtime. Method mirrors
# test-runner-contract.sh / test-entrypoint-contract.sh: assertions observe
# what the fixture recorded (events.log) and what survived in the object store
# — grepping body/lib source is not integration proof.
#
# The fixture is NOT a stub case template: it implements real kubectl argv
# semantics (create/get/list/apply/replace/patch/delete/exec, --context, -n,
# -o json, --ignore-not-found, `--` pod-arg separation), real API strictness
# (required fields per kind — a VolumeAttachment without spec.attacher or a
# Deployment without selector/template is rejected exactly like the API
# server; namespaced objects cannot be created in an absent namespace), and
# the REAL client/API delete model (CASE-CONTRACT "Gerçek delete gövdesi",
# ):
# * `delete <kind> <name> --field-selector=metadata.uid=<uid>` is NEVER
# accepted: real kubectl refuses it client-side with "name cannot be
# provided when a selector is specified" (rc 1, nothing reaches the API,
# nothing is removed) — a field selector is not a UID precondition.
# * the UID precondition travels in the DeleteOptions.body via the
# supported-client form `delete --raw=<api-path> -f -` (body
# {"kind":"DeleteOptions",...,"preconditions":{"uid":<uid>}}), enforced
# SERVER-SIDE: object absent → NotFound rc 1; a GET-then A→B replacement
# before the delete (uid mismatch) → 409 Conflict rc 1 with B's fields
# FULLY preserved and NO deletion record; match → deleted/terminating.
# * objects carrying finalizers are not removed — deletionTimestamp is set
# (terminating) until the finalizer is gone.
# * unknown verbs (including any synthetic `reconcile`) are rejected exactly
# like real kubectl — the fixture never presents a fake control-plane RPC
# as the live API (CASE-CONTRACT pin 4). Operator behavior is emulated as
# a watch-triggered reconciler: every API call advances ONE reconcile pass
# per Quartet CR (plus the k8s volume controllers: PV/PVC binding by
# driver identity sets claimRef/Bound, the attacher sets VA attached), so
# bodies observe status/observedGeneration through bounded polling
# against real-looking API responses. Precondition state is seeded
# out-of-band per case (installed cluster: CRD + node + installer RBAC
# set; W4 additionally the live converged singleton) — matching each
# case.json precondition, and the bodies' own readiness probes.
#
# Covered (each negative genuinely bites, then is restored — probes use fresh
# per-probe stores, arms live in ctl/ files a probe run consumes, and restore
# assertions rerun on disarmed stores):
# the fixture itself behaves like kubectl + a strict API server;
# each real case body + real lib converges to PASS (rc=0) under the
# strict API, with every deletion scoped to run-created (or fixture-
# provisioned) UIDs, no event outside the authorized namespaces (TEST_
# NAMESPACE ns-a + canonical operand ns ngxstorage-nfs), no run-created
# object surviving the finished run (cleanup on every exit path, pin 8),
# and no offline-stub fallback ("NOT RUN");
# wrong-behavior injection — the operator stamps a WRONG reason or
# WRONG condition status and the case must report FAIL (rc=1) with a
# visible assertion failure, never PASS;
# authority on the REAL lib surface: foreign-namespace mutation
# by flag, by manifest, cluster-scoped kind from a namespaced case,
# global --context override, and — tri-state kind lookup (follow-up
# ) — UNKNOWN kinds, the real OpenShift SCC
# (security.openshift.io/v1 SecurityContextConstraints), the cluster
# Quartet, and manifests carrying cluster/unknown kinds (incl. nested
# List items) are all refused BEFORE dispatch with ZERO cluster events
# (rc=2, events.log empty); in-scope Pod/ConfigMap calls and `exec --`
# pod-arg passthrough still work;
# (composition audit) UID replacement on ALL FIVE real bodies — the Quartet object is
# swapped under the case (new UID, marker annotation); the run must
# not PASS, must not delete the replaced object, and the replacement
# must survive (the pinned delete helper refuses it with rc 2);
# cleanup ownership — a pre-run foreign object (fixed UID) is seeded
# into TEST_NAMESPACE; it survives the whole run and is never deleted;
# fail-closed — the fixture SIGTERMs the case shell mid-run;
# the case must not exit 0 (catches `FINAL_RC=PASS` default +
# `trap 'exit $FINAL_RC' EXIT` masking), then a disarmed rerun restores;
# the REAL run.sh single-case dispatch returns the same exit protocol
# as the direct run (runner seam);
# the pinned atomic cleanup surface (scenario_create_tracked /
# scenario_delete_owned — CASE-CONTRACT "Ortak atomik cleanup helper")
# driven through the REAL lib gate + recording fixture: response-UID
# binding and ownership.tsv evidence rows, swap refusal (rc 2, object
# survives), postcondition-verified delete, absence idempotency, the
# GET→A→B replacement race answered by the server-side DeleteOptions.
# body precondition (409 — B fully preserved, no deleted/PASS record),
# and a post-gate context drift that registers nothing (refusal ≠
# success); plus the CLUSTER-scoped Quartet on the real CRD scope
# (spec.scope: Cluster): no namespace ever emitted, the EXACT cluster
# raw route asserted, --namespace misuse refused, and the GET→A→B race
# answered 409 on the cluster route (UID body proven to travel);
# (S8b) the real-kubectl TRANSPORT is kept a SEPARATE record from the
# API-server precondition enforcement: BLOCKED with an honest reason
# when the loopback socket is restricted or the binary absent — never
# a fake PASS;
# (S9) gate refusal is never a success on ALL FIVE real bodies — an
# under-granted run (allow-isolated vs the cluster-scoped-fault effect
# tier) is BLOCKED rc 2 with zero cluster events.
# (real CRD scope) the fixture contract is bound to the REAL CRD scope — spec.scope is
# parsed INDEPENDENTLY from config/crd/bases/
# quartet.ngxstorage.com_quartets.yaml (not the same constant re-
# asserted twice, which is how the namespaced model went undetected),
# and every model — canonical lib table, all five fixture tables, the
# API-fixture routing, the synthetic CRD seed, and the offline fake —
# is asserted to model the parsed value.
#
# Scratch only under $HOME/.cache/ngxstorage; the fixture tree is MOVED to
# the machine trash on exit (no-rm rule), never hard-deleted. No git ops, no
# live cluster: KUBE_BIN resolves to the recording fixture via PATH.
#
# PROTOCOL DEPENDENCY (parent-pin, 2026-11-12): the fixture models the REAL
# client/API delete contract — `delete --raw=<api-path> -f -` carrying the
# UID precondition in DeleteOptions.body, server-side enforced (409 on a
# GET-then A→B replacement; name+field-selector refused client-side, never
# accepted) — and CONSUMES the pinned lib/common.sh protocol (controlled GVK
# raw path, strict API reason classification, tightened target/scope gate).
# This script is the convergence gate: if the lib regresses to the old
# name+field-selector form or a substring "NotFound" classification, the S1
# convergence, authority gate and delete-argv assertions bite immediately.
set -uo pipefail
scen="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$scen/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"; mkdir -p "$cache"
fx="$(mktemp -d "$cache/quartet-integration.XXXXXX")"
export fx
trap 'fixture_to_trash' EXIT
fixture_to_trash() {
  [ -d "$fx" ] || return 0
  mkdir -p "$HOME/.cache/opencode/trash"
  mv "$fx" "$HOME/.cache/opencode/trash/$(date +%Y%m%d-%H%M%S)-integration-$$" 2>/dev/null \
    || echo "note: fixture trash move failed; left at $fx (hard-delete yok)" >&2
}

# ---- the REAL common lib (this contract runs it, never a stub) -------------
source "$scen/lib/common.sh"
# Export the lib's ENTIRE helper surface (contracted entry points + its
# internals, e.g. _scenario_effect_tier) — exactly what the runner's
# `export -f` injects into each case process. Prefix-scoped so test functions
# of this contract are never exported.
for _fn in $(declare -F | awk '{print $3}'); do
  case "$_fn" in scenario_*|_scenario_*) export -f "$_fn" ;; esac
done
export SCENARIO_ROOT REPO_ROOT

ok=0; fail=0
assert() { # $1 = behavioral message, $2 = shell condition
  if eval "$2" 2>/dev/null; then echo "OK   $1"; ok=$((ok + 1)); else echo "FAIL $1"; fail=$((fail + 1)); fi
}

# ---- fixture generation (recording + strict kubectl-or-API) ----------------
mkdir -p "$fx/bin" "$fx/sc"
cat > "$fx/api_fixture.py" <<'PYFIX'
#!/usr/bin/env python3
"""Recording/strict kubectl-or-API fixture (test-case-integration.sh).

Real kubectl argv semantics + API strictness over a file-backed object store,
with a watch-triggered operator emulation (one reconcile pass per API call).
EVERY invocation — accepted or rejected server-side — appends one line to
$SCENARIO_FAKE_STORE/events.log, so an empty log proves "zero cluster events":
  inv <seq> verb=<v> <fields> rc=<r> note=<note|->
fields per verb: create -> ns= kind= name= uid=; get -> ns= kind= name= uid=
(exec adds podargs=<json>); delete -> ns= kind= name= target_uid=
precondition= outcome=; seed -> ns= kind= name= uid=.
Maintenance channel (NOT a kubectl verb, never logged): `fixture __inspect
list` dumps the object store for the test's assertions.
"""
import json, os, sys, uuid, hashlib, signal, subprocess

STORE = os.environ.get("SCENARIO_FAKE_STORE")
if not STORE:
    sys.stderr.write("fixture: SCENARIO_FAKE_STORE unset\n")
    sys.exit(2)
STATEF = os.path.join(STORE, "state.json")
EVENTS = os.path.join(STORE, "events.log")
CTL = os.path.join(STORE, "ctl")
TS = "2026-11-12T00:00:00Z"

OWNER_ANN = "quartet.ngxstorage.com/owner-uid"
USG_LABEL = "quartet-scenario.test/case"
FINALIZER = "quartet.ngxstorage.com/quartet"
CSIDRIVER_NAME = "nfs.csi.ngxstorage.com"
OPERAND_NS = "ngxstorage-nfs"
INSTALLED_CR = "nfs-csi-ngxstorage-controller"

# verb token -> (Kind, apiVersion, namespaced)
# Quartet is CLUSTER-scoped — bound to config/crd/bases/
# quartet.ngxstorage.com_quartets.yaml (spec.scope: Cluster), which the real CRD scope
# section parses independently and asserts this table models.
KINDS = {
    "quartet": ("Quartet", "quartet.ngxstorage.com/v1alpha1", False),
    "quartets": ("Quartet", "quartet.ngxstorage.com/v1alpha1", False),
    "namespace": ("Namespace", "v1", False), "namespaces": ("Namespace", "v1", False),
    "secret": ("Secret", "v1", True), "secrets": ("Secret", "v1", True),
    "configmap": ("ConfigMap", "v1", True), "configmaps": ("ConfigMap", "v1", True),
    "serviceaccount": ("ServiceAccount", "v1", True), "serviceaccounts": ("ServiceAccount", "v1", True),
    "service": ("Service", "v1", True), "services": ("Service", "v1", True),
    "pod": ("Pod", "v1", True), "pods": ("Pod", "v1", True),
    "persistentvolumeclaim": ("PersistentVolumeClaim", "v1", True),
    "persistentvolumeclaims": ("PersistentVolumeClaim", "v1", True),
    "persistentvolume": ("PersistentVolume", "v1", False),
    "persistentvolumes": ("PersistentVolume", "v1", False),
    "lease": ("Lease", "coordination.k8s.io/v1", True), "leases": ("Lease", "coordination.k8s.io/v1", True),
    "deployment": ("Deployment", "apps/v1", True), "deployments": ("Deployment", "apps/v1", True),
    "daemonset": ("DaemonSet", "apps/v1", True), "daemonsets": ("DaemonSet", "apps/v1", True),
    "clusterrole": ("ClusterRole", "rbac.authorization.k8s.io/v1", False),
    "clusterroles": ("ClusterRole", "rbac.authorization.k8s.io/v1", False),
    "csidriver": ("CSIDriver", "storage.k8s.io/v1", False), "csidrivers": ("CSIDriver", "storage.k8s.io/v1", False),
    "volumeattachment": ("VolumeAttachment", "storage.k8s.io/v1", False),
    "volumeattachments": ("VolumeAttachment", "storage.k8s.io/v1", False),
    "crd": ("CustomResourceDefinition", "apiextensions.k8s.io/v1", False),
    "customresourcedefinition": ("CustomResourceDefinition", "apiextensions.k8s.io/v1", False),
    "node": ("Node", "v1", False), "nodes": ("Node", "v1", False),
}
KIND_BY_NAME = {v[0]: v[1] for v in KINDS.values()}

def load_state():
    try:
        with open(STATEF) as fh:
            return json.load(fh)
    except Exception:
 # note: no "pending_seeds" key — its absence triggers the seed scan
        return {"objects": {}, "next_uid": 1, "seq": 0, "qg": 0,
                "installed_cr_digest": None}

def save_state(st):
    tmp = STATEF + ".tmp"
    with open(tmp, "w") as fh:
        json.dump(st, fh, indent=1, sort_keys=True)
    os.replace(tmp, STATEF)

def key_of(kind, ns, name):
    return "%s|%s|%s" % (kind, ns or "-", name)

def new_uid(st, hint):
    uid = str(uuid.uuid5(uuid.NAMESPACE_URL, "quartet-itest://%s:%d" % (hint, st["next_uid"])))
    st["next_uid"] += 1
    return uid

def ctl_read(name):
    try:
        with open(os.path.join(CTL, name)) as fh:
            return fh.read().strip()
    except Exception:
        return ""

def canon(obj):
    return hashlib.sha256(json.dumps(obj, sort_keys=True).encode()).hexdigest()

def log_event(st, verb, fields, rc, note):
    line = "inv %d verb=%s ctx=%s %s rc=%d note=%s" % (
        st["seq"], verb, st.get("ctx", "-") or "-", fields, rc, note or "-")
    with open(EVENTS, "a") as fh:
        fh.write(line + "\n")

def ns_exists(st, ns):
    return any(o.get("kind") == "Namespace" and o["metadata"]["name"] == ns
               for o in st["objects"].values())

def server_fail(st, verb, fields, msg, rc=1):
    sys.stderr.write(msg + "\n")
    save_state(st)
    log_event(st, verb, fields, rc, "rejected")
    save_state(st)
    sys.exit(rc)

def find_case_ancestor():
    """First ancestor whose cmdline runs a case body (crash-injection victim)."""
    pid = os.getppid()
    seen = set()
    while pid > 1 and pid not in seen:
        seen.add(pid)
        try:
            with open("/proc/%d/cmdline" % pid, "rb") as fh:
                cmd = fh.read().decode(errors="replace")
            ppid = None
            with open("/proc/%d/status" % pid) as fh:
                for line in fh:
                    if line.startswith("PPid:"):
                        ppid = int(line.split()[1])
        except Exception:
            return None
        if "case.sh" in cmd and "test-case-integration" not in cmd:
            return pid
        pid = ppid or 1
    return None

def validate_kind(kind, obj):
    md = obj.get("metadata") or {}
    if not md.get("name"):
        return "metadata.name is required"
    spec = obj.get("spec")
    if kind == "VolumeAttachment" and not (spec or {}).get("attacher"):
        return "spec.attacher is required in VolumeAttachment"
    if kind in ("Deployment", "DaemonSet"):
        s = spec or {}
        if not s.get("selector") or not s.get("template"):
            return "spec.selector and spec.template are required in %s" % kind
    if kind == "Service" and spec is None:
        return "spec is required in Service"
    if kind == "ClusterRole" and "rules" not in obj:
        return "rules is required in ClusterRole"
    if kind == "PersistentVolume" and not (spec or {}).get("capacity"):
        return "spec.capacity is required in PersistentVolume"
    if kind == "PersistentVolumeClaim":
        s = spec or {}
        if not ((s.get("resources") or {}).get("requests") or {}).get("storage"):
            return "spec.resources.requests.storage is required in PersistentVolumeClaim"
    if kind == "Quartet":
        s = spec or {}
        if not s.get("platform"):
            return "spec.platform is required in Quartet"
        if not isinstance(s.get("profiles"), dict):
            return "spec.profiles is required in Quartet"
    if kind == "Pod" and not ((spec or {}).get("containers") or []):
        return "spec.containers is required in Pod"
    return None

def server_strip(obj, kind):
    """Client-set identity/status is server-owned (real API semantics)."""
    md = obj.setdefault("metadata", {})
    for f in ("uid", "creationTimestamp", "resourceVersion", "generation",
              "selfLink", "deletionTimestamp"):
        md.pop(f, None)
    obj.pop("status", None)
    if kind == "Pod":
        obj["status"] = {"phase": "Running"}
    obj.setdefault("apiVersion", KIND_BY_NAME.get(kind, "v1"))
    return obj

def usage_present(st):
    for kind in ("PersistentVolumeClaim", "PersistentVolume", "VolumeAttachment"):
        for o in st["objects"].values():
            if o.get("kind") == kind and USG_LABEL in ((o.get("metadata") or {}).get("labels") or {}):
                return True
    return False

def set_cond(conds, ctype, status, reason, message):
    for c in conds:
        if c["type"] == ctype:
            c.update(status=status, reason=reason, message=message, lastTransitionTime=TS)
            return
    conds.append({"type": ctype, "status": status, "reason": reason,
                  "message": message, "lastTransitionTime": TS})

def ensure_object(st, kind, ns, name, doc):
    """Operator-side operand creation (API-valid objects); ValueError if strict
    validation fails. Returns the stored object (existing or new)."""
    k = key_of(kind, ns, name)
    if k in st["objects"]:
        return st["objects"][k]
    why = validate_kind(kind, doc)
    if why:
        raise ValueError(why)
    doc = server_strip(doc, kind)
    doc["kind"] = kind
    doc["metadata"]["uid"] = new_uid(st, "%s/%s/%s" % (kind, ns, name))
    doc["metadata"]["creationTimestamp"] = TS
    if kind == "Quartet":
        doc["metadata"]["generation"] = 1
    st["objects"][k] = doc
    return doc

def ensure_ns(st, ns, uid):
    ensure_object(st, "Namespace", "", ns,
                  {"metadata": {"name": ns, "annotations": {OWNER_ANN: uid}}})

def ensure_operands(st, uid, partial):
    for name, doc in (
        ("nfs-csi-ngxstorage-controller",
         {"kind": "ServiceAccount",
          "metadata": {"name": "nfs-csi-ngxstorage-controller", "namespace": OPERAND_NS,
                       "annotations": {OWNER_ANN: uid}}}),
        ("nfs-csi-ngxstorage-coordination-config",
         {"kind": "ConfigMap",
          "metadata": {"name": "nfs-csi-ngxstorage-coordination-config", "namespace": OPERAND_NS,
                       "annotations": {OWNER_ANN: uid}},
          "data": {"coordination": uid}}),
    ):
        ensure_object(st, doc["kind"], OPERAND_NS, name, doc)
    if partial:
        return
    tmpl = {"metadata": {"labels": {"app": "nfs-csi-controller"}},
            "spec": {"containers": [{"name": "main",
                                     "image": "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + "a" * 64}]}}
    for name, kind, doc in (
        (CSIDRIVER_NAME, "CSIDriver",
         {"kind": "CSIDriver",
          "metadata": {"name": CSIDRIVER_NAME, "annotations": {OWNER_ANN: uid}},
          "spec": {"podInfoOnMount": False}}),
        ("nfs-csi-ngxstorage-controller-health", "Service",
         {"kind": "Service",
          "metadata": {"name": "nfs-csi-ngxstorage-controller-health", "namespace": OPERAND_NS,
                       "annotations": {OWNER_ANN: uid}},
          "spec": {"type": "ClusterIP",
                   "ports": [{"name": "http", "port": 8080, "targetPort": 8080}],
                   "selector": {"app": "nfs-csi"}}}),
        ("nfs-csi-ngxstorage-controller", "Deployment",
         {"kind": "Deployment",
          "metadata": {"name": "nfs-csi-ngxstorage-controller", "namespace": OPERAND_NS,
                       "annotations": {OWNER_ANN: uid}},
          "spec": {"replicas": 1,
                   "selector": {"matchLabels": {"app": "nfs-csi-controller"}},
                   "template": tmpl}}),
        ("nfs-csi-ngxstorage-node", "DaemonSet",
         {"kind": "DaemonSet",
          "metadata": {"name": "nfs-csi-ngxstorage-node", "namespace": OPERAND_NS,
                       "annotations": {OWNER_ANN: uid}},
          "spec": {"selector": {"matchLabels": {"app": "nfs-csi-node"}},
                   "template": {"metadata": {"labels": {"app": "nfs-csi-node"}},
                                "spec": {"containers": [{"name": "node",
                                                         "image": "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:" + "a" * 64}]}}}}),
    ):
        ensure_object(st, kind, "" if kind == "CSIDriver" else OPERAND_NS, name, doc)

def delete_owned(st, uid):
    for k in [k for k, o in st["objects"].items()
              if o.get("kind") not in ("Quartet", "Namespace")
              and ((o.get("metadata") or {}).get("annotations") or {}).get(OWNER_ANN) == uid]:
        st["objects"].pop(k, None)

def reconcile_quartet(st, k, q):
    md = q["metadata"]
    name = md["name"]
    uid = md.get("uid", "")
    spec = q.get("spec") or {}
    nfs = (spec.get("profiles") or {}).get("nfs") or {}
    enabled = bool(nfs.get("enabled"))
    gen = md.get("generation", 1)
 # The replacement marker simulates an object the operator cannot vouch for
 # (swapped under the case): the emulation refuses to advance its status.
    if "scenario-integration/replaced" in ((md.get("annotations") or {})):
        return
    usage = usage_present(st)

    if "deletionTimestamp" in md:
        qst = q.setdefault("status", {})
        qst["observedGeneration"] = gen
        if usage:
            msg = ("active test usage detected (labeled PVC/PV/VolumeAttachment); "
                   "finalizer %s retained; requeueing" % FINALIZER)
            agg = qst.setdefault("conditions", [])
            set_cond(agg, "Ready", "False", "DataInUse", msg)
            set_cond(agg, "Degraded", "True", "DataInUse", msg)
        else:
            delete_owned(st, uid)
            md["finalizers"] = [f for f in md.get("finalizers", []) if f != FINALIZER]
            st["objects"].pop(k, None)
        return

    if name != "quartet":
        q["status"] = {"conditions": [
            {"type": "Ready", "status": "False", "reason": "InvalidSpec",
             "message": 'singleton violation: only a Quartet named "quartet" is reconciled, got "%s"' % name,
             "lastTransitionTime": TS}]}
        md.pop("finalizers", None)
        return

    md.setdefault("finalizers", [])
    if FINALIZER not in md["finalizers"]:
        md["finalizers"].append(FINALIZER)

    qst = q.setdefault("status", {})
    qst["specPlatform"] = spec.get("platform")
    qst["observedGeneration"] = gen
    agg = qst.setdefault("conditions", [])
    prof = qst.setdefault("profiles", {}).setdefault("nfs", {}).setdefault("conditions", [])
    inject = ctl_read("inject")

 # RBAC verification gate (read-only): drifted/missing -> RBACDrift, frozen.
    cr = st["objects"].get(key_of("ClusterRole", "", INSTALLED_CR))
    digest = canon(cr) if cr else None
    if digest is None or digest != st["installed_cr_digest"]:
        msg = ("operand RBAC drifted or missing: re-apply the install manifest config/operands/nfs/ — "
               "RBAC is NOT created or modified by the operator (read-only verification gate)")
        set_cond(agg, "Ready", "False", "RBACDrift", msg)
        set_cond(agg, "Degraded", "True", "RBACDrift", msg)
        set_cond(prof, "Ready", "False", "RBACDrift", msg)
        return

 # CSIDriver ownership gate: foreign object -> ForeignResourceConflict,
 # terminal; partial apply (SA + ConfigMap only), foreign object untouched.
    cdriver = st["objects"].get(key_of("CSIDriver", "", CSIDRIVER_NAME))
    if cdriver is not None and \
            ((cdriver["metadata"].get("annotations") or {}).get(OWNER_ANN) != uid):
        ensure_ns(st, OPERAND_NS, uid)
        ensure_operands(st, uid, partial=True)
        msg = ("foreign %s at canonical operand name without this CR's ownership annotation; "
               "adoption is not attempted; remove it or point the profile elsewhere" % CSIDRIVER_NAME)
        set_cond(agg, "Ready", "False", "ForeignResourceConflict", msg)
        set_cond(agg, "Degraded", "True", "ForeignResourceConflict", msg)
        set_cond(prof, "Ready", "False", "ForeignResourceConflict", msg)
        return

    ensure_ns(st, OPERAND_NS, uid)
    ensure_operands(st, uid, partial=False)

    if not enabled:
        if usage:
            msg = ("profile disabled but active test usage present; cleanup cancelled — "
                   "usage must clear first (retryable requeue)")
            set_cond(agg, "Ready", "False", "DataInUse", msg)
            set_cond(agg, "Degraded", "True", "DataInUse", msg)
            set_cond(prof, "Ready", "False", "DataInUse", msg)
            set_cond(prof, "Degraded", "True", "DataInUse", msg)
        else:
            delete_owned(st, uid)
            set_cond(agg, "Ready", "True", "Ready", "profile disabled; operands removed after a clean scan")
            set_cond(agg, "Degraded", "False", "Ready", "profile disabled")
            set_cond(prof, "Ready", "True", "Ready", "profile disabled")
        return

    secret = (nfs.get("secretRef") or {}).get("name")
    sec = st["objects"].get(key_of("Secret", OPERAND_NS, secret)) if secret else None
    if sec is None:
        reason = "WorkloadNotReady"
        msg = ("workload not ready: kubelet waiting reason CreateContainerConfigError "
               "(backend Secret %s absent); the operator does not read Secrets (Secret API untouched)" % secret)
        ready_status, degraded_status = "False", "True"
        if inject == "wrong-reason":
            reason, msg = "MissingSecret", "backend secret missing"
        if inject == "wrong-status":
            ready_status, degraded_status = "True", "False"
    else:
        reason = "Ready"
        msg = "profile nfs is Ready (not a backend I/O proof)"
        ready_status, degraded_status = "True", "False"
        if inject == "wrong-reason":
            reason = "SecretsLoaded"
        if inject == "wrong-status":
            ready_status, degraded_status = "False", "True"
    set_cond(agg, "Ready", ready_status, reason, msg)
    set_cond(agg, "Degraded", degraded_status, reason, msg)
    set_cond(prof, "Ready", ready_status, reason, msg)
    set_cond(prof, "Degraded", degraded_status, reason, msg)

def volume_pass(st):
    """k8s control-plane volume behavior (independent of the quartet operator):
    a PV whose csi driver identity matches a PVC's volumeName becomes Bound
    (claimRef + phase, set by the binding controller — never by the create
    JSON), and a VA whose source is a bound PV becomes attached=true (the
    attacher). Bodies OBSERVE this state; they never pre-set it."""
    pvs = [o for o in st["objects"].values() if o.get("kind") == "PersistentVolume"]
    for pv in pvs:
        pvname = pv["metadata"]["name"]
        claim = (pv.get("spec") or {}).get("claimRef") or {}
        for pvc in [o for o in st["objects"].values()
                    if o.get("kind") == "PersistentVolumeClaim"]:
            pvcname = pvc["metadata"]["name"]
 # volumeName is the binding signal; an already-consistent claimRef
 # never blocks, an inconsistent one is re-pointed (one PVC names
 # each PV; the claimRef's namespace must match the PVC's)
            if ((pvc.get("spec") or {}).get("volumeName") == pvname
                    and (not claim
                         or (claim.get("namespace") == pvc["metadata"].get("namespace", "")
                             and claim.get("name") == pvcname))):
                pv["spec"]["claimRef"] = {"kind": "PersistentVolumeClaim",
                                          "namespace": pvc["metadata"].get("namespace", ""),
                                          "name": pvcname}
                pv.setdefault("status", {})["phase"] = "Bound"
                pvc.setdefault("status", {})["phase"] = "Bound"
    for va in [o for o in st["objects"].values() if o.get("kind") == "VolumeAttachment"]:
        source_pv = ((va.get("spec") or {}).get("source") or {}).get("persistentVolumeName")
        pv = next((o for o in pvs if o["metadata"]["name"] == source_pv), None)
        if pv is not None and pv.get("status", {}).get("phase") == "Bound" \
                and ((va.get("spec") or {}).get("nodeName") or ""):
            va.setdefault("status", {})["attached"] = True

def reconcile_all(st):
    volume_pass(st)
    for k in [k for k in list(st["objects"]) if k.split("|")[0] == "Quartet"]:
        reconcile_quartet(st, k, st["objects"][k])

def process_seeds(st):
    """Out-of-band provisioning (installer-style): lazy, namespace-aware —
    a namespaced seed applies only once its namespace exists (real API
    semantics). Honors a fixed metadata.uid for identity assertions."""
    if "pending_seeds" not in st:
        try:
            st["pending_seeds"] = sorted(
                f for f in os.listdir(os.path.join(CTL, "seed")) if f.endswith(".json"))
        except Exception:
            st["pending_seeds"] = []
    kinds_by_kindname = {v[0]: v for v in KINDS.values()}
    still = []
    for fname in st["pending_seeds"]:
        try:
            with open(os.path.join(CTL, "seed", fname)) as fh:
                doc = json.load(fh)
        except Exception as exc:
            sys.stderr.write("fixture: seed %s unparseable (%s) — test setup bug\n" % (fname, exc))
            sys.exit(2)
        kind = doc.get("kind", "")
        md = doc.get("metadata") or {}
        ns, name = md.get("namespace", ""), md.get("name", "")
        fixed_uid = md.get("uid")
        if kind not in kinds_by_kindname:
            sys.stderr.write("fixture: seed %s has unknown kind %r — test setup bug\n" % (fname, kind))
            sys.exit(2)
        if kinds_by_kindname[kind][2] and ns and not ns_exists(st, ns):
            still.append(fname)
            continue
        try:
            o = ensure_object(st, kind, ns, name, doc)
        except ValueError as exc:
            sys.stderr.write("fixture: seed %s failed strict validation: %s — test setup bug\n" % (fname, exc))
            sys.exit(2)
        if fixed_uid:
            o["metadata"]["uid"] = fixed_uid
        if kind == "ClusterRole" and name == INSTALLED_CR and st["installed_cr_digest"] is None:
            st["installed_cr_digest"] = canon(o)
        log_event(st, "seed", "ns=%s kind=%s name=%s uid=%s" % (ns or "-", kind, name, o["metadata"]["uid"]),
                  0, "out-of-band")
    st["pending_seeds"] = still

VALUED_FLAGS = {"-n", "--namespace", "-o", "--output", "-f", "--filename", "-p", "--patch",
                "-l", "--selector", "--type", "--field-selector", "--raw", "--grace-period",
                "--cascade", "--timeout"}

# plural API token -> Kind (raw delete path resolution; strict table — an
# untabled plural is a 404 like the real API's "not routable").
RAW_PLURAL_TO_KIND = {
    "namespaces": "Namespace", "nodes": "Node",
    "persistentvolumes": "PersistentVolume",
    "persistentvolumeclaims": "PersistentVolumeClaim",
    "configmaps": "ConfigMap", "secrets": "Secret",
    "serviceaccounts": "ServiceAccount", "services": "Service",
    "pods": "Pod", "leases": "Lease", "deployments": "Deployment",
    "daemonsets": "DaemonSet", "clusterroles": "ClusterRole",
    "clusterrolebindings": "ClusterRoleBinding", "csidrivers": "CSIDriver",
    "volumeattachments": "VolumeAttachment",
    "customresourcedefinitions": "CustomResourceDefinition",
    "priorityclasses": "PriorityClass", "quartets": "Quartet",
}


def raw_target(raw):
    """Resolve a DeleteOptions raw API path to (Kind, ns-or-None, name).
    /api/v1/<plural>/<name>, /api/v1/namespaces/<ns>/<plural>/<name>,
    /apis/<group>/<version>/[namespaces/<ns>/]<plural>/<name>; None = unroutable."""
    if raw.startswith("/api/v1/"):
        rest = raw[len("/api/v1/"):]
    elif raw.startswith("/apis/"):
        parts = raw[len("/apis/"):].split("/", 2)
        if len(parts) < 3:
            return None
        rest = parts[2]
    else:
        return None
    seg = rest.split("/")
    if len(seg) == 2:
        ns, plural, name = None, seg[0], seg[1]
    elif len(seg) == 4 and seg[0] == "namespaces":
        ns, plural, name = seg[1], seg[2], seg[3]
    else:
        return None
    kind = RAW_PLURAL_TO_KIND.get(plural)
    if kind is None:
        return None
 # strict scope routing: a namespaced segment on a cluster-scoped kind (or
 # a missing one on a namespaced kind) is an UNKNOWN route — None ->
 # "the server could not find the requested resource", exactly like the
 # real API. (A namespaced raw Quartet path must be refused.)
    namespaced = next(v[2] for v in KINDS.values() if v[0] == kind)
    if namespaced != (ns is not None):
        return None
    return kind, ns, name
BOOL_FLAGS = {"--ignore-not-found", "--all", "--force", "--now", "--wait", "--dry-run", "-q", "--quiet"}

def main():
    args = sys.argv[1:]
    if args and args[0] == "__inspect":
        st = load_state()
        if len(args) > 1 and args[1] == "list":
            print(json.dumps(st["objects"], indent=1, sort_keys=True))
        sys.exit(0)

    context = None
    if len(args) >= 2 and args[0] == "--context":
        context, args = args[1], args[2:]

    st = load_state()
    st["ctx"] = context or "-"
    process_seeds(st)
    st["seq"] += 1
    save_state(st)

    verb, rest = "", args
    for i, a in enumerate(args):
        if not a.startswith("-"):
            verb = a
            rest = args[i + 1:]
            break
    if verb not in ("create", "get", "apply", "replace", "patch", "delete", "exec"):
        save_state(st)
        sys.stderr.write('error: unknown command "%s" for "kubectl"\n' % (verb or "<none>"))
        sys.exit(1)

    positional, pod_args = [], None
    ns = None
    i = 0
    while i < len(rest):
        a = rest[i]
        if a == "--":
            pod_args = rest[i + 1:]
            break
 # real kubectl accepts both `--flag value` and `--flag=value`
        inline_val = None
        if a.startswith("-") and "=" in a:
            a, inline_val = a.split("=", 1)
        if a in VALUED_FLAGS:
            if inline_val is None:
                if i + 1 >= len(rest):
                    sys.stderr.write("fixture: flag %s requires a value\n" % a)
                    sys.exit(1)
                val, i = rest[i + 1], i + 2
            else:
                val, i = inline_val, i + 1
            if a in ("-n", "--namespace"):
                ns = val
            elif a == "-f":
                positional.append(("file", val))
            elif a == "-p":
                positional.append(("patch", val))
            elif a == "--type":
                positional.append(("type", val))
            elif a == "--field-selector":
                positional.append(("field", val))
            elif a == "--raw":
                positional.append(("raw", val))
            elif a in ("-l", "--selector"):
                positional.append(("label", val))
            elif a in ("-o", "--output"):
                positional.append(("out", val))
            continue
        if a in BOOL_FLAGS:
            positional.append(("bool", a))
            i += 1
            continue
        if a.startswith("-"):
            sys.stderr.write("error: unknown flag: %s\n" % a)
            sys.exit(1)
        positional.append(("arg", a))
        i += 1

    kind_tokens = [p[1] for p in positional if p[0] == "arg"]
    files = [p[1] for p in positional if p[0] == "file"]
    patches = [p[1] for p in positional if p[0] == "patch"]
    field_sel = [p[1] for p in positional if p[0] == "field"]
    ignore_nf = any(p[1] == "--ignore-not-found" for p in positional if p[0] == "bool")
    out_json = any(p[1] == "json" for p in positional if p[0] == "out")

    def resolve_kind():
        if not kind_tokens or kind_tokens[0] not in KINDS:
            server_fail(st, verb, "ns=%s" % (ns or "-"),
                        "error: the server doesn't have a resource type %r" % (kind_tokens[0] if kind_tokens else "<none>"))
        return KINDS[kind_tokens[0]]

    def require_ns():
        """Namespaced objects cannot exist in an absent namespace (real API)."""
        if ns and not ns_exists(st, ns):
            server_fail(st, verb, "ns=%s kind=%s name=%s" % (ns, kind_tokens[0] if kind_tokens else "-",
                        kind_tokens[1] if len(kind_tokens) > 1 else "-"),
                        "Error from server (NotFound): namespaces %r not found" % ns)

    def not_found_msg(kind, name):
        return "Error from server (NotFound): %s %r not found" % (kind, name)

    def out(obj):
        print(json.dumps(obj, sort_keys=True))

    def finish(fields, rc, note=""):
        save_state(st)
        log_event(st, verb, fields, rc, note)
        save_state(st)
        sys.exit(rc)

    def resolve_target(kind, name, eff_ns):
        """get/patch/delete by name: an explicit -n binds exactly (real API);
        without -n a UNIQUE object of that kind+name is bound (calling-context
        namespace defaulting), otherwise NotFound."""
        if ns:
            eff_ns[0] = ns
            return st["objects"].get(key_of(kind, ns, name))
        matches = [o for o in st["objects"].values()
                   if o.get("kind") == kind and (o.get("metadata") or {}).get("name") == name]
        if len(matches) == 1:
            eff_ns[0] = (matches[0].get("metadata") or {}).get("namespace", "") or "-"
            return matches[0]
        eff_ns[0] = ns or "-"
        return None

 # REAL CLIENT model (CASE-CONTRACT "Gerçek delete gövdesi"): name +
 # field-selector delete is refused by kubectl ITSELF before any API call
 # — a field selector is not a UID precondition (it only pre-filters a
 # list). No store change, no mutation count; the rejection is logged so
 # "nothing was accepted" is observable.
    if verb == "delete" and field_sel:
        sys.stderr.write("error: name cannot be provided when a selector is specified\n")
        fields = "ns=%s kind=%s name=%s" % (ns or "-",
                                            kind_tokens[0] if kind_tokens else "-",
                                            kind_tokens[1] if len(kind_tokens) > 1 else "-")
        save_state(st)
        log_event(st, "delete", fields + " target_uid=- precondition=none outcome=client-rejected",
                  1, "client-rejected")
        save_state(st)
        sys.exit(1)

 # crash injection: SIGTERM the case shell on the Nth MUTATING server call —
 # by then the body is guaranteed mid-run (preflight done, evidence open).
    if verb in ("create", "patch", "replace", "delete"):
        st["mut"] = st.get("mut", 0) + 1
        killmut = int(ctl_read("kill-on-mutation") or 0)
        if killmut and st["mut"] == killmut:
            victim = find_case_ancestor() or os.getppid()
            try:
                os.kill(victim, signal.SIGTERM)
            except Exception:
                pass
            log_event(st, "kill", "mut=%d" % st["mut"], 143, "crash-injection")
            save_state(st)
            sys.exit(143)

 # WATCH TRIGGER: every API call advances the emulated control plane by one
 # pass — the volume controllers bind PV<->PVC by driver identity and attach
 # VAs (so bodies OBSERVE Bound/attached state in API responses), and the
 # quartet operator reconciles each Quartet CR. A swapped-in replacement
 # (injected below) is refused advancement.
    if verb == "get" and len(kind_tokens) >= 2:
        kinfo = KINDS.get(kind_tokens[0])
        if kinfo and kinfo[0] == "Quartet":
            eff_ns = [ns or "-"]
            tgt = resolve_target("Quartet", kind_tokens[1], eff_ns)
            k = key_of("Quartet", eff_ns[0], kind_tokens[1])
            repn = int(ctl_read("replace-on-quartet-get") or 0)
            if repn:
                st["qg"] = st.get("qg", 0) + 1
                if st["qg"] == repn and tgt is not None:
                    md = tgt["metadata"]
                    new_uid_v = new_uid(st, "replacement")
                    md["uid"] = new_uid_v
                    md["annotations"] = {"scenario-integration/replaced": "yes"}
                    md["generation"] = 1
                    tgt["status"] = {}
                    save_state(st)
                    log_event(st, "replace-inject",
                              "ns=%s kind=Quartet name=%s uid=%s" % (eff_ns[0], kind_tokens[1], new_uid_v),
                              0, "swapped-under-case")
                    save_state(st)
    reconcile_all(st)
    save_state(st)

    if verb == "exec":
        if len(kind_tokens) < 1 or pod_args is None:
            sys.stderr.write("error: exec requires a pod and `-- <command>`\n")
            sys.exit(1)
 # for exec the positional is the POD NAME, not a kind
        pod = st["objects"].get(key_of("Pod", ns, kind_tokens[0]))
        if pod is None or pod.get("metadata", {}).get("namespace", "") != (ns or ""):
            server_fail(st, "exec", "ns=%s kind=Pod name=%s" % (ns or "-", kind_tokens[0]),
                        not_found_msg("pod", kind_tokens[0]))
        r = subprocess.run(pod_args)
        finish("ns=%s kind=Pod name=%s podargs=%s" % (ns or "-", kind_tokens[0], json.dumps(pod_args)),
               r.returncode)

    if verb in ("create", "apply", "replace"):
        if not files:
            sys.stderr.write("error: no resource files specified\n")
            sys.exit(1)
        docs = []
        for fn in files:
            if fn == "-":
                text = sys.stdin.read()
            else:
                try:
                    with open(fn) as fh:
                        text = fh.read()
                except Exception:
                    sys.stderr.write("error: open %s: no such file or directory\n" % fn)
                    sys.exit(1)
            for chunk in text.split("\n---\n"):
                chunk = chunk.strip()
                if not chunk:
                    continue
                try:
                    docs.append(json.loads(chunk))
                except Exception:
                    sys.stderr.write('error: error parsing %s: cannot parse JSON\n' % fn)
                    sys.exit(1)
        if not docs:
            sys.stderr.write('error: no documents found in %s\n' % ", ".join(files))
            sys.exit(1)
 # kind comes from the DOCUMENT for -f verbs (they take no kind token):
 # resolve it before anything else is validated.
        kind = docs[0].get("kind")
        if kind not in KIND_BY_NAME:
            server_fail(st, verb, "ns=- kind=- name=-",
                        "error: the server doesn't have a resource type %r" % kind)
        namespaced = next(v[2] for v in KINDS.values() if v[0] == kind)
        for doc in docs:
            if doc.get("kind") != kind:
                server_fail(st, verb, "ns=%s kind=%s name=-" % (ns or "-", kind),
                            "error: the name of the object should match %s (got %s)" % (kind, doc.get("kind")))
            doc_ns = (doc.get("metadata") or {}).get("namespace", "")
            doc_name = (doc.get("metadata") or {}).get("name", "")
            fields = "ns=%s kind=%s name=%s" % (doc_ns or "-", kind, doc_name)
            if namespaced and doc_ns and not ns_exists(st, doc_ns):
                server_fail(st, verb, fields, "Error from server (NotFound): namespaces %r not found" % doc_ns)
            why = validate_kind(kind, doc)
            if why:
                server_fail(st, verb, fields,
                            'error: error validating "stdin": error validating data: [ValidationError(%s): %s]' % (kind, why))
            k = key_of(kind, doc_ns, doc_name)
            exists = k in st["objects"]
            if verb == "replace" and not exists:
                server_fail(st, verb, fields, not_found_msg(kind, doc_name))
            if verb == "create" and exists:
                server_fail(st, verb, fields,
                            "Error from server (AlreadyExists): %s %r already exists" % (kind, doc_name))
            if exists:  # apply/replace: update
                old = st["objects"][k]
                spec_before = canon(old.get("spec") or {})
                for f, v in doc.items():
                    if f in ("kind", "apiVersion"):
                        continue
                    if f == "metadata":
                        for mf, mv in v.items():
                            if mf in ("labels", "annotations"):
                                old.setdefault("metadata", {})[mf] = mv
                        continue
                    old[f] = v
                if kind == "Quartet" and canon(old.get("spec") or {}) != spec_before:
                    old["metadata"]["generation"] = old["metadata"].get("generation", 1) + 1
                note = "updated"
            else:
                st["objects"][k] = server_strip(doc, kind)
                st["objects"][k]["kind"] = kind
                md = st["objects"][k]["metadata"]
                md["uid"] = new_uid(st, "%s/%s/%s" % (kind, doc_ns, md["name"]))
                md["creationTimestamp"] = TS
                if kind == "Quartet":
                    md["generation"] = 1
                note = "created"
            fields += " uid=%s" % st["objects"][k]["metadata"]["uid"]
            if out_json:
 # create response is the ownership-binding surface
                out(st["objects"][k])
            else:
                print('%s "%s" %s' % (kind.lower(), st["objects"][k]["metadata"]["name"], note))
            save_state(st)
            log_event(st, verb, fields, 0, note)
            save_state(st)
        save_state(st)
        sys.exit(0)

    if verb == "get":
        kind, _, namespaced = resolve_kind()
        if len(kind_tokens) > 1:
            eff_ns = [ns or "-"]
            o = resolve_target(kind, kind_tokens[1], eff_ns)
            k = key_of(kind, eff_ns[0], kind_tokens[1])
            fields = "ns=%s kind=%s name=%s" % (eff_ns[0], kind, kind_tokens[1])
            if o is None:
                server_fail(st, "get", fields, not_found_msg(kind, kind_tokens[1]))
            fields += " uid=%s" % o["metadata"]["uid"]
            out(o)
        else:
            if namespaced:
                require_ns()
            items = [o for o in st["objects"].values() if o.get("kind") == kind
                     and (not namespaced or o.get("metadata", {}).get("namespace") == ns)]
            fields = "ns=%s kind=%s name=- items=%d" % (ns or "-", kind, len(items))
            out({"apiVersion": "v1", "kind": "List", "items": items})
        finish(fields, 0)

    if verb == "patch":
        if len(kind_tokens) < 2:
            sys.stderr.write("error: patch requires kind and name\n")
            sys.exit(1)
        kind, _, namespaced = resolve_kind()
        if namespaced and ns:
            require_ns()
        eff_ns = [ns or "-"]
        old = resolve_target(kind, kind_tokens[1], eff_ns)
        k = key_of(kind, eff_ns[0], kind_tokens[1])
        fields = "ns=%s kind=%s name=%s" % (eff_ns[0], kind, kind_tokens[1])
        if old is None:
            server_fail(st, "patch", fields, not_found_msg(kind, kind_tokens[1]))
        patch_doc = None
        for fn in patches or ["-"]:
            if fn == "-":
                patch_doc = json.loads(sys.stdin.read())
            elif fn.lstrip().startswith(("{", "[")):
 # real kubectl/oc accepts an inline JSON patch string
                patch_doc = json.loads(fn)
            else:
                with open(fn) as fh:
                    patch_doc = json.load(fh)
        spec_before = canon(old.get("spec") or {})
        def mp(base, p):
            for pk, pv in p.items():
                if pv is None:
                    base.pop(pk, None)
                elif isinstance(pv, dict) and isinstance(base.get(pk), dict):
                    mp(base[pk], pv)
                else:
                    base[pk] = pv
        mp(old, patch_doc)
        if kind == "Quartet" and canon(old.get("spec") or {}) != spec_before:
            old["metadata"]["generation"] = old["metadata"].get("generation", 1) + 1
        fields += " uid=%s" % old["metadata"]["uid"]
        print("patched")
        finish(fields, 0, "patched")

    if verb == "delete":
        raws = [p[1] for p in positional if p[0] == "raw"]
        if raws:
 # REAL DeleteOptions.body UID precondition — the supported-client
 # form `delete --raw=<api-path> -f -` (CASE-CONTRACT "Gerçek
 # delete gövdesi"). The body is parsed and preconditions.uid is
 # enforced SERVER-SIDE: absent → NotFound; GET-then A→B
 # replacement before the delete → 409 Conflict with B fully
 # preserved and NO deletion record; match → deleted/terminating.
            raw = raws[0]
            tgt = raw_target(raw)
            rfields = "path=%s" % raw
            if tgt is None:
                server_fail(st, "delete", rfields,
                            "Error from server (NotFound): the server could not find the requested resource")
            kind, rns, name = tgt
            rfields = "ns=%s kind=%s name=%s path=%s" % (rns or "-", kind, name, raw)
            try:
                if "-" in files:
                    body = json.loads(sys.stdin.read())
                elif files:
                    with open(files[0]) as fh:
                        body = json.load(fh)
                else:
                    raise ValueError("no -f body")
                if body.get("kind") != "DeleteOptions":
                    raise ValueError("kind")
            except Exception:
                server_fail(st, "delete", rfields,
                            "error: the body of a raw delete must be a decodable DeleteOptions object")
            prec_uid = (body.get("preconditions") or {}).get("uid") or ""
            k = key_of(kind, rns, name)
            o = st["objects"].get(k)
            if o is None:
                server_fail(st, "delete", rfields + " target_uid=- precondition=%s outcome=not-found"
                            % (prec_uid or "none"), not_found_msg(kind, name))
 # race arm: A→B replacement AFTER the caller's get, BEFORE the
 # precondition evaluates (ctl/replace-before-delete carries B).
            repl = ctl_read("replace-before-delete")
            if prec_uid and repl and o["metadata"].get("uid") != repl:
                o["metadata"]["uid"] = repl
                o.setdefault("metadata", {}).setdefault("annotations", {})[
                    "scenario-integration/replaced"] = "yes"
                save_state(st)
                log_event(st, "replace-inject", rfields + " uid=%s" % repl,
                          0, "swapped-under-delete")
                save_state(st)
            live_uid = o["metadata"].get("uid")
            if prec_uid and live_uid != prec_uid:
                sys.stderr.write('Error from server (Conflict): %s "%s" — UID precondition '
                                 'failed: the object has been modified\n' % (kind.lower(), name))
                save_state(st)
                log_event(st, "delete",
                          rfields + " target_uid=%s precondition=uid:%s outcome=conflict" % (live_uid, prec_uid),
                          1, "uid-conflict-409")
                save_state(st)
                sys.exit(1)
            prec = "uid:" + prec_uid if prec_uid else "none"
            if o["metadata"].get("finalizers"):
                o["metadata"]["deletionTimestamp"] = TS
                print('%s "%s" deleted' % (kind.lower(), name))
                reconcile_all(st)
                finish(rfields + " target_uid=%s precondition=%s outcome=terminating" % (live_uid, prec), 0)
            else:
                st["objects"].pop(k)
                print('%s "%s" deleted' % (kind.lower(), name))
                if kind == "Quartet":
                    reconcile_all(st)
                finish(rfields + " target_uid=%s precondition=%s outcome=deleted" % (live_uid, prec), 0)

        if len(kind_tokens) < 2:
            sys.stderr.write("error: delete requires kind and name\n")
            sys.exit(1)
        kind, _, namespaced = resolve_kind()
        fields = "ns=%s kind=%s name=%s" % (ns or "-", kind, kind_tokens[1])
        prec = "none"
        if namespaced and ns:
            require_ns()
        eff_ns = [ns or "-"]
        o = resolve_target(kind, kind_tokens[1], eff_ns)
        k = key_of(kind, eff_ns[0], kind_tokens[1])
        fields = "ns=%s kind=%s name=%s" % (eff_ns[0], kind, kind_tokens[1])
        if o is None:
            if ignore_nf:
                print("No resources found")
                finish(fields + " target_uid=- precondition=%s outcome=not-found" % prec, 0, "absent")
            server_fail(st, "delete", fields + " target_uid=- precondition=%s outcome=not-found" % prec,
                        not_found_msg(kind, kind_tokens[1]))
        elif o["metadata"].get("finalizers"):
            o["metadata"]["deletionTimestamp"] = TS
            print('%s "%s" deleted' % (kind.lower(), kind_tokens[1]))
 # terminating objects advance in THIS call so GC/block is observable
            reconcile_all(st)
            finish(fields + " target_uid=%s precondition=%s outcome=terminating" % (o["metadata"]["uid"], prec), 0)
        else:
            st["objects"].pop(k)
            print('%s "%s" deleted' % (kind.lower(), kind_tokens[1]))
            if kind == "Quartet":
                reconcile_all(st)
            finish(fields + " target_uid=%s precondition=%s outcome=deleted" % (o["metadata"]["uid"], prec), 0)

    sys.exit(1)

if __name__ == "__main__":
    main()
PYFIX
for t in kubectl oc; do
  printf '%s\n' '#!/bin/bash' "exec python3 \"$fx/api_fixture.py\" \"\$@\"" > "$fx/bin/$t"
  chmod +x "$fx/bin/$t"
done

# Installed-cluster precondition manifests (out-of-band provisioning; lazy
# seeds — the real API does not create into a namespace that is absent).
write_seed() { # $1=store  $2=filename  $3=json
  mkdir -p "$fx/store/$1/ctl/seed"
  printf '%s\n' "$3" > "$fx/store/$1/ctl/seed/$2"
}
CLUSTERROLE_JSON='{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"nfs-csi-ngxstorage-controller","labels":{"app":"nfs-csi-ngxstorage"}},"rules":[{"apiGroups":[""],"resources":["secrets","configmaps","serviceaccounts","leases","persistentvolumeclaims"],"verbs":["get","list","watch"]},{"apiGroups":["apps"],"resources":["deployments","daemonsets"],"verbs":["get","list","watch","create","patch","delete"]},{"apiGroups":["storage.k8s.io"],"resources":["csidrivers","volumeattachments"],"verbs":["get","list","watch","create","patch","delete"]}]}'
NS_OPERAND_JSON='{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ngxstorage-nfs"}}'
NS_TEST_JSON='{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns-a"}}'
NODE_JSON='{"apiVersion":"v1","kind":"Node","metadata":{"name":"test-node"},"spec":{"podCIDR":"10.244.0.0/24"},"status":{"phase":"Running"}}'
# Synthetic CRD carries the REAL scope (Cluster) — real CRD scope binds it independently
# against config/crd/bases/quartet.ngxstorage.com_quartets.yaml.
CRD_JSON='{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"quartets.quartet.ngxstorage.com"},"spec":{"group":"quartet.ngxstorage.com","names":{"kind":"Quartet","plural":"quartets"},"scope":"Cluster"}}'
SECRET_JSON='{"apiVersion":"v1","kind":"Secret","metadata":{"name":"nfs-csi-ngxstorage-config","namespace":"ngxstorage-nfs"},"data":{"storage-ips":"MTkyLjE2OC4xLjE=","pool-name":"cG9vbA==","api-key":"a2V5","data-ip":"MTkyLjE2OC4xLjI="}}'
# Cluster-scoped object: NO metadata.namespace (the real CRD scope is Cluster).
SINGLETON_JSON='{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","logLevel":"info","profiles":{"nfs":{"enabled":true,"secretRef":{"name":"nfs-csi-ngxstorage-config"},"image":{"reference":"quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:809528ed53c084730c1f645c6b8f4d8b04dfaac2a4ca3fbee81059606e39243b"}},"fc":{"enabled":false},"iscsi":{"enabled":false}}}}'

# seed_for_case <store> <case-name>: per-case precondition provisioning —
# what an installed target carries BEFORE the case runs (the case.json
# preconditions). An installed cluster always has the CRD + a node; the W3
# preconditions add the installer-provisioned RBAC set. The operand namespace
# is NOT pre-seeded for cases whose assertion requires the operator to create
# it (dependency-recovery namespace ownership, singleton-rejection phase 1); TEST_NAMESPACE is
# not pre-seeded for W3 (the case creates or adopts it). The W4 preconditions
# a LIVE, converged singleton — so the case itself never creates it:
# singleton + operand namespace + backend secret pre-exist there.
seed_for_case() {
  write_seed "$1" "01-node.json" "$NODE_JSON"
  write_seed "$1" "02-crd.json" "$CRD_JSON"
  case "$2" in
    active-volume-disable-protection|active-volume-delete-protection)
      write_seed "$1" "03-ns-test.json" "$NS_TEST_JSON"
      write_seed "$1" "04-ns-operand.json" "$NS_OPERAND_JSON"
      write_seed "$1" "05-secret.json" "$SECRET_JSON"
      write_seed "$1" "06-singleton.json" "$SINGLETON_JSON" ;;
  esac
  write_seed "$1" "07-clusterrole.json" "$CLUSTERROLE_JSON"
}

run_case_body() { # $1=case $2=store-label ; extra env via ITEST_OV (already set)
  local c="$1" sl="$2" parsed risk scope grant rc
  parsed="$(_scenario_parse_case_json "$scen/cases/$c/case.json")" || { IT_CASE_RC=9; return; }
  risk="$(sed -n '4p' <<<"$parsed")"; scope="$(sed -n '5p' <<<"$parsed")"
 # Effect-based grant (protocol pin 1); fall back to risk if the lib has not
 # landed the tier helper yet (mid-wave state of the gate worker).
  if declare -F _scenario_effect_tier >/dev/null; then
    grant="allow-$(_scenario_effect_tier "$risk" "$scope")"
  else
    grant="allow-$risk"
  fi
  mkdir -p "$fx/out" "$fx/ev" "$fx/store/$sl" "$fx/store/$sl/ctl"
  rc=0
  env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a ALLOW_LIVE="$grant" \
      RUN_ID="itest-$sl" EVIDENCE_DIR="$fx/ev/$sl" SCENARIO_FAKE_STORE="$fx/store/$sl" \
      TMPDIR="$fx/tmp/$sl" SCENARIO_WAIT_SECONDS=2 \
      SCENARIO_MAX_POLLS=5 SCENARIO_POLL_INTERVAL=0.2 PATH="$fx/bin:$PATH" \
      "${ITEST_OV[@]}" \
      bash "$scen/cases/$c/case.sh" >"$fx/out/$sl.log" 2>&1 || rc=$?
  IT_CASE_RC=$rc
}

# ownership_audit <events.log> -> rc 0 iff every deletion (outcome=deleted or
# terminating) targeted a UID this RUN created (verb=create) or the fixture
# provisioned before the run (verb=seed). A deletion aimed at any other UID —
# e.g. an object swapped under the case — is an ownership violation.
ownership_audit() {
  local ev="$1" line tgt bad=0
  local allowed
  allowed="$( { grep -E '^inv [0-9]+ verb=create ' "$ev"; grep -E ' verb=seed ' "$ev"; } | grep -oE 'uid=[0-9a-f-]+' | sed 's/^uid=//' | sort -u )"
  while IFS= read -r line; do
    case "$line" in
      *" outcome=deleted "*|*" outcome=terminating "*)
        tgt="$(sed -n 's/.* target_uid=\([^ ]*\).*/\1/p' <<<"$line")"
        [[ -n "$tgt" ]] || continue
        grep -qxF "$tgt" <<<"$allowed" || bad=1 ;;
    esac
  done < <(grep -E '^inv [0-9]+ verb=delete ' "$ev")
  return $bad
}

# run_created_gone <state.json> <events.log> -> rc 0 iff no object in the final
# store carries a UID created by the run: cleanup runs on EVERY exit path
#, so leftovers are a failed cleanup obligation. (Owner-uid
# ANNOTATIONS on surviving operator-provisioned objects legitimately reference
# run UIDs — only metadata.uid membership counts. Fixture-provisioned state —
# singleton, RBAC, node, CRD — may remain.)
run_created_gone() {
  python3 - "$1" "$2" <<'PY'
import json, re, sys
st = json.load(open(sys.argv[1]))
ev = open(sys.argv[2]).read()
created = set(re.findall(r"^inv \d+ verb=create .* uid=([0-9a-f-]{36})", ev, re.M))
left = sorted(o["metadata"]["name"] for o in st["objects"].values()
              if o.get("metadata", {}).get("uid") in created)
print("leftover:" + ",".join(left))
sys.exit(1 if left else 0)
PY
}

ITEST_OV=()

# ============================================================================
# the fixture behaves like kubectl + a strict API server (self-checks)
# ============================================================================
mkdir -p "$fx/store/selfcheck/ctl/seed"
SELF_STORE="$fx/store/selfcheck"
printf '%s\n' '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns-a"}}' > "$SELF_STORE/ctl/seed/00-ns-a.json"
self() { env SCENARIO_FAKE_STORE="$SELF_STORE" PATH="$fx/bin:$PATH" kubectl "$@"; }
printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1","namespace":"ns-a"},"data":{"k":"v"}}' > "$fx/sc/cm1.json"
printf '%s\n' '{"apiVersion":"storage.k8s.io/v1","kind":"VolumeAttachment","metadata":{"name":"va-bad"},"spec":{"nodeName":"n1","source":{"persistentVolumeName":"pv1"}}}' > "$fx/sc/va-bad.json"
printf '%s\n' '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"dep-bad","namespace":"ns-a"},"spec":{"replicas":1}}' > "$fx/sc/dep-bad.json"
printf '%s\n' '{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true,"secretRef":{"name":"s1"}}}}}' > "$fx/sc/q.json"

rc=0; self create -f "$fx/sc/cm1.json" >/dev/null 2>&1 || rc=$?
assert "fixture self: strict create/get roundtrip returns the server-assigned UID" \
  '[ "$rc" = "0" ] && self get configmap cm1 -n ns-a -o json | grep -q "\"uid\""'
rc=0; self create -f "$fx/sc/cm1.json" >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: duplicate create is API-atomic AlreadyExists (rc=1)" \
  '[ "$rc" = "1" ] && grep -q "AlreadyExists" "$fx/sc/err.txt"'
rc=0; self get configmap absent -n ns-a -o json >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: get of absent object is NotFound (rc=1)" \
  '[ "$rc" = "1" ] && grep -q "NotFound" "$fx/sc/err.txt"'
rc=0; self create -f "$fx/sc/va-bad.json" >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: VolumeAttachment without spec.attacher is rejected (strict API)" \
  '[ "$rc" = "1" ] && grep -q "attacher" "$fx/sc/err.txt"'
rc=0; self create -f "$fx/sc/dep-bad.json" >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: Deployment without selector/template is rejected (strict API)" \
  '[ "$rc" = "1" ] && grep -q "selector" "$fx/sc/err.txt"'
rc=0; self reconcile --max-passes 4 >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: no synthetic reconcile RPC — unknown verb rejected like real kubectl" \
  '[ "$rc" = "1" ] && grep -q "unknown command" "$fx/sc/err.txt"'
# --- REAL client/API delete model (CASE-CONTRACT "Gerçek delete gövdesi") ---
# 1. name + field-selector is refused CLIENT-SIDE like real kubectl: rc 1,
# the exact kubectl error, nothing reaches the API, nothing is removed.
rc=0; self delete configmap cm1 -n ns-a --field-selector=metadata.uid=00000000-0000-0000-0000-000000000000 >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: name+field-selector delete refused client-side (rc=1, kubectl's exact error, object survives)" \
  '[ "$rc" = "1" ] && grep -q "name cannot be provided when a selector is specified" "$fx/sc/err.txt" && [ -n "$(self get configmap cm1 -n ns-a -o json 2>/dev/null)" ]'
CM1_UID="$(self get configmap cm1 -n ns-a -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["uid"])')"
# 2. the UID precondition travels in the DeleteOptions.body via the
# supported-client form `delete --raw=<api-path> -f -`.
# 2a. mismatched preconditions.uid → 409 Conflict: rc 1, B's fields fully
# preserved, no deletion record.
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"cm1"},"preconditions":{"uid":"00000000-0000-0000-0000-000000000000"}}' \
  | self delete --raw=/api/v1/namespaces/ns-a/configmaps/cm1 -f - >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: raw DeleteOptions.body with mismatched preconditions.uid → 409 Conflict (rc=1, object preserved byte-for-byte)" \
  '[ "$rc" = "1" ] && grep -q "Conflict" "$fx/sc/err.txt" && [ "$(self get configmap cm1 -n ns-a -o json 2>/dev/null | grep -c "\"uid\": \"$CM1_UID\"")" = "1" ]'
# 2b. GET-then A→B replacement before the delete (race arm) → 409, B survives
# with its fields intact; NO deletion outcome is recorded.
echo "replaced-uid-B" > "$fx/store/selfcheck/ctl/replace-before-delete"
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"cm1"},"preconditions":{"uid":"'"$CM1_UID"'"}}' \
  | self delete --raw=/api/v1/namespaces/ns-a/configmaps/cm1 -f - >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: A→B replacement between get and delete → 409, B fully preserved, no deleted record" \
  '[ "$rc" = "1" ] && grep -q "Conflict" "$fx/sc/err.txt" && grep -q "\"uid\": \"replaced-uid-B\"" "$fx/store/selfcheck/state.json" && grep -q "\"k\": \"v\"" "$fx/store/selfcheck/state.json" && ! grep "outcome=deleted" "$fx/store/selfcheck/events.log" | grep -q "replaced-uid-B\|target_uid=$CM1_UID"'
# 2c. matching preconditions.uid → deleted; postcondition get is NotFound.
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"cm1"},"preconditions":{"uid":"replaced-uid-B"}}' \
  | self delete --raw=/api/v1/namespaces/ns-a/configmaps/cm1 -f - >/dev/null 2>&1 || rc=$?
assert "fixture self: raw DeleteOptions.body with the LIVE uid deletes (rc=0, verified gone)" \
  '[ "$rc" = "0" ] && ! self get configmap cm1 -n ns-a -o json >/dev/null 2>&1'
mv "$fx/store/selfcheck/ctl/replace-before-delete" "$fx/store/selfcheck/ctl/replace-before-delete.consumed"
# 2d. raw delete of an absent object → clean NotFound rc 1.
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"cm1"},"preconditions":{"uid":"replaced-uid-B"}}' \
  | self delete --raw=/api/v1/namespaces/ns-a/configmaps/cm1 -f - >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: raw DeleteOptions delete of an absent object is a clean NotFound (rc=1)" \
  '[ "$rc" = "1" ] && grep -q "Error from server (NotFound)" "$fx/sc/err.txt"'
rc=0; self create -f "$fx/sc/q.json" >/dev/null 2>&1 || rc=$?
assert "fixture self: watch-triggered operator emulation converges a valid cluster-scoped Quartet (no -n; finalizer + observedGeneration)" \
  '[ "$rc" = "0" ] && self get quartet quartet -o json | grep -q "observedGeneration"'
rc=0; self get quartet quartet -n ns-a -o json >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: a -n on the cluster-scoped Quartet does NOT reach it (namespaced binding is not a namespace for a cluster source)" \
  '[ "$rc" = "1" ] && grep -q "NotFound" "$fx/sc/err.txt"'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"quartet"},"preconditions":{"uid":"x"}}' \
  | self delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/namespaces/ns-a/quartets/quartet -f - \
  >/dev/null 2>"$fx/sc/err.txt" || rc=$?
assert "fixture self: the NAMESPACED raw Quartet route is refused by the fixture (unknown route, zero mutation)" \
  '[ "$rc" = "1" ] && grep -q "could not find the requested resource" "$fx/sc/err.txt"'

# ============================================================================
# the five REAL case bodies + REAL lib converge under the strict API
# ============================================================================
CASES="dependency-recovery foreign-resource-conflict singleton-rejection active-volume-disable-protection active-volume-delete-protection"
declare -A IT_S1_RC=()
for c in $CASES; do
  sl="s1-${c//-/_}"
  seed_for_case "$sl" "$c"
  ITEST_OV=()
  run_case_body "$c" "$sl"
  rc=$IT_CASE_RC
  IT_S1_RC[$c]=$rc
  ev="$fx/store/$sl/events.log"; log="$fx/out/$sl.log"; state="$fx/store/$sl/state.json"
  assert "$c: real body + real lib converge to PASS (rc=0)" '[ "$rc" = "0" ]'
  assert "$c: live run — no offline-stub fallback, no NOT RUN verdict" \
    '! grep -qE "NOT RUN \(offline|offline stub" "$log"'
  assert "$c: the cluster surface was actually used (fixture events recorded)" \
    '[ -s "$ev" ] && grep -q "^inv " "$ev"'
  assert "$c: final verdict in the run log is PASS" \
    'grep -qE "RESULT[:=] *PASS|final=PASS" "$log"'
  assert "$c: no event outside the authorized scope (ns-a + ngxstorage-nfs)" \
    '! (grep -E "^inv " "$ev" | grep -vE " ns=(ns-a|ngxstorage-nfs|-) " | grep -q .)'
  assert "$c: cleanup actually removed run-owned objects" \
    'grep -qE " outcome=(deleted|terminating) " "$ev"'
  if ownership_audit "$ev"; then
    assert "$c: every deletion targeted a run-created (or fixture-provisioned) UID only" 'true'
  else
    assert "$c: every deletion targeted a run-created (or fixture-provisioned) UID only" 'false'
  fi
  if run_created_gone "$state" "$ev"; then
    assert "$c: no run-created object survives the finished run (cleanup on every exit path)" 'true'
  else
    assert "$c: no run-created object survives the finished run (cleanup on every exit path)" 'false'
  fi
  if [[ "$c" == active-volume-* ]]; then
    assert "$c: the case-owned canaries were registered from create UIDs (UIDs in the event log)" \
      'grep -q "verb=create.*foreign-canary" "$ev"'
  fi
done

# ============================================================================
# wrong-behavior injection: a wrong reason/status must FAIL the case
# ============================================================================
for inj in wrong-reason wrong-status; do
  sl="s2-$inj"
  seed_for_case "$sl" "dependency-recovery"
  echo "$inj" > "$fx/store/$sl/ctl/inject"
  ITEST_OV=()
  run_case_body "dependency-recovery" "$sl"
  rc=$IT_CASE_RC; log="$fx/out/$sl.log"
  case "$inj" in
    wrong-reason) injected_artifact="MissingSecret" ;;
    wrong-status) injected_artifact="True WorkloadNotReady" ;;
  esac
  assert "injected $inj: the wrong operator behavior reached the API surface (bite evidence)" \
    "grep -q '$injected_artifact' '$log'"
  assert "injected $inj: case does not PASS (rc=1)" '[ "$rc" = "1" ]'
  assert "injected $inj: the wrong operator behavior is asserted out, visibly" \
    'grep -qE "CHECK FAIL|ASSERT-FAIL" "$log"'
  assert "injected $inj: no PASS verdict recorded" '! grep -qE "RESULT[:=] *PASS|final=PASS" "$log"'
done

# ============================================================================
# authority on the REAL lib surface: refused BEFORE dispatch with
# ZERO cluster events; in-scope calls + exec passthrough still work
# ============================================================================
mkdir -p "$fx/probe-case"
printf '%s\n' '{"name":"probe-case","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"integration probe fixture","plan_ref":"integration-probe L00"}' > "$fx/probe-case/case.json"
printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1","namespace":"foreign-ns"},"data":{"k":"v"}}' > "$fx/probe-foreign.json"
printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1","namespace":"ns-a"},"data":{"k":"v"}}' > "$fx/probe-cm.json"
printf '%s\n' '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"probe-pod","namespace":"ns-a"},"spec":{"containers":[{"name":"main","image":"fixture-local"}]}}' > "$fx/probe-pod.json"
# Scope-gate manifest probes (tri-state, ):
# cluster-scoped Quartet (real CRD scope), an UNRESOLVABLE kind, an SCC, and
# nested List containers (refusing the cluster item / allowing the ns items).
printf '%s\n' '{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true}}}}' > "$fx/probe-quartet-manifest.json"
printf '%s\n' '{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w1","namespace":"ns-a"}}' > "$fx/probe-widget-manifest.json"
printf '%s\n' '{"apiVersion":"security.openshift.io/v1","kind":"SecurityContextConstraints","metadata":{"name":"review-only"},"allowHostNetwork":true}' > "$fx/probe-scc-manifest.json"
printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true}}}}]}' > "$fx/probe-list-quartet.json"
printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1","namespace":"ns-a"},"data":{"k":"v"}}]}' > "$fx/probe-list-cm.json"

run_probe() { # $1=label $2=probe-function (runs after a successful real preflight)
  local sl="$1" fn="$2" rc=0
  local store="$fx/store/probe-$sl"
  mkdir -p "$store/ctl/seed"
 # TEST_NAMESPACE exists out-of-band (installer provisioning) — a
 # namespaced-scope case is never allowed to create it itself.
  printf '%s\n' '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns-a"}}' > "$store/ctl/seed/00-ns-a.json"
  ( env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a ALLOW_LIVE=allow-isolated \
      RUN_ID="itest-probe-$sl" EVIDENCE_DIR="$fx/ev/probe-$sl" SCENARIO_FAKE_STORE="$store" \
      TMPDIR="$fx/tmp/probe-$sl" PATH="$fx/bin:$PATH" \
      bash -c "scenario_preflight \"$fx/probe-case/case.json\" && $fn" ) >/dev/null 2>&1 || rc=$?
  IT_PROBE_RC=$rc
}
prb_foreign_ns_flag()    { scenario_kube delete configmap cm1 -n foreign-ns; }
prb_foreign_ns_manifest(){ scenario_kube create -f "$fx/probe-foreign.json"; }
prb_context_override()   { scenario_kube get configmap cm1 -n ns-a --context ctx-evil; }
prb_cluster_kind()       { scenario_kube delete namespace ns-a; }
# Protocol pin 2 (tightened target gate): the ATTACHED -s form must be
# refused exactly like split/-s= — a valid attached target that slips past
# the gate would be dispatched to a foreign cluster.
prb_server_attached()    { scenario_kube get configmap cm1 -n ns-a -shttps://127.0.0.1:1; }
prb_server_equal()       { scenario_kube get configmap cm1 -n ns-a --server=https://127.0.0.1:1; }
# Source-scope gate: a namespaced grant + cluster-scoped kind is refused even
# with the CORRECT -n — the namespace flag does not constrain a cluster
# object (the authorized source scope is verified, not the env string).
prb_cluster_kind_ns()    { scenario_kube delete csidriver nfs.csi.ngxstorage.com -n ns-a; }
# Tri-state kind lookup (): UNKNOWN kinds are NOT
# namespaced under a namespaced grant — the correct -n must not make an
# untabled kind pass. SCC carries its real OpenShift GVK
# (security.openshift.io/v1 SecurityContextConstraints, cluster-scoped).
prb_unknown_kind_ns()    { scenario_kube delete unknowncr my-object -n ns-a; }
prb_scc_ns()             { scenario_kube delete scc review-only -n ns-a; }
prb_quartet_ns()         { scenario_kube delete quartets quartet -n ns-a; }
prb_manifest_quartet()   { scenario_kube create -n ns-a -f "$fx/probe-quartet-manifest.json"; }
prb_manifest_scc()       { scenario_kube create -n ns-a -f "$fx/probe-scc-manifest.json"; }
prb_manifest_unknown()   { scenario_kube create -n ns-a -f "$fx/probe-widget-manifest.json"; }
prb_manifest_list_quartet() { scenario_kube create -n ns-a -f "$fx/probe-list-quartet.json"; }
# Helper surface under a namespaced grant: the pinned cleanup pair refuses a
# cluster-scoped kind the same way (rc 2, zero events) — even a --namespace
# is refused (cluster kind + namespace = misuse) before any dispatch.
prb_delete_owned_quartet_ns() { scenario_delete_owned quartets quartet --namespace ns-a some-uid; }
prb_delete_owned_quartet_bare() { scenario_delete_owned quartets quartet some-uid; }
prb_in_scope_get()       { scenario_kube get configmap cm1 -n ns-a; }
prb_in_scope_pod()       { scenario_kube get pod probe-pod -n ns-a; }
prb_exec_passthru() {
  scenario_kube create -f "$fx/probe-pod.json" -n ns-a && \
  scenario_kube exec probe-pod -n ns-a -- printf 'ok --context evil -n foreign-ns'
}
prb_restore_create() { scenario_kube create -f "$fx/probe-cm.json" -n ns-a && scenario_kube get configmap cm1 -n ns-a -o json; }
export -f prb_foreign_ns_flag prb_foreign_ns_manifest prb_context_override prb_cluster_kind \
          prb_server_attached prb_server_equal prb_cluster_kind_ns \
          prb_unknown_kind_ns prb_scc_ns prb_quartet_ns \
          prb_manifest_quartet prb_manifest_scc prb_manifest_unknown prb_manifest_list_quartet \
          prb_delete_owned_quartet_ns prb_delete_owned_quartet_bare \
          prb_in_scope_get prb_in_scope_pod prb_exec_passthru prb_restore_create

for spec in "foreign-ns-flag|prb_foreign_ns_flag" "foreign-ns-manifest|prb_foreign_ns_manifest" \
            "context-override|prb_context_override" "cluster-scoped-kind|prb_cluster_kind" \
            "server-attached-form|prb_server_attached" "server-equal-form|prb_server_equal" \
            "cluster-kind-under-namespaced-grant|prb_cluster_kind_ns" \
            "unknown-kind-under-namespaced-grant|prb_unknown_kind_ns" \
            "scc-cluster-kind-under-namespaced-grant|prb_scc_ns" \
            "quartet-cluster-kind-under-namespaced-grant|prb_quartet_ns" \
            "manifest-carries-cluster-quartet|prb_manifest_quartet" \
            "manifest-carries-scc|prb_manifest_scc" \
            "manifest-carries-unknown-kind|prb_manifest_unknown" \
            "manifest-nested-list-with-quartet-item|prb_manifest_list_quartet" \
            "delete-owned-quartet-nsp-grant|prb_delete_owned_quartet_ns" \
            "delete-owned-quartet-bare-nsp-grant|prb_delete_owned_quartet_bare"; do
  sl="${spec%%|*}"; fn="${spec#*|}"
  run_probe "$sl" "$fn"
  assert "$sl: refused pre-dispatch (rc=2) with ZERO cluster events" \
    '[ "$IT_PROBE_RC" = "2" ] && [ ! -s "$fx/store/probe-$sl/events.log" ]'
done

run_probe in-scope-get prb_in_scope_get
assert "in-scope get is allowed and recorded under the authorized context" \
  '[ "$IT_PROBE_RC" = "1" ] && [ "$(grep -c " verb=get " "$fx/store/probe-in-scope-get/events.log")" = "1" ] && grep -q "ctx=ctx-a" "$fx/store/probe-in-scope-get/events.log"'

run_probe in-scope-pod prb_in_scope_pod
assert "valid Pod kind under the namespaced grant still dispatches (tri-state does not over-refuse known namespaced kinds)" \
  '[ "$IT_PROBE_RC" = "1" ] && [ "$(grep -c " verb=get " "$fx/store/probe-in-scope-pod/events.log")" = "1" ]'

# Nested List container whose items are all namespaced: the scan RESOLVES the
# items (not the "List" wrapper) and dispatches; the fixture's own strictness
# may still reject the verb, but the GATE must not.
run_probe manifest-list-cm-ok "scenario_kube create -n ns-a -f \"$fx/probe-list-cm.json\""
assert "manifest List of only namespaced items passes the scope scan (dispatch reached the binary, rc=$IT_PROBE_RC)" \
  '[ "$IT_PROBE_RC" != "2" ] && [ -s "$fx/store/probe-manifest-list-cm-ok/events.log" ]'

run_probe exec-passthru prb_exec_passthru
assert "exec ... -- pod args reach the pod command untouched (not normalized as kubectl globals)" \
  '[ "$IT_PROBE_RC" = "0" ] && grep -F -- "ok --context evil -n foreign-ns" "$fx/store/probe-exec-passthru/events.log" | grep -q "ctx=ctx-a"'

run_probe restore-create prb_restore_create
assert "authority probes restore: a fresh store accepts normal in-scope calls again" \
  '[ "$IT_PROBE_RC" = "0" ] && [ "$(grep -c " verb=create " "$fx/store/probe-restore-create/events.log")" = "1" ] && [ "$(grep -c " verb=get " "$fx/store/probe-restore-create/events.log")" = "1" ]'

# ============================================================================
# (composition audit) UID replacement on ALL FIVE real bodies: the Quartet object is swapped
# under the case (new UID + marker annotation) after the body captured
# it; the run cannot PASS over it, and the replacement is never adopted
# nor deleted — the pinned delete helper refuses it (rc 2)
# ============================================================================
for c in $CASES; do
  sl="s4-${c//-/_}"
  seed_for_case "$sl" "$c"
  echo 2 > "$fx/store/$sl/ctl/replace-on-quartet-get"   # after the body captured the UID
  ITEST_OV=()
  run_case_body "$c" "$sl"
  rc=$IT_CASE_RC; log="$fx/out/$sl.log"; state="$fx/store/$sl/state.json"; ev="$fx/store/$sl/events.log"
  REPL_UID="$(python3 - "$state" <<'PY'
import json, sys
st = json.load(open(sys.argv[1]))
for o in st["objects"].values():
    if "scenario-integration/replaced" in ((o.get("metadata") or {}).get("annotations") or {}):
        print(o["metadata"]["uid"])
        break
PY
)"
  assert "$c: UID replacement — the run does not PASS (rc=$rc)" '[ "$rc" != "0" ]'
  assert "$c: UID replacement — no PASS verdict recorded" \
    '! grep -qE "RESULT[:=] *PASS|final=PASS" "$log"'
  assert "$c: UID replacement — the fixture injection actually fired (bite evidence, not vacuous)" \
    'grep -q "verb=replace-inject" "$ev"'
  assert "$c: UID replacement — the swapped object was surfaced as a failure, not silently accepted" \
    'grep -qE "CHECK FAIL|ASSERT-FAIL|RESULT: FAIL|RESULT=FAIL|RESULT: BLOCKED|RESULT=BLOCKED|RESULT: FAIL_CLEANUP|RESULT=FAIL_CLEANUP|refused|ownership" "$log"'
  assert "$c: UID replacement — the replacement survived — never adopted or deleted" \
    '[ -n "$REPL_UID" ] && grep -qF "$REPL_UID" "$state" && ! grep -E "outcome=(deleted|terminating)" "$ev" | grep -qF "target_uid=$REPL_UID "'
done

# ============================================================================
# cleanup ownership: a pre-run foreign object with a fixed UID survives;
# only run-owned, UID-preconditioned objects are removed
# ============================================================================
sl="s5-ownership"
seed_for_case "$sl" "dependency-recovery"
# A pre-run foreign object with a fixed UID, inside the OPERATOR-owned operand
# namespace (a namespace the run creates is legitimately deletable with
# contents, like the real API's cascade GC — not a fair foreignness probe).
printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"foreign-preexisting-cm","namespace":"ngxstorage-nfs","uid":"foreign-preseed-uid-1"},"data":{"payload":"not ours"}}' > "$fx/store/$sl/ctl/seed/08-foreign-cm.json"
ITEST_OV=()
run_case_body "dependency-recovery" "$sl"
rc=$IT_CASE_RC; state="$fx/store/$sl/state.json"; ev="$fx/store/$sl/events.log"
assert "foreign pre-run object survives the whole case (cleanup never claims it)" \
  'grep -q "foreign-preseed-uid-1" "$state" && ! grep "target_uid=foreign-preseed-uid-1" "$ev" | grep -q "outcome=deleted"'
assert "foreign pre-run object deletion was never even attempted" \
  '! grep "target_uid=foreign-preseed-uid-1" "$ev" | grep -q "outcome="'
assert "s5 run converges to PASS (rc=0) with ownership-scoped cleanup" '[ "$rc" = "0" ]'
if ownership_audit "$ev"; then
  assert "s5: every deletion targeted a run-created (or fixture-provisioned) UID only" 'true'
else
  assert "s5: every deletion targeted a run-created (or fixture-provisioned) UID only" 'false'
fi

# ============================================================================
# fail-closed: an unexpected mid-run crash (SIGTERM to the case shell)
# must NEVER become exit 0; then the disarmed store restores behavior
# ============================================================================
for spec in "dependency-recovery|s6-crash-w3|3" "active-volume-delete-protection|s6-crash-w4|5"; do
  c="${spec%%|*}"; rest="${spec#*|}"; sl="${rest%%|*}"; mut_n="${rest#*|}"
  seed_for_case "$sl" "$c"
  echo "$mut_n" > "$fx/store/$sl/ctl/kill-on-mutation"
  ITEST_OV=()
  run_case_body "$c" "$sl"
  rc=$IT_CASE_RC; log="$fx/out/$sl.log"
  assert "$c: the crash injection actually fired at the armed mutation (bite evidence, not vacuous)" \
    'grep -q "note=crash-injection" "$fx/store/$sl/events.log"'
  assert "$c: mid-run crash (SIGTERM on mutation $mut_n) does not become exit 0" '[ "$rc" != "0" ]'
  assert "$c: crash leaves no PASS verdict behind" '! grep -qE "RESULT[:=] *PASS|final=PASS" "$log"'
done

# restore: disarm the kill on a fresh store and rerun — behavior returns to the
# plain S1 result for the same case (the probe consumed its own arm; nothing
# sticks in the lib, bodies, or fixture engine).
sl="s6-restore"
seed_for_case "$sl" "active-volume-delete-protection"
ITEST_OV=()
run_case_body "active-volume-delete-protection" "$sl"
rc=$IT_CASE_RC
assert "crash probe restores: disarmed rerun matches the plain S1 outcome and records no crash injection" \
  '[ "$rc" = "${IT_S1_RC[active-volume-delete-protection]}" ] && ! grep -q "note=crash-injection" "$fx/store/$sl/events.log"'

# ============================================================================
# real run.sh single-case dispatch: same exit protocol as the direct run
# ============================================================================
sl="s7-runner"
seed_for_case "$sl" "dependency-recovery"
S7_META="$(_scenario_parse_case_json "$scen/cases/dependency-recovery/case.json")"
S7_GRANT="allow-$(sed -n '4p' <<<"$S7_META")"
if declare -F _scenario_effect_tier >/dev/null; then
  S7_GRANT="allow-$(_scenario_effect_tier "$(sed -n '4p' <<<"$S7_META")" "$(sed -n '5p' <<<"$S7_META")")"
fi
rc=0
env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a \
    ALLOW_LIVE="$S7_GRANT" \
    EVIDENCE_DIR="$fx/ev/$sl" SCENARIO_FAKE_STORE="$fx/store/$sl" \
    TMPDIR="$fx/tmp/$sl" SCENARIO_WAIT_SECONDS=2 \
    SCENARIO_MAX_POLLS=5 SCENARIO_POLL_INTERVAL=0.2 \
    PATH="$fx/bin:$PATH" \
    bash "$scen/run.sh" dependency-recovery >"$fx/out/$sl.log" 2>&1 || rc=$?
assert "runner dispatch returns the same exit protocol as the direct run (rc parity)" \
  '[ "$rc" = "${IT_S1_RC[dependency-recovery]}" ]'

# ============================================================================
# the pinned atomic cleanup surface, driven through the REAL lib gate +
# the recording/strict fixture: exact names/argv/returns, the
# ownership.tsv evidence rows, swap refusal (rc 2, object survives),
# postcondition-verified delete, absence idempotency, and a context
# drift registering NOTHING — a refusal is never a success
# ============================================================================
sl="s8-atomic"
seed_for_case "$sl" "dependency-recovery"
mkdir -p "$fx/ev/$sl"
cat > "$fx/s8-probe.sh" <<'PROBE'
#!/usr/bin/env bash
# Runs inside the exported REAL lib surface (scenario_preflight/scenario_kube/
# scenario_create_tracked/scenario_delete_owned). Exercises the pinned calls
# exactly the way the case bodies do; exit codes 10..22 mark the failing step.
set -u
case_dir="$1"
scenario_preflight "$case_dir/case.json" || exit 9
declare -F scenario_create_tracked >/dev/null 2>&1 || \
  source "$case_dir/fixtures/scenario_atomic_cleanup.sh"
reg="$EVIDENCE_DIR/ownership.tsv"
: > "$reg"
# ns-a is not pre-seeded for this case's preconditions — create it through the
# pinned helper FIRST (cluster-scoped kind: no --namespace allowed at all).
printf '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns-a"}}' > "$TMPDIR/s8-ns.json"
rc=0; scenario_create_tracked namespace ns-a "$TMPDIR/s8-ns.json" >/dev/null || rc=$?
[[ "$rc" == "0" ]] || exit 10
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"s8-cm","namespace":"ns-a"},"data":{"p":"owned"}}' > "$TMPDIR/s8-cm.json"
rc=0; scenario_create_tracked configmap s8-cm --namespace ns-a "$TMPDIR/s8-cm.json" >/dev/null || rc=$?
[[ "$rc" == "0" ]] || exit 10
uid1="$(awk -F'\t' '$1=="CREATE" && $6=="created" && $2=="configmap" && $3=="ns-a" && $4=="s8-cm" {print $5}' "$reg")"
[[ -n "$uid1" ]] || exit 11
python3 - "$SCENARIO_FAKE_STORE/state.json" <<'PY'
import json, sys
st = json.load(open(sys.argv[1]))
for o in st["objects"].values():
    if o.get("kind") == "ConfigMap" and o["metadata"]["name"] == "s8-cm":
        o["metadata"]["uid"] = "s8-swapped-uid"
json.dump(st, open(sys.argv[1], "w"), indent=1, sort_keys=True)
PY
rc=0; scenario_delete_owned configmap s8-cm --namespace ns-a "$uid1" >/dev/null 2>&1 || rc=$?
[[ "$rc" == "2" ]] || exit 12
grep -q "s8-swapped-uid" "$SCENARIO_FAKE_STORE/state.json" || exit 13
grep -qF "$(printf 'DELETE\tconfigmap\tns-a\ts8-cm\t%s\trefused-uid-mismatch' "$uid1")" "$reg" || exit 14
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"s8-cm2","namespace":"ns-a"},"data":{"p":"owned"}}' > "$TMPDIR/s8-cm2.json"
rc=0; scenario_create_tracked configmap s8-cm2 --namespace ns-a "$TMPDIR/s8-cm2.json" >/dev/null || rc=$?
[[ "$rc" == "0" ]] || exit 15
uid2="$(awk -F'\t' '$1=="CREATE" && $6=="created" && $2=="configmap" && $3=="ns-a" && $4=="s8-cm2" {print $5}' "$reg")"
[[ -n "$uid2" ]] || exit 16
rc=0; scenario_delete_owned configmap s8-cm2 --namespace ns-a "$uid2" || rc=$?
[[ "$rc" == "0" ]] || exit 17
grep -q '"name": "s8-cm2"' "$SCENARIO_FAKE_STORE/state.json" && exit 18
grep -qF "$(printf 'DELETE\tconfigmap\tns-a\ts8-cm2\t%s\tdeleted' "$uid2")" "$reg" || exit 19
rc=0; scenario_delete_owned configmap s8-cm2 --namespace ns-a "$uid2" >/dev/null 2>&1 || rc=$?
[[ "$rc" == "0" ]] || exit 20
grep -qF "$(printf 'DELETE\tconfigmap\tns-a\ts8-cm2\t%s\tabsent' "$uid2")" "$reg" || exit 21
# GET-then A→B replacement before the delete → the server-side
# DeleteOptions.body precondition answers 409: the deletion is NEVER a
# success (rc 1 = API error; a Conflict is not a clean NotFound), B's fields
# are fully preserved, and the registry carries the unverified api-error
# outcome — never a deleted/PASS record.
mkdir -p "$SCENARIO_FAKE_STORE/ctl"
echo s8-replaced-uid-B > "$SCENARIO_FAKE_STORE/ctl/replace-before-delete"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"s8-cm3","namespace":"ns-a"},"data":{"p":"owned"}}' > "$TMPDIR/s8-cm3.json"
rc=0; uid3="$(scenario_create_tracked configmap s8-cm3 --namespace ns-a "$TMPDIR/s8-cm3.json" 2>"$EVIDENCE_DIR/create3.err")" || rc=$?
[[ "$rc" == "0" && -n "$uid3" ]] || exit 25
rc=0; scenario_delete_owned configmap s8-cm3 --namespace ns-a "$uid3" >/dev/null 2>&1 || rc=$?
# the pinned outcome for a server-side 409 is a REFUSAL (rc 2), never success
[[ "$rc" == "2" ]] || exit 26
grep -q '"name": "s8-cm3"' "$SCENARIO_FAKE_STORE/state.json" || exit 27
grep -q "\"p\": \"owned\"" "$SCENARIO_FAKE_STORE/state.json" || exit 28
grep -qF "$(printf 'DELETE\tconfigmap\tns-a\ts8-cm3\t%s\tprecondition-conflict' "$uid3")" "$reg" || exit 29
grep -qF "$(printf 'DELETE\tconfigmap\tns-a\ts8-cm3\t%s\tdeleted' "$uid3")" "$reg" && exit 30
# consume the cm3 race arm — a stale arm would swap the NEXT delete too
mv "$SCENARIO_FAKE_STORE/ctl/replace-before-delete" "$SCENARIO_FAKE_STORE/ctl/replace-before-delete.consumed" || exit 30
# --- cluster-scoped Quartet (real CRD spec.scope: Cluster) -----------------
# The helper must NOT emit a namespace for it: no --namespace at all, the
# exact cluster raw route, and the UID body enforced server-side.
printf '{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true,"secretRef":{"name":"nfs-csi-ngxstorage-config"}}}}}' > "$TMPDIR/s8-q.json"
rc=0; q_uid="$(scenario_create_tracked quartet quartet "$TMPDIR/s8-q.json" 2>"$EVIDENCE_DIR/create-q.err")" || rc=$?
[[ "$rc" == "0" && -n "$q_uid" ]] || exit 31
grep -qF "$(printf 'CREATE\tquartet\t-\tquartet\t%s\tcreated' "$q_uid")" "$reg" || exit 31
# misuse: --namespace on the cluster kind is refused BEFORE dispatch
rc=0; scenario_delete_owned quartet quartet --namespace ns-a "$q_uid" >/dev/null 2>&1 || rc=$?
[[ "$rc" == "2" ]] || exit 32
# the clean delete FIRST — exact cluster route, postcondition-verified
rc=0; scenario_delete_owned quartet quartet "$q_uid" >/dev/null 2>&1 || rc=$?
[[ "$rc" == "0" ]] || exit 33
grep -qF "$(printf 'DELETE\tquartet\t-\tquartet\t%s\tdeleted' "$q_uid")" "$reg" || exit 34
# then re-create and run the GET->A->B race on the CLUSTER route: the 409
# proves the UID body traveled. The replacement carries the fixture's
# replaced-marker, so the operator model refuses to advance it — the helper
# must REFUSE it (the postcondition can never verify), never claim a delete.
cp "$TMPDIR/s8-q.json" "$TMPDIR/s8-q2.json"
q_uid2="$(scenario_create_tracked quartet quartet "$TMPDIR/s8-q2.json" 2>"$EVIDENCE_DIR/create-q2.err")" || exit 35
[[ -n "$q_uid2" ]] || exit 36
echo s8-q-replaced-B > "$SCENARIO_FAKE_STORE/ctl/replace-before-delete"
rc=0; scenario_delete_owned quartet quartet "$q_uid2" >/dev/null 2>&1 || rc=$?
[[ "$rc" == "2" ]] || exit 37
grep -q "s8-q-replaced-B" "$SCENARIO_FAKE_STORE/state.json" || exit 38
grep -qF "$(printf 'DELETE\tquartet\t-\tquartet\t%s\tprecondition-conflict' "$q_uid2")" "$reg" || exit 39
mv "$SCENARIO_FAKE_STORE/ctl/replace-before-delete" "$SCENARIO_FAKE_STORE/ctl/replace-before-delete.consumed" || exit 40
# context drift AFTER the gate: the call is refused before dispatch — zero
# cluster events, nothing registered, never a success.
KUBE_CONTEXT=ctx-evil
n_before="$(wc -l < "$SCENARIO_FAKE_STORE/events.log")"
rc=0; scenario_create_tracked configmap s8-cm2 --namespace ns-a "$TMPDIR/s8-cm2.json" >/dev/null 2>&1 || rc=$?
[[ "$rc" == "2" ]] || exit 22
[[ "$(wc -l < "$SCENARIO_FAKE_STORE/events.log")" == "$n_before" ]] || exit 23
grep -qF "$(printf 'CREATE\tconfigmap\tns-a\ts8-cm2\t\tapi-error' '')" "$reg" 2>/dev/null && exit 24
exit 0
PROBE
AT_PROBE_RC=0
env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a \
    ALLOW_LIVE=allow-cluster-scoped-fault RUN_ID="itest-s8" \
    EVIDENCE_DIR="$fx/ev/$sl" SCENARIO_FAKE_STORE="$fx/store/$sl" \
    TMPDIR="$fx/tmp/$sl" PATH="$fx/bin:$PATH" \
    bash "$fx/s8-probe.sh" "$scen/cases/dependency-recovery" >"$fx/out/s8.log" 2>&1 || AT_PROBE_RC=$?
assert "atomic surface: pinned create/delete driven through the REAL lib gate (probe rc=$AT_PROBE_RC)" \
  '[ "$AT_PROBE_RC" = "0" ]'
assert "atomic surface: six creates (namespace + three ConfigMaps + two cluster Quartets) and exactly ONE verified delete reached the API as an unconditional removal (refusal paths touched nothing)" \
  '[ "$(grep -c " verb=create " "$fx/store/$sl/events.log")" = "6" ] && [ "$(grep -c "outcome=deleted" "$fx/store/$sl/events.log")" = "1" ]'
assert "atomic surface: the cluster Quartet clean delete went through the finalizer path (exactly one outcome=terminating, on the cluster route) and the helper verified absence via the postcondition get" \
  '[ "$(grep -cE "outcome=terminating" "$fx/store/$sl/events.log")" = "1" ] && grep -E "outcome=terminating" "$fx/store/$sl/events.log" | grep -q "kind=Quartet name=quartet path=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet "'
assert "atomic surface: the GET→A→B races before the deletes were answered by the server-side precondition (exactly TWO 409/conflict rejections — the namespaced ConfigMap AND the cluster Quartet — NO deleted record for a replaced object)" \
  '[ "$(grep -cE "outcome=(conflict|client-rejected)" "$fx/store/$sl/events.log")" = "2" ] && ! (grep -E "outcome=deleted" "$fx/store/$sl/events.log" | grep -qE "name=(s8-cm3|quartet)")'
assert "atomic surface: the cluster Quartet race was answered 409 ON THE EXACT cluster raw route — the UID body traveled (a 409 only fires when the body precondition mismatches the live uid)" \
  'grep -E "outcome=conflict" "$fx/store/$sl/events.log" | grep -q "kind=Quartet name=quartet path=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet "'
assert "atomic surface: the cluster Quartet delete used the EXACT cluster raw route (no namespace segment anywhere in it)" \
  'grep -q "verb=delete .*path=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet " "$fx/store/$sl/events.log"'
assert "atomic surface: a NAMESPACED raw Quartet route was never emitted by the helper" \
  '! grep -q "namespaces/ns-a/quartets" "$fx/store/$sl/events.log"'
assert "atomic surface: the swapped object survived with B's fields intact and its unmet obligation stayed in the registry (never silently dropped)" \
  'grep -q "s8-swapped-uid" "$fx/store/$sl/state.json" && grep -q "\"p\": \"owned\"" "$fx/store/$sl/state.json" && grep -q "^CREATE" "$fx/ev/$sl/ownership.tsv" && [ "$(grep -c "s8-cm2" "$fx/ev/$sl/ownership.tsv")" = "3" ]'
assert "atomic surface: the registry carries exactly the twelve verified outcomes — both 409 races (namespaced ConfigMap + cluster Quartet) recorded precondition-conflict (never deleted/absent), the clean cluster Quartet delete recorded deleted with namespace '-', and the drifted refusal wrote nothing" \
  '[ "$(wc -l < "$fx/ev/$sl/ownership.tsv")" = "12" ] && [ "$(grep -c "precondition-conflict" "$fx/ev/$sl/ownership.tsv")" = "2" ] && grep -qP "^CREATE\tquartet\t-\tquartet\t[^\t]+\tcreated\t" "$fx/ev/$sl/ownership.tsv" && grep -qP "^DELETE\tquartet\t-\tquartet\t[^\t]+\tdeleted\t" "$fx/ev/$sl/ownership.tsv" && ! grep -qE "api-error|no-uid-in-response" "$fx/ev/$sl/ownership.tsv"'
assert "atomic surface: the pinned delete argv is the DeleteOptions raw form on the cluster surface" \
  'grep -q "verb=delete .*path=/api/v1/namespaces/ns-a/configmaps/s8-cm3" "$fx/store/$sl/events.log"'
assert "atomic surface: the invalid name+field-selector form never produced a delete (no-match semantics are gone)" \
  '! grep -q "outcome=no-match" "$fx/store/$sl/events.log"'

# ============================================================================
# (S8b) REAL kubectl transport — a SEPARATE record from the API-server
# precondition enforcement above. The enforcement is proven against the
# recording fixture (self-checks + S8 through the real lib helpers).
# The real-kubectl transport (an actual kubectl binary transmitting the
# pinned DeleteOptions.body over a loopback API socket) needs both a real
# binary and a loopback endpoint; when the socket is restricted — or the
# binary is absent, as in an offline run — this piece is BLOCKED,
# recorded honestly in its own file, and is NEVER a fake PASS.
# ============================================================================
sl="s8b-transport"
mkdir -p "$fx/ev/$sl"
rec="$fx/ev/$sl/real-kubectl-transport.record"
real_kb="$(command -v kubectl 2>/dev/null || true)"
tendpoint="${SCENARIO_REAL_TRANSPORT_ENDPOINT:-}"
tstatus="BLOCKED"; treason=""
if [[ -z "$real_kb" ]]; then
  treason="real kubectl binary not on this machine's PATH (offline contract run — no transport to prove)"
elif [[ -z "$tendpoint" ]]; then
  treason="loopback API endpoint restricted: SCENARIO_REAL_TRANSPORT_ENDPOINT unset (no live cluster/backend in this run)"
else
  tresp="$(printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"quartet"},"preconditions":{"uid":"00000000-0000-0000-0000-000000000000"}}' \
    | "$real_kb" --server "$tendpoint" delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet -f - 2>&1)" || tresp="rc=$? :: $tresp"
  if [[ "$tresp" == *deleted* ]]; then
    tstatus="PASS"; treason="real kubectl transmitted the pinned DeleteOptions.body over $tendpoint"
  else
    treason="real kubectl transport against $tendpoint did not confirm the delete: $(printf '%s' "$tresp" | head -c 200)"
  fi
fi
{
  echo "record=real-kubectl-transport"
  echo "status=$tstatus"
  echo "reason=${treason:-none}"
  echo "kubectl=${real_kb:-none}"
  echo "endpoint=${tendpoint:-none}"
  echo "separate_from=API-server precondition enforcement (self-checks + S8, recording fixture — that proof stands independently)"
} > "$rec"
assert "real-kubectl transport is kept a SEPARATE record (never merged into the fixture precondition proof)" \
  '[ -s "$rec" ] && grep -q "^status=" "$rec" && grep -q "^separate_from=" "$rec"'
assert "real-kubectl transport: no fake PASS — a PASS must carry response evidence, a BLOCKED must carry a reason" \
  '! (grep -q "^status=PASS" "$rec" && ! grep -q "^reason=" "$rec") && ! (grep -q "^status=BLOCKED" "$rec" && grep -q "^reason=none" "$rec")'
if [[ "$tstatus" == "BLOCKED" ]]; then
  assert "real-kubectl transport is BLOCKED in this offline run with an honest reason (restricted loopback socket / absent binary)" \
    'grep -q "^status=BLOCKED" "$rec"'
fi

# ============================================================================
# (real CRD scope) fixture contract bound to the REAL CRD scope (independent parse) —
# asserting the same constant in two places is NOT a binding (that is
# how the namespaced Quartet model went undetected): spec.scope is
# parsed from config/crd/bases/quartet.ngxstorage.com_quartets.yaml and
# EVERY model — the canonical lib table, all five fixture tables, the
# API-fixture KINDS routing, and the synthetic CRD seed — is asserted to
# model the parsed value.
# ============================================================================
REAL_CRD="$root/config/crd/bases/quartet.ngxstorage.com_quartets.yaml"
REAL_SCOPE="$(python3 - "$REAL_CRD" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
in_spec = False
for line in text.splitlines():
    if re.match(r'^spec:\s*$', line):
        in_spec = True
        continue
    if in_spec:
        if line and not line.startswith(" ") and not line.startswith("#"):
            break  # next top-level key: spec block ended
        m = re.match(r'^  scope:\s*([A-Za-z]+)\s*$', line)
        if m:
            sys.stdout.write(m.group(1))
            sys.exit(0)
sys.exit(2)
PY
)" || { echo "real CRD scope: real CRD scope unparseable — test setup bug" >&2; exit 1; }
assert "real CRD spec.scope parses independently of every fixture constant" \
  '[[ -n "$REAL_SCOPE" ]] && [[ "$REAL_SCOPE" == "Cluster" || "$REAL_SCOPE" == "Namespaced" ]]'
EXPECT_SCOPE="cluster"; [[ "$REAL_SCOPE" == "Namespaced" ]] && EXPECT_SCOPE="namespaced"

# (a) the canonical lib table models the real scope — tri-state lookup
LIB_SCOPE="$(_scenario_kind_is_cluster_scoped quartet scope)"
assert "canonical lib GVK table models the REAL CRD scope for Quartet (lib=$LIB_SCOPE real=$REAL_SCOPE→$EXPECT_SCOPE)" \
  '[[ "$LIB_SCOPE" == "$EXPECT_SCOPE" ]]'
assert "lib table carries SCC with its real OpenShift GVK (security.openshift.io/v1 SecurityContextConstraints, cluster-scoped)" \
  '[[ "$(_scenario_kind_is_cluster_scoped scc scope)" == "cluster" ]] && [[ "$(_scenario_kind_is_cluster_scoped scc gvk)" == "security.openshift.io|v1|securitycontextconstraints|cluster" ]]'

# (b) all FIVE fixture tables model the real scope (each sourced in a
# subshell — the fixture's own lookup, not the lib's)
for fix in "$scen"/cases/*/fixtures/scenario_atomic_cleanup.sh; do
  fix_case="$(basename "$(dirname "$(dirname "$fix")")")"
  FIX_SCOPE="$( ( source "$fix" >/dev/null 2>&1; _atomic_kind_is_cluster_scoped quartet scope ) 2>/dev/null )"
  assert "fixture [$fix_case] models the REAL CRD scope for Quartet (fixture=$FIX_SCOPE real=$REAL_SCOPE→$EXPECT_SCOPE)" \
    '[[ "$FIX_SCOPE" == "$EXPECT_SCOPE" ]]'
done

# (c) the API fixture ROUTED Quartet by the real scope (behavioral, from the
# fixture self-check store: the object is filed under the cluster key
# "Quartet|-|quartet", NOT a namespace key)
assert "API-fixture KINDS routing files the Quartet under the cluster key (behavioral proof of scope=$EXPECT_SCOPE)" \
  'grep -q "\"Quartet|-|quartet\"" "$fx/store/selfcheck/state.json"'

# (d) the synthetic CRD seed (the installed-cluster precondition) carries the
# real scope — parsed value, not a re-asserted constant
assert "synthetic CRD seed carries the REAL scope (not the inverse schema)" \
  'grep -q "\"scope\":\"$REAL_SCOPE\"" <<<"$CRD_JSON"'

# (e) the offline fake model (fake_cluster.sh) agrees with the real scope —
# the reconcile loop must file Quartet under the cluster-scoped store dir
FC_SCOPE="$( ( source "$scen/cases/dependency-recovery/fixtures/fake_cluster.sh" 2>/dev/null; fc_is_cluster_scoped quartet ) && echo cluster || echo namespaced )"
assert "offline fake_cluster model agrees with the REAL CRD scope (fake=$FC_SCOPE real=$EXPECT_SCOPE)" \
  '[[ "$FC_SCOPE" == "$EXPECT_SCOPE" ]]'

# ============================================================================
# (S9) gate refusal is never a success — on ALL FIVE real bodies: an
# under-granted run (allow-isolated vs the cluster-scoped-fault effect
# tier) is rejected BEFORE any mutation: rc 2, zero cluster events, no
# PASS verdict
# ============================================================================
for c in $CASES; do
  sl="s9-${c//-/_}"
  seed_for_case "$sl" "$c"
  ITEST_OV=(ALLOW_LIVE=allow-isolated)
  run_case_body "$c" "$sl"
  rc=$IT_CASE_RC; log="$fx/out/$sl.log"
  assert "$c: under-granted run is BLOCKED (rc=2), never PASS" '[ "$rc" = "2" ]'
  assert "$c: refusal produced ZERO cluster events (rejected before anything)" \
    '[ ! -s "$fx/store/$sl/events.log" ]'
  assert "$c: no PASS verdict recorded behind the refusal" \
    '! grep -qE "RESULT[:=] *PASS|final=PASS" "$log"'
done

# ============================================================================
if [ "$fail" -ne 0 ]; then
  echo "scenario case-integration contract: $ok OK, $fail FAIL — convergence not yet met (expected mid-wave; re-run as the gate after all workers finish)" >&2
  exit 1
fi
echo "tests/scenario/test-case-integration.sh PASS ($ok OK: five real bodies + real lib vs strict recording API; shared atomic cleanup (create/delete pinned helpers); wrong-behavior fails; authority refused pre-dispatch; UID replacement on all five never adopted; cleanup UID-owned; crash fail-closed; runner parity)"
