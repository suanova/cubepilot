#!/usr/bin/env bash
# CubePilot redeploy -- push locally-built images onto a running kind cluster
# and roll the workloads (the edit -> test dev loop).
#
# Counterpart of scripts/setup.sh (one-shot bring-up). This script assumes the
# stack is already deployed once (scripts/setup.sh or `make deploy`) and that
# the four images have been built from the current tree (`make images`, the
# `make redeploy` prerequisite). It kind-loads the images, helm-upgrades with
# --reuse-values so only the four image refs change, rolls the operator / api /
# web Deployments, then waits for the per-user agent pods to converge on the
# new openclaw tag.
#
# The agent-image migration needs no manual step: the operator self-heals
# existing AgentInstance pods because the container image is part of the
# immutable security fingerprint compared in ensurePod (agentinstance
# controller) -- a drifted pod is deleted and recreated on the operator's
# current CUBEPILOT_AGENT_IMAGE within a reconcile window.
#
# Inputs (env, defaults shown):
#   CUBEPILOT_IMAGE_REPO     image repository (harbor.isuanova.com/suanova)
#   CUBEPILOT_IMAGE_TAG      image tag of the built images (local)
#   CUBEPILOT_KIND_CLUSTER   kind cluster name (cube)
#   CUBEPILOT_KUBE_CONTEXT   kubeconfig context (kind-$CUBEPILOT_KIND_CLUSTER)
#   CUBEPILOT_NAMESPACE      target namespace (cubepilot)
#   CUBEPILOT_HELM_RELEASE   helm release name (cubepilot)
#   CUBEPILOT_CHART_DIR      chart directory (deploy/charts/cubepilot)
#
# Requires: kind, kubectl, helm (v3). Run `make images` (or scripts/setup.sh)
# first if the :$(CUBEPILOT_IMAGE_TAG) images are not built yet.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

IMAGE_REPO="${CUBEPILOT_IMAGE_REPO:-harbor.isuanova.com/suanova}"
IMAGE_TAG="${CUBEPILOT_IMAGE_TAG:-local}"
KIND_CLUSTER="${CUBEPILOT_KIND_CLUSTER:-cube}"
# kind names its kubeconfig context kind-<cluster>; pin helm/kubectl to it so
# image loading and deployment target the same cluster even when another
# context is active.
KUBE_CONTEXT="${CUBEPILOT_KUBE_CONTEXT:-kind-$KIND_CLUSTER}"
NAMESPACE="${CUBEPILOT_NAMESPACE:-cubepilot}"
HELM_RELEASE="${CUBEPILOT_HELM_RELEASE:-cubepilot}"
CHART_DIR="${CUBEPILOT_CHART_DIR:-deploy/charts/cubepilot}"
case "$CHART_DIR" in /*) ;; *) CHART_DIR="$REPO_DIR/$CHART_DIR" ;; esac

# The per-user agent pods carry this label; the roll + convergence wait selects
# them by it.
AGENT_LABEL="cubepilot-agent=true"

log() { printf '\033[1;32m[redeploy]\033[0m %s\n' "$*"; }

# ---- prerequisites -------------------------------------------------------
command -v kind    >/dev/null || { echo "kind required"; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl required"; exit 1; }
command -v helm    >/dev/null || { echo "helm required"; exit 1; }
[ -d "$CHART_DIR" ] || { echo "error: chart dir not found: $CHART_DIR" >&2; exit 1; }

# ---- load images into the kind nodes -------------------------------------
log "loading images into kind ($KIND_CLUSTER)"
kind load docker-image \
  "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" \
  "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" \
  "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG" \
  "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG" \
  --name "$KIND_CLUSTER"

# ---- helm upgrade (image refs only, preserve custom values) --------------
log "helm-upgrading $HELM_RELEASE (--reuse-values, image refs only)"
helm upgrade --install "$HELM_RELEASE" "$CHART_DIR" -n "$NAMESPACE" \
  --kube-context "$KUBE_CONTEXT" \
  --reuse-values \
  --set agents.image="$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" \
  --set operator.image="$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" \
  --set api.image="$IMAGE_REPO/cubepilot-api:$IMAGE_TAG" \
  --set web.image="$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"

# ---- roll operator / api / web -------------------------------------------
# The image tag is unchanged between iterations and imagePullPolicy is
# IfNotPresent, so the deployment image refs do not change and helm will not
# roll by itself: force fresh pods onto the freshly loaded images.
log "rolling operator / api / web deployments"

# Agent pods existing before the rollout. The operator deletes and recreates
# them on the new image (drift), so afterwards they must all come back again:
# a transiently empty pod list mid-migration is not success.
agents_before=$(kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get pods -l "$AGENT_LABEL" --no-headers 2>/dev/null | wc -l | tr -d ' ')

# Resolve the rendered Deployment names from the installed release manifest, so
# a custom operator.name / api.name / web.name survives the rollout. Each
# Deployment's metadata.name is the first top-level `name:` after `kind:
# Deployment` (containers/volumes use `- name:`).
deployments=$(helm --kube-context "$KUBE_CONTEXT" get manifest "$HELM_RELEASE" -n "$NAMESPACE" \
  | awk '/^kind: Deployment$/{d=1;next} d&&/^  name: /{sub(/^  name: /,"");printf "deployment/%s ",$0;d=0}')
deployments=${deployments% } # drop the trailing space
[ -n "$deployments" ] || { echo "error: no Deployment found in the $HELM_RELEASE manifest" >&2; exit 1; }

kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" rollout restart $deployments
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" rollout status $deployments \
  --timeout=120s

# ---- wait for agent pods to converge on the new image --------------------
# The restarted operator self-heals each AgentInstance pod onto the new
# agents.image (drift delete + recreate). Poll until every agent pod is Running
# on the target tag. When agent pods existed before the rollout, all of them
# must be back (count >= agents_before) and Running on the target image; an
# empty or short pod list during the delete/recreate window is not convergence.
# A kubectl error is treated as "not yet converged" rather than success. Two
# consecutive clean samples avoid exiting into a mid-roll state.
TARGET_IMAGE="$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG"
log "waiting for agent pods to converge on $TARGET_IMAGE"
clean=0
deadline=$(( $(date +%s) + 180 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if out=$(kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get pods -l "$AGENT_LABEL" \
    -o go-template='{{range .items}}{{.status.phase}}{{range .spec.containers}} {{.image}}{{end}}{{"\n"}}{{end}}' 2>/dev/null); then
    count=0
    mismatch=""
    if [ -n "$out" ]; then
      count=$(printf '%s\n' "$out" | wc -l | tr -d ' ')
      mismatch=$(printf '%s\n' "$out" | awk -v img="$TARGET_IMAGE" '$1 != "Running" || $2 != img { print }')
    fi
    if [ "$agents_before" -gt 0 ] && [ "$count" -lt "$agents_before" ]; then
      # Some (or all) replacements are not back yet -- keep waiting.
      mismatch="waiting for $agents_before agent pods ($count up)"
    fi
  else
    # kubectl failed (transient): do not report success.
    mismatch=nonempty
  fi
  if [ -z "$mismatch" ]; then
    clean=$((clean + 1))
    if [ "$clean" -ge 2 ]; then
      log "agent pods converged on $TARGET_IMAGE"
      exit 0
    fi
  else
    clean=0
  fi
  sleep 5
done
echo "error: timed out waiting for agent pods to converge on $TARGET_IMAGE" >&2
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get pods -l "$AGENT_LABEL" -o wide
exit 1
