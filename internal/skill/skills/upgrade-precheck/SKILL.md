---
name: upgrade-precheck
description: "Pre-upgrade checks: component versions, removed APIs, workload readiness, disruption budgets, capacity and backups"
---

# Upgrade Pre-check

Run this before an upgrade, read-only, and answer one question: **what would
block it**. The output is a gate, not a report card -- a check that could not be
made is `unknown`, and `unknown` blocks.

This skill does not start, resume or roll back an upgrade. It only inspects.

## Steps

```bash
# 1. The versions actually running, taken from the images in use rather than
#    from any documented version.
kubectl get pods -A -o json | jq -r '
  .items[] | select(.status.phase == "Running")
  | .spec.containers[].image' | sort -u

# 2. CRDs whose stored version differs from what the target serves. A stored
#    version the target no longer serves blocks the upgrade.
kubectl get crd -o custom-columns='CRD:.metadata.name,SERVED:.spec.versions[*].name,STORAGE:.status.storedVersions'

# 3. Every apiVersion in use, to compare against the target's removal list.
#    Slow on a large cluster: run it once, and report the counts.
for r in $(kubectl api-resources --verbs=list -o name); do
  kubectl get "$r" -A -o jsonpath='{range .items[*]}{.apiVersion}{"\n"}{end}' 2>/dev/null
done | sort | uniq -c | sort -rn

# 4. Workload readiness.
kubectl get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded
kubectl get deploy,sts -A
kubectl get jobs -A

# 5. Disruption budgets already at their minimum: a drain hangs here.
kubectl get pdb -A -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,MIN:.spec.minAvailable,MAX:.spec.maxUnavailable,HEALTHY:.status.currentHealthy,DESIRED:.status.desiredHealthy'

# 6. Anything left behind by a previous attempt.
kubectl get pods -A --field-selector=status.phase=Failed
```

Capacity for a one-node-at-a-time drain has to be computed, not read: for each
node pool, subtract the largest node's allocatable resources from the pool's
free requests and check that the remainder still fits every workload that cannot
move.

Backups depend on what the cluster installs:

```bash
kubectl get crd | grep -i snapshot
kubectl get volumesnapshot -A
kubectl get pvc -A | grep -v Bound
```

## How to read the results

- **Blocking**: a CRD stored version the target no longer serves; a live object
  on an apiVersion the target removed; a Pod that cannot be rescheduled during a
  drain; a PodDisruptionBudget already at its healthy minimum.
- **Needs a decision, not a fix**: workloads below their replica count that have
  been that way independently of the upgrade; Jobs still running; capacity that
  only fits the drain if a pool is expanded first.
- **Unknown**: anything the agent's identity cannot read, and anything that
  depends on the target release's notes. Say which, and why -- do not fold an
  unknown into a pass.

## Report Format (Simplified Chinese)

- Verdict first: ready / not ready / cannot be determined.
- A table of every check with pass / fail / unknown.
- Then the blocking list, each with the evidence that shows it and what has to
  change.
- Close with the checks that returned `unknown` and what access or information
  would resolve them.
