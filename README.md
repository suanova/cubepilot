# CubePilot PoC

CubePilot 是 CubeStack 平台的智能助手。本仓库是一个 **PoC**，验证两条核心技术路线（对应《模块设计文档》的扩展点 E1/E2 与 FR-M2/M3）：

1. **每用户实例（K8s Pod）生命周期** —— Instance Manager 经 K8s API 按需拉起 / 闲置回收 / 异常自愈每用户 OpenClaw Pod，实例重建后会话与记忆不丢（独立 PVC）。
2. **对话闭环** —— 真实 OpenClaw（真实 DeepSeek V4 Flash）在能力目录（Skills）指引下，自主调用 `exec` 执行 `kubectl` 操作同一集群，结果以 SSE 事件流回传 Portal。

## 架构

```
浏览器(宿主机) ── kubectl port-forward ──► kind 集群内:
  cubepilot Deployment (Go: 助手服务 + Instance Manager 合并)
     ├──► 每用户 OpenClaw Pod  svc/agent-<user> (ClusterIP:18789)
     │         exec ──► kubectl(in-cluster SA token) ──► 同一 kind 集群
     └──► K8s API(client-go): Pod/PVC/Service 生命周期
```

- 每用户隔离 = **Pod + 独立 PVC**（NFR-002），会话持久化在各自 PVC（FR-M2-004）。
- 能力目录 = OpenClaw **Skills**（`capabilities/*/SKILL.md`）+ `workspace/SOUL.md`/`AGENTS.md`，烘焙进 agent 镜像。
- 对话流走 OpenClaw 的 `/v1/chat/completions`（OpenAI 兼容，`stream:true`，`model: openclaw/default`），网关侧跑完整 agent 循环；会话列表/历史走 `/tools/invoke`（`sessions_list`）与 `GET /sessions/{key}/history`。

## 目录

```
cmd/cubepilot           入口
internal/config         env 配置
internal/k8s            client-go + Pod/PVC/Service 构建
internal/instances      Instance Manager（Ensure/Reclaim/Heal）
internal/openclaw       OpenClaw HTTP 客户端 + 事件映射（含单测）
internal/server         REST/SSE 处理器 + Portal 静态服务
internal/inspect        巡检提示词
capabilities/           能力目录 SKILL.md ×4
workspace/              SOUL.md / AGENTS.md
ui/                     Portal（embed）
deploy/                 Dockerfile + RBAC + Service + kubeconfig 模板
scripts/setup.sh        一键部署
```

## 前置

- Docker、[kind](https://kind.sigs.k8s.io/)（集群名 `cube`）、`kubectl`、`jq`、Go 1.26+。
- 本机已有可用的 OpenClaw 配置 `~/.openclaw/openclaw.json`（含 `models.providers.deepseek` 与 `gateway.auth.token`），且已构建 `openclaw:local` 镜像。

## 运行

```bash
# 1. 构建镜像 + 装载进 kind + 创建 Secret/RBAC + 部署服务
scripts/setup.sh

# 2. 暴露 Portal
kubectl -n cubepilot port-forward svc/cubepilot 8080:8080

# 3. 打开 http://127.0.0.1:8080
```

首次发送消息会触发 Instance Manager 冷启动 `agent-zhang.wei` Pod（Portal 显示「正在思考…」，实为等待网关就绪），随后流式返回工具调用与回答。

## 验证点

| 验证 | 操作 | 预期 |
|---|---|---|
| 对话闭环 | 发「查询有哪些异常的 Pod」 | SSE 依次收到 `message_start → agent_thinking → tool_call(exec kubectl) → message_delta → message_done`，最终自然语言汇总真实 kind Pod 状态 |
| 按需拉起 | 首次消息 | `kubectl -n cubepilot get pods` 出现 `agent-zhang.wei` |
| 闲置回收 | 默认 TTL 5min，空闲后观察 | 对应 Pod 被删除，PVC 保留 |
| 异常自愈/记忆 | 手动删 Pod，再发消息 | IM 重建 Pod，会话/记忆仍在（PVC） |
| 用户隔离 | 用 `X-CubePilot-User: li.ming` 请求 | 各自独立 Pod/PVC |
| 巡检 | Portal「定时任务 → 立即执行」 | 返回节点/Pod 异常分级报告（`/api/inspect`） |

## 已知简化（PoC 边界）

- 助手服务与 Instance Manager 合并为一个进程（生产为分离组件，§11.1）。
- `agent-*` 用单一 ServiceAccount + 从宽 ClusterRole（生产为每用户最小 RBAC，FR-M3-001）。
- 能力目录烘焙进镜像（生产为 ConfigMap 挂载以支持「即时生效」FR-M2-005）。
- 未实现：cron 调度器、TaskRun/TaskTemplate/Capability CRD、HITL 确认（`confirm_*`）、审计 DB（M5）、RAG、多租户认证。均为阶段二/三。
- `tool_result` 事件在流中不单独出现（OpenClaw 的 agent 循环在服务端执行工具）；工具结果体现于最终回答文本，完整工具块可从会话历史读取。

## 本地开发（不经集群）

```bash
# 跑在宿主机，用 ~/.kube/config 连 kind（IM 仍创建 Pod，服务用 --listen）
CUBEPILOT_LISTEN=:8080 CUBEPILOT_GATEWAY_TOKEN=<token> go run ./cmd/cubepilot
# 注意：宿主机无法直连 ClusterIP，需为每个 agent Pod 单独 port-forward；推荐走集群内部署。
```

## 测试

```bash
go test ./...   # events.go 用 httptest 假网关断言 /v1/chat/completions → CubePilot 事件映射
```
