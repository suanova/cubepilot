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

# Workspace persona (AGENTS.md / SOUL.md) is baked in as the seed for the
# per-instance PVC: the seed-workspace initContainer copies it to the PVC on
# first start, where the gateway keeps it writable (TOOLS.md etc).
#
# Capability skills are NOT baked in anymore: they flow dynamically via the
# capability-skills ConfigMap (Capability CRD -> operator render -> agent Pod
# initContainer expand; a capability create/update rolls the agent Pods).
# This is the dynamic capability → runtime skill channel (design §3.3.1).
COPY --chown=node:node workspace/ /opt/cubepilot/workspace/

USER node
