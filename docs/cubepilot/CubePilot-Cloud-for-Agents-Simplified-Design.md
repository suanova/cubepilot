# CubePilot · Cloud for Agents 简化设计

**状态：** Draft
**原则：** 平台能力 API 化；Agent 负责理解、规划和汇总，平台负责执行、授权和记录。

> CubePilot 的核心是“每个用户一个管理 CubeStack 的 Agent”。本文保留未来扩展的接口，但不将 Agent 市场、多 Runtime、代码托管或通用 Agent 云作为当前架构前提。

---

# 1. 目标与边界

## 1.1 核心功能

- Portal/API 中与用户自己的平台管理 Agent 对话，并获得流式回答。
- Agent 以用户最小权限查询或操作已支持的 CubeStack 资源。
- 用户创建定时巡检任务，获得带证据链的报告。
- 每个用户的会话、记忆和运行数据相互隔离，并能跨实例重启保留。
- 平台管理实例生命周期，记录工具调用、确认决定和运行指标。

## 1.2 不属于当前核心

- 不实现 Agent 市场、跨用户共享、用户自定义容器或多 Agent 协作。
- 只运行 OpenClaw，不实现多个 Runtime。
- 不为每个 CRD 编写专用工具；通用 Kubernetes 能力基于用户权限动态发现和执行。
- 不建设独立 Tool Gateway、Credential Registry、统一模型代理或统一 fallback。

这些能力可在不改变本文接口的情况下增加，但不是当前设计的前置条件。

## 1.3 核心决策

1. 一个内置模板：`agent-for-cloud`。
2. 每用户一个实例：`(user, agent-for-cloud)` 对应一个 Runtime Pod 和一个 PVC。
3. 一个 Runtime 接口、一个实现：平台依赖 `AgentRuntime`，当前唯一实现是 `OpenClawRuntime`。
4. 工具在 Runtime Pod 内执行：`ToolExecutor` 直接调用 `kubectl` 或专用客户端。
5. 保留三层能力：generic 自动发现，atomic 补语义/安全，domain 承载领域知识与 Skills。
6. 声明配置在控制面，私有状态在 PVC：PVC 不作为 Agent 配置真源。

---

# 2. 总体架构

```mermaid
flowchart TB
    U["用户 / Portal / API"] --> S["CubePilot Service\n路由 · SSE · 配置"]
    S --> IM["Instance Manager\n生命周期 · PVC"]
    S --> RT
    SCH["Scheduler\nTaskTemplate → Task → TaskRun"] --> RT

    subgraph RT["AgentInstance Runtime Pod"]
      OC["OpenClawRuntime\n对话 · 规划 · 汇总"]
      TE["ToolExecutor\n校验 · 确认 · 审计 · 执行"]
      PVC["PVC\n会话 · 记忆 · 私有状态"]
      OC --> TE
      OC <--> PVC
    end

    TE --> K["Kubernetes API\n用户最小权限凭据"]
    OC --> L["LLM Endpoint"]
    S --> DB["PostgreSQL / Redis\n平台元数据、任务、审计索引"]
    SCH --> DB
```

| 组件 | 职责 | 不负责什么 |
|---|---|---|
| Portal/API | 认证、对话入口、配置、任务与报告查询 | 不持有 Agent 会话状态 |
| CubePilot Service | Agent 路由、SSE 转发、配置解析、实例查找 | 不执行 LLM 编排或 kubectl |
| Instance Manager | 创建、停止、自愈 Pod；挂载 PVC 和凭据 | 不理解用户自然语言 |
| `OpenClawRuntime` | 对话、规划、工具调用、回答 | 不决定 RBAC 或管理 Pod |
| `ToolExecutor` | generic/Capability 校验、确认、审计、命令执行 | 不做 Agent 规划 |
| Scheduler | 触发任务、调用实例、写入 TaskRun | 不持有用户资源权限 |

## 2.1 请求路径

1. Portal 通过 OIDC 认证，向 Service 发送消息。
2. Service 定位用户实例；不存在或不健康时由 Instance Manager 拉起。
3. Service 调用 `AgentRuntime.chat` 并转发统一事件流。
4. Runtime 需要工具时调用本 Pod 的 ToolExecutor。
5. Executor 按 generic/Capability 规则、用户身份和确认规则执行操作，返回结构化结果。
6. Runtime 汇总结果，将私有会话状态写入 PVC。

---

# 3. 核心对象与数据归属

## 3.1 AgentTemplate

当前只有一个平台内置模板 `agent-for-cloud`。它不是完整 Agent Registry，也不是可由用户创建的市场对象。

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: AgentTemplate
metadata:
  name: agent-for-cloud
spec:
  runtime: openclaw
  displayName: 平台管理助手
  defaultModel: deepseek-v4-flash            # 引用 Model 目录名（§3.3）
  # availableModels: []                      # 可选：限定可选模型清单；缺省 = 目录全部可用
  instructions: |
    你是 CubeStack 平台管理助手。优先使用已登记能力；
    不确定资源或权限时先解释并请求用户澄清。
  capabilities: [dev-environment, inference-service, cluster-inspection]
```

模板变更生成不可变 `revision`，供审计与回滚。实例引用模板名（不钉版），模板更新在下次实例 reconcile 或重启时生效，不能静默改变正在运行的行为。

## 3.2 AgentInstance

每个用户有一个 AgentInstance，保存受模板约束的用户配置和运行状态。

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: AgentInstance
metadata:
  name: zhang-wei-agent-for-cloud
spec:
  owner: zhang.wei
  templateRef: agent-for-cloud              # 引用模板名（不钉版；模板更新在下次 reconcile/重启时生效）
  selectedModel: deepseek-v4-flash         # 从 Model 目录（§3.3）中选择；切换后经 updateConfig 热生效
  enabledCapabilities: [dev-environment, inference-service, cluster-inspection]
  userInstructions: "回答尽量简洁，使用中文。"
  dataVolume: { pvc: pvc-zhang-wei-agent-for-cloud }
  identity: { userRef: zhang.wei }
status:
  phase: Ready
  podName: agent-zhang-wei-agent-for-cloud
```

允许覆盖的字段只有模型选择（`selectedModel`，候选集 = Model 目录；若模板指定 `availableModels` 则为其子集）、Capability 子集和 `userInstructions`。切换模型 = 改 `selectedModel` → Service 重新合并 `ResolvedAgentConfig` → `AgentRuntime.updateConfig()` 热生效；OpenClaw 不支持热更新时退化为 Pod 重启（会话与记忆在 PVC，不丢失）。`userInstructions` 仅追加用户偏好，最终指令由平台安全与执行约束、模板 `instructions`、用户指令依次组合；它不能删除、替换或降低模板中的安全边界、工具规则和身份限制，也不得扩大模板定义的能力或权限。

**实例开通（自服务）**：用户通过 Portal「Agent 配置」页或 `POST /api/instances` 开通自己的实例（owner 恒为请求者，服务端强制，防越权；读列表同样只返回自己的实例）。重复开通幂等返回已存在实例，不重复拉起 Pod/PVC。operator 控制器负责后续生命周期（Pod/PVC/Service 创建与自愈），API 只写 AgentInstance CR。阶段一预置用户（values 配置的 bootstrap 名单）由 operator 启动时创建；生产环境不依赖该名单，管理员在页面上开通或 `kubectl apply` 均可。

## 3.3 模型目录 (Model)

`Model` 是平台级的 LLM 模型目录，由管理员维护。Agent 模板和实例通过名字引用，不与具体端点或凭据耦合——端点与凭据在 Model 对象内，平台托管、不落明文。

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: Model
metadata:
  name: deepseek-chat                 # 目录名，被 selectedModel / defaultModel 引用
spec:
  displayName: DeepSeek Chat
  provider: platform                  # platform（平台托管推理）| external（OpenAI 兼容端点）
  endpoint: https://inference.example.com/v1  # 统一可填；platform 留空 = 内置运行时模型
  credentialRef: cubepilot/cred-llm-org  # external 必填；platform 可选（无鉴权端点可省）→ 平台托管 Secret（apiKey）
status:
  phase: Available                    # Available / Unreachable（controller 注册时探测）
```

**provider 语义（统一探测闭环）**：
- `platform` = 平台托管的推理能力。两种形态：
  - **内置运行时模型**（`endpoint` 留空，如预置的 `deepseek-v4-flash`）：运行时镜像内部解析，无端点可探测 → 直接 `Available`。
  - **手动部署的推理服务**（`endpoint` 填写）：管理员把已部署好的服务登记进目录，controller 与 external 一样探测连通性。
- `external` = OpenAI 兼容端点：`endpoint` + `credentialRef` 必填，controller 探测 `GET {endpoint}/models`（带凭据），2xx/3xx → `Available`，失败/401 → `Unreachable`。
- 探测不通过的模型 `status.phase = Unreachable`，实例不可选用（fail-closed，绝不静默回退）。

平台预置 `deepseek-v4-flash`（`provider: platform`、无 endpoint）等模型条目。管理员加新模型：`kubectl apply` 一个 Model CRD（或 Portal「模型管理」页填写名字、provider、endpoint、选已有 Secret）→ Controller 校验连通性（有 endpoint 一律探测）→ `status.phase = Available` → 用户立即可在 `selectedModel` 中选到。

新增模型不动任何运行中实例；只有修改实例 `selectedModel` 才触发配置变更（通过 `AgentRuntime.updateConfig()` 热生效或 Pod 重启——见 §3.2）。

## 3.4 Capability、Skill 与 MCP

三者不是替换关系：Capability 是平台治理和 Agent 可见性模型，Skill 是 Runtime 使用的知识/脚本包，MCP 是外部工具的接入协议。

```text
Agent
  → generic 工具：动态发现 CRD、通用 CRUD、kubectl-raw（可选）
  → Capability(atomic)：为一个 CRD 追加语义与安全规则
  → Capability(domain)：领域知识，引用 generic / atomic / MCP 工具
       → Skill：指令、受控脚本、参考资料
       → MCP：Prometheus、ITSM 等外部系统工具
```

| 层 | 是否为 CRD | 解决的问题 | 模块工作量 |
|---|---|---|---|
| generic | 否，平台内置 | `list-kinds`、`describe-kind`、`resource-manager` 通用 CRUD | 无 |
| atomic | `Capability` | 补充特定 CRD 的名称、说明、示例、禁止操作和确认规则 | 可选的薄 YAML |
| domain | `Capability` | 巡检、诊断、推荐等领域流程 | 指令/Skill，必要时少量脚本 |

generic 是默认能力：ToolExecutor 使用实例 owner 的 kubeconfig 发现其可见资源，并以相同身份执行。RBAC 是授权边界；平台不必为每个新 CRD 登记工具。generic 写操作遵循统一确认默认值，atomic Capability 可对目标资源追加更严格的 deny/confirm 规则。

**atomic 示例：**

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: Capability
metadata:
  name: dev-environment
spec:
  type: atomic
  target: { group: dev.suanova.io, version: v1alpha1, kind: DevEnvironment }
  semantics:
    title: 开发环境管理
    description: 按自然语言创建、查询或删除开发环境。
    examples: ["创建一个 4 核 16G、1 张 A100 的 PyTorch 环境"]
  security:
    denyOperations: []
    confirmWrites: true
```

**domain 示例：**

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: Capability
metadata:
  name: cluster-inspection
spec:
  type: domain
  uses: [resource-manager, kubectl-raw]
  skillRef: cluster-inspection@4
  instructions: |
    以只读方式检查节点、GPU、异常 Pod、PVC 和平台组件；
    对每项异常附上证据并按 P0/P1/P2 分类。
```

Skill 不新增一个控制面对象：它是 domain Capability 的内容包，可内联，也可引用不可变 ConfigMap/镜像文件。Skill 包含 `SKILL.md`、受控脚本和参考资料；Runtime 只加载当前 Capability 显式引用的 Skill。MCP 是 ToolExecutor 的一种外部执行实现，Capability 可在 `uses[]` 中引用已注册的 MCP 工具。

## 3.5 TaskTemplate、Task 与 TaskRun

三者同构「模板 ≠ 实例 ≠ 执行」：`TaskTemplate` 定义「做什么」（参数化、可复用）；`Task` 定义「谁 + 何时做」（用户绑定模板与调度）；`TaskRun` 记录「这次做得怎么样」（一次不可变执行记录）。

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: TaskTemplate
metadata:
  name: daily-inspection
spec:
  displayName: 每日集群巡检
  instruction: |
    以只读方式巡检集群（get/list/watch/logs）：检查节点 Ready 与压力、
    GPU 健康、异常 Pod、PVC 使用率与平台组件；异常附证据链并按 P0/P1/P2 分级；禁止写操作。
    巡检范围：{{scope}}。
  paramsSchema:
    - { name: scope, default: all, enum: [all, node-pool, project] }
  requiredPermissions: { level: cluster-read }
  capabilities: [cluster-inspection]      # 声明任务所需能力（执行时解析当前版本）
  defaultCron: "0 2 * * *"                # 创建向导的默认调度提示；以 Task.cron 为准
```

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: Task
metadata:
  name: zhang-wei-daily-inspection
spec:
  owner: zhang.wei
  templateRef: daily-inspection           # 引用模板名（不钉版，下次执行用当前版本）
  params: { scope: all }                  # 只覆盖 paramsSchema 允许的参数
  cron: "0 2 * * *"                       # 或 manual（手动触发）
  enabled: true
```

```yaml
apiVersion: assistant.suanova.io/v1alpha1
kind: TaskRun
metadata:
  name: zhang-wei-daily-inspection-20260820-020001
spec:
  creatorTaskRef: { name: zhang-wei-daily-inspection, uid: "…" }
  trigger: cron                           # cron | manual
status:
  phase: Completed                        # Pending → Running → Completed / Failed
  startedAt: "2026-08-20T02:00:01Z"
  finishedAt: "2026-08-20T02:02:30Z"
  templateRevision: 7                     # 运行时解析：本次实际用到的模板版本
  capabilityRevision: 4                   # 运行时解析：本次实际用到的 capability/skill 版本
  summary: { p0: 0, p1: 1, p2: 3 }
  content: "巡检报告全文……"
  error: ""                               # 失败原因
```

模板只回答「做什么」，调度与归属放在 Task 上。`templateRef` 只存名字、不钉版本，执行时解析当前模板（模板更新下次执行生效，不影响正在跑的一次）；因此 Task 上**不固化 `capabilitySnapshot`**——审计由 TaskRun 在运行时记录实际用到的 revision（见 §7）。`params` 只能覆盖模板 `paramsSchema` 允许的参数。阶段一每用户只有一个 `agent-for-cloud` 实例，可从 `owner` 推导，故不写 `agentInstanceRef`（阶段二多 Agent 时再加回）。每次执行前，Scheduler 重新验证用户有效性与授权；失败时写入 TaskRun，不执行工具操作。

## 3.6 数据真源

| 数据 | 真源 | 说明 |
|---|---|---|
| AgentTemplate、AgentInstance、Model、Capability、TaskTemplate、Task、TaskRun | CRD / 控制面数据库 | 声明配置、版本、生命周期、报告 |
| 会话、消息、记忆、Runtime 缓存 | 实例 PVC | Agent 私有数据，不复制到平台业务表 |
| 工具调用索引、确认决定 | 平台数据库 | 平台自产治理数据；为 Trajectory 的最小投影，不保存完整私有会话副本 |
| 运行指标 | 监控模块 / 日志 | 平台只产生并暴露，存储/查询/告警交监控 |

启动时，Service 将 AgentTemplate、AgentInstance、Model 和 Capability 合并为不可变 `ResolvedAgentConfig` 注入 Runtime；其中 `selectedModel` 解析到 Model 目录中的端点与凭据引用（明文密钥只在 Secret 中）。PVC 不是配置真源。

---

# 4. Runtime Adapter

平台仅依赖以下窄接口。当前只实现 `OpenClawRuntime`；OpenClaw 的 session、配置文件和原生事件不会泄漏到 Service、Scheduler 或 Instance Manager。

```ts
interface AgentRuntime {
  start(config: ResolvedAgentConfig): Promise<void>
  stop(): Promise<void>
  chat(request: ChatRequest): AsyncIterable<AgentEvent>
  runTask(request: TaskRequest): AsyncIterable<AgentEvent>
  updateConfig(config: ResolvedAgentConfig): Promise<void>
  health(): Promise<RuntimeHealth>
}
```

统一事件：`message_start`、`message_delta`、`tool_call`、`tool_result`、`confirm_pending`、`confirm_resolved`、`message_done`、`error`。

`ResolvedAgentConfig` 包含模型端点、系统指令、可用 Capability、用户身份、凭据挂载位置、PVC 路径和 ToolExecutor 配置；不包含明文密钥。

---

# 5. ToolExecutor

## 5.1 定位

ToolExecutor 是 Runtime 与下游系统之间的本地执行边界，随 Agent Pod 部署。当前由 OpenClaw 的 generic 工具和 Skill 调用受控脚本、`kubectl` 或已登记的 MCP 客户端，不引入独立网络 Gateway。

```text
OpenClaw generic 工具 / Skill
  → ToolExecutor.execute(toolOrCapability, operation, input)
  → 输入与资源范围校验 / 确认 / 审计事件
  → kubectl 或专用客户端（argv 直执行，无 shell）
  → { success, data | error }
```

该接口保持稳定。未来若需要集中 MCP 接入、集中凭据托管或跨 Runtime 策略，可由远程 Gateway 实现同一接口，不改变 AgentRuntime 或 Capability 的调用语义。

## 5.2 执行约束

- 加载平台内置 generic 工具，以及 `ResolvedAgentConfig` 允许的 atomic/domain Capability 与其 Skill。
- Kubernetes 调用使用实例所有者的最小权限短期凭据，禁止集群管理员凭据。
- Kubernetes API 使用用户凭据；Kubernetes RBAC 和资源归属校验是最终授权边界。
- `kubectl-raw` 不作为默认能力；如确有必要，仅允许 token 化 argv、动词和 flag 白名单，并默认要求确认。
- generic 写操作使用统一确认默认值；atomic/domain Capability 可以追加更严格的 deny/confirm 规则。确认拒绝或超时即失败，Executor 不重试被拒操作。
- 每次调用生成 AgentInstance、Capability、操作、目标、结果摘要和时间等审计事件。

## 5.3 通用资源发现与执行

generic 工具包含：

```text
list-kinds       列出当前用户可发现的资源类型
describe-kind    获取资源 Schema，帮助 Agent 正确填参
resource-manager {kind, action, data}
                 按 Schema 校验/渲染后执行通用 CRUD
kubectl-raw      可选逃生门；仅允许白名单 argv，默认要求确认
```

`resource-manager` 不需要为每个 CRD 编写专用代码。它以同一用户 kubeconfig 完成 discovery 与执行，因此无权限资源会由 API Server 拒绝。atomic Capability 只是 generic 工具的薄覆盖，不替换其字段 Schema 或执行器；domain Capability 则为多步问题提供领域知识和 Skill。

---

# 6. 身份、凭据与隔离

当前只支持**用户身份**：AgentInstance 的 `owner` 与 `identity.userRef` 必须一致，ToolExecutor 使用该用户派生的最小权限凭据。

- 用户被禁用、权限回收或凭据轮换时，Instance Manager 更新或撤销实例挂载的凭据。
- TaskRun 在运行前再次校验身份和授权，不依赖创建任务时的权限。
- 凭据由平台托管为 Secret，以文件挂载或短期令牌注入 Pod；Template、Instance、PVC 和审计记录中不存明文密钥。
- 模型凭据由 `Model.credentialRef` 引用平台托管 Secret，所有实例共享同一平台凭据；平台侧按实例/用户计量与审计，不分发个人密钥。用户自带模型凭据属于扩展项。
- 每实例独占 Pod 和 PVC，使用非 root、`readOnlyRootFilesystem`、`seccomp RuntimeDefault`、`drop ALL capabilities`、资源限制和 egress 白名单。

---

# 7. 定时任务与报告

Scheduler 只负责触发与记录，不拥有用户资源权限。

1. 到点后读取 Task，解析 `templateRef` 指向的 TaskTemplate 与当前 Capability，并检查 owner 状态。
2. 确认实例可用，调用 `AgentRuntime.runTask`。
3. Runtime 通过同一 ToolExecutor 执行 generic 工具及模板声明的 Capability（按当前版本）。
4. Scheduler 以平台身份写入 TaskRun；Runtime 不需要 CRD 写权限。

TaskRun 至少记录：Task UID、AgentInstance、Template revision（运行时解析）、Capability revision（运行时解析）、开始/结束时间、状态、报告摘要、证据引用和失败原因。完整私有会话仍保留在 PVC，需要长期留档时显式导出。

---

# 8. 观测与扩展边界

## 8.1 观测

- 健康检查：Service、Instance Manager、Scheduler、Runtime。
- 实例指标：启动耗时、就绪率、重启次数、PVC 使用率、并发会话数。
- Agent 指标：首 Token 延迟、完成延迟、模型调用量、工具成功率、确认等待时间。
- 审计索引：按用户、实例、Capability、TaskRun 和时间查询工具调用与确认。
- 故障隔离：单实例、PVC 或 LLM 失败不得阻塞其他用户或 CubeStack 控制面。

## 8.2 可扩展而不推翻核心的能力

| 扩展 | 保持不变的边界 |
|---|---|
| 第二个 Runtime | 实现 `AgentRuntime` 与统一 `AgentEvent` |
| 外置 MCP Gateway | 实现 `ToolExecutor`，Capability 语义不变 |
| 用户自建 Agent | 新增可版本化 Template 和 owner，不改变 Instance/PVC/Executor 模型 |
| 服务身份 | 扩展 identity 与凭据派生，仍由 Executor 执行前授权 |
| 外部下游 | 新增专用 Capability/Executor，不将业务逻辑写进 Runtime |
| 模型目录 | 新增 `Model` CRD，管理员维护；Template/Instance 按名引用 | 不改变 `AgentRuntime` 接口与 `AgentEvent` 契约 |
| 模型路由与 fallback | 放在 Runtime 或模型代理内部，不改变 Adapter 事件契约 |

## 8.3 待验证项

- OpenClaw 是否稳定支持事件、任务执行和配置热更新接口。
- 用户最小权限短期凭据的生成、轮换和撤销。
- Capability Schema 到受控 `kubectl` 参数/manifest 的转换与 dry-run。
- 审计索引能否满足排障，同时不复制 Agent 私有会话。
- PVC 容量、水位清理与实例重建后的会话恢复。

---

# 9. 演进方向

本设计刻意不把「Agent 云」作为当前架构前提，但保留通往它的路径。CubePilot 的战略方向是
「给 AI 提供算力 → 给 Agent 提供云（Cloud for Agents × Agents for Cloud）→ Agent-Native Cloud」，
本设计是这条线上的第一站。

```text
现在（本设计）：每个用户一个内置 agent-for-cloud
    平台能力 API 化，Agent 只做理解、规划、汇总；平台负责执行、授权、记录。
    │
    ├──► 阶段二：Agent 一等对象落地
            Agent / AgentInstance 版本化 · 用户自建 Agent（配置托管）
            Registry 审核发布 · Tool Gateway 统一 Policy/HITL · service 身份
    │
    └──► 阶段三：平台化与智能演进
            代码托管（container）· Agent Evaluation · 多 Agent · 模板市场
```

**关键认知：两层架构是「往哪去」，不是「现在建什么」。** 本设计已经把「与具体 Agent 无关」的部分
收敛为稳定接口——`AgentRuntime`（§4）、`ToolExecutor`（§5）、统一 `AgentEvent` 契约、`ResolvedAgentConfig`。
未来引入第二个 Agent、第二个 Runtime、集中 MCP 网关，都是在这些接口上的加法实现（§8.2），不重建核心对象模型。

用户自建 Agent 与内置 agent-for-cloud 的能力差异 = Agent 定义（tools / instructions / identity）的差异，
而非平台能力的差异；平台层新增任何能力，内置与自建 Agent 同时受益。这一演进不改动本设计的接口。

---

# 附录 A · 三层能力细节

正文 §3.4 已给出三层能力骨架，此处补充与阶段一实现相关的细节，作为参考而非独立规范。

## A.1 模块负担递减

| 层 | 是什么 | 谁提供 | 模块要做什么 |
|---|---|---|---|
| generic | `list-kinds` / `describe-kind` / `resource-manager` 通用 CRUD + `kubectl-raw` 逃生门 | 平台内置 | 零登记 |
| atomic 薄覆盖 | 绑定某 CRD，只补语义 / 安全，不碰字段 | 模块可选 | 几行 YAML |
| domain | 领域知识（`uses[]` 编排 + 指令 + 脚本） | 模块必须 | 指令 / Skill |

## A.2 atomic 薄覆盖：override + target，不碰字段

`type: atomic` 的 Capability 只覆盖两个维度：`semantics`（何时用 / 用户话怎么映射）与
`security`（deny / confirm），不定义字段——`parameters` 永远来自目标 CRD 的 OpenAPI schema + 平台注入。

- `override: true` 标记这是覆盖层，不是全新定义；
- `target`（group / version / kind）登记时平台校验 CRD 存在 + schema 可用，fail-fast；
- 执行仍走通用执行器：`resource-manager{kind, action, data}` 查 schema → 校验 data → 渲染 manifest → exec kubectl。

## A.3 加载策略（缓解工具爆炸）

```text
① group 分片：Agent 声明 groups + RBAC 过滤，上下文只放需要的模块
② 操作分片：读（get / list）常驻；写（create / delete / update）按需加载 + 默认确认
③ 发现兜底：常驻 list-kinds / describe-kind / resource-manager，长尾 CRD 动态发现即用
```

## A.4 两条腿：结构化 vs 逃生门

| | 结构化（默认） | kubectl-raw（逃生门，默认关闭） |
|---|---|---|
| 参数 | `resource-manager` 的 kind + data（schema 校验） | `command` 自由命令串 |
| 覆盖 | 任意 CRD 的 CRUD + 常见读 | 长尾（api-resources / explain / 子资源） |
| 安全 | schema 校验 + RBAC；写默认确认 | token 化 argv + verb/flag 白名单直 exec，歧义 fail-closed |

---

# 附录 B · 安全清单

正文 §6 是当前阶段（用户身份）的规范安全设计；下表为阶段二/三相关完整清单，作为演进检查表。

| 维度 | 设计 | 阶段 |
|---|---|---|
| 身份与授权 | Keycloak OIDC 鉴权；工具资源归属复用平台 RBAC；Agent 定义/实例 owner 与身份派生关系可审计 | 一（OIDC）/ 二（Agent 维度） |
| 凭据最小化 | 实例仅持声明且经授权的类型化凭据；定期轮换、失效即时吊销；禁止集群管理员凭据 | 一 |
| 物理隔离 | 一实例一 Pod 一 PVC；内置与自建 Agent 数据互相隔离 | 一 |
| Prompt 注入防护 | 用户输入与系统指令区分；工具返回作数据不作指令；高危操作须确认 | 一 |
| 实例最小权限 | 非 root、seccomp RuntimeDefault、drop ALL capabilities、readOnlyRootFilesystem、egress 白名单 | 一 |
| 确认护栏 | 写/高风险操作 HITL，本人确认；拒绝/超时 fail-closed；不重试被拒操作 | 二 |
| Agent 配额 | 每用户 Agent 数 + 全平台实例数上限；超限拒绝创建 | 三 |
| 自定义 Agent 边界 | 阶段二配置托管（工具来自能力目录）；阶段三代码托管需镜像审核 + 强化沙箱 + 评测 | 二 / 三 |
| 模型凭据 | 平台外端点 API Key 平台托管、不落明文；共享凭据记录使用方审计；egress 白名单 | 二 |
| 限流防滥用 | 按用户 / Agent / 工具 / LLM 维度限速 | 三 |
