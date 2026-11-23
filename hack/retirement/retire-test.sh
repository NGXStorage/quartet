#!/usr/bin/env bash
set -euo pipefail
# Forwarding wrapper — canonical suite lives in tests/; preserve args, env, exit status.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec bash "$root/tests/retirement/retire-test.sh" "$@"
