#!/usr/bin/env bash
# run.sh — Phase B shared scenario executor (tests/scenario).
#
# Surface:
# run.sh --list | --help discovery-only: prints case.json metadata, runs
# NO case code, makes NO cluster call, needs NO
# live env.
# run.sh all effect-scoped inclusion: ONLY namespace-local
# (isolated effect) cases are triggered. Any case
# whose EFFECT touches the canonical singleton or
# global operand resources (cluster-scoped scope)
# is excluded and NEVER auto-triggered under `all`,
# even if its case.json says risk=isolated.
# Classification is by effect, not by the declared
# risk field (CASE-CONTRACT protocol pin 1).
# run.sh risk:<risk> explicit risk selection: isolated |
# cluster-scoped-fault | uninstall.
# run.sh <davranış-adı> single-case dispatch. The case runs through its
# own scenario_main, so the SAME scenario_preflight
# gate applies here as under direct invocation.
#
# Any EXECUTION selection requires the explicit live runtime target (no silent
# defaults for context/namespace); a missing value is rejected rc=2 before any
# case is dispatched:
# KUBE_CONTEXT=<ctx> TEST_NAMESPACE=<ns> ALLOW_LIVE=allow-<risk>
# KUBE_BIN defaults to kubectl (kubectl|oc). ALLOW_LIVE is a risk-scoped
# grant: `all` (isolated only) needs ALLOW_LIVE=allow-isolated; a
# risk:cluster-scoped-fault selection needs ALLOW_LIVE=allow-cluster-scoped-fault.
# EVIDENCE_DIR is optional (default $HOME/.cache/ngxstorage/quartet-scenario-<run-id>)
#
# Result contract (tests/scenario/CASE-CONTRACT.md): per-case final status is
# PASS / FAIL / BLOCKED / SKIP / FAIL_CLEANUP; cleanup success is reported
# separately in the evidence status file and a failed cleanup never becomes a
# success. Aggregate exit: 0 = all PASS, 1 = any FAIL or FAIL_CLEANUP,
# 2 = any BLOCKED, 3 = any SKIP. Crash/missing status is fail-closed FAIL.
set -uo pipefail
SCENARIO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCENARIO_ROOT/lib/common.sh"
export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
mkdir -p "$TMPDIR"

usage() {
  echo "run.sh <all|risk:<risk>|<davranış-adı>|--list|--help>"
  echo "  all             etki-scoped: yalnız namespace-local (isolated etki) case'leri;"
  echo "                  cluster-scoped ETKİ (singleton/operand lifecycle) risk=isolated"
  echo "                  dese bile all'dan hariçtir — sınıflama etkiye göredir"
  echo "  risk:<risk>     explicit risk seçimi: isolated|cluster-scoped-fault|uninstall"
  echo "  <davranış-adı>  tek case dispatch (case kendi scenario_main/preflight gate'ini koşar)"
  echo " --list|--help   discovery-only: metadata basar, case kodu/cluster/canlı env gerekmez"
  echo "canlı env (execution için explicit, sessiz default yok):"
  echo "  KUBE_CONTEXT=<ctx> TEST_NAMESPACE=<ns> ALLOW_LIVE=allow-<risk>"
  echo "  KUBE_BIN opsiyonel (varsayılan kubectl; kubectl|oc)"
  echo "  ALLOW_LIVE risk grant'ıdır: all→allow-isolated, risk:X→allow-X"
  echo "  EVIDENCE_DIR opsiyonel (varsayılan: \$HOME/.cache/ngxstorage/quartet-scenario-<run-id>)"
}

# ---- discovery (zero execution, zero cluster call, zero env requirement) ----
case "${1:-all}" in
  --help|-h)
    usage
    exit 0 ;;
  --list)
    [ "$#" -eq 1 ] || { echo "keşif seçimi ek argüman kabul etmez" >&2; exit 2; }
    cases_dir="$SCENARIO_ROOT/cases"
    if [ -d "$cases_dir" ]; then
      found=0
      for cj in "$cases_dir"/*/case.json; do
        [ -f "$cj" ] || continue
        found=1
        parsed=""
        if parsed="$(_scenario_parse_case_json "$cj" 2>/dev/null)"; then
          local_dir_name="$(basename "$(dirname "$cj")")"
          list_f=()
          mapfile -t list_f <<< "$parsed"
          list_risk="${list_f[3]}"
          list_scope="${list_f[4]}"
 # EFFECT classification (protocol pin 1): `all` inclusion follows
 # what the case touches (scope), not its self-declared risk alone.
          list_effect="$(_scenario_effect_tier "$list_risk" "$list_scope")"
          list_consistent=1
          if [ "$list_risk" != "isolated" ] && [ "$list_scope" != "cluster-scoped" ]; then
            list_consistent=0
          fi
          if [ "${list_f[0]}" != "$local_dir_name" ]; then
            echo "INVALID  $local_dir_name (metadata name ≠ dizin)"
            continue
          fi
          if [ "$list_consistent" -eq 0 ]; then
            echo "INVALID  $local_dir_name (risk/scope tutarsız: $list_risk/$list_scope)"
            continue
          fi
          case "$list_effect" in
            isolated) list_all="all'da dâhil" ;;
            *)
              if [ "$list_effect" = "$list_risk" ]; then
                list_all="all'dan hariç (risk-scoped)"
              else
                list_all="all'dan hariç (etki: cluster-scoped)"
              fi ;;
          esac
          printf '%s  risk=%s effect=%s scope=%s platform=%s profile=%s [%s] plan_ref=%s\n' \
            "${list_f[0]}" "$list_risk" "$list_effect" "$list_scope" "${list_f[1]}" "${list_f[2]}" "$list_all" "${list_f[6]}"
        else
          echo "INVALID  $(basename "$(dirname "$cj")") (metadata)"
        fi
      done
      [ "$found" -eq 1 ] || echo "(no cases)"
    else
      echo "(no cases)"
    fi
    exit 0 ;;
esac

[ "$#" -le 1 ] || { echo "run.sh bu seçimde ek argüman kabul etmez: $2" >&2; exit 2; }

sel="${1:-all}"
selection="" ; included_risk="" ; case_name=""
case "$sel" in
  all)
    selection="all"; included_risk="isolated" ;;
  risk:*)
    selection="risk"; included_risk="${sel#risk:}"
    case "$included_risk" in
      isolated|cluster-scoped-fault|uninstall) : ;;
      *) echo "unknown risk selection: $included_risk (isolated|cluster-scoped-fault|uninstall; --list ile yüzeyi gör)" >&2; exit 2 ;;
    esac ;;
  *)
    selection="single"; case_name="${1}"
    case "$case_name" in
      [a-z0-9][a-z0-9-]*) : ;;
      *) echo "unknown case: $case_name (--list ile yüzeyi gör)" >&2; exit 2 ;;
    esac ;;
esac

# ---- execution: explicit live env is required, rejected before dispatch ----
# No silent fallback to a default target/context/namespace: anything missing is
# rejected rc=2 BEFORE any case is executed (zero mutation on rejection).
# KUBE_BIN has a documented default (kubectl); the grant is risk-scoped.
cases_dir="$SCENARIO_ROOT/cases"
if [ "$selection" = "single" ] && { [ ! -d "$cases_dir" ] || [ ! -f "$cases_dir/$case_name/case.json" ]; }; then
  echo "unknown case: $case_name (--list ile yüzeyi gör)" >&2
  exit 2
fi

KUBE_BIN="${KUBE_BIN:-kubectl}"
export KUBE_BIN
case "$KUBE_BIN" in
  kubectl|oc) : ;;
  *) echo "KUBE_BIN yalnız kubectl|oc" >&2; exit 2 ;;
esac
for env_var in KUBE_CONTEXT TEST_NAMESPACE ALLOW_LIVE; do
  if [ -z "${!env_var:-}" ]; then
    echo "live env eksik: $env_var — execution için explicit gerek (sessiz default yok); --list discovery-only" >&2
    exit 2
  fi
done

# ALLOW_LIVE is a risk-scoped grant matching the selection's risk tier:
# all -> allow-isolated, risk:X -> allow-X, single -> allow-<that case's risk>.
case "$selection" in
  all)    required_grant="allow-isolated" ;;
  risk)   required_grant="allow-$included_risk" ;;
  single)
    grant_parsed=""
    if ! grant_parsed="$(_scenario_parse_case_json "$cases_dir/$case_name/case.json" 2>/dev/null)"; then
      echo "case metadata okunamadı: $case_name (--list ile doğrula)" >&2
      exit 2
    fi
    grant_f=()
    mapfile -t grant_f <<< "$grant_parsed"
 # Grant is effect-based: a cluster-scoped effect can never ride an
 # isolated grant, whatever the risk field claims (protocol pin 1).
    required_grant="allow-$(_scenario_effect_tier "${grant_f[3]}" "${grant_f[4]}")" ;;
esac
if [ "$ALLOW_LIVE" != "$required_grant" ]; then
  echo "ALLOW_LIVE='$ALLOW_LIVE' bu seçim ($sel) için risk grant'ı değil — gerekli: $required_grant" >&2
  exit 2
fi

# ---- selection (metadata only — case code is NOT executed here) ------------
selected=()
excluded=()
meta_rejected=()
agg_fail=0 ; agg_blocked=0 ; agg_skip=0
if [ "$selection" != "single" ]; then
  [ -d "$cases_dir" ] || { echo "cases dizini yok: $cases_dir (henüz case yok)" >&2; exit 2; }
  for cj in "$cases_dir"/*/case.json; do
    [ -f "$cj" ] || continue
    cdir_name="$(basename "$(dirname "$cj")")"
    parsed=""
    if ! parsed="$(_scenario_parse_case_json "$cj" 2>/dev/null)"; then
      meta_rejected+=("$cdir_name")
      continue
    fi
    sel_f=()
    mapfile -t sel_f <<< "$parsed"
    sel_ok=0
    case "${sel_f[3]}" in
      isolated|cluster-scoped-fault|uninstall) sel_ok=1 ;;
    esac
 # Same risk/scope consistency preflight enforces — a case the gate cannot
 # authorize is never dispatched from a sweep (fail-closed classification).
    sel_consistent=1
    if [ "${sel_f[3]}" != "isolated" ] && [ "${sel_f[4]}" != "cluster-scoped" ]; then
      sel_consistent=0
    fi
    if [ "${sel_f[0]}" != "$cdir_name" ] || [ "$sel_ok" -eq 0 ] || [ "$sel_consistent" -eq 0 ]; then
      meta_rejected+=("$cdir_name")
      continue
    fi
 # EFFECT-based classification (protocol pin 1): selection follows what
 # the case touches (scope), never its self-declared risk alone. `all` /
 # allow-isolated therefore cannot pick a singleton-lifecycle case even if
 # its case.json says risk=isolated.
    sel_effect="$(_scenario_effect_tier "${sel_f[3]}" "${sel_f[4]}")"
    case "$selection" in
      all)
        if [ "$sel_effect" = "isolated" ]; then
          selected+=("$cdir_name")
        else
          excluded+=("$cdir_name:risk=${sel_f[3]} effect=$sel_effect")
        fi ;;
      risk)
        [ "$sel_effect" = "$included_risk" ] && selected+=("$cdir_name") ;;
    esac
  done
 # A healthy sweep that matches nothing is a configuration error (rc=2,
 # zero mutation) — the broken-sweep verdict below is separate: an
 # UNVALIDATED sweep is FAIL-CLOSED (rc=1), and that verdict is taken
 # BEFORE the first dispatch (see after the evidence setup).
 # (A sweep with NO healthy matches but broken metadata is NOT "no cases
 # found": the broken-sweep FAIL-CLOSED verdict below takes precedence.)
  if [ ${#selected[@]} -eq 0 ] && [ ${#meta_rejected[@]} -eq 0 ]; then
    echo "selection eşleşen case bulamadı ($sel; risk_scope=$included_risk) — --list ile doğrula" >&2
    exit 2
  fi
fi

# ---- run identity + evidence ------------------------------------------------
run_id="scenario-$(date +%Y%m%d-%H%M%S)-$$"
export RUN_ID="$run_id"
# Evidence immutability: an EXPLICITLY populated EVIDENCE_DIR belongs to an
# earlier run — run-meta/run-identity/case.log must never be overwritten on
# top of it. Reuse is rejected rc=2 BEFORE the first mutation (and before
# this run writes anything). An explicit-but-empty dir is honored; the
# default (run-id stamped) path is always fresh.
evidence_was_explicit=0
[ -n "${EVIDENCE_DIR:-}" ] && evidence_was_explicit=1
: "${EVIDENCE_DIR:=$TMPDIR/quartet-scenario-$run_id}"
export EVIDENCE_DIR
if [ "$evidence_was_explicit" -eq 1 ] && [ -d "$EVIDENCE_DIR" ] \
   && [ -n "$(ls -A "$EVIDENCE_DIR" 2>/dev/null)" ]; then
  echo "EVIDENCE_DIR='$EVIDENCE_DIR' dolu — önceki run'ın kanıtı (run-meta/run-identity/case.log) üzerine yazılmaz; run reddedildi, mutation yok (yeni/boş bir dizin verin)" >&2
  exit 2
fi
mkdir -p "$EVIDENCE_DIR"
{
  echo "run_id=$run_id"
  echo "source_revision=$(scenario_src_revision)"
  echo "selection=$sel risk_scope=$included_risk"
  echo "grant=$ALLOW_LIVE"
  echo "kube_bin=$KUBE_BIN kube_context=$KUBE_CONTEXT test_namespace=$TEST_NAMESPACE"
  echo "excluded=${excluded[*]:-none}"
  echo "meta_rejected=${meta_rejected[*]:-none}"
} > "$EVIDENCE_DIR/run-meta"

# Export the LIVE contracted helper surface (and the globals its bodies read)
# into each case process: case bodies that call `declare -F scenario_preflight`
# (plus the two atomic-cleanup helpers) find the live lib and never fall back
# to offline stubs; bodies that source lib/ directly get the same functions.
# Either convention drops in unmodified. The FULL contracted surface a case
# body may call is exactly: scenario_preflight, scenario_kube,
# scenario_create_tracked, scenario_delete_owned — everything else here is a
# lib internal those functions rely on.
export REPO_ROOT SCENARIO_ROOT TMPDIR
export -f scenario_preflight scenario_kube scenario_create_tracked scenario_delete_owned \
  _scenario_parse_case_json scenario_src_revision _scenario_effect_tier scenario_kube_target_flag \
  _scenario_kind_is_cluster_scoped _scenario_cleanup_ns _scenario_ownership_row _scenario_json_uid

for e in "${excluded[@]:-}"; do
  [ -n "$e" ] && echo "risk-scope: $sel altında otomatik devreye GİRMEDİ $e"
done

# METADATA FAIL-CLOSED: the WHOLE selection is validated before the first
# dispatch. A swept selection containing even one broken-metadata case never
# dispatches ANY case mutation — not the healthy ones either (a partially
# executed sweep is an unverifiable run and can never PASS): the verdict is
# recorded in run-meta and the run exits rc=1 with zero case dispatch.
if [ ${#meta_rejected[@]} -gt 0 ]; then
  for e in "${meta_rejected[@]}"; do
    echo "metadata FAIL-CLOSED: '$e' metadata doğrulanamadı — sweep tam doğrulanamadı, ilk dispatch ÖNCESİ sıfır mutation ile reddedildi; bu run PASS olamaz" >&2
  done
  echo "scenario-runner result=FAIL run_id=$run_id cases=0 dispatched=0 evidence=$EVIDENCE_DIR"
  exit 1
fi

# ---- dispatch ----------------------------------------------------------------
# Evidence isolation (protocol pin 5): each dispatched case gets its OWN
# subdirectory under EVIDENCE_DIR with an immutable run-identity record and its
# own status/cleanup evidence — no shared generic files colliding between cases
# or runs.
if [ "$selection" = "single" ]; then
  selected=("$case_name")
fi
for cname in "${selected[@]}"; do
  rc=0
  case_ev="$EVIDENCE_DIR/$cname"
  mkdir -p "$case_ev"
  {
    echo "run_id=$run_id"
    echo "case=$cname"
    echo "source_revision=$(scenario_src_revision)"
    echo "selection=$sel grant=$ALLOW_LIVE"
    echo "kube_bin=$KUBE_BIN kube_context=$KUBE_CONTEXT test_namespace=$TEST_NAMESPACE"
    echo "dispatched_utc=$(date -u +%Y%m%dT%H%M%SZ)"
  } > "$case_ev/run-identity"
  ( cd "$SCENARIO_ROOT" && EVIDENCE_DIR="$case_ev" bash "cases/$cname/case.sh" ) > "$case_ev/case.log" 2>&1 || rc=$?
 # Status is keyed off the exit-code protocol (0 PASS, 1 FAIL, 2 BLOCKED,
 # 3 SKIP, 4 FAIL_CLEANUP) — every case convention follows it. A case that
 # additionally writes <name>.status must AGREE with its exit code; a missing
 # status file is NOT a failure (linear-body cases write result.txt instead).
  case "$rc" in
    0) final=PASS ;;
    1) final=FAIL ;;
    2) final=BLOCKED ;;
    3) final=SKIP ;;
    4) final=FAIL_CLEANUP ;;
    *) final=FAIL ;;   # crash/signal/protocol dışı rc — fail-closed
  esac
  if [ -s "$case_ev/$cname.status" ]; then
    status_final="$(sed -n 's/^final=//p' "$case_ev/$cname.status" | head -n1)"
    if [ -n "$status_final" ] && [ "$status_final" != "$final" ]; then
      echo "case=$cname exit code diyor final=$final, status dosyası diyor final=$status_final — fail-closed FAIL" >&2
      final=FAIL
    fi
  fi
  echo "case=$cname final=$final rc=$rc"
  case "$final" in
    FAIL|FAIL_CLEANUP) agg_fail=1 ;;
    BLOCKED) agg_blocked=1 ;;
    SKIP) agg_skip=1 ;;
  esac
done

# (A sweep that carried broken metadata never reaches dispatch: it already
# exited rc=1 FAIL-CLOSED with zero mutation — see the gate above.)
result=PASS
[ "$agg_fail" -eq 1 ] && result=FAIL
[ "$agg_fail" -eq 0 ] && [ "$agg_blocked" -eq 1 ] && result=BLOCKED
[ "$agg_fail" -eq 0 ] && [ "$agg_blocked" -eq 0 ] && [ "$agg_skip" -eq 1 ] && result=SKIP
echo "scenario-runner result=$result run_id=$run_id cases=${#selected[@]} evidence=$EVIDENCE_DIR"
[ "$agg_fail" -eq 1 ] && exit 1
[ "$agg_blocked" -eq 1 ] && exit 2
[ "$agg_skip" -eq 1 ] && exit 3
exit 0
