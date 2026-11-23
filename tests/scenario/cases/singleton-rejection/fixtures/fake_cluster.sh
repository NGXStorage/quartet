#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# fake_cluster.sh — OFFLINE test double for the Quartet operator scenario cases.
#
# This is a FAKE: it emulates the Kubernetes API surface the scenario cases
# touch AND a faithful model of the reconciler's decision points, so the cases
# run hermetically (no live cluster). It is NOT a substitute for live evidence:
# a case driven against this fake reports "NOT RUN", never PASS.
#
# Every behavior below is BOUND to the operator source. The file:line
# references are the single source of truth for the expected reason/status; if
# the source changes, this double must change with it (and the contract
# sabotage below proves the binding bites).
#
# Reconcile order (validate -> detect -> finalizer -> applyProfiles):
# internal/controller/quartet_controller.go:90-167
# Singleton name rejection (reconciler-side only; CRD admits any name):
# api/v1alpha1/validation.go:25-37, quartet_types.go:7-15
# Terminal validation failure (reported once, never requeued):
# internal/controller/quartet_controller.go:109-114, 318-328 (failedStatus)
# NFS runtime apply order + stop-at-first-failure:
# internal/renderer/nfs.go:87-124, internal/controller/apply.go:119-171
# Ownership gate / no-force foreign conflict (no adoption, ownership gate):
# internal/controller/apply.go:831-900
# Installer-provisioned RBAC verify (read-only, RBACDrift gate):
# internal/controller/apply.go:221-244
# Operand namespace ensure (namespace ownership: create-if-absent, never delete):
# internal/controller/apply.go:502-526
# Workload readiness probe + Secret-never-read (Secret API untouched):
# internal/controller/apply.go:328-434
# Retryable requeue interval (30s):
# internal/controller/quartet_controller.go:45,172-179
#
# Storage: plain JSON objects under $FAKE_STATE_DIR/obj/<kind>/<ns>/, ns="_" for
# cluster-scoped kinds. Dependency-free except jq. Scratch lives only under
# $HOME/.cache/ngxstorage (machine policy: never /tmp or /var/tmp).
# -----------------------------------------------------------------------------

# --- pinned NFS operand constants (internal/renderer/nfs.go:34-49,289-292) ---
fc_nfs_namespace()        { echo "ngxstorage-nfs"; }
fc_nfs_csidriver_name()   { echo "nfs.csi.ngxstorage.com"; }
fc_nfs_controller_dep()   { echo "nfs-csi-ngxstorage-controller"; }
fc_nfs_node_ds()          { echo "nfs-csi-ngxstorage-node"; }
fc_nfs_health_svc()       { echo "nfs-csi-ngxstorage-controller-health"; }
fc_nfs_coord_cm()         { echo "nfs-csi-ngxstorage-coordination-config"; }
fc_nfs_lease_prefix()     { echo "nfs-csi-ngxstorage-operation-lock"; }
fc_nfs_shards()           { echo 128; }
fc_owner_annotation()     { echo "quartet.ngxstorage.com/owner-uid"; }
fc_field_manager()        { echo "quartet-operator"; }
fc_finalizer()            { echo "quartet.ngxstorage.com/quartet"; }
fc_singleton_name()       { echo "quartet"; }

# The Quartet CRD name the case bodies' live-adapter readiness probe reads
# (config/crd/bases/quartet.ngxstorage.com_quartets.yaml:7).
fc_quartet_crd()          { echo "quartets.quartet.ngxstorage.com"; }

# Approved NFS driver image (digest pin: the spec carries the DRIVER image only):
# hack/golden-verify/main.go:36 (rawDriverDigest), pinned to the raw render
# inventory (the pinned raw-render notes). Sidecar images are
# renderer CONSTANTS (internal/renderer/nfs.go:36-46) and never enter the spec.
fc_driver_image() {
  echo "quay.io/ngxstorage/nfs-csi-ngxstorage@sha256:809528ed53c084730c1f645c6b8f4d8b04dfaac2a4ca3fbee81059606e39243b"
}

# The EXACT backend Secret key set the NFS renderer's secretKeyRef env block
# reads (internal/renderer/nfs.go:476-479): storage-ips / pool-name / api-key /
# data-ip. The kubelet model below treats a Secret missing ANY of these keys as
# CreateContainerConfigError — the renderer reads all four, so the real
# workload can only start when all four resolve (nfs.go:565, driverArgs).
fc_secret_required_keys() { echo "storage-ips pool-name api-key data-ip"; }

fc_is_cluster_scoped() { # <kind-lc> -> rc 0 if cluster-scoped
 # quartet: bound to config/crd/bases/quartet.ngxstorage.com_quartets.yaml
 # (spec.scope: Cluster — the convergence gate parses the real CRD and
 # asserts the model matches).
  case "$1" in
    namespace|csidriver|clusterrole|clusterrolebinding|crd|quartet) return 0 ;;
    *) return 1 ;;
  esac
}

fc_state_dir() { echo "${FAKE_STATE_DIR:?FAKE_STATE_DIR not set}"; }

fc_obj_path() { # <kind> <ns> <name>
  local kind_lc
  kind_lc=$(echo "$1" | tr '[:upper:]' '[:lower:]')
  if fc_is_cluster_scoped "$kind_lc"; then
    echo "$(fc_state_dir)/obj/${kind_lc}/_/$3.json"
  else
    echo "$(fc_state_dir)/obj/${kind_lc}/${2:-_}/$3.json"
  fi
}

fc_get() { # <kind> <ns> <name> -> JSON; rc 1 if absent
  local p; p=$(fc_obj_path "$1" "$2" "$3")
  [[ -f "$p" ]] || return 1
  cat "$p"
}

fc_exists() { [[ -f "$(fc_obj_path "$1" "$2" "$3")" ]]; }

# Assign uid/rv on first write; bump rv on update.
_fc_stamp_identity() { # <path> <json> -> stamped json on stdout
  local path="$1" json="$2" uid rv
  if [[ -f "$path" ]]; then
    rv=$(( $(jq -r '.metadata.resourceVersion // 1' "$path") + 1 ))
    echo "$json" | jq --argjson rv "$rv" '.metadata.resourceVersion = $rv'
  else
    uid="fake-uid-$RANDOM$RANDOM-$(date +%s%N)"
    rv=1
    echo "$json" | jq --arg uid "$uid" --argjson rv "$rv" \
      '.metadata.uid = $uid | .metadata.resourceVersion = $rv'
  fi
}

fc_put() { # <kind> <ns> <name> <json>
  local path stamped
  path=$(fc_obj_path "$1" "$2" "$3")
  mkdir -p "$(dirname "$path")"
 # Stamp FIRST, then write: the redirection would pre-create the file and
 # make _fc_stamp_identity take its "exists" branch (losing the uid).
  stamped=$(_fc_stamp_identity "$path" "$4")
  printf '%s\n' "$stamped" > "$path"
}

fc_delete() { # <kind> <ns> <name> -> rc 1 if absent (move to deleted/, never rm)
  local p; p=$(fc_obj_path "$1" "$2" "$3")
  [[ -f "$p" ]] || return 1
  mkdir -p "$(fc_state_dir)/deleted"
  mv "$p" "$(fc_state_dir)/deleted/$(basename "$p").$(date +%s%N)"
  return 0
}

fc_event() { # <type> <reason> <message>
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$(fc_state_dir)/events.tsv"
}

# --- the fake operator's pinned NFS install-RBAC set --------------------------
# Mirrors renderer.nfsProfile.installRBAC() for platform=kubernetes
# (internal/renderer/nfs.go:125-133): the six installer-owned objects. The
# reconciler only VERIFIES these by pinned name + content (apply.go:221-244);
# it never creates them. The content is a fingerprint (rules/roleRef/subjects)
# distinct enough that a tampered object reads as drift.
fc_install_rbac_manifest() {
 # Golden fingerprint generated from the REAL committed install manifest
 # (config/operands/nfs/k8s/rbac.yaml via hack/generate-install-rbac <-
 # renderer.nfsProfile.InstallRBAC, internal/renderer/nfs.go:125-133). The
 # reconciler verifies these exact rules/roleRef/subjects by pinned name
 # (apply.go:221-244); a tampered/foreign object reads as drift.
  cat <<'GOLDEN_EOF'
[{"kind":"ClusterRole","name":"nfs-csi-ngxstorage-controller","namespace":"","rules":[{"apiGroups":[""],"resources":["persistentvolumes"],"verbs":["get","list","watch","create","delete","patch"]},{"apiGroups":[""],"resources":["persistentvolumeclaims"],"verbs":["get","list","watch","update"]},{"apiGroups":[""],"resources":["persistentvolumeclaims/status"],"verbs":["patch","update"]},{"apiGroups":[""],"resources":["nodes"],"verbs":["get","list","watch"]},{"apiGroups":[""],"resources":["events"],"verbs":["list","watch","create","update","patch"]},{"apiGroups":[""],"resources":["pods"],"verbs":["get","list","watch"]},{"apiGroups":["storage.k8s.io"],"resources":["storageclasses","csinodes"],"verbs":["get","list","watch"]},{"apiGroups":["storage.k8s.io"],"resources":["volumeattachments"],"verbs":["get","list","watch","patch"]},{"apiGroups":["storage.k8s.io"],"resources":["volumeattachments/status"],"verbs":["patch"]},{"apiGroups":["snapshot.storage.k8s.io"],"resources":["volumesnapshotclasses","volumesnapshots"],"verbs":["get","list","watch"]},{"apiGroups":["snapshot.storage.k8s.io"],"resources":["volumesnapshotcontents"],"verbs":["get","list","watch","update","patch"]},{"apiGroups":["snapshot.storage.k8s.io"],"resources":["volumesnapshotcontents/status"],"verbs":["update","patch"]}]},{"kind":"Role","name":"nfs-csi-ngxstorage-controller-operation-lock","namespace":"ngxstorage-nfs","rules":[{"apiGroups":["coordination.k8s.io"],"resourceNames":["nfs-csi-ngxstorage-operation-lock-000","nfs-csi-ngxstorage-operation-lock-001","nfs-csi-ngxstorage-operation-lock-002","nfs-csi-ngxstorage-operation-lock-003","nfs-csi-ngxstorage-operation-lock-004","nfs-csi-ngxstorage-operation-lock-005","nfs-csi-ngxstorage-operation-lock-006","nfs-csi-ngxstorage-operation-lock-007","nfs-csi-ngxstorage-operation-lock-008","nfs-csi-ngxstorage-operation-lock-009","nfs-csi-ngxstorage-operation-lock-010","nfs-csi-ngxstorage-operation-lock-011","nfs-csi-ngxstorage-operation-lock-012","nfs-csi-ngxstorage-operation-lock-013","nfs-csi-ngxstorage-operation-lock-014","nfs-csi-ngxstorage-operation-lock-015","nfs-csi-ngxstorage-operation-lock-016","nfs-csi-ngxstorage-operation-lock-017","nfs-csi-ngxstorage-operation-lock-018","nfs-csi-ngxstorage-operation-lock-019","nfs-csi-ngxstorage-operation-lock-020","nfs-csi-ngxstorage-operation-lock-021","nfs-csi-ngxstorage-operation-lock-022","nfs-csi-ngxstorage-operation-lock-023","nfs-csi-ngxstorage-operation-lock-024","nfs-csi-ngxstorage-operation-lock-025","nfs-csi-ngxstorage-operation-lock-026","nfs-csi-ngxstorage-operation-lock-027","nfs-csi-ngxstorage-operation-lock-028","nfs-csi-ngxstorage-operation-lock-029","nfs-csi-ngxstorage-operation-lock-030","nfs-csi-ngxstorage-operation-lock-031","nfs-csi-ngxstorage-operation-lock-032","nfs-csi-ngxstorage-operation-lock-033","nfs-csi-ngxstorage-operation-lock-034","nfs-csi-ngxstorage-operation-lock-035","nfs-csi-ngxstorage-operation-lock-036","nfs-csi-ngxstorage-operation-lock-037","nfs-csi-ngxstorage-operation-lock-038","nfs-csi-ngxstorage-operation-lock-039","nfs-csi-ngxstorage-operation-lock-040","nfs-csi-ngxstorage-operation-lock-041","nfs-csi-ngxstorage-operation-lock-042","nfs-csi-ngxstorage-operation-lock-043","nfs-csi-ngxstorage-operation-lock-044","nfs-csi-ngxstorage-operation-lock-045","nfs-csi-ngxstorage-operation-lock-046","nfs-csi-ngxstorage-operation-lock-047","nfs-csi-ngxstorage-operation-lock-048","nfs-csi-ngxstorage-operation-lock-049","nfs-csi-ngxstorage-operation-lock-050","nfs-csi-ngxstorage-operation-lock-051","nfs-csi-ngxstorage-operation-lock-052","nfs-csi-ngxstorage-operation-lock-053","nfs-csi-ngxstorage-operation-lock-054","nfs-csi-ngxstorage-operation-lock-055","nfs-csi-ngxstorage-operation-lock-056","nfs-csi-ngxstorage-operation-lock-057","nfs-csi-ngxstorage-operation-lock-058","nfs-csi-ngxstorage-operation-lock-059","nfs-csi-ngxstorage-operation-lock-060","nfs-csi-ngxstorage-operation-lock-061","nfs-csi-ngxstorage-operation-lock-062","nfs-csi-ngxstorage-operation-lock-063","nfs-csi-ngxstorage-operation-lock-064","nfs-csi-ngxstorage-operation-lock-065","nfs-csi-ngxstorage-operation-lock-066","nfs-csi-ngxstorage-operation-lock-067","nfs-csi-ngxstorage-operation-lock-068","nfs-csi-ngxstorage-operation-lock-069","nfs-csi-ngxstorage-operation-lock-070","nfs-csi-ngxstorage-operation-lock-071","nfs-csi-ngxstorage-operation-lock-072","nfs-csi-ngxstorage-operation-lock-073","nfs-csi-ngxstorage-operation-lock-074","nfs-csi-ngxstorage-operation-lock-075","nfs-csi-ngxstorage-operation-lock-076","nfs-csi-ngxstorage-operation-lock-077","nfs-csi-ngxstorage-operation-lock-078","nfs-csi-ngxstorage-operation-lock-079","nfs-csi-ngxstorage-operation-lock-080","nfs-csi-ngxstorage-operation-lock-081","nfs-csi-ngxstorage-operation-lock-082","nfs-csi-ngxstorage-operation-lock-083","nfs-csi-ngxstorage-operation-lock-084","nfs-csi-ngxstorage-operation-lock-085","nfs-csi-ngxstorage-operation-lock-086","nfs-csi-ngxstorage-operation-lock-087","nfs-csi-ngxstorage-operation-lock-088","nfs-csi-ngxstorage-operation-lock-089","nfs-csi-ngxstorage-operation-lock-090","nfs-csi-ngxstorage-operation-lock-091","nfs-csi-ngxstorage-operation-lock-092","nfs-csi-ngxstorage-operation-lock-093","nfs-csi-ngxstorage-operation-lock-094","nfs-csi-ngxstorage-operation-lock-095","nfs-csi-ngxstorage-operation-lock-096","nfs-csi-ngxstorage-operation-lock-097","nfs-csi-ngxstorage-operation-lock-098","nfs-csi-ngxstorage-operation-lock-099","nfs-csi-ngxstorage-operation-lock-100","nfs-csi-ngxstorage-operation-lock-101","nfs-csi-ngxstorage-operation-lock-102","nfs-csi-ngxstorage-operation-lock-103","nfs-csi-ngxstorage-operation-lock-104","nfs-csi-ngxstorage-operation-lock-105","nfs-csi-ngxstorage-operation-lock-106","nfs-csi-ngxstorage-operation-lock-107","nfs-csi-ngxstorage-operation-lock-108","nfs-csi-ngxstorage-operation-lock-109","nfs-csi-ngxstorage-operation-lock-110","nfs-csi-ngxstorage-operation-lock-111","nfs-csi-ngxstorage-operation-lock-112","nfs-csi-ngxstorage-operation-lock-113","nfs-csi-ngxstorage-operation-lock-114","nfs-csi-ngxstorage-operation-lock-115","nfs-csi-ngxstorage-operation-lock-116","nfs-csi-ngxstorage-operation-lock-117","nfs-csi-ngxstorage-operation-lock-118","nfs-csi-ngxstorage-operation-lock-119","nfs-csi-ngxstorage-operation-lock-120","nfs-csi-ngxstorage-operation-lock-121","nfs-csi-ngxstorage-operation-lock-122","nfs-csi-ngxstorage-operation-lock-123","nfs-csi-ngxstorage-operation-lock-124","nfs-csi-ngxstorage-operation-lock-125","nfs-csi-ngxstorage-operation-lock-126","nfs-csi-ngxstorage-operation-lock-127"],"resources":["leases"],"verbs":["get","update","patch"]}]},{"kind":"Role","name":"nfs-csi-ngxstorage-leader-election","namespace":"ngxstorage-nfs","rules":[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","list","watch","create","update","patch","delete"]}]},{"kind":"RoleBinding","name":"nfs-csi-ngxstorage-controller-operation-lock","namespace":"ngxstorage-nfs","roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"nfs-csi-ngxstorage-controller-operation-lock"},"subjects":[{"kind":"ServiceAccount","name":"nfs-csi-ngxstorage-controller","namespace":"ngxstorage-nfs"}]},{"kind":"RoleBinding","name":"nfs-csi-ngxstorage-leader-election","namespace":"ngxstorage-nfs","roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"nfs-csi-ngxstorage-leader-election"},"subjects":[{"kind":"ServiceAccount","name":"nfs-csi-ngxstorage-controller","namespace":"ngxstorage-nfs"}]},{"kind":"ClusterRoleBinding","name":"nfs-csi-ngxstorage-controller","namespace":"","roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"nfs-csi-ngxstorage-controller"},"subjects":[{"kind":"ServiceAccount","name":"nfs-csi-ngxstorage-controller","namespace":"ngxstorage-nfs"}]}]
GOLDEN_EOF
}

# Seed the installer-provisioned RBAC set as healthy (the admin applied
# config/operands/nfs/ out-of-band). Content matches the pinned manifest so
# verifyOperandRBAC passes.
fake_seed_install_rbac() {
  local item kind name nsl
  while IFS= read -r item; do
    kind=$(echo "$item" | jq -r '.kind')
    name=$(echo "$item" | jq -r '.name')
    nsl=$(echo "$item" | jq -r '.namespace')
 # Preserve rules/roleRef/subjects (the compared content) AND add metadata.
    fc_put "$kind" "$nsl" "$name" "$(echo "$item" | jq \
      '. + {apiVersion:"rbac.authorization.k8s.io/v1",
        metadata:{name:.name,namespace:(if .namespace=="" then null else .namespace end),
                  labels:{installer:"config/operands/nfs"}}}')"
  done < <(fc_install_rbac_manifest | jq -c '.[]')
}

# Seed the INSTALLED OPERATOR as a precondition fact (case.json precondition:
# "Operator installed (CRD + manager)"): the CRD object the case body's
# live-adapter readiness probe reads. This models the precondition; it is NOT
# live evidence — a run against this fixture still reports NOT RUN, never PASS.
fake_seed_operator_install() {
  local crd; crd=$(fc_quartet_crd)
  fc_put "crd" "" "$crd" "$(jq -n --arg n "$crd" \
    '{apiVersion:"apiextensions.k8s.io/v1",kind:"CustomResourceDefinition",
      metadata:{name:$n},
      spec:{group:"quartet.ngxstorage.com",
            names:{kind:"Quartet",plural:"quartets"}}}')"
}

# --- one faithful reconcile pass ---------------------------------------------
# The real controller watches EVERY Quartet object and reconciles each by name;
# ValidateName then rejects non-singletons (validation.go:32-37). So the pass
# takes the CR object to reconcile (not just the singleton name).
# Returns 0 when terminal (no requeue), 1 when retryable (live operator
# requeues after 30s). Writes CR status + events.
fake_reconcile_pass() { # <cr-path>
  local cr_path="$1"
  local ns="${TEST_NAMESPACE:?TEST_NAMESPACE not set}"
  local cr name uid gen secret
  cr=$(cat "$cr_path")

 # (1) ValidateName — reconciler-side singleton gate (validation.go:32-37).
  name=$(echo "$cr" | jq -r '.metadata.name')
  if [[ "$name" != "$(fc_singleton_name)" ]]; then
    _fc_set_failed_status "$cr_path" "InvalidSpec" \
      "singleton violation: only a Quartet named \"$(fc_singleton_name)\" is reconciled, got \"${name}\""
    return 0 # terminal (controller:109-114); the writer emits the deduped event
  fi

  uid=$(echo "$cr" | jq -r '.metadata.uid')
  gen=$(echo "$cr" | jq -r '.metadata.generation // 1')
  secret=$(echo "$cr" | jq -r '.spec.profiles.nfs.secretRef.name // ""')

 # (3) Finalizer before any operand mutation (controller:129-137).
  jq --arg f "$(fc_finalizer)" \
    '.metadata.finalizers = ((.metadata.finalizers // []) + [$f] | unique)' \
    "$cr_path" > "${cr_path}.t" && mv "${cr_path}.t" "$cr_path"
  cr=$(cat "$cr_path")

 # (4) applyProfiles: only nfs is enabled in these fixtures.
  if [[ "$(echo "$cr" | jq -r '.spec.profiles.nfs.enabled')" != "true" ]]; then
    _fc_set_ready_status "$cr_path" "$gen" "kubernetes" "no enabled profile renders operands"
    return 0
  fi

  FC_PROF_REASON=""; FC_PROF_MSG=""
  _fc_apply_nfs_profile "$uid" "$secret"
  if [[ -z "$FC_PROF_REASON" ]]; then
 # Message bound VERBATIM to quartet_controller.go:302 (buildStatus Ready bar).
    _fc_set_ready_status "$cr_path" "$gen" "kubernetes" \
      "singleton validated; platform=kubernetes; operands applied and owned; workloads converged (not a backend I/O proof — the readiness bar)"
    return 0
  fi

  _fc_set_blocked_status "$cr_path" "$gen" "kubernetes" "nfs" "$FC_PROF_REASON" "$FC_PROF_MSG"
 # ForeignResourceConflict is terminal (apply.go:143-146); everything else the
 # profile can produce here is retryable (RBACDrift apply.go:133, WorkloadNotReady).
  if [[ "$FC_PROF_REASON" == "ForeignResourceConflict" ]]; then
    return 0
  fi
  return 1
}

# applyProfile for nfs (apply.go:119-171). Sets FC_PROF_REASON/FC_PROF_MSG
# (empty reason == converged). Order: ensureNamespace -> verifyOperandRBAC ->
# applyOwned(runtime objects, stop at first failure) -> probeWorkloadReadiness.
_fc_apply_nfs_profile() { # <cr-uid> <secret-name>
  local uid="$1" secret="$2" op_ns
  op_ns=$(fc_nfs_namespace)
  local ann

 # ensureNamespace (namespace ownership): create-if-absent, owned, never deleted.
  if ! fc_exists "namespace" "" "$op_ns"; then
    ann=$(fc_owner_annotation)
    fc_put "namespace" "" "$op_ns" \
      "{\"metadata\":{\"name\":\"${op_ns}\",\"annotations\":{\"${ann}\":\"${uid}\"}}}"
  fi

 # verifyOperandRBAC: every pinned install object present + content-matched.
  local item kind name nsl got diff
  while IFS= read -r item; do
    kind=$(echo "$item" | jq -r '.kind'); name=$(echo "$item" | jq -r '.name')
    nsl=$(echo "$item" | jq -r '.namespace')
 # Message bound VERBATIM to apply.go:242 (verifyOperandRBAC):
 # "profile %s: operand RBAC is installer-provisioned and the cluster state
 # drifted from the rendered set (%s) — re-apply the install manifest
 # config/operands/%s/ with admin privileges; operands are NOT created or
 # modified until the set verifies healthy"
    if ! got=$(fc_get "$kind" "$nsl" "$name"); then
      FC_PROF_REASON="RBACDrift"
      FC_PROF_MSG="profile nfs: operand RBAC is installer-provisioned and the cluster state drifted from the rendered set (${kind} \"${name}\" is not installed) — re-apply the install manifest config/operands/nfs/ with admin privileges; operands are NOT created or modified until the set verifies healthy"
      return
    fi
    diff=$(_fc_rbac_drift_detail "$item" "$got")
    if [[ -n "$diff" ]]; then
      FC_PROF_REASON="RBACDrift"
      FC_PROF_MSG="profile nfs: operand RBAC is installer-provisioned and the cluster state drifted from the rendered set (${kind} \"${name}\" drifted: ${diff}) — re-apply the install manifest config/operands/nfs/ with admin privileges; operands are NOT created or modified until the set verifies healthy"
      return
    fi
  done < <(fc_install_rbac_manifest | jq -c '.[]')

 # runtimeApplyObjects in render order (nfs.go:87-124), stop at first failure.
  local kind2 ns2 cluster name2
 # '|' is a non-whitespace IFS char, so the empty ns field on the
 # cluster-scoped row is preserved (tabs are IFS-whitespace and collapse).
  while IFS='|' read -r kind2 ns2 cluster name2; do
    [[ -z "$kind2" ]] && continue
    _fc_apply_owned "$kind2" "$ns2" "$cluster" "$name2" "$uid" "$secret"
    local rc=$?
    if [[ $rc -eq 10 ]]; then
      FC_PROF_REASON="ForeignResourceConflict"
      FC_PROF_MSG="foreign resource ${kind2} \"${name2}\" exists without this Quartet's ownership annotation; adoption is not attempted"
      return
    fi
  done < <(_fc_nfs_runtime_apply_list)

 # probeWorkloadReadiness (apply.go:328-434).
  local reason
  reason=$(_fc_probe_workload "$op_ns" "$secret")
  case "$reason" in
    secret) FC_PROF_REASON="WorkloadNotReady"
            FC_PROF_MSG="pod \"$(fc_nfs_controller_dep)-000001\" blocked: CreateContainerConfigError (the operator does not read Secrets — verify the referenced secret keys exist)" ;;
    image)  FC_PROF_REASON="WorkloadNotReady"
            FC_PROF_MSG="pod \"$(fc_nfs_controller_dep)-000001\" blocked: ImagePullFailed" ;;
    rollout) FC_PROF_REASON="WorkloadNotReady"
            FC_PROF_MSG="workload rollout not converged" ;;
  esac
  return 0
}

# Emitted as "kind|ns|clusterflag|name" lines, in the source render order
# (nfs.go:87-124). CSIDriver is cluster-scoped (ns empty).
_fc_nfs_runtime_apply_list() {
  local ns; ns=$(fc_nfs_namespace)
  local i
  printf 'ServiceAccount|%s|n|%s\n' "$ns" "nfs-csi-ngxstorage-controller"
  printf 'ServiceAccount|%s|n|%s\n' "$ns" "$(fc_nfs_node_ds)"
  printf 'ConfigMap|%s|n|%s\n' "$ns" "$(fc_nfs_coord_cm)"
 # The 128-lease pool (coordination) is one logical unit: verified by its first
 # member, created wholesale. Steady-state passes thus cost one check.
  printf 'LeasePool|%s|n|%s\n' "$ns" "$(fc_nfs_lease_prefix)"
  printf 'CSIDriver||y|%s\n' "$(fc_nfs_csidriver_name)"
  printf 'Service|%s|n|%s\n' "$ns" "$(fc_nfs_health_svc)"
  printf 'Deployment|%s|n|%s\n' "$ns" "$(fc_nfs_controller_dep)"
  printf 'DaemonSet|%s|n|%s\n' "$ns" "$(fc_nfs_node_ds)"
}

# Content comparison for the RBAC gate (apply.go:250-285).
_fc_rbac_drift_detail() { # <expected-json> <got-json> -> diff text or ""
  local e="$1" g="$2" kind
  kind=$(echo "$e" | jq -r '.kind')
  case "$kind" in
    Role|ClusterRole)
      [[ "$(echo "$e" | jq -S '.rules')" == "$(echo "$g" | jq -S '.rules')" ]] || echo "rules differ from the rendered set" ;;
    RoleBinding|ClusterRoleBinding)
      if [[ "$(echo "$e" | jq -S '.roleRef')" != "$(echo "$g" | jq -S '.roleRef')" ]]; then
        echo "roleRef differs from the rendered set"; return
      fi
      [[ "$(echo "$e" | jq -S '.subjects')" == "$(echo "$g" | jq -S '.subjects')" ]] || echo "binding subjects differ from the rendered set" ;;
  esac
}

# applyOwned ownership gate (apply.go:831-900). rc 0 ok, 10 foreign conflict.
# LeasePool is a logical unit: the first member stands in for all 128
# (coordination leases carry no distinct content); a missing pool is created wholesale.
_fc_apply_owned() { # <kind> <ns> <clusterflag> <name> <cr-uid> <secret>
  local kind="$1" ns="$2" cluster="$3" name="$4" uid="$5" secret="$6"
  local ann owner existing i
  ann=$(fc_owner_annotation)
  if [[ "$kind" == "LeasePool" ]]; then
    kind="Lease"; name="${name}-000"
    if existing=$(fc_get "$kind" "$ns" "$name"); then
      owner=$(echo "$existing" | jq -r ".metadata.annotations[\"${ann}\"] // \"\"")
      [[ "$owner" == "$uid" ]] || return 10
      return 0
    fi
 # Wholesale create: direct writes (no per-lease jq stamping) — the pool is
 # one logical unit from the reconciler's point of view.
    local lp n uid2
    for i in $(seq 0 $(( $(fc_nfs_shards) - 1 ))); do
      n="${name%-000}$(printf '%03d' "$i")"
      uid2="fake-uid-$RANDOM$RANDOM-$(date +%s%N)"
      lp=$(fc_obj_path "$kind" "$ns" "$n")
      mkdir -p "$(dirname "$lp")"
      printf '{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":"%s","namespace":"%s","uid":"%s","resourceVersion":1,"annotations":{"%s":"%s"}}}\n' \
        "$n" "$ns" "$uid2" "$ann" "$uid" > "$lp"
    done
    return 0
  fi
  if existing=$(fc_get "$kind" "$ns" "$name"); then
    owner=$(echo "$existing" | jq -r ".metadata.annotations[\"${ann}\"] // \"\"")
    [[ "$owner" == "$uid" ]] || return 10 # foreignConflict — never adopted (no adoption, ownership gate)
  fi
  _fc_write_owned "$kind" "$ns" "$name" "$uid" "$secret"
  return 0
}

# Materialize an owned operand. Workloads also get ready status + owned pods.
_fc_write_owned() { # <kind> <ns> <name> <cr-uid> <secret>
  local kind="$1" ns="$2" name="$3" uid="$4" secret="$5" op_ns
  op_ns=$(fc_nfs_namespace)
  local ann managed obj
  ann=$(fc_owner_annotation); managed=$(fc_field_manager)
  if [[ -z "$ns" ]]; then
    obj=$(jq -n --arg n "$name" --arg k "$kind" --arg uid "$uid" --arg ann "$ann" --arg m "$managed" \
      '{apiVersion:"v1",kind:$k,metadata:{name:$n,uid:$uid,resourceVersion:1,generation:1,
        annotations:({($ann):$uid}),managedFields:[{manager:$m,operation:"Apply"}],labels:{}}}')
  else
    obj=$(jq -n --arg n "$name" --arg k "$kind" --arg ns "$ns" --arg uid "$uid" --arg ann "$ann" --arg m "$managed" \
      '{apiVersion:"v1",kind:$k,metadata:{name:$n,namespace:$ns,uid:$uid,resourceVersion:1,generation:1,
        annotations:({($ann):$uid}),managedFields:[{manager:$m,operation:"Apply"}],labels:{}}}')
  fi
  case "$kind" in
    Deployment)
      obj=$(echo "$obj" | jq \
        '.spec={replicas:1} | .status={observedGeneration:1,updatedReplicas:1,availableReplicas:1,replicas:1}')
      _fc_create_workload_pods "$op_ns" "$name" "$secret" "$obj"
      ;;
    DaemonSet)
      obj=$(echo "$obj" | jq \
        '.spec={} | .status={observedGeneration:1,desiredNumberScheduled:1,updatedNumberScheduled:1,numberAvailable:1}')
      _fc_create_node_pod "$op_ns" "$name" "$secret" "$obj"
      ;;
  esac
  fc_put "$kind" "$ns" "$name" "$obj"
}

# Owner chain: controller pod -> ReplicaSet -> Deployment (apply.go:445-491).
_fc_create_workload_pods() { # <op_ns> <dep-name> <secret> <dep-json>
  local op_ns="$1" dep_name="$2" secret="$3" dep="$4"
  local dep_uid rs_uid waiting pod
  dep_uid=$(echo "$dep" | jq -r '.metadata.uid')
  rs_uid="fake-rs-uid-$RANDOM$RANDOM"
  fc_put "replicaset" "$op_ns" "${dep_name}-6f9b" \
    "$(jq -n --arg n "${dep_name}-6f9b" --arg ns "$op_ns" --arg uid "$rs_uid" --arg depuid "$dep_uid" --arg dname "$dep_name" \
      '{apiVersion:"apps/v1",kind:"ReplicaSet",metadata:{name:$n,namespace:$ns,uid:$uid,
        ownerReferences:[{apiVersion:"apps/v1",kind:"Deployment",name:$dname,uid:$depuid,controller:true}]}}')"
  waiting=$(_fc_derive_waiting "$op_ns" "$secret")
  pod=$(jq -n --arg ns "$op_ns" --arg rsuid "$rs_uid" --arg waiting "$waiting" --arg cname "$dep_name" \
    '{apiVersion:"v1",kind:"Pod",metadata:{name:"controller-pod-6f9b",namespace:$ns,
      uid:("fake-pod-uid-'"$RANDOM"'"),
      ownerReferences:[{apiVersion:"apps/v1",kind:"ReplicaSet",name:($cname+"-6f9b"),uid:$rsuid,controller:true}]},
     status:{"phase":"Pending",
       "containerStatuses":[{name:$cname,state:(if $waiting=="" then {"running":{}} else {"waiting":{"reason":$waiting}} end)}]}}')
  fc_put "pod" "$op_ns" "controller-pod-6f9b" "$pod"
}

_fc_create_node_pod() { # <op_ns> <ds-name> <secret> <ds-json>
  local op_ns="$1" ds_name="$2" secret="$3" ds="$4"
  local ds_uid waiting pod
  ds_uid=$(echo "$ds" | jq -r '.metadata.uid')
  waiting=$(_fc_derive_waiting "$op_ns" "$secret")
  pod=$(jq -n --arg ns "$op_ns" --arg dsuid "$ds_uid" --arg waiting "$waiting" --arg dname "$ds_name" \
    '{apiVersion:"v1",kind:"Pod",metadata:{name:"node-pod-abc",namespace:$ns,
      uid:("fake-pod-uid-'"$RANDOM"'"),
      ownerReferences:[{apiVersion:"apps/v1",kind:"DaemonSet",name:$dname,uid:$dsuid,controller:true}]},
     status:{"phase":"Pending",
       "containerStatuses":[{name:"node-driver-registrar",state:(if $waiting=="" then {"running":{}} else {"waiting":{"reason":$waiting}} end)}]}}')
  fc_put "pod" "$op_ns" "node-pod-abc" "$pod"
}

# Kubelet-side derivation the operator relies on (Secret API untouched: it never reads
# Secrets — the key check below models the KUBELET's env resolution, not the
# operator's): missing referenced secret -> CreateContainerConfigError;
# unavailable image (snapshot controller absent) -> ImagePullBackOff.
#
# BOUND to the renderer's secretKeyRef key set (nfs.go:476-479): the workload
# env block references ALL FOUR keys (storage-ips/pool-name/api-key/data-ip,
# nfs.go:565), so the kubelet refuses to start the container until every
# referenced key resolves. A Secret with `data:{}` therefore does NOT heal —
# presence alone is not readiness (the review's "heal on Secret presence"
# defect).
_fc_secret_key_complete() { # <op_ns> <secret-name> -> rc 0 iff all renderer keys present
  local j key
  j=$(fc_get "secret" "$1" "$2") || return 1
  for key in $(fc_secret_required_keys); do
    echo "$j" | jq -e --arg k "$key" '.data | has($k)' >/dev/null 2>&1 || return 1
  done
  return 0
}

_fc_derive_waiting() { # <op_ns> <secret-name>
  local op_ns="$1" secret="$2"
  if [[ -n "$secret" ]] && { ! fc_exists "secret" "$op_ns" "$secret" || ! _fc_secret_key_complete "$op_ns" "$secret"; }; then
    echo "CreateContainerConfigError"; return
  fi
  if [[ -f "$(fc_state_dir)/flags/image-unavailable" ]]; then
    echo "ImagePullBackOff"; return
  fi
  echo ""
}

# probeWorkloadReadiness (apply.go:328-434). ready | secret | image | rollout.
_fc_probe_workload() { # <op_ns> <secret>
  local op_ns="$1" secret="$2" waiting
  fc_get "deployment" "$op_ns" "$(fc_nfs_controller_dep)" >/dev/null || { echo "rollout"; return; }
  fc_get "daemonset" "$op_ns" "$(fc_nfs_node_ds)" >/dev/null || { echo "rollout"; return; }
  waiting=$(_fc_derive_waiting "$op_ns" "$secret")
  case "$waiting" in
    CreateContainerConfigError) echo "secret" ;;
    ImagePullBackOff|ErrImagePull) echo "image" ;;
    "") echo "ready" ;;
    *) echo "rollout" ;;
  esac
}

# --- status writers ----------------------------------------------------------
# Change guard + event-dedupe (statusEqual, controller:234-241; emitTransition
# events only when the write was allowed, controller:336-379): a no-change
# status produces neither a write nor an event.
_fc_status_commit() { # <cr-path> <new-json> <event-type> <event-reason> <event-msg>
  local p="$1" new="$2"
  [[ "$new" == "$(cat "$p")" ]] && return 0
  printf '%s\n' "$new" > "${p}.t" && mv "${p}.t" "$p"
  fc_event "$3" "$4" "$5"
}

# failedStatus (controller:318-328): Ready=False, Degraded=False, profiles
# cleared, specPlatform intentionally NOT advanced on validation failure.
_fc_set_failed_status() { # <cr-path> <reason> <message>
  local p="$1" reason="$2" msg="$3" gen new
  gen=$(jq -r '.metadata.generation // 1' "$p")
  new=$(jq --arg gen "$gen" --arg reason "$reason" --arg msg "$msg" '
    .status = {
      observedGeneration: ($gen|tonumber),
      specPlatform: "", resolvedPlatform: "", profiles: null,
      conditions: [
        {type:"Ready",status:"False",reason:$reason,message:$msg,lastTransitionTime:"now"},
        {type:"Progressing",status:"False",reason:$reason,message:"no progress until the spec is fixed",lastTransitionTime:"now"},
        {type:"Degraded",status:"False",reason:$reason,message:$msg,lastTransitionTime:"now"}
      ]
    }' "$p")
  _fc_status_commit "$p" "$new" "Warning" "$reason" "$msg"
}

# buildStatus with a profile failure (controller:291-296): aggregate Ready=False
# + Degraded=True, first failure reason carried.
_fc_set_blocked_status() { # <cr-path> <gen> <platform> <profile> <reason> <message>
  local p="$1" gen="$2" plat="$3" prof="$4" reason="$5" msg="$6" new
  new=$(jq --arg gen "$gen" --arg plat "$plat" --arg prof "$prof" --arg reason "$reason" --arg msg "$msg" '
    .status = {
      observedGeneration: ($gen|tonumber),
      specPlatform: "auto", resolvedPlatform: $plat,
 # Profile conditions carry res.Message verbatim (buildStatus,
 # quartet_controller.go:276-277); the AGGREGATE gets the
 # "profile <p> blocked: " summary (controller:282-286).
      profiles: { ($prof): { conditions: [
        {type:"Ready",status:"False",reason:$reason,message:$msg,lastTransitionTime:"now"},
        {type:"Progressing",status:"False",reason:$reason,message:"blocked until the Ready condition clears",lastTransitionTime:"now"},
        {type:"Degraded",status:"True",reason:$reason,message:$msg,lastTransitionTime:"now"} ] } },
      conditions: [
        {type:"Ready",status:"False",reason:$reason,message:("profile "+$prof+" blocked: "+$msg),lastTransitionTime:"now"},
        {type:"Progressing",status:"False",reason:$reason,message:("profile "+$prof+" blocked: "+$msg),lastTransitionTime:"now"},
        {type:"Degraded",status:"True",reason:$reason,message:("profile "+$prof+" blocked: "+$msg),lastTransitionTime:"now"}
      ]
    }' "$p")
  _fc_status_commit "$p" "$new" "Warning" "$reason" "profile ${prof}: ${msg}"
}

# buildStatus converged (controller:296-310): Ready=True reason=Ready.
_fc_set_ready_status() { # <cr-path> <gen> <platform> <message>
  local p="$1" gen="$2" plat="$3" msg="$4" new
  new=$(jq --arg gen "$gen" --arg plat "$plat" --arg msg "$msg" '
    .status = {
      observedGeneration: ($gen|tonumber),
      specPlatform: "auto", resolvedPlatform: $plat,
      profiles: { nfs: { conditions: [
        {type:"Ready",status:"True",reason:"Ready",message:$msg,lastTransitionTime:"now"},
        {type:"Progressing",status:"False",reason:"Ready",message:"operands converged",lastTransitionTime:"now"},
        {type:"Degraded",status:"False",reason:"Ready",message:"no failures observed",lastTransitionTime:"now"} ] } },
      conditions: [
        {type:"Ready",status:"True",reason:"Ready",message:$msg,lastTransitionTime:"now"},
        {type:"Progressing",status:"False",reason:"Ready",message:"nothing in progress",lastTransitionTime:"now"},
        {type:"Degraded",status:"False",reason:"Ready",message:"no failures observed",lastTransitionTime:"now"}
      ]
    }' "$p")
  _fc_status_commit "$p" "$new" "Normal" "Ready" "$msg"
}

# Kubelet reaction: when the referenced secret appears/disappears or the image
# flag changes, re-derive owned pod waiting states. This is what drives a real
# recovery (pod watch -> reconcile; the operator has NO secret watch, Secret API untouched).
fake_kubelet_react() {
  local cr_path op_ns secret p pod_path waiting newcs
  cr_path=$(fc_obj_path "quartet" "${TEST_NAMESPACE}" "$(fc_singleton_name)")
  [[ -f "$cr_path" ]] || return 0
  op_ns=$(fc_nfs_namespace)
  secret=$(jq -r '.spec.profiles.nfs.secretRef.name // ""' "$cr_path")
  for p in controller-pod-6f9b node-pod-abc; do
    pod_path=$(fc_obj_path "pod" "$op_ns" "$p")
    [[ -f "$pod_path" ]] || continue
    waiting=$(_fc_derive_waiting "$op_ns" "$secret")
    if [[ "$waiting" == "" ]]; then
      newcs='{"name":"c","state":{"running":{}}}'
    else
      newcs=$(jq -n --arg w "$waiting" '{"name":"c","state":{"waiting":{"reason":$w}}}')
    fi
    jq --argjson cs "$newcs" \
      '.status.phase=(if $cs.state.running then "Running" else "Pending" end) | .status.containerStatuses=[$cs]' \
      "$pod_path" > "${pod_path}.t" && mv "${pod_path}.t" "$pod_path"
  done
}

# Settle ONE CR: run reconcile passes until its object stabilizes or the pass
# budget is spent. Terminal reasons stop immediately; retryable reasons requeue
# (30s) and re-check — collapsed to immediate, bounded re-checks for offline
# determinism.
fake_settle_cr() { # <cr-path>
  local cr_path="$1" prev="" cur="" i retryable
  for i in $(seq 1 "${FAKE_RECONCILE_MAX_PASSES:-8}"); do
    [[ -f "$cr_path" ]] || return 0
    fake_reconcile_pass "$cr_path"
    retryable=$?
    cur=$(jq -S . "$cr_path")
    if [[ "$cur" == "$prev" ]]; then break; fi
    prev="$cur"
    if [[ $retryable -eq 0 ]]; then break; fi
  done
}

# Settle every Quartet object (watch model). Quartet is cluster-scoped
# (real CRD spec.scope: Cluster) — the store files it under obj/quartet/_/.
fake_reconcile() {
  local dir cr_path
  dir="$(fc_state_dir)/obj/quartet/_"
  [[ -d "$dir" ]] || return 0
  for cr_path in "$dir"/*.json; do
    [[ -e "$cr_path" ]] || continue
    fake_settle_cr "$cr_path"
  done
}

# Plural API token -> the lowercase kind token this store files objects under
# (the store layout keys on the singular form the case bodies address).
_fc_plural_to_kind() {
  case "$1" in
    namespaces)                echo namespace ;;
    nodes)                     echo node ;;
    persistentvolumes)         echo persistentvolume ;;
    priorityclasses)           echo priorityclass ;;
    csidrivers)                echo csidriver ;;
    volumeattachments)         echo volumeattachment ;;
    clusterroles)              echo clusterrole ;;
    clusterrolebindings)       echo clusterrolebinding ;;
    customresourcedefinitions) echo customresourcedefinition ;;
    configmaps)                echo configmap ;;
    secrets)                   echo secret ;;
    serviceaccounts)           echo serviceaccount ;;
    services)                  echo service ;;
    pods)                      echo pod ;;
    persistentvolumeclaims)    echo persistentvolumeclaim ;;
    leases)                    echo lease ;;
    deployments)               echo deployment ;;
    daemonsets)                echo daemonset ;;
    quartets)                  echo quartet ;;
    *)                         return 1 ;;
  esac
}

# Real DeleteOptions.body UID precondition — `delete --raw=<api-path> -f -`
# with the body on stdin (or -f <file>) (CASE-CONTRACT "Gerçek delete
# gövdesi"). Parses the body, resolves the raw path to kind/namespace/name,
# and enforces preconditions.uid SERVER-SIDE:
# * object absent → NotFound, rc 1;
# * GET-then A→B replacement (uid mismatch) → 409 Conflict, rc 1 — the
# object is FULLY preserved, nothing deleted, no deletion record;
# * match → deleted (rc 0, postcondition
# is the caller's to verify).
# Test arm: <state>/flags/replace-before-delete carrying a replacement UID
# swaps the object AFTER the caller's get and BEFORE the precondition is
# evaluated (models the A→B race window the server-side check exists for).
_fake_delete_raw() { # <raw-path> [<body-file-or-->] — body on stdin when "-"
  local raw="$1" f="${2:--}"
  local body klc ns="" plural="" name="" rest segs=()
  if [[ "$raw" == /api/v1/* ]]; then
    rest="${raw#/api/v1/}"
  elif [[ "$raw" == /apis/* ]]; then
    rest="${raw#/apis/}"; rest="${rest#*/}"; rest="${rest#*/}" # group, version
  else
    echo "Error from server (NotFound): the server could not find the requested resource" >&2
    return 1
  fi
  IFS='/' read -r -a segs <<< "$rest"
  if [[ ${#segs[@]} -eq 2 ]]; then
    plural="${segs[0]}"; name="${segs[1]}"
  elif [[ ${#segs[@]} -eq 4 && "${segs[0]}" == "namespaces" ]]; then
    ns="${segs[1]}"; plural="${segs[2]}"; name="${segs[3]}"
  else
    echo "Error from server (NotFound): the server could not find the requested resource" >&2
    return 1
  fi
  if ! klc="$(_fc_plural_to_kind "$plural")"; then
    echo "Error from server (NotFound): the server could not find the requested resource" >&2
    return 1
  fi
 # Strict scope routing: a namespaced segment on a cluster-scoped kind (or a
 # missing one on a namespaced kind) is an UNKNOWN route — NotFound, exactly
 # like the real API. Quartet is cluster-scoped (real CRD spec.scope).
  if fc_is_cluster_scoped "$klc" && [[ -n "$ns" ]]; then
    echo "Error from server (NotFound): the server could not find the requested resource" >&2
    return 1
  fi
  if ! fc_is_cluster_scoped "$klc" && [[ -z "$ns" ]]; then
    echo "Error from server (NotFound): the server could not find the requested resource" >&2
    return 1
  fi
  if [[ "$f" == "-" ]]; then
    body="$(cat)"
  elif [[ -f "$f" ]]; then
    body="$(cat "$f")"
  else
    echo "error: open $f: no such file or directory" >&2
    return 1
  fi
  if [[ "$(echo "$body" | jq -r '.kind // empty' 2>/dev/null)" != "DeleteOptions" ]]; then
    echo "error: the body of a raw delete must be a DeleteOptions object" >&2
    return 1
  fi
  local prec
  prec="$(echo "$body" | jq -r '.preconditions.uid // empty' 2>/dev/null)"

  local fpath
  fpath="$(fc_obj_path "$klc" "$ns" "$name")"
  if [[ ! -f "$fpath" ]]; then
    echo "Error from server (NotFound): ${klc} \"$name\" not found" >&2
    return 1
  fi
  local armf rep live_uid
  armf="$(fc_state_dir)/flags/replace-before-delete"
  if [[ -n "$prec" && -f "$armf" ]]; then
    rep="$(cat "$armf")"
    jq --arg uid "$rep" \
      '.metadata.uid = $uid | .metadata.annotations = ((.metadata.annotations // {}) + {"scenario-offline/replaced": "yes"})' \
      "$fpath" > "${fpath}.t" && mv "${fpath}.t" "$fpath"
  fi
  live_uid="$(jq -r '.metadata.uid // empty' "$fpath")"
  if [[ -n "$prec" && "$live_uid" != "$prec" ]]; then
 # 409 Conflict: the server-side UID precondition failed — B's fields are
 # preserved, nothing is deleted.
    echo "Error from server (Conflict): ${klc} \"$name\" — UID precondition failed: the object has been modified" >&2
    return 1
  fi
  fc_delete "$klc" "$ns" "$name" || return 1
  _fake_after_mutation
  return 0
}

# --- scenario_kube verb surface (kubectl-like) --------------------------------
fake_scenario_kube() {
  local verb="${1:-}"; shift || true
  case "$verb" in
    create|apply)
      local f="" body kind name ns klc out_json=0
      while [[ $# -gt 0 ]]; do
        case "$1" in
          -f|--filename) f="${2:-}"; shift 2 ;;
          -o|--output) [[ "${2:-}" == "json" ]] && out_json=1; shift 2 ;;
          *) shift ;;
        esac
      done
      [[ -n "$f" ]] || { echo "$verb: missing -f" >&2; return 2; }
 # Read the manifest in memory — never mutate the caller's file (kubectl
 # semantics; process-substitution -f targets are not writable).
      body=$(cat "$f")
      kind=$(echo "$body" | jq -r '.kind'); name=$(echo "$body" | jq -r '.metadata.name')
      ns=$(echo "$body" | jq -r '.metadata.namespace // ""')
      klc=$(echo "$kind" | tr '[:upper:]' '[:lower:]')
      if fc_exists "$klc" "$ns" "$name"; then
 # create -> AlreadyExists (POST); apply -> 409 Conflict (no-force SSA
 # refuses a pre-existing object; adoption is never attempted, no adoption / ownership gate).
        if [[ "$verb" == "apply" ]]; then
          echo "Error from server (Conflict): ${klc} \"$name\" — no-force apply refuses the pre-existing object" >&2
        else
          echo "Error from server (AlreadyExists): ${klc} \"$name\" already exists" >&2
        fi
        return 1
      fi
      [[ "$klc" == "quartet" ]] && body=$(echo "$body" | jq '.metadata.generation = 1')
      fc_put "$klc" "$ns" "$name" "$body"
      _fake_after_mutation
 # kubectl create -o json prints the created object (uid/rv stamped):
 # the case captures OWNERSHIP from this response (CASE-CONTRACT pin 8).
      (( out_json )) && fc_get "$klc" "$ns" "$name"
      return 0
      ;;
    get)
      local kind name ns="" output="json" klc obj
      [[ "${1:-}" != -* ]] && kind="${1:-}"; shift || true
      [[ "${1:-}" != -* ]] && name="${1:-}"; shift || true
      while [[ $# -gt 0 ]]; do case "$1" in -n|--namespace) ns="${2:-}"; shift 2;; -o|--output) output="${2:-}"; shift 2;; *) shift;; esac; done
      klc=$(echo "$kind" | tr '[:upper:]' '[:lower:]')
      if [[ "$klc" == "quartet" ]]; then
        fake_kubelet_react; fake_reconcile # watch model: reading lets it settle
      fi
      if ! obj=$(fc_get "$klc" "$ns" "$name"); then
        echo "Error from server (NotFound): ${klc} \"$name\" not found" >&2
        return 1
      fi
      echo "$obj"
      return 0
      ;;
    list)
      local kind ns="" klc dir obj items=()
      kind="${1:-}"; shift || true
      while [[ $# -gt 0 ]]; do case "$1" in -n|--namespace) ns="${2:-}"; shift 2;; -o|--output) shift 2;; *) shift;; esac; done
      klc=$(echo "$kind" | tr '[:upper:]' '[:lower:]')
      if fc_is_cluster_scoped "$klc"; then dir="$(fc_state_dir)/obj/${klc}/_"; else dir="$(fc_state_dir)/obj/${klc}/${ns:-_}"; fi
      if [[ -d "$dir" ]]; then
        for obj in "$dir"/*.json; do [[ -e "$obj" ]] && items+=("$(cat "$obj")"); done
      fi
      if [[ ${#items[@]} -eq 0 ]]; then
        echo '{"apiVersion":"v1","kind":"List","items":[]}'
      else
        printf '%s\n' "${items[@]}" | jq -s '{"apiVersion":"v1","kind":"List","items":.}'
      fi
      return 0
      ;;
    delete)
      local kind="" name="" ns="" klc ignore_nf=0
      local fs="" raw="" f=""
      local -a dpos=()
      while [[ $# -gt 0 ]]; do
        case "$1" in
          -n|--namespace) ns="${2:-}"; shift 2 ;;
          --ignore-not-found) ignore_nf=1; shift ;;
          --field-selector) fs="${2:-}"; shift 2 ;;
          --field-selector=*) fs="${1#--field-selector=}"; shift ;;
          --raw) raw="${2:-}"; shift 2 ;;
          --raw=*) raw="${1#--raw=}"; shift ;;
          -f|--filename) f="${2:-}"; shift 2 ;;
          -*) shift ;;
          *) dpos+=("$1"); shift ;;
        esac
      done
      kind="${dpos[0]:-}"; name="${dpos[1]:-}"
      if [[ -z "$raw" && -z "$kind" ]]; then
        echo "delete: need <kind> <name> or --raw=<api-path>" >&2
        return 2
      fi
      if [[ -n "$fs" ]]; then
 # Real kubectl refuses name + field-selector BEFORE the API is ever
 # reached — the pinned client/API model (CASE-CONTRACT "Gerçek
 # delete gövdesi", ): a field selector is not a UID
 # precondition (it only pre-filters a list). The UID precondition
 # travels in the DeleteOptions.body via --raw, enforced server-side.
        echo "error: name cannot be provided when a selector is specified" >&2
        return 1
      fi
      if [[ -n "$raw" ]]; then
        _fake_delete_raw "$raw" "$f"
        return $?
      fi
      klc=$(echo "$kind" | tr '[:upper:]' '[:lower:]')
      if ! fc_delete "$klc" "$ns" "$name"; then
        (( ignore_nf )) && return 0   # kubectl --ignore-not-found semantics
        echo "Error from server (NotFound): ${klc} \"$name\" not found" >&2
        return 1
      fi
      _fake_after_mutation
      return 0
      ;;
    replace)
 # kubectl replace -f: PUT semantics — the object MUST exist (uid kept).
      local f="" kind name ns klc body
      while [[ $# -gt 0 ]]; do case "$1" in -f) f="${2:-}"; shift 2;; *) shift;; esac; done
      [[ -n "$f" ]] || { echo "replace: missing -f" >&2; return 2; }
      body=$(cat "$f")
      kind=$(echo "$body" | jq -r '.kind'); name=$(echo "$body" | jq -r '.metadata.name')
      ns=$(echo "$body" | jq -r '.metadata.namespace // ""')
      klc=$(echo "$kind" | tr '[:upper:]' '[:lower:]')
      local p; p=$(fc_obj_path "$klc" "$ns" "$name")
      if [[ ! -f "$p" ]]; then
        echo "Error from server (NotFound): ${klc} \"$name\" not found" >&2
        return 1
      fi
 # Preserve the stored uid (PUT keeps identity); bump rv via stamp helper.
      body=$(jq --slurpfile cur "$p" '(.metadata.uid) = $cur[0].metadata.uid' <<<"$body")
      fc_put "$klc" "$ns" "$name" "$body"
      _fake_after_mutation
      return 0
      ;;
    annotate)
      local kind name ns="" kv="" klc k v p
      kind="${1:-}"; name="${2:-}"; shift 2 || true
      while [[ $# -gt 0 ]]; do case "$1" in -n|--namespace) ns="${2:-}"; shift 2;; *) kv="$1"; shift;; esac; done
      [[ -n "$kv" ]] || { echo "annotate: missing key=value" >&2; return 2; }
      klc=$(echo "$kind" | tr '[:upper:]' '[:lower:]')
      k="${kv%%=*}"; v="${kv#*=}"
      p=$(fc_obj_path "$klc" "$ns" "$name")
      [[ -f "$p" ]] || { echo "Error from server (NotFound): ${klc} \"$name\" not found" >&2; return 1; }
      jq --arg k "$k" --arg v "$v" '.metadata.annotations=((.metadata.annotations // {})+{($k):$v})' "$p" > "${p}.t" && mv "${p}.t" "$p"
      _fake_after_mutation
      return 0
      ;;
    *)
      echo "scenario_kube (offline fake): unsupported verb '$verb'" >&2
      return 2
      ;;
  esac
}

_fake_after_mutation() {
  fake_kubelet_react
  fake_reconcile
}

# --- fixture control ---------------------------------------------------------
fake_reset() {
  FAKE_STATE_DIR="${FAKE_STATE_DIR:?}"
  mkdir -p "$FAKE_STATE_DIR/deleted"
 # Archive stale state instead of deleting it (no-rm machine rule): a
 # re-run against the same dir must start from a truly empty cluster.
  if [[ -d "$FAKE_STATE_DIR/obj" ]]; then
    mv "$FAKE_STATE_DIR/obj" "$FAKE_STATE_DIR/deleted/obj.$(date +%s%N)"
  fi
  if [[ -d "$FAKE_STATE_DIR/flags" ]]; then
    mv "$FAKE_STATE_DIR/flags" "$FAKE_STATE_DIR/deleted/flags.$(date +%s%N)"
  fi
  mkdir -p "$FAKE_STATE_DIR/obj" "$FAKE_STATE_DIR/flags"
  : > "$FAKE_STATE_DIR/events.tsv"
}

# Backend Secret carrying ALL four renderer key names (nfs.go:476-479) with
# SYNTHETIC values: the offline model binds on key presence only (the kubelet
# env-resolution step), and the operator's Ready bar is itself "not a backend
# I/O proof" (quartet_controller.go:297-302). Contract-seeded test doubles,
# never a claim about a real backend.
fake_complete_backend_secret() { # <name> <op_ns>
  fc_put "secret" "$2" "$1" "$(jq -n --arg n "$1" --arg ns "$2" \
    '{apiVersion:"v1",kind:"Secret",type:"Opaque",
      metadata:{name:$n,namespace:$ns},
      data:{"storage-ips":"MTI3LjAuMC4x","pool-name":"cXVhcnRldC1wb29s",
            "api-key":"cXVhcnRldC1zeW50aGV0aWMta2V5","data-ip":"MTI3LjAuMC4y"}}')"
  _fake_after_mutation
}

fake_set_image_unavailable()   { touch "$(fc_state_dir)/flags/image-unavailable"; _fake_after_mutation; }
fake_clear_image_unavailable() {
  local f="$(fc_state_dir)/flags/image-unavailable"
  [[ -f "$f" ]] && mv "$f" "$(fc_state_dir)/deleted/image-flag.$(date +%s%N)"
  _fake_after_mutation
}
