---
name: inference-service
description: 部署与查询模型推理服务（InferenceService CRD，apiVersion assistant.suanova.io/v1alpha1）
---

# 推理服务（InferenceService）

平台以 CRD `InferenceService`（`assistant.suanova.io/v1alpha1`）表示模型推理服务。

## 查询

```bash
kubectl get inferenceservices -n <ns>
kubectl get inferenceservice <name> -n <ns> -o yaml
kubectl get pods -n <ns> -l app=<name>     # 关联 Pod
```

## 部署

用户要求「部署推理服务」时，根据模型/流量/延迟需求生成 InferenceService YAML 并 apply。关键字段：
- `spec.model`：模型标识（如 `qwen2.5-72b`）
- `spec.framework`：推理框架（如 `vllm`）
- `spec.gpu.count` / `spec.gpu.type`：GPU 资源
- `spec.replicas`：副本数

示例：

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

## 排障

服务异常时：`kubectl describe` 看事件 → `kubectl logs` 看启动日志 → 结合 GPU 状态（节点 `nvidia.com/gpu` allocatable）定位是资源不足还是配置问题。
