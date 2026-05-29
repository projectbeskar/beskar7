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
VERSION ?= v0.4.0-alpha.5
IMAGE_REGISTRY ?= ghcr.io/projectbeskar/beskar7
IMAGE_REPO ?= beskar7
IMG ?= $(IMAGE_REGISTRY)/$(IMAGE_REPO):$(VERSION)

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

# Install golangci-lint pinned to the version CI uses. .golangci.yml targets
# v1 schema; installing a v2 binary will reject the config. Use this target
# instead of `go install` so local lint matches CI.
GOLANGCI_LINT = $(GOBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v1.64.8
install-golangci-lint:
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Run linters. Pins to the CI version via install-golangci-lint so a system-
# installed v2 binary doesn't reject .golangci.yml's v1 syntax.
lint: install-golangci-lint
	$(GOLANGCI_LINT) run --timeout=5m

# Generate manifests e.g. CRDs, RBAC, and DeepCopy objects
manifests: install-controller-gen
	$(CONTROLLER_GEN) object:headerFile="./hack/boilerplate.go.txt" paths="./..."
	$(MAKE) rbac crd

# Generate RBAC manifests
rbac:
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./..." output:rbac:dir=config/rbac

# Generate CRD manifests
crd:
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true,maxDescLen=0,crdVersions=v1 paths="./api/..." output:crd:artifacts:config=config/crd/bases

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
	$(KUSTOMIZE) build config/default | kubectl apply -f -

# Undeploy controller from the cluster specified in ~/.kube/config
undeploy:
	$(KUSTOMIZE) build config/default | kubectl delete -f -

# Generate a single manifest file for a release
release-manifests:
	$(MAKE) manifests # Ensure CRDs and RBAC are up-to-date
	# Update kustomization.yaml files with current VERSION before building
	# Pattern matches versions with optional suffixes like -alpha, -beta, -rc1, etc.
	sed -i.bak 's/app\.kubernetes\.io\/version: v[0-9]\+\.[0-9]\+\.[0-9]\+\(-[a-zA-Z0-9]\+\)*/app.kubernetes.io\/version: $(VERSION)/g' config/default/kustomization.yaml
	sed -i.bak 's/newTag: v[0-9]\+\.[0-9]\+\.[0-9]\+\(-[a-zA-Z0-9]\+\)*/newTag: $(VERSION)/g' config/default/kustomization.yaml
	# Update manager kustomization (critical for image tag)
	sed -i.bak 's/newTag: v[0-9]\+\.[0-9]\+\.[0-9]\+\(-[a-zA-Z0-9]\+\)*/newTag: $(VERSION)/g' config/manager/kustomization.yaml
	# Update overlay files too
	find config/overlays -name "kustomization.yaml" -exec sed -i.bak 's/app\.kubernetes\.io\/version: v[0-9]\+\.[0-9]\+\.[0-9]\+\(-[a-zA-Z0-9]\+\)*/app.kubernetes.io\/version: $(VERSION)/g' {} \;
	find config/overlays -name "kustomization.yaml" -exec sed -i.bak 's/newTag: v[0-9]\+\.[0-9]\+\.[0-9]\+\(-[a-zA-Z0-9]\+\)*/newTag: $(VERSION)/g' {} \;
	# CRITICAL: Delete backup files BEFORE kustomize runs (it tries to parse them)
	find config/ -name "*.yaml.bak" -delete 2>/dev/null || true
	# Build manifests (now that .bak files are gone)
	$(KUSTOMIZE) build config/default > beskar7-manifests-$(VERSION).yaml
	# Restore original kustomization.yaml files using git
	git checkout config/default/kustomization.yaml config/manager/kustomization.yaml 2>/dev/null || true
	git checkout config/overlays/ 2>/dev/null || true
	@echo "Release manifests generated: beskar7-manifests-$(VERSION).yaml"

.PHONY: build build-mock-redfish build-mock-inspector generate manifests test lint docker-build docker-build-mock-redfish docker-build-mock-inspector docker-push deploy install-controller-gen install-golangci-lint install uninstall undeploy rbac crd release-manifests sync-chart-crds manifests-and-sync smoke smoke-teardown