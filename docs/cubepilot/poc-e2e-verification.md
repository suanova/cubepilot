# CubePilot PoC 端到端验证记录

> 验证日期：2026-08-17（kind 集群 `cube`，K8s v1.36.1，OpenClaw agent 镜像 `cubepilot-agent:local`）
> 对应设计文档 §4.1 PoC 验收清单 ①~⑥ 与 §5.2/§5.4/§3.3/§9 关键机制。
> 事件映射契约见 [agent-runtime-event-mapping.md](./agent-runtime-event-mapping.md)。

## 环境

- kind 集群：`cube`（单 control-plane 节点）
- 助手服务：`deploy/service.yaml`，**2 副本**（多副本 Active/Standby，lease 选主）
- 每用户实例：`agent-<user>`（对话/巡检共用，创建者身份，SA `cubepilot-agent`）
- 数据：每用户 PVC `data-<user>` 1Gi；元数据 JSON 存后端 PVC

## 验收清单结果

| # | 验收项 | 结果 | 证据 |
|---|---|---|---|
| ① | 单会话消息往返（新消息 → 完整回复） | ✅ | `POST /api/messages` SSE：`message_start → agent_thinking → message_delta* → message_done` |
| ② | 工具调用往返（`tool_call`/`tool_result` 时序与载荷） | ✅ | 请求「查看节点状态」→ 流式 `tool_call`（kubectl get nodes / describe）→ `message_done`；M5 审计记录 L0 工具调用 |
| ③ | 流式增量（`message_delta`/`message_done`） | ✅ | 回复以多条 `message_delta` 增量输出，终态 `message_done` |
| ④ | 会话接回（实例重建后同一会话可继续） | ✅ | 删除 `agent-zhang-wei` → IM ~35s 自动补拉（常驻自愈）→ 同一 session key 追问「我刚才让你做了什么」，agent 准确答出历史内容（会话从 PVC 恢复） |
| ⑤ | 事件流捕获同步 → Message 账本 → 历史渲染/换 runtime 播种 | ✅ | `/ledger` 返回 19 行账本（user + assistant delta + message_done 终态）；`/seed` 播种 19 行成功；实例无需存活即可读账本 |
| ⑥ | 事件映射表定稿 | ✅ | `agent-runtime-event-mapping.md`（OpenClaw → 8 类 SSE 映射 + 降级契约） |

## 关键机制验证

### 1. 控制面 Leader Election 边界（§3.3/§11.1）
- Leader Election 仅适用于控制面组件（IM/调度器）；Agent 实例是每用户有状态单例（单副本、单写者，0~N 为用户数），不做副本复制，可靠性由 K8s 自愈 + 数据目录持久承接。
- IM/调度器**单副本起步**（`CUBEPILOT_REPLICAS=1`，deploy replicas=1）；控制器化实现时随框架（--leader-elect）平滑升 2 副本。
- 单副本阶段：`leader.New(..., replicas<=1)` 恒为 leader（单测覆盖），无选举开销、行为与旧版一致。

### 2. 常驻策略 + 自愈补拉（§5.2 / FR-M2-002）
- 默认 `CUBEPILOT_RECLAIM=false`：实例常驻，不闲置回收。
- **修复的缺陷**：初版 reconcile 只 heal 已存在的 crash-loop pod，不补拉被意外删除的 pod。已改为常驻模式下按 PVC（数据目录）反推用户，pod 缺失即重建。
- 实证：`kubectl delete pod agent-zhang-wei` → ~35s 内自动补拉并 Ready（数据目录 PVC 复用，会话无损）。

### 3. 巡检权限模型（§5.4：以创建者身份）

> 2026-08-17 设计确认：撤销「专用只读 kubeconfig」技术强制，巡检以创建者身份执行、
> 权限与创建者一致（FR-M4 授权约定）；只读由巡检模板行为约束 + RBAC 兜底，残留风险与
> 「创建者派生只读凭据」加固项（阶段二、Q-002）记入设计文档 §5.4/§13。

- 巡检任务（`/api/inspect`、定时巡检）复用当前用户实例 `agent-<user>`，以创建者身份 + 创建者 RBAC 执行，不另建实例/凭据。
- 巡检模板（`inspect.go` prompt）行为约束：只读命令（`get/list/watch/logs`）、禁止写操作、每项发现附证据链、疑似发现标注「AI 疑似，需人工复核」；无权限项被 RBAC 拒绝并如实标注，不重试被拒操作。
- 实证：巡检报告正常产出（`P0:0 / P1:1 / P2:1`，结构化含证据链），以 `zhang.wei` 身份执行。

### 4. 数据目录 GC（§5.1 / §10）
- `internal/instances/manager.go` `gcDataDirs`：leader 每 5min 对每用户 PVC 执行 `find -mmin +窗口` 清理超期 session/transcript（默认 72h 滑动窗口）。
- 水位：exec `df` 实测 PVC 使用率，>70%（`CUBEPILOT_GC_WATERMARK`）告警日志。
- 账本行同步清理：`store.GCExpiredMessages(72h)`（单测覆盖）。

### 5. 可观测性（§9）
- `GET /metrics`：`cubepilot_messages_total`（role）、`cubepilot_sessions_total`、`cubepilot_turns_total`（status）、`cubepilot_tool_calls_total`（level）、`cubepilot_pool_instances/reclaims/rebuilds`、首 Token/整轮延迟 summary（P95/avg）。
- 结构化日志：`conversation_id / user / tool_call_id` 关联字段随事件流转（`ledgerEvent`）。

### 6. 会话真源（§4.1）
- 平台 Message 账本为消息历史真源：user 消息先落账（中途失败也持久），SSE 事件在转发路径顺带落账（event-sourcing），`message_done` 将 assistant 行置终态（失败标 `incomplete` + error）。
- `/api/sessions/{key}/ledger`：读账本（实例无需存活）。
- `/api/sessions/{key}/seed`：账本近期行播种新 runtime（换运行时接回）。
- M5 审计：工具调用经 transcript 回放记录（`/api/audit`，L0/L1 分类）。

## 遗留 / 后续

- 阶段二：HITL（`confirm_pending/confirm_resolved` 契约 + `requireApproval` 钩子）、Message 表作为正式持久层、能力目录按 RBAC 可见范围动态加载。
- 调度器多副本故障转移演练（kill leader → standby 接管）未做破坏性测试，仅验证了 lease 唯一性。
- Agent Runtime Adapter 接口契约（§4.1）已由本 PoC 回填事件映射表，正式冻结待阶段二 HITL 事件补全后。
