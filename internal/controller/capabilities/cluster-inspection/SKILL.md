---
name: cluster-inspection
description: 集群健康巡检：检查节点/Pod/事件，发现异常并按 P0/P1/P2 分级输出报告
---

# 集群健康巡检

对当前集群执行一次基础巡检，发现异常并按严重程度分级输出报告。

## 巡检步骤

```bash
kubectl get nodes                          # 节点 Ready 状态
kubectl get pods -A --field-selector=status.phase!=Running   # 异常 Pod
kubectl get events -A --sort-by=.lastTimestamp | tail -50    # 最近事件
```

## 异常分级

- **P0 紧急**：控制面不可用、节点 NotReady、关键服务 CrashLoopBackOff。
- **P1 重要**：Pod 长时间 Pending（GPU 不足）、OOMKilled、存储接近上限、GPU 节点降级。
- **P2 一般**：偶发重启、非关键组件异常、资源使用率偏高、证书临近过期。

## 报告格式（简体中文）

- 概览：节点数 / 异常 Pod 数 / 分级计数。
- 逐项：级别 + 现象 + 证据（命令输出摘要）+ 建议。
- 每项附证据链（命令与关键输出），便于用户复核。
