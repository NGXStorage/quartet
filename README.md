# NGX Storage Quartet Operator

![NGX Storage Quartet](logo/ngx_quartet.png)

NGX Storage Quartet is an open source storage orchestrator developed and supported by NGX Storage. It gives containerized applications persistent storage through Kubernetes-native interfaces such as the [Container Storage Interface (CSI)](https://kubernetes-csi.github.io/docs/introduction.html).

Quartet runs as pods within the Kubernetes cluster and dynamically provisions storage for Kubernetes workloads. When an application requests a persistent volume, Quartet creates it on NGX Storage systems and makes it available to the workload automatically. It also supports data protection, disaster recovery, portability and migration use cases, building on NGX Storage's data management capabilities to provide snapshots, backups, replication and cloning for stateful applications running on Kubernetes.

Quartet runs the NGX Storage NFS, Fibre Channel and iSCSI drivers on Kubernetes and OpenShift. A cluster administrator selects the drivers in one cluster-wide `Quartet` resource named `quartet`. The operator runs a controller Deployment and a node DaemonSet for each enabled driver.

## Supported platforms

Quartet is supported on a range of Kubernetes distributions:

* Upstream Kubernetes
* Red Hat OpenShift

## Features

- One cluster-wide `Quartet` resource selects any combination of three profiles: `nfs`, `fc` and `iscsi`.
- Reconciles driver workloads, CSIDriver registrations and coordination resources.
- Manages a driver-specific SCC on OpenShift.
- Refuses to disable a profile or remove its drivers while storage is still in use.
- Phase-1 installation from verified manifests, with image digests supplied by the administrator.

## Profiles

| Profile | Driver namespace | Storage path to prepare |
| --- | --- | --- |
| `nfs` | `ngxstorage-nfs` | Workers can mount NFS exports from the array's data IP. |
| `fc` | `ngxstorage-fc` | Worker HBAs are zoned to the array targets and multipath is configured. |
| `iscsi` | `ngxstorage-iscsi` | Workers have initiator IQNs, iSCSI tools and connectivity to the configured portal group. |

## Install

Start with the [installation guide](docs/INSTALL.md). It walks through a first NFS installation, from selecting the manager image to checking the driver on the workers. You supply the backend Secrets, driver image digests and StorageClasses, and install each profile's RBAC before enabling it.

## Operate

Use the [profile and troubleshooting guide](docs/INSTALL.md#check-a-profile) to inspect readiness, add a driver or diagnose a blocked change. The [API definition](api/v1alpha1/quartet_types.go) describes the `v1alpha1` spec and status fields.

## Build and test

Run `make build` to compile the manager from the committed Go dependencies. See [building and image provenance](docs/BUILDING.md) and the [test runner guide](tests/README.md).

## Documentation

Detailed documentation can be found in the [NGX Storage knowledge base](https://kb.ngxstorage.com).

## License

Licensed under [Apache 2.0](LICENSE).
