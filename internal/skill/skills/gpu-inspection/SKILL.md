---
name: gpu-inspection
description: "GPU node inspection: inventory, allocatable vs. allocated, device plugin health, stuck allocations and hardware errors"
---

# GPU Node Inspection

Inspect every GPU node and the workloads holding its GPUs, and report which GPUs
are unusable, which are over-committed, and which are allocated to workloads that
are not running. Read-only throughout (`get` / `list` / `watch` / `logs`).

## Discover the GPU resource name first

Vendors expose GPUs under different extended resource names. Read the name the
cluster actually uses instead of assuming one, and keep it in `GPU_RES` -- every
command below reads it from there:

```bash
kubectl get nodes -o json | jq -r '.items[].status.allocatable | keys[]' \
  | grep -i gpu | sort -u
```

On an nvidia cluster this prints `nvidia.com/gpu`. If it prints nothing, the
cluster has no GPU nodes -- say so and stop.

A mixed cluster prints one name per vendor. Then there is no single `GPU_RES`:
run the per-resource steps once for each name, and report per resource rather
than summing unrelated accelerators into one figure. Never let an nvidia name
stand in for a count of Metax cards, or the reverse -- that reports zero
allocated GPUs on hardware that has them.

When the run names a vendor, inspect only that vendor's resource name; when it
says all, cover every name the cluster reports.

```bash
GPU_RES=nvidia.com/gpu   # <- the name discovered above, per vendor
```

## Steps

```bash
# 1. GPU nodes: the nodes that report GPU_RES as allocatable. The
#    nvidia.com/gpu.present label is the nvidia GPU operator's and is a faster
#    filter on an nvidia cluster, but it does not exist for other vendors --
#    never make the inventory depend on it.
kubectl get nodes -o json | jq -r --arg res "$GPU_RES" '
  .items[] | select(.status.allocatable[$res] != null)
  | "\(.metadata.name)\t\(.status.allocatable[$res])"'

# 2. How many GPUs each node carries, next to its other allocatable resources.
kubectl get nodes -o json | jq -r --arg res "$GPU_RES" '
  .items[] | select(.status.allocatable[$res] != null)
  | [.metadata.name, .status.allocatable[$res],
     .status.allocatable.cpu, .status.allocatable.memory] | @tsv'

# 3. GPUs requested per node, summed over the Pods scheduled there. A node whose
#    requests exceed its allocatable GPUs is over-committed.
#
#    Quantities arrive as JSON strings, and jq's `add` concatenates strings: a
#    Pod with two containers asking for "1" each would report "11", not 2. So
#    every value is converted to a number first. `tonumber` is safe on this
#    resource specifically -- GPUs are extended resources, whose quantities are
#    always whole numbers (unlike cpu's "100m" or memory's "1Gi").
kubectl get pods -A -o json | jq -r --arg res "$GPU_RES" '
  .items[]
  | select(.status.phase != "Succeeded" and .status.phase != "Failed")
  | .spec.nodeName as $n
  | (([.spec.containers[].resources.requests[$res] // "0" | tonumber] | add) // 0)
  | select(. > 0)
  | "\($n)\t\(.)"' | awk '{s[$1]+=$2} END {for (n in s) print n, s[n]}'

# 4. The device plugin on each GPU node: present, Running, restart count.
kubectl get pods -A -o wide | grep -i device-plugin

# 5. Its log, for registration failures ("no devices found", "failed to
#    allocate", "incompatible driver").
kubectl logs -n <device-plugin-namespace> <pod> --tail=100
```

## Stuck and idle GPUs

```bash
# Pods Pending because there is no free GPU.
kubectl get pods -A --field-selector=status.phase=Pending -o wide
kubectl describe pod -n <namespace> <pod> | grep -A5 -i "insufficient\|$GPU_RES"

# Pods holding a GPU while not Running -- a leak the scheduler cannot reclaim.
kubectl get pods -A -o json | jq -r --arg res "$GPU_RES" '
  .items[]
  | select(.status.phase != "Running" and .status.phase != "Succeeded")
  | select(([.spec.containers[].resources.requests[$res] // "0" | tonumber] | add) > 0)
  | "\(.metadata.namespace)/\(.metadata.name)\t\(.status.phase)"'

# Nodes tainted or cordoned while still holding GPUs nothing can use.
kubectl get nodes -o custom-columns='NODE:.metadata.name,UNSCHEDULABLE:.spec.unschedulable,TAINTS:.spec.taints[*].key'
```

## Hardware errors

GPU hardware faults reach the cluster as node events when a node problem
detector is installed, or as metrics from the vendor's exporter:

```bash
kubectl get events -A | grep -iE 'xid|ecc|gpu'
kubectl get pods -A | grep -iE 'dcgm|node-problem-detector'
```

If neither is present, kernel-level GPU errors cannot be read from the agent's
identity. **Report that check as not performed** -- do not infer hardware health
from the fact that Pods are Running.

## Severity

- **High**: a GPU node NotReady; the device plugin missing or crash-looping on a
  GPU node (its GPUs are unusable, however healthy the hardware is); a node whose
  GPU requests exceed its allocatable GPUs.
- **Medium**: a GPU allocated to a Pod that is not Running; a cordoned GPU node;
  Pods Pending on insufficient GPU while GPUs sit allocated but idle elsewhere.
- **Low**: GPU nodes with no GPU workload at all; high but non-saturated
  utilization; a device plugin that restarted once, long ago.

## Report Format (Simplified Chinese)

- Overview: GPU node count, total GPUs, allocated vs. allocatable, and the count
  per severity.
- Per node: GPUs carried, GPUs allocated, device plugin state, and anything
  wrong with it.
- Per finding: severity + symptom + evidence (the command and its key output) +
  what to do. Attach an evidence chain to every finding.

Say plainly which checks could not be made from the agent's identity, and why.
An unmeasured check is not a pass.
