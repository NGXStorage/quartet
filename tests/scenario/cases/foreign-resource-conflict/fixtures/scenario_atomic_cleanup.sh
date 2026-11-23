#!/usr/bin/env bash
# ============================================================================
# scenario_atomic_cleanup.sh — OFFLINE TEST STUB of the pinned atomic cleanup
# helpers (tests/scenario/CASE-CONTRACT.md, "Ortak atomik cleanup helper
# (parent-pin)").
#
# Ownership: the REAL implementation lives in tests/scenario/lib/common.sh
# (the risk-gate worker, single owner). The case body probes the loaded helper
# surface AFTER its scenario_preflight probe: when the runner's export -f lib
# already defines scenario_create_tracked, the real lib wins and this stub is
# never sourced. This stub mirrors the real implementation's CONTRACT exactly
# — argv shape, return codes, stdout and the ownership.tsv row format — so a
# case body behaves identically against either surface:
#
# scenario_create_tracked <kind> <name> [--namespace <ns>] <manifest>
# `create -f <manifest> -o json [-n <ns>]`; rc 0 = created, and the
# create-response metadata.uid is emitted on STDOUT (captured with
# `uid="$(scenario_create_tracked ...)"`). rc 1 = API error. rc 2 =
# refusal (gate not run, invocation/namespace misuse, unreadable
# manifest, or a create response with no verifiable uid — a name guess is
# NEVER registered).
# scenario_delete_owned <kind> <name> [--namespace <ns>] <uid>
# get → NotFound: rc 0 (already absent — cleanup goal met). live uid !=
# <uid>: SAHİPLENME YOK, no delete dispatched, rc 2. On match:
# **the real DeleteOptions body** — `delete --raw=<api-path> -f -` with
# stdin `{"kind":"DeleteOptions","apiVersion":"v1",
# "metadata":{"name":<name>},"preconditions":{"uid":<uid>}}` — the UID
# precondition travels server-side in DeleteOptions.body (CASE-CONTRACT
# "Gerçek delete gövdesi", ). The
# `delete <kind> <name> --field-selector=metadata.uid=...` form is NEVER
# issued: real kubectl rejects it client-side ("name cannot be provided
# when a selector is specified") and a field selector is not a
# precondition (it only pre-filters a list). The raw path is built from
# this stub's OWN controlled GVK table (same table as the lib) — untabled
# kinds fail closed, unbounded `--raw` passthrough is never opened. A
# GET-then A→B replacement before the delete is answered by the API with
# 409 Conflict: the object (B) is fully preserved, the deletion is never
# claimed → rc 2 REFUSAL (row: precondition-conflict), never a success.
# Postcondition get must be a CLEAN NotFound (API reason token) → rc 0
# (row: deleted); object still present → rc 2 (row:
# postcondition-failed); any other read error → rc 1 (API error).
# Emits nothing on stdout.
#
# $EVIDENCE_DIR/ownership.tsv — tab-separated EVIDENCE log, one row per
# helper call: action kind namespace name uid outcome run_id created_utc
# (namespace "-" for cluster-scoped kinds; CREATE rows carry the create-
# response uid with outcome created|api-error|no-uid-in-response; DELETE
# rows carry the EXPECTED uid with outcome deleted|absent|
# refused-uid-mismatch|precondition-conflict|postcondition-failed|api-
# error). The registry is
# evidence, not a gate: the refusal to delete comes from the live UID
# comparison, never from registry membership.
#
# refusal-vs-API-error discipline (CASE-CONTRACT final paragraph): scenario_
# kube returns 2 on every refusal path; this stub propagates the split — rc 2
# is a REFUSAL and never a success; rc 1 is an API error.
#
# Real API reason classification (CASE-CONTRACT "Gerçek API reason
# sınıflaması"): an "absent" decision is only ever rc 0 on a CLEAN NotFound —
# the API's status reason token `Error from server (NotFound)`. Seeing the
# substring "NotFound" anywhere in a message is NOT absence (a Forbidden
# response such as `User "NotFoundActor" ...` is an API error, rc 1);
# Forbidden / Conflict / timeout / malformed responses never count as
# successful cleanup.
#
# Namespace binding mirrors the lib: a tabled cluster-scoped kind NEVER gets
# -n; every other kind is namespaced and REQUIRES an explicit --namespace
# (the offline cases run under a cluster-scoped-scope gate, so a missing
# namespace is a refusal, rc 2 — never a silent current-namespace default).
#
# No variable from this stub is read by a case body (protocol pin 3) except
# through the two pinned entry points. The gate proof offline is a bound
# EVIDENCE_DIR (the offline preflight exports it exactly like the real lib).
# ============================================================================
set -u

_atomic_gate() {
  if [[ -z "${EVIDENCE_DIR:-}" ]]; then
    echo "scenario_atomic rejected: scenario_preflight gate koşulsuz (EVIDENCE_DIR unbound — mutation reddedildi)" >&2
    return 2
  fi
  return 0
}

# Controlled GVK contract (CASE-CONTRACT protocol pin 2) — SAME table as the
# lib: one static kind table serving the cluster-scope check (mode 0), the
# tri-state scope print (mode "scope": namespaced|cluster, rc 1 = unknown)
# and the DeleteOptions raw-path construction (mode "gvk"). Input is
# normalized like the lib: lowercased, apiVersion/kind prefixes and
# kind.group suffixes stripped. An UNTABLED kind is fail-closed (rc 1) —
# sınırsız `--raw` geçişi AÇILMAZ.
_atomic_kind_is_cluster_scoped() { # <kind> [gvk]
  local mode="${2:-}" info=""
  local tok
  tok="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
  tok="${tok##*/}"
  tok="${tok%%.*}"
  case "$tok" in
    namespace|namespaces|ns)                        info='|v1|namespaces|cluster' ;;
    persistentvolume|persistentvolumes|pv)          info='|v1|persistentvolumes|cluster' ;;
    node|nodes|no)                                  info='|v1|nodes|cluster' ;;
    persistentvolumeclaim|persistentvolumeclaims|pvc) info='|v1|persistentvolumeclaims|namespaced' ;;
    pod|pods|po)                                    info='|v1|pods|namespaced' ;;
    service|services|svc)                           info='|v1|services|namespaced' ;;
    secret|secrets)                                 info='|v1|secrets|namespaced' ;;
    configmap|configmaps|cm)                        info='|v1|configmaps|namespaced' ;;
    serviceaccount|serviceaccounts|sa)              info='|v1|serviceaccounts|namespaced' ;;
    event|events)                                   info='|v1|events|namespaced' ;;
    replicationcontroller|replicationcontrollers|rc) info='|v1|replicationcontrollers|namespaced' ;;
    deployment|deployments|deploy)                  info='apps|v1|deployments|namespaced' ;;
    daemonset|daemonsets|ds)                        info='apps|v1|daemonsets|namespaced' ;;
    statefulset|statefulsets|sts)                   info='apps|v1|statefulsets|namespaced' ;;
    replicaset|replicasets|rs)                      info='apps|v1|replicasets|namespaced' ;;
    job|jobs)                                       info='batch|v1|jobs|namespaced' ;;
    cronjob|cronjobs)                               info='batch|v1|cronjobs|namespaced' ;;
    csidriver|csidrivers)                           info='storage.k8s.io|v1|csidrivers|cluster' ;;
    volumeattachment|volumeattachments|va)          info='storage.k8s.io|v1|volumeattachments|cluster' ;;
    storageclass|storageclasses|sc)                 info='storage.k8s.io|v1|storageclasses|cluster' ;;
    role|roles)                                     info='rbac.authorization.k8s.io|v1|roles|namespaced' ;;
    rolebinding|rolebindings|rb)                    info='rbac.authorization.k8s.io|v1|rolebindings|namespaced' ;;
    clusterrole|clusterroles)                       info='rbac.authorization.k8s.io|v1|clusterroles|cluster' ;;
    clusterrolebinding|clusterrolebindings)         info='rbac.authorization.k8s.io|v1|clusterrolebindings|cluster' ;;
    customresourcedefinition|customresourcedefinitions|crd) info='apiextensions.k8s.io|v1|customresourcedefinitions|cluster' ;;
    priorityclass|priorityclasses|pc)               info='scheduling.k8s.io|v1|priorityclasses|cluster' ;;
    scc|securitycontextconstraint|securitycontextconstraints) \
        info='security.openshift.io|v1|securitycontextconstraints|cluster' ;;
 # Cluster-scoped, bound to config/crd/bases/quartet.ngxstorage.com_quartets.yaml
 # (spec.scope: Cluster) — the convergence gate parses the real CRD and
 # asserts this row models it.
    quartet|quartets)                               info='quartet.ngxstorage.com|v1alpha1|quartets|cluster' ;;
    *)                                              info='' ;;
  esac
  if [[ -z "$info" ]]; then
    return 1
  fi
  if [[ "$mode" == "gvk" ]]; then
    _ATOMIC_GVK_INFO="$info"
    return 0
  fi
  if [[ "$mode" == "scope" ]]; then
    printf '%s' "${info##*|}"
    return 0
  fi
  [[ "${info##*|}" == "cluster" ]]
}

# The ONLY --raw path this helper ever issues: built from the controlled GVK
# table exactly as the lib does (group|version|plural|scope →
# /api/v1 | /apis/<group>/<version> | /namespaces/<ns> | /<plural>/<name>).
# Sets _ATOMIC_RAW on success; rc 2 = fail-closed refusal.
_atomic_api_path() { # <kind> <ns> <name>
  local gvk_group gvk_version gvk_plural gvk_scope base api_path
  _ATOMIC_RAW=""
  if ! _atomic_kind_is_cluster_scoped "$1" gvk; then
    echo "scenario_delete_owned rejected: kind '$1' controlled GVK sözleşmesinde yok — fail-closed, sınırsız --raw geçişi AÇILMAZ (dispatch ÖNCESİ)" >&2
    return 2
  fi
  IFS='|' read -r gvk_group gvk_version gvk_plural gvk_scope <<< "$_ATOMIC_GVK_INFO"
  if [[ -n "$gvk_group" ]]; then base="/apis/$gvk_group/$gvk_version"; else base="/api/$gvk_version"; fi
  if [[ "$gvk_scope" == "namespaced" ]]; then
    if [[ -z "$2" ]]; then
      echo "scenario_delete_owned rejected: namespaced kind '$1' için namespace yok — fail-closed (dispatch ÖNCESİ)" >&2
      return 2
    fi
    base="$base/namespaces/$2"
  fi
  api_path="$base/$gvk_plural/$3"
  [[ -n "$3" && "${api_path##*/}" == "$3" ]] || return 2
  _ATOMIC_RAW="$api_path"
  return 0
}

# Real API reason extraction (CASE-CONTRACT "Gerçek API reason
# sınıflaması", SAME method as the lib): the FIRST
# `Error from server (<Reason>)` token in the stderr. ONLY reason ==
# NotFound is evidence of absence — a "NotFound" substring anywhere else
# (e.g. `Forbidden: User "NotFoundActor" ...`) is NOT; Conflict is the
# server-side UID-precondition 409.
_atomic_api_reason() { # <err-file> -> reason token or nothing
  [[ -f "$1" ]] || return 0
  sed -nE 's/.*Error from server \(([A-Za-z]+)\).*/\1/p' "$1" 2>/dev/null | head -n 1
}

_atomic_cleanup_ns() { # <kind> <given-ns> <have-ns> -> sets _ATOMIC_NS; rc 2 = refusal
  local kind="$1" given="${2:-}" have="${3:-0}"
  _ATOMIC_NS=""
  if _atomic_kind_is_cluster_scoped "$kind"; then
    if [[ "$have" == "1" ]]; then
      echo "cleanup helper rejected: --namespace '$given' cluster-scoped kind '$kind' için anlamsız (dispatch ÖNCESİ reddedildi)" >&2
      return 2
    fi
    return 0
  fi
  if [[ "$have" == "1" ]]; then
    _ATOMIC_NS="$given"
    return 0
  fi
  echo "cleanup helper rejected: namespaced kind '$kind' için --namespace <ns> gerekli (context default'ına sessiz düşüş YOK — dispatch ÖNCESİ reddedildi)" >&2
  return 2
}

_atomic_ownership_row() { # <action> <kind> <ns> <name> <uid> <outcome>
  local reg="${EVIDENCE_DIR}/ownership.tsv"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$1" "$2" "${3:--}" "$4" "$5" "$6" "${RUN_ID:-unknown}" \
    "$(date -u +%Y%m%dT%H%M%SZ)" >> "$reg"
}

_atomic_json_uid() { # <json> -> metadata.uid or nothing (fail-closed)
  printf '%s' "$1" | python3 -c '
import json, sys
try:
    uid = json.load(sys.stdin)["metadata"]["uid"]
    print(uid)
except Exception:
    pass' 2>/dev/null
}

scenario_create_tracked() {
  local kind="${1:-}" name="${2:-}"
  if [[ -z "$kind" || -z "$name" ]]; then
    echo "scenario_create_tracked rejected: <kind> <name> [--namespace <ns>] <manifest> — eksik argüman (dispatch ÖNCESİ)" >&2
    return 2
  fi
  shift 2
  local ns="" have_ns=0 manifest=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --namespace)
        [[ -n "${2:-}" ]] || { echo "scenario_create_tracked rejected: --namespace boş değer (dispatch ÖNCESİ)" >&2; return 2; }
        if [[ "$have_ns" == "1" ]]; then echo "scenario_create_tracked rejected: --namespace iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        ns="$2"; have_ns=1; shift 2 ;;
      --namespace=*)
        ns="${1#--namespace=}"
        if [[ -z "$ns" ]] || [[ "$have_ns" == "1" ]]; then echo "scenario_create_tracked rejected: --namespace geçersiz/iki kez (dispatch ÖNCESİ)" >&2; return 2; fi
        have_ns=1; shift ;;
      -*)
        echo "scenario_create_tracked rejected: bilinmeyen flag '$1' (yalnız --namespace desteklenir; argv surface scenario_kube'dir)" >&2
        return 2 ;;
      *)
        if [[ -n "$manifest" ]]; then echo "scenario_create_tracked rejected: manifest iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        manifest="$1"; shift ;;
    esac
  done
  if [[ -z "$manifest" ]]; then
    echo "scenario_create_tracked rejected: <manifest> dosyası verilmedi (dispatch ÖNCESİ)" >&2
    return 2
  fi
  if [[ ! -f "$manifest" || ! -r "$manifest" ]]; then
    echo "scenario_create_tracked rejected: manifest okunamıyor: $manifest (dispatch ÖNCESİ)" >&2
    return 2
  fi
  _atomic_gate || return 2
 # Controlled GVK contract (protocol pin 2): an UNTABLED kind is fail-closed,
 # and a namespaced grant never authorizes a cluster-scoped kind.
  if ! _atomic_kind_is_cluster_scoped "$kind" gvk; then
    echo "scenario_create_tracked rejected: kind '$kind' controlled GVK sözleşmesinde yok — fail-closed (dispatch ÖNCESİ)" >&2
    return 2
  fi
  if [[ "${SCENARIO_AUTHORIZED_SCOPE:-}" == "namespaced" && "${_ATOMIC_GVK_INFO##*|}" == "cluster" ]]; then
    echo "scenario_create_tracked rejected: namespaced grant + cluster-scoped kind '$kind' — doğru -n verilse de reddedilir (dispatch ÖNCESİ)" >&2
    return 2
  fi
  if ! _atomic_cleanup_ns "$kind" "$ns" "$have_ns"; then return 2; fi
  local nsbound="$_ATOMIC_NS"

  local errf="$EVIDENCE_DIR/create-tracked.err"
  local rc=0 resp uid nsarg=""
  [[ -n "$nsbound" ]] && nsarg="-n $nsbound"
  resp="$(scenario_kube create -f "$manifest" -o json $nsarg </dev/null 2>"$errf")" || rc=$?
  if [[ "$rc" == "2" ]]; then
    [[ -s "$errf" ]] && cat "$errf" >&2
    return 2
  fi
  if [[ "$rc" != "0" ]]; then
    [[ -s "$errf" ]] && cat "$errf" >&2
    _atomic_ownership_row CREATE "$kind" "$nsbound" "$name" "" "api-error"
    return 1
  fi
  uid="$(_atomic_json_uid "$resp")"
  if [[ -z "$uid" ]]; then
    echo "scenario_create_tracked rejected: create yanıtında doğrulanabilir metadata.uid yok — isim-tenkini KAYIT EDİLMEZ, sahiplik kurulamadı" >&2
    _atomic_ownership_row CREATE "$kind" "$nsbound" "$name" "" "no-uid-in-response"
    return 2
  fi
  _atomic_ownership_row CREATE "$kind" "$nsbound" "$name" "$uid" "created"
  printf '%s' "$uid"
  return 0
}

scenario_delete_owned() {
  local kind="${1:-}" name="${2:-}"
  if [[ -z "$kind" || -z "$name" ]]; then
    echo "scenario_delete_owned rejected: <kind> <name> [--namespace <ns>] <uid> — eksik argüman (dispatch ÖNCESİ)" >&2
    return 2
  fi
  shift 2
  local ns="" have_ns=0 uid=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --namespace)
        [[ -n "${2:-}" ]] || { echo "scenario_delete_owned rejected: --namespace boş değer (dispatch ÖNCESİ)" >&2; return 2; }
        if [[ "$have_ns" == "1" ]]; then echo "scenario_delete_owned rejected: --namespace iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        ns="$2"; have_ns=1; shift 2 ;;
      --namespace=*)
        ns="${1#--namespace=}"
        if [[ -z "$ns" ]] || [[ "$have_ns" == "1" ]]; then echo "scenario_delete_owned rejected: --namespace geçersiz/iki kez (dispatch ÖNCESİ)" >&2; return 2; fi
        have_ns=1; shift ;;
      -*)
        echo "scenario_delete_owned rejected: bilinmeyen flag '$1' (yalnız --namespace desteklenir; argv surface scenario_kube'dir)" >&2
        return 2 ;;
      *)
        if [[ -n "$uid" ]]; then echo "scenario_delete_owned rejected: <uid> iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        uid="$1"; shift ;;
    esac
  done
  if [[ -z "$uid" ]]; then
    echo "scenario_delete_owned rejected: <uid> verilmedi — UID precondition olmadan silme YOK (dispatch ÖNCESİ)" >&2
    return 2
  fi
  _atomic_gate || return 2
  if ! _atomic_cleanup_ns "$kind" "$ns" "$have_ns"; then return 2; fi
  local nsbound="$_ATOMIC_NS"

 # Controlled GVK contract (protocol pin 2): an UNTABLED kind is fail-closed,
 # and a namespaced grant never authorizes a cluster-scoped kind — even with
 # a correct --namespace (the flag does not bound a cluster source).
  if ! _atomic_kind_is_cluster_scoped "$kind" gvk; then
    echo "scenario_delete_owned rejected: kind '$kind' controlled GVK sözleşmesinde yok — fail-closed (dispatch ÖNCESİ)" >&2
    return 2
  fi
  local gvk_scope="${_ATOMIC_GVK_INFO##*|}"
  if [[ "${SCENARIO_AUTHORIZED_SCOPE:-}" == "namespaced" && "$gvk_scope" == "cluster" ]]; then
    echo "scenario_delete_owned rejected: namespaced grant + cluster-scoped kind '$kind' — doğru -n verilse de reddedilir (dispatch ÖNCESİ)" >&2
    return 2
  fi

  local errf="$EVIDENCE_DIR/delete-owned.err"
  local cur rc=0 live_uid nsarg="" reason=""
  [[ -n "$nsbound" ]] && nsarg="-n $nsbound"
 # -- step 1: live UID read (no delete of any kind before this verifies) -----
  cur="$(scenario_kube get "$kind" "$name" $nsarg -o json </dev/null 2>"$errf")" || rc=$?
  if [[ "$rc" == "2" ]]; then
    [[ -s "$errf" ]] && cat "$errf" >&2
    return 2
  fi
  if [[ "$rc" != "0" ]]; then
    reason="$(_atomic_api_reason "$errf")"
    if [[ "$reason" == "NotFound" ]]; then
 # Clean API NotFound: already absent — the cleanup goal is met;
 # idempotent success. A "NotFound" substring anywhere else is NOT
 # evidence of absence.
      _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "absent"
      return 0
    fi
    cat "$errf" >&2
    _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "api-error"
    return 1
  fi
  live_uid="$(_atomic_json_uid "$cur")"
  if [[ -z "$live_uid" || "$live_uid" != "$uid" ]]; then
    echo "scenario_delete_owned rejected: UID uyumsuzluğu — beklenen '$uid', yasan '$live_uid' — SAHİPLENME YOK: delete denetlenmedi, stale/foreign aynı-adlı nesne yerinde korunur" >&2
    _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "refused-uid-mismatch"
    return 2
  fi

 # -- step 2: UID-preconditioned delete via the REAL DeleteOptions body ------
 # `delete --raw=<api-path> -f -` with preconditions.uid in the body — the
 # supported-client form (CASE-CONTRACT "Gerçek delete gövdesi"). The
 # name+field-selector form is NEVER issued (real kubectl refuses it
 # client-side; a field selector is not a precondition). A GET-then
 # A→B replacement before this call is answered 409 Conflict by the API:
 # the object is fully preserved, the deletion is never claimed → rc 2
 # (row: precondition-conflict), never a success.
  local api_path body
  if ! _atomic_api_path "$kind" "$nsbound" "$name"; then return 2; fi
  api_path="$_ATOMIC_RAW"
  body="$(python3 -c 'import json, sys
print(json.dumps({"kind": "DeleteOptions", "apiVersion": "v1",
                  "metadata": {"name": sys.argv[1]},
                  "preconditions": {"uid": sys.argv[2]}},
                 separators=(",", ":")))' \
    "$name" "$uid" 2>/dev/null)" || {
    echo "scenario_delete_owned rejected: DeleteOptions gövdesi kurulamadı (dispatch ÖNCESİ)" >&2
    return 2
  }
  if [[ -z "$body" ]]; then
    echo "scenario_delete_owned rejected: DeleteOptions.body boş çıktı (dispatch ÖNCESİ)" >&2
    return 2
  fi
  rc=0
  printf '%s' "$body" | scenario_kube delete --raw="$api_path" $nsarg -f - 2>"$errf" || rc=$?
  if [[ "$rc" == "2" ]]; then
    [[ -s "$errf" ]] && cat "$errf" >&2
    return 2
  fi
  if [[ "$rc" != "0" ]]; then
    reason="$(_atomic_api_reason "$errf")"
    if [[ "$reason" == "Conflict" ]]; then
 # Server-side precondition mismatch (A→B replacement between the live
 # read and the delete): the 409 PRESERVED the object. No delete is
 # claimed — refusal, not success.
      echo "scenario_delete_owned rejected: server precondition conflict (409) — nesne KORUNUR, silme doğrulanamaz, sahiplenilmez (refusal)" >&2
      _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "precondition-conflict"
      return 2
    fi
    if [[ "$reason" != "NotFound" ]]; then
 # Forbidden / timeout / malformed response: cleanup is NEVER a success.
      cat "$errf" >&2
      _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "api-error"
      return 1
    fi
 # reason == NotFound: the object is already gone — the postcondition
 # get below verifies the absence.
  fi

 # -- step 3: postcondition — the deletion must verify as a CLEAN NotFound ---
  rc=0
  cur="$(scenario_kube get "$kind" "$name" $nsarg -o json </dev/null 2>"$errf")" || rc=$?
  if [[ "$rc" == "2" ]]; then
    [[ -s "$errf" ]] && cat "$errf" >&2
    return 2
  fi
  if [[ "$rc" != "0" ]]; then
    reason="$(_atomic_api_reason "$errf")"
    if [[ "$reason" == "NotFound" ]]; then
      _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "deleted"
      return 0
    fi
    cat "$errf" >&2
    _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "api-error"
    return 1
  fi
  echo "scenario_delete_owned rejected: postcondition — nesne hâlâ mevcut; silme DOĞRULANAMADI ve asla sahiplenilmez" >&2
  _atomic_ownership_row DELETE "$kind" "$nsbound" "$name" "$uid" "postcondition-failed"
  return 2
}
