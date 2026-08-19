# CubePilot 架构 Review（2026-08-19）

> 范围：`cmd/*` + `internal/*` + `deploy/*` + `config/crd`，对照设计文档
> `CubePilot-Cloud-for-Agents-Design.md` §2.3/§9 与社区最佳实践（controller-runtime / kubebuilder / operator 形态）。
> **状态：2026-08-19 已按本评审执行「拆 2 进程」重构**（cubepilot-operator + cubepilot-api，删 legacy 路径与自研选举），见文末《重构执行记录》。

## 0. 结论速览

- **controller-runtime 用对了**（Manager + SetupWithManager + Owns，kubebuilder 惯例），但工程化不完整：缺 validating webhook、缺 kubebuilder 工程骨架、controller-runtime 标准 metrics 被禁用、存在**双套 leader election 反模式**。
- **最大问题：单进程单 Deployment 合并了 3 类职责**（控制器 / 调度器 / API+UI），与设计文档 §9 自己的部署表（assistant-service ×2 / instance-manager / scheduler 独立）脱节。
- **API 目前不是真正无状态**：store 是 JSON 文件、挂在单个 RWO PVC（`cubepilot-data`）上，多副本物理上不可用。这是拆分的硬前提。
- **leader election 的正确问题不是「有状态吗」，而是「副作用是否幂等」**——纯 reconcile 控制器幂等可不选举；会 fire 任务（非幂等副作用）的调度器必须单活。
- UI 单独组件是长期最佳实践；当前 97KB 单文件 embed 可接受，但应保留独立构建产物。

## 1. 现状盘点（代码事实）

| 关注点 | 现状 | 评价 |
|---|---|---|
| 进程形态 | ~~`cmd/cubepilot` 单进程~~ → `cmd/cubepilot-operator`（3 controllers，无 HTTP/PVC）+ `cmd/cubepilot-api`（REST/SSE + embed UI + JSON store，无控制器） | ✅ 已拆（见文末） |
| 部署形态 | `deploy/service.yaml`：2 Deployment（operator replicas:1 开选举 / api replicas:1 RWO PVC）、2 Service、headless operator svc | 与 §9 部署表对齐；多副本待共享存储 |
| 控制器 | AgentInstance / BuiltinBootstrap / ReconcileScheduler，均走 controller-runtime | 方向正确 |
| 选举 | ~~manager 级 + 自研 `internal/leader` 双轨~~ → 仅 operator 用 controller-runtime `LeaderElection: true`（lease: `cubepilot-operator.suanova.io`）；api 无选举 | ✅ 已统一（见文末） |
| 实例管理 | `instances.Manager` 仅保留 CRD 门面（Ensure/Wait/Status/BaseURL/Touch）；legacy 直管 Pod 路径已删 | ✅ 已删（见文末） |
| 存储 | JSON 文件（tasks/audit/ledger/agent config），单 RWO PVC | 阶段一选型 OK；**阻塞 API 多副本** |
| UI | ~~`ui/index.html`（97KB）go:embed 进二进制~~ → `web/` Vue3+TS+Vite 独立组件（nginx 部署，/api 反代） | ✅ 已拆（见文末 §7） |
| RBAC | 三个 SA 按组件最小化：operator = pod/pvc/svc 写 + lease + CRD 全量；api = CRD 只读；agent = 现状通配 | ✅ 已拆（见文末） |
| metrics | controller-runtime metrics 被 `BindAddress:"0"` 禁用，仅自研 `/metrics` | 丢失标准观测（reconcile 次数/workqueue 深度） |
| webhook | 无 validating/defaulting webhook | 生产建议补 |
| 测试 | 有单测（events/store/leader/cron）；无 envtest 控制器集成测试 | 建议补 |

## 2. 逐问回答

### Q1：控制 CRD 的 controller 是否需要使用 controller-runtime？

**需要，且已经在用**（v0.24.1）。这是社区标准（kubebuilder/operator-sdk 同源），别手写 informer/workqueue。
缺口不是「用不用」，而是工程化补齐：

- 补 **validating webhook**（CRD 校验/默认值，kubebuilder marker 生成）；
- 补 **kubebuilder 工程骨架**（PROJECT / Makefile / kustomize overlay，现在只有 `config/crd/bases` + controller-gen 产物）；
- 恢复 **controller-runtime 标准 metrics**（与自研 `/metrics` 并存），否则丢 reconcile 次数、队列深度等关键观测；
- 控制器级集成测试用 **envtest**（`sigs.k8s.io/controller-runtime/pkg/envtest`）补上。

### Q2：controller 是否应该做成单独 pod？

**应该**。理由：

- 职责与生命周期不同：operator 需要 K8s 写权限 + 常驻 watch；API 只做 HTTP 转发 + 读 CR；
- 独立扩缩容 / 故障域 / RBAC（最小权限：operator 写 Pod/PVC，API 根本不碰 Pod）；
- 设计 §9 本来就是这么写的，代码落后于设计。

目标形态（与 §9 对齐）：

```
cubepilot-operator  控制器（AgentInstance + bootstrap + scheduler），2 副本，leader election on，无 HTTP 端口，无 PVC
cubepilot-api       无状态网关（REST/SSE + embed UI），2+ 副本，无选举，依赖共享存储
cubepilot-ui        静态前端（nginx/CDN），可选——现阶段可继续 embed 在 api
```

### Q3：controller 有状态吗？需要 leader election 吗？

先纠正概念：**controller 本身无状态**（状态在 CR / etcd 里）。leader election 解决的是「单活 / 单写者」，不是「有状态」。判据是**副作用是否幂等**：

| 组件 | 副作用 | 是否需要单活 |
|---|---|---|
| AgentInstanceReconciler | 幂等（ensure*：缺则建、坏则重建） | 不强制；标准 operator 仍开选举，避免重复 status 写与无谓争抢 |
| ReconcileScheduler.fire | **非幂等**（创建 TaskRun + 真跑一轮 agent） | **必须单活**，否则双副本双触发 |

当前实现的正确部分：scheduler 挂在 manager 级选举下（`replicas>1` 时启用）。
错误部分：**自研 `internal/leader` 与 manager 选举双轨并行**（两把 lease）。多副本时可能出现「控制器 leader = Pod A，legacy 循环 leader = Pod B」的分裂。应删除自研 elector，统一用 controller-runtime 选举（`mgr.Elected()` / 标准 `--leader-elect`）。

（可选进阶：cron 触发改用 **claim 语义**——各副本以 resourceVersion CAS 抢占 Task status 的 claim 字段，抢到者执行。比 leader election 更细粒度、故障转移更快，但复杂度高，现阶段单活足够。）

### Q4：给 UI 提供 API 的模块是否无状态？能否独立组件、多副本、无选举？

- **逻辑上无状态**：SSE 转发 + 读 CR + 代理到 agent 实例，无本地会话状态；SSE 长连接也不需要 sticky session（ledger 写在请求路径上，聊天直连 agent Pod）。
- **但当前实现有状态**：store = JSON 文件，挂单个 **RWO** PVC。RWO 卷不能被多个 Pod 同时挂载 → `replicas>1` 直接不可用。
- 结论：独立组件 ✅；**多副本无选举的前提是换共享存储**（阶段二 PG/Redis，`store` 接口已按表结构预留）。PG 落地前：API 保持单副本，或先把 store 换共享介质。
- 无选举 ✅：纯读 CR + HTTP 转发，不需要任何 leader 机制。

### Q5：前端 UI 是否单独组件？

- **最佳实践：是**。独立静态服务（nginx/CDN）：解耦发布、可缓存、独立扩缩、无 CORS。
- 现状：97KB 单文件 `go:embed`，现阶段可接受（少一个组件、单 artifact 交付）。
- 建议路径：现阶段继续 embed，但**前端源码独立成模块 + 独立构建产物**（构建出 index.html 再 embed）；规模上来后抽成 nginx 部署。设计 §9 的 Portal 本来就是独立组件。

## 3. 落地步骤

### 阶段 A（近期，低风险）
1. 拆 cmd：`cmd/cubepilot-operator` + `cmd/cubepilot-api`（共用 internal 库）
   - operator：manager + 3 controllers；scheduler 的 Runner 改为**直连 agent 实例**（`internal/openclaw` 已是独立库，按 AgentKey → Service DNS 组 URL），不再依赖 API 进程；
   - api：HTTP + store + 实例 facade（读 CR 等 Warm）+ embed UI；
2. 删 legacy 路径：`instances.Manager` 的 `ensureLegacy / createResources / waitReady / reconcile / gcDataDirs` 等（CRD 为唯一路径后）；
3. 删 `internal/leader` 自研选举，统一 controller-runtime；
4. deploy 拆 2 个 Deployment + 各自 SA/RBAC：operator = pod/pvc/svc 写 + CRD 读写；api = 只读 CRD、无 pod 权限；
5. store 保持 JSON 但明确单写者边界（api 单副本），或直接上 Redis。

### 阶段 B（随阶段二 PG 落地）
6. api 多副本 + PDB + HPA；
7. kubebuilder 骨架补齐：webhook、kustomize overlay、make targets；
8. 恢复 controller-runtime 标准 metrics + 告警；
9. UI 独立部署（nginx）或继续 embed——明确决策并记录。

## 4. 其他最佳实践检查项

- **scheduler fire 是 at-least-once**：pod 在 fire 中途重启会重复创建 TaskRun（无 claim 机制）。可接受则文档化；不可接受则加 claim。
- **探针**：只有 readiness，无 liveness；controller 卡死无法自愈，建议补 liveness。
- **资源配额**：Deployment 未设 requests/limits，建议补。
- **优雅停机**：httpServer 有 Shutdown；manager 是 ctx cancel 即停（reconcile 中断可接受，标准做法）。
- **Secret**：`CUBEPILOT_GATEWAY_TOKEN` 全实例共享，阶段二改每实例独立凭据。
- **RBAC**：agent SA 通配（文档已承认的当前边界，FR-M3-001 阶段二收紧）。
- **目录分层**：`internal/{api,controller,scheduler,instances,openclaw,server,store}` 依赖方向正确（server→instances→k8s；scheduler→openclaw），拆分时基本不用动结构。

## 5. 结论

架构方向（CRD + controller-runtime + 无状态网关 + 每用户有状态实例池）是行业标准形态（与 Argo / Crossplane 的组件拆分同构）。主要差距两点：

1. **「单进程装所有」的合并**——按 §9 拆成 operator / api / ui 三组件是正确终点；
2. **store 单写者阻塞水平扩展**——是 api 多副本的硬前提，随阶段二 PG 解决。

建议先做「拆 2（operator + api）+ 删 legacy + 统一选举 + 最小 RBAC 拆分」，PG 落地后再补 webhook / PDB / 探针 / UI 独立部署。

---

## 6. 重构执行记录（2026-08-19）

按 §5 建议执行「拆 2 进程」第一轮，已完成：

| 项 | 结果 |
|---|---|
| 进程拆分 | `cmd/cubepilot` → `cmd/cubepilot-operator` + `cmd/cubepilot-api` |
| operator | 3 controllers（AgentInstance / BuiltinBootstrap / ReconcileScheduler），controller-runtime manager，`LeaderElection: true`（lease `cubepilot-operator.suanova.io`），无 HTTP、无 PVC |
| api | REST/SSE + embed UI + JSON store（单副本 RWO PVC）；只读 CRD client，无控制器、无选举 |
| 调度器接线 | 新增 `internal/runner`：operator 内 ReconcileScheduler 经 Runner 直连 agent 实例（instances.Manager → openclaw），不再依赖 API 进程；`scheduler.Runner` 接口实现从 server 移走 |
| 删 legacy | `instances.Manager` 删 ensureLegacy/createResources/waitReady/reconcile/gcDataDirs/execInPod/isCrashLoop/podReady/SetElector/isLeader/Run，仅保留 CRD 门面（Ensure/Wait/Status/BaseURL/Touch） |
| 统一选举 | 删 `internal/leader`（自研 Lease 选举）；server 的 schedulerLeader gate 一并删除，StartLegacyScheduler 不再需要 leader 判断（api 单副本） |
| 日志适配器 | `cmd/cubepilot/logr_adapter.go` → `internal/logrlog`（operator/api 共用） |
| deploy | 拆 `cubepilot-operator` + `cubepilot-api` 两个 Deployment；Service `cubepilot` 指向 api；operator 加 headless Service |
| RBAC | 三 SA 按组件最小化：`cubepilot-operator`（namespace pod/pvc/svc 写 + lease + events + CRD 全量）、`cubepilot-api`（CRD 只读）、`cubepilot-agent`（现状通配） |
| 镜像 | `deploy/service-image.Dockerfile` → `api-image.Dockerfile` + `operator-image.Dockerfile`；`scripts/setup.sh` 构建/加载两镜像 |
| 验证 | `go build ./...` + `go vet ./...` + `go test ./...` 全绿 |

### 遗留（下一轮）

- api 多副本：store 换共享存储（PG，阶段二）后 `replicas>1` + PDB + HPA；api 进程内 StartLegacyScheduler 届时可删（CRD scheduler 已是平台唯一调度器）。
- operator `replicas: 2`（现为 1，`LeaderElection: true` 已就绪，直接升即可）。
- 补 liveness 探针、资源 requests/limits、validating webhook、controller-runtime 标准 metrics（operator 现 `BindAddress:"0"` 禁用，恢复后暴露到 `:8080/metrics`）。
- UI 独立组件（nginx）或继续 embed——决策待定。

---

## 7. 前端独立组件（2026-08-19 追加）

按 §4「UI 独立部署」执行：单文件 `ui/index.html`（97KB go:embed）迁移为独立 Vue 3 + TypeScript + Vite 工程 `web/`。

| 项 | 结果 |
|---|---|
| 工程 | `web/`：Vue 3.5 + TS 5.8 + Vite 6 + Pinia + vue-router（history 模式）；`vue-tsc` 严格类型检查 + 代码分割（按视图懒加载，gzip 后主包 43KB / 各视图 2~6KB） |
| 页面迁移 | 对话（SSE 流式 + 会话历史）/ 定时任务（CRUD + 报告 + 模板）/ 审计（筛选 + CSV 导出）/ Agent 配置（模型 + Skills + 实例状态）——与原功能一一对应 |
| API 层 | `src/api/`：types / client（统一 X-CubePilot-User 头）/ service（13 端点）/ sse（fetch 流解析，替代原手写 parseSSEBlock） |
| 部署 | `web/Dockerfile`（node 多阶段构建 → nginx 1.27-alpine）+ `web/nginx.conf`（SPA fallback + `/api` 反代到 `cubepilot-api` + SSE 关缓冲） |
| 组件 | `deploy/service.yaml` 新增 `cubepilot-web` Deployment（replicas: 2，无状态可扩）+ `cubepilot-web` Service；入口 `cubepilot` Service 改指 web；补回 `cubepilot-api` 独立 Service（nginx upstream 依赖） |
| API 进程 | 移除 go:embed（`ui/` 目录删除）；`/` 路由不再由 API 进程服务 |
| 实测 | 集群部署后：UI 200（422B index.html + 110KB JS）、SPA fallback（/chat 200）、/api 经 nginx 反代正常、SSE 流式对话经 nginx 全链路通（message_start→thinking→delta→done） |

### 遗留

- web 镜像构建依赖外网拉 node/nginx 基础镜像（当前用镜像加速源 `docker.1ms.run` / `dockerproxy.net`）；后续可固化到私有 registry。
- CSP / 安全头、认证接入（当前仍靠 X-CubePilot-User 头）随阶段二身份方案补。
