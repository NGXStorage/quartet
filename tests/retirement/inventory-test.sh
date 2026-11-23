#!/usr/bin/env bash
# Offline tests for inventory.sh consumer scan: real
# Kubernetes field names — VolumeAttachment spec.attacher, StorageClass
# top-level provisioner, all-namespace PVC->SC/PV links, cross-namespace and
# cluster RBAC subjects; API errors stay INVENTORY-ERROR (fail-closed).
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/inventory-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
INV="$root/hack/retirement/inventory.sh"
FAKE="$root/hack/retirement/fake-kubectl.py"
NS=nfs-csi-ngxstorage
DRIVER=nfs.csi.ngxstorage.com

build_state() { # $1=state
python3 - "$1" "$NS" "$DRIVER" <<'PY'
import json, os, sys
S, NS, DRIVER = sys.argv[1], sys.argv[2], sys.argv[3]
os.makedirs(f"{S}/objects", exist_ok=True)
open(f"{S}/calls.log","w").close()
json.dump([], open(f"{S}/faults.json","w"))
def obj(kind, ns, name, uid, extra=None):
    d={"kind":kind,"metadata":{"name":name,"namespace":ns,"uid":uid,"resourceVersion":"1"},
       "spec":{}}
    if extra: d.update(extra)
    json.dump(d, open(f"{S}/objects/{kind}__{ns}__{name}.json","w"))
# namespaced: an nfs object + a neighbor
obj("secret", NS, "nfs-csi-ngxstorage-config", "sec1", {"data":{"api-key":"x"}})
obj("configmap", NS, "kube-root-ca.crt", "cm1", {"data":{"ca.crt":"x"}})
# cluster-scoped retirement target present
obj("csidriver", "-", DRIVER, "cd1")
# CONSUMER: VA uses REAL field spec.attacher
obj("volumeattachment", "-", "va-nfs", "va1",
    {"spec":{"attacher":DRIVER,"source":{"persistentVolumeName":"pv-nfs"}}})
# CONSUMER: SC uses top-level provisioner
obj("sc", "-", "sc-nfs", "sc1", {"provisioner":DRIVER})
# foreign VA/SC must NOT be flagged
obj("volumeattachment", "-", "va-foreign", "va2",
    {"spec":{"attacher":"other.csi.example","source":{"persistentVolumeName":"pv-x"}}})
obj("sc", "-", "sc-foreign", "sc2", {"provisioner":"other.csi.example"})
# CONSUMER-PV: CSI nfs PV
obj("pv", "-", "pv-nfs", "pv1",
    {"spec":{"csi":{"driver":DRIVER,"volumeHandle":"h1"},"capacity":{"storage":"1Gi"}}})
# foreign PV
obj("pv", "-", "pv-foreign", "pv2",
    {"spec":{"csi":{"driver":"other.csi.example","volumeHandle":"h2"},"capacity":{"storage":"1Gi"}}})
# CONSUMER-PVC: a Pending PVC in ANOTHER namespace selecting the nfs SC
obj("pvc", "other-ns", "pvc-pending", "pvc1",
    {"spec":{"storageClassName":"sc-nfs"},"status":{"phase":"Pending"}})
# CONSUMER-PVC: bound PVC in another ns to the nfs PV
obj("pvc", "other-ns", "pvc-bound", "pvc2",
    {"spec":{"storageClassName":"sc-foreign","volumeName":"pv-nfs"},"status":{"phase":"Bound"}})
# foreign pvc (other sc, no pv) must NOT be flagged
obj("pvc", "other-ns", "pvc-foreign", "pvc3",
    {"spec":{"storageClassName":"sc-foreign"},"status":{"phase":"Bound"}})
# CONSUMER-RB: cross-namespace RoleBinding subject -> nfs SA
obj("rolebinding", "other-ns", "rb-cross", "rb1",
    {"subjects":[{"kind":"ServiceAccount","name":"nfs-csi-ngxstorage-controller","namespace":NS}],
     "roleRef":{"kind":"Role","name":"r"}})
# CONSUMER-CRB: cluster grant to nfs SA
obj("clusterrolebinding", "-", "crb-cluster", "crb1",
    {"subjects":[{"kind":"ServiceAccount","name":"nfs-csi-ngxstorage-node","namespace":NS}],
     "roleRef":{"kind":"ClusterRole","name":"cr"}})
PY
}

run_inv() { # $1=state -> prints TSV
  cat > "$1/kubectl" <<EOF
#!/bin/sh
FAKE_STATE="$1" exec python3 "$FAKE" "\$@"
EOF
  chmod +x "$1/kubectl"
  bash "$INV" "$1/kubectl" "$NS" csidriver/$DRIVER
}

# positive: real-schema consumers detected
S=$scratch/pos; mkdir -p $S; build_state $S
tsv="$(run_inv $S)"
for want in \
  "CONSUMER-VA	$DRIVER	-	va-nfs" \
  "CONSUMER-SC	$DRIVER	-	sc-nfs" \
  "CONSUMER-PV	$DRIVER	-	pv-nfs" \
  "CONSUMER-PVC-PENDING	sc:sc-nfs	other-ns	pvc-pending" \
  "CONSUMER-PVC	bound-pv:pv-nfs	other-ns	pvc-bound" \
  "CONSUMER-RB	rb-cross	other-ns	subject:nfs-csi-ngxstorage-controller" \
  "CONSUMER-CRB	crb-cluster	-	subject:nfs-csi-ngxstorage-node"
do
  grep -qP "^$want" <<< "$tsv" || { echo "FAIL pos: missing [$want]" >&2; echo "$tsv" | grep CONSUMER; exit 1; }
done
# foreign objects NOT flagged
grep -q "va-foreign\|sc-foreign\b\|pv-foreign\|pvc-foreign" <<< "$tsv" && {
  echo "FAIL pos: foreign object flagged as consumer" >&2; exit 1; }

echo "consumer-scan positive OK: VA/SC/PV/Pending-PVC/bound-PVC/cross-ns-RB/cluster-CRB detected via real schema"

# negative API error: sc list 403 -> INVENTORY-ERROR row (fail-closed)
S=$scratch/neg; mkdir -p $S; build_state $S
python3 -c "import json;json.dump([{'match':{'verb':'get','kind':'sc'},'reply':'forbidden'}],open('$S/faults.json','w'))"
tsv="$(run_inv $S)"
grep -qP "^INVENTORY-ERROR" <<< "$tsv" || { echo "FAIL neg: no INVENTORY-ERROR on sc 403" >&2; exit 1; }
echo "consumer-scan negative OK: API error -> INVENTORY-ERROR (fail-closed)"

echo "inventory consumer-scan offline tests PASS"
