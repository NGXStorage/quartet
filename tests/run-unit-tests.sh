#!/usr/bin/env bash
# run-unit-tests.sh — Quartet offline `local` test orchestrator.
#
# Runs: go vet, go test -race -count=1 -json ./..., and the seven relocated
# offline suites. Produces a comprehensive PASS / DEGRADED / FAIL result that is
# DISTINCT from the plain `make test` contract (which exits 0 when envtest
# scope skips): a successful Go subset with the required envtest scope not
# proven is DEGRADED, never a plain PASS.
#
# No live cluster and no envtest asset download. envtest honors KUBEBUILDER_ASSETS:
# present -> QUARTET_TEST_ENVTEST=required (the TestEnvtest* API gate runs)
# absent -> QUARTET_TEST_ENVTEST=optional (envtest scope skips -> DEGRADED)
# This does NOT change what a direct `make test` / `go test ./...` invocation
# does; it only adds this runner's own grading on top.
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --help/--list are discovery-only: they emit the surface and return without
# running any test, download, or live call.
case "${1:-}" in
  --help|-h)
    echo "run-unit-tests.sh [--list]   offline local orchestrator"
    exit 0 ;;
  --list)
    echo "run-unit-tests (offline local) dispatches:"
    echo "  go vet ./..."
    echo "  go test -race -count=1 -json ./...   (envtest per KUBEBUILDER_ASSETS)"
    echo "  tests/build-args-test.sh"
    echo "  tests/install-artifacts-test.sh"
    echo "  tests/envtest-matrix-test.sh   (local fake suite; not the 8-version matrix)"
    echo "  tests/retirement/retire-test.sh"
    echo "  tests/retirement/inventory-test.sh"
    echo "  tests/retirement/execution-manifest-test.sh"
    echo "  tests/retirement/closure-check-test.sh"
    exit 0 ;;
  "") : ;;
  *)
    echo "unknown argument: $1 (no run arguments; use --list)" >&2
    exit 2 ;;
esac

cache="$HOME/.cache/ngxstorage"; mkdir -p "$cache"
export TMPDIR="$cache"
export GOCACHE="${GOCACHE:-$cache/gocache}"
out_dir="$cache/quartet-run-unit-$(date +%Y%m%d-%H%M%S)-$$"
mkdir -p "$out_dir"

# envtest mode from asset presence; never downloads either way.
if [ -n "${KUBEBUILDER_ASSETS:-}" ] && [ -x "${KUBEBUILDER_ASSETS}/kube-apiserver" ]; then
  export QUARTET_TEST_ENVTEST=required
else
  export QUARTET_TEST_ENVTEST=optional
fi

fail=0; degraded=0

( cd "$root" && go vet ./... ) > "$out_dir/vet.log" 2>&1 || { echo "go vet FAIL"; fail=1; }
( cd "$root" && go test -race -count=1 -json ./... ) > "$out_dir/go-test.json" 2>"$out_dir/go-test.err" \
  || { echo "go test FAIL"; fail=1; }

# Grade the go-test JSON stream. Exit: 0 = nothing failed, no named skip, and
# the required envtest scope proven; 2 = a named test/subtest was skipped OR the
# envtest scope not proven (degraded); 1 = a named test failed or the stream is
# malformed. Any other exit (crash/signal/127) is fail-closed FAIL below.
python3 - "$out_dir/go-test.json" "$QUARTET_TEST_ENVTEST" <<'PY' > "$out_dir/go-grade.txt" 2>&1
import json, sys
mode = sys.argv[2]
tp = tf = ts = malformed = req_pass = req_skip = 0
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    line = line.strip()
    if not line:
        continue
    try:
        r = json.loads(line)
    except Exception:
        malformed += 1
        continue
    if r.get("Action") not in ("pass", "fail", "skip"):
        continue
    if r.get("Test"):
        a = r["Action"]
        if a == "pass":
            tp += 1
            if r["Test"].startswith("TestEnvtest"):
                req_pass += 1
        elif a == "fail":
            tf += 1
        else:
            ts += 1
            if r["Test"].startswith("TestEnvtest"):
                req_skip += 1
print(f"envtestMode={mode} pass={tp} fail={tf} skip={ts} "
      f"requiredEnvtestPass={req_pass} requiredEnvtestSkip={req_skip} malformed={malformed}")
# PASS only when nothing failed AND every NAMED test/subtest passed AND the
# required TestEnvtest* API gate was proven. Any named SKIP — envtest or
# otherwise — means a successful-but-partial subset -> DEGRADED, never a plain
# PASS (package-level "no test files" skips do not degrade). Zero envtest
# passes means the API gate was absent -> DEGRADED. FAIL on any named failure or
# a malformed stream.
sys.exit(1 if (tf or malformed) else (2 if (ts > 0 or req_pass == 0) else 0))
PY
grade=$?
# A grader that crashes (signal/127/other rc) or produces no output is NOT a
# success — only the defined grade (0) means PASS. Fail-closed otherwise.
case "$grade" in
  0) : ;;
  1) fail=1 ;;
  2) degraded=1 ;;
  *) fail=1; echo "grader crashed or errored (python rc=$grade) — fail-closed" ;;
esac
[ -s "$out_dir/go-grade.txt" ] || { echo "grader produced no output — fail-closed"; fail=1; }

# The seven relocated offline suites, each recorded separately (log + real rc).
# A suite FAIL outranks DEGRADED: fail=1 once any suite or the go surface fails.
for s in build-args install-artifacts envtest-matrix; do
    rc=0; ( cd "$root" && bash "tests/${s}-test.sh" ) > "$out_dir/suite.$s.log" 2>&1 || rc=$?
    [ "$rc" -eq 0 ] || { echo "suite FAIL: tests/${s}-test.sh (rc=$rc)"; fail=1; }
done
for s in retire inventory execution-manifest closure-check; do
    rc=0; ( cd "$root" && bash "tests/retirement/${s}-test.sh" ) > "$out_dir/suite.retirement-$s.log" 2>&1 || rc=$?
    [ "$rc" -eq 0 ] || { echo "suite FAIL: tests/retirement/${s}-test.sh (rc=$rc)"; fail=1; }
done

echo "go-grade: $(cat "$out_dir/go-grade.txt")"
result=PASS
[ "$fail" -eq 1 ] && result=FAIL
[ "$fail" -eq 0 ] && [ "$degraded" -eq 1 ] && result=DEGRADED
echo "run-unit-tests result=$result out=$out_dir"
[ "$fail" -eq 1 ] && exit 1
[ "$degraded" -eq 1 ] && exit 2
exit 0
