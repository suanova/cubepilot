# CubePilot 实现状态与设计对比（阶段一落地记录）

> 本文记录 CubePilot 简化设计在阶段一实现中的实际状态：已完成项、与设计正文(git 内的简体「cubepilot 简化设计」当前版)的有意偏差、以及后续演进清单。实现仓库：cubePilot（operator / api / web / agent supervisor）。设计正文见 [cubepilot-design.md](./cubepilot-design.md)。
>
> 状态：已按当前代码与当前设计重新核对（2026-08-27）。本次更新：**网关配置改为声明式** —— `CUBEPILOT_MODEL_PROVIDERS` 退役，operator 从 AgentTemplate models + 凭据 Secret 生成 `openclaw-config`；`TemplateModelSpec` 简化为 `{name, endpoint, credentialRef?}`。

## 已对齐（一期已实现并验证）

- **AgentTemplate 与实例分离**：AgentTemplate（`agent-for-cloud` 内置）+ AgentInstance（每用户）分离；实例引用模板名（`templateRef`，不钉版）；内置实例由 operator 启动时按 bootstrap 名单自动创建（设计 §3.1/§3.2）。**已对齐设计：Agent→AgentTemplate 重命名完成。**
- **模型内联（无独立 Model CRD）**：模型清单内联在 `AgentTemplate.spec.models`（每项 name + endpoint + credentialRef），`defaultModel` 从 models 里选默认，`AgentInstance.selectedModel` 覆盖。**已对齐设计 §3.3：Model CRD + ModelReconciler + `/api/models` 已删除；`provider`/`modelId` 字段已删除。**
- **声明式网关配置**：`OpenClawConfigReconciler` 从 AgentTemplate models + 凭据 Secret 渲染 `openclaw-config`（providers + allowlist + primary），网关 token 由 cubepilot 生成一次并持久化；`CUBEPILOT_MODEL_PROVIDERS` 与 `deploy/openclaw-config.jq` 退役。`POST /api/llms` + Portal「LLM 配置」可追加模型（name + endpoint + 可选 apiKey），operator 自动接入网关。
- **实例自服务**：`POST /api/instances` owner 强制 = 请求者，幂等创建，冲突 409（设计 §3.2）。请求体使用 `templateRef`（非旧 `agentRef`）。
- **模型选择 fail-closed**：`ResolvedAgentConfig` 解析链 `instance.selectedModel → template.defaultModel → template.models 内联清单`，selectedModel 不在 models 列表即报错，绝不静默回退；`x-openclaw-model` 头每请求热生效。
- **实例能力 / 指令子集**：`AgentInstance.spec.enabledSkills` 限定 skill 子集（**已对齐设计字段名**），`spec.userInstructions` 追加到指令之后；resolver 合并进 `ResolvedAgentConfig`（设计 §3.2 组合顺序）。
- **实例状态阶段**：`status.phase` 六态（Creating/Ready/Idle/Reclaiming/Failed），Ready 为稳态（设计 §3.2）。
- **模板与执行分离**：TaskTemplate / Task / TaskRun 三态分离；TaskRun 记录 `templateRevision` / `skillRevision`（内容 sha256 前 12 hex）；手动 run 走 annotation 触发，幂等。
- **Task 状态字符串枚举**：`spec.state: Enabled | Paused`（自定义 bool），CRD default=Enabled。
- **枚举值 CRD 校验**：六种枚举（runtime / provider / skill type / confirmPolicy / trigger / task state）均带 `kubebuilder:validation:Enum`。
- **统一事件契约**：message_start / delta / tool_call / tool_result / message_done / error 全套实现（设计 §4）。confirm_* 仅定义、一期不 emit。
- **Runtime 窄接口**：`AgentRuntime` Go interface（SetModel / StreamChat / ListSessions / GetHistory），concrete client 实现。
- **能力目录 + Skill 落盘**：能力分层（generic / domain），Skill CRD 登记；supervisor 把启用能力以 `SKILL.md` 渲染到实例 `workspace/skills/`；OpenClaw 文件监听热重载。
- **Pod 安全基线**：非 root、seccomp RuntimeDefault、drop ALL、禁特权提升、readOnlyRootFS、emptyDir /tmp（设计 §6）。
- **观测**：healthz / readyz / metrics / readiness 全绿。
- **数据真源**：AgentTemplate / AgentInstance / Skill / TaskTemplate / Task / TaskRun 走 CRD（group `ai.cubestack.io`）；会话/记忆/运行时缓存走实例 PVC（设计 §3.6）。

## 与当前设计的剩余结构性差异

> 以下为设计已要求、但实现尚未同步的项。**请不要把设计文档改回旧版来迁就实现；实现应逐步对齐本节。**

1. **技能市场（CRD + 仓库已建；发布 UI 已建，安装 UI 未建）**。
   新设计 §3.4：能力分两层，skill 为多文件目录（SKILL.md + scripts/references），经「技能市场」发布/安装（`Skill` CRD 登记 + 共享文件卷 tar 包 + sha256 校验；对象存储 S3 源属阶段二），`AgentTemplate.skills` 声明默认、实例 `enabledSkills` 用户子集。代码现状（2026-09-01，issue #22/#23）：**Skill CRD 已切换为 marketplace schema**（displayName/visibility/source(type/path/sha256) + status.phase，CEL 校验 source 判别字段）；**共享文件卷仓库已建 + API 独占**（supervisor 经 `GET /internal/skills/{name}/tar` 拉取解压到 PVC；内置预设由 API 启动时经 `publishSkill` 自 seed，`cubepilot/publisher=system`）；**发布流已通（#23）**：Portal「Skills」页选技能目录 → 前端打包 gzip tar → `POST /api/skills/{name}/publish`（强制 `visibility=Platform`，记 `cubepilot/publisher` 注解，校验根目录 `SKILL.md` + 超 10MB 拒收，原子写版本化 tar + 建/更新 Skill CRD）→ 市场立即可装。→ 剩余：**Portal 安装 UI（#24）**、对象存储 S3 源、用户私有技能（`visibility: User`）属阶段二。

2. **简单 HITL ——设计一期要求，代码执行侧未接入**。
   设计 §5：一期写操作**靠命令匹配（动词/资源白名单）命中即暂停确认的简单 HITL**（尽力而为）。代码现状：`confirmPolicy` 字段已进 AgentTemplate 对象模型（内置模板默认 `ConfirmWrites`），`confirm_pending`/`confirm_resolved` 事件已在合约定义，但**执行侧（OpenClaw exec kubectl 前）未接入命令匹配与暂停确认**。→ HITL 为设计一期交付项，实现侧缺口。

3. **双 kubeconfig —— 设计 §5.3 要求，实现未做**。
   设计 §5.1 等：schema 发现走「用户 kubeconfig(操作) + 平台只读 CRD kubeconfig(读 schema)」两把。代码现状：Pod 只挂一个用户 kubeconfig。→ 登记为待完成。

4. **agentInstanceRef / 多实例显式记录**——阶段一每用户单实例从 owner 推导，符合设计 §3.5「不写 agentInstanceRef」；阶段二多 Agent 时再加回（现状一致）。

## 已确认的有意取舍（非缺口）

- **MCP Gateway**：阶段一不建（设计 §1.2/§5 阶段二统一执行边界），kubectl 由 OpenClaw 直接 exec。审计由 API 从 SSE 流捕获 tool_call 事后记录。
- **存储**：不用 PostgreSQL/Redis，CRD/对象存储 + 每实例 RWO PVC（设计 §3.6 一致）。
- **身份**：一期用 `X-CubePilot-User` 请求头模拟身份（OIDC 归阶段二 Keycloak）。
- **可观测性**：验收不强制（设计 §8.1 预留即可）。

## 本次对齐变更清单（2026-08-25）

- **删除 Model CRD**：移除 `model_types.go`、`model_controller.go`、`model_controller_test.go`、`/api/models` 路由、Model CRD YAML、RBAC 中的 models 权限。
- **Agent → AgentTemplate**：`agent_types.go` → `agenttemplate_types.go`，类型 `Agent`/`AgentSpec`/`AgentList` → `AgentTemplate`/`AgentTemplateSpec`/`AgentTemplateList`，`AgentModelSpec` → `TemplateModelSpec`。
- **字段重命名**：`AgentInstanceSpec.AgentRef` → `TemplateRef`，`EnabledCapabilities` → `EnabledSkills`，`AgentSpec.Capabilities` → `AgentTemplateSpec.Skills`，`AgentSpec.Model` → `AgentTemplateSpec.Models`，`AgentSpec.AvailableModels` 删除（内联 models 替代）。
- **内联模型**：`BuiltinModels()` 返回 `[]TemplateModelSpec`（不再创建独立 Model CR）；`BuiltinAgent()` → `BuiltinAgentTemplate()` 返回带内联 `Spec.Models` 的模板。
- **resolver**：`resolveModel` 从 Model CRD 查询改为扫描 `AgentTemplate.Spec.Models` 内联清单。
- **server**：路由 `/api/agents` → `/api/agenttemplates`，删除 `/api/models`，实例创建体 `agentRef` → `templateRef`。
- **CRD YAML**：删除 `assistant.suanova.io_models.yaml`，重命名 `assistant.suanova.io_agents.yaml` → `assistant.suanova.io_agenttemplates.yaml`，更新 `assistant.suanova.io_agentinstances.yaml` 字段。
- **RBAC**：去掉 models 权限，agents → agenttemplates。
- **Web UI**：`AgentView.tsx` 去掉 Model 管理对话框（不再有独立 Model CRD），模型从 AgentTemplate 内联清单展示；`api/index.ts` 路由对齐。

## 本次对齐变更清单（2026-08-27）

- **API group → `ai.cubestack.io`**：所有 CRD 从 `assistant.suanova.io` 迁到设计示例的 `ai.cubestack.io`（groupversion_info、RBAC markers/finalizer、catalog SchemaFor 默认 group、CRD yaml 文件名与内容、chart rbac.yaml、e2e 断言、文档）。
- **Capability → Skill 重命名**：`Capability`/`CapabilitySpec`/`CapabilityList` → `Skill`/`SkillSpec`/`SkillList`，`CapabilityType` → `SkillType`，`internal/capability` → `internal/skill`；`TaskTemplateSpec.Capabilities` → `Skills`，`TaskRunStatus.CapabilityRevision` → `SkillRevision`（设计 §3.5 字段名）；`/api/capabilities` → `/api/skills`；CRD `capabilities` → `skills`；web UI 同步。**保留现有目录登记 schema**（type/title/description/instructions/files），完整技能市场字段（source path/sha256/visibility）与发布/安装流程属阶段一 Skill-market epic（issue #21 / #22 / #23 / #24，Path 源 + Platform 可见性）；仅对象存储 S3 源与用户私有技能（`visibility: User`）属阶段二（设计 §3.4）。
- **删除陈旧 CRD yaml**：`config/crd/bases` 中遗留的 `agents`、`models`（无对应类型）随 controller-gen 重生成删除；CRD 集合收敛为设计六件：`agenttemplates / agentinstances / skills / tasktemplates / tasks / taskruns`。
- **issue #9 补全**：内置 `agent-for-cloud` 增加内联 External 模型（Platform + External，设计 §3.1）；`TemplateModelSpec.Validate()` + CEL `XValidation` 拒绝非法 External 组合（§3.3）；新增 AgentTemplate 序列化 / revision / 非法组合单元测试。

## 阶段二/演进清单（设计 §9 / 附录 B）

- 集中 Tool/MCP Gateway（统一执行边界 + 完整 HITL + 审计）。
- Keycloak OIDC 鉴权替换 `X-CubePilot-User`。
- 模型凭据托管、轮换与 egress 白名单；两把 kubeconfig 补齐。
- 技能市场（Path 源 + Platform 可见性 + 发布/安装）是**阶段一**交付项（设计阶段一清单，issue #21/#22/#23/#24）；阶段二仅剩：对象存储 S3 技能源、用户私有技能（`visibility: User`）。
- AgentTemplate/AgentInstance 版本化 Revision、用户自建模板、service 身份。
- 多 Agent/多 Runtime 形态，TaskRun 显式记录 Agent；trajectory / 工具调用索引 / 确认决定。

_注：非代码实现（文档/图表/计划）不在本文范围；设计文档本身未改动。_
