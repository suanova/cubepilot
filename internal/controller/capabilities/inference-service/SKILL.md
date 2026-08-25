---
name: inference-service
description: "Deploy and query model inference services (InferenceService CRD, apiVersion assistant.suanova.io/v1alpha1)"
---

# Inference Service (InferenceService)

The platform represents model inference services as the CRD `InferenceService` (`assistant.suanova.io/v1alpha1`).

## Query

```bash
kubectl get inferenceservices -n <ns>
kubectl get inferenceservice <name> -n <ns> -o yaml
kubectl get pods -n <ns> -l app=<name>     # associated Pods
```

## Deploy

When a user asks to "deploy an inference service", generate an InferenceService YAML from the model/traffic/latency requirements and apply it. Key fields:
- `spec.model`: model identifier (e.g. `qwen2.5-72b`)
- `spec.framework`: inference framework (e.g. `vllm`)
- `spec.gpu.count` / `spec.gpu.type`: GPU resources
- `spec.replicas`: number of replicas

Example:

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: InferenceService
metadata:
  name: qwen2.5-72b-infer
  namespace: default
spec:
  model: qwen2.5-72b
  framework: vllm
  gpu:
    count: 2
    type: A100
  replicas: 1
```

## Troubleshooting

When a service is unhealthy: `kubectl describe` to check events -> `kubectl logs` to check startup logs -> combine with GPU status (node `nvidia.com/gpu` allocatable) to determine whether it is a resource shortage or a configuration issue.