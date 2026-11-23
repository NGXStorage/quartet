# lib/common.sh — Phase B scenario helper surface (tests/scenario/CASE-CONTRACT.md).
#
# Contracted surface (the ONLY names case bodies and the runner rely on):
# scenario_preflight <case.json> — validates case metadata AND the explicit
# live runtime env (KUBE_BIN/KUBE_CONTEXT/TEST_NAMESPACE/ALLOW_LIVE grant);
# rejects BEFORE any mutation. rc: 0 = authorized, 2 = rejected. The grant
# is checked against the case's EFFECT tier (scope), never its self-declared
# risk alone (CASE-CONTRACT protocol pin 1).
# scenario_kube <args...> — the single cluster-call surface. Applies
# the authorized KUBE_BIN + KUBE_CONTEXT and refuses BEFORE invoking when
# (a) the gate was not run, (b) the runtime drifted from the authorized
# target/context/namespace, (c) the authorized scope is anything other
# than exactly namespaced|cluster-scoped (unknown scope fails closed),
# (d) a global option that changes context/server/kubeconfig/auth/target
# appears in the argv — `-s`/`--server` in ALL three forms (split
# `-s https://…`, attached `-shttps://…`, `-s=…`), or (e) a
# namespaced-scope call is not explicitly bound to the authorized
# namespace (-n <authorized ns>). A namespaced grant additionally refuses
# any cluster-scoped kind — positionally named (a CORRECT -n does not
# bound a cluster source) or declared in a readable -f manifest — via the
# controlled GVK/verb/manifest contract; an unverifiable `-f -` stdin
# manifest is refused under a namespaced grant (protocol pin 2). The kind
# lookup is TRI-STATE (known-namespaced / known-cluster / unknown): under
# a namespaced grant an UNKNOWN kind or an UNRESOLVABLE manifest scope is
# refused too — a correct `-n` does not make a cluster source (or an
# untabled kind) namespaced. "Bilinmeyen scope fail-closed" is enforced,
# not just claimed.
# Pod-command arguments after `exec ... --` are never treated as kubectl
# flags. `--raw` is refused for every caller: the ONLY raw path in this
# library is scenario_delete_owned's inspected DeleteOptions.body
# (controlled marker + exact-path check, no unrestricted passthrough).
# scenario_create_tracked <kind> <name> [--namespace <ns>] <manifest>
# and scenario_delete_owned <kind> <name> [--namespace <ns>] <uid> —
# the shared atomic-cleanup pair (CASE-CONTRACT.md "Ortak atomik cleanup
# helper (parent-pin)", protocol pin 8). Case bodies call ONLY these four
# names after scenario_preflight: NO label sweep, NO name-guess deletion.
# Both run through scenario_kube, so every refusal/argv/namespace rule
# above applies to them unchanged. The delete sends the UID precondition
# in DeleteOptions.body `preconditions.uid` through `delete
# --raw=<api-path> -f -` (the pinned body shape); a CLEAN API NotFound is
# the only "absent" verdict — Forbidden/timeout/malformed reasons are API
# errors (1), never a successful cleanup. See their own comments for the
# exact return codes (refusal=2 vs API error=1 throughout).
# scenario_main — case executor: scenario_run (body), then
# optional scenario_cleanup, then final status. Exit codes:
# 0=PASS 1=FAIL 2=BLOCKED 3=SKIP 4=FAIL_CLEANUP
# Cleanup outcome is reported SEPARATELY; a failed cleanup never becomes a
# success (FAIL_CLEANUP).
#
# Case template (cases/<davranış-adı>/case.sh). The helper source path is
# resolved from the case's OWN BASH_SOURCE root, so the same gate runs whether
# the runner dispatches the case or the case is invoked directly:
#
# #!/usr/bin/env bash
# set -uo pipefail
# case_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# source "$case_root/../../lib/common.sh"
# CASE_JSON="$case_root/case.json"
#
# scenario_run() {
# scenario_preflight "$CASE_JSON" || return 2 # gate: rejects before mutation
# scenario_kube apply -n "$TEST_NAMESPACE" -f deploy.yaml || return 1
# [ "<precondition unmet>" ] && return 3 # SKIP when intentionally not met
# }
#
# scenario_cleanup() { # optional
# scenario_kube delete -n "$TEST_NAMESPACE" -f deploy.yaml || return 1
# }
#
# if [ "${BASH_SOURCE[0]}" = "$0" ]; then
# scenario_main
# fi
#
# scenario_run protocol: return 0=PASS, 2 only from preflight rejection
# (BLOCKED), 3=SKIP, any other nonzero=FAIL. Body assertions must `|| return 1`.
# No credentials ever enter case.json metadata or command output — the runtime
# channel (env) is the only carrier; metadata keys that look credential-like
# are rejected by preflight.

SCENARIO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$SCENARIO_ROOT/../.." && pwd)"

_SCENARIO_SRC_REVISION=""
scenario_src_revision() {
 # Safe in a fresh process: exported functions (export -f) do NOT inherit the
 # parent's cache variable, so re-initialize it set-u-safely.
  : "${_SCENARIO_SRC_REVISION:=}"
  if [ -z "$_SCENARIO_SRC_REVISION" ]; then
    _SCENARIO_SRC_REVISION="$(git -C "${REPO_ROOT:-.}" rev-parse --short HEAD 2>/dev/null || echo no-rev)"
  fi
  printf '%s' "$_SCENARIO_SRC_REVISION"
}

# _scenario_parse_case_json <case.json> — METADATA DISCOVERY ONLY: parses
# case.json without ever executing case code. Emits exactly seven values, one
# per line, in fixed order (name, platform, profile, risk, scope,
# precondition, plan_ref); rc=2 on any invalid metadata. Values are validated
# to be newline/control-free before emission, so the line protocol cannot be
# poisoned (no eval of file content anywhere).
_scenario_parse_case_json() {
  python3 - "$1" <<'PY'
import json, re, sys

def reject(msg):
    sys.stderr.write(f"case.json invalid: {msg}\n")
    sys.exit(2)

try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        data = json.load(fh)
except Exception as exc:
    reject(f"unparseable ({exc})")
if not isinstance(data, dict):
    reject("top level must be an object")

# Credentials are a runtime-channel-only concept: metadata must not carry them.
cred = re.compile(r"(?i)(token|secret|password|credential|bearer|apikey|api_key)")
def walk(obj, where="$"):
    if isinstance(obj, dict):
        for k, v in obj.items():
            if cred.search(k):
                reject(f"credential-like key at {where}.{k}")
            walk(v, f"{where}.{k}")
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            walk(v, f"{where}[{i}]")
walk(data)

required = ["name", "platform", "profile", "risk", "scope", "precondition", "plan_ref"]
for key in required:
    value = data.get(key)
    if not isinstance(value, str) or not value.strip():
        reject(f"missing or empty string field: {key}")
    if re.search(r"[\x00-\x1f]", value):
        reject(f"control character in field: {key}")

if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", data["name"]):
    reject(f"name is not a behavioral name: {data['name']}")

for key in required:
    sys.stdout.write(data[key] + "\n")
PY
}

# _scenario_effect_tier <risk> <scope> — EFFECT-based classification
# (CASE-CONTRACT protocol pin 1). A case's grant tier follows what it TOUCHES,
# not what it declares: any cluster-scoped effect (it may create/mutate the
# canonical singleton or global operand resources) is classified at least as
# cluster-scoped-fault, even when the risk field says `isolated`. `all` /
# allow-isolated therefore can never select a singleton-lifecycle case.
_scenario_effect_tier() {
  if [ "$1" = "isolated" ] && [ "$2" = "cluster-scoped" ]; then
    printf 'cluster-scoped-fault'
  else
    printf '%s' "$1"
  fi
}

# scenario_preflight <case.json> — the shared gate. Validates metadata
# (structure, enums, risk/scope consistency, directory/name agreement, no
# credential keys) and the explicit live runtime env. There is NO silent
# fallback to a default target/context/namespace: anything missing is rejected
# before any mutation. On success it exports the case identity and the
# authorized target so scenario_kube can refuse later drift.
scenario_preflight() {
  local case_json="${1:-}"
  local rejected="preflight rejected (before any mutation): "
  if [ -z "$case_json" ] || [ ! -f "$case_json" ]; then
    echo "${rejected}case.json yok: ${case_json:-<hiç verilmedi>}" >&2
    return 2
  fi

  local parsed
  if ! parsed="$(_scenario_parse_case_json "$case_json" 2>&1)"; then
    echo "${rejected}metadata geçersiz — $(printf '%s' "$parsed" | tail -n 1)" >&2
    return 2
  fi

  local -a f
  mapfile -t f <<< "$parsed"
  local name="${f[0]}" platform="${f[1]}" profile="${f[2]}" risk="${f[3]}" scope="${f[4]}"
  local dir_name
  dir_name="$(basename "$(dirname "$case_json")")"
  if [ "$name" != "$dir_name" ]; then
    echo "${rejected}metadata name '$name' case dizini '$dir_name' ile uyuşmuyor" >&2
    return 2
  fi
  case "$risk" in
    isolated|cluster-scoped-fault|uninstall) : ;;
    *) echo "${rejected}risk değersiz: '$risk'" >&2; return 2 ;;
  esac
  case "$scope" in
    namespaced|cluster-scoped) : ;;
    *) echo "${rejected}scope değersiz: '$scope'" >&2; return 2 ;;
  esac
  if [ "$risk" != "isolated" ] && [ "$scope" != "cluster-scoped" ]; then
    echo "${rejected}risk '$risk' cluster-scoped scope gerektirir (scope=$scope)" >&2
    return 2
  fi

 # Explicit live runtime target — no silent defaults for context/namespace,
 # ever. KUBE_BIN has a documented default (kubectl) per the helper contract.
  KUBE_BIN="${KUBE_BIN:-kubectl}"
  case "$KUBE_BIN" in
    kubectl|oc) : ;;
    *) echo "${rejected}KUBE_BIN yalnız kubectl|oc" >&2; return 2 ;;
  esac
  local env_var env_value
  for env_var in KUBE_CONTEXT TEST_NAMESPACE ALLOW_LIVE; do
    env_value="${!env_var:-}"
    if [ -z "$env_value" ]; then
      echo "${rejected}live env eksik: $env_var (sessiz default yok)" >&2
      return 2
    fi
  done
 # Risk-scoped live grant: ALLOW_LIVE must authorize THIS case's EFFECT tier —
 # derived from scope, never from the self-declared risk alone (protocol pin 1).
 # A grant for one tier never authorizes another; unauthorized mutation is
 # rejected here, before any resource exists.
  local effect_tier
  effect_tier="$(_scenario_effect_tier "$risk" "$scope")"
  if [ "$ALLOW_LIVE" != "allow-$effect_tier" ]; then
    echo "${rejected}ALLOW_LIVE='$ALLOW_LIVE' bu case'in ETKİSİ için izin vermiyor (risk=$risk scope=$scope → etki kademesi $effect_tier; ALLOW_LIVE=allow-$effect_tier gerekli)" >&2
    return 2
  fi
  export KUBE_BIN

  SCENARIO_CASE_JSON="$case_json"
  SCENARIO_CASE_NAME="$name"
  SCENARIO_CASE_PLATFORM="$platform"
  SCENARIO_CASE_PROFILE="$profile"
  SCENARIO_CASE_RISK="$risk"
  SCENARIO_CASE_SCOPE="$scope"
  SCENARIO_EFFECT_TIER="$effect_tier"
  SCENARIO_AUTHORIZED_BIN="$KUBE_BIN"
  SCENARIO_AUTHORIZED_CONTEXT="$KUBE_CONTEXT"
 # The authorized namespace is pinned at gate time: a later TEST_NAMESPACE
 # string in the environment constrains NOTHING by itself (protocol pin 2) —
 # scenario_kube binds objects to this recorded value.
  SCENARIO_AUTHORIZED_NAMESPACE="$TEST_NAMESPACE"
  SCENARIO_AUTHORIZED_SCOPE="$scope"

 # Runtime fields case bodies read AFTER preflight (CASE-CONTRACT "Ortak
 # runtime alanları"): RUN_ID, EVIDENCE_DIR, SCENARIO_FAKE_STORE, SOURCE_REV,
 # CASE_SHA. Scratch only under $HOME/.cache/ngxstorage (never /tmp).
  export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
  mkdir -p "$TMPDIR"
  : "${RUN_ID:=scenario-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
  export RUN_ID
  : "${EVIDENCE_DIR:=$TMPDIR/scenario-$name-$RUN_ID/evidence}"
  export EVIDENCE_DIR
  mkdir -p "$EVIDENCE_DIR"
  : "${SCENARIO_FAKE_STORE:=$TMPDIR/scenario-$name-$RUN_ID/store}"
  export SCENARIO_FAKE_STORE
  mkdir -p "$SCENARIO_FAKE_STORE"
  export SOURCE_REV="$(scenario_src_revision)"
  export CASE_SHA="$(find "$(dirname "$case_json")" -maxdepth 1 -type f \
      \( -name 'case.sh' -o -name 'case.json' \) -exec sha256sum {} + 2>/dev/null \
      | sha256sum | cut -d' ' -f1)"

 # Decision record — target/context only; credentials never appear here.
  echo "preflight authorized: case=$name risk=$risk effect=$effect_tier scope=$scope bin=$KUBE_BIN context=$KUBE_CONTEXT namespace=$TEST_NAMESPACE run=$RUN_ID"
  return 0
}

# Global kubectl options that change the call's context/server/kubeconfig/auth/
# target (both `--flag value` and `--flag=value` forms, plus the `-s` short
# form of --server in ALL three forms: split `-s https://…`, attached
# `-shttps://…`, and `-s=…`). scenario_kube applies the gate's authorized
# context ITSELF after the binary name, so any of these in a caller's argv
# would silently re-target the call — they are refused, never normalized away.
scenario_kube_target_flag() {
  case "$1" in
    --context|--context=*|--cluster|--cluster=*|--kubeconfig|--kubeconfig=* \
    |--server|--server=*|-s*|--token|--token=*|--user|--user=* \
    |--username|--username=*|--password|--password=*|--as|--as=* \
    |--as-group|--as-group=*|--request-header|--request-header=* \
    |--certificate-authority|--certificate-authority=* \
    |--client-certificate|--client-certificate=*|--client-key|--client-key=* \
    |--insecure-skip-tls-verify|--insecure-skip-tls-verify=*) return 0 ;;
    *) return 1 ;;
  esac
}

# scenario_kube <args...> — the single cluster-call surface. Refuses BEFORE
# dispatch (return 2, zero binary invocation) when (a) the gate was not run in
# this process, (b) the runtime env drifted from the authorized bin/context,
# (c) TEST_NAMESPACE drifted from the namespace pinned at gate time,
# (d) the authorized scope is not exactly namespaced|cluster-scoped — unknown
# scope fails closed, (e) a global target/auth option appears in the argv
# (`-s`/`--server` in all three forms: split, attached, `=`), or (f) a
# namespaced-scope call is not explicitly bound with -n to the authorized
# namespace. A namespaced grant additionally refuses a cluster-scoped KIND in
# the argv — positionally named (even with a CORRECT -n) or declared in a
# readable -f manifest — via the controlled GVK/verb/manifest contract, and it
# refuses an UNKNOWN kind / unresolvable manifest scope too (tri-state kind
# lookup: known-namespaced / known-cluster / unknown; unknown fails closed).
# The ONLY exception to every raw refusal is the cleanup helper's inspected
# DeleteOptions delete: `--raw=<api-path>` passes when this very process set
# _SC_RAW_DELETE_AUTHORIZED=1 AND the path equals the helper-constructed
# _SC_RAW_DELETE_PATH — there is no unrestricted --raw passthrough. Every call
# that passes is bound to run-id + case identity in $EVIDENCE_DIR.
scenario_kube() {
  if [ -z "${SCENARIO_AUTHORIZED_CONTEXT:-}" ] || [ -z "${SCENARIO_AUTHORIZED_BIN:-}" ]; then
    echo "scenario_kube refused: scenario_preflight gate koşulsuz (mutation reddedildi)" >&2
    return 2
  fi
  if [ "${KUBE_BIN:-}" != "$SCENARIO_AUTHORIZED_BIN" ] || \
     [ "${KUBE_CONTEXT:-}" != "$SCENARIO_AUTHORIZED_CONTEXT" ]; then
    echo "scenario_kube refused: target/context authorized gate'den saptı (mismatch — mutation reddedildi)" >&2
    return 2
  fi
  if [ "${TEST_NAMESPACE:-}" != "${SCENARIO_AUTHORIZED_NAMESPACE:-}" ]; then
    echo "scenario_kube refused: TEST_NAMESPACE gate'te yetkilendirilen kapsamdan saptı — ortamda bir namespace string'inin bulunması uygulanan nesneyi sınırlamaz (mutation reddedildi)" >&2
    return 2
  fi
 # Unknown authorized scope fails closed: only the two values the gate emits
 # are honored, ever.
  case "${SCENARIO_AUTHORIZED_SCOPE:-}" in
    namespaced|cluster-scoped) : ;;
    *)
      echo "scenario_kube refused: yetkili scope bilinmiyor ('${SCENARIO_AUTHORIZED_SCOPE:-none}') — fail-closed, dispatch YOK (mutation reddedildi)" >&2
      return 2 ;;
  esac

 # argv scan: stop at the first `--` — everything after it is the pod command
 # (`exec <pod> -- <pod-command>`), DATA, never kubectl flags (protocol pin 2).
 # The scan also COLLECTS namespace values, -f files and positional tokens so
 # a namespaced grant can be checked against what the call actually addresses.
  local arg verb="${1:-}" expect=none has_raw=0
  local -a ns_values=() file_values=() positional=()
  for arg in "$@"; do
    case "$expect" in
      ns) ns_values+=("$arg"); expect=none; continue ;;
      file) file_values+=("$arg"); expect=none; continue ;;
      skip) expect=none; continue ;;
    esac
    case "$arg" in
      --) break ;;
      --namespace|-n) expect=ns; continue ;;
      --namespace=*|-n=*) ns_values+=("${arg#*=}"); continue ;;
      --filename|-f) expect=file; continue ;;
      --filename=*|-f=*) file_values+=("${arg#*=}"); continue ;;
      -o|--output|-l|--selector|-k|--kustomize|--field-selector|--subresource \
        |-c|--container|-p|--patch|-t|--type|--timeout|--for|--grace-period)
        expect=skip; continue ;;
      --raw)
        echo "scenario_kube refused: --raw dispatch ÖNCESİ reddedildi — sınırsız raw passthrough YOK; tek raw yolu scenario_delete_owned'un incelediği DeleteOptions.body'dir (mutation reddedildi)" >&2
        return 2 ;;
      --raw=*)
        if [ "${_SC_RAW_DELETE_AUTHORIZED:-}" != "1" ] || \
           [ "${_SC_RAW_DELETE_PATH:-}" != "${arg#--raw=}" ]; then
          echo "scenario_kube refused: --raw='${arg#--raw=}' cleanup helper'in incelediği gövde/raw yol değil — sınırsız raw passthrough YOK (mutation reddedildi)" >&2
          return 2
        fi
        has_raw=1; continue ;;
      -*) : ;;
      *) positional+=("$arg") ;;
    esac
    if scenario_kube_target_flag "$arg"; then
      echo "scenario_kube refused: target/auth global flag '$arg' dispatch ÖNCESİ reddedildi — context/server/kubeconfig/auth yalnız gate kanalıyla yetkili değere sabittir (mutation reddedildi)" >&2
      return 2
    fi
  done

 # Namespace binding: a namespaced-scope case may only address the gate-
 # authorized namespace, EXPLICITLY, on every call. No -n at all leaves the
 # object unconstrained (the TEST_NAMESPACE string is not a constraint); a
 # different namespace is foreign. cluster-scoped scope is explicitly
 # authorized to cluster-scope access — there -n may be present or absent.
  if [ "${SCENARIO_AUTHORIZED_SCOPE:-}" = "namespaced" ]; then
    if [ "${#ns_values[@]}" -ne 1 ]; then
      echo "scenario_kube refused: namespaced yetki kapsamı nesne -n $SCENARIO_AUTHORIZED_NAMESPACE ile açık bağlanmadı (namespace'siz nesne sınırlı değil — mutation reddedildi)" >&2
      return 2
    fi
    if [ "${ns_values[0]}" != "$SCENARIO_AUTHORIZED_NAMESPACE" ]; then
      echo "scenario_kube refused: yabancı namespace '${ns_values[0]}' — yetkili kapsam namespaced ($SCENARIO_AUTHORIZED_NAMESPACE); mutation reddedildi" >&2
      return 2
    fi

 # A CORRECT -n does not bound a cluster source (protocol pin 2): the
 # namespaced grant refuses any cluster-scoped kind the call addresses —
 # positionally named, or declared in a manifest — and ALSO any UNKNOWN
 # kind (tri-state lookup: unknown scope fails closed). The controlled raw
 # delete (has_raw) is exempt: its kind/scope/path were pre-verified by the
 # helper that constructed it (and no other caller can reach it).
    if [ "$has_raw" -eq 0 ]; then
      local tok kscope
      case "$verb" in
        get|delete|create|patch|replace|annotate|label|describe|explain)
 # positional[0] is the VERB itself (the scan iterates over all argv);
 # the KIND is positional[1]; everything after it is the object NAME
 # (and never a kind — do not confuse the two). Verbs whose first
 # positional is an object name (exec, logs, ...) are not in this
 # list and are not kind-checked.
          if [ "${#positional[@]}" -ge 2 ]; then
            tok="${positional[1]}"
            if ! kscope="$(_scenario_kind_is_cluster_scoped "$tok" scope)"; then
              echo "scenario_kube refused: kind '$tok' controlled GVK sözleşmesinde YOK (unknown kind) — namespaced grant altında bilinmeyen/çözülemeyen scope fail-closed (mutation reddedildi)" >&2
              return 2
            fi
            if [ "$kscope" = "cluster" ]; then
              echo "scenario_kube refused: namespaced grant + cluster-scoped kind '$tok' — doğru -n verilse de cluster kaynak namespaced yetkiyi aşar (mutation reddedildi)" >&2
              return 2
            fi
          fi ;;
      esac
      if [ "${#file_values[@]}" -gt 0 ]; then
        local ffile kinds mkk
        for ffile in "${file_values[@]}"; do
          if [ "$ffile" = "-" ]; then
            echo "scenario_kube refused: namespaced grant altında -f - (stdin manifest) doğrulanamaz — fail-closed (mutation reddedildi)" >&2
            return 2
          fi
          if [ ! -e "$ffile" ] || [ ! -r "$ffile" ]; then
 # A missing manifest fails CLIENT-SIDE before any API call (kubectl
 # errors locally) — nothing mutable can reach the cluster, so the
 # gate defers to the binary rather than blocking the call.
            continue
          fi
 # Every object in the manifest — including the items of a nested
 # List container — must carry a REAL apiVersion+kind whose scope the
 # controlled table can RESOLVE; unknown or inconsistent pairs are
 # refused (an unresolvable scope is not namespaced).
          if ! pairs="$(python3 - "$ffile" <<'PY'
import json, re, sys

text = open(sys.argv[1], encoding="utf-8", errors="replace").read()
pairs = []  # (apiVersion, kind) per object document
def emit(doc):
    if not isinstance(doc, dict):
        sys.exit(2)  # unresolvable document shape
    pairs.append((doc.get("apiVersion") or "", doc.get("kind") or ""))

try:
    data = json.loads(text)
    for doc in (data if isinstance(data, list) else [data]):
        if not isinstance(doc, dict):
            sys.exit(2)
        items = doc.get("items")
        if isinstance(items, list):
            for item in items:  # List container: validate the items, not "List"
                if not isinstance(item, dict):
                    sys.exit(2)
                pairs.append((item.get("apiVersion") or "", item.get("kind") or ""))
        else:
            emit(doc)
except SystemExit:
    raise
except Exception:
 # YAML fallback: top-level apiVersion:/kind: of each document.
    for chunk in re.split(r"^---\s*$", text, flags=re.M):
        av = re.search(r"^apiVersion:\s*[\'\"]?([^\s\'\"]+)", chunk, flags=re.M)
        kd = re.search(r"^kind:\s*[\'\"]?([^\s\'\"]+)", chunk, flags=re.M)
        if av or kd:
            pairs.append((av.group(1) if av else "", kd.group(1) if kd else ""))
for av, kd in pairs:
    sys.stdout.write(av + "\t" + kd + "\n")
PY
)"; then
            echo "scenario_kube refused: manifest nesne taraması başarısız: $ffile — fail-closed (mutation reddedildi)" >&2
            return 2
          fi
          local mver mkind minfo mg mv mpl msc expected_av
          while IFS=$'\t' read -r mver mkind; do
            if [ -z "$mkind" ]; then
              echo "scenario_kube refused: manifest kind'siz/çözülemeyen nesne taşır ($ffile) — namespaced grant altında unknown scope fail-closed (mutation reddedildi)" >&2
              return 2
            fi
            if ! minfo="$(_scenario_kind_is_cluster_scoped "$mkind" gvk)"; then
              echo "scenario_kube refused: manifest bilinmeyen kind taşır: '$mkind' ($ffile) — controlled GVK sözleşmesinde yok, scope çözülemez, fail-closed (mutation reddedildi)" >&2
              return 2
            fi
            IFS='|' read -r mg mv mpl msc <<< "$minfo"
            expected_av="$mv"
            [ -n "$mg" ] && expected_av="$mg/$mv"
            if [ -z "$mver" ] || [ "$mver" != "$expected_av" ]; then
              echo "scenario_kube refused: manifest apiVersion/kind tutarsız: '${mver:-<hiç>}/$mkind' (beklenen $expected_av) ($ffile) — scope doğrulanamaz, fail-closed (mutation reddedildi)" >&2
              return 2
            fi
            if [ "$msc" = "cluster" ]; then
              echo "scenario_kube refused: manifest cluster-scoped kind taşır: '$mkind' ($ffile) — namespaced grant yetmiyor (mutation reddedildi)" >&2
              return 2
            fi
          done <<< "$pairs"
        done
      fi
    fi
  fi

  if [ -n "${EVIDENCE_DIR:-}" ] && [ -d "${EVIDENCE_DIR}" ]; then
    echo "run_id=${RUN_ID:-unknown} case=${SCENARIO_CASE_NAME:-unknown} $KUBE_BIN --context $KUBE_CONTEXT $*" \
      >> "$EVIDENCE_DIR/kube-calls.log"
  fi
  "$KUBE_BIN" --context "$KUBE_CONTEXT" "$@"
}

# =============================================================================
# Ortak atomik cleanup helper (CASE-CONTRACT.md "Ortak atomik cleanup helper
# (parent-pin)"; tek sahip bu dosya — risk-gate sahibi). Beş case gövdesi
# scenario_preflight sonrası YALNIZ bu iki işlevle sahiplik kurar/siler; label
# sweep YOK, isim-tenkine delete YOK. İki işlev de scenario_kube üzerinden
# çalıştığından gate/argv/namespace refusal'ları birebir aynen uygulanır ve
# her çağrı $EVIDENCE_DIR/kube-calls.log'a düşer. Return ayrımı tüm surface'de
# korunur: refusal (gate/invocation/UID)=2, API error=1.
# =============================================================================

# Controlled GVK/verb/manifest contract (CASE-CONTRACT protocol pin 2): the
# single static kind table of this library. It serves THREE modes from ONE
# exported name (run.sh's fixed export list — no new helper names exist):
# _scenario_kind_is_cluster_scoped <kind>
# rc 0 = KNOWN cluster-scoped, rc 1 = known-namespaced OR unknown.
# Used where only the cluster direction matters (the cleanup helper's
# --namespace misuse check); the namespaced grant checks use the tri-state
# "scope" mode below instead — unknown is NOT namespaced there.
# _scenario_kind_is_cluster_scoped <kind> scope
# prints "namespaced" or "cluster"; rc 1 = kind NOT in the contract
# (UNKNOWN — the third state). A correct -n does not make an unknown or
# cluster kind namespaced: under a namespaced grant, unknown fails closed.
# _scenario_kind_is_cluster_scoped <kind> gvk
# prints "group|version|plural|scope" for the kind (group empty = core);
# rc 1 = kind NOT in the contract (fail-closed at the construction sites).
# Input is normalized: lowercased, `apiVersion/kind` prefixes and
# `kind.group` suffixes stripped (apps/v1/deployments, csidrivers.storage.
# k8s.io → deployments, csidrivers). Kinds the cleanup helpers treat as
# cluster-scoped are NEVER passed -n (kubectl would ignore it, and the strict
# API fixtures bind -n into the object key, so a stray -n would read the
# wrong row).
_scenario_kind_is_cluster_scoped() {
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
 # (spec.scope: Cluster) — the convergence gate (test-case-integration.sh)
 # parses that real CRD independently and asserts this row models it.
    quartet|quartets)                               info='quartet.ngxstorage.com|v1alpha1|quartets|cluster' ;;
    *)                                              info='' ;;
  esac
  if [ -z "$info" ]; then
    return 1
  fi
  if [ "$mode" = "gvk" ]; then
    printf '%s' "$info"
    return 0
  fi
  if [ "$mode" = "scope" ]; then
    printf '%s' "${info##*|}"
    return 0
  fi
  [ "${info##*|}" = "cluster" ]
}

# _scenario_cleanup_ns <kind> <given-ns> <have-ns:0|1> — namespace binding for
# a cleanup-helper call. Sets _SC_CLEANUP_NS on success; rc=2 (refusal, zero
# cluster call) when the binding would need a silent default:
# * cluster-scoped kind + --namespace given -> misuse, refused;
# * namespaced kind + --namespace given -> bound to the given value
# (a foreign value is refused by scenario_kube itself, rc=2);
# * namespaced kind, namespaced-scope gate, no --namespace -> bound to the
# gate's authorized namespace, explicitly;
# * namespaced kind, cluster-scoped-scope gate, no --namespace -> refused
# (never inherit the context's current-namespace default).
_scenario_cleanup_ns() {
  local kind="$1" given="${2:-}" have="${3:-0}"
  _SC_CLEANUP_NS=""
  if _scenario_kind_is_cluster_scoped "$kind"; then
    if [ "$have" = 1 ]; then
      echo "cleanup helper rejected: --namespace '$given' cluster-scoped kind '$kind' için anlamsız (dispatch ÖNCESİ reddedildi)" >&2
      return 2
    fi
    return 0
  fi
  if [ "$have" = 1 ]; then
    _SC_CLEANUP_NS="$given"
    return 0
  fi
  if [ "${SCENARIO_AUTHORIZED_SCOPE:-}" = "namespaced" ]; then
    _SC_CLEANUP_NS="${SCENARIO_AUTHORIZED_NAMESPACE:-}"
    return 0
  fi
  echo "cleanup helper rejected: namespaced kind '$kind' için --namespace <ns> gerekli (context default'ına sessiz düşüş YOK — dispatch ÖNCESİ reddedildi)" >&2
  return 2
}

# $EVIDENCE_DIR/ownership.tsv — the run's atomic-cleanup registry. One tab-
# separated row per tracked create/delete outcome:
# action kind namespace name uid outcome run_id created_utc
# namespace is "-" for cluster-scoped objects. CREATE rows carry the
# create-response uid; DELETE rows carry the EXPECTED (passed) uid and a
# verified outcome: created | api-error | no-uid-in-response | deleted |
# absent | refused-uid-mismatch | postcondition-failed |
# precondition-conflict (server 409 on the DeleteOptions.body precondition —
# the object was preserved, no delete claimed). The registry is
# EVIDENCE, not a gate: the refusal to delete comes from the live UID
# comparison, never from registry membership. Written best-effort (a missing
# EVIDENCE_DIR downgrades nothing — the refusal itself is the contract).
_scenario_ownership_row() { # <action> <kind> <ns> <name> <uid> <outcome>
  [ -d "${EVIDENCE_DIR:-}" ] || return 0
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$1" "$2" "${3:--}" "$4" "$5" "$6" "${RUN_ID:-unknown}" \
    "$(date -u +%Y%m%dT%H%M%SZ)" >> "$EVIDENCE_DIR/ownership.tsv"
}

# _scenario_json_uid <object-json> — print metadata.uid, or nothing when the
# input is not a JSON object carrying one (fail-closed: an empty result is a
# refusal, never a guess).
_scenario_json_uid() {
  python3 -c 'import json, sys
try:
    uid = json.load(sys.stdin)["metadata"]["uid"]
    print(uid)
except Exception:
    pass' <<<"$1" 2>/dev/null
}

# scenario_create_tracked <kind> <name> [--namespace <ns>] <manifest> —
# create the object through the single cluster surface (`create -f <manifest>
# -o json`) and bind THIS run's ownership to the create-response
# metadata.uid (CASE-CONTRACT pin 8). Emits the registered uid on stdout
# (diagnostics on stderr), so a body captures it with
# `uid="$(scenario_create_tracked ...)"` and later passes it to
# scenario_delete_owned. Registers a CREATE row in $EVIDENCE_DIR/ownership.tsv.
# Return: 0 created + registered (stdout = uid); 1 API error (the create
# itself failed — nothing registered); 2 refusal (gate not run, invocation or
# namespace misuse, kind outside the controlled GVK contract, namespaced grant
# + cluster-scoped kind even with a correct --namespace, or a create response
# with no verifiable metadata.uid — a name guess is NEVER registered).
scenario_create_tracked() {
  local kind="${1:-}" name="${2:-}"
  if [ -z "$kind" ] || [ -z "$name" ]; then
    echo "scenario_create_tracked rejected: <kind> <name> [--namespace <ns>] <manifest> — eksik argüman (dispatch ÖNCESİ)" >&2
    return 2
  fi
  shift 2
  local ns="" have_ns=0 manifest=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --namespace)
        [ -n "${2:-}" ] || { echo "scenario_create_tracked rejected: --namespace boş değer (dispatch ÖNCESİ)" >&2; return 2; }
        if [ "$have_ns" = 1 ]; then echo "scenario_create_tracked rejected: --namespace iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        ns="$2"; have_ns=1; shift 2 ;;
      --namespace=*)
        ns="${1#--namespace=}"
        if [ -z "$ns" ] || [ "$have_ns" = 1 ]; then echo "scenario_create_tracked rejected: --namespace geçersiz/iki kez (dispatch ÖNCESİ)" >&2; return 2; fi
        have_ns=1; shift ;;
      -*)
        echo "scenario_create_tracked rejected: bilinmeyen flag '$1' (yalnız --namespace desteklenir; argv surface scenario_kube'dir)" >&2
        return 2 ;;
      *)
        if [ -n "$manifest" ]; then echo "scenario_create_tracked rejected: manifest iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        manifest="$1"; shift ;;
    esac
  done
  if [ -z "$manifest" ]; then
    echo "scenario_create_tracked rejected: <manifest> dosyası verilmedi (dispatch ÖNCESİ)" >&2
    return 2
  fi
  if [ ! -f "$manifest" ] || [ ! -r "$manifest" ]; then
    echo "scenario_create_tracked rejected: manifest okunamıyor: $manifest (dispatch ÖNCESİ)" >&2
    return 2
  fi
  if [ -z "${SCENARIO_AUTHORIZED_CONTEXT:-}" ] || [ -z "${SCENARIO_AUTHORIZED_BIN:-}" ]; then
    echo "scenario_create_tracked rejected: scenario_preflight gate koşulsuz (mutation reddedildi)" >&2
    return 2
  fi
 # Controlled GVK contract (protocol pin 2): an UNTABLED kind is fail-closed,
 # and a namespaced grant never authorizes a cluster-scoped kind — even with
 # a correct --namespace (the flag does not bound a cluster source).
  local gvk_info gvk_scope
  if ! gvk_info="$(_scenario_kind_is_cluster_scoped "$kind" gvk)"; then
    echo "scenario_create_tracked rejected: kind '$kind' controlled GVK sözleşmesinde yok — fail-closed (dispatch ÖNCESİ)" >&2
    return 2
  fi
  gvk_scope="${gvk_info##*|}"
  if [ "${SCENARIO_AUTHORIZED_SCOPE:-}" = "namespaced" ] && [ "$gvk_scope" = "cluster" ]; then
    echo "scenario_create_tracked rejected: namespaced grant + cluster-scoped kind '$kind' — doğru -n verilse de reddedilir (dispatch ÖNCESİ)" >&2
    return 2
  fi
  _scenario_cleanup_ns "$kind" "$ns" "$have_ns" || return 2

  local errf="$EVIDENCE_DIR/create-tracked.err"
  local resp rc=0
  resp="$(scenario_kube create -f "$manifest" -o json ${_SC_CLEANUP_NS:+-n "$_SC_CLEANUP_NS"} </dev/null 2>"$errf")" || rc=$?
  if [ "$rc" -eq 2 ]; then
    [ -s "$errf" ] && cat "$errf" >&2
    return 2
  fi
  if [ "$rc" -ne 0 ]; then
    [ -s "$errf" ] && cat "$errf" >&2
    _scenario_ownership_row CREATE "$kind" "$_SC_CLEANUP_NS" "$name" "" "api-error"
    return 1
  fi
  local uid
  uid="$(_scenario_json_uid "$resp")"
  if [ -z "$uid" ]; then
    echo "scenario_create_tracked rejected: create yanıtında doğrulanabilir metadata.uid yok — isim-tenkini KAYIT EDİLMEZ, sahiplik kurulamadı" >&2
    _scenario_ownership_row CREATE "$kind" "$_SC_CLEANUP_NS" "$name" "" "no-uid-in-response"
    return 2
  fi
  _scenario_ownership_row CREATE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "created"
  printf '%s' "$uid"
  return 0
}

# scenario_delete_owned <kind> <name> [--namespace <ns>] <uid> — atomic,
# ownership-verified deletion (CASE-CONTRACT pin 8), in three steps:
# 1. get: read the LIVE uid of the object at kind/name.
# CLEAN NotFound -> the cleanup goal is already met: return 0
# (idempotent; row: absent). ANY other error — Forbidden
# (even a body mentioning a NotFound* user), timeout,
# malformed response — is an API error: return 1.
# A bare substring match on "NotFound" is NOT evidence.
# live uid != <uid> -> SAHİPLENME YOK: NO delete is dispatched, the
# stale/foreign same-name object survives untouched,
# return 2 (row: refused-uid-mismatch).
# 2. delete --raw=<api-path> -f - with the UID precondition in the REAL
# DeleteOptions.body sent on stdin: {"kind":"DeleteOptions",
# "apiVersion":"v1","metadata":{"name":<name>},"preconditions":
# {"uid":<uid>}} — no label sweep, no bare name delete, and NO
# --field-selector=metadata.uid (real kubectl rejects name+selector: it
# is not a delete precondition). scenario_kube admits --raw ONLY for
# this helper's inspected path+body (controlled marker, no unrestricted
# passthrough). Server-side precondition mismatch (an A→B replacement
# between get and delete) -> 409 Conflict -> the object is preserved
# and the deletion is never claimed: return 2 (row:
# precondition-conflict).
# 3. postcondition get: must be a CLEAN NotFound. Object still present ->
# the deletion is UNVERIFIED and never claimed: return 2 (row:
# postcondition-failed); any other read error -> return 1.
# Return: 0 deleted and verified (or already absent); 1 API error; 2 refusal
# (gate not run, invocation/namespace misuse, kind outside the controlled
# GVK contract, namespaced grant + cluster-scoped kind, UID mismatch, server
# 409, unverified postcondition). Emits nothing on stdout; diagnostics on
# stderr. The passed <uid> is authoritative ONLY because the contract
# requires it to come from this run's create response / ownership registry.
scenario_delete_owned() {
  local kind="${1:-}" name="${2:-}"
  if [ -z "$kind" ] || [ -z "$name" ]; then
    echo "scenario_delete_owned rejected: <kind> <name> [--namespace <ns>] <uid> — eksik argüman (dispatch ÖNCESİ)" >&2
    return 2
  fi
  shift 2
  local ns="" have_ns=0 uid=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --namespace)
        [ -n "${2:-}" ] || { echo "scenario_delete_owned rejected: --namespace boş değer (dispatch ÖNCESİ)" >&2; return 2; }
        if [ "$have_ns" = 1 ]; then echo "scenario_delete_owned rejected: --namespace iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        ns="$2"; have_ns=1; shift 2 ;;
      --namespace=*)
        ns="${1#--namespace=}"
        if [ -z "$ns" ] || [ "$have_ns" = 1 ]; then echo "scenario_delete_owned rejected: --namespace geçersiz/iki kez (dispatch ÖNCESİ)" >&2; return 2; fi
        have_ns=1; shift ;;
      -*)
        echo "scenario_delete_owned rejected: bilinmeyen flag '$1' (yalnız --namespace desteklenir; argv surface scenario_kube'dir)" >&2
        return 2 ;;
      *)
        if [ -n "$uid" ]; then echo "scenario_delete_owned rejected: <uid> iki kez verildi (dispatch ÖNCESİ)" >&2; return 2; fi
        uid="$1"; shift ;;
    esac
  done
  if [ -z "$uid" ]; then
    echo "scenario_delete_owned rejected: <uid> verilmedi — UID precondition olmadan silme YOK (dispatch ÖNCESİ)" >&2
    return 2
  fi
  if [ -z "${SCENARIO_AUTHORIZED_CONTEXT:-}" ] || [ -z "${SCENARIO_AUTHORIZED_BIN:-}" ]; then
    echo "scenario_delete_owned rejected: scenario_preflight gate koşulsuz (mutation reddedildi)" >&2
    return 2
  fi
 # Controlled GVK contract (protocol pin 2): an UNTABLED kind is fail-closed,
 # and a namespaced grant never authorizes a cluster-scoped kind — even with
 # a correct --namespace (the flag does not bound a cluster source).
  local gvk_info gvk_group gvk_version gvk_plural gvk_scope reason=""
  if ! gvk_info="$(_scenario_kind_is_cluster_scoped "$kind" gvk)"; then
    echo "scenario_delete_owned rejected: kind '$kind' controlled GVK sözleşmesinde yok — fail-closed (dispatch ÖNCESİ)" >&2
    return 2
  fi
  IFS='|' read -r gvk_group gvk_version gvk_plural gvk_scope <<< "$gvk_info"
  if [ "${SCENARIO_AUTHORIZED_SCOPE:-}" = "namespaced" ] && [ "$gvk_scope" = "cluster" ]; then
    echo "scenario_delete_owned rejected: namespaced grant + cluster-scoped kind '$kind' — doğru -n verilse de reddedilir (dispatch ÖNCESİ)" >&2
    return 2
  fi
  _scenario_cleanup_ns "$kind" "$ns" "$have_ns" || return 2

 # NB: the ${_SC_CLEANUP_NS:+-n "$_SC_CLEANUP_NS"} expansion below is
 # deliberately unquoted so it word-splits into the two argv words
 # `-n <ns>` (or nothing for cluster-scoped kinds).
  local errf="$EVIDENCE_DIR/delete-owned.err"
  local cur rc=0
 # _scenario_api_reason equivalent (inline, no new exported name): the FIRST
 # `Error from server (<Reason>)` in the stderr — the real API reason.
 # ONLY reason == NotFound is evidence of absence; Forbidden/timeout/
 # malformed/client errors carry a different (or no) reason.
 # -- step 1: live UID read (no delete of any kind before this verifies) -----
  cur="$(scenario_kube get "$kind" "$name" ${_SC_CLEANUP_NS:+-n "$_SC_CLEANUP_NS"} -o json </dev/null 2>"$errf")" || rc=$?
  if [ "$rc" -eq 2 ]; then
    [ -s "$errf" ] && cat "$errf" >&2
    return 2
  fi
  if [ "$rc" -ne 0 ]; then
    reason="$(sed -nE 's/.*Error from server \(([A-Za-z]+)\).*/\1/p' "$errf" 2>/dev/null | head -n 1)"
    if [ "$reason" = "NotFound" ]; then
 # Clean API NotFound: already absent — the cleanup goal is met;
 # idempotent success. A "NotFound" substring anywhere else in the
 # message (e.g. `Forbidden: User NotFoundActor ...`) is NOT evidence.
      _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "absent"
      return 0
    fi
    cat "$errf" >&2
    _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "api-error"
    return 1
  fi
  local live_uid
  live_uid="$(_scenario_json_uid "$cur")"
  if [ -z "$live_uid" ] || [ "$live_uid" != "$uid" ]; then
    echo "scenario_delete_owned rejected: UID uyumsuzluğu — beklenen '$uid', yasan '$live_uid' — SAHİPLENME YOK: delete denetlenmedi, stale/foreign aynı-adlı nesne yerinde korunur" >&2
    _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "refused-uid-mismatch"
    return 2
  fi

 # -- step 2: UID-preconditioned delete — the precondition travels in the
 # REAL DeleteOptions.body of the DELETE request, sent through the
 # supported client path `delete --raw=<api-path> -f -` (CASE-CONTRACT
 # "Gerçek delete gövdesi"). The body is the pinned shape, constructed
 # here and inspected by scenario_kube via the controlled marker; no
 # other caller may pass --raw.
  local api_path base
  if [ -n "$gvk_group" ]; then base="/apis/$gvk_group/$gvk_version"; else base="/api/$gvk_version"; fi
  if [ "$gvk_scope" = "namespaced" ]; then
    if [ -z "$_SC_CLEANUP_NS" ]; then
      echo "scenario_delete_owned rejected: namespaced kind '$kind' için namespace yok — fail-closed (dispatch ÖNCESİ)" >&2
      return 2
    fi
    base="$base/namespaces/$_SC_CLEANUP_NS"
  fi
  api_path="$base/$gvk_plural/$name"
  local body
  body="$(python3 -c 'import json, sys
print(json.dumps({"kind": "DeleteOptions", "apiVersion": "v1",
                  "metadata": {"name": sys.argv[1]},
                  "preconditions": {"uid": sys.argv[2]}},
                 separators=(",", ":")))' "$name" "$uid")" || {
    echo "scenario_delete_owned rejected: DeleteOptions.body kurulamadı — fail-closed (dispatch ÖNCESİ)" >&2
    return 2
  }
  if [ -z "$body" ]; then
    echo "scenario_delete_owned rejected: DeleteOptions.body boş çıktı — fail-closed (dispatch ÖNCESİ)" >&2
    return 2
  fi
  rc=0
  printf '%s' "$body" | \
    _SC_RAW_DELETE_AUTHORIZED=1 _SC_RAW_DELETE_PATH="$api_path" \
    scenario_kube delete --raw="$api_path" ${_SC_CLEANUP_NS:+-n "$_SC_CLEANUP_NS"} \
    -f - 2>"$errf" || rc=$?
  if [ "$rc" -eq 2 ]; then
    [ -s "$errf" ] && cat "$errf" >&2
    return 2
  fi
  if [ "$rc" -ne 0 ]; then
    reason="$(sed -nE 's/.*Error from server \(([A-Za-z]+)\).*/\1/p' "$errf" 2>/dev/null | head -n 1)"
    if [ "$reason" = "Conflict" ]; then
 # Server-side precondition mismatch (A→B replacement between the live
 # read and the delete): the 409 PRESERVED the object. No delete is
 # claimed — refusal, not success.
      echo "scenario_delete_owned rejected: server precondition conflict (409) — nesne KORUNUR, silme doğrulanamaz, sahiplenilmez (refusal)" >&2
      _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "precondition-conflict"
      return 2
    fi
    if [ "$reason" != "NotFound" ]; then
 # Forbidden / timeout / malformed response: cleanup is NEVER a success.
      cat "$errf" >&2
      _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "api-error"
      return 1
    fi
 # reason == NotFound: the object is already gone — the postcondition
 # get below verifies the absence.
  fi

 # -- step 3: postcondition — the deletion must verify as a CLEAN NotFound ---
 # Live objects with finalizers/grace periods terminate asynchronously. A
 # bounded wait (SCENARIO_DELETE_POSTCONDITION_WAIT seconds, default 0 =
 # immediate, the offline model) allows graceful termination WITHOUT
 # weakening the proof: only a typed NotFound completes; a UID swap is a
 # refusal (foreign replacement preserved); an API error is never absence.
  local pc_deadline=$(( $(date +%s) + ${SCENARIO_DELETE_POSTCONDITION_WAIT:-0} ))
  while :; do
    rc=0
    cur="$(scenario_kube get "$kind" "$name" ${_SC_CLEANUP_NS:+-n "$_SC_CLEANUP_NS"} -o json </dev/null 2>"$errf")" || rc=$?
    if [ "$rc" -eq 2 ]; then
      [ -s "$errf" ] && cat "$errf" >&2
      return 2
    fi
    if [ "$rc" -ne 0 ]; then
      reason="$(sed -nE 's/.*Error from server \(([A-Za-z]+)\).*/\1/p' "$errf" 2>/dev/null | head -n 1)"
      if [ "$reason" = "NotFound" ]; then
        _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "deleted"
        return 0
      fi
      cat "$errf" >&2
      _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "api-error"
      return 1
    fi
    live_uid="$(_scenario_json_uid "$cur")"
    if [ -z "$live_uid" ] || [ "$live_uid" != "$uid" ]; then
      echo "scenario_delete_owned rejected: postcondition UID değişimi — beklenen '$uid', yaşayan '$live_uid' — foreign replacement korunur, sahiplenme YOK" >&2
      _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "refused-uid-mismatch"
      return 2
    fi
    if [ "$(date +%s)" -lt "$pc_deadline" ]; then
      sleep 2
      continue
    fi
    echo "scenario_delete_owned rejected: postcondition — nesne hâlâ mevcut; silme DOĞRULANAMADI ve asla sahiplenilmez" >&2
    _scenario_ownership_row DELETE "$kind" "$_SC_CLEANUP_NS" "$name" "$uid" "postcondition-failed"
    return 2
  done
}

# scenario_main — case executor (called from the case.sh tail on every
# EXECUTION of the case: both runner dispatch and direct invocation). Runs the
# body (which must call scenario_preflight per the template), then the
# optional cleanup, then records the final status. Exit:
# 0=PASS 1=FAIL 2=BLOCKED 3=SKIP 4=FAIL_CLEANUP.
scenario_main() {
  export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
  mkdir -p "$TMPDIR"
 # RUN_ID is the contract run-id; preflight normally sets it, but a case that
 # reaches scenario_main without a successful preflight still gets one.
  : "${RUN_ID:=scenario-direct-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
  export RUN_ID
  : "${EVIDENCE_DIR:=$TMPDIR/quartet-scenario-$RUN_ID}"
  export EVIDENCE_DIR
  mkdir -p "$EVIDENCE_DIR"

  local body_rc=0
  scenario_run || body_rc=$?

  local body_status
  case "$body_rc" in
    0) body_status=PASS ;;
    2) body_status=BLOCKED ;;   # sanctioned only by preflight rejection
    3) body_status=SKIP ;;
    *) body_status=FAIL ;;
  esac

  local cleanup_status=SKIPPED cleanup_rc=0
  if [ "$body_status" = PASS ] || [ "$body_status" = FAIL ]; then
    if declare -F scenario_cleanup >/dev/null 2>&1; then
      scenario_cleanup || cleanup_rc=$?
      if [ "$cleanup_rc" -eq 0 ]; then cleanup_status=PASS; else cleanup_status=FAIL; fi
    fi
  fi

  local final final_rc
  case "$body_status" in
    PASS)
      if [ "$cleanup_status" = FAIL ]; then
        final=FAIL_CLEANUP; final_rc=4          # FAIL_CLEANUP never becomes success
      else
        final=PASS; final_rc=0
      fi ;;
    BLOCKED) final=BLOCKED; final_rc=2 ;;
    SKIP)    final=SKIP; final_rc=3 ;;
    *)       final=FAIL; final_rc=1 ;;
  esac

  local case_dir_name="case"
  [ -n "${CASE_JSON:-}" ] && case_dir_name="$(basename "$(dirname "${CASE_JSON}")")"
  local status_name="${SCENARIO_CASE_NAME:-$case_dir_name}"
  {
    echo "name=$status_name"
    echo "run_id=$RUN_ID"
    echo "source_revision=$(scenario_src_revision)"
    echo "kube_bin=${KUBE_BIN:-unset} kube_context=${KUBE_CONTEXT:-unset} test_namespace=${TEST_NAMESPACE:-unset}"
    echo "risk=${SCENARIO_CASE_RISK:-unset} scope=${SCENARIO_CASE_SCOPE:-unset}"
    echo "body=$body_status(body_rc=$body_rc)"
    echo "cleanup=$cleanup_status(cleanup_rc=$cleanup_rc)"
    echo "final=$final"
  } > "$EVIDENCE_DIR/$status_name.status"

  echo "[$status_name] final=$final body=$body_status cleanup=$cleanup_status run_id=$RUN_ID evidence=$EVIDENCE_DIR"
  exit "$final_rc"
}
