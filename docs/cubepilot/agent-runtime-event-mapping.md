# CubePilot · Agent Runtime 事件映射表（E1 契约实测回填）

> 对应模块设计文档 §4.1（扩展点一：Agent Runtime Adapter）「契约冻结与验证」验收⑥：
> 事件映射表（OpenClaw 原生事件 → 8 类 SSE）定稿。
> 本文档由阶段一实测回填，2026-08-17 与 `internal/openclaw/events.go` 同步。

## 1. 契约

CubePilot SSE 事件契约共 **8 类**，阶段一落地其中 **6 类**（`confirm_*` 为阶段二 HITL 启用）：

| # | 事件 | 方向 | 阶段一 | 载荷要点 |
|---|---|---|---|---|
| 1 | `message_start` | 服务 → 客户端 | ✅ | `session_id` |
| 2 | `agent_thinking` | 服务 → 客户端 | ✅ | `session_id`（LLM 规划阶段） |
| 3 | `tool_call` | 服务 → 客户端 | ✅ | `session_id / name / call_id / arguments` |
| 4 | `tool_result` | 服务 → 客户端 | ✅ | `session_id / name / output` |
| 5 | `message_delta` | 服务 → 客户端 | ✅ | `session_id / delta`（流式文本增量） |
| 6 | `message_done` | 服务 → 客户端 | ✅ | `session_id / error?`（终态；`error` 非空 = 本轮失败） |
| 7 | `confirm_pending` | 服务 → 客户端 | ❌ 二期 | HITL 待确认 |
| 8 | `confirm_resolved` | 服务 → 客户端 | ❌ 二期 | HITL 确认结果 |

**降级契约**：若运行时无法精确映射全部 8 类，最少必要事件为
`message_delta / message_done / tool_call / tool_result`（4 类）。
OpenClaw 映射已覆盖 6/8（缺 `confirm_*`，属预期，阶段二以
`registerTrustedToolPolicy` / exec approval 钩子补齐）。

## 2. OpenClaw 原生 → CubePilot 映射

OpenClaw 侧有三个数据来源，映射逻辑分别位于：

- `internal/openclaw/events.go`：`streamMapper` —— 将 OpenAI 兼容
  `/v1/chat/completions` 流式 chunk 折叠为 CubePilot 事件；
- `internal/server/handlers.go`：`handleMessages` —— 补发 `message_start /
  agent_thinking`（HTTP 处理层，含实例冷启动「warming」信号）；
- `internal/server/handlers.go`：`extractToolEvents` + `parseHistoryTools` ——
  chat 流不含工具调用，从会话 transcript（`GET /sessions/{key}/history`）
  回放 `tool_call / tool_result`。

### 2.1 流式文本（/v1/chat/completions SSE）

| OpenClaw chunk | CubePilot 事件 | 说明 |
|---|---|---|
| `choices[].delta.content` 非空 | `message_delta` | 追加 `delta`；首个 delta 触发首 Token 计时 |
| `choices[].finish_reason` | `message_done` | 流结束（`[DONE]` 或 finish_reason）；错误路径由 `fail()` 发 `message_done{error}` |
| 请求发出前（HTTP 层） | `message_start` | 携带 `session_id` |
| 请求发出后、首个内容前（HTTP 层） | `agent_thinking` | 前端可展示「思考中…」；冷启动时前端先显示「正在唤醒助手…」 |

### 2.2 工具调用（transcript 回放）

OpenClaw 的 chat-completions 流**不暴露工具调用**，工具往返从会话
transcript（`sessions_history` 的结构化消息，含 `toolCall` / `toolResult`
块）回放：

| OpenClaw transcript 块 | CubePilot 事件 | 说明 |
|---|---|---|
| `content[].type == "toolCall"` | `tool_call` | `name / call_id / arguments`（arguments 为 JSON 原文） |
| `content[].type == "toolResult"` | `tool_result` | `output`（结果文本） |

回放顺序：`message_done` 前先回放工具事件（`emit` 内持有 `doneEvent`），
保证客户端按 `tool_call → tool_result → message_delta → message_done`
顺序渲染。

### 2.3 阶段二 HITL（预留，未映射）

OpenClaw 原生确认钩子（`registerTrustedToolPolicy` → `requireApproval` /
exec approval，`allowed-once / rejected` 回执，见
`docs/notes/human-in-the-loop-openclaw.md`）经 Adapter 映射为
`confirm_pending / confirm_resolved`；阶段二冻结，本文档届时追加映射行。

## 3. 会话真源（事件流捕获落账）

设计 §4.1「会话真源 = 平台为真源」：M1 对话域是 SSE 中枢，**在转发路径上
顺带把事件写入 Message 账本**（event-sourcing）。

| SSE 事件 | Message 账本行 |
|---|---|
| （请求入口） | `role=user, content=原文`（先落账，保证中途失败也持久） |
| `tool_call` | `role=tool, eventType=tool_call, toolName, callId, toolCalls(JSON)` |
| `tool_result` | `role=tool, eventType=tool_result, content=output` |
| `message_delta` | `role=assistant, eventType=message_delta, content=delta`（每增量一行） |
| `message_done` | `TurnEnd()` 把该轮最后一条 assistant 行置终态：`eventType=message_done`；失败时 `incomplete=true, error=…` |

**用途**：

- 历史渲染：`GET /api/sessions/{key}/ledger`（无需实例存活）；
- 换运行时播种：`POST /api/sessions/{key}/seed` 用账本近期行（≤50 条）
  重组 chat 历史播种进新 runtime 会话（§4.1 换运行时接回）；
- 保留策略：`Instance Manager` 数据目录 GC（72h 滑动窗口，§5.1/§10）+
  `store.GCExpiredMessages`（账本行同步清理）。

## 4. 验收对照（§4.1 清单 ①~⑥）

| # | 验收项 | 结果 | 证据 |
|---|---|---|---|
| ① | 单会话消息往返（新消息 → 完整回复） | ✅ | `POST /api/messages` SSE 流 `message_start → agent_thinking → message_delta* → message_done` |
| ② | 工具调用往返（`tool_call` / `tool_result` 时序与载荷） | ✅ | transcript 回放 `extractToolEvents`（含重试 4×500ms 等 flush） |
| ③ | 流式增量（`message_delta` / `message_done`） | ✅ | `streamMapper.mapChunk` 折叠 content delta；`finish()` 终态 |
| ④ | 会话接回（实例重建后同一会话可继续） | ✅ | 会话持久于 PVC（`sessions.json` + transcript JSONL），Pod 重建后 `x-openclaw-session-key` 直达同会话 |
| ⑤ | 事件流捕获同步 → Message 账本 → 历史渲染 / 换 runtime 播种 | ✅ | `ledgerEvent` 转发路径落账；`/ledger`、`/seed` 端点 |
| ⑥ | 事件映射表定稿 | ✅ | 本文档 |

> 端到端验证记录见 `docs/cubepilot/e2e-verification.md`（随验证执行回填）。
