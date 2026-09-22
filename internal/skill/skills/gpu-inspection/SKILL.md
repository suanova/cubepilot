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
cluster actually uses instead of assuming one:

```bash
kubectl get nodes -o json | jq -r '.items[].status.allocatable | keys[]' \
  | grep -i gpu | sort -u
```

On an nvidia cluster this prints `nvidia.com/gpu`. If it prints nothing, the
cluster has no GPU nodes -- say so and stop.

## Steps

```bash
# 1. GPU nodes. The label below is the nvidia GPU operator's; without that
#    operator, list the nodes that report the resource name from above.
kubectl get nodes -l nvidia.com/gpu.present=true -o wide

# 2. How many GPUs each node carries, next to its other allocatable resources.
#    (Substitute the resource name discovered above.)
kubectl get nodes -o custom-columns='NODE:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,CPU:.status.allocatable.cpu,MEM:.status.allocatable.memory'

# 3. GPUs requested per node, summed over the Pods scheduled there. A node whose
#    requests exceed its allocatable GPUs is over-committed.
kubectl get pods -A -o json | jq -r '
  .items[]
  | select(.status.phase != "Succeeded" and .status.phase != "Failed")
  | .spec.nodeName as $n
  | (([.spec.containers[].resources.requests["nvidia.com/gpu"] // 0]
      | add) // 0)
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
kubectl describe pod -n <namespace> <pod> | grep -A5 -i 'insufficient\|nvidia.com/gpu'

# Pods holding a GPU while not Running -- a leak the scheduler cannot reclaim.
kubectl get pods -A -o json | jq -r '
  .items[]
  | select(.status.phase != "Running" and .status.phase != "Succeeded")
  | select([.spec.containers[].resources.requests["nvidia.com/gpu"] // 0] | add > 0)
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
