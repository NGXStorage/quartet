#!/usr/bin/env bash
# test-entrypoint-contract.sh — offline contract for the tests/ entry points.
#
# Verifies, WITHOUT re-running the heavy `go test` or the real suites:
# (1) discovery (--help/--list) is side-effect-free — no go/kubectl/oc call;
# (2) dispatch is behavior-correct — each leaf called exactly once, no live
# (kubectl/oc) call, arg/env propagation to the matrix runner;
# (3) exit-code propagation — a failing leaf is not swallowed (FAIL=1);
# (4) unknown selection is rejected (rc=2) and A has no live path.
#
# Dispatch is proven by a scratch fixture that copies the REAL run-all-tests.sh
# and run-unit-tests.sh into a same-relative-layout tree, with recording STUBS
# for the seven leaf suites, the matrix runner, and go/kubectl/oc. Asserting by
# grepping filenames out of the entry point sources is NOT dispatch proof; this
# fixture observes what the stubs record. The fixture never points back at the
# canonical real suites (recursion-free).
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"; mkdir -p "$cache"
fx="$(mktemp -d "$cache/quartet-contract-fixture.XXXXXX")"
trap 'rm -rf "$fx"' EXIT   # ephemeral test-scratch cleanup (accepted pattern)

ev="$fx/events.log"; ctl="$fx/ctl"; mkdir -p "$ctl" "$fx/bin" "$fx/tests/retirement" "$fx/hack"
SUITES_TOP="build-args install-artifacts envtest-matrix"
SUITES_RET="retire inventory execution-manifest closure-check"

fail=0
assert() { # $1 = message
    if eval "$2"; then echo "OK  $1"; else echo "FAIL $1"; fail=1; fi
}

# Recording tool stubs (go/kubectl/oc) are created BEFORE the discovery checks
# so a side effect smuggled into --help/--list is actually captured. An empty
# event log with no recorders present is NOT proof of "no call". Each stub just
# records the invocation and exits 0 — a discovery path that calls any of them
# leaves a trace and fails the assertion.
setup_recorders() {
    : > "$ev"; rm -rf "$ctl"; mkdir -p "$ctl"
    for t in go kubectl oc; do
        printf '%s\n' '#!/bin/bash' "echo \"$t \$*\" >> \"$ev\"" 'exit 0' > "$fx/bin/$t"
        chmod +x "$fx/bin/$t"
    done
}

# Build a fresh fixture (real entrypoints + recording stubs) into $fx with an
# empty event log. Control a specific leaf's exit code via $ctl/<stem>.
setup_fixture() {
    : > "$ev"
    rm -rf "$ctl"; mkdir -p "$ctl"
    cp "$root/tests/run-all-tests.sh" "$fx/tests/run-all-tests.sh"
    cp "$root/tests/run-unit-tests.sh" "$fx/tests/run-unit-tests.sh"
 # stub contract (avoid recursion): record + exit 0
    printf '%s\n' '#!/bin/bash' "echo 'contract' >> \"$ev\"" 'exit 0' \
        > "$fx/tests/test-entrypoint-contract.sh"; chmod +x "$fx/tests/test-entrypoint-contract.sh"
    for s in $SUITES_TOP; do
        printf '%s\n' '#!/bin/bash' "echo \"suite:$s\" >> \"$ev\"" \
            "[ -f \"$ctl/$s\" ] && exit 1" 'exit 0' \
            > "$fx/tests/$s-test.sh"; chmod +x "$fx/tests/$s-test.sh"
    done
    for s in $SUITES_RET; do
        printf '%s\n' '#!/bin/bash' "echo \"suite:$s\" >> \"$ev\"" \
            "[ -f \"$ctl/$s\" ] && exit 1" 'exit 0' \
            > "$fx/tests/retirement/$s-test.sh"; chmod +x "$fx/tests/retirement/$s-test.sh"
    done
 # matrix runner stub: records args + whether NO_DOWNLOAD was exported
    printf '%s\n' '#!/bin/bash' "echo \"matrix \$*\" >> \"$ev\"" \
        "echo \"matrix-nodownload=\${ENVTEST_MATRIX_NO_DOWNLOAD:-unset}\" >> \"$ev\"" 'exit 2' \
        > "$fx/hack/envtest-matrix.sh"; chmod +x "$fx/hack/envtest-matrix.sh"
 # recording kubectl/oc stubs (live must never be called)
    for t in kubectl oc; do
        printf '%s\n' '#!/bin/bash' "echo \"$t \$*\" >> \"$ev\"" 'exit 0' \
            > "$fx/bin/$t"; chmod +x "$fx/bin/$t"
    done
 # recording go stub: `go test ... -json` emits a SYNTHETIC stream chosen by
 # $ctl/go-mode (pass | skip-envtest) so the real grader in run-unit-tests.sh
 # is exercised without a heavy real go test. Other go invocations emit
 # nothing (e.g. go vet).
    cat > "$fx/bin/go" <<EOF
#!/bin/bash
echo "go \$*" >> "$ev"
case "\$*" in
  *-json*)
    mode=\$(cat "$ctl/go-mode" 2>/dev/null || echo pass)
    case "\$mode" in
      skip-envtest)
 # Partial envtest scope: a plane-independent TestEnvtest* passes while
 # another SKIPS (assets missing) — a successful subset with the API gate
 # not proven -> DEGRADED, never a plain PASS.
        printf '{"Action":"pass","Package":"p","Test":"TestUnit"}\n'
        printf '{"Action":"pass","Package":"p","Test":"TestEnvtestPlaneFree"}\n'
        printf '{"Action":"skip","Package":"p","Test":"TestEnvtestNeedsPlane"}\n'
        ;;
      skip-ordinary)
 # Envtest scope fully passes, but an ORDINARY named test skips -> still
 # DEGRADED (any named skip is not full coverage).
        printf '{"Action":"pass","Package":"p","Test":"TestEnvtestNeedsPlane"}\n'
        printf '{"Action":"skip","Package":"p","Test":"TestOrdinaryFeature"}\n'
        ;;
      skip-package)
 # Package-level "no test files" skip (no Test field): reported, does NOT
 # degrade. All named tests pass -> PASS.
        printf '{"Action":"pass","Package":"p","Test":"TestUnit"}\n'
        printf '{"Action":"pass","Package":"p","Test":"TestEnvtestNeedsPlane"}\n'
        printf '{"Action":"skip","Package":"empty-pkg"}\n'
        ;;
      *)
        printf '{"Action":"pass","Package":"p","Test":"TestUnit"}\n'
        printf '{"Action":"pass","Package":"p","Test":"TestEnvtestNeedsPlane"}\n'
        ;;
    esac
    ;;
esac
exit 0
EOF
    chmod +x "$fx/bin/go"
}

# ---- (1) discovery is side-effect-free (REAL entrypoints + recording stubs) --
# Recorders exist BEFORE the first discovery call; every discovery invocation
# runs under the same controlled PATH so a smuggled go/kubectl/oc call is caught.
setup_recorders
( cd "$root" && PATH="$fx/bin:$PATH" bash tests/run-all-tests.sh --help >/dev/null )
( cd "$root" && PATH="$fx/bin:$PATH" bash tests/run-all-tests.sh --list >/dev/null )
( cd "$root" && PATH="$fx/bin:$PATH" bash tests/run-unit-tests.sh --list >/dev/null )
assert "discovery (--help/--list) triggers no go/kubectl/oc call" '[ ! -s "$ev" ]'
# surface strings are emitted (still under the recording PATH)
assert "run-all --list surfaces local/matrix/--list/--help" \
    'PATH="$fx/bin:$PATH" bash "$root/tests/run-all-tests.sh" --list | grep -q -- "--list" && PATH="$fx/bin:$PATH" bash "$root/tests/run-all-tests.sh" --list | grep -q "matrix"'

# ---- (2) dispatch: each leaf once, no live, arg/env to matrix -------------
setup_fixture
( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-all-tests.sh" local >/dev/null 2>&1 )
assert "dispatch: exactly one go vet"        '[ "$(grep -c "^go vet" "$ev")" = "1" ]'
assert "dispatch: exactly one go test"       '[ "$(grep -c "^go test" "$ev")" = "1" ]'
for s in $SUITES_TOP $SUITES_RET; do
    assert "dispatch: suite $s exactly once" "[ \"\$(grep -c \"suite:$s\" \"$ev\")\" = \"1\" ]"
done
assert "dispatch: stub contract called once" '[ "$(grep -c "^contract$" "$ev")" = "1" ]'
assert "dispatch: NO live (kubectl/oc zero)" '[ "$(grep -cE "^kubectl|^oc " "$ev")" = "0" ]'

setup_fixture
( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-all-tests.sh" matrix 1.99.9 >/dev/null 2>&1 )
assert "matrix dispatch: version arg propagated"      'grep -q "^matrix 1.99.9$" "$ev"'
assert "matrix dispatch: ENVTEST_MATRIX_NO_DOWNLOAD=1 exported" 'grep -q "^matrix-nodownload=1$" "$ev"'

# ---- (3) exit-code propagation: a failing leaf is not swallowed -----------
setup_fixture
touch "$ctl/build-args"
loc_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-all-tests.sh" local >/dev/null 2>&1 ) || loc_rc=$?
assert "a failing leaf surfaces as FAIL (local rc=1)" '[ "$loc_rc" = "1" ]'

# ---- (3b) grader: a successful subset that is not full coverage is DEGRADED,
# never a plain PASS; a package-level "no test files" skip does not degrade ---
setup_fixture; echo pass > "$ctl/go-mode"
gr_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-unit-tests.sh" >/dev/null 2>&1 ) || gr_rc=$?
assert "grader: all proven -> PASS (rc=0)" '[ "$gr_rc" = "0" ]'
setup_fixture; echo skip-envtest > "$ctl/go-mode"
gr_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-unit-tests.sh" >/dev/null 2>&1 ) || gr_rc=$?
assert "grader: envtest scope skipped -> DEGRADED (rc=2)" '[ "$gr_rc" = "2" ]'
setup_fixture; echo skip-ordinary > "$ctl/go-mode"
gr_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-unit-tests.sh" >/dev/null 2>&1 ) || gr_rc=$?
assert "grader: ordinary named skip -> DEGRADED (rc=2), not a plain PASS" '[ "$gr_rc" = "2" ]'
setup_fixture; echo skip-package > "$ctl/go-mode"
gr_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-unit-tests.sh" >/dev/null 2>&1 ) || gr_rc=$?
assert "grader: package-level skip only -> PASS (rc=0)" '[ "$gr_rc" = "0" ]'

# ---- (3c) grader crash / no output is fail-closed, never a plain PASS -----
setup_fixture; echo pass > "$ctl/go-mode"
printf '%s\n' '#!/bin/bash' 'exit 3' > "$fx/bin/python3"; chmod +x "$fx/bin/python3"
gr_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-unit-tests.sh" >/dev/null 2>&1 ) || gr_rc=$?
assert "grader crash (python rc=3) -> FAIL (rc=1), not PASS" '[ "$gr_rc" = "1" ]'
rm -f "$fx/bin/python3"
setup_fixture; echo pass > "$ctl/go-mode"
printf '%s\n' '#!/bin/bash' 'exit 0' > "$fx/bin/python3"; chmod +x "$fx/bin/python3"
gr_rc=0; ( cd "$root" && PATH="$fx/bin:$PATH" bash "$fx/tests/run-unit-tests.sh" >/dev/null 2>&1 ) || gr_rc=$?
assert "grader no output (python rc=0) -> FAIL (rc=1), not PASS" '[ "$gr_rc" = "1" ]'
rm -f "$fx/bin/python3"

# ---- (4) unknown selection rejected; no live path in A --------------------
bog_rc=0; bog_err="$(bash "$root/tests/run-all-tests.sh" bogus 2>&1 >/dev/null)" || bog_rc=$?
assert "unknown selection rc=2" '[ "$bog_rc" = "2" ]'
assert "unknown selection message points to --list for the surface" \
    'grep -q -- "--list" <<<"$bog_err"'

if [ "$fail" -ne 0 ]; then
    echo "entrypoint contract FAIL" >&2
    exit 1
fi
echo "tests/test-entrypoint-contract.sh PASS (discovery side-effect-free; dispatch once/no-live/arg-env; FAIL propagates; unknown selection rc=2)"
