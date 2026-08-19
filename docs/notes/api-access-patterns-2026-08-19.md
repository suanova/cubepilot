# CubePilot API 访问模式与认证决策记录（2026-08-19）

> 本文记录一次架构讨论的完整脉络与结论：前端如何访问平台能力、API
> 如何认证、以及"把 API 做成 k8s 聚合 apiserver"的可行性分析。
> 决策未落地为代码，作为阶段二规划输入。

## 1. 问题背景

前端（`web/`）与 cubepilot 交互时，哪些请求直接走 k8s api server、哪些必须
经过 cubepilot api？cubepilot api 如何认证？能否复用 k8s 的 RBAC？

## 2. 现状：访问路径全景

**前端 100% 只走 cubepilot api**（经 nginx 反代），从不直连 k8s。k8s api
server 只被三方访问：

| 访问方 | 方式 | RBAC |
|---|---|---|
| api 进程 | controller-runtime client 只读 CRD | `cubepilot-api` SA（只读） |
| operator | 控制器调谐 + leader election | `cubepilot-operator` SA（全量+lease） |
| agent 内 kubectl | 对话中执行 kubectl 命令 | `cubepilot-agent` SA（阶段一通配） |

api 内部数据源三分：

| 数据源 | 端点 |
|---|---|
| JSON store（PVC 文件） | `/api/tasks*`、`/api/audit`、`/api/agent/config`、`/api/sessions*` |
| CRD 只读 | `/api/agents`、`/api/instances`、`/api/capabilities`、`/api/taskruns`、`/api/agent/status`、`/api/kinds` |
| 直连 agent 网关 HTTP | `/api/messages`（SSE 对话，`agent-<user>:18789`） |

完整路径（4 条）：

```
1. 浏览器 → nginx → api → JSON store        （任务/审计/配置/会话历史）
2. 浏览器 → nginx → api → k8s api（CRD 只读）（平台对象/实例状态）
3. 浏览器 → nginx → api → agent 网关(HTTP)   （对话流 SSE）
4. 对话内容 → agent 内 kubectl → k8s api    （实际执行 kubectl）
```

## 3. 认证现状与方案谱系

**现状：无认证。** `userOf()` 直接读 `X-CubePilot-User` header，无校验，
任何人可 curl 冒充任意用户。阶段一内网信任可接受，但这是最大权限隐患。

三层身份分清（缺的只有最外层"人"的认证）：

| 链路 | 身份 | 现状 |
|---|---|---|
| 浏览器 → api | 人 | ❌ header 自报 |
| api → agent 网关 | 服务 | ✅ gatewayToken |
| api / agent → k8s | 服务 | ✅ SA token |

方案谱系（轻 → 重）：

| 方案 | 说明 | 成本 | 定位 |
|---|---|---|---|
| A. 静态共享 token | Bearer 校验，防裸奔 | 半天 | 过渡垫底 |
| B. 按用户 JWT | 签名不可伪造 + `CUBEPILOT_USERS` 白名单 | 1 天 | **先落这个** |
| C. 平台 OIDC 集成 | 校验平台 JWT，用户从 claims 取 | 中 | 推荐方向 |
| D. 网关代认证 | 平台网关注入可信头 | 零代码 | 依赖网关存在 |

**决策倾向：先落 B（按用户 JWT，密钥环境变量注入），等平台 OIDC 出现切 C。**
中间件位置固定（`logRequests` 外包一层），`userOf` 从 header 取改为从 token
解析，改动面小。

## 4. 聚合 apiserver 可行性分析

问题：能否把 cubepilot api 做成 k8s 聚合 apiserver（API Aggregation），
借用 k8s 认证 + RBAC？

### 4.1 机制

```
客户端(kubectl/前端) → kube-apiserver →(APIService 注册)→ 聚合后端
                          │ 认证（用户身份）
                          ├ 鉴权（RBAC rules / nonResourceURL）
                          └ 通过后反代转发，带 X-Remote-User / X-Remote-Group 头
```

metrics-server、prometheus-adapter 同款机制。

### 4.2 借用程度

| 层 | 借用效果 |
|---|---|
| 认证（谁） | ✅ 全借，后端白拿现成身份 |
| 鉴权（粗粒度） | ✅ 资源型 RBAC rules；动作型 nonResourceURL |
| 鉴权（细粒度） | ❌ 自付（"谁能给谁发消息""可见哪些实例"是业务规则） |

### 4.3 三类 API 的塞法（技术上全都能塞）

**① CRD 只读视图 —— 天生适合**
变 `/apis/cubepilot.suanova.io/v1/agents` 等，白嫖 kubectl 直查 + RBAC 按
用户过滤 + watch 实时推送。**这是"前端直连 CRD"路线 B 的标准实现路径。**

**② store 数据（tasks/audit/config）—— 两种塞法**
- 塞法 A：RESTStorage 包 JSON store（实现 Get/List/Watch/Create/Update/
  Delete + uid/resourceVersion/PATCH/分页，协议税全付）
- 塞法 B（更优）：**数据迁 CRD**。Task 本来就是 CRD（operator scheduler
  就是 CRD 驱动），JSON store 是影子副本；迁 CRD 后聚合只需只读透传，
  "换个路径前缀"的事。审计等不适合 CRD 的再单独考虑。

**③ 动作型（POST /api/messages）—— subresource**
k8s 先例：`pods/exec`、`pods/logs`。建模为
`POST /apis/.../agentinstances/{name}/chat`，后端照常调 agent 网关，
返回 SSE，聚合层不解析 body 原样转发。

**④ SSE 流式 —— 两条路**
- 路 1：subresource 流式（如上）
- 路 2：伪装 watch（`?watch=true` 返回 `{"type":"ADDED","object":...}`
  事件流），kubectl 都能看对话流，前端 k8s client 的 watch API 直接复用；
  代价是事件格式包一层壳。

### 4.4 硬代价（躲不掉）

1. **apiserver 扛业务长连接**：SSE 过集群核心组件，默认超时/连接限制，
   对话并发一高 apiserver 成瓶颈——架构退化，这是"不划算"最硬的理由
2. **token 前提**：前端必须持 k8s 用户身份（OIDC/SA token）打 apiserver，
   **无平台 OIDC 聚合了也没人能用**（绕不开的硬前提）
3. **语义扭曲税**：动作硬扮资源、文件硬扮资源，每个新 API 先想"怎么塞
   资源模型"；PATCH/RV/watch 协议全要维护
4. **调试变重**：curl 打 apiserver 带 k8s 协议与认证

### 4.5 结论：混合形态，分阶段

```
阶段一（现在）   : 业务 API（messages/tasks/audit/config）独立服务 + 自有 JWT
阶段二（有 OIDC）: CRD 只读视图挂聚合 apiserver（白嫖 RBAC + watch + kubectl）
                  动作型业务 API 保持独立（apiserver 不是为长连接设计的）
```

- 前端可接受换 k8s client：资源视图用 k8s client，业务 API 用 fetch，各得其所
- 触发条件：**CubeStack 平台层是否有 OIDC/统一身份**（决定路线 B 是否值得）
- 若走全聚合，起点是"tasks/audit/config 迁 CRD"——那一步做了，一半聚合是送的

## 5. 待拍板事项

- [ ] CubeStack 平台层有没有统一身份（OIDC）？有 → 规划聚合 + C 方案；无 → 先 B
- [ ] token 签发方式：部署静态配置 vs 用户自助登录
- [ ] tasks/audit/config 是否迁 CRD（决定聚合的工程量）
