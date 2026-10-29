# CubePilot 安全模型

> 本文描述**当前实现**的信任边界、授权路径和隔离措施。设计意图见
> [cubepilot-design.md](./cubepilot-design.md) 第 5、6 节，设计与实现的差异及历史记录见
> [implementation-status.md](./implementation-status.md)。代码与本文不一致时以代码为准，
> 并在同一个 PR 里更新本文。
>
> 最后核对：2026-10-29，upstream/main `34b0516`。

## 1. 资产与信任假设

| 资产 | 存放位置 | 保护方式 |
|---|---|---|
| 集群资源（工作负载、平台 CR） | 集群 | Kubernetes RBAC |
| 模型 provider 凭据 | Secret | `credentialRef` 引用；运行时写入 emptyDir 的 `keys.json`，不落 PVC |
| gateway token | Secret | 环境变量注入；网关 `auth: token` |
| 每用户 kubeconfig | Secret | 用户 ServiceAccount 的 token |
| 会话、记忆、workspace | 每实例 PVC | 实例独占 |
| 审计账本 | API 进程的存储卷 | 每用户一个 JSON 文件 |
| 控制面 CRD | kube-apiserver | RBAC + API 的 owner 校验 |

信任假设：

1. **kube-apiserver 的 RBAC 是唯一的最终授权点**，平台不维护第二套权限系统。
2. **agent 进程按不可信处理**：它可能被提示注入，也可能自行改写 workspace 和 skill，
   因此控制点不能依赖 agent 自觉。
3. **集群内网络为半可信**：平台内部端点没有应用层认证。
4. 模型 endpoint 由部署方配置，属于信任边界之外。

## 2. 身份

### 2.1 两个身份

| 身份 | ServiceAccount | 用途 |
|---|---|---|
| 用户身份 | `user-<sanitize>` | 业务 kubectl，例如创建 DevEnvironment、查看 Pod |
| agent Pod 身份 | agent 的 ServiceAccount | supervisor 拉配置、gateway 自身运行、CRD schema 发现 |

设计上的执行身份只有**用户身份**；Pod 身份是平台为了让 agent 自己跑起来而存在的。

### 2.2 用户身份

用户身份按需生成、随实例回收。operator 的 builtin bootstrap 为**每个拥有 AgentInstance
的用户**创建一个 ServiceAccount，并绑定三个 ClusterRole；最后一个实例删除后整套身份被回收
（`internal/controller/builtin.go`）。

| ClusterRole | 权限范围 |
|---|---|
| `view`（Kubernetes 内置） | 命名空间级只读，不含 secrets |
| `cubepilot-cluster-read` | 集群级只读；排除 secrets、`nodes/proxy`、`pods/proxy`、`pods/exec`、`pods/attach` |
| `cubepilot-user-crds` | `ai.cubestack.io` 全量 CRUD，集群范围 |

因此用户身份可以读取集群内原生 API 组与 `ai.cubestack.io` 的资源状态（含 pods/log），
并创建、修改平台 CR；未列出的第三方 API 组不在读取范围内。
**不能写工作负载，也不能读任何 Secret**。

`ai.cubestack.io` 的写权限包含控制面对象（AgentTemplate、Skill、Task 等）。API 路径上有
owner 校验，但用户直接用 kubectl 时不经过这些校验，由 RBAC 决定边界。

身份轮换：kubeconfig Secret 的 resourceVersion 变化会触发 agent Pod 重建，使新凭据生效
（`internal/controller/agentinstance_controller.go`）。

### 2.3 平台发现身份

agent Pod 挂两个 kubeconfig：

| 路径 | 身份 | 用途 |
|---|---|---|
| `~/.kube/config`（默认） | 用户身份 | 所有业务操作 |
| `$CUBEPILOT_PLATFORM_KUBECONFIG` | agent Pod 身份 | CRD / kind schema 发现 |

schema 发现需要读 CRD 定义，而用户身份没有集群级 CRD 读权限，所以单独挂一份平台
kubeconfig。默认约定是它只用于 `kubectl get crd` / `explain`；这是 skill 和提示词层面的
约定，不是 RBAC 约束。agent Pod 身份当前按平台运行需要配置权限，尚未收窄到 schema
发现所需的最小集合。

## 3. 执行与写操作确认

```text
Portal 交互回合
  -> API PreTurn: 解析 approvalPolicy
     -> 审批通道不可用: 回合不启动 (fail closed)
  -> session 标记 guarded
  -> OpenClaw gateway exec
     -> 命中有效 allowlist: 直接执行
     -> 未命中: 暂停, exec.approval.requested
        -> SSE 卡片 -> 用户批准/拒绝 -> exec.approval.resolve
  -> RBAC 在 API Server 做最终判定
```

### 3.1 策略

`approvalPolicy` 定义在 AgentTemplate，实例可覆盖：

| 值 | 行为 |
|---|---|
| `None` | 不确认，读写都放行 |
| `Allowlist`（默认） | 命中 allowlist 自动放行，其余在交互回合询问 |
| `AlwaysAsk` | 交互回合内所有命令都询问 |

策略解析失败或审批通道不可用时，回合**不启动**，不会静默降级为不确认。

### 3.2 有效 allowlist

有效 allowlist 是四者的并集（`internal/allowlist`）：

1. 平台内置的安全命令：kubectl 只读动词（get、describe、logs、top、explain 等），
   以及 `ls`、`cat`、`pwd`、`grep`、`head`、`tail`、`wc`、`jq`、`echo`、`printf`、`which`；
2. 模板声明的条目；
3. 实例手写的条目；
4. 用户在聊天里选择 allow always 后学到的条目。

并集只增不减，所以内置条目无法按实例移除；需要更严格时使用 `AlwaysAsk`。

### 3.3 生效范围

- 确认只在**交互回合**生效。
- 定时任务等 one-shot 回合不设 guarded，执行时不做确认。
- 确认逻辑运行在 agent 自己的 gateway 进程内，不是独立的执行边界。被绕过的 agent
  可以不受其约束地调用 exec。

## 4. 隔离与凭据

每个 AgentInstance 独占一个 Pod 和一个 PVC。一个用户可以拥有多个实例，不同实例之间
不共享 Pod 或 PVC。Pod 的安全基线（`internal/k8s/resources.go`）：

| 项 | 值 |
|---|---|
| 用户 | 非 root（uid/gid 1000） |
| root 文件系统 | 只读；`/tmp` 和缓存目录使用 emptyDir |
| seccomp | `RuntimeDefault` |
| capabilities | drop ALL |
| 提权 | 禁止 |
| 资源 | requests 与 limits 均设置 |

凭据处理：

- 模型凭据通过 `AgentTemplate.spec.providers[].credentialRef` 引用 Secret，由 supervisor
  写入 Pod 内 emptyDir 的 `keys.json`，不进入渲染后的配置和 PVC。
- gateway token 存在 Secret 中，通过环境变量注入。
- 每用户 kubeconfig 是独立的 Secret，作为只读文件挂载。
- 当前没有 egress 白名单或 NetworkPolicy，模型 endpoint 的可达性由集群网络决定。
- 集群内部端点（`/internal/*`）不做应用层认证，只依赖集群网络隔离。

## 5. 审计

- 交互回合中的每次工具调用都从事件流记录下来，按只读（L0）/ 写（L1）分类。
- 审批决定单独记录：批准、拒绝、所属 approval id。
- 审计是**事后记录**，不阻断执行，也不是不可篡改的账本。
- 记录保存在 API 进程的存储卷上，每个用户一个文件。

## 6. 当前边界

以下能力属于设计目标但尚未落地，评估安全性时应按“不存在”计算：

| 能力 | 当前状态 |
|---|---|
| MCP Gateway 统一执行边界 | 未建设；kubectl 由 OpenClaw 直接 exec |
| 完整 HITL（覆盖所有会话类型） | 仅交互回合；one-shot 不确认 |
| 平台发现身份的最小权限 | 使用 agent Pod 身份，权限尚未单独收窄 |
| API 应用层认证（OIDC 等） | 未实现；身份由部署方约定 |
| 用户凭据的动态投递与轮换 | 未实现；靠 Secret 更新触发 Pod 重建 |
| egress 白名单 | 未实现 |
| 审计的完整性与防篡改 | 未实现 |
