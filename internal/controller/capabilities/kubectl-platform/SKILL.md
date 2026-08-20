---
name: kubectl-platform
description: 通过 exec 执行 kubectl 查询与操作集群资源（节点/Pod/命名空间/事件），只读直放、写操作谨慎并说明影响范围
---

# 集群资源操作（kubectl）

通过 `exec` 工具执行 `kubectl` 命令操作当前集群。kubectl 已配置好指向当前集群（in-cluster 凭据）。

## 常用只读命令

```bash
kubectl get nodes                          # 节点状态
kubectl get pods -n <ns>                   # 某命名空间 Pod
kubectl get pods -A                        # 全命名空间 Pod
kubectl get pods -n <ns> --field-selector=status.phase!=Running   # 异常 Pod
kubectl describe pod <name> -n <ns>        # Pod 详情与事件
kubectl logs <pod> -n <ns> --tail=50       # 日志
kubectl get events -n <ns> --sort-by=.lastTimestamp   # 事件
kubectl get namespaces                     # 命名空间
```

## 写操作（阶段一直放，执行前说明动作与影响范围）

```bash
kubectl apply -f <file> -n <ns>
kubectl delete pod <name> -n <ns>
kubectl scale deployment <name> --replicas=N -n <ns>
```

## 原则

- 优先用 `--field-selector`、`-o wide`、`--sort-by` 拿到结构化结果。
- 无权限（RBAC 拒绝）时如实告知用户，不重试同一被拒操作。
- 输出较长时用 `| head` / `--tail` 截断。
