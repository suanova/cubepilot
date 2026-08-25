---
name: dev-environment
description: "Create and query development environments (DevEnvironment CRD, apiVersion assistant.suanova.io/v1alpha1)"
---

# Development Environment (DevEnvironment)

The platform represents GPU development environments as the CRD `DevEnvironment` (`assistant.suanova.io/v1alpha1`).

## Query

```bash
kubectl get devenvironments -n <ns>        # list development environments
kubectl get devenvironment <name> -n <ns> -o yaml   # details
```

## Create (Natural Language -> YAML)

When a user asks to "create a development environment", generate a DevEnvironment YAML from their description and apply it. Key fields:
- `metadata.name` / `namespace`: environment name and namespace
- `spec.image`: image (e.g. `pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime`)
- `spec.resources`: requested CPU/memory/GPU (`nvidia.com/gpu`)
- `spec.gpu.count`: number of GPUs (if the field exists)

Example:

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: DevEnvironment
metadata:
  name: dev-cuda121
  namespace: default
spec:
  image: pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
  gpu:
    count: 1
  resources:
    requests:
      cpu: "4"
      memory: "16Gi"
```

## Principles

- Confirm the image, GPU count and namespace with the user before creating; when not specified, propose a sensible default and explain it.
- When diagnosing an environment (CUDA conflict / OOM), combine `kubectl logs`, `kubectl describe` and GPU status to find the root cause, and give a fix recommendation.