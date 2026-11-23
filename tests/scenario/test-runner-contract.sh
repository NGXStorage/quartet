#!/usr/bin/env bash
# test-runner-contract.sh — offline contract for the Phase B scenario runner.
#
# Proves, WITHOUT touching any live cluster and WITHOUT pointing back at the
# real cases/ (recursion-free):
# (1) discovery (--list/--help) is side-effect-free — no kubectl/oc/go call,
# no case CODE executed, no live env required;
# (2) risk-scoped inclusion — `all` triggers only `isolated` cases;
# cluster-scoped-fault/uninstall are excluded and never auto-triggered;
# (3) explicit risk selection + risk-scoped grant (ALLOW_LIVE=allow-<risk>),
# single-case dispatch, unknown arg/case/risk and extra-arg rejection;
# (4) the shared gate (scenario_preflight) runs even when a case is invoked
# DIRECTLY — zero mutation on rejection (missing target, missing grant,
# invalid metadata);
# (5) context mismatch is refused BEFORE any kubectl/oc call;
# (6) FAIL_CLEANUP is reported separately and never becomes success;
# (7) run-id + identity + source revision are bound to the evidence;
# (8) arg/env propagation (KUBE_BIN=oc, TEST_NAMESPACE, KUBE_BIN default);
# (9) SKIP/BLOCKED/FAIL aggregation ordering (FAIL outranks SKIP/BLOCKED);
# (10) the REAL linear-body case protocol (call scenario_preflight itself,
# read RUN_ID/SCENARIO_FAKE_STORE/SOURCE_REV/CASE_SHA, exit 0-4, no
# .status file) runs unmodified through the runner's exported live lib;
# (11) EFFECT-based selection bound to the FIVE REAL case.json files —
# singleton-lifecycle cases (dependency-recovery, singleton-rejection)
# are never executed under `all`/allow-isolated, and a lying
# risk=isolated+scope=cluster-scoped stub is classified by effect;
# (12) scenario_kube refuses global target/auth argv options (--context,
# --kubeconfig, --server, --token, ...) BEFORE dispatch with zero binary
# events; `exec ... -- <pod-command>` args are never mangled;
# (13) namespace binding — a namespaced-scope call without the authorized -n,
# or with a foreign -n, is refused; TEST_NAMESPACE drift is refused;
# (14) broken sweep metadata is FAIL-CLOSED — a run containing it never
# PASSes AND the whole selection is validated before the first
# dispatch: a broken + valid mix asserts ZERO case mutation;
# (15) evidence isolation — per-case subdir + immutable run identity + own
# status/cleanup records, no shared generic files;
# (16) the shared atomic-cleanup pair (CASE-CONTRACT "Ortak atomik cleanup
# helper (parent-pin)"): scenario_create_tracked registers the
# create-RESPONSE uid (not a name guess) in EVIDENCE_DIR/ownership.tsv;
# scenario_delete_owned refuses a UID-swapped object (2, zero delete
# dispatch, object survives); it does nothing on a foreign same-name
# object (no registry row, foreign uid + payload intact); a refused
# cleanup surfaces as FAIL_CLEANUP(4), never success. The probe cases
# source NO lib and declare -F all four contracted names, so they run
# only under the runner's export -f surface. Includes (16.0) the REAL
# client/API delete model proven lib-independently against the
# recording stub — name+field-selector refused client-side, UID
# precondition enforced server-side from the DeleteOptions.body
# (GET-then A→B replacement → 409, B fully preserved, no deleted
# record) — and (16.6) the same race driven through the real lib
# helpers under runner dispatch (pinned outcome: refusal rc 2, row
# precondition-conflict, B fully preserved, no deleted record);
# (17) evidence immutability — a populated explicit EVIDENCE_DIR is
# rejected rc=2 before the first mutation; old run-meta/run-identity/
# case.log survive byte-identical; an explicit-but-empty dir is
# honored;
# (16.7) the CLUSTER-scoped Quartet (real CRD spec.scope: Cluster) on the
# raw model — the namespaced raw route is refused as an unknown path
# and the UID body is enforced on the exact cluster route (mismatch
# -> 409, object preserved); and through the REAL cleanup helpers
# under a cluster grant: exact raw path asserted, no namespace ever
# emitted, --namespace misuse refused, GET->A->B race -> 409 ->
# refusal;
# (18) the TRI-STATE scope gate on the actual common.sh + run.sh surface —
# the kind lookup returns known-namespaced / known-cluster / unknown;
# under a namespaced grant, SCC (real OpenShift GVK), an untabled
# cluster CR, and the cluster Quartet are refused rc=2 with ZERO
# dispatch even with a CORRECT -n; manifests carrying cluster or
# unknown kinds (incl. nested List items, real apiVersion/kind) are
# refused; valid Pod/ConfigMap and `exec ... -- <pod-data>` still work.
#
# Method mirrors tests/test-entrypoint-contract.sh: a scratch fixture copies the
# REAL run.sh + lib/ into a same-relative-layout tree, generates STUB cases, and
# records kubectl/oc/go through PATH stubs. Assertions observe what the stubs
# recorded — grepping runner source is not dispatch proof. Recorded invocation
# shape is `<bin> --context <ctx> <args...>` (scenario_kube applies the gate's
# authorized context), so event greps match the args portion.
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"; mkdir -p "$cache"
fx="$(mktemp -d "$cache/quartet-scenario-contract.XXXXXX")"
trap 'fixture_to_trash' EXIT
# Machine rule: no hard delete — the ephemeral fixture is MOVED to trash.
fixture_to_trash() {
  [ -d "$fx" ] || return 0
  mkdir -p "$HOME/.cache/opencode/trash"
  mv "$fx" "$HOME/.cache/opencode/trash/$(date +%Y%m%d-%H%M%S)-scenario-contract-$$" 2>/dev/null \
    || echo "note: fixture trash move failed; left at $fx (hard-delete yok)" >&2
}

ev="$fx/events.log"; ctl="$fx/ctl"
fail=0
assert() { # $1 = message, $2 = shell condition
  if eval "$2"; then echo "OK  $1"; else echo "FAIL $1"; fail=1; fi
}
ctl_reset() { : > "$ctl/kubectl-fail-on"; : > "$ctl/skip"; }

# Recording tool stubs. kubectl/oc also honor $ctl/kubectl-fail-on: an
# invocation whose args contain the recorded substring exits 1 (controlled
# failure for the cleanup/skip aggregation probes). After logging, the stub
# hands off to stub_api.py, a stateful recording API model that is INERT
# unless STUB_API_STORE is set — then it models the REAL client/API delete
# contract over $STUB_API_STORE: create -o json / get -o json / the
# DeleteOptions.body UID precondition via `delete --raw=<path> -f -`
# (server-side 409 on a GET-then A→B replacement) and the client-side
# refusal of name+field-selector deletes — every other probe runs
# byte-identical to the legacy silent rc=0 stub.
setup_recorders() {
  : > "$ev"; mkdir -p "$ctl"; ctl_reset
  cat > "$fx/stub_api.py" <<'PYEOF'
#!/usr/bin/env python3
"""Stateful recording API model for the atomic-cleanup-helper probes.

Activated ONLY when STUB_API_STORE is set (per-probe store dir); otherwise
every call is a silent rc=0 passthrough — the legacy recording stub.
Models the REAL client/API delete contract (CASE-CONTRACT "Gerçek delete
gövdesi", review 2026-11-12) over $STUB_API_STORE/<kind>/<ns>/<name>.json
(ns "_" when absent):
  create -f <file> -o json -> server-assigned uid (stub-uid-*, never
      name-derivable); AlreadyExists rc=1 on a second create.
  get <kind> <name> -o json -> stored object, or "NotFound" rc=1.
  delete <kind> <name> --field-selector=metadata.uid=U -> NEVER accepted:
      refused client-side exactly like real kubectl ("error: name cannot be
      provided when a selector is specified", rc 1, nothing removed) — a
      field selector is not a UID precondition.
  delete --raw=<api-path> -f - -> the DeleteOptions.body is PARSED and
      preconditions.uid enforced SERVER-SIDE: absent -> NotFound rc=1;
      GET-then A->B replacement before the delete (uid mismatch) -> 409
      Conflict rc=1 with B's fields fully preserved and NO deleted record;
      match -> removed objects MOVED to <store>/deleted/ (no hard delete).
      Race arm: <store>/ctl/replace-before-delete carrying a UID swaps the
      object AFTER the caller's get and BEFORE the precondition evaluates.
Anything else passes through rc=0 untouched.
"""
import json, os, random, shutil, sys, time

store = os.environ.get("STUB_API_STORE", "")
if not store:
    sys.exit(0)

args = sys.argv[1:]
if args and args[0] == "--context":
    args = args[2:]  # scenario_kube applies the authorized context itself


def obj_path(kind, ns, name):
    return os.path.join(store, kind.lower(), ns or "_", name + ".json")


def fail(rc, msg):
    sys.stderr.write(msg + "\n")
    sys.exit(rc)


# plural API token -> lowercase kind token (raw delete path resolution).
PLURAL_KIND = {
    "namespaces": "namespace", "nodes": "node",
    "persistentvolumes": "persistentvolume",
    "persistentvolumeclaims": "persistentvolumeclaim",
    "configmaps": "configmap", "secrets": "secret",
    "serviceaccounts": "serviceaccount", "services": "service",
    "pods": "pod", "leases": "lease", "deployments": "deployment",
    "daemonsets": "daemonset", "clusterroles": "clusterrole",
    "csidrivers": "csidriver", "volumeattachments": "volumeattachment",
    "customresourcedefinitions": "customresourcedefinition",
    "quartets": "quartet",
}

# cluster-scoped kind tokens the raw router knows (strict route/scope
# agreement — Quartet is cluster-scoped, bound to the real CRD scope).
CLUSTER_KINDS = {
    "namespace", "node", "persistentvolume", "clusterrole",
    "csidriver", "volumeattachment", "customresourcedefinition",
    "quartet",
}


def raw_target(raw):
    """Resolve a DeleteOptions raw API path to (kind, ns-or-None, name)."""
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
    kind = PLURAL_KIND.get(plural)
    if kind is None:
        return None  # untabled plural: unknown route, like the real API
 # strict scope routing: a namespaced segment on a cluster-scoped kind
 # (or a missing one on a namespaced kind) is an UNKNOWN route -> None.
    if (kind in CLUSTER_KINDS) == (ns is not None):
        return None
    return kind, ns, name


def parse(rest):
    ns, pos, out_json, fname, field_sel = None, [], False, None, None
    i = 0
    while i < len(rest):
        a = rest[i]
        if a in ("-o", "--output"):
            if i + 1 < len(rest) and rest[i + 1] == "json":
                out_json = True
            i += 2
            continue
        if a in ("-f", "--filename"):
            fname = rest[i + 1] if i + 1 < len(rest) else None
            i += 2
            continue
        if a in ("-n", "--namespace"):
            ns = rest[i + 1] if i + 1 < len(rest) else None
            i += 2
            continue
        if a.startswith("--namespace="):
            ns = a.split("=", 1)[1]
            i += 1
            continue
        if a.startswith("--field-selector="):
            field_sel = a.split("=", 1)[1]
            i += 1
            continue
        if a in ("-l", "--selector", "-p", "--patch"):
            i += 2
            continue
        if a.startswith("-"):
            i += 1
            continue
        pos.append(a)
        i += 1
    return ns, pos, out_json, fname, field_sel


verb, rest = (args[0] if args else ""), args[1:]

if verb == "create":
    ns, pos, out_json, fname, _ = parse(rest)
    if not (out_json and fname and not pos):
        sys.exit(0)  # passthrough (legacy stub behavior)
    try:
        with open(fname) as fh:
            doc = json.load(fh)
        kind = doc.get("kind", "")
        md = doc.get("metadata") or {}
        name = md.get("name", "")
        doc_ns = md.get("namespace", "") or ns
    except Exception:
        sys.exit(0)  # unparseable manifest: passthrough rc=0
    if not kind or not name:
        sys.exit(0)
    p = obj_path(kind, doc_ns, name)
    if os.path.exists(p):
        fail(1, 'Error from server (AlreadyExists): %s "%s" already exists' % (kind.lower(), name))
    uid = "stub-uid-%012x-%04x" % (int(time.time() * 1e6) & 0xFFFFFFFFFFFF,
                                   random.randrange(16 ** 4))
    md["uid"] = uid
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w") as fh:
        json.dump(doc, fh, sort_keys=True)
    print(json.dumps(doc, sort_keys=True))
    sys.exit(0)

if verb == "get":
    ns, pos, out_json, _, _ = parse(rest)
    if len(pos) != 2 or not out_json:
        sys.exit(0)  # passthrough (list form / non-json)
    kind, name = pos
    p = obj_path(kind, ns, name)
    if os.path.exists(p):
        with open(p) as fh:
            print(fh.read())
        sys.exit(0)
    fail(1, 'Error from server (NotFound): %s "%s" not found' % (kind.lower(), name))

if verb == "delete":
    ns, pos, _, fname, field_sel = parse(rest)
    raw = None
    for a in rest:
        if a.startswith("--raw="):
            raw = a.split("=", 1)[1]
            break
    if field_sel and len(pos) >= 2:
 # Real kubectl refuses this CLIENT-SIDE, before any API call: a
 # field selector is not a UID precondition (it only pre-filters a
 # list). Nothing is removed.
        fail(1, "error: name cannot be provided when a selector is specified")
    if raw:
        tgt = raw_target(raw)
        if tgt is None:
            fail(1, "Error from server (NotFound): the server could not find the requested resource")
        kind, rns, name = tgt
        p = obj_path(kind, rns, name)
        try:
            if fname == "-":
                body = json.loads(sys.stdin.read())
            elif fname:
                with open(fname) as fh:
                    body = json.load(fh)
            else:
                raise ValueError("no -f body")
            if body.get("kind") != "DeleteOptions":
                raise ValueError("kind")
        except Exception:
            fail(1, "error: the body of a raw delete must be a decodable DeleteOptions object")
        prec = (body.get("preconditions") or {}).get("uid") or ""
        if not os.path.exists(p):
            fail(1, 'Error from server (NotFound): %s "%s" not found' % (kind, name))
        with open(p) as fh:
            obj = json.load(fh)
        cur = (obj.get("metadata") or {}).get("uid", "")
 # race arm: A->B replacement AFTER the caller's get, BEFORE the
 # precondition evaluates (<store>/ctl/replace-before-delete).
        arm = os.path.join(store, "ctl", "replace-before-delete")
        if prec and os.path.exists(arm):
            with open(arm) as fh:
                repl = fh.read().strip()
            if repl and cur != repl:
                obj["metadata"]["uid"] = repl
                obj["metadata"].setdefault("annotations", {})["stub-integration/replaced"] = "yes"
                tmp = p + ".t"
                with open(tmp, "w") as fh:
                    json.dump(obj, fh, sort_keys=True)
                os.replace(tmp, p)
                cur = repl
        if prec and cur != prec:
 # 409 Conflict: the server-side precondition failed — B's fields
 # are fully preserved, nothing removed, no deleted record.
            fail(1, 'Error from server (Conflict): %s "%s" — UID precondition '
                    'failed: the object has been modified' % (kind, name))
        os.makedirs(os.path.join(store, "deleted"), exist_ok=True)
        shutil.move(p, os.path.join(store, "deleted",
                                    "%s-%s-%d.json" % (kind, name, int(time.time() * 1e9))))
        print('%s "%s" deleted' % (kind, name))
        sys.exit(0)
    if len(pos) != 2:
        sys.exit(0)  # passthrough (delete -f / -l ...)
    kind, name = pos
    p = obj_path(kind, ns, name)
    if not os.path.exists(p):
        print("No resources found")
        sys.exit(0)
    os.makedirs(os.path.join(store, "deleted"), exist_ok=True)
    shutil.move(p, os.path.join(store, "deleted",
                                "%s-%s-%d.json" % (kind, name, int(time.time() * 1e9))))
    print('%s "%s" deleted' % (kind.lower(), name))
    sys.exit(0)

sys.exit(0)
PYEOF
  for t in kubectl oc; do
    cat > "$fx/bin/$t" <<EOF
#!/bin/bash
echo "$t \$*" >> "$ev"
failon="\$(cat "$ctl/kubectl-fail-on" 2>/dev/null || true)"
if [ -n "\$failon" ]; then case "\$*" in *"\$failon"*) exit 1;; esac; fi
exec python3 "$fx/stub_api.py" "\$@"
EOF
    chmod +x "$fx/bin/$t"
  done
  printf '%s\n' '#!/bin/bash' "echo \"go \$*\" >> \"$ev\"" 'exit 0' > "$fx/bin/go"
  chmod +x "$fx/bin/go"
}

# Stub case generator: emits the documented scenario_run/scenario_main template
# with fixture paths baked in. $1=name $2=json $3=run-body $4=cleanup-body
# (- for none) $5=top-level extra (fires whenever case code is executed).
write_stub_case() {
  local dir="$fx/tests/scenario/cases/$1"
  mkdir -p "$dir"
  printf '%s\n' "$2" > "$dir/case.json"
  {
    echo '#!/usr/bin/env bash'
    echo 'set -uo pipefail'
    echo 'case_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"'
    echo 'source "$case_root/../../lib/common.sh"'
    echo 'CASE_JSON="$case_root/case.json"'
    [ -n "${5:-}" ] && printf '%s\n' "$5"
    echo 'scenario_run() {'
    echo '  scenario_preflight "$CASE_JSON" || return 2'
    [ -n "$3" ] && printf '%s\n' "$3"
    echo '  return 0'
    echo '}'
    if [ "$4" != "-" ]; then
      echo 'scenario_cleanup() {'
      printf '%s\n' "$4"
      echo '}'
    fi
    echo 'if [ "${BASH_SOURCE[0]}" = "$0" ]; then'
    echo '  scenario_main'
    echo 'fi'
  } > "$dir/case.sh"
  chmod +x "$dir/case.sh"
}

# Linear-body stub generator: mirrors the REAL case protocol (see the sibling
# cases) — resolves helpers via declare -F (the runner's exported live lib
# wins; a direct run without the runner lib has none and must BLOCK), calls
# scenario_preflight itself, reads RUN_ID/SCENARIO_FAKE_STORE/SOURCE_REV/
# CASE_SHA after the gate, defines _linear_cleanup, and exits 0=PASS
# 1=FAIL 2=BLOCKED 3=SKIP 4=FAIL_CLEANUP WITHOUT writing a .status file.
write_linear_case() { # $1=name $2=json $3=body (mutate + define _linear_cleanup)
  local dir="$fx/tests/scenario/cases/$1"
  mkdir -p "$dir"
  printf '%s\n' "$2" > "$dir/case.json"
  cat > "$dir/case.sh" <<EOF
#!/usr/bin/env bash
set -u
CASE_DIR="\$(cd "\$(dirname "\${BASH_SOURCE[0]}")" && pwd)"
RC_PASS=0; RC_FAIL=1; RC_BLOCKED=2; RC_SKIP=3; RC_FAIL_CLEANUP=4
FINAL_RC=\$RC_PASS; CLEANUP_RC=0
if ! declare -F scenario_preflight >/dev/null 2>&1; then
  echo "LINEAR-STUB: no helper surface present (direct invocation without the runner lib) — BLOCKED" >&2
  exit \$RC_BLOCKED
fi
scenario_preflight "\$CASE_DIR/case.json" || exit \$RC_BLOCKED
[[ -n "\${RUN_ID:-}" && -n "\${SCENARIO_FAKE_STORE:-}" && -n "\${SOURCE_REV:-}" && -n "\${CASE_SHA:-}" ]] || exit \$RC_FAIL
$3
if declare -F _linear_cleanup >/dev/null 2>&1; then
  _linear_cleanup || CLEANUP_RC=1
fi
if [[ \$CLEANUP_RC -ne 0 && \$FINAL_RC -eq \$RC_PASS ]]; then
  FINAL_RC=\$RC_FAIL_CLEANUP
fi
exit \$FINAL_RC
EOF
  chmod +x "$dir/case.sh"
}

# Atomic-cleanup probe case generator (section 16): the LINEAR protocol on the
# FULL contracted surface. The case sources NO lib: it probes the four
# contracted names with declare -F, so it runs ONLY when the runner's
# `export -f` delivered the live helpers — direct invocation without the
# runner BLOCKS with zero mutation. $1=name $2=json $3=body (literal $ = the
# case's own runtime; the body must set FINAL_RC and the wrapper exits it).
write_cleanup_case() {
  local dir="$fx/tests/scenario/cases/$1"
  mkdir -p "$dir"
  printf '%s\n' "$2" > "$dir/case.json"
  cat > "$dir/case.sh" <<EOF
#!/usr/bin/env bash
set -u
CASE_DIR="\$(cd "\$(dirname "\${BASH_SOURCE[0]}")" && pwd)"
RC_PASS=0; RC_FAIL=1; RC_BLOCKED=2; RC_SKIP=3; RC_FAIL_CLEANUP=4
FINAL_RC=\$RC_PASS
if ! declare -F scenario_preflight scenario_create_tracked scenario_delete_owned >/dev/null 2>&1; then
  echo "ATOMIC-CLEANUP-PROBE: contracted helper surface absent (direct invocation without the runner lib) — BLOCKED" >&2
  exit \$RC_BLOCKED
fi
scenario_preflight "\$CASE_DIR/case.json" || exit \$RC_BLOCKED
$3
exit \$FINAL_RC
EOF
  chmod +x "$dir/case.sh"
}

# Build the fixture ONCE per contract run: REAL runner + lib copied into a
# same-relative-layout tree, STUB cases generated (never the real cases/ —
# recursion-free).
setup_fixture() {
  mkdir -p "$fx/tests/scenario/lib" "$fx/tests/scenario/cases" "$fx/bin" "$fx/ctl"
  cp "$root/tests/scenario/run.sh" "$fx/tests/scenario/run.sh"
  cp "$root/tests/scenario/lib/common.sh" "$fx/tests/scenario/lib/common.sh"
  setup_recorders

  write_stub_case stub-normal-io \
    '{"name":"stub-normal-io","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L01"}' \
    'scenario_kube apply -n "$TEST_NAMESPACE" -f deploy.yaml || return 1' \
    'scenario_kube delete -n "$TEST_NAMESPACE" -f deploy.yaml || return 1'
  write_stub_case stub-skip \
    '{"name":"stub-skip","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture dependency-recovery fault"}' \
    "[ -s \"$ctl/skip\" ] && return 3" \
    '-'
  write_stub_case stub-cleanup-fail \
    '{"name":"stub-cleanup-fail","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L03"}' \
    'scenario_kube apply -n "$TEST_NAMESPACE" -f cfail.yaml || return 1' \
    'scenario_kube delete -n "$TEST_NAMESPACE" -l cleanupprobe=1 || return 1'
  write_stub_case stub-source-marker \
    '{"name":"stub-source-marker","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture foreign-resource-conflict fault"}' \
    '' \
    '-' \
    "echo \"sourced-case-code\" >> \"$ev\""
  write_stub_case stub-cluster-fault \
    '{"name":"stub-cluster-fault","platform":"kubernetes","profile":"nfs","risk":"cluster-scoped-fault","scope":"cluster-scoped","precondition":"fixture","plan_ref":"fixture L05"}' \
    'scenario_kube create -f fault.yaml || return 1' \
    'scenario_kube delete -f fault.yaml || return 1'
  write_stub_case stub-uninstall \
    '{"name":"stub-uninstall","platform":"kubernetes","profile":"nfs","risk":"uninstall","scope":"cluster-scoped","precondition":"fixture","plan_ref":"fixture L06"}' \
    'scenario_kube delete crd quartet-crd.example.com || return 1' \
    '-'
  write_stub_case stub-context-shift \
    '{"name":"stub-context-shift","platform":"kubernetes","profile":"nfs","risk":"cluster-scoped-fault","scope":"cluster-scoped","precondition":"fixture","plan_ref":"fixture active-volume-disable fault"}' \
    'scenario_kube apply -f shift.yaml || return 1
scenario_kube apply --context ctx-evil -f shift-arg.yaml || true
scenario_kube create --kubeconfig evil-kube.yaml -f shift-arg2.yaml || true
( KUBE_CONTEXT=ctx-evil; scenario_kube apply -f shifted.yaml ) || true
return 1' \
    '-'
 # Lying metadata: declares risk=isolated but its scope says it touches
 # cluster-scoped resources. Classification must follow the EFFECT — the top
 # marker fires the instant case code executes, so any wrong selection leaves
 # evidence even if preflight would then block the body.
  write_stub_case stub-lying-isolated \
    '{"name":"stub-lying-isolated","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"cluster-scoped","precondition":"fixture","plan_ref":"fixture L12"}' \
    '' \
    '-' \
    "echo \"LYING-EXECUTED\" >> \"$ev\""
 # Namespace-binding probe (namespaced scope): healthy by default; with
 # $ctl/ns-violate it attempts a foreign -n and an unconstrained (no -n)
 # call — both must be refused BEFORE dispatch.
  write_stub_case stub-ns-gate \
    '{"name":"stub-ns-gate","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L13"}' \
    "[ -s \"$ctl/ns-violate\" ] && { scenario_kube apply -n foreign-ns -f nsx.yaml || true; scenario_kube get pods -o json || true; return 1; }" \
    '-'
  write_stub_case stub-badmeta-riskscope \
    '{"name":"stub-badmeta-riskscope","platform":"kubernetes","profile":"nfs","risk":"uninstall","scope":"namespaced","precondition":"fixture","plan_ref":"fixture singleton-rejection fault"}' \
    'scenario_kube delete crd quartet-crd.example.com || return 1' \
    '-'
  write_stub_case stub-badmeta-missing \
    '{"name":"stub-badmeta-missing","platform":"kubernetes","profile":"nfs","risk":"uninstall","scope":"cluster-scoped","precondition":"fixture"}' \
    'scenario_kube delete crd quartet-crd.example.com || return 1' \
    '-'
  write_stub_case stub-badmeta-credential \
    '{"name":"stub-badmeta-credential","platform":"kubernetes","profile":"nfs","risk":"uninstall","scope":"cluster-scoped","precondition":"fixture","plan_ref":"fixture L10","api_token":"hunter2"}' \
    'scenario_kube delete crd quartet-crd.example.com || return 1' \
    '-'
 # Linear-body (real protocol) isolated case: mutate, then run its own
 # labeled cleanup via _linear_cleanup.
  write_linear_case stub-linear-io \
    '{"name":"stub-linear-io","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L11"}' \
    'scenario_kube apply -n "$TEST_NAMESPACE" -f linear.yaml || exit $RC_FAIL
_linear_cleanup() { scenario_kube delete -n "$TEST_NAMESPACE" -l linear-cleanup=1 >/dev/null 2>&1; }'

 # The five REAL case.json files (verbatim copies), with marker bodies.
  write_realmeta_case dependency-recovery
  write_realmeta_case singleton-rejection
  write_realmeta_case foreign-resource-conflict
  write_realmeta_case active-volume-delete-protection
  write_realmeta_case active-volume-disable-protection

 # The invalid-metadata stubs live QUARANTINED outside cases/: with metadata
 # fail-closed (P2), any sweep that sees them cannot PASS, so healthy aggregate
 # runs keep them out. Discovery and direct-invocation probes address them
 # explicitly. Direct invocation still works from cases-disabled/ because the
 # case template resolves lib/ from its own two-level root.
  mkdir -p "$fx/tests/scenario/cases-disabled"
  mv "$fx/tests/scenario/cases/stub-badmeta-riskscope" \
     "$fx/tests/scenario/cases/stub-badmeta-missing" \
     "$fx/tests/scenario/cases/stub-badmeta-credential" \
     "$fx/tests/scenario/cases-disabled/"
}

# Real-metadata stub generator (P1-1 bind): copies one of the FIVE real
# case.json files VERBATIM into the fixture and gives it a stub body whose
# FIRST act is to mark $ev — if any selection ever dispatches it under the
# wrong grant, REAL-META-EXECUTED evidence appears regardless of what the
# body's preflight then does. The real case.sh bodies are never executed here.
write_realmeta_case() { # $1 = real case name
  local dir="$fx/tests/scenario/cases/$1"
  mkdir -p "$dir"
  cp "$root/tests/scenario/cases/$1/case.json" "$dir/case.json"
  {
    echo '#!/usr/bin/env bash'
    echo 'set -uo pipefail'
    echo "echo \"REAL-META-EXECUTED $1\" >> \"$ev\""
    echo 'case_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"'
    echo 'source "$case_root/../../lib/common.sh"'
    echo 'CASE_JSON="$case_root/case.json"'
    echo 'scenario_run() {'
    echo '  scenario_preflight "$CASE_JSON" || return 2'
    echo '  return 0'
    echo '}'
    echo 'if [ "${BASH_SOURCE[0]}" = "$0" ]; then'
    echo '  scenario_main'
    echo 'fi'
  } > "$dir/case.sh"
  chmod +x "$dir/case.sh"
}

# Live invocation helper: explicit runtime env defaults (risk-scoped grant for
# isolated by default), overridable per test (later VAR=val wins), recording
# PATH in front.
live() {
  local evdir="$1"; shift
  local ov=()
  while [ "$1" != "--" ]; do ov+=("$1"); shift; done
  shift
  ( cd "$root" && env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a ALLOW_LIVE=allow-isolated \
      EVIDENCE_DIR="$evdir" "${ov[@]}" PATH="$fx/bin:$PATH" \
      bash "$fx/tests/scenario/run.sh" "$@" )
}

setup_fixture

# ---- (1) discovery is side-effect-free (recorders present BEFORE) ----------
# Deliberately invoked with NO live env at all: discovery must not require it.
# The invalid-metadata stubs are restored into cases/ JUST for the discovery
# probe, then quarantined again (a sweep that saw them could never PASS).
mv "$fx/tests/scenario/cases-disabled/stub-badmeta-credential" \
   "$fx/tests/scenario/cases-disabled/stub-badmeta-riskscope" \
   "$fx/tests/scenario/cases/"
list_out="$( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/scenario/run.sh" --list 2>&1 )"; list_rc=$?
mv "$fx/tests/scenario/cases/stub-badmeta-credential" \
   "$fx/tests/scenario/cases/stub-badmeta-riskscope" \
   "$fx/tests/scenario/cases-disabled/"
help_rc=0
( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/scenario/run.sh" --help >/dev/null 2>&1 ) || help_rc=$?
assert "discovery (--list) rc=0 with NO live env" '[ "$list_rc" = 0 ]'
assert "discovery (--help) rc=0" '[ "$help_rc" = 0 ]'
assert "discovery triggers no kubectl/oc/go call" '[ ! -s "$ev" ]'
assert "discovery executes no case code" '[ ! -s "$ev" ]'
assert "--list surfaces stub metadata (name + risk + plan_ref)" \
  'grep -q "stub-normal-io  risk=isolated" <<<"$list_out" && grep -q "plan_ref=fixture L01" <<<"$list_out"'
assert "--list flags invalid metadata without executing it" \
  'grep -q "INVALID  stub-badmeta-credential" <<<"$list_out" && grep -q "INVALID  stub-badmeta-riskscope (risk/scope tutarsız" <<<"$list_out"'
assert "--list marks cluster-scoped risks as excluded from all" \
  'grep -q "stub-cluster-fault.*all'"'"'dan hariç" <<<"$list_out"'
assert "--list classifies the REAL singleton-lifecycle metadata as effect-excluded" \
  'grep -q "dependency-recovery  risk=cluster-scoped-fault effect=cluster-scoped-fault" <<<"$list_out" && grep -q "singleton-rejection  risk=cluster-scoped-fault effect=cluster-scoped-fault" <<<"$list_out" && grep -q "stub-lying-isolated.*etki: cluster-scoped" <<<"$list_out"'

# ---- (2) risk-scoped inclusion under `all` ----------------------------------
all_rc=0; live "$fx/ev-all" -- all >/dev/null 2>&1 || all_rc=$?
assert "all: aggregate PASS (rc=0)" '[ "$all_rc" = 0 ]'
assert "all: isolated case mutated exactly once (apply + delete)" \
  '[ "$(grep -c "apply -n ns-a -f deploy.yaml" "$ev")" = 1 ] && [ "$(grep -c "delete -n ns-a -f deploy.yaml" "$ev")" = 1 ]'
assert "all: cluster-scoped-fault NOT auto-triggered (no evidence subdir either)" \
  '! grep -q "fault.yaml" "$ev" && [ ! -d "$fx/ev-all/stub-cluster-fault" ]'
assert "all: uninstall NOT auto-triggered (no evidence subdir either)" \
  '! grep -q "delete crd" "$ev" && [ ! -d "$fx/ev-all/stub-uninstall" ]'
assert "all: lying risk=isolated+scope=cluster-scoped stub excluded by EFFECT, never executed" \
  '! grep -q "LYING-EXECUTED" "$ev" && [ ! -d "$fx/ev-all/stub-lying-isolated" ]'
all_out="$(live "$fx/ev-all2" -- all 2>&1)"; all2_rc=$?
assert "all: second run also aggregate PASS" '[ "$all2_rc" = 0 ]'
assert "all: runner reports the exclusion" \
  'grep -q "otomatik devreye GİRMEDİ stub-cluster-fault:risk=cluster-scoped-fault" <<<"$all_out"'

# ---- (2b) EFFECT gate bound to the FIVE REAL case.json files ---------------
# Protocol pin 1: `all`/allow-isolated must NEVER select a case that creates or
# mutates the canonical singleton / global operand resources — by EFFECT.
realall_out="$(live "$fx/ev-real" -- all 2>&1)"; realall_rc=$?
assert "all with REAL metadata: aggregate PASS (rc=0)" '[ "$realall_rc" = 0 ]'
assert "REAL metadata: dependency-recovery (singleton lifecycle) never executed under all" \
  '! grep -q "REAL-META-EXECUTED dependency-recovery" "$ev" && [ ! -d "$fx/ev-real/dependency-recovery" ]'
assert "REAL metadata: singleton-rejection (singleton lifecycle) never executed under all" \
  '! grep -q "REAL-META-EXECUTED singleton-rejection" "$ev" && [ ! -d "$fx/ev-real/singleton-rejection" ]'
assert "runner reports the real singleton-lifecycle cases excluded by effect" \
  'grep -q "otomatik devreye GİRMEDİ dependency-recovery:risk=cluster-scoped-fault effect=cluster-scoped-fault" <<<"$realall_out" && grep -q "otomatik devreye GİRMEDİ singleton-rejection:risk=cluster-scoped-fault effect=cluster-scoped-fault" <<<"$realall_out"'
assert "REAL metadata declares both singleton-lifecycle cases by their effect (cluster-scoped-fault)" \
  'grep -Eq "\"risk\"[[:space:]]*:[[:space:]]*\"cluster-scoped-fault\"" "$root/tests/scenario/cases/dependency-recovery/case.json" && grep -Eq "\"risk\"[[:space:]]*:[[:space:]]*\"cluster-scoped-fault\"" "$root/tests/scenario/cases/singleton-rejection/case.json"'
: > "$ev"
lie_rc=0; live "$fx/ev-real1" -- singleton-rejection >/dev/null 2>&1 || lie_rc=$?
assert "real singleton-lifecycle case single-dispatch refused under allow-isolated (rc=2, zero mutation)" \
  '[ "$lie_rc" = 2 ] && [ ! -s "$ev" ] && ! grep -q "REAL-META-EXECUTED singleton-rejection" "$ev"'
: > "$ev"
lieok_rc=0; live "$fx/ev-real2" ALLOW_LIVE=allow-cluster-scoped-fault -- singleton-rejection >/dev/null 2>&1 || lieok_rc=$?
assert "same real case authorized only under its effect grant (rc=0, body ran)" \
  '[ "$lieok_rc" = 0 ] && grep -q "REAL-META-EXECUTED singleton-rejection" "$ev"'
: > "$ev"
lying1_rc=0; live "$fx/ev-lying1" -- stub-lying-isolated >/dev/null 2>&1 || lying1_rc=$?
assert "lying risk=isolated+scope=cluster-scoped metadata refused under allow-isolated (rc=2, zero mutation)" \
  '[ "$lying1_rc" = 2 ] && [ ! -s "$ev" ] && ! grep -q "LYING-EXECUTED" "$ev"'
: > "$ev"
lying2_rc=0; live "$fx/ev-lying2" ALLOW_LIVE=allow-cluster-scoped-fault -- stub-lying-isolated >/dev/null 2>&1 || lying2_rc=$?
assert "lying metadata runs only under the effect grant (classification by effect, not risk)" \
  '[ "$lying2_rc" = 0 ] && grep -q "LYING-EXECUTED" "$ev"'

# ---- (3) explicit risk selection + grant, single dispatch, arg rejection ----
: > "$ev"
risk_rc=0; live "$fx/ev-risk" ALLOW_LIVE=allow-cluster-scoped-fault -- risk:cluster-scoped-fault >/dev/null 2>&1 || risk_rc=$?
assert "risk:cluster-scoped-fault runs exactly that EFFECT tier (fault + shift)" \
  '[ "$(grep -c "create -f fault.yaml" "$ev")" = 1 ] && [ "$(grep -c "apply -f shift.yaml" "$ev")" = 1 ]'
assert "effect tier selects ALL FIVE real-metadata cases by effect (grant covers them)" \
  '[ "$(grep -c "REAL-META-EXECUTED" "$ev")" = 5 ] && grep -q "LYING-EXECUTED" "$ev"'
assert "risk tier run FAILs only via the context-shift case (rc=1)" '[ "$risk_rc" = 1 ]'
assert "risk tier run never drags in isolated/uninstall" \
  '! grep -q "deploy.yaml" "$ev" && ! grep -q "delete crd" "$ev"'
: > "$ev"
urc=0; live "$fx/ev-risk2" ALLOW_LIVE=allow-uninstall -- risk:uninstall >/dev/null 2>&1 || urc=$?
assert "risk:uninstall runs exactly once (rc=0)" \
  '[ "$urc" = 0 ] && [ "$(grep -c "delete crd quartet-crd.example.com" "$ev")" = 1 ]'
: > "$ev"
wrong_grant_rc=0; live "$fx/ev-grant" -- risk:uninstall >/dev/null 2>&1 || wrong_grant_rc=$?
assert "wrong risk grant (allow-isolated for uninstall tier) rejected rc=2, zero mutation" \
  '[ "$wrong_grant_rc" = 2 ] && [ ! -s "$ev" ]'
: > "$ev"
bad_risk_rc=0; live "$fx/ev-risk3" -- risk:bogus >/dev/null 2>&1 || bad_risk_rc=$?
assert "unknown risk selection rejected rc=2, zero mutation" \
  '[ "$bad_risk_rc" = 2 ] && [ ! -s "$ev" ]'
: > "$ev"
one_rc=0; live "$fx/ev-one" -- stub-normal-io >/dev/null 2>&1 || one_rc=$?
assert "single-case dispatch runs exactly that case" \
  '[ "$one_rc" = 0 ] && [ "$(grep -c "apply -n ns-a -f deploy.yaml" "$ev")" = 1 ] && ! grep -q "fault.yaml" "$ev"'
: > "$ev"
unk_rc=0; live "$fx/ev-unk" -- bogus-case >/dev/null 2>&1 || unk_rc=$?
assert "unknown case rejected rc=2, zero mutation" '[ "$unk_rc" = 2 ] && [ ! -s "$ev" ]'
: > "$ev"
extra_rc=0; live "$fx/ev-extra" -- all extra-arg >/dev/null 2>&1 || extra_rc=$?
assert "extra argument rejected rc=2, zero mutation" '[ "$extra_rc" = 2 ] && [ ! -s "$ev" ]'

# ---- (3b) broken sweep metadata is FAIL-CLOSED — ZERO mutation --------------
# Created AFTER every healthy aggregate run above: a sweep containing a
# broken-metadata case is validated WHOLE before the first dispatch — the
# healthy cases in the same selection are NEVER dispatched either (a
# partially executed sweep is an unverifiable run), the verdict is recorded
# in run-meta, and the run exits rc=1.
mkdir -p "$fx/tests/scenario/cases/stub-brokenmeta"
echo '{}' > "$fx/tests/scenario/cases/stub-brokenmeta/case.json"
: > "$fx/tests/scenario/cases/stub-brokenmeta/case.sh"
: > "$ev"
bm_out="$(live "$fx/ev-broken" -- all 2>&1)"; bm_rc=$?
assert "run containing a broken-metadata case does NOT PASS (rc=1, FAIL-CLOSED)" '[ "$bm_rc" = 1 ]'
assert "broken + valid mix: ZERO case mutation — the healthy case was never dispatched" \
  '[ ! -s "$ev" ] && [ ! -d "$fx/ev-broken/stub-normal-io" ] && ! grep -q "case=stub-normal-io" <<<"$bm_out"'
assert "runner reports the unvalidated sweep metadata" \
  'grep -q "metadata FAIL-CLOSED: .stub-brokenmeta. metadata doğrulanamadı" <<<"$bm_out"'
assert "run-meta binds the rejected metadata (audit trail of the zero-dispatch verdict)" \
  'grep -q "^meta_rejected=stub-brokenmeta$" "$fx/ev-broken/run-meta"'
: > "$ev"
bms_rc=0; live "$fx/ev-broken1" -- stub-brokenmeta >/dev/null 2>&1 || bms_rc=$?
assert "single dispatch of the broken-metadata case rejected rc=2, zero mutation" \
  '[ "$bms_rc" = 2 ] && [ ! -s "$ev" ]'
# Quarantine the broken-metadata stub for the remaining healthy sweeps: with
# whole-selection validation, a sweep that sees it never dispatches — later
# sections assert healthy aggregation, so the broken stub stays out.
mkdir -p "$fx/tests/scenario/cases-disabled"
mv "$fx/tests/scenario/cases/stub-brokenmeta" "$fx/tests/scenario/cases-disabled/"

# ---- (4) gate runs even under DIRECT invocation; zero mutation on reject ----
direct_ok_rc=0
( cd "$fx" && env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a ALLOW_LIVE=allow-isolated \
    EVIDENCE_DIR="$fx/ev-direct" PATH="$fx/bin:$PATH" \
    bash tests/scenario/cases/stub-normal-io/case.sh ) >/dev/null 2>&1 || direct_ok_rc=$?
assert "direct invocation with env runs the body through the gate (rc=0)" \
  '[ "$direct_ok_rc" = 0 ] && [ "$(grep -c "apply -n ns-a -f deploy.yaml" "$ev")" = 1 ]'
assert "direct invocation wrote evidence under EVIDENCE_DIR" \
  '[ -s "$fx/ev-direct/stub-normal-io.status" ] && grep -q "final=PASS" "$fx/ev-direct/stub-normal-io.status"'
: > "$ev"
direct_bad_rc=0
( cd "$fx" && env PATH="$fx/bin:$PATH" \
    bash tests/scenario/cases/stub-normal-io/case.sh ) >/dev/null 2>&1 || direct_bad_rc=$?
assert "direct invocation WITHOUT live env rejected rc=2, zero mutation" \
  '[ "$direct_bad_rc" = 2 ] && [ ! -s "$ev" ]'
: > "$ev"
direct_nogrant_rc=0
( cd "$fx" && env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a EVIDENCE_DIR="$fx/ev-dng" \
    PATH="$fx/bin:$PATH" bash tests/scenario/cases/stub-normal-io/case.sh ) >/dev/null 2>&1 || direct_nogrant_rc=$?
assert "direct invocation WITHOUT any grant rejected rc=2, zero mutation" \
  '[ "$direct_nogrant_rc" = 2 ] && [ ! -s "$ev" ]'
: > "$ev"
for badcase in stub-badmeta-riskscope stub-badmeta-missing stub-badmeta-credential; do
  bm_rc=0
  ( cd "$fx" && env KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a ALLOW_LIVE=allow-uninstall \
      EVIDENCE_DIR="$fx/ev-bm" PATH="$fx/bin:$PATH" \
      bash "tests/scenario/cases-disabled/$badcase/case.sh" ) >/dev/null 2>&1 || bm_rc=$?
  assert "invalid metadata case $badcase rejected before mutation (rc=2, no events)" \
    '[ "$bm_rc" = 2 ] && [ ! -s "$ev" ]'
done
: > "$ev"
# Single dispatch of an invalid-metadata case: restore it into cases/ for this
# probe only, then quarantine again.
mv "$fx/tests/scenario/cases-disabled/stub-badmeta-riskscope" "$fx/tests/scenario/cases/"
bmr_rc=0; live "$fx/ev-bmr" ALLOW_LIVE=allow-uninstall -- stub-badmeta-riskscope >/dev/null 2>&1 || bmr_rc=$?
assert "runner dispatch of invalid metadata case BLOCKED rc=2, zero mutation" \
  '[ "$bmr_rc" = 2 ] && [ ! -s "$ev" ]'
mv "$fx/tests/scenario/cases/stub-badmeta-riskscope" "$fx/tests/scenario/cases-disabled/"

# ---- (4b) missing live env / grant rejected by the runner BEFORE dispatch ---
missing_env_cases=(
  "KUBE_CONTEXT|KUBE_BIN=kubectl TEST_NAMESPACE=ns-a ALLOW_LIVE=allow-isolated"
  "TEST_NAMESPACE|KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a ALLOW_LIVE=allow-isolated"
  "ALLOW_LIVE|KUBE_BIN=kubectl KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a"
)
for entry in "${missing_env_cases[@]}"; do
  missing_name="${entry%%|*}"
  envs="${entry#*|}"
  : > "$ev"
  m_rc=0
  ( cd "$root" && env $envs EVIDENCE_DIR="$fx/ev-miss" PATH="$fx/bin:$PATH" \
      bash "$fx/tests/scenario/run.sh" stub-normal-io ) >/dev/null 2>&1 || m_rc=$?
  assert "missing $missing_name rejected rc=2, zero mutation" '[ "$m_rc" = 2 ] && [ ! -s "$ev" ]'
done
: > "$ev"
binbad_rc=0; live "$fx/ev-binbad" KUBE_BIN=microk8s.kubectl -- stub-normal-io >/dev/null 2>&1 || binbad_rc=$?
assert "KUBE_BIN outside kubectl|oc rejected rc=2, zero mutation" \
  '[ "$binbad_rc" = 2 ] && [ ! -s "$ev" ]'

# ---- (5) context mismatch / target-auth argv overrides refused pre-dispatch -
# Protocol pin 2: global options that change context/server/kubeconfig/auth are
# rejected BEFORE any binary dispatch; the authorized namespace is pinned at
# gate time (an env string alone constrains nothing).
: > "$ev"
shift_rc=0; live "$fx/ev-shift" ALLOW_LIVE=allow-cluster-scoped-fault -- stub-context-shift >/dev/null 2>&1 || shift_rc=$?
shift_log="$fx/ev-shift/stub-context-shift/case.log"
assert "context-shift case FAILs (rc=1) — refusal is not a success" '[ "$shift_rc" = 1 ]'
assert "exactly ONE kubectl call reached the (stub) cluster" '[ "$(grep -c "^kubectl" "$ev")" = 1 ]'
assert "env-drifted context never reached kubectl" '! grep -q "shifted.yaml" "$ev"'
assert "argv --context override never reached kubectl (zero events)" '! grep -q "ctx-evil" "$ev"'
assert "argv --kubeconfig override never reached kubectl (zero events)" '! grep -q "evil-kube.yaml" "$ev"'
assert "case log records the target/auth argv refusals BEFORE dispatch" \
  'grep -q -- "--context" "$shift_log" && grep -q -- "--kubeconfig" "$shift_log" && grep -q "dispatch ÖNCESİ reddedildi" "$shift_log" && grep -q "saptı" "$shift_log"'
assert "status file records FAIL with body refusal, cleanup skipped" \
  'grep -q "final=FAIL" "$fx/ev-shift/stub-context-shift/stub-context-shift.status" && grep -q "cleanup=SKIPPED" "$fx/ev-shift/stub-context-shift/stub-context-shift.status"'
# argv threat evidence: when a real kubectl is present, prove with the LOCAL
# config parser (no network — `config view --minify` reads only the file) that
# a trailing --context really would have re-targeted the call; otherwise the
# zero-recorded-events assertions above ARE the argv evidence (no binary saw
# the override).
if real_kb="$(command -v kubectl 2>/dev/null)"; then
  cat > "$fx/kubeconfig.yaml" <<'KC'
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:16443
  name: cluster-a
- cluster:
    server: https://127.0.0.1:26443
  name: cluster-evil
contexts:
- context:
    cluster: cluster-a
    user: user-a
  name: ctx-a
- context:
    cluster: cluster-evil
    user: user-a
  name: ctx-evil
current-context: ctx-a
users:
- name: user-a
  user: {}
KC
  view_evil="$("$real_kb" --kubeconfig "$fx/kubeconfig.yaml" config view --minify --context ctx-evil 2>/dev/null)"
  assert "argv evidence (real kubectl config parser): trailing --context ctx-evil retargets the call" \
    'grep -q "name: ctx-evil" <<<"$view_evil"'
else
  echo "NOTE  real kubectl not on PATH — argv evidence = zero recorded events carrying the override (asserted above)"
fi

# ---- (5b) namespace binding to the authorized scope --------------------------
: > "$ev"
ng_ok_rc=0; live "$fx/ev-nsok" -- stub-ns-gate >/dev/null 2>&1 || ng_ok_rc=$?
assert "namespaced-scope case with gate-bound -n passes (control)" '[ "$ng_ok_rc" = 0 ]'
: > "$ev"
echo 1 > "$ctl/ns-violate"
ng_rc=0; live "$fx/ev-ns" -- stub-ns-gate >/dev/null 2>&1 || ng_rc=$?
: > "$ctl/ns-violate"
assert "namespaced scope: violations surface as FAIL (rc=1), not success" '[ "$ng_rc" = 1 ]'
assert "foreign-ns call refused BEFORE dispatch — zero kubectl events" '! grep -q "nsx.yaml" "$ev" && ! grep -q "foreign-ns" "$ev"'
assert "unconstrained (no -n) call refused — TEST_NAMESPACE string alone bounds nothing" \
  '! grep -q "get pods" "$ev"'
assert "case log records both namespace refusals" \
  'grep -q "yabancı namespace" "$fx/ev-ns/stub-ns-gate/case.log" && grep -q "açık bağlanmadı" "$fx/ev-ns/stub-ns-gate/case.log"'
assert "status records FAIL with cleanup skipped" \
  'grep -q "final=FAIL" "$fx/ev-ns/stub-ns-gate/stub-ns-gate.status" && grep -q "cleanup=SKIPPED" "$fx/ev-ns/stub-ns-gate/stub-ns-gate.status"'

# ---- (6) FAIL_CLEANUP never becomes success ---------------------------------
echo cleanupprobe > "$ctl/kubectl-fail-on"
cf_rc=0; live "$fx/ev-cf" -- stub-cleanup-fail >/dev/null 2>&1 || cf_rc=$?
ctl_reset
assert "failed cleanup surfaces as FAIL to the runner (rc=1)" '[ "$cf_rc" = 1 ]'
assert "final status is FAIL_CLEANUP, not PASS" \
  'grep -q "final=FAIL_CLEANUP" "$fx/ev-cf/stub-cleanup-fail/stub-cleanup-fail.status"'
assert "body PASS and cleanup FAIL are reported SEPARATELY" \
  'grep -q "body=PASS(body_rc=0)" "$fx/ev-cf/stub-cleanup-fail/stub-cleanup-fail.status" && grep -q "cleanup=FAIL(cleanup_rc=1)" "$fx/ev-cf/stub-cleanup-fail/stub-cleanup-fail.status"'
assert "the failing cleanup attempt was recorded" \
  'grep -q "cleanupprobe=1" "$ev"'
cf_ok_rc=0; live "$fx/ev-cf-ok" -- stub-cleanup-fail >/dev/null 2>&1 || cf_ok_rc=$?
assert "same case with healthy cleanup is PASS (rc=0) — control" \
  '[ "$cf_ok_rc" = 0 ] && grep -q "final=PASS" "$fx/ev-cf-ok/stub-cleanup-fail/stub-cleanup-fail.status"'

# ---- (7) run-id + identity + source revision bound to evidence --------------
assert "run-meta binds run-id, revision, grant, target/context/namespace + metadata verdict" \
  'grep -q "^run_id=scenario-" "$fx/ev-all/run-meta" && grep -q "^source_revision=no-rev" "$fx/ev-all/run-meta" && grep -q "^grant=allow-isolated" "$fx/ev-all/run-meta" && grep -q "kube_context=ctx-a test_namespace=ns-a" "$fx/ev-all/run-meta" && grep -q "^meta_rejected=none" "$fx/ev-all/run-meta"'
assert "every recorded cluster call carries run-id + --context identity" \
  '[ "$(grep -c "^run_id=scenario-" "$fx/ev-all/stub-normal-io/kube-calls.log")" = "$(wc -l < "$fx/ev-all/stub-normal-io/kube-calls.log")" ] && grep -q -- "--context ctx-a" "$fx/ev-all/stub-normal-io/kube-calls.log"'
assert "EVIDENCE_DIR override honored: each dispatched case gets its OWN subdir with immutable run identity" \
  '[ -s "$fx/ev-all/stub-normal-io/run-identity" ] && grep -q "^run_id=scenario-" "$fx/ev-all/stub-normal-io/run-identity" && grep -q "^source_revision=no-rev" "$fx/ev-all/stub-normal-io/run-identity" && grep -q "^case=stub-normal-io$" "$fx/ev-all/stub-normal-io/run-identity"'
assert "no shared generic files at the run root (isolation, no collisions)" \
  '[ ! -f "$fx/ev-all/kube-calls.log" ] && [ ! -f "$fx/ev-all/stub-normal-io.status" ] && [ ! -f "$fx/ev-all/stub-normal-io.log" ]'
assert "per-case status + logs land inside the case subdir" \
  '[ -s "$fx/ev-all/stub-normal-io/stub-normal-io.status" ] && [ -s "$fx/ev-all/stub-normal-io/case.log" ]'
assert "per-case status carries source revision + risk/scope identity" \
  'grep -q "^source_revision=no-rev" "$fx/ev-all/stub-normal-io/stub-normal-io.status" && grep -q "^risk=isolated scope=namespaced" "$fx/ev-all/stub-normal-io/stub-normal-io.status"'

# ---- (8) arg/env propagation -------------------------------------------------
: > "$ev"
oc_rc=0; live "$fx/ev-oc" KUBE_BIN=oc -- stub-normal-io >/dev/null 2>&1 || oc_rc=$?
assert "KUBE_BIN=oc propagates: oc called, kubectl never" \
  '[ "$oc_rc" = 0 ] && [ "$(grep -c "oc --context ctx-a apply -n ns-a" "$ev")" = 1 ] && [ "$(grep -c "^kubectl" "$ev")" = 0 ]'
: > "$ev"
ns_rc=0; live "$fx/ev-nsz" TEST_NAMESPACE=ns-z -- stub-normal-io >/dev/null 2>&1 || ns_rc=$?
assert "TEST_NAMESPACE propagates into the case's cluster calls" \
  '[ "$ns_rc" = 0 ] && grep -q -- "apply -n ns-z" "$ev"'
: > "$ev"
nodflt_rc=0
( cd "$root" && env KUBE_CONTEXT=ctx-a TEST_NAMESPACE=ns-a ALLOW_LIVE=allow-isolated \
    EVIDENCE_DIR="$fx/ev-ndef" PATH="$fx/bin:$PATH" \
    bash "$fx/tests/scenario/run.sh" stub-normal-io ) >/dev/null 2>&1 || nodflt_rc=$?
assert "KUBE_BIN defaults to kubectl (documented default, not a target default)" \
  '[ "$nodflt_rc" = 0 ] && [ "$(grep -c "^kubectl" "$ev")" = 2 ]'

# ---- (9) SKIP/BLOCKED/FAIL aggregation ordering ------------------------------
echo 1 > "$ctl/skip"
sk_rc=0; live "$fx/ev-skip" -- stub-skip >/dev/null 2>&1 || sk_rc=$?
ctl_reset
assert "SKIP case surfaces as SKIP (rc=3), not success" \
  '[ "$sk_rc" = 3 ] && grep -q "final=SKIP" "$fx/ev-skip/stub-skip/stub-skip.status"'
echo 1 > "$ctl/skip"; echo deploy.yaml > "$ctl/kubectl-fail-on"
ord_rc=0; live "$fx/ev-ord" -- all >/dev/null 2>&1 || ord_rc=$?
ctl_reset
assert "FAIL outranks SKIP in aggregation (rc=1)" '[ "$ord_rc" = 1 ]'
assert "SKIP still recorded separately inside the failing run" \
  'grep -q "final=SKIP" "$fx/ev-ord/stub-skip/stub-skip.status"'

# ---- (10) real linear-body protocol runs unmodified through the runner ------
: > "$ev"
lin_out="$(live "$fx/ev-linear" -- stub-linear-io 2>&1)"; lin_rc=$?
assert "linear-body case (declare -F finds the exported live lib) passes (rc=0)" \
  '[ "$lin_rc" = 0 ]'
assert "runner reports the linear-body case as final=PASS rc=0" \
  'grep -q "case=stub-linear-io final=PASS rc=0" <<<"$lin_out"'
assert "linear-body case mutated exactly once + ran its own bound cleanup" \
  '[ "$(grep -c "apply -n ns-a -f linear.yaml" "$ev")" = 1 ] && [ "$(grep -c "delete -n ns-a -l linear-cleanup=1" "$ev")" = 1 ]'
assert "runner keys status off the exit code — NO .status file required" \
  '[ ! -f "$fx/ev-linear/stub-linear-io/stub-linear-io.status" ]'
: > "$ev"
lin_direct_rc=0
( cd "$fx" && env PATH="$fx/bin:$PATH" \
    bash tests/scenario/cases/stub-linear-io/case.sh ) >/dev/null 2>&1 || lin_direct_rc=$?
assert "linear-body case WITHOUT the runner lib BLOCKS (rc=2), zero mutation" \
  '[ "$lin_direct_rc" = 2 ] && [ ! -s "$ev" ]'

# ---- (16) shared atomic-cleanup pair (CASE-CONTRACT parent-pin) --------------
# Generated HERE, after every sweep above (adding cases to cases/ earlier would
# change the `all`/`--list` aggregates). The probe cases source NO lib and
# declare -F all four contracted names, so they run ONLY when the runner's
# export -f delivered the live surface; each runs through the runner dispatch
# against its own stateful store (STUB_API_STORE arms stub_api.py, everything
# else passes through silently). CANARY_MANIFEST carries the manifest path
# into the case (env channel — bodies bake no fixture paths).
mkdir -p "$fx/sc"
printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"canary-cm","namespace":"ns-a"},"data":{"payload":"run-owned-canary"}}' \
  > "$fx/sc/canary-cm.json"

# (16.0) the recording API models the REAL client/API delete contract —
# driven DIRECTLY against the stub (no lib in the loop), so this is green
# regardless of the mid-wave state of lib/common.sh: name+field-selector is
# refused client-side like real kubectl; the UID precondition travels in
# DeleteOptions.body via `delete --raw=<api-path> -f -` and is enforced
# server-side (409 on a GET-then A→B replacement, B fully preserved).
: > "$ev"
mkdir -p "$fx/store-p0/ctl"
P0_UID="$(STUB_API_STORE="$fx/store-p0" "$fx/bin/kubectl" --context ctx-a \
  create -f "$fx/sc/canary-cm.json" -o json -n ns-a | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["uid"])')"
assert "raw model: create through the stub returns a server-assigned uid" '[ -n "$P0_UID" ]'
rc=0
STUB_API_STORE="$fx/store-p0" "$fx/bin/kubectl" --context ctx-a \
  delete configmap canary-cm -n ns-a --field-selector=metadata.uid="$P0_UID" \
  >/dev/null 2>"$fx/p0-err" || rc=$?
assert "raw model: name+field-selector delete refused CLIENT-SIDE with kubectl's exact error (rc=1, object survives)" \
  '[ "$rc" = 1 ] && grep -q "name cannot be provided when a selector is specified" "$fx/p0-err" && [ -f "$fx/store-p0/configmap/ns-a/canary-cm.json" ]'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"canary-cm"},"preconditions":{"uid":"stub-uid-never-there"}}' \
  | STUB_API_STORE="$fx/store-p0" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/api/v1/namespaces/ns-a/configmaps/canary-cm -f - \
  >/dev/null 2>"$fx/p0-err" || rc=$?
assert "raw model: DeleteOptions.body with mismatched preconditions.uid → 409 Conflict (rc=1, object preserved)" \
  '[ "$rc" = 1 ] && grep -q "Conflict" "$fx/p0-err" && grep -q "\"uid\": \"$P0_UID\"" "$fx/store-p0/configmap/ns-a/canary-cm.json"'
echo "stub-uid-B" > "$fx/store-p0/ctl/replace-before-delete"
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"canary-cm"},"preconditions":{"uid":"'"$P0_UID"'"}}' \
  | STUB_API_STORE="$fx/store-p0" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/api/v1/namespaces/ns-a/configmaps/canary-cm -f - \
  >/dev/null 2>"$fx/p0-err" || rc=$?
assert "raw model: GET-then A→B replacement before the delete → 409, B fully preserved (payload + marker), NO deleted record" \
  '[ "$rc" = 1 ] && grep -q "Conflict" "$fx/p0-err" && grep -q "\"uid\": \"stub-uid-B\"" "$fx/store-p0/configmap/ns-a/canary-cm.json" && grep -q "\"payload\": \"run-owned-canary\"" "$fx/store-p0/configmap/ns-a/canary-cm.json" && ! ls "$fx/store-p0/deleted/" 2>/dev/null | grep -q canary-cm'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"canary-cm"},"preconditions":{"uid":"stub-uid-B"}}' \
  | STUB_API_STORE="$fx/store-p0" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/api/v1/namespaces/ns-a/configmaps/canary-cm -f - \
  >/dev/null 2>&1 || rc=$?
assert "raw model: matching preconditions.uid deletes (rc=0), and a second attempt is a clean NotFound (rc=1)" \
  '[ "$rc" = "0" ] && ! ls "$fx/store-p0/configmap/ns-a/" | grep -q canary-cm'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"canary-cm"},"preconditions":{"uid":"stub-uid-B"}}' \
  | STUB_API_STORE="$fx/store-p0" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/api/v1/namespaces/ns-a/configmaps/canary-cm -f - \
  >/dev/null 2>"$fx/p0-err" || rc=$?
assert "raw model: DeleteOptions delete of an absent object is a clean NotFound (rc=1)" \
  '[ "$rc" = "1" ] && grep -q "Error from server (NotFound)" "$fx/p0-err"'

# (16.7a) the CLUSTER-scoped Quartet route in the raw model (real CRD
# spec.scope: Cluster): the fixture REFUSES the namespaced route (unknown
# path — the inverse schema), and the UID body is enforced on the exact
# cluster route (mismatch -> 409, object preserved; match -> deleted).
mkdir -p "$fx/store-p0q/ctl"
printf '%s\n' '{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true}}}}' \
  > "$fx/sc/quartet-cluster.json"
Q0_UID="$(STUB_API_STORE="$fx/store-p0q" "$fx/bin/kubectl" --context ctx-a \
  create -f "$fx/sc/quartet-cluster.json" -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["uid"])')"
assert "raw model (cluster Quartet): create through the stub returns a server-assigned uid" \
  '[ -n "$Q0_UID" ] && [[ "$Q0_UID" != "quartet" ]]'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"quartet"},"preconditions":{"uid":"x"}}' \
  | STUB_API_STORE="$fx/store-p0q" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/namespaces/ns-a/quartets/quartet -f - \
  >/dev/null 2>"$fx/p0q-err" || rc=$?
assert "raw model (cluster Quartet): the NAMESPACED raw route is refused by the fixture (unknown route, object preserved)" \
  '[ "$rc" = "1" ] && grep -q "could not find the requested resource" "$fx/p0q-err" && [ -f "$fx/store-p0q/quartet/_/quartet.json" ]'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"quartet"},"preconditions":{"uid":"stub-uid-never-there"}}' \
  | STUB_API_STORE="$fx/store-p0q" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet -f - \
  >/dev/null 2>"$fx/p0q-err" || rc=$?
assert "raw model (cluster Quartet): the EXACT cluster raw route + mismatched UID body -> 409, object preserved" \
  '[ "$rc" = "1" ] && grep -q "Conflict" "$fx/p0q-err" && grep -q "\"uid\": \"$Q0_UID\"" "$fx/store-p0q/quartet/_/quartet.json"'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"quartet"},"preconditions":{"uid":"'"$Q0_UID"'"}}' \
  | STUB_API_STORE="$fx/store-p0q" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet -f - \
  >/dev/null 2>"$fx/p0q-err" || rc=$?
assert "raw model (cluster Quartet): matching UID body on the cluster route deletes (rc=0), second attempt is a clean NotFound" \
  '[ "$rc" = "0" ] && ! [ -f "$fx/store-p0q/quartet/_/quartet.json" ]'
rc=0
printf '%s' '{"kind":"DeleteOptions","apiVersion":"v1","metadata":{"name":"quartet"},"preconditions":{"uid":"'"$Q0_UID"'"}}' \
  | STUB_API_STORE="$fx/store-p0q" "$fx/bin/kubectl" --context ctx-a \
      delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet -f - \
  >/dev/null 2>"$fx/p0q-err" || rc=$?
assert "raw model (cluster Quartet): delete of the now-absent cluster object is a clean NotFound (rc=1)" \
  '[ "$rc" = "1" ] && grep -q "Error from server (NotFound)" "$fx/p0q-err"'

write_cleanup_case atomic-cleanup-registry \
  '{"name":"atomic-cleanup-registry","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L16"}' \
  'CT_UID="$(scenario_create_tracked configmap canary-cm --namespace "$TEST_NAMESPACE" "$CANARY_MANIFEST" 2>"$EVIDENCE_DIR/create.err")" || exit $RC_FAIL
[[ -n "$CT_UID" && "$CT_UID" != "canary-cm" ]] || exit $RC_FAIL
grep -qF -- "$CT_UID" "$EVIDENCE_DIR/ownership.tsv" || exit $RC_FAIL
LIVE_JSON="$(scenario_kube get configmap canary-cm -n "$TEST_NAMESPACE" -o json)" || exit $RC_FAIL
case "$LIVE_JSON" in *"$CT_UID"*) : ;; *) exit $RC_FAIL ;; esac'

# (16.1) create_tracked registers the create-RESPONSE UID — not a name guess
: > "$ev"; mkdir -p "$fx/store-p1"
p1_rc=0; p1_out="$(live "$fx/ev-p1" STUB_API_STORE="$fx/store-p1" CANARY_MANIFEST="$fx/sc/canary-cm.json" -- atomic-cleanup-registry 2>&1)" || p1_rc=$?
assert "create_tracked probe passes under runner dispatch (rc=0 — export -f surface reached the case)" \
  '[ "$p1_rc" = 0 ]'
P1_TSV="$fx/ev-p1/atomic-cleanup-registry/ownership.tsv"
P1_STORE_OBJ="$fx/store-p1/configmap/ns-a/canary-cm.json"
P1_UID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["metadata"]["uid"])' "$P1_STORE_OBJ")"
P1_ROW="$(grep -P '^CREATE\tconfigmap\tns-a\tcanary-cm\t' "$P1_TSV" | head -n1)"
P1_ROW_UID="$(printf '%s' "$P1_ROW" | cut -f5)"
P1_ROW_OUTCOME="$(printf '%s' "$P1_ROW" | cut -f6)"
P1_ROW_RUNID="$(printf '%s' "$P1_ROW" | cut -f7)"
P1_RUNID="$(sed -n 's/^run_id=//p' "$fx/ev-p1/run-meta" | head -n1)"
assert "create_tracked registered the create-RESPONSE uid (server-issued, never a name guess)" \
  '[ -n "$P1_UID" ] && [ "$P1_UID" != "canary-cm" ] && [ "$P1_ROW_UID" = "$P1_UID" ]'
assert "registry carries exactly one CREATE row, outcome=created, bound to the run-id" \
  '[ "$(wc -l < "$P1_TSV")" = 1 ] && [ "$P1_ROW_OUTCOME" = "created" ] && [ "$P1_ROW_RUNID" = "$P1_RUNID" ] && [ -n "$P1_RUNID" ]'
assert "create_tracked created through the single cluster surface (one gate-bound create event)" \
  '[ "$(grep -cF -- "create -f $fx/sc/canary-cm.json -o json -n ns-a" "$ev")" = 1 ]'

write_cleanup_case atomic-cleanup-uid-swap \
  '{"name":"atomic-cleanup-uid-swap","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L17"}' \
  'CT_UID="$(scenario_create_tracked configmap canary-cm --namespace "$TEST_NAMESPACE" "$CANARY_MANIFEST" 2>"$EVIDENCE_DIR/create.err")" || exit $RC_FAIL
[[ -n "$CT_UID" ]] || exit $RC_FAIL
STORE_OBJ="$STUB_API_STORE/configmap/$TEST_NAMESPACE/canary-cm.json"
[[ -f "$STORE_OBJ" ]] || exit $RC_FAIL
python3 -c '"'"'import json,sys,os;p=sys.argv[1];d=json.load(open(p));d["metadata"]["uid"]="stub-uid-swapped-under-case";json.dump(d,open(p+".t","w"),sort_keys=True);os.replace(p+".t",p)'"'"' "$STORE_OBJ" || exit $RC_FAIL
rc=0
scenario_delete_owned configmap canary-cm --namespace "$TEST_NAMESPACE" "$CT_UID" 2>"$EVIDENCE_DIR/delete.err" || rc=$?
[[ $rc -eq 2 ]] || { echo "delete_owned rc=$rc — beklenen 2 (UID uyusmazligi refusal)" >&2; exit $RC_FAIL; }
[[ -f "$STORE_OBJ" ]] || exit $RC_FAIL
grep -q "stub-uid-swapped-under-case" "$STORE_OBJ" || exit $RC_FAIL'

# (16.2) delete_owned refuses to adopt a UID-swapped object (2; survives)
: > "$ev"; mkdir -p "$fx/store-p2"
p2_rc=0; live "$fx/ev-p2" STUB_API_STORE="$fx/store-p2" CANARY_MANIFEST="$fx/sc/canary-cm.json" -- atomic-cleanup-uid-swap >/dev/null 2>&1 || p2_rc=$?
assert "uid-swapped object: the run treats the refusal as the correct outcome (rc=0)" \
  '[ "$p2_rc" = 0 ]'
assert "uid-swapped object: NO delete was dispatched (zero delete events for canary-cm)" \
  '! grep -qF -- "delete configmap canary-cm" "$ev"'
assert "uid-swapped object survives, un-adopted (swapped uid still live in the store)" \
  'grep -q "stub-uid-swapped-under-case" "$fx/store-p2/configmap/ns-a/canary-cm.json"'
P2_TSV="$fx/ev-p2/atomic-cleanup-uid-swap/ownership.tsv"
assert "registry records CREATE + the refusal (outcome=refused-uid-mismatch), exactly two rows" \
  '[ "$(wc -l < "$P2_TSV")" = 2 ] && grep -q "refused-uid-mismatch" "$P2_TSV" && grep -qP "^CREATE\t" "$P2_TSV"'
assert "the refusal reason was recorded (UID mismatch — no adoption, delete untouched)" \
  'grep -q "UID uyumsuzluğu" "$fx/ev-p2/atomic-cleanup-uid-swap/delete.err"'

write_cleanup_case atomic-cleanup-foreign-same-name \
  '{"name":"atomic-cleanup-foreign-same-name","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L18"}' \
  'STORE_OBJ="$STUB_API_STORE/configmap/$TEST_NAMESPACE/canary-cm.json"
[[ -f "$STORE_OBJ" ]] || exit $RC_FAIL
grep -q "external-uid-1" "$STORE_OBJ" || exit $RC_FAIL
rc=0
scenario_delete_owned configmap canary-cm --namespace "$TEST_NAMESPACE" "owned-uid-this-run-believed" 2>"$EVIDENCE_DIR/delete.err" || rc=$?
[[ $rc -eq 2 ]] || { echo "delete_owned rc=$rc — beklenen 2 (yabanci ayni-adli nesne)" >&2; exit $RC_FAIL; }
[[ -f "$STORE_OBJ" ]] || exit $RC_FAIL
grep -q "external-uid-1" "$STORE_OBJ" || exit $RC_FAIL
grep -q "not ours" "$STORE_OBJ" || exit $RC_FAIL'

# (16.3) delete_owned does NOTHING on a foreign same-name object
mkdir -p "$fx/store-p3/configmap/ns-a"
printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"canary-cm","namespace":"ns-a","uid":"external-uid-1"},"data":{"payload":"not ours"}}' \
  > "$fx/store-p3/configmap/ns-a/canary-cm.json"
: > "$ev"
p3_rc=0; live "$fx/ev-p3" STUB_API_STORE="$fx/store-p3" -- atomic-cleanup-foreign-same-name >/dev/null 2>&1 || p3_rc=$?
assert "foreign same-name object: the run treats the refusal as the correct outcome (rc=0)" \
  '[ "$p3_rc" = 0 ]'
assert "foreign same-name object: NO delete dispatched — only the read-only UID check ran" \
  '! grep -qF -- "delete configmap canary-cm" "$ev"'
P3_OBJ="$fx/store-p3/configmap/ns-a/canary-cm.json"
assert "foreign object survives intact (uid + payload untouched)" \
  'grep -q "external-uid-1" "$P3_OBJ" && grep -q "not ours" "$P3_OBJ"'
P3_TSV="$fx/ev-p3/atomic-cleanup-foreign-same-name/ownership.tsv"
assert "registry has NO CREATE row for the foreign object — only the refusal" \
  '! grep -qP "^CREATE" "$P3_TSV" && grep -q "refused-uid-mismatch" "$P3_TSV" && [ "$(wc -l < "$P3_TSV")" = 1 ]'

write_cleanup_case atomic-cleanup-fail-cleanup \
  '{"name":"atomic-cleanup-fail-cleanup","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L19"}' \
  'CT_UID="$(scenario_create_tracked configmap canary-cm --namespace "$TEST_NAMESPACE" "$CANARY_MANIFEST" 2>"$EVIDENCE_DIR/create.err")" || exit $RC_FAIL
[[ -n "$CT_UID" ]] || exit $RC_FAIL
STORE_OBJ="$STUB_API_STORE/configmap/$TEST_NAMESPACE/canary-cm.json"
[[ -f "$STORE_OBJ" ]] || exit $RC_FAIL
python3 -c '"'"'import json,sys,os;p=sys.argv[1];d=json.load(open(p));d["metadata"]["uid"]="stub-uid-swapped-under-case";json.dump(d,open(p+".t","w"),sort_keys=True);os.replace(p+".t",p)'"'"' "$STORE_OBJ" || exit $RC_FAIL
cleanup_rc=0
scenario_delete_owned configmap canary-cm --namespace "$TEST_NAMESPACE" "$CT_UID" 2>"$EVIDENCE_DIR/delete.err" || cleanup_rc=$?
[[ $cleanup_rc -ne 0 ]] || exit $RC_FAIL
if [[ $cleanup_rc -ne 0 && $FINAL_RC -eq $RC_PASS ]]; then FINAL_RC=$RC_FAIL_CLEANUP; fi'

# (16.4) a refused cleanup surfaces as FAIL_CLEANUP(4) — never success
: > "$ev"; mkdir -p "$fx/store-p4"
p4_rc=0; p4_out="$(live "$fx/ev-p4" STUB_API_STORE="$fx/store-p4" CANARY_MANIFEST="$fx/sc/canary-cm.json" -- atomic-cleanup-fail-cleanup 2>&1)" || p4_rc=$?
assert "refused cleanup surfaces as FAIL_CLEANUP(4) for the case, not success" \
  'grep -q "case=atomic-cleanup-fail-cleanup final=FAIL_CLEANUP rc=4" <<<"$p4_out"'
assert "runner aggregate is FAIL (rc=1) for the FAIL_CLEANUP run" '[ "$p4_rc" = 1 ]'
assert "the swapped object survives the refused cleanup" \
  'grep -q "stub-uid-swapped-under-case" "$fx/store-p4/configmap/ns-a/canary-cm.json"'
assert "FAIL_CLEANUP keyed off the exit code — NO .status file required" \
  '[ ! -f "$fx/ev-p4/atomic-cleanup-fail-cleanup/atomic-cleanup-fail-cleanup.status" ]'

# (16.5) the probe cases are export -f proof: without the runner's surface they
# BLOCK with zero mutation (they source no lib of their own).
: > "$ev"
dcb_rc=0
( cd "$fx" && env PATH="$fx/bin:$PATH" bash tests/scenario/cases/atomic-cleanup-registry/case.sh ) >/dev/null 2>&1 || dcb_rc=$?
assert "cleanup probe case WITHOUT the runner's exported surface BLOCKS (rc=2), zero mutation" \
  '[ "$dcb_rc" = 2 ] && [ ! -s "$ev" ]'

# (16.6) GET-then A→B replacement before the delete, driven through the REAL
# lib helpers under runner dispatch: the pinned outcome is state-invariant —
# the run NEVER PASSes over the replaced object, the server answers 409, the
# helper returns rc 2 (refusal, never success), B's fields are fully
# preserved, and the registry carries the precondition-conflict row — never
# a deleted/absent record. The server-side 409 semantics themselves are
# proven lib-independently in (16.0).
write_cleanup_case atomic-cleanup-race-conflict \
  '{"name":"atomic-cleanup-race-conflict","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L20"}' \
  'CT_UID="$(scenario_create_tracked configmap canary-cm --namespace "$TEST_NAMESPACE" "$CANARY_MANIFEST" 2>"$EVIDENCE_DIR/create.err")" || exit $RC_FAIL
[[ -n "$CT_UID" ]] || exit $RC_FAIL
mkdir -p "$STUB_API_STORE/ctl"
echo stub-uid-replaced-B > "$STUB_API_STORE/ctl/replace-before-delete"
rc=0
scenario_delete_owned configmap canary-cm --namespace "$TEST_NAMESPACE" "$CT_UID" 2>"$EVIDENCE_DIR/delete.err" || rc=$?
[[ $rc -eq 2 ]] || { echo "delete_owned rc=$rc — beklenen 2 (server 409 = precondition-conflict refusal, success asla değil)" >&2; exit $RC_FAIL; }
[[ -f "$STUB_API_STORE/configmap/$TEST_NAMESPACE/canary-cm.json" ]] || exit $RC_FAIL
grep -q "\"payload\": \"run-owned-canary\"" "$STUB_API_STORE/configmap/$TEST_NAMESPACE/canary-cm.json" || exit $RC_FAIL
grep -qE "precondition-conflict" "$EVIDENCE_DIR/ownership.tsv" | grep -q canary-cm || exit $RC_FAIL
grep -qF -- "$(printf "DELETE\tconfigmap\tns-a\tcanary-cm\t%s\tdeleted" "$CT_UID")" "$EVIDENCE_DIR/ownership.tsv" && exit $RC_FAIL'

: > "$ev"; mkdir -p "$fx/store-p6"
p6_rc=0; p6_out="$(live "$fx/ev-p6" STUB_API_STORE="$fx/store-p6" CANARY_MANIFEST="$fx/sc/canary-cm.json" -- atomic-cleanup-race-conflict 2>&1)" || p6_rc=$?
assert "race probe: the run never PASSes over a GET→A→B-replaced object (rc=$p6_rc)" \
  '[ "$p6_rc" != "0" ] && ! grep -q "final=PASS rc=0" <<<"$p6_out"'
assert "race probe: the replaced object survives with ALL its fields preserved (payload + replacement uid intact)" \
  '[ -f "$fx/store-p6/configmap/ns-a/canary-cm.json" ] && grep -q "\"payload\": \"run-owned-canary\"" "$fx/store-p6/configmap/ns-a/canary-cm.json" && grep -q "stub-uid-replaced-B" "$fx/store-p6/configmap/ns-a/canary-cm.json"'
P6_TSV="$fx/ev-p6/atomic-cleanup-race-conflict/ownership.tsv"
assert "race probe: NO deleted/PASS record — registry carries exactly the CREATE row + the precondition-conflict refusal row" \
  '[ "$(grep -c "canary-cm" "$P6_TSV")" = "2" ] && grep -q "precondition-conflict" "$P6_TSV" && ! grep "canary-cm" "$P6_TSV" | grep -qE "deleted|absent"'

# (16.7b) cluster grant: the Quartet DELETE's exact raw path + UID body,
# driven through the REAL cleanup helpers under runner dispatch — the helper
# must NOT emit a namespace (registry ns column "-", no -n argv), must build
# the exact cluster route, and must have the UID precondition enforced
# server-side (GET->A->B race on the cluster route -> 409 -> refusal).
write_cleanup_case atomic-cleanup-quartet-cluster \
  '{"name":"atomic-cleanup-quartet-cluster","platform":"kubernetes","profile":"nfs","risk":"cluster-scoped-fault","scope":"cluster-scoped","precondition":"fixture","plan_ref":"fixture L22"}' \
  'Q_MANIFEST="$QUARTET_MANIFEST"
[[ -n "$Q_MANIFEST" && -f "$Q_MANIFEST" ]] || exit $RC_FAIL
q_uid="$(scenario_create_tracked quartet quartet "$Q_MANIFEST" 2>"$EVIDENCE_DIR/create-q.err")" || exit $RC_FAIL
[[ -n "$q_uid" && "$q_uid" != "quartet" ]] || exit $RC_FAIL
grep -qF "$(printf "CREATE\tquartet\t-\tquartet\t%s\tcreated" "$q_uid")" "$EVIDENCE_DIR/ownership.tsv" || exit $RC_FAIL
# --namespace on the cluster kind is a misuse, refused BEFORE dispatch:
rc=0; scenario_delete_owned quartet quartet --namespace "$TEST_NAMESPACE" "$q_uid" >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || exit $RC_FAIL
# GET->A->B race on the cluster route: the 409 proves the UID body traveled
mkdir -p "$STUB_API_STORE/ctl"
echo stub-uid-q-B > "$STUB_API_STORE/ctl/replace-before-delete"
rc=0; scenario_delete_owned quartet quartet "$q_uid" >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || exit $RC_FAIL
grep -q "stub-uid-q-B" "$STUB_API_STORE/quartet/_/quartet.json" || exit $RC_FAIL
grep -qF "$(printf "DELETE\tquartet\t-\tquartet\t%s\tprecondition-conflict" "$q_uid")" "$EVIDENCE_DIR/ownership.tsv" || exit $RC_FAIL
# the clean delete on the cluster route (the live uid is now the replacement)
rc=0; scenario_delete_owned quartet quartet stub-uid-q-B >/dev/null 2>&1 || rc=$?
[[ $rc -eq 0 ]] || exit $RC_FAIL
grep -qF "$(printf "DELETE\tquartet\t-\tquartet\tstub-uid-q-B\tdeleted")" "$EVIDENCE_DIR/ownership.tsv" || exit $RC_FAIL
exit $RC_PASS'

: > "$ev"; mkdir -p "$fx/store-p7"
p7_rc=0; live "$fx/ev-p7" ALLOW_LIVE=allow-cluster-scoped-fault STUB_API_STORE="$fx/store-p7" \
  QUARTET_MANIFEST="$fx/sc/quartet-cluster.json" -- atomic-cleanup-quartet-cluster >/dev/null 2>&1 || p7_rc=$?
assert "cluster grant: Quartet create+delete driven through the REAL cleanup helpers under runner dispatch (rc=0)" \
  '[ "$p7_rc" = "0" ]'
assert "cluster grant: the Quartet DELETE used the EXACT cluster raw path — no namespace segment anywhere" \
  'grep -qF -- "delete --raw=/apis/quartet.ngxstorage.com/v1alpha1/quartets/quartet -f -" "$ev"'
assert "cluster grant: a namespaced raw Quartet path was never emitted by the helper" \
  '! grep -q "namespaces/ns-a/quartets" "$ev"'
q_n_hit="$(grep -E "get quartet |delete quartet " "$ev" | grep -c " -n " || true)"
assert "cluster grant: no -n argv ever attached to the cluster Quartet calls" \
  '[ "$q_n_hit" = "0" ]'
P7_TSV="$fx/ev-p7/atomic-cleanup-quartet-cluster/ownership.tsv"
assert "cluster grant: the race on the cluster route was a REFUSAL (registry: precondition-conflict for the captured uid; exactly the two DELETE rows — refusal + clean delete of the replacement; namespace column '-' throughout)" \
  '[ "$(grep -cP "^DELETE\tquartet\t-\tquartet\t" "$P7_TSV")" = "2" ] && grep -qP "\tprecondition-conflict\t" "$P7_TSV" && grep -qP "^CREATE\tquartet\t-\tquartet\t[^\t]+\tcreated\t" "$P7_TSV" && grep -qP "^DELETE\tquartet\t-\tquartet\tstub-uid-q-B\tdeleted\t" "$P7_TSV"'

# ---- (17) evidence immutability: a populated explicit EVIDENCE_DIR is
# rejected BEFORE the first mutation — old run-meta/run-identity/case.log are
# never overwritten, old evidence survives byte-identical; an explicit-but-
# empty dir is still honored.
mkdir -p "$fx/ev-reuse/stub-normal-io"
echo "run_id=scenario-earlier-run" > "$fx/ev-reuse/run-meta"
echo "run_id=scenario-earlier-run" > "$fx/ev-reuse/stub-normal-io/run-identity"
echo "older run's case log — must survive" > "$fx/ev-reuse/stub-normal-io/case.log"
REUSE_SHA="$( (sha256sum "$fx/ev-reuse/run-meta" "$fx/ev-reuse/stub-normal-io/run-identity" "$fx/ev-reuse/stub-normal-io/case.log") | sha256sum | cut -d' ' -f1 )"
: > "$ev"
reuse_rc=0; live "$fx/ev-reuse" -- all >/dev/null 2>&1 || reuse_rc=$?
assert "populated explicit EVIDENCE_DIR rejected rc=2, ZERO mutation (before the first dispatch)" \
  '[ "$reuse_rc" = 2 ] && [ ! -s "$ev" ]'
assert "old evidence preserved byte-identical (run-meta/run-identity/case.log never overwritten)" \
  '[ "$( (sha256sum "$fx/ev-reuse/run-meta" "$fx/ev-reuse/stub-normal-io/run-identity" "$fx/ev-reuse/stub-normal-io/case.log") | sha256sum | cut -d" " -f1 )" = "$REUSE_SHA" ]'
assert "refused run never created a new case subdir in the reused evidence dir" \
  '! ls "$fx/ev-reuse/" | grep -q "^stub-skip$\|^stub-uninstall$"'
# restore/control: an explicit-but-empty dir is honored and the run PASSes.
: > "$ev"
mkdir -p "$fx/ev-empty"
emp_rc=0; live "$fx/ev-empty" -- stub-normal-io >/dev/null 2>&1 || emp_rc=$?
assert "explicit-but-EMPTY EVIDENCE_DIR honored: run PASSes (rc=0) into the fresh dir" \
  '[ "$emp_rc" = 0 ] && [ "$(grep -c "apply -n ns-a -f deploy.yaml" "$ev")" = 1 ] && [ -s "$fx/ev-empty/run-meta" ]'

# ---- (18) tri-state scope gate on the ACTUAL common.sh + run.sh surface ----
# The kind lookup is known-namespaced / known-cluster / UNKNOWN: under a
# namespaced grant an UNKNOWN kind is refused exactly like a known cluster
# kind (rc 2, ZERO dispatch) — a correct -n does not make an untabled kind
# namespaced. SCC carries its real OpenShift GVK (security.openshift.io/v1
# SecurityContextConstraints, cluster-scoped); the general gap is closed, not
# just SCC added. Manifest validation covers real apiVersion/kind/scope and
# nested List objects. Generated here, after every sweep above.
printf '%s\n' '{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true}}}}' > "$fx/m-q.json"
printf '%s\n' '{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w1","namespace":"ns-a"}}' > "$fx/m-w.json"
printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"quartet.ngxstorage.com/v1alpha1","kind":"Quartet","metadata":{"name":"quartet"},"spec":{"platform":"auto","profiles":{"nfs":{"enabled":true}}}}]}' > "$fx/m-list-q.json"
printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1","namespace":"ns-a"},"data":{"k":"v"}}]}' > "$fx/m-list-cm.json"

# The body is single-quoted (case-side runtime only): manifest paths and the
# ctl dir arrive through the ENV channel (live ov pairs), the established
# pattern — no fixture paths are baked into the generated case.
write_stub_case stub-scope-gate \
  '{"name":"stub-scope-gate","platform":"kubernetes","profile":"nfs","risk":"isolated","scope":"namespaced","precondition":"fixture","plan_ref":"fixture L23"}' \
  '[ -s "$CTL_DIR/scope-violate" ] || return 0
r=0; scenario_kube delete scc review-only -n ns-a || r=$?
[[ $r == 2 ]] || { echo "expected SCC refusal rc=2, got rc=$r"; return 1; }
r=0; scenario_kube delete unknowncr my-object -n ns-a || r=$?
[[ $r == 2 ]] || { echo "expected unknown-kind refusal rc=2, got rc=$r"; return 1; }
r=0; scenario_kube delete quartets quartet -n ns-a || r=$?
[[ $r == 2 ]] || { echo "expected cluster-Quartet refusal rc=2, got rc=$r"; return 1; }
r=0; scenario_kube create -n ns-a -f "$MQ_MANIFEST" || r=$?
[[ $r == 2 ]] || { echo "expected quartet-manifest refusal rc=2, got rc=$r"; return 1; }
r=0; scenario_kube create -n ns-a -f "$MW_MANIFEST" || r=$?
[[ $r == 2 ]] || { echo "expected unknown-manifest-kind refusal rc=2, got rc=$r"; return 1; }
r=0; scenario_kube create -n ns-a -f "$MLQ_MANIFEST" || r=$?
[[ $r == 2 ]] || { echo "expected nested-List(quartet)-manifest refusal rc=2, got rc=$r"; return 1; }
r=0; scenario_kube create -n ns-a -f "$MLMC_MANIFEST" || r=$?
[[ $r != 2 ]] || { echo "nested-List(configmap)-manifest must PASS the scope scan"; return 1; }
r=0; scenario_kube get configmap cm1 -n ns-a || r=$?
[[ $r != 2 ]] || { echo "in-scope ConfigMap must dispatch"; return 1; }
r=0; scenario_kube get pods -n ns-a || r=$?
[[ $r != 2 ]] || { echo "in-scope Pod list must dispatch"; return 1; }
r=0; scenario_kube exec probe-pod -n ns-a -- sh -c "echo payload-data --context evil -n foreign-ns" || r=$?
[[ $r != 2 ]] || { echo "exec ... -- pod args must dispatch"; return 1; }
return 0' \
  '-'

SG_OV="CTL_DIR=$ctl MQ_MANIFEST=$fx/m-q.json MW_MANIFEST=$fx/m-w.json MLQ_MANIFEST=$fx/m-list-q.json MLMC_MANIFEST=$fx/m-list-cm.json"

# control: unarmed the case is healthy and dispatches nothing
: > "$ev"
sg_ok_rc=0; live "$fx/ev-sg-ok" $SG_OV -- stub-scope-gate >/dev/null 2>&1 || sg_ok_rc=$?
assert "scope-gate control: unarmed run PASSes with zero dispatch (rc=0)" \
  '[ "$sg_ok_rc" = "0" ] && [ ! -s "$ev" ]'
# armed: every expectation must hold — the run PASSes only if the gate
# refuses exactly the right calls and dispatches exactly the right controls
: > "$ev"
echo 1 > "$ctl/scope-violate"
sg_rc=0; sg_out="$(live "$fx/ev-sg" $SG_OV -- stub-scope-gate 2>&1)" || sg_rc=$?
: > "$ctl/scope-violate"
assert "scope-gate armed: the run PASSes only when every refusal + control behaved (rc=0)" \
  '[ "$sg_rc" = "0" ]'
assert "scope-gate: SCC under a namespaced grant with the CORRECT -n is refused (rc 2 / ZERO dispatch — the recorded reproduction is closed)" \
  '! grep -q "delete scc review-only -n ns-a" "$ev"'
assert "scope-gate: an UNKNOWN kind (untabled cluster CR) under a namespaced grant is refused (rc 2 / ZERO dispatch)" \
  '! grep -q "delete unknowncr my-object -n ns-a" "$ev"'
assert "scope-gate: the cluster Quartet under a namespaced grant is refused (rc 2 / ZERO dispatch)" \
  '! grep -q "delete quartets quartet -n ns-a" "$ev"'
assert "scope-gate: a manifest carrying the cluster Quartet (real apiVersion/kind) is refused (ZERO dispatch)" \
  '! grep -qF -- "create -n ns-a -f $fx/m-q.json" "$ev"'
assert "scope-gate: a manifest carrying an UNKNOWN kind is refused (ZERO dispatch — the general gap, not just SCC)" \
  '! grep -qF -- "create -n ns-a -f $fx/m-w.json" "$ev"'
assert "scope-gate: a nested List manifest with a cluster Quartet item is refused (ZERO dispatch)" \
  '! grep -qF -- "create -n ns-a -f $fx/m-list-q.json" "$ev"'
assert "scope-gate: a nested List manifest of only namespaced items PASSES the scan (dispatched to the binary)" \
  'grep -qF -- "create -n ns-a -f $fx/m-list-cm.json" "$ev"'
assert "scope-gate: valid ConfigMap and Pod calls still dispatch (the tri-state gate does not over-refuse known namespaced kinds)" \
  'grep -q "get configmap cm1 -n ns-a" "$ev" && grep -q "get pods -n ns-a" "$ev"'
assert "scope-gate: exec ... -- pod args reach the binary untouched (not mangled, not namespace-checked)" \
  'grep -q -- "exec probe-pod -n ns-a --" "$ev" && grep -q -- "-c echo payload-data --context evil -n foreign-ns" "$ev"'
assert "scope-gate: exactly the four control dispatches reached the (stub) cluster — the six refusals left zero events" \
  '[ "$(grep -c "^kubectl" "$ev")" = "4" ]'

if [ "$fail" -ne 0 ]; then
  echo "scenario runner contract FAIL" >&2
  exit 1
fi
echo "tests/scenario/test-runner-contract.sh PASS (discovery side-effect-free; risk-scoped all + grants; EFFECT-based selection on the five REAL case.json files; gate on direct invocation; context mismatch + target/auth argv overrides refused pre-dispatch; namespace binding; broken sweep metadata fail-closed with ZERO mutation (whole selection validated before first dispatch); FAIL_CLEANUP separate; per-case evidence isolation with run identity; populated explicit EVIDENCE_DIR rejected pre-mutation with old evidence byte-identical; env propagation; aggregation order; linear-body protocol unmodified; atomic cleanup: response-uid registry, UID-swap refusal, foreign same-name untouched, refused cleanup = FAIL_CLEANUP(4); real client/API delete model: name+field-selector refused client-side, DeleteOptions.body UID precondition enforced server-side — 409 on GET-then A→B replacement with B fully preserved and no deleted record, lib-independently and through the real lib helpers; cluster Quartet: namespaced raw route refused, exact cluster raw path + UID body through the real helpers under a cluster grant; tri-state scope gate: SCC/unknown-kind/cluster-Quartet refused rc=2 ZERO dispatch under a namespaced grant, manifests with cluster/unknown kinds incl. nested Lists refused, valid Pod/ConfigMap and exec -- intact)"
