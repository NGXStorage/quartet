# Install and operate Quartet

This walkthrough installs one manager and enables NFS on a new installation.
For OpenShift select `ocp` when rendering. The commands use `kubectl` with
the target cluster's kubeconfig. Run them from the repository root in one
Bash session, continuing only after each step succeeds.

## Before installation

- Use a cluster administrator context that can create CRDs, namespaces and
  cluster RBAC. The manager needs a pullable image, and the workers need
  access to the driver and sidecar images.
- Check for a previous installation of the driver. A standalone driver
  registering the same CSI name also uses the same kubelet plugin path.
  Plan its removal or migration before enabling that profile in Quartet.
- Have the manager and driver image references from your registry. Each
  reference includes the repository and a full `@sha256:` manifest digest.
- The render script uses Bash, Go, Python 3 with PyYAML, `sha256sum` and
  `kubectl`. It obtains Kustomize v5.7.1 through Go; the module must be
  available in your cache or through your module proxy.

## Install the manager and profile permissions

Set `MANAGER_IMAGE` and `NFS_IMAGE` to your verified registry references.
Choose a new bundle directory outside the checkout. Keep the rendered bundle
so you can identify the installed permissions and image later.

```bash
: "${MANAGER_IMAGE:?Set the full manager image digest reference}"
: "${NFS_IMAGE:?Set the full NFS driver image digest reference}"
platform=k8s                 # use ocp on OpenShift
cache_root="${XDG_CACHE_HOME:-$HOME/.cache}"
mkdir -p "$cache_root"
bundle=$(mktemp -d "$cache_root/quartet-install.XXXXXXXX")
kubectl config current-context
QUARTET_INSTALL_PROFILE=nfs bash hack/install-artifacts.sh "$platform" "$MANAGER_IMAGE" "$bundle"
```

Review the three phase files below. In particular, check the selected platform,
the driver namespace and the manager image. The checksum manifest is written
after the renderer verifies the bundle. Apply the phases in order and wait
for the API server to establish the CRD before continuing.

```bash
(
  set -eu
  sha256sum --check "$bundle/artifacts.sha256"
  kubectl apply -f "$bundle/phase1-namespace-crd.yaml"
  kubectl wait --for=condition=Established crd/quartets.quartet.ngxstorage.com --timeout=120s
  kubectl apply -f "$bundle/phase2-rbac-operands.yaml"
  kubectl apply -f "$bundle/phase3-manager.yaml"
  kubectl -n quartet-system rollout status deployment/quartet-manager --timeout=300s
)
```

## Enable NFS

Create the backend Secret in the driver namespace. In this example
`SECRET_DIR` is a private directory populated by your secret manager. Each
file contains the exact field value, without a trailing newline.
`storage-ips` contains the management addresses as a comma-separated list;
`data-ip` is the address used for NFS mounts.

```bash
: "${SECRET_DIR:?Set the directory containing the four NFS Secret fields}"
kubectl -n ngxstorage-nfs create secret generic nfs-csi-ngxstorage-config \
  --from-file=storage-ips="$SECRET_DIR/storage-ips" \
  --from-file=api-key="$SECRET_DIR/api-key" \
  --from-file=pool-name="$SECRET_DIR/pool-name" \
  --from-file=data-ip="$SECRET_DIR/data-ip"
```

Create the cluster-scoped resource once. The example keeps the other profiles
disabled. Choose `spec.platform` at creation time; it is immutable.
The [sample manifest](../config/samples/quartet_v1alpha1_quartet.yaml)
shows all three profile fields.

```bash
kubectl create -f - <<YAML
apiVersion: quartet.ngxstorage.com/v1alpha1
kind: Quartet
metadata:
  name: quartet
spec:
  platform: auto
  profiles:
    nfs:
      enabled: true
      secretRef:
        name: nfs-csi-ngxstorage-config
      image:
        reference: "${NFS_IMAGE:?Set the NFS driver image digest reference}"
YAML
kubectl wait --for=condition=Ready quartets.quartet.ngxstorage.com/quartet --timeout=5m
```

## Check a profile

```bash
kubectl get quartets.quartet.ngxstorage.com quartet -o yaml
kubectl -n ngxstorage-nfs get deployment,daemonset,pods -o wide
kubectl get csidriver nfs.csi.ngxstorage.com
kubectl get csinode -o 'custom-columns=NODE:.metadata.name,DRIVERS:.spec.drivers[*].name'
```

Check that `status.observedGeneration` matches `metadata.generation` and
inspect `status.profiles.nfs.conditions` for the NFS result. Confirm that the
driver is registered on the workers that will run storage workloads. To
verify I/O, use a StorageClass whose provisioner is `nfs.csi.ngxstorage.com`,
create a test claim and check that a pod can write and read the mounted volume.
The cluster administrator supplies the StorageClass and any VolumeSnapshotClass.

| Reason | Where to look |
| --- | --- |
| `RBACDrift` (draft reason, not yet stable) | Compare the installed roles and bindings with the selected profile and platform. Reapply the profile's installer RBAC after reviewing the difference. |
| `WorkloadNotReady` | Inspect the profile's pod events, image pulls and DaemonSet placement. On OpenShift, also check SCC admission. |
| `ForeignResourceConflict` | Inspect the existing object's ownership. Resolve the naming or installation conflict before retrying. |
| `DataInUse` (draft reason, not yet stable) | Check PVs, their claims and VolumeAttachments for this driver before proceeding with disable or deletion. |

## Add or disable a profile

For each additional profile, install its namespace and RBAC on the same
cluster, create its Secret, then update the **existing** Quartet resource
with `enabled: true`, `secretRef.name` and the driver image digest. For
example, the FC permissions can be installed with:

```bash
make install-operands PLATFORM="$platform" PROFILE=fc
```

| Profile | Secret namespace | Required Secret keys |
| --- | --- | --- |
| `nfs` | `ngxstorage-nfs` | `storage-ips`, `api-key`, `pool-name`, `data-ip` |
| `fc` | `ngxstorage-fc` | `storage-ips`, `api-key`, `pool-name`, `fc-target-names` |
| `iscsi` | `ngxstorage-iscsi` | `storage-ips`, `api-key`, `pool-name`, `portal-group` |

To retire a profile, first review its consumers and the retention policy for
their data. Set that profile's `enabled` field to `false` and watch the CR
conditions during cleanup. Deleting the Quartet resource starts cleanup
for all managed profiles. Keep the manager running until the finalizer
completes. Backend Secrets, namespaces and customer volumes remain under
administrator control.
