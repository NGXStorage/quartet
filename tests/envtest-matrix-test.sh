#!/usr/bin/env bash
# Offline regression for hack/envtest-matrix.sh (envtest grading, review).
# Runs the REAL runner against synthetic asset dirs and a `go` stub that
# replays fixture go-test JSON — no API server, no network, no real suite:
# - named test FAIL -> row FAIL, exit 1
# - named test SKIP -> DEGRADED, exit 2 (never PASS)
# - zero tests (pkg only)-> FAIL, exit 1 (min-pass gate)
# - malformed JSON -> FAIL, exit 1
# - missing assets -> BLOCKED, exit 2 (NO_DOWNLOAD)
# - wrong binary version -> FAIL, exit 1
# - package-level skip -> reported separately, does NOT degrade
# - known PASS -> PASS, exit 0
# - --optional -> statuses recorded, exit 0 by contract
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/envtest-matrix-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
runner="$root/hack/envtest-matrix.sh"
V=1.30.0
mkdir -p "$scratch/assets" "$scratch/out" "$scratch/bin"

# --- fake asset dir whose kube-apiserver reports the requested version ----
mkassets() { # $1 = dir, $2 = version string the binary claims
    mkdir -p "$1"
    printf '#!/bin/sh\necho "Kubernetes %s"\n' "$2" > "$1/kube-apiserver"
    printf '#!/bin/sh\nexit 0\n' > "$1/etcd"
    printf 'kubectl-stub\n' > "$1/kubectl"
    chmod +x "$1/kube-apiserver" "$1/etcd"
}

# --- `go` stub: replay fixture JSON for `go test`, refuse everything else -
cat > "$scratch/bin/go" <<'EOF'
#!/bin/sh
case "$1" in
    test)
        cat "$FAKE_GO_FIXTURE"
        exit "${FAKE_GO_RC:-0}"
        ;;
    *)
        echo "stub: refusing non-test go invocation: $*" >&2
        exit 255
        ;;
esac
EOF
chmod +x "$scratch/bin/go"

F="$scratch/fixtures"
mkdir -p "$F"
# PASS fixture: >= min_pass named passes, INCLUDING one required-scope
# TestEnvtest* pass, + one PACKAGE-level skip (no test files)
printf '%s\n' \
  '{"Time":"t","Action":"pass","Package":"p","Test":"TestEnvtestSuite"}' \
  '{"Time":"t","Action":"pass","Package":"p","Test":"TestUnitOne"}' \
  '{"Time":"t","Action":"pass","Package":"p","Test":"TestUnitTwo"}' \
  '{"Time":"t","Action":"skip","Package":"no-tests/pkg"}' > "$F/pass.json"
# unit-only: enough named passes but NO required-scope envtest test -> FAIL
printf '%s\n' \
  '{"Time":"t","Action":"pass","Package":"p","Test":"TestUnitOne"}' \
  '{"Time":"t","Action":"pass","Package":"p","Test":"TestUnitTwo"}' > "$F/unitonly.json"
# named FAIL + go exit 1
printf '%s\n' \
  '{"Time":"t","Action":"pass","Package":"p","Test":"TestOne"}' \
  '{"Time":"t","Action":"fail","Package":"p","Test":"TestBroken"}' > "$F/fail.json"
# named SKIP + go exit 0
printf '%s\n' \
  '{"Time":"t","Action":"skip","Package":"p","Test":"TestSkippedEnvtest"}' > "$F/skip.json"
# zero named tests (package skip only) + go exit 0
printf '%s\n' '{"Time":"t","Action":"skip","Package":"no-tests/pkg"}' > "$F/zero.json"
# malformed JSON + go exit 0
printf 'not json at all\n{broken\n' > "$F/malformed.json"

expect_status() { # $1=fixture $2=go_rc $3=want-status $4=want-exit
    mkassets "$scratch/assets/$V-linux-amd64" "v$V"
    rc=0
    out="$(FAKE_GO_FIXTURE="$1" FAKE_GO_RC="$2" env PATH="$scratch/bin:$PATH" \
        ENVTEST_ASSET_ROOT="$scratch/assets" ENVTEST_MATRIX_OUT="$scratch/out" \
        ENVTEST_MATRIX_NO_DOWNLOAD=1 ENVTEST_MATRIX_MIN_PASS=2 \
        TMPDIR="$cache" bash "$runner" "$V" 2>&1)" || rc=$?
    grep -q "$V $3" <<< "$out" || { echo "FAIL: want status $3, got: $out" >&2; return 1; }
    [ "$rc" -eq "$4" ] || { echo "FAIL: want exit $4, got $rc (out: $out)" >&2; return 1; }
}

expect_status "$F/pass.json"      0 PASS     0
expect_status "$F/fail.json"      1 FAIL     1
expect_status "$F/skip.json"      0 DEGRADED 2
expect_status "$F/zero.json"      0 FAIL     1
expect_status "$F/malformed.json" 0 FAIL     1
# row-count rule: enough unit tests but no required-scope envtest test is NOT a pass.
expect_status "$F/unitonly.json"  0 FAIL     1

# BLOCKED: no asset dir, download not attempted — and the JSON twin must
# still carry a row for it (row-count rule: row count == requested count).
rm -rf "$scratch/assets/$V-linux-amd64"
rc=0
out="$(FAKE_GO_FIXTURE="$F/pass.json" env PATH="$scratch/bin:$PATH" \
    ENVTEST_ASSET_ROOT="$scratch/assets" ENVTEST_MATRIX_OUT="$scratch/out" \
    ENVTEST_MATRIX_NO_DOWNLOAD=1 ENVTEST_MATRIX_MIN_PASS=2 \
    TMPDIR="$cache" bash "$runner" "$V" 2>&1)" || rc=$?
grep -q "BLOCKED" <<< "$out" || { echo "FAIL: missing assets must be BLOCKED: $out" >&2; exit 1; }
[ "$rc" -eq 2 ] || { echo "FAIL: BLOCKED matrix must exit 2, got $rc" >&2; exit 1; }
blocked_tsv="$(grep -o "$scratch/out/envtest-matrix-[0-9-]*\.tsv" <<< "$out" | head -1)"
[ -n "$blocked_tsv" ] || { echo "FAIL: no TSV path in runner output: $out" >&2; exit 1; }
blocked_json="${blocked_tsv%.tsv}.json"
grep -q "\"BLOCKED\"" "$blocked_json" \
  || { echo "FAIL: BLOCKED row lost from the JSON twin: $blocked_json" >&2; exit 1; }

# row-count rule: same-minor wrong patch must be recorded as resolved:<ver>, never exact
mkassets "$scratch/assets/$V-linux-amd64" "v$V"
printf '#!/bin/sh\necho "Kubernetes v1.30.99"\n' > "$scratch/assets/$V-linux-amd64/kube-apiserver"
chmod +x "$scratch/assets/$V-linux-amd64/kube-apiserver"
rc=0
out="$(FAKE_GO_FIXTURE="$F/pass.json" FAKE_GO_RC=0 env PATH="$scratch/bin:$PATH" \
    ENVTEST_ASSET_ROOT="$scratch/assets" ENVTEST_MATRIX_OUT="$scratch/out" \
    ENVTEST_MATRIX_NO_DOWNLOAD=1 ENVTEST_MATRIX_MIN_PASS=2 \
    TMPDIR="$cache" bash "$runner" "$V" 2>&1)" || rc=$?
grep -q "resolved:v1.30.99" <<< "$out" \
  || { echo "FAIL: same-minor wrong patch must record resolved:<ver>: $out" >&2; exit 1; }
grep -q "\[exact\]" <<< "$out" \
  && { echo "FAIL: same-minor wrong patch must NOT be labeled exact: $out" >&2; exit 1; } || true

# wrong binary version
mkassets "$scratch/assets/$V-linux-amd64" "v$V"
printf '#!/bin/sh\necho "Kubernetes v1.31.0"\n' > "$scratch/assets/$V-linux-amd64/kube-apiserver"
chmod +x "$scratch/assets/$V-linux-amd64/kube-apiserver"
rc=0
out="$(FAKE_GO_FIXTURE="$F/pass.json" FAKE_GO_RC=0 env PATH="$scratch/bin:$PATH" \
    ENVTEST_ASSET_ROOT="$scratch/assets" ENVTEST_MATRIX_OUT="$scratch/out" \
    ENVTEST_MATRIX_NO_DOWNLOAD=1 ENVTEST_MATRIX_MIN_PASS=2 \
    TMPDIR="$cache" bash "$runner" "$V" 2>&1)" || rc=$?
grep -q "FAIL" <<< "$out" || { echo "FAIL: version mismatch must FAIL: $out" >&2; exit 1; }
[ "$rc" -eq 1 ] || { echo "FAIL: version mismatch must exit 1, got $rc" >&2; exit 1; }

# --optional: a FAIL row still records but exits 0 by contract
mkassets "$scratch/assets/$V-linux-amd64" "v$V"
rc=0
out="$(FAKE_GO_FIXTURE="$F/fail.json" FAKE_GO_RC=1 env PATH="$scratch/bin:$PATH" \
    ENVTEST_ASSET_ROOT="$scratch/assets" ENVTEST_MATRIX_OUT="$scratch/out" \
    ENVTEST_MATRIX_NO_DOWNLOAD=1 ENVTEST_MATRIX_MIN_PASS=2 \
    TMPDIR="$cache" bash "$runner" --optional "$V" 2>&1)" || rc=$?
grep -q "FAIL" <<< "$out" || { echo "FAIL: optional mode must still record FAIL: $out" >&2; exit 1; }
[ "$rc" -eq 0 ] || { echo "FAIL: optional mode exits 0, got $rc" >&2; exit 1; }

# package-level skip must not degrade a green run (covered by pass.json
# carrying a package skip yet grading PASS above) — assert the counter was
# reported separately in the last PASS run's TSV.
pass_tsv="$(ls "$scratch/out"/envtest-matrix-*.tsv | while read f; do grep -q $'\tPASS\t' "$f" && { echo "$f"; break; }; done)"
[ -n "$pass_tsv" ] || { echo "FAIL: no PASS run recorded in any matrix TSV" >&2; exit 1; }
# pkgSkip is column 11 (status=PASS, skip=0 but pkgSkip=1 => reported, not degraded)
awk -F'\t' '$7=="PASS" && $10==0 && $11==1 {found=1} END {exit found?0:1}' "$pass_tsv" \
  || { echo "FAIL: package-level skip not reported as a separate counter (pkgSkip col): $pass_tsv" >&2; exit 1; }

echo "envtest-matrix offline gate tests PASS (FAIL/SKIP/BLOCKED/zero/malformed/version-mismatch nonzero; pkg-skip separate; optional contract)"
