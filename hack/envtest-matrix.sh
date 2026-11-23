#!/usr/bin/env bash
# Layer-2 envtest version matrix runner (candidate range).
#
# Runs the FULL suite once per K8s version against that version's EXACT
# asset dir, verifies the asset binary's own `kube-apiserver --version`,
# and grades each run from `go test -json`.
#
# Grading contract (per-version grading contract):
# PASS go exit 0, named-test fail=0, named-test skip=0,
# named-test pass >= $ENVTEST_MATRIX_MIN_PASS (default 150 —
# a green run with zero tests is not success), AND at least
# one PASSING test matching the required envtest scope
# ($ENVTEST_MATRIX_REQUIRED_TESTS, default prefix TestEnvtest —
# unit tests alone never satisfy the API gate).
# DEGRADED go exit 0 but a NAMED TEST was skipped (test-level skip is
# never success; package-level "no test files" skips are
# reported separately and do not degrade).
# FAIL go exit non-zero, malformed JSON (strict policy: any
# non-JSON line FAILs the run), min-pass not met, required
# envtest scope absent, unparseable binary version, or binary
# minor mismatch.
# BLOCKED assets missing and the download attempt was not made
# (ENVTEST_MATRIX_NO_DOWNLOAD=1) or could not complete.
# Exit (required mode, the default): 0 only if EVERY version PASSes;
# 1 if any FAIL; 2 if any DEGRADED/BLOCKED without FAIL. A TSV row that
# is lost into the JSON twin (row count mismatch) is itself an error
# (exit 1) — every requested version has exactly one result row.
# --optional / ENVTEST_MATRIX_OPTIONAL=1: reporting mode — statuses are
# still recorded, exit 0 with a summary.
#
# Provenance: every row carries the requested version, the resolved asset
# dir, the binary's own `--version` string, requested-vs-resolved (exact
# only when the BINARY reports the full requested version; otherwise
# resolved:<binary-version> — never "exact" by directory name alone), the
# source commit + dirty state, and test/package counters. Evidence lives
# OUTSIDE the checkout.
#
# Scope honesty: envtest is kube-apiserver+etcd only — no kubelet, CSI
# mount, I/O or OCP admission. Per-version status is "API validated" at
# best; live validation is Layer 3.
#
# Usage: envtest-matrix.sh [--optional] [version ...] (default 1.30.0..1.37.0)
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}"
export GOCACHE="${GOCACHE:-$HOME/.cache/ngxstorage/gocache}"

optional=0
args=()
for a in "$@"; do
    case "$a" in
        --optional) optional=1 ;;
        *) args+=("$a") ;;
    esac
done
[ "${ENVTEST_MATRIX_OPTIONAL:-0}" = "1" ] && optional=1
versions=()
[ "${#args[@]}" -gt 0 ] && versions=("${args[@]}")
[ "${#versions[@]}" -eq 0 ] && versions=(1.30.0 1.31.0 1.32.0 1.33.0 1.34.0 1.35.0 1.36.0 1.37.0)
asset_root="${ENVTEST_ASSET_ROOT:-$HOME/.cache/ngxstorage/kubebuilder-assets/k8s}"
out_dir="${ENVTEST_MATRIX_OUT:-$HOME/.cache/ngxstorage/quartet-p05/envtest-matrix}"
min_pass="${ENVTEST_MATRIX_MIN_PASS:-150}"
required_tests="${ENVTEST_MATRIX_REQUIRED_TESTS:-TestEnvtest}"
no_download="${ENVTEST_MATRIX_NO_DOWNLOAD:-0}"
mkdir -p "$out_dir"
run_id="$(date +%Y%m%d-%H%M%S)-$$"
tsv="$out_dir/envtest-matrix-$run_id.tsv"

# Source snapshot identity attached to every run (build-args contract).
src_commit="$(git -C "$root" rev-parse HEAD 2>/dev/null || echo unknown)"
src_dirty="$(git -C "$root" status --porcelain 2>/dev/null | grep -q . && echo true || echo false)"

# Every status branch writes this ONE 12-column schema (row-count rule: the earlier
# BLOCKED/mismatch branches wrote 11 columns and were silently dropped
# from the JSON twin).
HDR=$'version\tassetDir\tbinaryVersion\trequestedResolved\tsourceCommit\tdirty\tstatus\tpass\tfail\tskip\tpkgSkip\tdetail'
printf '%s\n' "$HDR" > "$tsv"
emit_row() { # version dir binver match commit dirty status p f s ps detail
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9" "${10}" "${11}" "${12}" >> "$tsv"
}

overall_bad=0
overall_blocked=0

grade_json() { # json_path go_rc min_pass required_tests -> "status pass fail skip pkgSkip reason"
    python3 - "$1" "$2" "$3" "$4" <<'PY'
import json, sys
path, go_rc, min_pass, required = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
tp = tf = ts = ps = 0
required_hits = 0
malformed = 0
try:
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            r = json.loads(line)
        except Exception:
            malformed += 1  # strict policy: a broken stream fails the run
            continue
        act = r.get("Action")
        if act not in ("pass", "fail", "skip"):
            continue
        if r.get("Test"):
            if act == "pass":
                tp += 1
                if r["Test"].startswith(required):
                    required_hits += 1
            elif act == "fail":
                tf += 1
            else:
                ts += 1
        elif act == "skip":
            ps += 1  # package-level ("no test files") — reported, not degraded
except OSError:
    malformed = 1
reasons = []
if malformed:
    reasons.append("malformed go-test JSON line(s) (strict policy)")
if go_rc != 0:
    reasons.append(f"go test exited {go_rc}")
if ts > 0 and go_rc == 0 and malformed == 0:
    status = "DEGRADED"
    reasons.append("named test skipped")
elif required_hits == 0:
    status = "FAIL"
    reasons.append(f"required envtest scope '{required}' did not pass — unit tests alone never satisfy the API gate")
elif tp < min_pass:
    status = "FAIL"
    reasons.append(f"only {tp} named tests passed, want >= {min_pass}")
elif go_rc == 0 and malformed == 0:
    status = "PASS"
else:
    status = "FAIL"
print(status, tp, tf, ts, ps, "; ".join(reasons))
PY
}

# Sequential runs; each `go test` process owns its envtest control plane
# (its own etcd datadir and free ports).
for v in "${versions[@]}"; do
    minor="${v%.*}"
    dir="$asset_root/$v-linux-amd64"
    if [ ! -x "$dir/kube-apiserver" ] || [ ! -x "$dir/etcd" ]; then
        resolved=""
        if [ "$no_download" != "1" ]; then
            resolved="$(timeout 300 go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest \
                            use "$v" -p path --bin-dir "$asset_root" 2>/dev/null || true)"
        fi
        if [ -n "$resolved" ] && [ -x "$resolved/kube-apiserver" ] && [ -x "$resolved/etcd" ]; then
            dir="$resolved"
        else
            if [ "$no_download" = "1" ]; then dl_note="download skipped (ENVTEST_MATRIX_NO_DOWNLOAD=1)"; else dl_note="download unavailable"; fi
            emit_row "$v" "$dir" "-" "-" "$src_commit" "$src_dirty" BLOCKED - - - - "assets missing; $dl_note"
            echo "  $v BLOCKED ($dl_note)"
            overall_blocked=1
            continue
        fi
    fi

 # The BINARY's own version string is the authority (row-count rule): parse it,
 # compare minor, and record requested-vs-resolved from the parse —
 # never from the directory name.
    kv="$("$dir/kube-apiserver" --version 2>&1 | head -1 || true)"
    bin_ver="$(grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' <<< "$kv" | head -1 || true)"
    if [ -z "$bin_ver" ]; then
        emit_row "$v" "$dir" "$kv" "-" "$src_commit" "$src_dirty" FAIL - - - - "unparseable kube-apiserver --version"
        echo "  $v FAIL (unparseable binary version: $kv)"
        overall_bad=1
        continue
    fi
    case "$bin_ver" in
        *"$minor".*) : ;;
        *)
            emit_row "$v" "$dir" "$kv" "resolved:$bin_ver" "$src_commit" "$src_dirty" FAIL - - - - "binary minor does not match requested $v"
            echo "  $v FAIL (asset version mismatch: $kv)"
            overall_bad=1
            continue
            ;;
    esac
    if [ "$bin_ver" = "v$v" ]; then match="exact"; else match="resolved:$bin_ver"; fi

    jsonf="$out_dir/go-test-$v-$run_id.json"
    errf="$out_dir/go-test-$v-$run_id.err"
    go_rc=0
    (cd "$root" && KUBEBUILDER_ASSETS="$dir" QUARTET_TEST_ENVTEST=required \
        go test -race -count=1 -json ./... > "$jsonf" 2> "$errf") || go_rc=$?

    read -r status np nf ns nps reason <<< "$(grade_json "$jsonf" "$go_rc" "$min_pass" "$required_tests")"
    emit_row "$v" "$dir" "$kv" "$match" "$src_commit" "$src_dirty" \
        "$status" "$np" "$nf" "$ns" "$nps" "${reason:-API validated only}"
    echo "  $v $status  $np/$nf/$ns (pkg-skip $nps)  [$match]  $reason"
    case "$status" in
        FAIL) overall_bad=1 ;;
        DEGRADED|BLOCKED) overall_blocked=1 ;;
    esac
done

# Machine-readable twin. row-count rule: row count must equal the requested count —
# a dropped row (schema drift) is an error, not silent loss.
n_rows=$(( $(wc -l < "$tsv") - 1 ))
if [ "$n_rows" -ne "${#versions[@]}" ]; then
    echo "FATAL: TSV has $n_rows result rows for ${#versions[@]} requested versions — schema drift; refusing to publish the JSON twin" >&2
    exit 1
fi
python3 - "$tsv" "$out_dir/envtest-matrix-$run_id.json" "$run_id" "$src_commit" "$src_dirty" "$optional" "$required_tests" <<'PY'
import json, sys
tsv, out, run_id, commit, dirty, optional, required = sys.argv[1:8]
rows = []
with open(tsv) as f:
    hdr = f.readline().rstrip("\n").split("\t")
    for line in f:
        p = line.rstrip("\n").split("\t")
        if len(p) != len(hdr):
            sys.exit(f"row lost/mangled in TSV: {p!r}")
        rows.append(dict(zip(hdr, p)))
json.dump({
    "runId": run_id,
    "optional": optional == "1",
    "scope": "envtest = kube-apiserver+etcd only; no kubelet/CSI mount/IO/OCP admission",
    "minPass": "green run requires >= MIN_PASS named tests",
    "requiredEnvtestPrefix": required,
    "requiredEnvtestRule": "unit tests alone never satisfy the API gate",
    "source": {"commit": commit, "dirty": dirty == "true"},
    "versions": rows,
}, open(out, "w"), indent=2)
PY

summary="$out_dir/envtest-matrix-$run_id.summary"
{
    echo "runId=$run_id optional=$optional minPass=$min_pass requiredEnvtest=$required_tests"
    echo "requested=${#versions[@]} rows=$n_rows"
    echo "PASS=$(grep -c $'\tPASS\t' "$tsv" || true) DEGRADED=$(grep -c $'\tDEGRADED\t' "$tsv" || true) FAIL=$(grep -c $'\tFAIL\t' "$tsv" || true) BLOCKED=$(grep -c $'\tBLOCKED\t' "$tsv" || true)"
} > "$summary"
echo "envtest-matrix: results at $tsv (and $run_id.json twin, $summary)"
echo "scope: per-version status is 'API validated' at best (layer 2); live validation is Layer 3."

if [ "$optional" = "1" ]; then
    echo "optional mode: statuses recorded, exit 0 by contract"
    exit 0
fi
if [ "$overall_bad" -eq 1 ]; then exit 1; fi
if [ "$overall_blocked" -eq 1 ]; then exit 2; fi
exit 0
