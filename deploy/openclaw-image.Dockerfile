# CubePilot agent runtime image (OpenClaw): gateway + kubectl + workspace seed.
# Self-contained: the supervisor Go binary compiles inside Docker and the
# runtime bases on the PUBLISHED openclaw image -- no locally built
# openclaw:local and no host Go toolchain (CI-friendly).
#
# Pin the openclaw base to a release tag (never :latest). If a newer tag is
# wanted, mirror it from ghcr.io/openclaw/openclaw into
# harbor.isuanova.com/library (scripts/mirror-base-images.sh).
ARG OPENCLAW_IMAGE_TAG=2026.6.33

FROM harbor.isuanova.com/library/golang:1.26-bookworm AS supervisor-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/cubepilot-supervisor ./cmd/cubepilot-supervisor/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/cubepilot-supervisor ./cmd/cubepilot-supervisor

FROM harbor.isuanova.com/library/openclaw:${OPENCLAW_IMAGE_TAG}
ARG KUBECTL_VERSION=v1.36.1

USER root
# curl/ca-certificates ship in the openclaw runtime image; this only guards
# against base drift and downloads kubectl.
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && rm -rf /var/lib/apt/lists/*

# The agent-pod supervisor (pid 1): pulls the resolved agent config (internal
# API), renders skills into the PVC workspace, and runs the OpenClaw gateway
# as a child process. The gateway owns its own reload (config reloader watches
# openclaw.json), so the supervisor never restarts the gateway for a config
# change.
COPY --from=supervisor-build /out/cubepilot-supervisor /usr/local/bin/cubepilot-supervisor

# Workspace persona (AGENTS.md / SOUL.md) seed for the per-instance PVC; the
# seed-workspace initContainer copies it to the PVC on first start.
# Capability skills flow dynamically via the resolved agent config (Capability
# CRD -> operator resolver -> internal API -> supervisor renders into workspace).
COPY --chown=node:node workspace/ /opt/cubepilot/workspace/

USER node
