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

# Capability catalog + persona live OUTSIDE the per-user PVC mount point
# (/home/node/.openclaw), so the PVC never shadows them. The Pod sets
# OPENCLAW_WORKSPACE_DIR=/opt/cubepilot/workspace; OpenClaw loads SOUL.md /
# AGENTS.md and skills/<name>/SKILL.md from there. Owned by `node` so the
# gateway (running as node) can read and write bootstrap files like TOOLS.md.
COPY --chown=node:node workspace/ /opt/cubepilot/workspace/
COPY --chown=node:node capabilities/ /opt/cubepilot/workspace/skills/

USER node
