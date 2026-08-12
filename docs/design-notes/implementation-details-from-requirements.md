# 从需求文档移出的实现细节（供重写设计文档参考）

**来源文档：** [CubePilot 功能需求细化文档 v0.5](../CubeStack-平台智能助手-CubePilot-功能需求细化文档.md)
**目的：** 以下条目描述的是实现层面（HOW）而非功能需求（WHAT），已从需求文档移除或改写为功能语言。保留于此供后续重写设计文档时参考，避免实现意图丢失。

---

## 1. 对话域（M1）

### FR-M1-008（原）— 知识注入 hook

原内容：
> 知识注入 hook 预留（返回空，供 RAG 接入）　阶段：二
> 验收要点：接口存在且不干扰对话

**移出原因**：纯粹是代码扩展点机制（hook + 返回空）。功能需求已由 RAG 知识库问答覆盖。扩展点规划保留在需求文档 §5.3。

**设计参考**：系统提示词组装流程中预留一个注入点，阶段一返回空，阶段二接入 RAG 检索结果。实现时需保证注入内容不影响系统指令的优先级。

### FR-M1-003 中的 SSE 协议选择

原验收要点表述中"SSE 流式消息"改为"流式消息推送"。SSE 是传输协议的具体选择——对需求文档而言，用户需要的是流式推送，协议是实现决策。

**设计参考**：当前选择 SSE（Server-Sent Events），8 类事件：`message_start / agent_thinking / tool_call / tool_result / confirm_pending / confirm_resolved / message_delta / message_done`。

### FR-M1-005 中的系统提示词机制

原表述中"系统提示词 ="改为"上下文组装含四要素"。提示词（system prompt）是 LLM 领域的具体机制——上下文组装策略可能有多种实现。

**设计参考**：当前方案为每次请求动态组装 system prompt：操作者身份 + 工具清单/能力目录 + 确认规则 + 会话历史。

### FR-M1-011（原）中的截断优先级

原表述"系统提示词>最近消息>早期摘要"是实现策略。若后续采用滑动窗口、分层摘要等不同策略，优先级会变化。

**设计参考**：当前截断优先级为系统提示词 > 最近消息 > 早期摘要。

---

## 2. Agent 域（M2）

### FR-M2-001（原）— Agent 实例 + 数据目录

原内容：
> 每操作者 Agent 实例 + 独立数据目录（多用户体系就绪后为每用户，物理隔离）

**移出原因**："每用户一个实例 Pod + 数据目录"是实现方案。功能需求是物理隔离——也可通过其他机制（如强多租户 Agent 进程、命名空间隔离等）达成。

**设计参考**：当前方案为一用户一个 OpenClaw 实例 Pod + 独立数据目录（HERMES_HOME），进程 + 存储物理隔离。

### FR-M2-002（原）— Instance Manager 具体机制

原内容：
> Instance Manager：按需拉起/闲置回收（默认 30min 可配）/异常重建/单主 Leader Election
> 验收要点：按需启停、回收、重建

**移出原因**：Instance Manager 组件名、Leader Election 等是架构设计概念。功能需求是实例按需可用、闲置回收、异常自愈——不规定由哪个组件实现。

**设计参考**：当前方案为 Instance Manager 组件（单主 + Leader Election），首条消息触发拉起，闲置 30min 回收，异常自动重建，重试耗尽后告警。

### FR-M2-004（原）— 状态恢复介质

原表述"状态从 DB/数据目录恢复"中，DB 和数据目录是具体存储介质。

**设计参考**：实例无状态化，状态持久化于 PostgreSQL（会话/消息/审计）和共享存储（实例记忆/数据目录）。

### FR-M2-009（原）— Agent Runtime Adapter 接口预留

原内容：
> Agent Runtime Adapter 接口预留（窄接口，可切其他运行时）
> 验收要点：换运行时只替换适配器

**移出原因**：Adapter 模式、窄接口等是架构设计模式。功能需求是运行时不锁定单一实现——不规定如何达成。

**设计参考**：预留窄接口 Adapter（进入 = 消息 + 上下文 + 工具清单；返回 = 事件流），隔离 OpenClaw API，后续可平滑切换到自研运行时。

---

## 3. 工具域（M3）

### FR-M3-001（原）— kubectl 直连 + kubeconfig 注入

原内容：
> kubectl 直连：实例注入用户 kubeconfig，Agent 以用户身份操作 K8s（含全部平台 CRD）；`get/list/logs` 与 `apply/patch/delete/scale/exec` 均直放；高危命令确认由 FR-M3-002/003 承接；权限由 API Server RBAC 强制

**移出原因**：kubectl 直连、kubeconfig Secret 注入是实现手段。功能需求是 Agent 以用户身份操作平台资源、权限由 RBAC 强制执行。未来可能换成 client-go SDK、MCP K8s client 等。

**设计参考**：当前方案为实例挂载用户 kubeconfig（Secret，0600），等效用户自己的 kubectl；只读命令（get/list/watch/logs）直放，写命令（apply/patch/scale/delete/exec）命中确认规则后经 HITL 执行；权限由 K8s API Server RBAC 强制，操作记入 K8s Audit Log。

### FR-M3-004（原）— kubectl --dry-run

原表述"复用 kubectl --dry-run"是实现手段。功能需求是执行前预览变更。

**设计参考**：当前复用 `kubectl --dry-run=server` 生成影响预览，在确认弹窗中展示变更范围。

### FR-M3-006（原）— 能力目录中的 kubectl 示例

原表述中将"kubectl 调用示例"作为能力目录必含项。能力目录是功能概念，但强制包含 kubectl 示例绑定了实现方式。

**设计参考**：能力目录每项含用途、关键 spec 字段、调用示例（阶段一为 kubectl 命令示例，后续可扩展为 API/CLI 示例）。

---

## 4. 巡检域（M4）

### FR-M4-003（原）— InspectionRun CRD

原表述"InspectionRun CRD 持久化"中，CRD 是 K8s 特定机制。

**设计参考**：巡检报告以 InspectionRun CRD（`assistant.suanova.io/v1alpha1`）持久化，含 spec（scope/items/schedule/trigger）和 status（phase/items/summary/conditions）。

### FR-M4-006（原）— 巡检 Agent 持有只读 kubectl

原表述"巡检 Agent 持有只读 kubectl"是实现方式。功能需求是 AI 巡检能自主探索集群。

**设计参考**：巡检 Agent 持有只读 kubeconfig（最小权限），自主执行 kubectl 查询探索集群状态，发现预置巡检项外的异常。

---

## 5. 审计域（M5）

### FR-M5-001（原）— tool_call_record 单表

原表述"工具调用/确认单表（tool_call_record：user/conversation/tool/args/level/status/confirm/result）"中包含表名和字段定义——是数据模型设计。

**设计参考**：`tool_call_record` 表字段：`user_id / conversation_id / message_id / tool / args / level(L0|L1) / status(pending|executed|denied|failed|timeout) / confirm(json) / result(json) / created_at`。

### FR-M5-003（原）— 异步写 + 失败重试

原表述"审计写入与主链路解耦（异步写、失败重试）"中，异步写和重试是实现策略。

**设计参考**：审计写入通过消息队列/异步任务与对话主链路解耦，失败后指数退避重试（最多 3 次），审计故障不阻塞或影响对话响应。

---

## 6. 平台集成（M6）

### FR-M6-001（原）— kubeconfig 凭据管理细节

原表述"kubeconfig 凭据管理：最小权限生成/注入(0600)/轮换/吊销；禁管理员凭据"中，kubeconfig 格式、0600 权限位、轮换机制是实现细节。

**设计参考**：用户 kubeconfig 以 Secret 挂载（0600），RBAC 最小权限生成，支持定期轮换和即时吊销，禁止使用 cluster-admin 凭据。

### FR-M6-003（原）— LLM 部署形态

原表述"助手 LLM 服务：独立推理池（InferenceService）、HPA、私有化"中，InferenceService（CRD 名称）和 HPA 是 K8s 部署机制。

**设计参考**：助手 LLM 以平台 InferenceService CRD 部署，独立推理池（与用户推理隔离），HPA 按 GPU 利用率/Metrics 扩缩，完全内网可达。

---

## 7. 安全 NFR

### NFR-001（原）— Keycloak OIDC 鉴权

原表述"Keycloak OIDC 鉴权"是具体产品选型。

**设计参考**：当前统一鉴权方案为 Keycloak OIDC，平台所有模块复用；助手服务从 Token 解析 user_id/tenant_id/project_id/role。

### NFR-004（原）— 实例加固的 K8s 配置

原表述"runAsNonRoot/seccomp/drop caps/readOnlyRootFilesystem/egress 白名单"是 K8s Pod SecurityContext 的具体字段。

**设计参考**：Agent 实例 Pod 安全配置：
- `securityContext.runAsNonRoot: true`
- `seccompProfile: RuntimeDefault`
- `capabilities.drop: [ALL]`
- `readOnlyRootFilesystem: true`
- NetworkPolicy egress 白名单（仅 K8s API Server / MCP 工具下游 / LLM 推理服务）

### NFR-005（原）— PSA + Kyverno

原表述"PSA restricted + Kyverno 基线"是具体 K8s 策略工具选型。

**设计参考**：用户命名空间启用 Pod Security Admission（restricted 级别）+ Kyverno ClusterPolicy 基线，禁止 privileged containers / hostPath / hostNetwork / hostPID 等。

---

## 8. 对接机制（§8.3）的三种实现方式

原 §8.3 中「模块自带」的三种实现方式（A/B/C）属于实现策略指导，不属于功能需求。功能层面只需明确"模块自带"与"基座提供"的分工边界。

**设计参考**（保留三种实现方式以供后续设计）：
- **A. 能力 API 化**：模块封装领域逻辑为 API → 登记能力目录 → Agent 工具化调用
- **B. 领域 Skill**：模块打包查询工具 + 领域提示词 → 基座场景化加载
- **C. 数据开放 + 基座推理**：模块暴露数据/API → 基座 LLM 直接分析（关键诊断场景倾向 A/B）
