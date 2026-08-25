---
name: cluster-inspection
description: "Cluster health inspection: check nodes/Pods/events, classify findings and output a report by P0/P1/P2 severity"
---

# Cluster Health Inspection

Run a basic inspection of the current cluster and output a report with findings classified by severity.

## Inspection Steps

```bash
kubectl get nodes                          # node Ready status
kubectl get pods -A --field-selector=status.phase!=Running   # abnormal Pods
kubectl get events -A --sort-by=.lastTimestamp | tail -50    # recent events
```

## Severity Classification

- **P0 Critical**: control plane unavailable, node NotReady, key service CrashLoopBackOff.
- **P1 Important**: Pod stuck in Pending (insufficient GPU), OOMKilled, storage near capacity, GPU node degraded.
- **P2 Minor**: occasional restarts, non-critical component issues, high resource usage, certificates nearing expiry.

## Report Format (Simplified Chinese)

- Overview: node count / abnormal Pod count / classification counts.
- Itemized: severity + symptom + evidence (command output excerpt) + recommendation.
- Attach an evidence chain to each item (command and key output) so the user can review it.