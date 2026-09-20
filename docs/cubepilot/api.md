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
>
> **要改这个 API？** 先读 [api-conventions.md](./api-conventions.md)——命名来源、
> 形状规则、状态码与方法语义，以及每条规则由哪个测试守住。

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

CubePilot 有**两条**给客户端用的路径。先确定你的客户端属于哪一类，再决定读哪一节。

```text
                    ┌── 路径 A：REST facade ──┐
浏览器 / 客户端 ────┤                        ├──▶ cubepilot-api:8080
                    └── 路径 B：CRD-first ───┘        │  WebSocket
                              │                      ▼
                              │            每用户一个 OpenClaw 实例（Pod）
                              │                      │
                              └──────────────▶ kubectl → 集群
                            kube-apiserver
                        （ai.cubestack.io CRD）
```

## 路径 A：REST facade（`/api/v1/*`）

内置 Portal 走这条路。**只需要访问 `/api/v1/*`**，不需要 Kubernetes API server 的访问权限——
所有 CRD 资源的读写由 `cubepilot-api` 代理。

反向代理配置见下节。这是本文档的主体。

## 路径 B：CRD-first（直接对 kube-apiserver 操作）

有 kube-apiserver 权限的客户端（例如接入 CubePilot 的 cubeStack 统一 UI）**优先直接操作
六个平台 CRD**，只在 CRD 无法表达的操作上回退到 REST。

两组端点的划分：

| 组 | 内容 | 能否走 CRD |
|---|---|---|
| **REST-only** | 对话/会话历史、审批、问答、审计、技能内容发布 | ❌ 没有 CRD 承载（会话内容在实例运行时里、审计是 API 自己的 PVC 状态、技能包是 API 自己的仓库）|
| **CRD facade** | `AgentTemplate` / `AgentInstance` / `Skill` / `TaskTemplate` / `Task` / `TaskRun` 的 HTTP 镜像 | ✅ 同样可对 CR 直接操作 |

**CRD-first 客户端应当以 CRD 的字段名为准**——本文档中 REST 的字段名刻意与 CRD 的 json tag
保持一致（例如 `selectedModel`、`userInstructions`、`approvalPolicy`、`instruction`、`cron`），
这样同一个概念在两条路径上是同一个名字。

> CRD 是命名空间作用域的，组为 `ai.cubestack.io`，版本 `v1alpha1`。
> 数据面契约的完整记录见 GitHub issue #148（设计决策记在 issue 里，不在仓库文档里）。

## 版本

客户端端点统一在 **`/api/v1/`** 之下。版本被冻结在此处，将来若有破坏性变更会引入新的前缀
（`/api/v2/`），而不是就地改动 v1。

集群内部端点 `/internal/*` **不带版本**：它们唯一的消费者是 agent Pod 内的 supervisor，
随 agent 镜像与 API 一起发布。

## 反向代理的硬性要求

SSE（`POST /api/v1/messages`）必须**关闭代理缓冲**，否则事件会被攒着一起吐，失去流式效果。
参考 `web/nginx.conf`：

```nginx
location /api/v1/ {
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

## 2.2 响应形状

两条路径共用同一套规则——这也是 §1「以 CRD 字段名为准」在响应结构上的体现：

> **返回一个完整的「东西」→ 用具名 key 包起来，key 就是那个东西的名字。**
> **返回某个东西的若干字段 → 不加信封，字段直接摊平。**

**有信封**（key 命名 payload）：

| 端点 | 包裹 key |
| --- | --- |
| `/api/v1/sessions` | `sessions` |
| `/api/v1/audit` | `entries` |
| `/api/v1/agenttemplates` · `/api/v1/agenttemplates/{name}` | `agentTemplates` · `agentTemplate` |
| `/api/v1/instances` GET · POST | `instances` · `instance` |
| `/api/v1/llms` POST · `/api/v1/llms/{name}` PUT | `provider` |
| `/api/v1/llms/{name}` DELETE | `removed`（+ 可选 `warning`）|
| `/api/v1/skills` · `POST .../publish` | `skills` · `skill` |
| `/api/v1/skills/{name}/install` · `uninstall` | `enabledSkills` |
| `/api/v1/tasks` GET · POST · `/toggle` · `/run` | `tasks` · `task` · `task` · `{started, task}` |
| `/api/v1/tasks/{id}/reports` | `reports` |
| `/api/v1/tasktemplates` | `taskTemplates` |
| `/api/v1/taskruns` · `/api/v1/taskruns/{name}` | `taskruns` · `taskrun` |
| `/api/v1/kinds` | `kinds` |
| `/api/v1/sessions/{key}/question/pending` · `.../approval/pending` | `questions` · `approvals` |

**扁平**（是某个东西的字段，不是一个独立的东西）：

| 端点 | 说明 |
| --- | --- |
| `GET`·`PUT /api/v1/agent/config` | `{exists, selectedModel, userInstructions}` —— 是实例的两个字段，不是名为 config 的对象 |
| `GET /api/v1/agent/status` | 实例的状态字段 |
| `GET`·`PUT /api/v1/agent/approval` | 策略视图的字段 |
| `POST /api/v1/sessions/{key}/approval` | `{approved, decision, approvalId, allowlisted?}` —— 请求要带 `approvalId` |
| `POST /api/v1/sessions/{key}/question` | `{questionId, cancelled}` |
| `POST /api/v1/sessions/{key}/abort` · `GET .../turn` | `{ok}` · `{active}` |
| `DELETE /api/v1/sessions/{key}` | `{deleted, archived, worktreePreserved?}`，是这次删除的字段，不是一个叫 deleted 的对象 |
| `DELETE /api/v1/tasks/{id}` | `{deleted}` |

**字节透传**（不包——包一层就等于篡改别人的格式）：

| 端点 | 说明 |
| --- | --- |
| `GET /api/v1/sessions/{key}/messages` | 运行时历史文档，原样转发，不重新编码 |
| `POST /api/v1/messages` | SSE 流，不是 JSON |
| `GET /api/v1/sessions/{key}/stream` | SSE 流，不是 JSON（观察别人发起的回合） |
| `GET /internal/gateway/config/{user}` | 原样转发 `openclaw.json` |
| `GET /internal/skills/{name}/tar` | 原始 gzip |

## 2.3 错误与状态码语义

**所有**错误响应都包含 `error`：

```json
{ "error": "人类可读的原因" }
```

不存在第二种形态——未匹配的路径也由 mux 兜底返回 JSON，不会出现 Go 默认的纯文本 404。
客户端可以无条件地按 JSON 解析错误体。

错误体是**可扩展**的：个别端点会在 `error` 之外附带结构化字段，客户端应当容忍未知键。
目前只有一处：删掉正被选用的模型时——`DELETE` 整个 provider，或 `PUT` 把它服务的某个 id
从列表里去掉——`409` 额外带一个 `instances` 数组（见 §6.3）。

以下状态码**有特定语义**，客户端必须区别处理：

| 码 | 含义 | 客户端应当 |
| --- | --- | --- |
| **503** | `instance warming failed: ...` —— 实例正在冷启动（Pod 未就绪 / 网关未监听） | **等待并重试**，提示「正在启动实例」。这不是故障 |
| **503** | `CRD path disabled` —— 部署未启用 CRD 路径 | 视为部署配置问题，不要重试 |
| **409** | `another turn is already streaming for this session` | 同一会话已有回合在跑。**不要重试发送**，提示等待或先调 `/abort` |
| **404** | `no pending approval` / `no pending question` | 正常的「已过期 / 无未决项」，**静默忽略** |
| **502** | 网关往返失败 | 后端到实例的链路问题，可重试一次 |
| **504** | `the run did not settle in time; try again`（`/abort`）· 删除会话的两种超时（`DELETE /api/v1/sessions/{key}`）：`the session delete did not finish in time; retrying it is safe and idempotent`，以及 `the conversation was deleted, but the session's turn did not release in time; retry (the delete is idempotent)`——后者会话**已经删掉** | 重试 |
| **413** | 仅技能发布，tar 超过 10 MiB | 换更小的包 |
| **201** | 创建成功：`POST /api/v1/instances`、`POST /api/v1/tasks`、`POST /api/v1/llms`、`POST .../publish` | 正常成功。注意它**不是** 200 |
| **200** | `POST /api/v1/instances` 在实例已存在时返回 200 + `alreadyExists: true` | 正常成功（幂等重复）|
| **202** | 仅 `POST /api/v1/tasks/{id}/run`，表示已登记手动触发 | 正常成功 |

## 2.4 方法语义

每个端点都只接受它声明的方法，其他方法一律 `405` + JSON。

两点需要留意，它们和直觉不同：

| 端点 | 方法 | 为什么 |
| --- | --- | --- |
| `/api/v1/skills/{name}/install` · `uninstall` | **`PUT`** | 这是幂等的集合成员变更（重复调用结果一致），所以用 PUT 而非 POST |
| `/api/v1/tasks/{id}/run` · `/toggle` | `POST` | 非幂等（触发一次执行 / 翻转状态），POST 正确 |
| `POST /api/v1/sessions/{key}/approval` | `POST` | 提交一个决定，是动作 |

## 2.5 路径细节

- `sessionKey` 含冒号（形如 `agent:main:conv-<uuid>`），**必须 URL 编码**。
- 会话子资源靠**后缀**匹配，所以 `/api/v1/sessions/a/b/messages` 也命中，且 `sessionKey` 取 `a/b`。
- 通配路由带尾斜杠会落到兜底 404（如 `/api/v1/llms/`），不会匹配 `{name}`。
- `/internal/*` 不带版本前缀（见 §1）。

---

# 3. 调用顺序

## 3.1 关键前提：只有 3 个端点会「加热」实例

CubePilot 的 agent 实例是**常驻**的（起来后不回收），但第一次访问要**冷启动**一个 Pod，
可能耗时数十秒。只有下面 3 个端点会触发这个过程（它们调用 `mgr.Ensure`）：

| 会加热（可能慢、可能 503） |
| --- |
| `GET /api/v1/sessions` |
| `GET /api/v1/sessions/{key}/messages` |
| `POST /api/v1/messages` |

**其余所有端点都不加热**——它们直接读 CR 或网关，永远快。
调用顺序就建立在这条分界上。

## 3.2 推荐的启动序列

```ts
// ── 阶段 0：并行发出，不碰实例，首屏立刻可渲染 ──────────────
const [status, config, approval] = await Promise.all([
  api.agentStatus(),     // 实例存在吗？phase 是什么？
  api.agentConfig(),     // 我选的模型 / 提示词
  api.agentConfirm(),    // 我的审批策略
])

if (!status.exists) {
  // 还没实例 → 引导用户去「Agent 配置」页，调 POST /api/v1/instances
  // 此时不要调 /api/v1/sessions，只会 503
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
GET  /api/v1/agenttemplates          → 挑模板（默认 cubepilot）
POST /api/v1/instances               → 创建实例（201；已存在则 200 + alreadyExists）
       ↑ 此后 /api/v1/sessions 等端点才可用
```

创建后实例仍需控制器调度，`GET /api/v1/agent/status` 的 `phase` 会经历
`Creating` → `Ready`。`phase` 为 `not provisioned (resident policy)` 表示实例不存在。

---

# 4. 对话流程

## 4.1 发送一条消息（SSE）

```ts
const resp = await fetch('/api/v1/messages', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'X-CubePilot-User': user,
  },
  body: JSON.stringify({
    sessionId: currentSessionId,  // 新会话传 null / '' / 省略
    content: text,                 // 必填，空白会被拒
  }),
})
```

请求体：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `content` | 是 | 用户消息。空或纯空白 → `400 {"error":"content required"}` |
| `sessionId` | 否 | 省略或空 → 后端生成 `conv-<uuid>` |

**新会话不要自己编 key。** 后端会生成并**规范化**为 `agent:main:<key>`，
然后通过第一个事件告知：

```text
event: message_start
data: {"type":"message_start","sessionId":"agent:main:conv-9f3a..."}
```

**必须保存这个 `sessionId`**，后续所有请求都用它（它就是 `{key}`）。

响应头：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`。
空闲 15 秒会收到注释行 `: ping` 保活（客户端应忽略非 `data:` 行）。

**请求阶段的失败不走 HTTP 状态码**（流已经打开了），而是以 SSE 事件返回：

```json
{"type":"message_done","sessionId":"...","error":"instance warming failed: ..."}
```

## 4.2 SSE 帧格式

每帧一个 `event:` 行加一个 `data:` 行，空行结束：

```text
event: message_delta
data: {"type":"message_delta","sessionId":"agent:main:conv-x","delta":"集群里有"}

```

`event:` 名与 `data` 里的 `type` 字段始终一致。全部字段见 §7。

**客户端解析要求**：
- `EventSource` 只支持 GET，此处是 POST，**必须用 `fetch` + `response.body.getReader()` 手写解析**。
- 流可能在 `message_done` 之前断掉（网络抖动、服务重启）。
  **此时要合成一个终止事件**让 UI 复位，否则界面永久卡在「进行中」。

参考实现：`web/src/api/sse.ts`。

## 4.3 会话列表与历史

```ts
GET /api/v1/sessions                        → {"sessions":[{"sessionKey","title"}]}
GET /api/v1/sessions/{key}/messages
                                         → {"items":[HistoryMessage]}   // 原始透传
```

两者**都要求实例是热的**——平台不存消息副本，会话内容的唯一真相源是实例自身的运行时。
实例不可达时**读不到历史**（是失败，不是「读到旧数据」）。

但「会话还不存在」不是失败：会话由它的第一条消息创建，在那之前读历史是**正常的空**。
网关回答 404，后端把它原样透传成 **404 `{"error":"no such session"}`**，不再压成 502。

| 状态 | 含义 | 客户端应当 |
| --- | --- | --- |
| **404** | 这个会话还没开始 | 渲染为空会话，**不要**报错——否则网关故障和全新会话在用户眼里是同一件事：「我的历史被清空了」 |
| **502** | 后端到实例的链路失败 | 报错，可重试 |

**历史消息的 `content` 有两种形状**，必须归一化：

| `role` | `content` 形状 |
| --- | --- |
| `user` | **字符串** |
| `assistant` / `toolResult` | **内容块数组**：`[{"type":"text"\|"toolCall","text"?,"name"?,"id"?,"arguments"?}]` |

把字符串当数组遍历会逐字符拆开，用户消息会整条消失。参考 `web/src/views/chat/useChatThread.ts` 的 `blocks()`。

## 4.4 停止进行中的回合

```ts
POST /api/v1/sessions/{key}/abort   → {"ok":true}
```

- **幂等**：没有运行中的回合时直接返回 200，不发任何网关 RPC。
- 最长等待约 5 秒让回合落定；超时 → `504 {"error":"the run did not settle in time; try again"}`。
- 不加热实例。
- 被停止的回合以 `message_done` + `"stopped": true` 结束（注意：`stopped` 与 `error` 互斥）。

## 4.5 查询回合状态

```ts
GET /api/v1/sessions/{key}/turn   → {"active":true|false}
```

状态来自网关（不是本地 hub），响应带 `Cache-Control: no-store`。
可用于页面加载时判断「刷新前那轮还在跑吗」。

## 4.6 重连到已暂停的回合（re-attach）

```ts
GET /api/v1/sessions/{key}/stream   → SSE 流
```

页面刷新、关标签或连接断开会让 `POST /api/v1/messages` 那条流消失，但**网关侧的回合还在跑**。
若它正停在一个人工决定上，卡片可以从 pending 端点恢复——可恢复出来的卡片答完之后，
续写的输出已经没有流可以送达（写回答的那个标签页只会看到卡片被 settle）。
这个端点补上那一段：它观察一个**不是自己发起**的回合，把续写送到回答卡片的那一页。

- **门禁**：会话必须停在一个人工决定上（有待回答的问题，或有待处理的审批），否则
  `404 {"error":"no parked turn for this session"}`。这同时保证重连不漏事件：回合正在等人，
  它不产出任何东西。
- **一个会话只有一条流**：已有流时 `409`——持流的那一页本来就收得到全部事件，重连的这页应当让开。
- 未配置 HITL → `503 {"error":"question channel unavailable"}`。
- 结束方式：回合到终态 → 以 `message_done` 结束。**本流没有观察到这个回合的终态**时——订阅没能建立、
  门禁通过之后决定在订阅就绪前被答掉、或到达 1 小时上限——流会**不带终止事件**地结束（客户端断开时
  自然也没有事件可发）。后者是刻意的：卡片上那个人工决定可能仍然是卡住这个回合的唯一出口，而客户端
  会把服务端的终止事件当作回合真的结束去 settle 卡片，用户就再也答不上去了。不带终止事件正是
  「本流什么都没观察到」的交代——客户端按常规自己合成 `message_done`（attach 路径不会把它应用到卡片上），
  再去 history 把这段补齐。
- 不加热实例。

## 4.7 清空会话

```ts
DELETE /api/v1/sessions/{key}   → {"deleted":true,"archived":[]}
```

删除这个会话**连同它的对话记录**，于是下一次用同一个 key 发消息就是一个全新的会话。
给「每个用户一个固定 key」的客户端用的：它没有别的 key 可换。

- **不需要先 `/abort`**：网关在删除流程里自己把活跃的工作停下来并等它落定。
- **幂等**：key 不存在不是错误，返回 `200` + `deleted:false`。固定 key 的客户端每次按「清空」
  都会撞上这种情况，这就是它要的答案。
- **`{key}` 是 `/api/v1/sessions/` 之后的全部内容**，即使它以某个子资源后缀（`/messages`、`/turn`…）
  结尾——没有任何子资源接受 DELETE，所以 DELETE 永远指的是路径所命名的那个会话。给会话起名时
  不必绕开这些后缀。
- 不加热实例。请求没有 body。
- 网关调用有 **30 秒**上限。这个数是从网关的行为推出来的，不是随手取的：删除之前网关会先把会话里
  活跃的工作停下来，而它给这个排空的上限是 15 秒，删除和清理工作树都在这之后、同一次调用里；上限
  低于排空就会把一次正常的删除报成超时。超时返回 `504`，删除可能已经生效，重试一次即可（幂等）。
  客户端断开不会取消这次删除（调用与请求解绑），所以按下「清空」后再离开页面，会话照样会被清空。
- **删除成功之后才回答**：API 还会等这个会话的 SSE 流释放（有上限）再返回 `200`，所以紧接着用同一个
  key 发下一轮不会撞上 `409 another turn is already streaming`。这一步等不到时返回 `504`——这时
  会话**已经删掉了**，按幂等重试即可。
- **400**：网关拒绝这个请求本身（例如受保护的 `agent:main:main`、模型选择被锁定的会话），
  重试无用。
- **409**：会话仍在活跃，或者在你读取之后发生了变化。这不是调用方的错，也不用先 abort，
  等这一轮结束再重试一次即可。
- **502**：网关往返失败。其中 `FORBIDDEN` 一类是**平台自己的问题**（配对的设备没有被授予
  `operator.admin`），重试无用。

**「清空」不保证「这个实例看起来像从没聊过天」**：运行时可能保留会话的周边产物，响应把
保留了什么如实说出来：

| 字段 | 含义 |
| --- | --- |
| `deleted` | 这次调用是否真的删掉了东西（`false` 表示本来就没有，仍然是成功）|
| `archived` | 被归档的项；为空时是 `[]`，不是 `null` |
| `worktreePreserved` | 仅在**真的**留下了一个工作树时出现：`{id,branch,path,reason}` |

客户端不要把「记录没了」当成「实例干净了」：清空只重置本地的对话视图，不要假设运行时上的
残留也一并消失了。

---

# 5. 人机协同（HITL）

两条独立的通道，都复用**同一条对话 SSE 流**：审批（写操作放行）和问答（`ask_user`）。
共同模式是「事件弹出 → 用户决定 → POST 回答 → 收到 resolved 事件」。

## 5.1 写操作审批（approval）

当 agent 要执行被策略拦截的写操作时，回合在网关侧暂停：

```text
event: approval_pending
data: {"type":"approval_pending","sessionId":"...","callId":"<approval id>",
       "name":"exec","command":"kubectl delete pod x","level":"write","message":"...",
       "createdAtMs":1758355200000,"expiresAtMs":1758357000000}
```

**一个会话可以同时压着多条审批**（一轮里并发多条被拦的命令）。每条各有自己的
`callId`，按 `createdAtMs` 从旧到新排列；决定必须**指名**要结算哪一条。

用户决定后提交：

```ts
POST /api/v1/sessions/{key}/approval
body: {"approvalId": "<approval id>", "decision": "approve" | "reject" | "allow-always"}
```

`approvalId` 必填：一个会话可能同时有多条未决审批，不指名就没法知道要结算哪一条，
所以缺 id 一律 `400`，不会替你挑「最新那条」。

| decision | 效果 |
| --- | --- |
| `approve` | 本次放行，回合继续 |
| `reject` | 拒绝，写操作不执行 |
| `allow-always` | 本次放行，**并把该命令记为当前用户的 learned 授权**（进 grants store，**不写实例 spec**），此后自动通过 |

响应：`{"approved":bool,"decision":"...","approvalId":"...","allowlisted"?:bool}` ——
回带的 `approvalId` 就是被结算的那条，客户端据此核对「点的那张卡确实被结算了」。

`404` 表示该审批已不在网关（过期、被别处结算、或不属于这个会话）；
`409` 表示同一张卡的另一条决定正在飞行中，或已被别处结算 —— 两者都意味着卡片可以关掉，
不是请求失败。

随后同一流上收到：

```text
event: approval_resolved
data: {"type":"approval_resolved","sessionId":"...","callId":"...","approved":true}
```

**刷新后恢复卡片**（必需，不是可选）：

```ts
GET /api/v1/sessions/{key}/approval/pending
// 404 {"error":"no pending approval"} → 静默忽略
// 200：{"approvals":[{"sessionId","approvalId","tool","command","level","message",
//                     "createdAtMs","expiresAtMs"}]}
// 其它错误码（如 502）→ 读不到网关，**不能**当成「没有未决项」
```

**失败关闭语义**：`approvalPolicy` 要求「问」时，若审批通道不可用，回合会**直接失败**而不是静默放行。
未知的策略值同样失败关闭——绝不降级成「不拦」。
因此 `503 {"error":"approval channel unavailable"}` 表示该回合被拒——不要重试成「跳过确认」。

## 5.2 `ask_user` 问答

agent 调用 `ask_user` 工具时，回合同样暂停：

```text
event: question_pending
data: {"type":"question_pending","sessionId":"...","callId":"<question id>",
       "question":{"questions":[{"questionId","header","question",
                                 "options":[{"label","description"?}],"multiSelect"?,"isOther":true}],
                   "timeoutSeconds":n}}
```

**注意 `callId` 是问答会话的 id，`question.questions[].questionId` 是每个问题的 id**，
提交答案时用的是后者。

`isOther` 是 `omitempty` 字段：`ask_user` 的每个问题都会带 `isOther: true`（表示该问题在选项之外
还接受人类自己的文本），但别的生产者（例如 `question.request`）可能不带，客户端要按缺省处理。
带这个标记的问题，Portal 会在选项下方渲染一个输入框；`options` 为空的问题是**纯自由文本**，只渲染
输入框；两种形式都由同一条 `question_pending` 下发，不需要客户端分支。

```ts
// 回答：选项 label
POST /api/v1/sessions/{key}/question
body: {"id": "<call_id>", "answers": {"<questionId>": ["选项 label"]}}

// 或回答人类自己的文本（`isOther` 或 `options` 为空的问题）
POST /api/v1/sessions/{key}/question
body: {"id": "<call_id>", "answers": {"<questionId>": ["<自由文本>"]}}

// 或取消（让 agent 继续而不是等到超时）
POST /api/v1/sessions/{key}/question
body: {"id": "<call_id>", "cancel": true}
```

服务端**原样转发**、不与选项做校验（它不知道网关的答案语义）。网关侧的约束：非多选问题只接受
**一个**答案（给多个 → `400 does not allow multiple answers`），多选问题可以给多个；Portal 的卡片
一律按"二选一"提交，不混用 label 与自由文本。

`answers` 与 `cancel` **必须二选一**，同时给或都不给 → `400 {"error":"send either answers or cancel"}`。

响应：`{"questionId":"...","cancelled":bool}`

```text
event: question_resolved
data: {"type":"question_resolved","sessionId":"...","callId":"...",
       "message":"answered" | "cancelled" | "expired"}
```

**刷新后恢复**：

```ts
GET /api/v1/sessions/{key}/question/pending
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
| GET | `/api/v1/sessions` | — | `{"sessions":[{"sessionKey","title"}]}` | 是 |
| GET | `/api/v1/sessions/{key}/messages` | — | 原始历史 JSON（`{"items":[...]}`） | 是 |
| POST | `/api/v1/messages` | `{"sessionId"?,"content"}` | **SSE 流** | 是 |
| POST | `/api/v1/sessions/{key}/approval` | `{"approvalId","decision"}` | `{"approved","decision","approvalId","allowlisted"?}` | 否 |
| GET | `/api/v1/sessions/{key}/approval/pending` | — | `{"approvals":[{"sessionId","approvalId","tool","command","level","message","createdAtMs","expiresAtMs"}]}` | 否 |
| POST | `/api/v1/sessions/{key}/question` | `{"id","answers":{qid:[label\|text]}\|"cancel"}` | `{"questionId","cancelled"}` | 否 |
| GET | `/api/v1/sessions/{key}/question/pending` | — | `{"questions":[...]}` | 否 |
| POST | `/api/v1/sessions/{key}/abort` | — | `{"ok":true}` | 否 |
| GET | `/api/v1/sessions/{key}/turn` | — | `{"active":bool}` | 否 |
| GET | `/api/v1/sessions/{key}/stream` | — | **SSE 流**（观察别人发起的、停在人工决定上的回合） | 否 |
| DELETE | `/api/v1/sessions/{key}` | — | `{"deleted":bool,"archived":[...],"worktreePreserved"?}` | 否 |

## 6.2 Agent 配置与实例

| 方法 | 路径 | 请求 | 响应 | 加热 |
| --- | --- | --- | --- | --- |
| GET | `/api/v1/agent/config` | — | `{"exists","selectedModel","userInstructions"}`（扁平） | 否 |
| PUT | `/api/v1/agent/config` | `{"selectedModel","userInstructions"}` | 同上 | 否 |
| GET | `/api/v1/agent/status` | — | **裸** `{"user","id","exists","phase","gatewayImage","gatewayPort",...}` | 否 |
| GET | `/api/v1/agent/approval` | — | **裸** `approvalView`，见下 | 否 |
| PUT | `/api/v1/agent/approval` | `{"approvalPolicy","allowlist":[...],"revokeGrants"?}` | **裸** `approvalView` | 否 |
| GET | `/api/v1/instances` | — | `{"instances":[...]}` | 否 |
| POST | `/api/v1/instances` | `{"templateRef","selectedModel","enabledSkills","userInstructions"}` | `201 {"instance":{...}}` | 否 |
| GET | `/api/v1/agenttemplates` | — | `{"agentTemplates":[...]}` | 否 |
| GET | `/api/v1/agenttemplates/{name}` | — | `{"agentTemplate":{...}}` | 否 |

字段名与 CRD 一致：`selectedModel` ↔ `AgentInstance.spec.selectedModel`，
`userInstructions` ↔ `spec.userInstructions`。

`approvalView`（裸对象）：

```json
{
  "exists": true,
  "approvalPolicy": "None | Allowlist | AlwaysAsk | \"\"",
  "override": "",                       // 实例自身设定，"" = 继承模板
  "templatePolicy": "Allowlist",
  "allowlist":     [{"pattern","argPattern?","label"?,"source"?,"command"?}],
  "allowlistOwned":[{"pattern","argPattern?","label"?,"source":"user"}],
  "allowlistLearned":[{"pattern","argPattern?","label"?,"source":"learned","command"?}],
  "channel": "up | pairing | down | unconfigured | \"\""
}
```

- `approvalPolicy` 只接受 `""` / `None` / `Allowlist` / `AlwaysAsk`，其他值 → 400。
- `label` **只在**规则精确匹配平台内置只读规则时出现；用户自己加的规则没有 `label`，
  界面上不要把它当作只读展示。
- `allowlist` 每条规则带 `source`，说明它从哪来：`builtin | template | user | learned`
  —— 分别是平台内置、模板、实例自己加的和聊天里 allow-always 学到的。界面按来源分组，
  不要再用「是不是自己的」这种标志去猜来源。
- `allowlistOwned` 和 `allowlistLearned` 的条目**也**带 `source`，分别是 `user` 和 `learned`
  ——每个分组列表只装自己那一类，`source` 只是让三个列表的条目形状统一，前端不必再记
  「哪个列表对应哪个来源」。
- `allowlistLearned` 是**可选**字段，列出 learned 授权，非空时才出现——为空即缺省，
  规则同 `allowlist` / `allowlistOwned`。
- **learned 条目**（不管出现在 `allowlist` 还是 `allowlistLearned`）额外带 `command`：
  用户当时批准的那条命令。learned 分组应该显示它，而不是派生的正则（`pattern` 加上转义过的
  `argPattern`，人认不出来）。其他来源的条目没有这个字段。
- `channel` 为 `""` 表示策略是 `None` 或实例不存在；`unconfigured` 表示策略要求拦截但
  HITL 通道未配置（此时拦截会失败关闭）。
- `allowlist` / `allowlistOwned` / `allowlistLearned` 为空时字段**缺省**（不是 `[]`）。

**Agent 配置的常见错误**：

- `400 model "x" is not served by any provider of the cubepilot template (add it under Agent Config -> LLM Config first)`
  —— 模型没进模板的 `spec.providers`（要的是 `<provider>/<modelId>` ref）；空 `selectedModel` 永远允许（表示「用运行时默认」）。
- `400 pattern is required` —— `allowlist[]` 或 `revokeGrants[]` 里的 `pattern` 为空
  （或只有空白）。以前这种条目被静默丢弃，现在整次 PUT 被拒：规则不会写进实例，也不会下发给
  网关。同一条校验还拒绝含 `|` 的 `pattern`（错误文是 `pattern must not contain '|'`）：
  `pattern` 是命令名，而 `|` 是规则身份 `pattern|argPattern` 的分隔符，带上它会让两条不同的
  规则撞成同一个 key；`argPattern` 里的 `|` 是正则的或运算，不受影响。
- `400 argPattern is not a valid regular expression: <detail>` —— `allowlist[]` 或
  `revokeGrants[]` 里的 `argPattern` 不是合法正则，整次 PUT 被拒：规则**不会**写进实例，
  也不会下发给网关（网关按 JavaScript `RegExp` 匹配，写入时用 Go 正则先行校验）。
- `400 argPattern uses <construct>, which JavaScript's new RegExp does not accept: ...` ——
  同上拒法，但原因是该写法 Go 正则接受、JavaScript `RegExp` 不接受或读法不同：内联 flag
  如 `(?i)`、命名组写成 `(?P<name>)`（JavaScript 是 `(?<name>)`）、POSIX 字符类
  `[[:alpha:]]`、以及未加 `u` 标志的 `\p{L}`。写入时按这份清单先行拒绝。
- `409 no agent instance yet — provision it on the Agent Config page first`
  —— 实例不存在，先去创建。

## 6.3 模型目录（LLM）

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| POST | `/api/v1/llms` | `{"name","endpoint","models","apiKey"?,"public"?}` | **201** `{"provider":{...}}` |
| PUT | `/api/v1/llms/{name}` | 同上（`apiKey` 省略 = 保留原凭证） | `200 {"provider":{...},"warning"?}` |
| DELETE | `/api/v1/llms/{name}` | — | `200 {"removed":"<name>","warning"?}` |

- 一个 **provider** = 一份 endpoint + 一份凭证 + 它服务的若干 model id。所以「一个有多个 id 的
  网关」写一条记录，而不是每个 id 一条。
- `name` 是**provider 名**：DNS-1123 label（小写字母数字和 `-`，≤63 字符），不可变——
  改名要删了重建。它同时是网关 provider key、每个模型 ref 的前缀（`<name>/<modelId>`）
  和本 API 建的凭据 Secret 名后缀（`llm-<name>`），与 `models` 里的 id **无关**。
- `models` 是**后端模型 id 列表**，至少一个：id 按原样发给 endpoint，可含 `/`
  （如 OpenRouter 的 `anthropic/claude-sonnet-4.5`），但不能含空白、不以 `/` 开头或结尾、
  不含 `//`，也不能是 `*`（allowlist 通配符保留字）。服务端会 trim 并去重；
  空或缺失 → `400`（provider 没有 id 就什么都渲染不出来，也选不中）。
  「按原样」有一个例外：provider 名与 OpenClaw 内置 provider key 同名时会继承该内置 provider
  的 model id 归一化，发给 endpoint 的 id 可能被改写。见
  [cubepilot-design.md](./cubepilot-design.md) §3.3。
- `PUT` **整体替换** endpoint、凭证和 `models`：增删单个 id 就是同一次 PUT 带上全量列表
  （PUT 不带 `models` 不是「保持不变」，是 `400`）。
- 凭据按 provider 建**一次**（`llm-<name>`），不是每个 id 一个。
- `apiKey` 与 `public` **互斥**：公开 provider 不能带凭证；非公开 provider 必须给 key。
- `PUT` 时省略 `apiKey` = 保留已存凭证；`public:true` 会清掉凭证并删掉该 Secret。
- `DELETE` 删掉整个 provider 以及它服务的**所有** id；`PUT` 则删掉新列表中不再出现的 id
  （整体替换的必然结果）。两种删法都受同一条规则约束：只要被删的 id **其中任何一个**正被实例选用
  → `409`，错误体会**额外带一个 `instances` 数组**：

```json
{"error":"provider \"x\" serves a model selected by alice, bob; select another model there first",
 "instances":[{"name":"...","owner":"alice"}]}
```

  之所以拒绝而不是放行：`selectedModel` 是 fail-closed 的，那个用户下一回合会直接报
  `model "..." is not available in template ...`，而不是回退到默认模型。`PUT` 只对被**本次编辑
  真正删掉**的 ref 生效——保留的 id、新增的 id 都不算；被删的 ref 若**同一模板的别的 provider
  还在服务**（删掉后仍然可达），其用户照常解析，也不算，不会拒绝。
- `DELETE` 只删本 API 为这个 provider 命名的那个 Secret（`llm-<name>`）。若 `credentialRef`
  指向别的 Secret（手工改过 CR、多个 provider 共用一个凭据），该 Secret **会被保留**并在响应的
  `warning` 里说明。删单个 id 走 `PUT`，永远不会动 Secret。

这是**唯一**要求客户端解析结构化错误体的地方（Portal 会把它渲染成可点击的实例列表）。

## 6.4 任务与技能

| 方法 | 路径 | 请求 | 响应 |
| --- | --- | --- | --- |
| GET | `/api/v1/tasks` | — | `{"tasks":[taskDTO]}` |
| POST | `/api/v1/tasks` | `{"name","instruction"?,"cron"?,"templateRef"?,"params"?,"state"?}` | **201** `{"task":taskDTO}` |
| DELETE | `/api/v1/tasks/{id}` | — | `{"deleted":"<id>"}` |
| POST | `/api/v1/tasks/{id}/run` | — | **202** `{"started":true,"task":taskDTO}` |
| POST | `/api/v1/tasks/{id}/toggle` | — | `{"task":taskDTO}` |
| GET | `/api/v1/tasks/{id}/reports` | — | `{"reports":[reportDTO]}` |
| GET | `/api/v1/tasktemplates` | — | `{"taskTemplates":[...]}` |
| GET | `/api/v1/taskruns` | `?task=<name>` 可选 | `{"taskruns":[...]}` |
| GET | `/api/v1/taskruns/{name}` | — | `{"taskrun":{...}}` |
| GET | `/api/v1/audit` | `?limit=`（默认 400） | `{"entries":[...]}` |
| GET | `/api/v1/kinds` | — | `{"kinds":[...]}` |
| GET | `/api/v1/skills` | — | `{"skills":[...]}` |
| POST | `/api/v1/skills/{name}/publish` | query `displayName`（必填）、`description`；body = gzip tar | **201** `{"skill":{...}}` |
| PUT | `/api/v1/skills/{name}/install` | — | `{"enabledSkills":[...]}` |
| PUT | `/api/v1/skills/{name}/uninstall` | — | `{"enabledSkills":[...]}` |

`POST /api/v1/tasks` 的字段规则：

- `name` 必填；`instruction` 与 `templateRef` **至少一个**。
- `cron` 是**指针语义**：省略 ⇒ 用模板的 `defaultCron`（或 Manual）；显式 `""` ⇒ Manual；
  给值 ⇒ 按 5 字段 cron 解析（按 UTC 求值，非法 → 400）。
- `params` 必须配合 `templateRef`，否则 400。
- `state` 为 `Enabled` / `Paused`。

`taskDTO` 字段：`id`（CR 名）、`name`（显示名）、`instruction`、`cron`、`templateRef?`、
`state`（`Enabled` / `Paused`）、`creator`、`createdAt`、`lastRunAt?`、`lastStatus?`、`lastRunId?`、`nextRunAt?`。

`state` 是**唯一**的启停字段——没有 `enabled` 布尔（两个字段表达同一事实会产生不一致状态）。

`reportDTO` 字段：`id`、`taskId`、`taskName`、`trigger`（`Manual|Cron`）、
`status`（`success|failed|running`）、`startedAt`、`finishedAt`、`content`。
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

`POST /api/v1/messages` 的全部事件（`event:` 名与 `data.type` 一致）。
除 `type` 外所有字段都是 `omitempty`——**不出现即缺席**。

| `type` | 载荷字段 | 含义 |
| --- | --- | --- |
| `message_start` | `sessionId` | 回合开始，**保存 `sessionId`** |
| `agent_thinking` | `sessionId` | agent 正在思考 |
| `message_delta` | `sessionId`,`delta` | 追加正文（增量） |
| `text_replace` | `sessionId`,`delta` | **替换**正文（不是追加） |
| `narration` | `sessionId`,`blockId`,`text` | agent 的工具间解说，`text` 是**整段快照** |
| `tool_call` | `sessionId`,`name`,`callId`,`arguments` | 一次工具调用开始 |
| `tool_result` | `sessionId`,`name`,`callId`,`output` | 该工具的输出 |
| `approval_pending` | `sessionId`,`callId`,`name`,`command`,`level`,`message` | 写操作待审批 |
| `approval_resolved` | `sessionId`,`callId`,`approved` | 确认已提交 |
| `question_pending` | `sessionId`,`callId`,`question` | 问答待回答（结构见 §5.2） |
| `question_resolved` | `sessionId`,`callId`,`message` | `answered`/`cancelled`/`expired` |
| `message_done` | `sessionId`,`error?`,`stopped?` | **唯一的终止事件** |

要点：

- **`message_done` 是唯一终止信号**，且**必须处理**：流可能提前断开，
  客户端要自行合成一个 `message_done` 复位 UI。`error` 与 `stopped:true` 互斥。
- **`text_replace` 必须替换而非追加**。网关会在工具执行后重写先前的解说文本，
  当成 `message_delta` 追加会出现重复内容。
- **`narration` 是整段快照，且按 `blockId` 归并**。它承载 agent 在工具之间说的话
  （"发现了什么、接下来做什么"），`text` 是该段的**完整文本**而非增量：同一个
  `blockId` 再来一版就**替换**这一段，`blockId` 变了才是新的一段。它**不是答复**——
  答复仍然只走 `message_delta` / `text_replace`，两者不要混进同一个字段。
- `tool_result` 与 `tool_call` 通过 `callId` 配对；没有 `callId` 时按**到达顺序**
  与最旧的未完成调用配对（`web/src/views/ChatView.tsx` 的 `attachToolResult`）。
- 事件可能来自**其他连接**（审批/问答由网关侧广播注入），
  所以「收到 `approval_pending` 时不一定正好在你自己那次请求的处理路径上」。
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
7. **先判断信封再解包** —— 端点返回的是「一个东西」还是「若干字段」决定了要不要往下走一层，
   见 §2.2。取错层不会报错，只会得到 `undefined`（界面显示为空，后端其实有数据）。
8. **新会话不要自己编 `sessionId`** —— 用 `message_start` 返回的那个。
9. **创建成功是 `201` 不是 `200`** —— 只判断 `resp.ok` 就没问题；写死 `=== 200` 会误判为失败。
10. **安装/卸载技能用 `PUT`** —— 幂等的集合成员变更，不是 POST。
11. **`/api/v1/sessions` 与历史都要求实例是热的** —— 实例不可达时读不到历史，
    不要靠本地缓存假装可用。
12. **`ask_user` 的 `callId` 与 `questions[].questionId` 不是一回事**。
13. **任务是 `state` 而不是 `enabled`** —— 没有布尔字段，用 `state === 'Enabled'` 判断。

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
