# 用 Bruno GUI 走一遍对话 / 审批 / 问答

三个流程共同的核心：**都是「两条连接」**。对话 SSE 流开着并**停住**等人工输入，
你在**另一条连接**上提交决定，第一条流随即恢复。

Bruno 里「第二条连接」就是**另一个标签页** —— 第一个标签的流还在，
切过去发另一个请求，再切回来。

---

## 准备

### 1. 开通道

```bash
kubectl -n <namespace> port-forward svc/cubepilot-api 8080:8080
```

### 2. Bruno 里选 `local` 环境

右上角下拉（**要先点开一个请求才会出现**）。不选也能跑 ——
`collection.bru` 有同名的兜底变量。

### 3. ⚠️ 确认审批策略是打开的

`1-read/03-agent-approval` 点一下，看 `approvalPolicy`。
**如果是空的（`""`），下面流程 B 永远不会停** —— 没有任何东西在拦。

打开它（`Allowlist` 是模板默认值）：

```
PUT {{baseUrl}}/api/v1/agent/approval
{"approvalPolicy": "Allowlist"}
```

期望回读到 `"channel": "up"`。`channel` 为空或 `unconfigured` 说明 HITL 通道没建立，
拦截会**失败关闭**（回合直接失败，而不是放行）。

> **升级过的安装注意**：如果上面这个 PUT 返回 200 但值没变，
> 说明集群里的 CRD 还是旧 schema（`confirmPolicy`），
> 新字段 `approvalPolicy` 被**静默剪掉**了。Helm 不升级 `crds/` 目录，
> 要手动补：
>
> ```bash
> kubectl apply -f deploy/charts/cubepilot-chart/crds/
> ```

---

## 流程 A：普通对话（只看流式）

**请求**：`3-chat/01-send-message` → Send

响应面板会**逐帧追加**（发送按钮变成 Cancel）。要看的：

- `message_delta` 是**追加**，`text_replace` 是**替换**
- `message_done` 是唯一终态

想看「被 `text_replace` 丢弃了什么」，把响应内容存成文件跑
`python3 scripts/decode-sse.py <file>`。

---

## 流程 B：写操作审批（approval）

### 触发

**标签 1**：`3-chat/01-send-message`，把 `content` 换成：

```
执行这条 shell 命令: mkdir -p /home/node/.openclaw/workspace/bruno-demo
```

Send 之后，流会**停在**：

```
event: approval_pending
data: {"type":"approval_pending","callId":"d71907a6-...","name":"exec",
       "command":"mkdir -p /home/node/.openclaw/workspace/bruno-demo","level":"write"}
```

**发送按钮仍是 Cancel 状态 —— 回合没结束，它在等你。**

> **选什么提示词很关键。** 两点：
> - 审批门控**只管 `exec` 工具**。让模型"创建文件"它会用自己的 `write` 工具，
>   那个**不经过审批**，流不会停。要触发就得让它跑 shell 命令。
> - 命令**要能成功**。用 `kubectl create namespace` 的话，你的用户身份没有那个权限
>   （`view` + ai.cubestack.io admin），模型会反复自查权限、绕圈。
>   `mkdir` 这类在 workspace 里的操作最干净。

### 提交决定

**开一个新标签**（⌘/Ctrl + T，或左侧请求列表点一下），发
`3-chat/06-submit-approval` → Send。

它用的 `{{sessionId}}` 是 `01-send-message` 的后置脚本自动写进环境变量的
—— 所以**不用手动填**，只要 01 跑过。

`decision` 三个取值：

| 值 | 效果 |
|---|---|
| `approve` | 本次放行，回合继续 |
| `reject` | 拒绝，写操作**不执行** |
| `allow-always` | 放行，**并把该命令记入实例 allowlist**，此后同类命令自动通过 |

### 看它恢复

**切回标签 1**，流上会继续出现：

```
approval_resolved → tool_result → … → message_done
```

### 两个容易困惑的点

- **一个回合可能停多次。** 模型干了 3 件事就停 3 次。用 `allow-always`
  一次过掉后续同类命令。`01-send-message` 的响应面板里数 `approval_pending`
  出现的次数就知道还要批几次。
- **`reject` 之后回合不会失败**，模型会换个做法继续，可能再次停。

### 补充：拒绝也能反证

拒绝后用 `kubectl get ns <name>`（或看目标路径）确认**确实没发生** ——
门控是 fail-closed 的，但我们这次没验成，因为模型改用了别的路径。

---

## 流程 C：`ask_user` 问答

### 触发

**标签 1**：`3-chat/01-send-message`，`content` 换成：

```
请调用 ask_user 工具问我一个问题,给我几个选项选择
```

> `ask_user` 是 OpenClaw 内置工具，**调不调完全由模型决定**。
> "我需要…但没想好" 这类提示不一定触发（实测本地这个模型就不触发）；
> 直接点名最可靠。

流停在：

```
event: question_pending
data: {"type":"question_pending","callId":"ask_3bba005e...",
       "question":{"questions":[{"questionId":"cluster_action",
         "header":"集群操作","question":"你想让我在集群上做什么?",
         "options":[{"label":"集群健康巡检 (Recommended)"}, ...]}]}}
```

### 拿到 id

**新标签** → `3-chat/08-question-pending` → Send。回读：

```json
{"questions":[{"id":"ask_3bba005e...","questions":[{"questionId":"cluster_action",...}]}]}
```

⚠️ **两个 id 不是一回事：**

| 字段 | 是什么 | 用在哪 |
|---|---|---|
| `questions[].id` | **问答会话 id**（= 事件里的 `callId`） | 提交时的 `id` |
| `questions[].questions[].questionId` | **每个问题的 id** | `answers` 的**键** |

### 提交答案

改 `3-chat/09-submit-question` 的三个环境变量
（`questionId` / `questionKey` / `questionAnswer`），或直接在 body 里改，然后 Send：

```json
{"id": "ask_3bba005e...", "answers": {"cluster_action": ["查看资源状态"]}}
```

值必须是**选项的 `label` 原文**。

**或者取消**（`3-chat/10-cancel-question`）：

```json
{"id": "ask_3bba005e...", "cancel": true}
```

不回答也不取消的话，**默认要等 900 秒**才会超时。

### 看它恢复

切回标签 1：

```
question_resolved {"message":"answered"} → … → message_done
```

---

## 三条流程的共同注意点

1. **必须用 canonical 形式的 sessionKey。**
   `{{sessionId}}` 被 `01-send-message` 的后置脚本设成服务端返回的
   `agent:main:conv-...`，是对的。但如果你**手填**短形式（`conv-...`），
   `/approval` 和 `/question` 会 **404**（`/turn`、`/abort` 却接受短形式 —— 契约不一致，
   [issue #180](https://github.com/suanova/cubepilot/issues/180)）。

2. **同一个会话同时只能有一个回合。** 主流还开着时再发一条 → **409**，
   不要重试；先 `/abort` 或等它结束。

3. **`message_done` 是唯一终态。** 流也可能提前断开（网络、重启）——
   那就自己判断回合结束了，别盯着响应面板干等。

4. **Bruno 不会替你重连。** 流断开后要看后续，重新发一次 `01-send-message`
   （用同一个 `sessionId`）。

5. **改 `.bru` 文件后 Bruno 可能不刷新**（集合在 WSL 的 UNC 路径上）。
   File → Reload Collection。
