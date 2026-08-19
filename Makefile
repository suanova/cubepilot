# CubePilot development Makefile — a thin orchestration layer over the
# existing tooling (go / npm / docker / helm). It does NOT replace
# scripts/setup.sh (one-shot local deploy); it covers the per-task commands
# developers run repeatedly.
#
#   make build      build both Go binaries into bin/
#   make test       go vet + go test ./...
#   make web        build the Vue SPA (type-check + vite build)
#   make images     build the four local images
#   make lint       helm lint + helm template sanity check
#   make deploy     helm upgrade --install (requires built images + secrets)
#   make undeploy   helm uninstall
#   make clean      remove build artifacts
#
# Overridable: IMAGE_TAG (default local), NAMESPACE, HELM_RELEASE.

BIN_DIR      ?= bin
IMAGE_TAG    ?= local
NAMESPACE    ?= cubepilot
HELM_RELEASE ?= cubepilot
CHART_DIR    ?= deploy/charts/cubepilot

GO       ?= go
DOCKER   ?= docker
HELM     ?= helm
NPM      ?= npm

.PHONY: build test web images lint deploy undeploy clean

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

## Build the four local images (agent / operator / api / web).
images:
	$(DOCKER) build -t cubepilot-openclaw:$(IMAGE_TAG)    -f deploy/openclaw-image.Dockerfile    .
	$(DOCKER) build -t cubepilot-operator:$(IMAGE_TAG) -f deploy/operator-image.Dockerfile .
	$(DOCKER) build -t cubepilot-api:$(IMAGE_TAG)      -f deploy/api-image.Dockerfile      .
	$(DOCKER) build -t cubepilot-web:$(IMAGE_TAG)      -f web/Dockerfile                  web

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
