# CubePilot development Makefile -- a thin orchestration layer over the
# existing tooling (go / npm / docker / helm). It does NOT replace
# scripts/setup.sh (one-shot local deploy); it covers the per-task commands
# developers run repeatedly.
#
#   make build      build both Go binaries into bin/
#   make test       go vet + unit tests (excludes the test/e2e suite)
#   make test-e2e   bring up the stack (scripts/setup.sh) and run the Ginkgo
#                   e2e suite (test/e2e) against it
#   make web        build the React SPA (type-check + vite build)
#   make images     build the four images (registry-addressed)
#   make push       push the four images to $(IMAGE_REGISTRY)
#   make update-crds refresh the vendored CubeStack CRDs from upstream
#   make lint       helm lint + helm template sanity check
#   make deploy     helm upgrade --install (defaults to the published
#                   :latest images; to deploy locally-built :local images,
#                   override the chart image tags, e.g.
#                   helm upgrade --install $(HELM_RELEASE) $(CHART_DIR) \
#                     --set agents.image=...,operator.image=...,api.image=...,web.image=...)
#   make undeploy   helm uninstall
#   make clean      remove build artifacts
#
# Overridable: IMAGE_REGISTRY (default harbor.isuanova.com/suanova),
#              IMAGE_TAG (default local), NAMESPACE, HELM_RELEASE,
#              KUBECONFIG (default $(HOME)/.kube/config).

BIN_DIR      ?= bin
IMAGE_REGISTRY ?= harbor.isuanova.com/suanova
IMAGE_TAG    ?= local
NAMESPACE    ?= cubepilot
HELM_RELEASE ?= cubepilot
CHART_DIR    ?= deploy/charts/cubepilot
KUBECONFIG   ?= $(HOME)/.kube/config

# CubeStack target CRDs (DevEnvironment / InferenceService / ModelVersion /
# InferenceRuntimeProfile), vendored for the e2e suite as a test precondition.
CUBESTACK_REPO       ?= git@github.com:suanova/cubestack.git
CUBESTACK_CRD_DIR    ?= test/e2e/framework/testdata/cubestack-crds
CUBESTACK_CRD_BASES  ?= operator/config/crd/bases

GO       ?= go
DOCKER   ?= docker
HELM     ?= helm
NPM      ?= npm

# OCM-style package scoping: `go test` skips test/e2e (it needs a live
# cluster); go vet still covers the whole module, including the e2e suite.
GO_TEST_PACKAGES = $(shell $(GO) list ./... | grep -v '/test/')

.PHONY: build test test-e2e web images push update-crds lint deploy undeploy clean

## Build both Go binaries (linux/amd64, matching the container runtime).
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o $(BIN_DIR)/cubepilot-operator ./cmd/cubepilot-operator
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o $(BIN_DIR)/cubepilot-api ./cmd/cubepilot-api

## vet + unit tests (excludes the cluster-backed e2e suite).
test:
	$(GO) vet ./...
	$(GO) test $(GO_TEST_PACKAGES)

## End-to-end: bring up the stack via scripts/setup.sh, then run the compiled
## Ginkgo suite (test/e2e) against it. CUBEPILOT_LLM_APIKEY is required (use
## sk-placeholder for a deploy-only run); CUBEPILOT_E2E_CHAT=1 enables the
## chat spec (needs a real provider key).
test-e2e:
	@[ -n "$(CUBEPILOT_LLM_APIKEY)" ] || (echo "CUBEPILOT_LLM_APIKEY is required (use sk-placeholder for deploy-only)" && exit 1)
	scripts/setup.sh
	@mkdir -p $(BIN_DIR)
	$(GO) test -c ./test/e2e -o $(BIN_DIR)/e2e.test
	KUBECONFIG=$(KUBECONFIG) CUBEPILOT_NAMESPACE=$(NAMESPACE) \
		./$(BIN_DIR)/e2e.test -test.v -ginkgo.v -ginkgo.fail-fast

## Build the React SPA (strict type-check via tsc, then vite build).
web:
	cd web && $(NPM) run build

## Build the four images (agent / operator / api / web) as
## $(IMAGE_REGISTRY)/cubepilot-<name>:$(IMAGE_TAG).
images:
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-openclaw:$(IMAGE_TAG) -f deploy/openclaw-image.Dockerfile .
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-operator:$(IMAGE_TAG) -f deploy/operator-image.Dockerfile .
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-api:$(IMAGE_TAG)      -f deploy/api-image.Dockerfile      .
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-web:$(IMAGE_TAG)      -f web/Dockerfile                  web

## Push the four images to $(IMAGE_REGISTRY).
push:
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-openclaw:$(IMAGE_TAG)
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-operator:$(IMAGE_TAG)
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-api:$(IMAGE_TAG)
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-web:$(IMAGE_TAG)

## Refresh the vendored CubeStack CRDs from the upstream operator
## (test/e2e/framework/testdata/cubestack-crds). Re-run when
## suanova/cubestack's operator/config/crd/bases changes; verify with
## git diff before committing.
update-crds:
	@rm -rf /tmp/cubestack-crds
	@git clone --depth 1 $(CUBESTACK_REPO) /tmp/cubestack-crds
	@mkdir -p $(CUBESTACK_CRD_DIR)
	@cp /tmp/cubestack-crds/$(CUBESTACK_CRD_BASES)/*.yaml $(CUBESTACK_CRD_DIR)/
	@rm -rf /tmp/cubestack-crds
	@echo "Updated CubeStack CRDs in $(CUBESTACK_CRD_DIR):"
	@ls -1 $(CUBESTACK_CRD_DIR)

## Helm chart sanity: lint + render.
lint:
	$(HELM) lint $(CHART_DIR)
	$(HELM) template $(HELM_RELEASE) $(CHART_DIR) -n $(NAMESPACE) > /dev/null

## Install/upgrade the release (secrets must exist; see scripts/setup.sh).
deploy:
	$(HELM) upgrade --install $(HELM_RELEASE) $(CHART_DIR) -n $(NAMESPACE)

## Remove the release.
undeploy:
	$(HELM) uninstall $(HELM_RELEASE) -n $(NAMESPACE)

## Remove build artifacts.
clean:
	rm -rf $(BIN_DIR) web/dist web/node_modules
