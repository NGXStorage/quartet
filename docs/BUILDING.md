# Build the Quartet manager

## Compile from a standalone checkout

Install Make and a Go toolchain compatible with [go.mod](../go.mod)
(Go 1.27.0 or newer). From the repository root:

```bash
GOFLAGS=-mod=vendor GOPROXY=off make build
```

The binary is written to `bin/manager`. The committed `vendor/` tree
contains the same pinned Go dependencies used by the module build.

## Build a container image

The [Dockerfile](../Dockerfile) uses digest-pinned Go and runtime base images.
With `vendor/modules.txt` present, it builds with `-mod=vendor` and
`GOPROXY=off`. Without vendor files, it resolves the pinned versions in
`go.mod` and verifies them with `go.sum`.

- Logging comes from `github.com/ngxstorage/go-logutils@v1.0.1`.
- Structured logging uses upstream `go.uber.org/zap@v1.28.0`.

From a clean checkout:

```bash
docker build --build-arg VCS_REF="$(git rev-parse HEAD)" -t quartet-manager:local .
```

Use the registry manifest digest when installing Quartet. The source revision
identifies the code and the manifest digest identifies what the cluster pulls.
See the [installation guide](INSTALL.md).

`make provenance-check` checks the module pins, builds with a fresh module
cache, and inspects provenance labels when the local image exists. Set
`IMAGE` to its local reference. Run `bash hack/provenance-check.sh --optional`
to report unavailable network or Docker checks separately from failures.

## Tests and CI

| Check | What it exercises |
| --- | --- |
| Standalone Go tests | The GitLab test job uses `go test -count=1 -skip TestGoldenVerifyAllProfiles ./...` with optional envtest. |
| Controller API tests | Set `KUBEBUILDER_ASSETS` to the API server and etcd binaries. Required envtest exercises reconciliation against a real API server. |
| Driver manifest parity | `make golden-verify` compares rendered resources with the driver manifests. It needs the three driver checkouts and Kustomize v5.7.1. |
| Live scenarios | A configured cluster and backend exercise workload readiness, provisioning and mounted data. See the [test commands](../tests/README.md). |

The [GitLab pipeline](../.gitlab-ci.yml) defines automatic lint, test and
binary-build jobs, plus manual envtest and cross-repository golden checks.
The pinned Kustomize command uses a versioned Go tool outside the project's
vendor tree. Populate the tool cache or provide a reachable module proxy
when running render checks.
