# CubePilot 前端 API 文档（k8s API server 直连版 · 按设计整理）

> 依据 [cubepilot-design.md](./cubepilot-design.md)（简化设计）整理。
>
> 架构原则（项目负责人确认）：**有 CRD 的资源，前端直接访问 Kubernetes API server；
> 没有 CRD、走不了 k8s API server 的能力，仍走 cubepilot-api（`/api/*`）。**
> 不单独为 CRD 资源再开一层 REST facade。
>
> 本文档 = 前端需要对接的**全部** API，分两大部分：
> - [第 2~3 章](#2-k8s-api-server-访问通用约定)：CRD 资源 → k8s API server；
> - [第 4 章](#4-仍在-cubepilot-api-的接口非-crd)：非 CRD 能力 → cubepilot-api。
>
> **字段来源标注**：
> - 📘 **设计原文** —— 直接来自设计文档（group、kind、spec/status 字段、事件名）。
> - 🛠 **拟定契约** —— 设计给出意图但未定形（REST 路径、k8s 常规行为、手动触发机制等），实现前需冻结。

---

## 目录

1. [总览与快速对照](#1-总览与快速对照)
2. [k8s API server 访问通用约定](#2-k8s-api-server-访问通用约定)
3. [各 CRD 资源操作](#3-各-crd-资源操作)
4. [仍在 cubepilot-api 的接口（非 CRD）](#4-仍在-cubepilot-api-的接口非-crd)
5. [前端页面 ↔ 接口映射](#5-前端页面--接口映射)
6. [附录](#6-附录)

---

# 1. 总览与快速对照

## 1.1 架构

```text
Portal / Web (前端)
   │
   ├── CRD 资源 ────────► Kubernetes API server
   │                     （/apis/ai.cubestack.io/v1alpha1/...）
   │                      鉴权：RBAC / OIDC 令牌，owner 由平台授权层兜底
   │
   └── 非 CRD 能力 ──────► cubepilot-api（/api/*）
                         （对话 SSE、会话历史、技能发布上传）
```

## 1.2 快速对照

| 前端要做什么 | 走哪 |
|---|---|
| Agent 模板（含模型清单，只读） | k8s API server · `agenttemplates` |
| 开通 / 查看 / 改自己的实例 | k8s API server · `agentinstances` |
| 技能市场浏览 / 安装 | k8s API server · `skills` + `agentinstances` |
| 任务模板（创建向导，只读） | k8s API server · `tasktemplates` |
| 任务 CRUD / 暂停恢复 / 手动触发 | k8s API server · `tasks` |
| 巡检报告（只读） | k8s API server · `taskruns` |
| 对话（浮窗 / 独立 tab，SSE） | cubepilot-api · `POST /api/chat` |
| 会话历史 / 确认决策 | cubepilot-api · `/api/sessions/*` |
| 技能发布（上传 tar 写技能仓库） | cubepilot-api · `/api/skills*` |

## 1.3 CRD 资源清单（k8s API server）

group/version：**`ai.cubestack.io/v1alpha1`**（📘，见设计全部 YAML）。对象不落在任何 namespace（📘 未声明 namespace，以 `owner` 字段区分归属）。

| kind（📘） | REST 路径尾段（🛠 常规复数） | 前端读写 | 状态字段写方 | 前端用途 |
|---|---|---|---|---|
| `AgentTemplate` | `agenttemplates` | 只读 | — | 平台内置模板（阶段一仅 `agent-for-cloud`），配置页数据源 |
| `AgentInstance` | `agentinstances` | 读 + 建 + 改 | 控制器 | 「Agent 配置」页：开通 / 选模型 / 启技能 / 用户指令 |
| `Skill` | `skills` | 读；安装=改实例 | 控制器 | 技能市场：浏览 / 详情 / 安装 |
| `TaskTemplate` | `tasktemplates` | 只读 | — | 任务创建向导 |
| `Task` | `tasks` | 读 + 建 + 改 + 删 | Scheduler | 任务列表、暂停/恢复、手动触发 |
| `TaskRun` | `taskruns` | 只读 | Scheduler | 巡检报告、证据链、P0/P1/P2 |

> 📘 **不存在的 CRD**：`Model`（模型内联在 AgentTemplate.models，设计 §3.3）、`Agent`（模板即 `AgentTemplate`，§3.1）；`Capability` 已重命名为 `Skill`（设计 §3.4）。

---

# 2. k8s API server 访问通用约定

## 2.1 入口与认证 🛠（部署相关）

- REST 路径统一为：`/apis/ai.cubestack.io/v1alpha1/<plural>[/<name>][/status|/watch]`
- 前端到达 k8s API server 的两种方式：
  - **Web 网关反向代理**：把 `/apis/...` 转发到集群 API server（推荐，前端用相对路径）；
  - 或 k8s API server 直接暴露（Ingress/Route），前端携带 OIDC 令牌（`Authorization: Bearer <token>`）。
- 认证：OIDC（📘 设计 §6）。平台为每个登录用户签发**最小权限 RBAC**，前端只携带自己的身份令牌，不注入他人身份。

## 2.2 标准动词与路径模板

| 操作 | 方法与路径 | 说明 |
|---|---|---|
| 列表 | `GET` `/apis/…/v1alpha1/{plural}` | 返回 `{apiVersion, kind: "…List", metadata, items[]}` |
| 单个 | `GET` `…/{plural}/{name}` | 返回单个对象 |
| 创建 | `POST` `…/{plural}` | body = 完整对象（含 `apiVersion`/`kind`/`metadata.name`/`spec`） |
| 整体更新 | `PUT` `…/{plural}/{name}` | body = 完整对象，`metadata.resourceVersion` 必须带 |
| 部分更新 | `PATCH` `…/{plural}/{name}` | body = JSON Patch 或 Merge Patch（见 §2.4） |
| 删除 | `DELETE` `…/{plural}/{name}` | 可带 `?propagationPolicy=Background` |
| 状态 | `PUT` `…/{plural}/{name}/status` | **前端只读状态，不写**（控制器/Scheduler 独占） |
| 监听 | `GET` `…/{plural}?watch=true` | 实时变化推送，见 §2.5 |

## 2.3 对象格式

所有 CRD 对象是标准 k8s 对象：

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "AgentInstance",
  "metadata": { "name": "zhang-wei-agent-for-cloud", "annotations": {}, "resourceVersion": "12345" },
  "spec": { … },
  "status": { … }
}
```

- `spec` 由前端提供；`status` 由控制器/Scheduler 维护，**前端绝不提交/修改 status**。
- 列表项在 `items[]` 里，每项即一个完整对象。

## 2.4 部分更新（PATCH）🛠

CRD **不支持** strategic-merge-patch（对 CRD 无效），用：

- JSON Patch（`application/json-patch+json`）：`[{ "op": "replace", "path": "/spec/state", "value": "Paused" }]`
- Merge Patch（`application/merge-patch+json`）：`{ "spec": { "state": "Paused" } }`

示例——暂停任务：

```json
PATCH /apis/ai.cubestack.io/v1alpha1/tasks/zhang-wei-daily-inspection
Content-Type: application/merge-patch+json

{ "spec": { "state": "Paused" } }
```

## 2.5 列表过滤与分页

- **labelSelector**：`?labelSelector=cubepilot/owner=zhang.wei` —— 推荐用于「只拉自己的」列表（见 §2.8）。
- **fieldSelector**：仅支持索引字段（`metadata.name` 等）；`spec.owner` **不可**用 fieldSelector 过滤。
- **分页**：列表响应 `metadata.continue` 非空时，加 `?continue=<值>` 取下一页（配合 `?limit=`）。
- **resourceVersion**：更新冲突时返回 `409`，前端重拉后重试。

## 2.6 watch 实时状态（可选）🛠

`GET /apis/…/{plural}?watch=true`（配合 `&fieldSelector=metadata.name=…` 可只看单个对象）。响应逐帧：

```json
{"type":"ADDED","object":{ … }}
{"type":"MODIFIED","object":{ … }}
{"type":"DELETED","object":{ … }}
```

前端用途：实例 `status.phase`、TaskRun `status.phase`（Running→Completed）、Task 状态实时刷新。不想引入 watch 就降级为轮询。

## 2.7 资源名（metadata.name）约束 🛠

- CR name 必须满足 **DNS-1123**：小写字母数字，可含 `-` `.`。
- 中文等人类可读名放 **字段/annotation**，不放 CR name：AgentTemplate/Skill/TaskTemplate 有 `displayName` 字段；Task 的显示名放 `cubepilot/display-name` annotation（🛠 拟定，见 §3.5）。
- 用户输入的中文/空格/特殊字符，前端生成 CR name 前必须 **sanitize**。

## 2.8 隔离与授权（最重要的一条）⚠️

> 直连 k8s API server 后，原 facade 不再替前端做「owner=请求者」的强制与列表过滤。
> **安全边界整体移交平台侧 RBAC / 授权层**，前端必须遵守以下规则，否则就是越权通道：

- **写（create/update/delete）**：`spec.owner` 字段**只能填当前登录用户**。平台必须用 RBAC + 授权（validating admission / 授权 webhook）强制「谁创建 owner 就必须是自己」，前端只是客户端、不承担安全责任。
- **读（list）**：k8s list 无法按 `spec.owner` 过滤，平台**应为对象打 `cubepilot/owner=<user>` 标签**（🛠 拟定约定），前端列表一律带 `?labelSelector=cubepilot/owner=<当前用户>`。若平台暂未提供该标签，列表请求必须被平台授权层拦截为「只返回调用者自己的对象」——前端**不要**依赖「拉全量再前端过滤」来达成隔离。
- **状态**：`status` 字段只读，由控制器/Scheduler 写入，前端不提交。
- **二次校验**：展示、编辑、删除前都校验 `spec.owner === 当前用户`，不符即不展示（双重保险，不依赖它做安全）。

---

# 3. 各 CRD 资源操作

## 3.1 `agenttemplates` —— Agent 模板（只读）

阶段一只有平台内置 `agent-for-cloud`（📘 设计 §3.1），前端只读。

- `GET /apis/ai.cubestack.io/v1alpha1/agenttemplates`
- `GET /apis/ai.cubestack.io/v1alpha1/agenttemplates/agent-for-cloud`

对象（📘 字段来自设计 §3.1 YAML）：

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "AgentTemplate",
  "metadata": { "name": "agent-for-cloud" },
  "spec": {
    "runtime": "OpenClaw",
    "displayName": "平台管理助手",
    "defaultModel": "deepseek-v4-flash",
    "models": [
      { "name": "deepseek-v4-flash", "endpoint": "https://api.deepseek.com", "credentialRef": { "name": "cubepilot-llm" } }
    ],
    "instructions": "你是 CubeStack 平台管理助手。……",
    "skills": ["dev-environment", "inference-service", "cluster-inspection"],
    "confirmPolicy": "ConfirmWrites"
  }
}
```

前端用途（Agent 配置页）：

- `spec.models[].name` → 「切换模型」下拉选项（`selectedModel` 只允许从中选）。
- `spec.skills` → 可启用技能清单（实例子集 = `enabledSkills`）。
- `spec.confirmPolicy` → 决定对话中是否会出现 `confirm_pending`（`ConfirmWrites` 时写操作暂停确认）。

> 📘 模型**没有独立 CRD**：模型清单（`name` + `endpoint` + 可选 `credentialRef`）内联在模板的 `models` 列表（设计 §3.3）。`name` 同时是选择 key、网关 provider key 与后端模型名。

### 添加 LLM（cubepilot-api · `POST /api/llms`）

平台管理员追加一个 OpenAI 兼容模型到内置 `agent-for-cloud` 模板；operator 将其渲染进网关配置。非 public 模型会创建一个 `llm-<name>` 凭据 Secret（只存 apiKey）。

```json
{ "name": "qwen2.5-72b", "endpoint": "https://api.example.com/v1", "apiKey": "sk-..." }
```

省略 `apiKey` 表示 public 模型（不建 Secret）。返回创建的模型条目。Portal「LLM 配置」页面封装此调用。

## 3.2 `agentinstances` —— 实例（开通 / 配置）

每用户一个实例（📘 设计 §3.2）。用户自服务开通，owner 必填 = 当前用户。

**列表**：`GET /apis/ai.cubestack.io/v1alpha1/agentinstances?labelSelector=cubepilot/owner=<user>`

**开通**：`POST /apis/ai.cubestack.io/v1alpha1/agentinstances`

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "AgentInstance",
  "metadata": { "name": "zhang-wei-agent-for-cloud" },
  "spec": {
    "owner": "zhang.wei",
    "templateRef": "agent-for-cloud",
    "selectedModel": "deepseek-v4-flash",
    "enabledSkills": ["dev-environment", "inference-service"],
    "userInstructions": "回答尽量简洁，使用中文。",
    "dataVolume": { "pvc": "pvc-zhang-wei-agent-for-cloud" },
    "identity": { "mode": "user", "principalRef": { "userRef": "zhang.wei" } }
  }
}
```

前端注意：

- `metadata.name` = `{sanitize(owner)}-{templateRef}`（如 `zhang-wei-agent-for-cloud`），前端按此规则生成，保证幂等（重复创建同 name → 409，前端视为「已存在」）。
- **`spec.owner` 与 `spec.identity.principalRef.userRef` 必须 = 当前登录用户**（§2.8）。
- `selectedModel` 只允许从 §3.1 模板的 `models` 里选（📘）；`enabledSkills` 是启用的技能子集（📘）。
- 开通后 `status.phase` 由控制器从创建 → `Ready`（可用 watch 刷新）；`status.podName` 只读展示。

**更新**：`PATCH /apis/ai.cubestack.io/v1alpha1/agentinstances/{name}`

```json
{ "spec": { "selectedModel": "qwen2.5-72b", "enabledSkills": ["dev-environment", "inference-service", "cluster-inspection"], "userInstructions": "…" } }
```

只允许改这三个字段（📘 设计 §3.2 可覆盖字段）：模型切换重新解析注入；技能变更热加载；提示词变更不支持热加载时退化为重启 OpenClaw（会话与记忆在 PVC，不丢失）。

## 3.3 `skills` —— 技能市场

📘 设计 §3.4：skill = 一个多文件目录（`SKILL.md` + 可选 `scripts/`、`references/`），内容在技能仓库（共享文件卷），`Skill` CRD 只登记「有什么、在哪、什么版本、谁可见」。

**列表（浏览）**：`GET /apis/ai.cubestack.io/v1alpha1/skills`

**详情**：`GET /apis/ai.cubestack.io/v1alpha1/skills/{name}`

对象（📘 字段来自设计 §3.4 YAML）：

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "Skill",
  "metadata": { "name": "harbor" },
  "spec": {
    "displayName": "镜像管理",
    "description": "查询 / 清理 Harbor 镜像",
    "visibility": "Platform",
    "source": { "type": "Path", "path": "skills/harbor/v1.tar.gz", "sha256": "…" }
  },
  "status": { "phase": "Available" }
}
```

前端要点：

- `spec.visibility` 枚举 `Platform | Tenant | User`（📘）；阶段一只有平台级技能（`User` 私有技能阶段二放开）。
- `spec.source.type` 判别字段 `Path | S3`（📘）：阶段一只支持 `Path`（共享文件卷内路径，含版本号，不可变）；`source.sha256` 为内容校验指纹（手动 apply 可留空）。
- `status.phase`: `Available | Unreachable`（📘）。

**安装**（🛠 无独立端点）：把技能名加进自己的实例 `spec.enabledSkills`（§3.2 PATCH）→ injector 从技能仓库读取解压 → OpenClaw 文件监听热加载。前端判断「已安装」= 技能名 ∈ 我的实例 `enabledSkills`。

**发布**：⚠️ 上传 skill 目录要**写技能仓库共享文件卷**，不是 CRD 操作，走平台服务（§4.3，后端打包写卷 + 建 Skill CR）。

## 3.4 `tasktemplates` —— 任务模板（只读）

`GET /apis/ai.cubestack.io/v1alpha1/tasktemplates` —— 任务创建向导用。

对象（📘 字段来自设计 §3.5 YAML）：

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "TaskTemplate",
  "metadata": { "name": "daily-inspection" },
  "spec": {
    "displayName": "每日集群巡检",
    "instruction": "以只读方式巡检集群……巡检范围：{{scope}}。",
    "paramsSchema": [ { "name": "scope", "default": "All", "enum": ["All", "NodePool", "Project"] } ],
    "requiredPermissions": { "level": "ClusterRead" },
    "skills": ["cluster-inspection"],
    "defaultCron": "0 2 * * *"
  }
}
```

前端用途：向导按 `spec.paramsSchema` 渲染参数输入（下拉/输入框），`defaultCron` 作调度默认值提示。

## 3.5 `tasks` —— 任务（CRUD + 暂停恢复 + 手动触发）

**列表**：`GET /apis/ai.cubestack.io/v1alpha1/tasks?labelSelector=cubepilot/owner=<user>`

**创建**：`POST /apis/ai.cubestack.io/v1alpha1/tasks`

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "Task",
  "metadata": {
    "name": "zhang-wei-daily-inspection",
    "annotations": { "cubepilot/display-name": "每日集群巡检" }
  },
  "spec": {
    "owner": "zhang.wei",
    "templateRef": "daily-inspection",
    "params": { "scope": "all" },
    "trigger": "Cron",
    "cron": "0 2 * * *",
    "state": "Enabled"
  }
}
```

前端注意：

- `metadata.name` 前端生成 `{sanitize(owner)}-{模板或随机名}`；人类可读名放 `cubepilot/display-name` annotation（🛠 拟定；**中文名必须放 annotation，否则 DNS-1123 校验失败**）。
- `spec.owner` = 当前用户（§2.8）；`templateRef` 引用 §3.4 模板；`params` 只允许覆盖模板 `paramsSchema` 允许的参数（📘）。
- `trigger` 枚举 `Cron | Manual`（📘）；`state` 枚举 `Enabled | Paused`（📘 字符串枚举，不用 bool）。

**暂停 / 恢复**：`PATCH` `…/tasks/{name}`，`{ "spec": { "state": "Paused" } }` / `{ "spec": { "state": "Enabled" } }`。

**手动触发** 🛠（契约拟定）：PATCH 写 run 请求 annotation，Scheduler 监听触发一次后自动清除（时间戳即幂等键）：

```json
{ "metadata": { "annotations": { "cubepilot/manual-run": "2026-08-25T10:00:00Z" } } }
```

> 📘 设计 §3.5/§7：每次执行前 Scheduler 重新校验用户与授权，以平台身份写 TaskRun；前端不直接创建 TaskRun。上面 annotation 名/值格式是 🛠 拟定，需与 Scheduler 约定后冻结。

**更新**：`PUT`（整体，带 `resourceVersion`）或 `PATCH`（改 `params`/`cron`/`state`）。
**删除**：`DELETE` `…/tasks/{name}`（历史 TaskRun 保留）。

**列表展示最近执行情况**：关联查 §3.6 `taskruns`（按 task 过滤）。（📘 设计 §3.5 未给 Task 定义 status 字段，前端不从 Task 状态取运行结果。）

## 3.6 `taskruns` —— 运行记录 / 巡检报告（只读）

Scheduler 以平台身份创建，前端只读（📘 设计 §3.5/§7）。

**列表**：`GET /apis/ai.cubestack.io/v1alpha1/taskruns?labelSelector=cubepilot/owner=<user>`
（单个任务的报告：叠加 `?labelSelector=cubepilot/task=<taskName>`，🛠 标签名待定）

**单个**：`GET /apis/ai.cubestack.io/v1alpha1/taskruns/{name}`

对象（📘 字段来自设计 §3.5 YAML）：

```json
{
  "apiVersion": "ai.cubestack.io/v1alpha1",
  "kind": "TaskRun",
  "metadata": { "name": "zhang-wei-daily-inspection-20260820-020001" },
  "spec": {
    "creatorTaskRef": { "name": "zhang-wei-daily-inspection", "uid": "…" },
    "trigger": "Cron"
  },
  "status": {
    "phase": "Completed",
    "startedAt": "2026-08-20T02:00:01Z",
    "finishedAt": "2026-08-20T02:02:30Z",
    "templateRevision": 7,
    "skillRevision": 4,
    "summary": { "p0": 0, "p1": 1, "p2": 3 },
    "content": "巡检报告全文……（异常附证据链）",
    "error": ""
  }
}
```

前端要点：

- `status.phase`：`Pending → Running → Completed / Failed`（📘），报告页用 watch 或轮询刷新。
- 报告渲染：`status.content`（全文）+ `status.summary`（P0/P1/P2 计数）+ `status.error`（失败原因）。
- `status.templateRevision` / `skillRevision` 展示「本次实际用到的版本」（📘 审计）。

---

# 4. 仍在 cubepilot-api 的接口（非 CRD）

> 以下能力**没有 CRD**（或内容不落在 CRD 上），走不了 k8s API server，仍走平台服务（`/api/*`）。
> 认证：OIDC（📘 设计 §6）。**路径为 🛠 拟定契约**（设计 §4 只定义了对话经平台服务转发、事件契约，未定 REST 路径）。

## 4.1 对话与会话

| 接口（🛠 路径拟定） | 方法 | 说明 |
|---|---|---|
| `POST /api/chat` | 发送消息 | 响应为 **SSE 流**（事件契约见 §4.2） |
| `GET /api/sessions/{sessionId}/messages` | 会话历史 | 渲染 / 刷新后恢复 |
| `POST /api/sessions/{sessionId}/confirm` | 确认决策 | 写操作 HITL（收到 `confirm_pending` 后调用） |

请求体（发送消息）：

```json
{ "sessionId": "conv-xxx", "content": "帮我创建一个 DevEnvironment" }
```

> - `sessionId` 缺省时服务端新建，经 `message_start` 事件返回；前端持久化后复用。
> - 📘 设计 §1.1：session **全局统一**（浮窗与独立 tab 共用同一会话，不按模块区分）——前端维护一个 sessionId 即可。
> - 📘 设计 §3.6：会话与消息真源在实例 PVC（Agent 私有数据），历史经平台服务代取，前端不感知存储位置。

## 4.2 SSE 事件契约（对话）📘

设计 §4 的**统一事件**，共 8 个（字段为 🛠 拟定）：

| 事件（📘） | 数据字段（🛠） | 前端行为 |
|---|---|---|
| `message_start` | `sessionId` | 记录 sessionId，进入「回答中」 |
| `message_delta` | `sessionId`, `delta` | 追加助手文本 |
| `tool_call` | `sessionId`, `name`, `callId`, `arguments`(JSON 字符串) | 展示工具调用（可折叠） |
| `tool_result` | `sessionId`, `name`, `callId`, `output` | 展示结果摘要 |
| `confirm_pending` | `sessionId`, `callId`, `tool`, `command`, `level`(read/write), `message` | 写操作命中确认规则，弹确认框 |
| `confirm_resolved` | `sessionId`, `callId`, `approved` | 决策已提交，继续 |
| `message_done` | `sessionId`, `error`(空=成功) | 终态，清除「回答中」 |
| `error` | `sessionId`, `error` | 致命错误 |

- 写操作 HITL 时序（📘 设计 §5）：`tool_call → confirm_pending →（前端 POST /api/sessions/{sid}/confirm）→ confirm_resolved → tool_result → … → message_done`。
- 只读操作无 `confirm_pending`，直放（📘：读操作直放，写命中才确认）。
- 🛠 前端应忽略未知事件类型（实现可能补充事件）。

## 4.3 技能发布（上传）🛠

📘 设计 §3.4：发布 = **上传 skill 目录**（`SKILL.md` + 可选 `scripts/`、`references/`）→ 后端打包写入技能仓库共享文件卷（先写临时文件再原子 rename）+ 建 `Skill` CRD。

- 走 `cubepilot-api`（如 `POST /api/skills`，multipart 上传，🛠 端点待定）——因为「写共享文件卷」不是 CRD 操作，走不了 k8s API server。
- 上传完成后 Skill CR（§3.3）由后端创建；前端可在列表里刷新看到新技能。
- 反之：技能**浏览 / 安装**（改自己实例的 `enabledSkills`）走 k8s API server（§3.3）。

---

# 5. 前端页面 ↔ 接口映射

| 页面 / 入口 | 数据来源 | 走 |
|---|---|---|
| 全局对话浮窗 / cubepilot 独立 tab | 历史、发消息（SSE）、确认 | cubepilot-api |
| Agent 配置页 | 模板（`agenttemplates`）· 实例（`agentinstances`） | k8s API server |
| 技能市场（浏览 / 安装） | `skills` 列表 · 我的实例 `enabledSkills` | k8s API server |
| 技能管理（发布上传） | `POST /api/skills`（multipart） | cubepilot-api |
| 任务列表 / 创建向导 | `tasktemplates` · `tasks`（CRUD） | k8s API server |
| 手动触发 / 暂停恢复 | `tasks`（PATCH annotation / state） | k8s API server |
| 巡检报告页 | `taskruns`（列表 / 详情） | k8s API server |

---

# 6. 附录

## 6.1 k8s API server 错误格式

非 2xx 返回 k8s `Status` 对象：

```json
{ "kind": "Status", "apiVersion": "v1", "status": "Failure", "message": "…", "reason": "NotFound", "code": 404 }
```

常用 `code`：`400`(非法请求) · `403`(RBAC 拒绝) · `404`(不存在) · `409`(resourceVersion 冲突/重名) · `422`(校验失败)。

## 6.2 时间格式

k8s 时间字段为 RFC3339 UTC（`2026-08-20T02:00:01Z`）。

## 6.3 对象名生成规则速查

| 资源 | CR name 规则（🛠） |
|---|---|
| AgentInstance | `{sanitize(owner)}-{templateRef}` |
| Task | `{sanitize(owner)}-{模板或随机名}`（显示名放 annotation） |
| Skill / TaskTemplate / AgentTemplate | 简短英文名（DNS-1123） |

## 6.4 隔离与安全清单（对照 §2.8）

- [ ] 前端所有写请求 `spec.owner` = 当前登录用户；
- [ ] 列表一律带 `labelSelector=cubepilot/owner=<user>`（平台提供该标签的前提下，🛠 需平台落地）；
- [ ] 平台侧：RBAC 最小权限 + 授权层强制 owner（**前置条件，未落地前前端直连存在越权风险**）；
- [ ] `status` 字段前端只读，绝不提交；
- [ ] 展示前二次校验 `spec.owner === 当前用户`。

## 6.5 与设计文档的对应

| 本文档章节 | 设计章节 |
|---|---|
| §3.1 `agenttemplates` | §3.1（AgentTemplate）、§3.3（模型内联） |
| §3.2 `agentinstances` | §3.2 |
| §3.3 `skills` | §3.4 |
| §3.4 `tasktemplates`、§3.5 `tasks`、§3.6 `taskruns` | §3.5、§7 |
| §4.1/§4.2 对话与会话 | §1.1、§4、§5 |
| §2.8 隔离与授权 | §6、附录 B |
