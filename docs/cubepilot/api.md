# CubePilot API 文档（HTTP / SSE）

> **读者**：任何要对接 `cubepilot-api` 的客户端——内置 Portal、CubeStack 统一 UI、
> 或第三方集成。启动内置 Portal 之外的 UI 时用 `--set web.enabled=false` 关掉 Portal。
>
> **来源**：本文以 `internal/server/` 的实现为准（路由表见 `internal/server/server.go`
> 的 `Handler()`）。字段级参考不在此重复：CRD 字段见 `config/crd/bases/`，
> 客户端类型见 `web/src/api/types.ts`。
>
> **维护**：`internal/server/apidoc_test.go` 会在路由表与本文不一致时失败。
> 新增或删除端点必须同步更新本文，否则 CI 报错。

---

## 目录

1. [架构与接入](#1-架构与接入)
2. [通用约定](#2-通用约定)
3. [调用顺序](#3-调用顺序)
4. [对话流程](#4-对话流程)
5. [人机协同（HITL）](#5-人机协同hitl)
6. [端点参考](#6-端点参考)
7. [SSE 事件参考](#7-sse-事件参考)
8. [客户端易错点](#8-客户端易错点)

---

# 1. 架构与接入

```text
浏览器 / 客户端
     │  HTTP + SSE   /api/*
     ▼
反向代理（生产 nginx / 开发 vite dev server）
     │
     ▼
cubepilot-api:8080  ──WebSocket──▶  每用户一个 OpenClaw 实例（Pod）
                                              │
                                            exec → kubectl → 集群
```

客户端**只需要访问 `/api/*`**，不需要、也拿不到 Kubernetes API server 的访问权限。
CRD 资源的读写全部由 `cubepilot-api` 代理。

## 反向代理的硬性要求

SSE（`POST /api/messages`）必须**关闭代理缓冲**，否则事件会被攒着一起吐，失去流式效果。
参考 `web/nginx.conf`：

```nginx
location /api/ {
    proxy_pass         http://cubepilot-api:8080;
    proxy_http_version 1.1;
    proxy_buffering    off;      # SSE 必需
    proxy_read_timeout 3600s;    # 长回合
    proxy_set_header   Connection "";
}
```

---

# 2. 通用约定

## 2.1 身份

每个请求都带：

```http
X-CubePilot-User: <用户名>
```

- 取不到该头时，后端回退到配置的默认用户（环境变量 `CUBEPILOT_DEFAULT_USER`，默认 `admin`）。
- **当前没有认证**。这个头是可伪造的，身份由调用方自报；多租户隔离靠后端按此值过滤，
  RBAC 是最终闸门。生产接入前需要在此之上补认证。

## 2.2 响应信封（**不统一，重点**）

多数端点用具名 key 包裹返回值，但**有一批是裸对象或无包裹的原始字节**：

| 有包裹 | 包裹 key |
| --- | --- |
| `/api/sessions` | `sessions` |
| `/api/audit` | `entries` |
| `/api/agent/config` | `config` |
| `/api/agenttemplates` · `/api/agenttemplates/{name}` | `agentTemplates` · `agentTemplate` |
| `/api/instances` POST | `instance` |
| `/api/instances` GET | `instances` |
| `/api/llms` · `/api/llms/{name}` | `model` · `model`（DELETE 用 `removed`） |
| `/api/skills` | `skills` |
| `/api/skills/{name}/install` · `uninstall` | `enabledSkills` |
| `/api/tasks` GET · POST · `/toggle` · `/run` | `tasks` · `task` · `task` · `{started, task}` |
| `/api/tasks/{id}/reports` | `reports` |
| `/api/tasktemplates` | `taskTemplates` |
| `/api/taskruns` · `/api/taskruns/{name}` | `taskruns` · `taskrun` |
| `/api/kinds` | `kinds` |
| `/api/sessions/{key}/question/pending` | `questions` |

| **无包裹（裸对象 / 原始字节）** | 说明 |
| --- | --- |
| `GET /api/agent/status` | 裸 `AgentStatus` |
| `GET /api/agent/confirm` · `PUT /api/agent/confirm` | 裸 `confirmView` |
| `GET /api/sessions/{key}/confirm/pending` | 裸 `PendingConfirm` |
| `GET /api/sessions/{key}/messages` | **原始 JSON 透传**（运行时历史文档），不重新编码 |
| `POST /api/skills/{name}/publish` | 裸 `Skill` CR |
| `POST /api/messages` | SSE 流，不是 JSON |
| 全部 `/internal/*` | 集群内部端点，见 §6.5 |

## 2.3 错误与状态码语义

错误体统一为：

```json
{ "error": "人类可读的原因" }
```

以下状态码**有特定语义**，客户端必须区别处理：

| 码 | 含义 | 客户端应当 |
| --- | --- | --- |
| **503** | `instance warming failed: ...` —— 实例正在冷启动（Pod 未就绪 / 网关未监听） | **等待并重试**，提示「正在启动实例」。这不是故障 |
| **503** | `CRD path disabled` —— 部署未启用 CRD 路径 | 视为部署配置问题，不要重试 |
| **409** | `another turn is already streaming for this session` | 同一会话已有回合在跑。**不要重试发送**，提示等待或先调 `/abort` |
| **404** | `no pending approval` / `no pending question` | 正常的「已过期 / 无未决项」，**静默忽略** |
| **502** | 网关往返失败 | 后端到实例的链路问题，可重试一次 |
| **504** | `the run did not settle in time; try again`（仅 `/abort`） | 重试 |
| **413** | 仅技能发布，tar 超过 10 MiB | 换更小的包 |
| **202** | 仅 `POST /api/tasks/{id}/run`，表示已登记手动触发 | 正常成功 |
| **201** | 仅 `POST /api/instances`（新建）；已存在时返回 200 + `alreadyExists: true` | 正常成功 |

**注意**：`404` 有两种形态——JSON 的 `{"error": ...}`（业务意义上的「没有」），
和 Go `http.NotFound` 的**纯文本** `404 page not found`（路径写错、路径段为空、
通配路由带尾斜杠）。客户端解析 404 体前要容错。

## 2.4 方法检查不一致

绝大多数端点对错误方法返回 `405` + JSON `{"error": "..."}`，但**这四个端点不检查方法**，
任何 method 都按正常流程返回 200：

- `GET/POST/... /api/sessions`
- `/api/sessions/{key}/messages`
- `/api/agent/status`
- `GET /internal/agents/{user}/config`

## 2.5 路径细节

- `sessionKey` 含冒号（形如 `agent:main:conv-<uuid>`），**必须 URL 编码**。
- 会话子资源靠**后缀**匹配，所以 `/api/sessions/a/b/messages` 也命中，且 `sessionKey` 取 `a/b`。
- 通配路由带尾斜杠会落到 mux 的 404（如 `/api/llms/`），不会匹配 `{name}`。

---

# 3. 调用顺序

## 3.1 关键前提：只有 4 个端点会「加热」实例

CubePilot 的 agent 实例是**常驻**的（起来后不回收），但第一次访问要**冷启动**一个 Pod，
可能耗时数十秒。只有下面 4 个端点会触发这个过程（它们调用 `mgr.Ensure`）：

| 会加热（可能慢、可能 503） |
| --- |
| `GET /api/sessions` |
| `GET /api/sessions/{key}/messages` |
| `POST /api/messages` |
| `POST /api/inspect` |

**其余所有端点都不加热**——它们直接读 CR 或网关，永远快。
调用顺序就建立在这条分界上。

## 3.2 推荐的启动序列

```ts
// ── 阶段 0：并行发出，不碰实例，首屏立刻可渲染 ──────────────
const [status, config, confirm] = await Promise.all([
  api.agentStatus(),     // 实例存在吗？phase 是什么？
  api.agentConfig(),     // 我选的模型 / 提示词
  api.agentConfirm(),    // 我的确认策略
])

if (!status.exists) {
  // 还没实例 → 引导用户去「Agent 配置」页，调 POST /api/instances
  // 此时不要调 /api/sessions，只会 503
  return showOnboarding()
}

// ── 阶段 1：会加热，必须容忍 503 ─────────────────────────────
try {
  const sessions = await api.listSessions()
} catch (e) {
  if (e.status === 503) showStartingUp("正在启动实例…")
  else throw e
}
```

**为什么是这个顺序**：`agentStatus` 不加热，能立刻回答「有没有实例」，
这是决定 UI 形态的第一问。没有实例时调 `listSessions` 只会白等并失败。

## 3.3 首次使用（尚无实例）

```text
GET  /api/agenttemplates          → 挑模板（默认 cubepilot）
POST /api/instances               → 创建实例（201；已存在则 200 + alreadyExists）
       ↑ 此后 /api/sessions 等端点才可用
```

创建后实例仍需控制器调度，`GET /api/agent/status` 的 `phase` 会经历
`Creating` → `Ready`。`phase` 为 `not provisioned (resident policy)` 表示实例不存在。

---

# 4. 对话流程

## 4.1 发送一条消息（SSE）

```ts
const resp = await fetch('/api/messages', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'X-CubePilot-User': user,
  },
  body: JSON.stringify({
    session_id: currentSessionId,  // 新会话传 null / '' / 省略
    content: text,                 // 必填，空白会被拒
  }),
})
```

请求体：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `content` | 是 | 用户消息。空或纯空白 → `400 {"error":"content required"}` |
| `session_id` | 否 | 省略或空 → 后端生成 `conv-<uuid>` |

**新会话不要自己编 key。** 后端会生成并**规范化**为 `agent:main:<key>`，
然后通过第一个事件告知：

```text
event: message_start
data: {"type":"message_start","session_id":"agent:main:conv-9f3a..."}
```

**必须保存这个 `session_id`**，后续所有请求都用它（它就是 `{key}`）。

响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`。
空闲 15 秒会收到注释行 `: ping` 保活（客户端应忽略非 `data:` 行）。

**请求阶段的失败不走 HTTP 状态码**（流已经打开了），而是以 SSE 事件返回：

```json
{"type":"message_done","session_id":"...","error":"instance warming failed: ..."}
```

## 4.2 SSE 帧格式

每帧一个 `event:` 行加一个 `data:` 行，空行结束：

```text
event: message_delta
data: {"type":"message_delta","session_id":"agent:main:conv-x","delta":"集群里有"}

```

`event:` 名与 `data` 里的 `type` 字段始终一致。全部字段见 §7。

**客户端解析要求**：
- `EventSource` 只支持 GET，此处是 POST，**必须用 `fetch` + `response.body.getReader()` 手写解析**。
- 流可能在 `message_done` 之前断掉（网络抖动、服务重启）。
  **此时要合成一个终止事件**让 UI 复位，否则界面永久卡在「进行中」。

参考实现：`web/src/api/sse.ts`。

## 4.3 会话列表与历史

```ts
GET /api/sessions                        → {"sessions":[{"sessionKey","title"}]}
GET /api/sessions/{encodeURIComponent(key)}/messages
                                         → {"items":[HistoryMessage]}   // 原始透传
```

两者**都要求实例是热的**——平台不存消息副本，会话内容的唯一真相源是实例自身的运行时。
实例不可达时**读不到历史**（是失败，不是「读到旧数据」）。

**历史消息的 `content` 有两种形状**，必须归一化：

| `role` | `content` 形状 |
| --- | --- |
| `user` | **字符串** |
| `assistant` / `toolResult` | **内容块数组**：`[{"type":"text"\|"toolCall","text"?,"name"?,"id"?,"arguments"?}]` |

把字符串当数组遍历会逐字符拆开，用户消息会整条消失。参考 `web/src/views/ChatView.tsx` 的 `blocks()`。

## 4.4 停止进行中的回合

```ts
POST /api/sessions/{key}/abort   → {"ok":true}
```

- **幂等**：没有运行中的回合时直接返回 200，不发任何网关 RPC。
- 最长等待约 5 秒让回合落定；超时 → `504 {"error":"the run did not settle in time; try again"}`。
- 不加热实例。
- 被停止的回合以 `message_done` + `"stopped": true` 结束（注意：`stopped` 与 `error` 互斥）。

## 4.5 查询回合状态

```ts
GET /api/sessions/{key}/turn   → {"active":true|false}
```

状态来自网关（不是本地 hub），响应带 `Cache-Control: no-store`。
可用于页面加载时判断「刷新前那轮还在跑吗」。

---

# 5. 人机协同（HITL）

两条独立的通道，都复用**同一条对话 SSE 流**：审批（写操作确认）和问答（`ask_user`）。
共同模式是「事件弹出 → 用户决定 → POST 回答 → 收到 resolved 事件」。

## 5.1 写操作确认

当 agent 要执行被策略拦截的写操作时，回合在网关侧暂停：

```text
event: confirm_pending
data: {"type":"confirm_pending","session_id":"...","call_id":"<approval id>",
       "name":"exec","command":"kubectl delete pod x","level":"write","message":"..."}
```

用户决定后提交：

```ts
POST /api/sessions/{encodeURIComponent(key)}/confirm
body: {"decision": "approve" | "reject" | "allow-always"}
```

| decision | 效果 |
| --- | --- |
| `approve` | 本次放行，回合继续 |
| `reject` | 拒绝，写操作不执行 |
| `allow-always` | 本次放行，**并把该命令记入实例 allowlist**，此后自动通过 |

响应：`{"approved":bool,"decision":"...","approval_id":"...","allowlisted"?:bool}`

随后同一流上收到：

```text
event: confirm_resolved
data: {"type":"confirm_resolved","session_id":"...","call_id":"...","approved":true}
```

**刷新后恢复卡片**（必需，不是可选）：

```ts
GET /api/sessions/{key}/confirm/pending
// 404 {"error":"no pending approval"} → 静默忽略
// 200 裸对象：{"session_id","approval_id","tool","command","level","message"}
```

**失败关闭语义**：确认策略要求「问」时，若审批通道不可用，回合会**直接失败**而不是静默放行。
因此 `503 {"error":"approval channel unavailable"}` 表示该回合被拒——不要重试成「跳过确认」。

## 5.2 `ask_user` 问答

agent 调用 `ask_user` 工具时，回合同样暂停：

```text
event: question_pending
data: {"type":"question_pending","session_id":"...","call_id":"<question id>",
       "question":{"questions":[{"questionId","header","question",
                                 "options":[{"label","description"?}],"multiSelect"?}],
                   "timeoutSeconds":n}}
```

**注意 `call_id` 是问答会话的 id，`question.questions[].questionId` 是每个问题的 id**，
提交答案时用的是后者。

```ts
// 回答
POST /api/sessions/{encodeURIComponent(key)}/question
body: {"id": "<call_id>", "answers": {"<questionId>": ["选项 label"]}}

// 或取消（让 agent 继续而不是等到超时）
POST /api/sessions/{encodeURIComponent(key)}/question
body: {"id": "<call_id>", "cancel": true}
```

`answers` 与 `cancel` **必须二选一**，同时给或都不给 → `400 {"error":"send either answers or cancel"}`。

响应：`{"question_id":"...","cancelled":bool}`

```text
event: question_resolved
data: {"type":"question_resolved","session_id":"...","call_id":"...",
       "message":"answered" | "cancelled" | "expired"}
```

**刷新后恢复**：

```ts
GET /api/sessions/{key}/question/pending
// 404 {"error":"no pending question"} → 静默忽略
// 200 {"questions":[{"id","questions":[QuestionItem],"timeoutSeconds"?}]}
```

`timeoutSeconds` 是事件产生时的**剩余时间**，不是绝对截止时刻——倒计时不要依赖客户端时钟与
服务端一致。

**可能的错误**（`POST .../question`）：

| 状态 | `error` | 含义 |
| --- | --- | --- |
| 404 | `no such pending question for this session` | 该 id 不属于这个会话 |
| 404 | `QUESTION_NOT_FOUND` | 网关侧已无此问题 |
| 409 | `question is no longer pending` | 已过期或已回答 |
| 409 | `QUESTION_ALREADY_TERMINAL` | 同上（网关侧表述） |
| 400 | `QUESTION_INVALID_ANSWER` | 答案不符合选项定义 |
| 503 | `question channel unavailable` | 问答通道不可用 |
| 502 | 其他 | 网关往返失败 |

收到 404 / 409 时应**清掉本地卡片**——问题已不可回答。

---

# 6. 端点参考

## 6.1 对话与会话

| 方法 | 路径 | 请求 | 响应 | 加热 |
| --- | --- | --- | --- | --- |
| ANY | `/api/sessions` | — | `{"sessions":[{"sessionKey","title"}]}` | 是 |
| ANY | `/api/sessions/{key}/messages` | — | 原始历史 JSON（`{"items":[...]}`） | 是 |
| POST | `/api/messages` | `{"session_id"?,"content"}` | **SSE 流** | 是 |
| POST | `/api/inspect` | — | `{"report":"<自然语言文本>"}` | 是 |
| POST | `/api/sessions/{key}/confirm` | `{"decision"}` | `{"approved","decision","approval_id","allowlisted"?}` | 否 |
| GET | `/api/sessions/{key}/confirm/pending` | — | 裸 `{"session_id","approval_id","tool","command","level","message"}` | 否 |
| POST | `/api/sessions/{key}/question` | `{"id","answers"\|"cancel"}` | `{"question_id","cancelled"}` | 否 |
| GET | `/api/sessions/{key}/question/pending` | — | `{"questions":[...]}` | 否 |
| POST | `/api/sessions/{key}/abort` | — | `{"ok":true}` | 否 |
| GET | `/api/sessions/{key}/turn` | — | `{"active":bool}` | 否 |

## 6.2 Agent 配置与实例

| 方法 | 路径 | 请求 | 响应 | 加热 |
| --- | --- | --- | --- | --- |
| GET | `/api/agent/config` | — | `{"config":{"exists","model","systemPrompt"}}` | 否 |
| PUT | `/api/agent/config` | `{"config":{"model","systemPrompt"}}` | 同上 | 否 |
| GET | `/api/agent/status` | — | **裸** `{"user","id","exists","phase","gatewayImage","gatewayPort",...}` | 否 |
| GET | `/api/agent/confirm` | — | **裸** `confirmView`，见下 | 否 |
| PUT | `/api/agent/confirm` | `{"confirmPolicy","allowlist":[...]}` | **裸** `confirmView` | 否 |
| GET | `/api/instances` | — | `{"instances":[...]}` | 否 |
| POST | `/api/instances` | `{"templateRef","selectedModel","enabledSkills","userInstructions"}` | `201 {"instance":{...}}` | 否 |
| GET | `/api/agenttemplates` | — | `{"agentTemplates":[...]}` | 否 |
| GET | `/api/agenttemplates/{name}` | — | `{"agentTemplate":{...}}` | 否 |

`PUT /api/agent/config` 的 body **外面多包一层 `config`**——全 API 只有它这么干。

`confirmView`（裸对象）：

```json
{
  "exists": true,
  "confirmPolicy": "None | Allowlist | AlwaysAsk | \"\"",
  "override": "",                       // 实例自身设定，"" = 继承模板
  "templatePolicy": "Allowlist",
  "allowlist":     [{"pattern","argPattern?","label"?}],
  "allowlistOwned":[...],
  "channel": "up | pairing | down | unconfigured | \"\""
}
```

- `confirmPolicy` 只接受 `""` / `None` / `Allowlist` / `AlwaysAsk`，其他值 → 400。
- `label` **只在**规则精确匹配平台内置只读规则时出现；用户自己加的规则没有 `label`，
  界面上不要把它当作只读展示。
- `channel` 为 `""` 表示策略是 `None` 或实例不存在；`unconfigured` 表示策略要求拦截但
  HITL 通道未配置（此时拦截会失败关闭）。
- `allowlist` / `allowlistOwned` 为空时字段**缺省**（不是 `[]`）。

**Agent 配置的常见错误**：

- `400 model "x" is not in the cubepilot template (add it under Agent Config -> LLM Config first)`
  —— 模型没进模板的 `spec.models`；空 model 永远允许。
- `409 no agent instance yet — provision it on the Agent Config page first`
  —— 实例不存在，先去创建。

## 6.3 模型目录（LLM）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| POST | `/api/llms` | `{"name","endpoint","apiKey"?,"public"?}` | `200 {"model":{...}}` |
| PUT | `/api/llms/{name}` | 同上（`apiKey` 省略 = 保留原凭证） | `200 {"model":{...},"warning"?}` |
| DELETE | `/api/llms/{name}` | — | `200 {"removed":"<name>","warning"?}` |

- `apiKey` 与 `public` **互斥**：公开模型不能带凭证；非公开模型必须给 key。
- `name` 不可变——改名要删了重建。
- `PUT` 时省略 `apiKey` = 保留已存凭证；`public:true` 会清掉凭证。
- `DELETE` 若该模型正被实例选用 → `409`，错误体会**额外带一个 `instances` 数组**：

```json
{"error":"model \"x\" is selected by alice, bob; select another model there first",
 "instances":[{"name":"...","owner":"alice"}]}
```

这是**唯一**要求客户端解析结构化错误体的地方（Portal 会把它渲染成可点击的实例列表）。

## 6.4 任务与技能

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/api/tasks` | — | `{"tasks":[taskDTO]}` |
| POST | `/api/tasks` | `{"name","prompt"?,"schedule"?,"templateRef"?,"params"?,"state"?}` | `{"task":taskDTO}` |
| DELETE | `/api/tasks/{id}` | — | `{"deleted":"<id>"}` |
| POST | `/api/tasks/{id}/run` | — | **202** `{"started":true,"task":taskDTO}` |
| POST | `/api/tasks/{id}/toggle` | — | `{"task":taskDTO}` |
| GET | `/api/tasks/{id}/reports` | — | `{"reports":[reportDTO]}` |
| GET | `/api/tasktemplates` | — | `{"taskTemplates":[...]}` |
| GET | `/api/taskruns` | `?task=<name>` 可选 | `{"taskruns":[...]}` |
| GET | `/api/taskruns/{name}` | — | `{"taskrun":{...}}` |
| GET | `/api/audit` | `?limit=`（默认 400） | `{"entries":[...]}` |
| GET | `/api/kinds` | — | `{"kinds":[...]}` |
| GET | `/api/skills` | — | `{"skills":[...]}` |
| POST | `/api/skills/{name}/publish` | query `displayName`（必填）、`description`；body = gzip tar | 裸 `Skill` CR |
| POST | `/api/skills/{name}/install` | — | `{"enabledSkills":[...]}` |
| POST | `/api/skills/{name}/uninstall` | — | `{"enabledSkills":[...]}` |

`POST /api/tasks` 的字段规则：

- `name` 必填；`prompt` 与 `templateRef` **至少一个**。
- `schedule` 是**指针语义**：省略 ⇒ 用模板的 `defaultCron`（或 Manual）；显式 `""` ⇒ Manual；
  给值 ⇒ 按 5 字段 cron 解析（按 UTC 求值，非法 → 400）。
- `params` 必须配合 `templateRef`，否则 400。
- `state` 为 `Enabled` / `Paused`。

`taskDTO` 字段：`id`（CR 名）、`name`（显示名）、`prompt`、`schedule`、`templateRef?`、
`state`、`enabled`、`creator`、`createdAt`、`lastRunAt?`、`lastStatus?`、`nextRunAt?`。

`reportDTO` 字段：`id`、`taskId`、`taskName`、`trigger`（`Manual|Cron`）、
`status`（`success|failed|running`）、`startedAt`、`finishedAt`、`content`、`p0`、`p1`、`p2`。
`running` 包含「已入队但未开始」的状态。

`AuditEntry` 字段：`id`、`ts`、`user`、`sessionId`、`tool`、`command`、
`level`（`L0` 只读 / `L1` 写）、`status`（`executed|approved|rejected|failed`）、`detail?`。

**技能发布**：`displayName` 走 **query 参数**（不是 body），body 是**原始 gzip tar 字节**
（`Content-Type: application/gzip`）。tar 上限 10 MiB → 超出 `413`。
当前只支持 `visibility=Platform`，其他值 → 400。

## 6.5 集群内部端点（`/internal/*`）

**客户端不应调用。** 这些是 agent Pod 内的 supervisor 向 API 拉配置用的，
不经过 Portal，也不做用户身份校验：

| 路径 | 用途 |
| --- | --- |
| `GET /internal/agents/{user}/config` | supervisor 拉解析后的 agent 配置 |
| `GET /internal/gateway/config/{user}` | supervisor 拉渲染好的 `openclaw.json` |
| `GET /internal/skills/{name}/tar` | supervisor 拉技能包 |

---

# 7. SSE 事件参考

`POST /api/messages` 的全部事件（`event:` 名与 `data.type` 一致）。
除 `type` 外所有字段都是 `omitempty`——**不出现即缺席**。

| `type` | 载荷字段 | 含义 |
| --- | --- | --- |
| `message_start` | `session_id` | 回合开始，**保存 `session_id`** |
| `agent_thinking` | `session_id` | agent 正在思考 |
| `message_delta` | `session_id`,`delta` | 追加正文（增量） |
| `text_replace` | `session_id`,`delta` | **替换**正文（不是追加） |
| `tool_call` | `session_id`,`name`,`call_id`,`arguments` | 一次工具调用开始 |
| `tool_result` | `session_id`,`name`,`call_id`,`output` | 该工具的输出 |
| `confirm_pending` | `session_id`,`call_id`,`name`,`command`,`level`,`message` | 写操作待确认 |
| `confirm_resolved` | `session_id`,`call_id`,`approved` | 确认已提交 |
| `question_pending` | `session_id`,`call_id`,`question` | 问答待回答（结构见 §5.2） |
| `question_resolved` | `session_id`,`call_id`,`message` | `answered`/`cancelled`/`expired` |
| `message_done` | `session_id`,`error?`,`stopped?` | **唯一的终止事件** |

要点：

- **`message_done` 是唯一终止信号**，且**必须处理**：流可能提前断开，
  客户端要自行合成一个 `message_done` 复位 UI。`error` 与 `stopped:true` 互斥。
- **`text_replace` 必须替换而非追加**。网关会在工具执行后重写先前的解说文本，
  当成 `message_delta` 追加会出现重复内容。
- `tool_result` 与 `tool_call` 通过 `call_id` 配对；没有 `call_id` 时按**到达顺序**
  与最旧的未完成调用配对（`web/src/views/ChatView.tsx` 的 `attachToolResult`）。
- 事件可能来自**其他连接**（审批/问答由网关侧广播注入），
  所以「收到 `confirm_pending` 时不一定正好在你自己那次请求的处理路径上」。
- 空闲 15 秒会有注释行 `: ping`，不是事件。

---

# 8. 客户端易错点

按踩坑代价排序：

1. **503 是「正在冷启动」，不是故障** —— 要等待并重试，并给用户明确反馈。
2. **409 不要重试** —— 同一会话同时只有一个回合；应提示等待或调 `/abort`。
3. **`text_replace` 是替换不是追加** —— 否则正文重复。
4. **历史消息的 `content` 有字符串/数组两种形状** —— 必须归一化，否则用户消息消失。
5. **SSE 必须手写解析**（`EventSource` 不支持 POST），且要处理流提前断开。
6. **`sessionKey` 必须 URL 编码**（含冒号）。
7. **信封不统一** —— 见 §2.2 的裸对象清单，不要假设都有包裹 key。
8. **新会话不要自己编 `session_id`** —— 用 `message_start` 返回的那个。
9. **`PUT /api/agent/config` 的 body 多一层 `config`**。
10. **404 可能是纯文本**（Go 的 `404 page not found`），解析前要容错。
11. **`/api/sessions` 与历史都要求实例是热的** —— 实例不可达时读不到历史，
    不要靠本地缓存假装可用。
12. **`ask_user` 的 `call_id` 与 `questions[].questionId` 不是一回事**。

---

# 附：相关实现位置

| 主题 | 位置 |
| --- | --- |
| 路由表 | `internal/server/server.go` (`Handler()`) |
| 对话与历史 | `internal/server/handlers.go` |
| 审批 | `internal/server/approvals.go` |
| 问答 | `internal/server/questions.go` |
| 停止 / 回合状态 | `internal/server/abort.go` |
| 任务 | `internal/server/handlers_tasks.go` |
| 平台对象 | `internal/server/handlers_platform.go` |
| 模型目录 | `internal/server/handlers_llms.go` |
| SSE 事件契约 | `internal/runtime/contracts.go` |
| 参考客户端 | `web/src/api/` |
