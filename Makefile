# Quartet operator — build/test/generation entry points.
#
# Scratch/build artifacts are kept under a user-owned cache directory
# (TMPDIR/GOCACHE exports); /tmp and /var/tmp are not used.

export TMPDIR ?= $(HOME)/.cache/ngxstorage
export GOCACHE ?= $(TMPDIR)/gocache

IMAGE        ?= quay.io/ngxstorage/quartet-operator:v0.0.0-dev
VCS_REF      ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
KUSTOMIZE    ?= kustomize
# controller-gen resolves through the go.mod `tool` pin (sigs.k8s.io/controller-tools v0.16.5).
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen
SETUP_ENVTEST  := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
ENVTEST_BIN_DIR := $(TMPDIR)/kubebuilder-assets
# envtest API server version: 1.33.0, inside the candidate test matrix
# (K8s 1.30–1.37); pin explicitly instead of
# setup-envtest's default latest. One pinned version proves nothing about
# the range — see test-matrix / hack/envtest-matrix.sh (Layer 2).
ENVTEST_K8S_VERSION ?= 1.33.0

.PHONY: all
all: build

##@ codegen

.PHONY: manifests
manifests: ## Regenerate the CRD (config/crd/bases) and RBAC role (config/rbac/role.yaml) from markers.
	# crd reads the api package; rbac reads the reconciler's kubebuilder:rbac
	# markers (paths=./... covers both without leaking controller code into
	# the CRD schema).
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths="./api/..." output:crd:dir=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=quartet-manager-role paths="./..." output:rbac:dir=config/rbac

.PHONY: generate
generate: ## Regenerate zz_generated.deepcopy.go from API markers.
	$(CONTROLLER_GEN) object paths="./api/..."

##@ build

.PHONY: build
build: ## Build the manager binary into bin/manager.
	CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/manager ./cmd/manager

.PHONY: run
run: ## Run the manager locally against the current kubeconfig (no leader election).
	go run ./cmd/manager --leader-elect=false

.PHONY: docker-build
docker-build: build ## Build the container image (pinned bases; VCS_REF label).
	docker build --build-arg VCS_REF=$(VCS_REF) -t $(IMAGE) .

##@ test

.PHONY: test
test: ## Unit tests. envtest: QUARTET_TEST_ENVTEST=required|optional (default: required when KUBEBUILDER_ASSETS is set, optional otherwise).
	go test -count=1 ./...

.PHONY: test-race
test-race: ## Race-enabled unit tests.
	go test -race -count=1 ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test-envtest
test-envtest: ## Download envtest assets via setup-envtest and run the envtest suite.
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path --bin-dir $(ENVTEST_BIN_DIR) > /dev/null
	KUBEBUILDER_ASSETS=$$( $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path --bin-dir $(ENVTEST_BIN_DIR) ) \
		go test -count=1 -v ./internal/controller/ -run 'TestEnvtest' -timeout 10m

ENVTEST_MATRIX_VERSIONS := 1.30.0 1.31.0 1.32.0 1.33.0 1.34.0 1.35.0 1.36.0 1.37.0

.PHONY: test-matrix
test-matrix: ## Layer 2: run the full suite once per candidate K8s version (1.30-1.37) against that version's exact asset dir; records PASS/FAIL/BLOCKED per version (SKIP never masquerades as PASS).
	./hack/envtest-matrix.sh $(ENVTEST_MATRIX_VERSIONS)

##@ verify

.PHONY: verify
verify: vet test-race ## Full gate: vet + race unit tests.

.PHONY: golden-verify
golden-verify: ## Re-render the upstream NFS raw manifests (kustomize v5.7.1) and field-compare against the golden fixtures (approved-transforms mirror, regeneration-independent).
	./hack/verify-golden.sh

##@ provenance

.PHONY: provenance-check
provenance-check: ## provenance: verify module pins and build with a clean module cache, and check the local image's VCS labels. Network-bound checks report BLOCKED instead of claiming success.
	./hack/provenance-check.sh

##@ install

# Install selection (explicit and verified): PLATFORM picks the platform
# entrypoint of the operand install set, PROFILE picks the profile:
# k8s → config/operands/<PROFILE>/k8s (rbac.yaml),
# ocp → config/operands/<PROFILE>/ocp (rbac-ocp.yaml superset: node SCC
# use grant).
# PROFILE=nfs|fc|iscsi — EXPLICIT selection required, no default profile
# (NFS is not the default). The
# selection installs ONLY the chosen profile's set (deterministic order
# inside each set: namespace → RBAC); it does NOT claim or apply all
# three. A multi-profile install runs the target once per profile — the
# sets are disjoint (distinct namespaces/object names), so applying each
# converges to the union. The full single-artifact path per selection is
# `make install-artifacts PLATFORM=<k8s|ocp> PROFILE=<nfs|fc|iscsi>
# MANAGER_IMAGE=<repo@sha256:...>` (hack/install-artifacts.sh with
# QUARTET_INSTALL_PROFILE; compositions config/default-nfs{,-ocp},
# config/default-fc{,-ocp}, config/default-iscsi{,-ocp}).
PLATFORM ?= k8s
PROFILE  ?=
# MANAGER_IMAGE: the manager image for install-artifacts — must be an
# immutable repo@sha256:<digest> reference (v1alpha1 image.reference
# contract; enforced with RC=2 before any render/mutation).
MANAGER_IMAGE ?=

.PHONY: install-operands
install-operands: ## Apply ONE selected profile's operand namespace + RBAC set out-of-band as the cluster admin (PLATFORM=k8s|ocp, PROFILE=nfs|fc|iscsi REQUIRED — no default).
	@test "$(PLATFORM)" = "k8s" -o "$(PLATFORM)" = "ocp" || { echo "install-operands: PLATFORM must be k8s or ocp (got '$(PLATFORM)')"; exit 2; }
	@test "$(PROFILE)" = "nfs" -o "$(PROFILE)" = "fc" -o "$(PROFILE)" = "iscsi" || { echo "install-operands: PROFILE must be nfs, fc or iscsi (got '$(PROFILE)')"; exit 2; }
	kubectl apply -k config/operands/$(PROFILE)/$(PLATFORM)

.PHONY: install-artifacts
install-artifacts: ## Render ONE platform-complete, digest-pinned, phase-split install artifact for the selected profile (PLATFORM=k8s|ocp, PROFILE=nfs|fc|iscsi; MANAGER_IMAGE=<repo@sha256:...> required; KUBEBUILDER_ASSETS with a kubectl enables offline pinning).
	@test "$(MANAGER_IMAGE)" != "" || { echo "install-artifacts: MANAGER_IMAGE=<repo@sha256:...> is required"; exit 2; }
	@test "$(PLATFORM)" = "k8s" -o "$(PLATFORM)" = "ocp" || { echo "install-artifacts: PLATFORM must be k8s or ocp (got '$(PLATFORM)')"; exit 2; }
	@test "$(PROFILE)" = "nfs" -o "$(PROFILE)" = "fc" -o "$(PROFILE)" = "iscsi" || { echo "install-artifacts: PROFILE must be nfs, fc or iscsi (got '$(PROFILE)')"; exit 2; }
	# Command-line make variables do NOT reach the recipe environment
	# (make expands them in the recipe but does not export them) — pass
	# them through explicitly so a command-line KUBEBUILDER_ASSETS wins
	# over PATH lookup, exactly as the script contract requires.
	QUARTET_INSTALL_PROFILE=$(PROFILE) KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" \
	    bash hack/install-artifacts.sh $(PLATFORM) "$(MANAGER_IMAGE)"

