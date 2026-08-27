---
name: kubectl-platform
description: "Run kubectl via exec to query and operate cluster resources (nodes/Pods/namespaces/events); read-only runs directly, write operations run cautiously and state the blast radius"
---

# Cluster Resource Operations (kubectl)

Use the `exec` tool to run `kubectl` commands against the current cluster. kubectl is already configured to point at the current cluster (in-cluster credentials).

## Common Read-Only Commands

```bash
kubectl get nodes                          # node status
kubectl get pods -n <ns>                   # Pods in a namespace
kubectl get pods -A                        # Pods in all namespaces
kubectl get pods -n <ns> --field-selector=status.phase!=Running   # abnormal Pods
kubectl describe pod <name> -n <ns>        # Pod details and events
kubectl logs <pod> -n <ns> --tail=50       # logs
kubectl get events -n <ns> --sort-by=.lastTimestamp   # events
kubectl get namespaces                     # namespaces
```

## Write Operations (phase-one direct pass-through; state the action and blast radius before running)

```bash
kubectl apply -f <file> -n <ns>
kubectl delete pod <name> -n <ns>
kubectl scale deployment <name> --replicas=N -n <ns>
```

## Principles

- Prefer `--field-selector`, `-o wide`, `--sort-by` to get structured results.
- When lacking permission (RBAC denied), tell the user honestly and do not retry the same rejected operation.
- When output is long, truncate with `| head` / `--tail`.