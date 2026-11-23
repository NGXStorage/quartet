# Writing a Quartet scenario

Keep each case in `tests/scenario/cases/<behavior>/` with a `case.sh` body
and a `case.json` description. Discovery reads JSON without executing the case.
The metadata contains `name`, `platform`, `profile`, `risk`, `scope`,
`precondition` and `plan_ref`. Describe what the case changes and what must
already exist on the target.

## Select a target

The runner requires `KUBE_CONTEXT`, `TEST_NAMESPACE` and a matching
`ALLOW_LIVE=allow-<risk>` grant. `KUBE_BIN` defaults to `kubectl`. Choose
the risk from the actual effect: `isolated`, `cluster-scoped-fault` or
`uninstall`. The `all` selection uses isolated cases. A case that changes
the cluster singleton or driver workloads requires the corresponding
cluster-wide scope, even when its test volumes are in a dedicated namespace.

Call `scenario_preflight <case.json>` before creating resources. Use the
runner's exported helpers when present; resolve offline fixtures relative
to the case script. Direct invocation must pass the same preflight.
Use `scenario_kube` for cluster requests. It binds the context and resource
scope and rejects auth, server or namespace overrides. A namespaced grant
permits only known namespaced resources in the selected namespace.

## Observe the result

Poll the actual API with a time limit. For a spec change, check
`observedGeneration` as well as the resulting conditions. For active-volume
protection, `SCENARIO_USAGE_MODE=synthetic` is the offline fixture mode.
Use `SCENARIO_USAGE_MODE=live` to provision a test volume, mount it on a
worker and observe the actual PV and VolumeAttachment through the claim
and volume identity chain. A setup failure leaves the live case blocked.

Each case gets its own evidence directory under the run directory. Keep
the target identity, runner revision, operand UIDs, image digest, status
and cleanup result together. Write large API lists to
`$EVIDENCE_DIR/lists/<kind>.json`. Record failed GETs or JSON parsing as
errors so an unreadable list cannot look empty. Supply credentials through
the runtime environment and keep their values out of metadata and output.

## Clean up by UID

- `scenario_create_tracked <kind> <name> [--namespace <ns>] <manifest>`
  records the UID from the successful create response in `ownership.tsv`.
- `scenario_delete_owned <kind> <name> [--namespace <ns>] <uid>`
  checks the recorded identity and sends `DeleteOptions.preconditions.uid`
  in the DELETE body. It verifies absence after deletion. Both helpers use
  1 for API errors and 2 for refused operations.

Run cleanup on every exit path, including failed setup. A same-name
replacement belongs to a different owner. The offline fixture must exercise
replacement between GET and DELETE and preserve the replacement on conflict.
Only a Kubernetes NotFound response confirms absence. Authorization errors,
timeouts and malformed responses fail cleanup.

| Case exit | Meaning |
| --- | --- |
| 0 | PASS, including successful cleanup |
| 1 | FAIL, including unexpected script errors |
| 2 | BLOCKED |
| 3 | SKIP |
| 4 | FAIL_CLEANUP |

Default to failure until the case has observed its postconditions and
verified cleanup. The runner reads the case exit code.
