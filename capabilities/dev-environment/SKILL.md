---
name: dev-environment
description: 创建与查询开发环境（DevEnvironment CRD，apiVersion assistant.suanova.io/v1alpha1）
---

# 开发环境（DevEnvironment）

平台以 CRD `DevEnvironment`（`assistant.suanova.io/v1alpha1`）表示 GPU 开发环境。

## 查询

```bash
kubectl get devenvironments -n <ns>        # 列出开发环境
kubectl get devenvironment <name> -n <ns> -o yaml   # 详情
```

## 创建（自然语言 → YAML）

用户要求「创建一个开发环境」时，根据其描述生成 DevEnvironment YAML 并 apply。关键字段：
- `metadata.name` / `namespace`：环境名与命名空间
- `spec.image`：镜像（如 `pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime`）
- `spec.resources`：请求的 CPU/内存/GPU（`nvidia.com/gpu`）
- `spec.gpu.count`：GPU 数量（如有该字段）

示例：

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

## 原则

- 创建前向用户确认镜像、GPU 数量与命名空间；缺省时给出合理默认并说明。
- 环境诊断（CUDA 冲突 / OOM）时，结合 `kubectl logs`、`kubectl describe` 与 GPU 状态定位根因，给出修复建议。
