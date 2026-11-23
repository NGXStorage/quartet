#!/usr/bin/env bash
# Check Quartet's module pins, clean-cache build, and optional image labels.
set -euo pipefail

optional=false
case "${1:-}" in
  "") ;;
  --optional) optional=true ;;
  -h|--help) echo "Usage: $0 [--optional]"; exit 0 ;;
  *) echo "Usage: $0 [--optional]" >&2; exit 2 ;;
esac
[[ $# -le 1 ]] || exit 2

cd "$(dirname "${BASH_SOURCE[0]}")/.."
export TMPDIR="${TMPDIR:-$HOME/.cache/ngxstorage}" GOWORK=off
mkdir -p "$TMPDIR"
scratch=$(mktemp -d "$TMPDIR/quartet-provenance.XXXXXXXX")
cleanup() {
  if [[ -d "$scratch/mod" ]]; then
    GOMODCACHE="$scratch/mod" go clean -modcache
  fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT
image=${IMAGE:-quay.io/ngxstorage/quartet-operator:v0.0.0-dev}
revision=$(git rev-parse HEAD)
status=$(git status --porcelain --untracked-files=all)
blocked=0
block() {
  echo "provenance-check: BLOCKED — $*"
  blocked=$((blocked + 1))
}

go mod edit -json | python3 -c '
import json, sys

mod = json.load(sys.stdin)
required = {m["Path"]: m["Version"] for m in mod["Require"]}
replaced = {m["Old"]["Path"]: m["New"] for m in mod.get("Replace", [])}
if any(not new.get("Version") for new in replaced.values()):
    sys.exit("Quartet dependencies must resolve without a workspace checkout")
if required.get("github.com/ngxstorage/go-logutils") != "v1.0.1":
    sys.exit("go-logutils pin differs from the reviewed image dependency")
if required.get("go.uber.org/zap") != "v1.28.0" or "go.uber.org/zap" in replaced:
    sys.exit("The operator must use the upstream Zap v1.28.0 module")
'
GOPROXY=off go build -mod=vendor ./...
echo "provenance-check: offline vendor build passed"

if GOMODCACHE="$scratch/mod" go build -mod=readonly ./... >"$scratch/build.out" 2>&1; then
  echo "provenance-check: fresh module-cache build passed"
elif python3 - "$scratch/build.out" <<'PY'
import re, sys
from pathlib import Path

output = Path(sys.argv[1]).read_text()
sys.exit(not re.search(r"dial tcp|no such host|connection refused|TLS handshake|module lookup disabled|network is unreachable", output))
PY
then
  block "module network unavailable"
  tail -5 "$scratch/build.out"
else
  cat "$scratch/build.out" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  block "Docker daemon unavailable"
elif labels=$(docker image inspect "$image" --format '{{json .Config.Labels}}' 2>/dev/null); then
  python3 - "$labels" "$revision" "$status" <<'PY'
import json, re, sys
from pathlib import Path

labels, revision, status = json.loads(sys.argv[1]), sys.argv[2], sys.argv[3]
dockerfile = Path("Dockerfile").read_text()
for key in ("org.opencontainers.image.revision", "vcs-ref"):
    if labels.get(key) != revision:
        sys.exit(f"{key} does not match the checkout revision")
match = re.search(r"^ARG GO_LOGUTILS_REF=([a-f0-9]{40})$", dockerfile, re.M)
if not match or labels.get("vcs-ref-dep-go-logutils") != match.group(1):
    sys.exit("vcs-ref-dep-go-logutils does not match the Dockerfile pin")
if status and labels.get("io.ngxstorage.source.dirty") == "false":
    sys.exit("image claims a clean source but the working tree is dirty")
PY
  echo "provenance-check: image labels verified"
else
  echo "provenance-check: no local image $image to inspect"
fi

if (( blocked > 0 )); then
  echo "provenance-check: $blocked check(s) blocked"
  [[ "$optional" == true ]] || exit 1
fi
echo "provenance-check: available checks passed"
