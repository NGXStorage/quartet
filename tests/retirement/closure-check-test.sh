#!/usr/bin/env bash
# Offline negative tests for hack/retirement/closure-check.py (closure check review
# 24baabf). No cluster, no API server — fixture objects only. Acceptance
# cases: a RoleBinding roleRef/subjects reference that is absent, in the
# WRONG namespace, or points at a protected set member must MISS (the old
# bug read .spec, so these passed vacuously); initContainer, imagePull,
# projected and PVC references are checked too; a fully known set is OK;
# an incomplete manifest row is rejected.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/closure-check-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
chk="$root/hack/retirement/closure-check.py"

HDR=$'role\tapiVersion\tkind\tnamespace\tname\tuid\tresourceVersion\tsemanticSha256\tsnapshotId'

# --- allowlist: the canonical execution manifest -------------------------
# role/apiVersion/kind/namespace/name/uid/rv/hash/snapshotId
# DELETE+PROTECTED = the retirement set (refs extracted); every row is a
# resolvable target. The broken fixtures' objects are DELETE too, so their
# references are actually checked.
{
  printf '%s\n' "$HDR"
 # retirement set (ref-extracted)
  printf 'DELETE\trbac.authorization.k8s.io/v1\tRoleBinding\tns\trb2\tu\tr\th\tsnap\n'
  printf 'DELETE\trbac.authorization.k8s.io/v1\tRoleBinding\tns\trb-miss-role\tu\tr\th\tsnap\n'
  printf 'DELETE\trbac.authorization.k8s.io/v1\tRoleBinding\tns\trb-wrong-ns\tu\tr\th\tsnap\n'
  printf 'DELETE\trbac.authorization.k8s.io/v1\tRoleBinding\tns\trb-protected-secret-subj\tu\tr\th\tsnap\n'
  printf 'DELETE\trbac.authorization.k8s.io/v1\tClusterRoleBinding\t-\tcrb-ok\tu\tr\th\tsnap\n'
  printf 'DELETE\tapps/v1\tDeployment\tns\tdeploy-ok\tu\tr\th\tsnap\n'
  printf 'DELETE\tapps/v1\tDeployment\tns\tdeploy-missing\tu\tr\th\tsnap\n'
  printf 'DELETE\tv1\tServiceAccount\tns\tsa-ok\tu\tr\th\tsnap\n'
  printf 'DELETE\tv1\tServiceAccount\tns\tsa-secret-missing\tu\tr\th\tsnap\n'
 # resolvable targets (NEIGHBOR = resolution set only, not ref-checked)
  printf 'NEIGHBOR\trbac.authorization.k8s.io/v1\tRole\tns\trole-known\tu\tr\th\tsnap\n'
  printf 'NEIGHBOR\trbac.authorization.k8s.io/v1\tClusterRole\t-\tcr-known\tu\tr\th\tsnap\n'
  printf 'NEIGHBOR\tv1\tServiceAccount\tns\tsa-cluster-subj\tu\tr\th\tsnap\n'
  printf 'PROTECTED\tv1\tSecret\tns\tsecret-protected\tu\tr\th\tsnap\n'
  printf 'NEIGHBOR\tv1\tSecret\tns\tsecret-known\tu\tr\th\tsnap\n'
  printf 'NEIGHBOR\tv1\tConfigMap\tns\tcm-known\tu\tr\th\tsnap\n'
  printf 'NEIGHBOR\tv1\tPersistentVolumeClaim\tns\tpvc-known\tu\tr\th\tsnap\n'
} > "$scratch/allow.tsv"

# --- fixture where every reference resolves ------------------------------
cat > "$scratch/fixtures-ok.json" <<'EOF'
{
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb2": {
    "roleRef": {"kind": "Role", "name": "role-known", "apiGroup": "rbac.authorization.k8s.io"},
    "subjects": [{"kind": "ServiceAccount", "name": "sa-ok", "namespace": "ns"}]
  },
  "rbac.authorization.k8s.io/v1::ClusterRoleBinding::-::crb-ok": {
    "roleRef": {"kind": "ClusterRole", "name": "cr-known"},
    "subjects": [{"kind": "ServiceAccount", "name": "sa-cluster-subj", "namespace": "ns"}]
  },
  "apps/v1::Deployment::ns::deploy-ok": {
    "spec": {"template": {"spec": {
      "serviceAccountName": "sa-ok",
      "initContainers": [{"name": "i", "envFrom": [{"secretRef": {"name": "secret-known"}}]}],
      "containers": [{
        "name": "c",
        "env": [{"name": "E", "valueFrom": {"secretKeyRef": {"name": "secret-known", "key": "k"}}}],
        "envFrom": [{"configMapRef": {"name": "cm-known"}}]
      }],
      "volumes": [
        {"name": "p", "projected": {"sources": [{"secret": {"name": "secret-known"}}, {"configMap": {"name": "cm-known"}}]}},
        {"name": "d", "persistentVolumeClaim": {"claimName": "pvc-known"}}
      ]
    }}}
  },
  "v1::ServiceAccount::ns::sa-ok": {"secrets": []},
  "v1::Secret::ns::secret-protected": {},
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb-miss-role": {"roleRef": {"kind": "Role", "name": "role-known"}, "subjects": []},
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb-wrong-ns": {"roleRef": {"kind": "Role", "name": "role-known"}, "subjects": []},
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb-protected-secret-subj": {"roleRef": {"kind": "Role", "name": "role-known"}, "subjects": []},
  "apps/v1::Deployment::ns::deploy-missing": {"spec": {"template": {"spec": {"containers": []}}}},
  "v1::ServiceAccount::ns::sa-secret-missing": {"secrets": []}
}
EOF

# --- broken fixtures: every closure check acceptance negative ------------------------
cat > "$scratch/fixtures-broken.json" <<'EOF'
{
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb-miss-role": {
    "roleRef": {"kind": "Role", "name": "role-missing", "apiGroup": "rbac.authorization.k8s.io"},
    "subjects": [{"kind": "ServiceAccount", "name": "sa-ok", "namespace": "ns"}]
  },
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb-wrong-ns": {
    "roleRef": {"kind": "Role", "name": "role-known", "apiGroup": "rbac.authorization.k8s.io"},
    "subjects": [{"kind": "ServiceAccount", "name": "sa-ok", "namespace": "other-ns"}]
  },
  "rbac.authorization.k8s.io/v1::RoleBinding::ns::rb-protected-secret-subj": {
    "roleRef": {"kind": "Role", "name": "role-known", "apiGroup": "rbac.authorization.k8s.io"},
    "subjects": [{"kind": "Secret", "name": "secret-protected", "namespace": "ns"}]
  },
  "apps/v1::Deployment::ns::deploy-missing": {
    "spec": {"template": {"spec": {
      "imagePullSecrets": [{"name": "pull-secret-missing"}],
      "initContainers": [{"name": "i", "env": [{"name": "E", "valueFrom": {"secretKeyRef": {"name": "init-secret-missing", "key": "k"}}}]}],
      "containers": [{"name": "c", "envFrom": [{"secretRef": {"name": "secret-protected"}}]}]
    }}}
  },
  "v1::ServiceAccount::ns::sa-secret-missing": {"secrets": [{"name": "sa-secret-missing"}]}
}
EOF

# --- incomplete manifest row is rejected ----------------------------------
printf '%s\n' "$HDR" > "$scratch/allow-bad.tsv"
printf 'DELETE\tv1\tSecret\t-\n' >> "$scratch/allow-bad.tsv"
rc=0
python3 "$chk" --allow "$scratch/allow-bad.tsv" --fixture "$scratch/fixtures-ok.json" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: incomplete manifest row must be rejected (rc=$rc)" >&2; exit 1; }

# --- canonical manifest schema: fixture mode is fail-closed --------------------------------------
# Empty fixture + populated retire set must NOT close (the old code
# iterated only fixture-present objects and returned OK with 0 checks).
echo '{}' > "$scratch/fixtures-empty.json"
out="$(python3 "$chk" --allow "$scratch/allow.tsv" --fixture "$scratch/fixtures-empty.json" 2>&1)" || true
grep -q "closure: FAIL" <<< "$out" || { echo "FAIL: empty fixture must not close" >&2; echo "$out"; exit 1; }
grep -q "FETCH-ERROR" <<< "$out" || { echo "FAIL: missing retire roots must be FETCH-ERROR" >&2; echo "$out"; exit 1; }
rc=0; python3 "$chk" --allow "$scratch/allow.tsv" --fixture "$scratch/fixtures-empty.json" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: empty fixture must exit 1 (got $rc)" >&2; exit 1; }

# A single retire root absent from the fixture while the rest resolve:
# still must fail (no partial closure).
python3 - "$scratch/fixtures-ok.json" "$scratch/fixtures-missing.json" <<'PY'
import json, sys
src = json.load(open(sys.argv[1]))
del src["v1::ServiceAccount::ns::sa-ok"]
json.dump(src, open(sys.argv[2], "w"))
PY
out="$(python3 "$chk" --allow "$scratch/allow.tsv" --fixture "$scratch/fixtures-missing.json" 2>&1)" || true
grep -q "sa-ok" <<< "$out" || { echo "FAIL: missing root sa-ok not reported" >&2; echo "$out"; exit 1; }
grep -q "closure: FAIL" <<< "$out" || { echo "FAIL: partial fixture must not close" >&2; echo "$out"; exit 1; }

# --- broken fixture: exit 1 with the exact misses --------------------------
out="$(python3 "$chk" --allow "$scratch/allow.tsv" --fixture "$scratch/fixtures-broken.json" 2>&1)" || true
# explicit per-case asserts (grep on the raw lines)
grep -q "role-missing" <<< "$out"      || { echo "FAIL: missing Role not reported" >&2; exit 1; }
grep -q "other-ns/sa-ok" <<< "$out"    || { echo "FAIL: wrong-namespace SA not reported" >&2; exit 1; }
grep -q "secret-protected" <<< "$out"  || { echo "FAIL: protected Secret reference not reported" >&2; exit 1; }
grep -q "pull-secret-missing" <<< "$out" || { echo "FAIL: imagePullSecret reference not reported" >&2; exit 1; }
grep -q "init-secret-missing" <<< "$out" || { echo "FAIL: initContainer reference not reported" >&2; exit 1; }
grep -q "sa-secret-missing" <<< "$out" || { echo "FAIL: SA secrets reference not reported" >&2; exit 1; }
grep -q "closure: FAIL" <<< "$out"     || { echo "FAIL: broken fixture must not close" >&2; exit 1; }

# --- ok fixture: exit 0, closure OK, and the top-level roleRef path was
# actually exercised (refs-checked must be > 0) ----------------------------
out="$(python3 "$chk" --allow "$scratch/allow.tsv" --fixture "$scratch/fixtures-ok.json" 2>&1)" || {
  echo "FAIL: fully-known fixture must close" >&2; echo "$out"; exit 1; }
grep -q "closure: OK" <<< "$out" || { echo "FAIL: expected closure OK" >&2; echo "$out"; exit 1; }
refs="$(grep -o 'refs-checked: [0-9]*' <<< "$out" | grep -o '[0-9]*')"
[ "${refs:-0}" -ge 8 ] || { echo "FAIL: only $refs refs checked — roleRef/subjects path not exercised" >&2; exit 1; }

echo "closure-check offline negative tests PASS (top-level roleRef/subjects, wrong-ns, protected set, init/imagePull/projected/PVC, incomplete row)"
