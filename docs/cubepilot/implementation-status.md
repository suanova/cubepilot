# CubePilot 实现状态与设计对比（阶段一落地记录）

> 本文记录 CubePilot 简化设计在阶段一实现中的实际状态：已完成项、与设计正文的有意偏差、以及后续阶段的演进清单。实现仓库：cubePilot（operator / api / web / agent supervisor）。设计正文见 [cubepilot-design.md](./cubepilot-design.md)。
>
> ⚠️ **本文为 2026-08 评审前的实现记录**；评审后设计方向已调整（删 Model CRD、skill 走镜像文件目录而非 CRD 内联、简单 HITL、可观测性验收后置、不做 trajectory），本文与设计正文的偏差待实现同步后更新。

## 已对齐（阶段一已实现并验证）

- **对象模型**：Agent / AgentInstance / Model / Capability / TaskTemplate / Task / TaskRun
  全部 CRD 化，status subresource + printcolumn；模板、实例、执行三态分离（设计 §3.5）。
- **实例自服务**：`POST /api/instances` owner 强制 = 请求者，幂等创建，冲突 409
  （设计 §3.2）。
- **模型目录闭环**：`Model` CRD + 探测控制器；platform 无 endpoint 直通 `Available`、
  有 endpoint 统一探测；external 必填；fail-closed（设计 §3.3）。
- **模型选择 fail-closed**：`ResolvedAgentConfig` 解析链
  `instance.selectedModel → agent.defaultModel → availableModels 白名单 → Model 目录`，
  任一步失败即报错，绝不静默回退；`x-openclaw-model` 头每请求热生效（优于设计文档的
  updateConfig / pod 重启）。
- **模板与执行分离**：TaskRun 记录 `templateRevision` / `capabilityRevision`（内容
  sha256 前 12 hex）；手动 run 走 annotation 触发，幂等。
- **Task 状态为字符串枚举**：`spec.state: Enabled | Paused`（设计 §3.5 明确不用 bool），
  CRD default=Enabled；API 保留 `enabled` 兼容字段，web 以派生 `enabled` 展示。
- **实例能力子集**：`AgentInstance.spec.enabledCapabilities` 限定 Domain 能力子集
  （设计 §3.2），resolver 过滤注入；空 = 全部声明能力。
- **用户指令接线**：`AgentInstance.spec.userInstructions` 追加到模板指令之后
  （设计 §3.2 组合顺序），resolver 合并进 `ResolvedAgentConfig.Instructions`。
- **实例状态阶段**：`status.phase = Ready`（设计 §3.2，与文档一致；旧 `Warm` 已废弃）。
- **字段命名对齐**：Agent `spec.capabilities`（设计 §3.1）、`spec.runtime: OpenClaw`
  （设计 §3.1）、Model `provider: Platform | External`（设计 §3.3）。
- **枚举值 CRD 校验**：六个枚举（runtime / provider / type / confirmPolicy /
  trigger / task state）均带 `kubebuilder:validation:Enum`，非法值被 API server
  拒绝（fail-fast，实测验证）。
- **统一事件契约**：message_start / delta / tool_call / tool_result / message_done /
  confirm_* 全套实现（设计 §4）。
- **Runtime 窄接口**：`AgentRuntime` Go interface（SetModel / StreamChat / ListSessions /
  GetHistory），concrete client 实现，编译期断言（设计 §4）。
- **Pod 安全基线**：非 root、seccomp RuntimeDefault、drop ALL、禁特权提升、
  readOnlyRootFilesystem、emptyDir /tmp（设计 §6 / 附录B「实例最小权限」条目）。
- **观测**：healthz / readyz / metrics / readiness 全绿（设计 §8.1）。
- **Skill 热重载**：OpenClaw（2026.7.1-2）对 `workspace/skills` 目录做文件监听（chokidar + 100ms 轮询兜底），Capability 变更经 resolver → supervisor 重写 SKILL.md 后自动热加载，无需重启 Pod（此前「skill 变更必须重启 Pod」的旧结论已随版本更新修正）。

## 已知取舍（现实现与设计文字的有意偏差，已选其一，不再当作缺口）

1. **MCP Gateway（设计 §5，ToolExecutor 接口的正式实现）阶段一未建**：kubectl 由 OpenClaw 直接 exec（挂用户 kubeconfig，RBAC 免底，无执行前校验/HITL）；审计由 API 从 SSE 流捕获 tool_call 事后记录，只能记录、不能阻断。这是明确接受的临时缺口；MCP Gateway 作为阶段二目标，落地时切换到受控执行，中间不建过渡组件。
2. **存储不采用 PostgreSQL / Redis（设计 §2 mermaid）**：实现为 CRD/对象存储 + 每实例
   RWO PVC，与设计 §3.6 文字「CRD/控制面数据库」一致。§2 图为历史参考，以 §3.6 为准。
3. **TaskRun 不冗余记录 Agent 实例名（设计 §7「至少记录」）**：实现按设计 §3.5 从 owner 推导
   （阶段一单实例每用户，推导无歧义）。多实例每用户时改为显式记录。
4. **身份用 `X-CubePilot-User` 请求头模拟（附录 B 标「一」）**：OIDC 属外部依赖，
   阶段二引入 Keycloak 后替换；模拟身份可审计、单一来源。
5. **设计 §5.3 双 kubeconfig 未实施**：现阶段 Pod 只挂一个用户 kubeconfig（操作与读 schema 同源，`kubectl explain/get crd` 以用户权限完成）；「用户无 CRD 读权限时挂只读 CRD kubeconfig」的场景登记阶段二，随 MCP Gateway / 凭据治理落地。
6. **egress 白名单未实施**：与模型凭据/外部端点管控绑定（表B「模型凭据」行），阶段二随
   模型凭据统一治理落地。

## 阶段二演进清单（已知偏差的后续归属）

- 集中 Tool Gateway（设计 §5 ToolExecutor 独立组件 + 统一 Policy + HITL）。
- Keycloak OIDC 鉴权替换 `X-CubePilot-User`（附录 B）。
- 模型凭据托管、轮换与 egress 白名单（附录 B）。
- 确认护栏落地：写/高危操作 HITL（确认策略已进入对象模型，执行侧未接入）。
- 多实例/多 agent 形态，TaskRun 显式记录 Agent。
