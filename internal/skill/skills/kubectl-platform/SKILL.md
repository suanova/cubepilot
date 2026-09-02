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

## Schema Discovery (zero-registration CRD support)

Platform CRDs live under the `ai.cubestack.io` group (`DevEnvironment`, `InferenceService`, `ModelVersion`, `InferenceRuntimeProfile`, …). There is no per-CRD skill — discover the kind / group / schema before applying:

1. Find the resource type: `kubectl api-resources | grep -i <keyword>` (e.g. `grep -i deve` → `devenvironments.ai.cubestack.io`).
2. Read the schema if RBAC allows: `kubectl explain devenvironment --api-version=ai.cubestack.io/v1alpha1` (or `kubectl get crd devenvironments.ai.cubestack.io -o yaml`). The agent SA may lack discovery / crd-read permission — then skip to step 3.
3. Validate against the server before creating (needs no extra permission): apply with `--dry-run=server` and iterate on the error — the API server lists the required fields (e.g. `spec.image`, `spec.resources`) and rejects unknown ones:

   ```bash
   kubectl apply --dry-run=server -f - -n <ns> <<'EOF'
   apiVersion: ai.cubestack.io/v1alpha1
   kind: DevEnvironment
   metadata:
     name: dev
   spec:
     image: pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
     resources:
       requests:
         cpu: "4"
         memory: 16Gi
   EOF
   ```

4. Only then apply for real and verify: `kubectl get devenvironments -n <ns>`.

## Principles

- Prefer `--field-selector`, `-o wide`, `--sort-by` to get structured results.
- When lacking permission (RBAC denied), tell the user honestly and do not retry the same rejected operation.
- When output is long, truncate with `| head` / `--tail`.