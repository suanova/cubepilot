# 文档-实现对齐计划(2026-08-21)

用户拍板:现在不需要 MCP Gateway,直接用 kubectl(现状保持);MCP Gateway 仅作为阶段二目标保留在文档。

## 阶段 0:§8.4 重写(纯文档) — 进行中
- 已知取舍 #1 重写:MCP Gateway 阶段一未建 → OpenClaw 直接 exec kubectl(挂用户 kubeconfig,RBAC 兜底);审计经 SSE 流捕获(事后记录,无执行前校验/阻断/HITL)
- "已对齐"措辞更新:updateConfig/pod 重启 → 文件监听热重载(chokidar 已验证)、x-openclaw-model 每请求热生效
- 新增取舍:§5.3"双 kubeconfig"未实现(现在只挂一个 agent-kubeconfig)

## 阶段 1:枚举大写统一(代码+CRD+web+测试)
- ModelProvider → "Platform"/"External";CapabilityType → "Atomic"/"Domain";ConfirmPolicy → "None"/"ConfirmWrites";TaskTrigger → "Cron"/"Manual"(Task state 重构单列阶段 2)
- 消费方:builtin.go / handlers_platform.go / handlers_tasks.go / skill_source.go
- controller-gen 重新生成 CRD,同步 config/crd + deploy/charts/cubepilot/crds + 集群 apply
- web 硬编码字符串同步;测试断言更新;go build/vet/test 全绿 + kind 验证

## 阶段 2(可选,待拍板):Task state: Enabled/Paused 重构
- TaskSpec.Enabled bool → State TaskState "Ready"|"Paused";trigger "manual"→"Manual"(兼容旧值)
- 若不做:文档 §3.5 回退为 enabled: true

## 阶段 3:§4 配置注入主方案对齐现实
- 主方案改为"控制面 resolve(resolver+API) + pod 内 supervisor 拉取渲染"(现实现);injector sidecar 降为备选/将来选项

## 阶段 4:§5.3 双 kubeconfig 标注待办
- 现在单 kubeconfig(用户);双 kubeconfig(只读 CRD)触发条件 = 用户无 CRD 读权限场景,登记阶段二清单不实现

## 不做
- 现在不实现 MCP Gateway、不实现 HITL 确认链路、不实现双 kubeconfig