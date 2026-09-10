# Basic Makefile for beskar7

# Go parameters
GOPATH:=$(shell go env GOPATH)
GOBIN=$(firstword $(subst :, ,${GOPATH}))/bin
GO ?= go

# Controller-gen tool
CONTROLLER_GEN = $(GOBIN)/controller-gen

# Kustomize tool
KUSTOMIZE ?= kustomize

# Image URL to use all building/pushing image targets
VERSION ?= v0.5.0
IMAGE_REGISTRY ?= ghcr.io/projectbeskar/beskar7
IMAGE_REPO ?= beskar7
IMG ?= $(IMAGE_REGISTRY)/$(IMAGE_REPO):$(VERSION)

# clusterctl variable syntax (${VAR:=default}, drone/envsubst) resolved to the
# default for consumers that never run envsubst: `make deploy` and the
# plain-kubectl release manifest. `clusterctl init` gets the raw file.
RESOLVE_CLUSTERCTL_DEFAULTS = sed -E 's/\$$\{[A-Za-z_][A-Za-z0-9_]*:=([^}]*)\}/\1/g'

# Where `make clusterctl-override` writes a local clusterctl repository.
CLUSTERCTL_OVERRIDES ?= $(HOME)/.cluster-api/overrides

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "generateEmbeddedObjectMeta=true,maxDescLen=0"

# Build the manager binary. Builds the package (./cmd/manager) instead of
# just main.go so additional .go files in the package (e.g. flags.go) are
# picked up.
build:
	$(GO) build -o bin/manager ./cmd/manager

# Build the mock Redfish server binary.
build-mock-redfish:
	$(GO) build -o bin/mock-redfish ./cmd/mock-redfish

# Build the mock inspector binary.
build-mock-inspector:
	$(GO) build -o bin/mock-inspector ./cmd/mock-inspector

# Run code generators
generate:
	$(GO) generate ./...

# Install controller-gen.
# Pinned to v0.21.0 so the CRD `controller-gen.kubebuilder.io/version` annotation
# stays stable across machines. Bumping to @latest caused every PR's
# "Generate and Validate Manifests" job to fail with a one-line annotation diff
# (controller-tools releases tend to bump that annotation on every release).
# To upgrade: bump the pin, run `make manifests && make sync-chart-crds`,
# commit the regenerated YAML alongside the Makefile change.
CONTROLLER_GEN_VERSION ?= v0.21.0
install-controller-gen:
	$(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

# Install golangci-lint pinned to the version CI uses. .golangci.yml is v2
# schema; this target installs the matching v2 binary so local lint matches
# CI. The v2 module path includes /v2/.
GOLANGCI_LINT = $(GOBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2
install-golangci-lint:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Run linters with the pinned v2 binary (matches CI).
lint: install-golangci-lint
	$(GOLANGCI_LINT) run --timeout=5m

# Generate manifests e.g. CRDs, RBAC, webhook configurations, and DeepCopy objects
manifests: install-controller-gen
	$(CONTROLLER_GEN) object:headerFile="./hack/boilerplate.go.txt" paths="./..."
	$(MAKE) rbac crd webhook

# Generate RBAC manifests
rbac:
	$(CONTROLLER_GEN) rbac:roleName=capb7-manager-role paths="./..." output:rbac:dir=config/rbac

# Generate CRD manifests
crd:
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true,maxDescLen=0,crdVersions=v1 paths="./api/..." output:crd:artifacts:config=config/crd/bases

# Generate the webhook configurations (config/webhook/manifests.yaml) from the
# +kubebuilder:webhook and +kubebuilder:webhookconfiguration markers in
# api/*/webhooks. Generated, not hand-edited: a path declared there that no Go
# handler serves is a failurePolicy=Fail webhook answering 404, which blocks
# every create/update of that kind. test/contract pins the Helm chart's
# hand-maintained copy to the same markers.
webhook:
	$(CONTROLLER_GEN) webhook paths="./..." output:webhook:dir=config/webhook

# Sync chart-bundled CRDs from the generated source of truth.
# Run this after `make manifests` (or use `make manifests-and-sync`) so that
# charts/beskar7/crds/ always matches config/crd/bases/. The stray _.yaml stub
# is removed if present.
sync-chart-crds:
	cp config/crd/bases/*.yaml charts/beskar7/crds/
	@rm -f charts/beskar7/crds/_.yaml

# Convenience target: regenerate manifests then sync the chart CRDs.
# Use this instead of plain `make manifests` when the chart is in scope.
manifests-and-sync: manifests sync-chart-crds

# Run tests
# The envtest suites need the real CAPI CRDs (Cluster, Machine) at the version
# we compile against; hand-maintained stubs drift. Copied from the module cache.
CAPI_MODULE_DIR = $(shell $(GO) list -m -f '{{.Dir}}' sigs.k8s.io/cluster-api)
test-external-crds:
	cp $(CAPI_MODULE_DIR)/config/crd/bases/cluster.x-k8s.io_clusters.yaml config/test-external-crds/
	cp $(CAPI_MODULE_DIR)/config/crd/bases/cluster.x-k8s.io_machines.yaml config/test-external-crds/
	chmod u+w config/test-external-crds/*.yaml

test:
	$(GO) test ./... -coverprofile cover.out

# Docker build for linux/amd64
docker-build:
	# Ensure you have a buildx builder configured that supports cross-compilation
	# e.g., docker buildx create --use
	docker buildx build --platform linux/amd64 -t $(IMG) --load .

# Docker build for mock Redfish server
docker-build-mock-redfish:
	docker buildx build --platform linux/amd64 -t $(IMAGE_REGISTRY)/mock-redfish:$(VERSION) --load -f Dockerfile.mock-redfish .

# Docker build for mock inspector
docker-build-mock-inspector:
	docker buildx build --platform linux/amd64 -t $(IMAGE_REGISTRY)/mock-inspector:$(VERSION) --load -f Dockerfile.mock-inspector .

# Layered smoke test against the current kubectl context. Requires the
# beskar7 chart to already be installed (helm install ...) and cert-manager
# + CAPI core to be present. Exercises:
#   1. Static install   - operator pod Running, CRDs present
#   2. Admission        - CRD validation rejects malformed addresses
#   3. Reconcile        - mock BMC + PhysicalHost -> Status.Ready=true
#   4. CAPI claim       - Beskar7Machine claims the host, ProviderID set
# Layer 5 (PXE/inspector callback) requires a real iPXE boot path and is
# out of scope for this rig. See hack/smoke/run.sh for flags (--keep,
# --teardown, MOCK_IMAGE=...).
smoke:
	bash hack/smoke/run.sh

# Smoke test plus the watch-namespaces isolation check (layer 6). Only
# meaningful when the operator was installed with watchNamespaces set; the
# layer self-skips otherwise. See docs and SEC-2 (charts watchNamespaces).
smoke-watch-namespaces:
	bash hack/smoke/run.sh --with-isolation

# Tear down smoke-test fixtures without running the suite.
smoke-teardown:
	bash hack/smoke/run.sh --teardown

# Docker push (uses IMG variable defined at the top)
docker-push:
	docker push $(IMG)

# Deploy to Kubernetes
# deploy: manifests
# 	kubectl apply -k config/default

# Install CRDs into the cluster
install:
	$(MAKE) manifests
	kustomize build config/crd | kubectl apply -f -

# Uninstall CRDs from the cluster
uninstall:
	$(MAKE) manifests
	kustomize build config/crd | kubectl delete -f -

# Deploy controller to the cluster specified in ~/.kube/config
deploy:
	$(MAKE) manifests
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | $(RESOLVE_CLUSTERCTL_DEFAULTS) | kubectl apply -f -

# Undeploy controller from the cluster specified in ~/.kube/config
undeploy:
	$(KUSTOMIZE) build config/default | $(RESOLVE_CLUSTERCTL_DEFAULTS) | kubectl delete -f -

# Generate the release manifests: infrastructure-components.yaml (clusterctl,
# keeps ${VAR:=default}) and beskar7-manifests-$(VERSION).yaml (plain kubectl,
# defaults resolved). VERSION is stamped into the image tag and the version
# label of temporary copies of the kustomizations; the originals are restored
# afterwards from a scratch directory — never with `git checkout`, which would
# also throw away uncommitted edits to those files.
release-manifests:
	$(MAKE) manifests
	@set -e; \
	files="config/default/kustomization.yaml config/manager/kustomization.yaml $$(find config/overlays -name kustomization.yaml)"; \
	tmp=$$(mktemp -d); \
	for f in $$files; do \
	  mkdir -p "$$tmp/$$(dirname $$f)"; cp "$$f" "$$tmp/$$f"; \
	  sed -i -E 's/app\.kubernetes\.io\/version: v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?/app.kubernetes.io\/version: $(VERSION)/g; s/newTag: v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?/newTag: $(VERSION)/g' "$$f"; \
	done; \
	rc=0; $(KUSTOMIZE) build config/default > infrastructure-components.yaml || rc=$$?; \
	for f in $$files; do cp "$$tmp/$$f" "$$f"; done; rm -rf "$$tmp"; \
	if [ $$rc -ne 0 ]; then echo "kustomize build failed ($$rc)"; rm -f infrastructure-components.yaml; exit $$rc; fi; \
	$(RESOLVE_CLUSTERCTL_DEFAULTS) infrastructure-components.yaml > beskar7-manifests-$(VERSION).yaml; \
	echo "Release manifests generated: infrastructure-components.yaml (clusterctl) and beskar7-manifests-$(VERSION).yaml (kubectl)"

# Publish the release assets into a clusterctl local repository so that
# `clusterctl init --infrastructure beskar7:$(VERSION)` installs this tree.
# VERSION must be a semantic version (the directory name is one).
clusterctl-override: release-manifests
	mkdir -p $(CLUSTERCTL_OVERRIDES)/infrastructure-beskar7/$(VERSION)
	cp infrastructure-components.yaml metadata.yaml $(CLUSTERCTL_OVERRIDES)/infrastructure-beskar7/$(VERSION)/
	@echo "clusterctl local repository: $(CLUSTERCTL_OVERRIDES)/infrastructure-beskar7/$(VERSION)"
	@echo "install with: clusterctl init --infrastructure beskar7:$(VERSION)  (provider 'beskar7' must be in your clusterctl config, see docs/installation.md)"

.PHONY: build build-mock-redfish build-mock-inspector generate manifests test lint docker-build docker-build-mock-redfish docker-build-mock-inspector docker-push deploy install-controller-gen install-golangci-lint install uninstall undeploy rbac crd webhook release-manifests clusterctl-override sync-chart-crds manifests-and-sync smoke smoke-teardown