# CubePilot 实现状态与设计对比（阶段一落地记录）

> 本文记录 CubePilot 简化设计在阶段一实现中的实际状态：已完成项、与设计正文(git 内的简体「cubepilot 简化设计」当前版)的有意偏差、以及后续演进清单。实现仓库：cubePilot（operator / api / web / agent supervisor）。设计正文见 [cubepilot-design.md](./cubepilot-design.md)。
>
> 状态：已按当前代码与当前设计重新核对（2026-08-27）。本次更新：**网关配置改为声明式** —— `CUBEPILOT_MODEL_PROVIDERS` 退役，operator 从 AgentTemplate models + 凭据 Secret 生成 `openclaw-config`；`TemplateModelSpec` 简化为 `{name, endpoint, credentialRef?}`。

## 已对齐（一期已实现并验证）

- **AgentTemplate 与实例分离**：AgentTemplate（`cubepilot` 内置）+ AgentInstance（每用户）分离；实例引用模板名（`templateRef`，不钉版）；内置实例由 operator 启动时按 bootstrap 名单自动创建（设计 §3.1/§3.2）。**已对齐设计：Agent→AgentTemplate 重命名完成。**
- **模型内联（无独立 Model CRD）**：模型清单内联在 `AgentTemplate.spec.models`（每项 name + endpoint + credentialRef），`defaultModel` 从 models 里选默认，`AgentInstance.selectedModel` 覆盖。**已对齐设计 §3.3：Model CRD + ModelReconciler + `/api/models` 已删除；`provider`/`modelId` 字段已删除。**
- **声明式网关配置**：`OpenClawConfigReconciler` 从 AgentTemplate models + 凭据 Secret 渲染 `openclaw-config`（providers + allowlist + primary），网关 token 由 cubepilot 生成一次并持久化；`CUBEPILOT_MODEL_PROVIDERS` 与 `deploy/openclaw-config.jq` 退役。`POST /api/llms` + Portal「LLM 配置」可追加模型（name + endpoint，凭据二选一：apiKey 或 `public: true`），`PUT`/`DELETE /api/llms/{name}` 可编辑与删除（endpoint 归一化为 API root，删除同步清理凭据 Secret），operator 自动接入网关。
- **实例自服务**：`POST /api/instances` owner 强制 = 请求者，幂等创建，冲突 409（设计 §3.2）。请求体使用 `templateRef`（非旧 `agentRef`）。
- **模型选择 fail-closed**：`ResolvedAgentConfig` 解析链 `instance.selectedModel -> template.defaultModel -> template.models 内联清单`，selectedModel 不在 models 列表即报错，绝不静默回退；Portal WS 交互回合在 `sessions.send` 前通过 `sessions.patch {model}` 设置 session override（Runtime Default 显式发 `null` 清除旧 override），HTTP one-shot 回合通过 `x-openclaw-model` 头热生效。
- **实例能力 / 指令子集**：`AgentInstance.spec.enabledSkills` 限定 skill 子集（**已对齐设计字段名**），`spec.userInstructions` 追加到指令之后；resolver 合并进 `ResolvedAgentConfig`（设计 §3.2 组合顺序）。
- **System Prompt 运行时送达（issue #137）**：WS-only chat 移除旧 `role=system` 注入后，Agent Config 的 System Prompt 已随 #143 改存 `AgentInstance.spec.userInstructions`（**每用户**，全局 `store.AgentConfig` 整体随 #143 移除），但 runtime 送达之前缺失 -- supervisor 每 poll 把 `ResolvedAgentConfig.Instructions`（模板指令 + 用户指令合成）渲染进实例 `workspace/AGENTS.md` 的 `<!-- cubepilot:system-prompt:start/end -->` 管理段（marker 外内容原样保留，原子写 + 内容哈希去重），OpenClaw 每回合重读该文件即下一回合生效；Agent Config 保存时拒绝超过 20,000 字符或包含保留 marker 的用户指令，supervisor 对绕过公开 API 的无效模板指令仍保持 last-good；Pod 重建后 seed initContainer 会冲掉 AGENTS.md，supervisor 首轮 poll 自动重写收敛。
- **实例状态阶段**：`status.phase` 为 Creating/Ready/Failed（Ready 为稳态，设计 §3.2）；原设计的 Idle/Reclaiming 相位与 idle-reclaim 配置面未实现，已随 issue #134 移除（实例常驻 resident，见「已确认的有意取舍」）。
- **模板与执行分离**：TaskTemplate / Task / TaskRun 三态分离；TaskRun 记录 `templateRevision` / `skillRevision`（内容 sha256 前 12 hex）；手动 run 走 annotation 触发，幂等。
- **Task 状态字符串枚举**：`spec.state: Enabled | Paused`（自定义 bool），CRD default=Enabled。
- **枚举值 CRD 校验**：六种枚举（runtime / provider / skill type / confirmPolicy / trigger / task state）均带 `kubebuilder:validation:Enum`。
- **统一事件契约**：message_start / agent_thinking / message_delta / text_replace / tool_call / tool_result / confirm_pending / confirm_resolved / message_done 已实现（设计 §4）。
- **Runtime 契约与传输边界**：平台级 `internal/runtime.AgentRuntime` 组合 `LiveTurnRunner` / `OneShotRunner` / `SessionReader`，新增 runtime 只需实现这一完整语义契约；OpenClaw adapter 内部组合 WS 与 HTTP 实现。Portal 交互聊天与 HITL 使用 gateway protocol WS，确保文本、工具、确认、终态位于同一有序实时通道；定时任务和同步巡检走 OpenAI-compatible HTTP/SSE；session 列表与历史走 HTTP。只读、一次性调用不为“统一”而迁移到 WS。
- **能力目录 + Skill 落盘**：能力分层（generic / domain），Skill CRD 登记；supervisor 把启用能力以 `SKILL.md` 渲染到实例 `workspace/skills/`；OpenClaw 文件监听热重载。
- **Pod 安全基线**：非 root、seccomp RuntimeDefault、drop ALL、禁特权提升、readOnlyRootFS、emptyDir /tmp（设计 §6）。
- **观测**：healthz / readyz / metrics / readiness 全绿。
- **数据真源**：AgentTemplate / AgentInstance / Skill / TaskTemplate / Task / TaskRun 走 CRD（group `ai.cubestack.io`）；会话/记忆/运行时缓存走实例 PVC（设计 §3.6）。**API 不再保留会话消息副本**：曾实现的平台侧 message ledger（`store.Message`/`messages.json` + `/ledger`）与跨 runtime 回放 `/seed` 已随 issue #140 移除——对话历史仅存于 runtime PVC，历史渲染走运行时 `/messages`，跨引擎换 runtime（阶段一外）可接受丢近期对话。


## 与当前设计的剩余结构性差异

> 以下为设计已要求、但实现尚未同步的项。**请不要把设计文档改回旧版来迁就实现；实现应逐步对齐本节。**

1. **技能市场（CRD + 仓库 + 发布/安装流全部就绪，phase 1 完成）**。
   新设计 §3.4：能力分两层，skill 为多文件目录（SKILL.md + scripts/references），经「技能市场」发布/安装（`Skill` CRD 登记 + 共享文件卷 tar 包 + sha256 校验；对象存储 S3 源属阶段二），`AgentTemplate.skills` 声明默认、实例 `enabledSkills` 用户子集。代码现状（2026-09-01，issue #22/#23/#24）：**Skill CRD 已切换为 marketplace schema**（displayName/visibility/source(type/path/sha256) + status.phase，CEL 校验 source 判别字段）；**共享文件卷仓库已建 + API 独占**（supervisor 经 `GET /internal/skills/{name}/tar` 拉取解压到 PVC，OpenClaw 热重载；内置预设由 API 启动时经 `publishSkill` 自 seed，`cubepilot/publisher=system`）；**发布流已通（#23）**：Portal「Publish」页选技能目录 → 前端打包 gzip tar → `POST /api/skills/{name}/publish`（强制 `visibility=Platform`，记 `cubepilot/publisher` 注解，校验根目录 `SKILL.md` + 超 10MB 拒收，原子写版本化 tar + 建/更新 Skill CRD）；**安装流已通（#24）**：安装统一走 **Agent Config → Skills** 开关 → `POST /api/skills/{name}/install|uninstall` 改当前实例 `enabledSkills`（空集 = 全开基线，首次卸载物化 allow-list，未开通实例 409，owner 不符 403）；内置 kubectl-platform 锁定展示于 System 区。→ 剩余：对象存储 S3 源、用户私有技能（`visibility: User`）属阶段二。

2. **简单 HITL 与 WS-only 交互聊天已实现（issue #20/#130）**。
   设计 §5 一期写操作确认。写前拦截点在 gateway 内部，故走 OpenClaw **原生 exec 审批**，由平台的 **gateway-protocol WS 设备客户端**（Ed25519 device，operator scopes）驱动：Portal 交互回合由同一 WS 连接执行 `sessions.send` 并订阅完整 live stream，把文本、工具和 `exec.approval.requested` 投影为 SSE；Portal 批准/拒绝 -> `POST /api/sessions/{key}/confirm` -> WS `exec.approval.resolve`（同回合恢复/拒绝，写不执行）。仅**交互回合**启用 gate；cron/巡检等 one-shot 回合仍走 HTTP 且不 guard。设备配对由 supervisor 通过 loopback 管理员会话自动批准。-> 端到端已建立；CI kind e2e 的 chat 路径含写 gate（reject 不执行）spec。

3. **每用户身份由平台生成（issue #19 落地，2026-09-03）**。
   设计 §5.3 的双 kubeconfig 已实现并由平台**自动生成**用户身份：operator（builtin bootstrap）为每个 `CUBEPILOT_USERS` 用户创建一个 namespaced `ServiceAccount`（`user-<sanitize>`），ClusterRoleBinding 绑定 **内置 `view`**（cluster 只读、不含 secrets）+ **`cubepilot-user-crds`**（ai.cubestack.io 全量 admin），再用 SA token 渲染 kubeconfig 写入 `<sanitize>-kubeconfig-<32hex>` Secret；agent Pod 默认 `~/.kube/config` 挂它，业务 kubectl 以该用户身份执行。`cubepilot-agent` SA 的 `agent-kubeconfig` 挪到非默认路径 `$CUBEPILOT_PLATFORM_KUBECONFIG`，仅 CRD/kind schema 发现用。helm `agents.kubeconfigs` / setup `--user-kubeconfig` 已移除（不再需要管理员喂）。→ 剩余：SA 角色（现仍通配）收窄另立；动态每用户凭证（`AgentInstance.spec.credentials[target=k8s]` + resolver/supervisor 动态投递）随 #79 动态身份。

4. **agentInstanceRef / 多实例显式记录**——阶段一每用户单实例从 owner 推导，符合设计 §3.5「不写 agentInstanceRef」；阶段二多 Agent 时再加回（现状一致）。

## 已确认的有意取舍（非缺口）

- **MCP Gateway**：阶段一不建（设计 §1.2/§5 阶段二统一执行边界），kubectl 由 OpenClaw 直接 exec。审计由 API 从 SSE 流捕获 tool_call 事后记录。
- **存储**：不用 PostgreSQL/Redis，CRD/对象存储 + 每实例 RWO PVC（设计 §3.6 一致）。
- **身份**：一期用 `X-CubePilot-User` 请求头模拟身份（OIDC 归阶段二 Keycloak）。
- **实例闲置回收（idle reclaim / on-demand）**：不实现。实例一经启动即常驻（resident），不做闲置回收；相关配置面（`CUBEPILOT_RECLAIM` / `CUBEPILOT_IDLE_TTL`、`spec.lifecycle.idleTTLSeconds`、Idle/Reclaiming 相位）已移除（issue #134）。
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
- **issue #9 补全**：内置 `cubepilot` 增加内联 External 模型（Platform + External，设计 §3.1）；`TemplateModelSpec.Validate()` + CEL `XValidation` 拒绝非法 External 组合（§3.3）；新增 AgentTemplate 序列化 / revision / 非法组合单元测试。

## 阶段二/演进清单（设计 §9 / 附录 B）

- 集中 Tool/MCP Gateway（统一执行边界 + 完整 HITL + 审计）。
- Keycloak OIDC 鉴权替换 `X-CubePilot-User`。
- 模型凭据托管、轮换与 egress 白名单；每用户 kubeconfig 动态投递（`AgentInstance.spec.credentials[target=k8s]`，随 #79 动态身份）。
- 技能市场（Path 源 + Platform 可见性 + 发布/安装）是**阶段一**交付项（设计阶段一清单，issue #21/#22/#23/#24）；阶段二仅剩：对象存储 S3 技能源、用户私有技能（`visibility: User`）。
- AgentTemplate/AgentInstance 版本化 Revision、用户自建模板、service 身份。
- 多 Agent/多 Runtime 形态，TaskRun 显式记录 Agent；trajectory / 工具调用索引 / 确认决定。

_注：非代码实现（文档/图表/计划）不在本文范围；设计文档本身未改动。_
