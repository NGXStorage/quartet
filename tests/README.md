# Test Quartet

From the repository root, list the suites or inspect the available live cases:

```bash
bash tests/run-all-tests.sh --list
bash tests/run-all-tests.sh scenario --list
```

## Local and API server tests

| Command | Coverage |
| --- | --- |
| `bash tests/run-all-tests.sh local` | Go vet, race-enabled Go tests, install/build tooling, retirement scripts and the test entry point. |
| `bash tests/run-all-tests.sh matrix` | Runs the Go suite against each requested envtest API server version using installed assets. |
| `make golden-verify` | Compares rendered resources against manifests from the three driver repositories. See the [build guide](../docs/BUILDING.md#tests-and-ci) for prerequisites. |

Set `KUBEBUILDER_ASSETS` to the directory containing the envtest binaries
for a local run with required API coverage. The local runner returns 0 for
`PASS`, 1 for `FAIL` and 2 for `DEGRADED` when named tests were skipped
or required envtest coverage was missing. The matrix entry point disables
asset downloads and reports a missing version as `BLOCKED`.

These suites run without a live Kubernetes cluster. A cold Go or Kustomize
cache may still require network access. A standalone checkout can run the
[standalone Go subset](../docs/BUILDING.md#tests-and-ci); the full golden
comparison needs the driver sources.

## Live scenarios

Use `scenario --list` to see each case's profile and scope. Its
`case.json` lists the preconditions.
Live cases require an explicit cluster context, test namespace and matching
risk grant. A case that disables a driver or deletes the Quartet resource
can affect every workload using that driver, including workloads outside
the test namespace.

See the [scenario contract](scenario/CASE-CONTRACT.md) for inputs, cleanup
and exit codes. Driver CSI sanity and data-path suites live in the individual
driver repositories.
