# CubePilot agent runtime image (OpenClaw): gateway + kubectl + capability
# catalog. Named after the runtime (openclaw) so a future runtime (e.g.
# hermes) can ship as a sibling image without ambiguity.
# Loaded into the kind cluster by scripts/setup.sh and run as a per-user Pod.
FROM openclaw:local

ARG KUBECTL_VERSION=v1.36.1

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && rm -rf /var/lib/apt/lists/*

# The agent-pod supervisor: pulls the resolved agent config (internal API),
# renders skills into the PVC workspace, and runs the OpenClaw gateway as a
# child process (graceful restart on config change — never a pod delete).
# Built on the host: CGO_ENABLED=0 GOOS=linux go build -o bin/cubepilot-supervisor ./cmd/cubepilot-supervisor
COPY bin/cubepilot-supervisor /usr/local/bin/cubepilot-supervisor

# Workspace persona (AGENTS.md / SOUL.md) is baked in as the seed for the
# per-instance PVC: the seed-workspace initContainer copies it to the PVC on
# first start, where the gateway keeps it writable (TOOLS.md etc).
#
# Capability skills are NOT baked in anymore: they flow dynamically via the
# resolved agent config (Capability CRD → operator resolver → internal API →
# supervisor renders into workspace/skills).
COPY --chown=node:node workspace/ /opt/cubepilot/workspace/

USER node
