# CubePilot development Makefile -- a thin orchestration layer over the
# existing tooling (go / npm / docker / helm). It does NOT replace
# scripts/setup.sh (one-shot local deploy); it covers the per-task commands
# developers run repeatedly.
#
#   make build      build both Go binaries into bin/
#   make test       go vet + go test ./...
#   make web        build the Vue SPA (type-check + vite build)
#   make images     build the four images (registry-addressed)
#   make push       push the four images to $(IMAGE_REGISTRY)
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
#              IMAGE_TAG (default local), NAMESPACE, HELM_RELEASE.

BIN_DIR      ?= bin
IMAGE_REGISTRY ?= harbor.isuanova.com/suanova
IMAGE_TAG    ?= local
NAMESPACE    ?= cubepilot
HELM_RELEASE ?= cubepilot
CHART_DIR    ?= deploy/charts/cubepilot

GO       ?= go
DOCKER   ?= docker
HELM     ?= helm
NPM      ?= npm

.PHONY: build test web images push lint deploy undeploy clean

## Build both Go binaries (linux/amd64, matching the container runtime).
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o $(BIN_DIR)/cubepilot-operator ./cmd/cubepilot-operator
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o $(BIN_DIR)/cubepilot-api ./cmd/cubepilot-api

## vet + unit tests.
test:
	$(GO) vet ./...
	$(GO) test ./...

## Build the Vue SPA (strict type-check via vue-tsc, then vite build).
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
