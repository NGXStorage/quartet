#!/usr/bin/env bash
# Offline tests for make-execution-manifest.py: the
# canonical execution manifest must keep PROTECTED out of DELETE, refuse
# Namespace/Secret deletions, refuse duplicate/incomplete rows, and carry
# apiVersion + snapshot identity on every row. Synthetic bad manifests are
# rejected; a generated one is accepted. No cluster, no live data.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/execution-manifest-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
gen="$root/hack/retirement/make-execution-manifest.py"
HDR=$'role\tapiVersion\tkind\tnamespace\tname\tuid\tresourceVersion\tsemanticSha256\tsnapshotId'

violate() { python3 -c "
import sys, importlib.util
spec = importlib.util.spec_from_file_location('m', '$gen')
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
v = m.validate_manifest(sys.argv[1])
[print(x) for x in v]
sys.exit(1 if v else 0)" "$1"; }

mk() { # $1=file $2...=rows
  local f="$1"; shift
  printf '%s\n' "$HDR" > "$f"
  while [ "$#" -gt 0 ]; do printf '%s\n' "$1" >> "$f"; shift; done
}

good_row=$'DELETE\tv1\tServiceAccount\tns\tsa-a\tu1\tr1\th1\tsnap'
prot_row=$'PROTECTED\tv1\tSecret\tns\tnfs-csi-ngxstorage-config\tu2\tr2\th2(data-excluded)\tsnap'

# --- positive: generated from a synthetic discovery inventory ------------
inv="$scratch/inv.tsv"
printf 'role\tkind\tnamespace\tname\tuid\tresourceVersion\tsemanticSha256\n' > "$inv"
printf 'ALLOWLIST\tsecret\tns\tnfs-csi-ngxstorage-config\tu2\tr2\th2(data-excluded)\n' >> "$inv"
printf 'ALLOWLIST\tconfigmap\tns\tnfs-csi-ngxstorage-coordination-config\tu9\tr9\th9\n' >> "$inv"
printf 'ALLOWLIST\tserviceaccount\tns\tsa-a\tu1\tr1\th1\n' >> "$inv"
printf 'ALLOWLIST\tsecret\tns\tsa-a-dockercfg-ab12c\tu10\tr10\th10\n' >> "$inv"
printf 'NEIGHBOR\tdeployment\tns\tiscsi-csi-ngxstorage-controller\tu3\tr3\th3\n' >> "$inv"
manifest="$scratch/manifest.tsv"
python3 "$gen" "$inv" "$manifest" >/dev/null
if violate "$manifest"; then echo "generated manifest validates"; else echo "FAIL: generated manifest rejected" >&2; exit 1; fi
grep -q $'^PROTECTED\tv1\tsecret\tns\tnfs-csi-ngxstorage-config' "$manifest" \
  || { echo "FAIL: Secret not relabeled PROTECTED" >&2; exit 1; }
grep -q $'^PROTECTED\tv1\tconfigmap\tns\tnfs-csi-ngxstorage-coordination-config' "$manifest" \
  || { echo "FAIL: ConfigMap not relabeled PROTECTED" >&2; exit 1; }
grep -q $'^DELETE\tv1\tsecret' "$manifest" \
  && { echo "FAIL: Secret still in DELETE" >&2; exit 1; } || true
grep -q $'^NEIGHBOR\tapps/v1\tdeployment\tns\tiscsi' "$manifest" \
  || { echo "FAIL: neighbor lost its apiVersion" >&2; exit 1; }
grep -q $'^PLATFORM-MANAGED\tv1\tsecret\tns\tsa-a-dockercfg-ab12c' "$manifest" \
  || { echo "FAIL: OCP dockercfg Secret not classified PLATFORM-MANAGED" >&2; exit 1; }

# --- negatives: each must be rejected ------------------------------------
mk "$scratch/bad-overlap.tsv" "$good_row" "$prot_row"
printf 'DELETE\tv1\tSecret\tns\tnfs-csi-ngxstorage-config\tu2\tr2\th2\tsnap\n' >> "$scratch/bad-overlap.tsv"
violate "$scratch/bad-overlap.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: PROTECTED∩DELETE accepted" >&2; exit 1; } || true

mk "$scratch/bad-ns.tsv" $'DELETE\tv1\tNamespace\tns\tngxstorage-nfs\tu\tr\th\tsnap'
violate "$scratch/bad-ns.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: Namespace deletion accepted" >&2; exit 1; } || true

mk "$scratch/bad-secret.tsv" $'DELETE\tv1\tSecret\tns\tsome-secret\tu\tr\th\tsnap'
violate "$scratch/bad-secret.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: arbitrary Secret deletion accepted" >&2; exit 1; } || true

mk "$scratch/bad-dup.tsv" "$good_row" "$good_row"
violate "$scratch/bad-dup.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: duplicate identity accepted" >&2; exit 1; } || true

mk "$scratch/bad-incomplete.tsv" 'DELETE\tv1\tServiceAccount\tns'
violate "$scratch/bad-incomplete.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: incomplete row accepted" >&2; exit 1; } || true

mk "$scratch/bad-header.tsv"
printf 'wrong\theader\n' >> "$scratch/bad-header.tsv"
violate "$scratch/bad-header.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: wrong header accepted" >&2; exit 1; } || true

# A DELETE of the protected ConfigMap with NO PROTECTED row must still be
# rejected — the protected-name policy is validated independently.
mk "$scratch/bad-protcm.tsv" \
  $'DELETE\tv1\tConfigMap\tns\tnfs-csi-ngxstorage-coordination-config\tu\tr\th\tsnap'
violate "$scratch/bad-protcm.tsv" >/dev/null 2>&1 \
  && { echo "FAIL: protected ConfigMap in DELETE accepted without PROTECTED row" >&2; exit 1; } || true

# --- the generator must fail-closed BEFORE publishing -------------------
# Any INVENTORY-ERROR, CONSUMER, unknown role, or malformed inventory row
# must make the generator exit non-zero and write NO manifest file — a
# flawed scan can never become a "valid empty manifest".
gen_fail() { # $1=inventory path, $2=output path (must not be created)
    local rc=0
    python3 "$gen" "$1" "$2" >/dev/null 2>&1 || rc=$?
    [ "$rc" -ne 0 ] || { echo "FAIL: generator accepted bad inventory $1" >&2; exit 1; }
    [ ! -e "$2" ] || { echo "FAIL: generator published a manifest from a bad inventory: $2" >&2; exit 1; }
}
INVHDR=$'role\tkind\tnamespace\tname\tuid\tresourceVersion\tsemanticSha256'
printf '%s\n' "$INVHDR" > "$scratch/inv-err.tsv"
printf 'INVENTORY-ERROR\tpvc\t-\t-\t-\t-\tscan-403\n' >> "$scratch/inv-err.tsv"
gen_fail "$scratch/inv-err.tsv" "$scratch/manifest-err.tsv"

printf '%s\n' "$INVHDR" > "$scratch/inv-consumer.tsv"
printf 'ALLOWLIST\tserviceaccount\tns\tsa-a\tu1\tr1\th1\n' >> "$scratch/inv-consumer.tsv"
printf 'CONSUMER-PV\tnfs.csi.ngxstorage.com\t-\tpv-active\tu2\tr2\th2\n' >> "$scratch/inv-consumer.tsv"
gen_fail "$scratch/inv-consumer.tsv" "$scratch/manifest-consumer.tsv"

printf '%s\n' "$INVHDR" > "$scratch/inv-unknownrole.tsv"
printf 'WHATEVER\tserviceaccount\tns\tsa-a\tu1\tr1\th1\n' >> "$scratch/inv-unknownrole.tsv"
gen_fail "$scratch/inv-unknownrole.tsv" "$scratch/manifest-unknown.tsv"

printf '%s\n' "$INVHDR" > "$scratch/inv-malformed.tsv"
printf 'ALLOWLIST\tserviceaccount\tns\tsa-a\tu1\n' >> "$scratch/inv-malformed.tsv"
gen_fail "$scratch/inv-malformed.tsv" "$scratch/manifest-malformed.tsv"

# An INTERNAL consumer (a CONSUMER-* row whose uid is itself in the
# ALLOWLIST — the stack's own RoleBinding -> SA) must not block; the
# external one above does.
printf '%s\n' "$INVHDR" > "$scratch/inv-intconsumer.tsv"
printf 'ALLOWLIST\trolebinding\tns\trb-self\tu1\tr1\th1\n' >> "$scratch/inv-intconsumer.tsv"
printf 'ALLOWLIST\tserviceaccount\tns\tsa-a\tu2\tr2\th2\n' >> "$scratch/inv-intconsumer.tsv"
printf 'CONSUMER-RB\trolebinding\tns\trb-self\tu1\tr1\t-\n' >> "$scratch/inv-intconsumer.tsv"
python3 "$gen" "$scratch/inv-intconsumer.tsv" "$scratch/manifest-intconsumer.tsv" >/dev/null \
  || { echo "FAIL: internal (self-referential) CONSUMER row must not block generation" >&2; exit 1; }

echo "execution-manifest offline tests PASS (PROTECTED separation + independent protected-name policy, namespace/secret refusal, duplicate/incomplete/header rejection, apiVersion+snapshot per row, generator fail-closed: error/consumer/unknown-role/malformed reject and publish nothing)"
