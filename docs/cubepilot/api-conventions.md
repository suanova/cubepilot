# CubePilot API 约定（改 API 前先读）

> **读者**：要新增或修改 `cubepilot-api` 端点的人。
> **面向客户端的行为契约**见 [api.md](./api.md)；本文是**写它时要遵守的规则**。
>
> 每条规则后面标注**由什么守住**。标 ✅ 的会让 CI 变红；标 ⚠️ 的只能靠 review——
> 知道哪条没有牙齿，比以为都有牙齿安全。

---

## 1. 版本与路径

**客户端端点一律在 `/api/v1/` 之下。**

版本是冻结点：将来若有破坏性变更，**引入新前缀 `/api/v2/`**，而不是就地改动 v1。

**`/internal/*` 不带版本**——它唯一的消费者是 agent Pod 内的 supervisor，随 agent 镜像与 API
一起发布，不需要独立的兼容承诺。

> **守住**：✅ `TestClientRoutesAreVersioned`（客户端路由漏了前缀就失败）

## 2. 命名：词从哪来

新增一个概念前，先确认它**已经有名字**，不要另起一个：

| 概念来源 | 以谁为准 | 例 |
|---|---|---|
| 平台 CRD 里的字段 | **CRD 的 json tag** | `selectedModel`、`userInstructions`、`approvalPolicy`、`instruction`、`cron` |
| 网关（OpenClaw）协议的概念 | **协议里的词** | `approval`（不是 `confirm`）、`allow-once`/`allow-always`/`deny` |
| 平台自己发明的概念 | 新起，但要说明为什么已有词不够用 | `sessions`、`questions` |

**这条是本仓库最容易踩的**：`confirm` 是 API 层凭空造的词，而协议、数据库表、权限 scope
全都叫 `approval`——同一个东西四个名字，追查问题时每次都要重新建立映射。

> **守住**：⚠️ 靠 review。测试无法判断一个名字「对不对」，只能判断它「格式对不对」（见 §7）。

## 3. 路径：名词还是动词

| 路径在做什么 | 用什么 | 例 |
|---|---|---|
| 读/写一个资源 | **名词** | `GET /api/v1/sessions/{key}/approval/pending` |
| 对它做个动作 | **动词** | `POST /api/v1/sessions/{key}/approval` |

同一个词不要既当名词又当动词：`.../confirm/pending`（读一个资源）与
`POST .../confirm`（做一个动作）指两个不同的东西却共用一个词——现在前者是
`approval/pending`，后者是 `approval`。

> **守住**：⚠️ 靠 review。

## 4. 响应形状

> **返回一个完整的「东西」→ 用具名 key 包起来，key 就是那个东西的名字。**
> **返回某个东西的若干字段 → 不加信封，字段摊平。**
> **转发别人的字节 → 原样透传，不包（包一层就是篡改别人的格式）。**

理由：前两者客户端必须区分（取错层只会得到 `undefined`，界面显示为空而不是报错）；
第三类如果包了，就改写了 OpenClaw 或 OpenAI 的格式。

`GET /api/v1/agent/config` 是「字段」的例子：它返回实例的两个字段，不是名为 `config` 的对象
（CRD 里没有这个概念），所以它**扁平**。

> **守住**：⚠️ 靠 review。形状对不对需要理解语义，测试只能看声明。

## 5. 错误与状态码

**所有错误响应都包含 `error`，且都是 JSON。** 错误体可扩展——个别端点会附加结构化字段
（如删除被选用的模型时 `409` 附带 `instances`），客户端应容忍未知键。

**绝不能让 Go 的纯文本 404 漏出去**：任何状态下同一个状态码只有一种 body 格式。

| 码 | 用于 |
|---|---|
| **201** | 创建成功（不是 200）|
| **200** | 读取、更新、幂等重复创建（`alreadyExists`）|
| **202** | 已登记但未完成（手动触发任务）|
| **400 / 403 / 404 / 409** | 请求问题 / 无权 / 不存在 / 冲突 |
| **502 / 503 / 504** | 后端链路 / 依赖未就绪 / 超时 |

**503 有特定含义**：`instance warming failed` 表示实例正在冷启动，客户端应重试——
不要把它当故障。

> **守住**：✅ `TestUnknownPathAnswersJSON`（未匹配路径必须是 JSON 404）；
> ⚠️ 其余靠 review 与各端点的既有测试。

## 6. 方法语义

| 语义 | 方法 |
|---|---|
| 幂等（重复调用结果一致）| **PUT** |
| 非幂等（触发、翻转）| **POST** |
| 集合成员变更（安装/卸载技能）| **PUT** |

**每个端点都必须检查方法**，错误方法返回 `405` + JSON。

> **守住**：✅ `TestClientRoutesRejectUnsupportedMethods`（任何端点对不支持的
> 方法返回 2xx 就失败）

## 7. 字段大小写

| 层 | 规则 | 例 |
|---|---|---|
| **JSON 字段** | lowerCamelCase，`Id` 不写成 `ID` | `sessionId`、`callId`、`toolCallId` |
| **Go 标识符** | Go 的 initialism 规则，`ID` 全大写 | `SessionID`、`CallID` |

两层规则不同**且都对**——JSON 侧跟随网关协议（OpenClaw 自己就用 `agentId`、`runId`）。

**例外**：`internal/openclaw/events.go` 镜像 OpenAI 兼容协议（`tool_calls`、`finish_reason`），
那些是**那个协议的**名字，不要「纠正」。

> **守住**：✅ `TestWireJSONTagsAreCamelCase`（扫描 `internal/server` 与 `internal/runtime`
> 的 json tag，出现下划线即失败）

## 8. 请求体必须严格

**每一个接受 body 的 handler 都要走 `decodeJSONBody`**（`internal/server/handlers.go`），
它打开 `DisallowUnknownFields`。

```go
var body x
if !decodeJSONBody(w, r, &body) {
    return   // 400 已经写好了
}
```

**为什么**：`encoding/json` 遇到不认识的键**不报错，直接丢掉**，对应字段保持零值。
于是打错一个字母不会被发现，而是按默认值办：

| 场景 | 后果 |
|---|---|
| `{"selectedModell": "..."}` | 模型被清空，返回 **200** |
| `{"name":..., "instruction":..., "cronn": "0 3 * * *"}` | 任务建成 **Manual**，返回 **201** |
| `{"...", "enabled": false}`（旧字段）| 任务建成 **Enabled** |

**这类错误全部是静默的**——调用方拿到成功状态码，资源却和他要求的不一样。

**严格性是免费的**：唯一的代价是前向兼容（新客户端发新字段给旧服务端）。
v1 未发布、**没有需要前向兼容的客户端**，所以这个代价为零；
将来若真需要，那是引入 `/api/v2/` 的场景，不是放宽 v1 的理由。

> **守住**：✅ `TestNoUnstrictBodyDecode`（源码里出现裸 `json.NewDecoder(r.Body)` 即失败）、
> ✅ `TestWriteEndpointsRejectUnknownFields`（行为验证：未知字段必须 400）

---

## 改动 API 的清单

新增或修改端点时，按顺序过一遍：

1. **词从哪来**（§2）—— 已有名字就用已有的
2. **名词还是动词**（§3）
3. **形状**（§4）—— 一个东西 / 若干字段 / 字节透传
4. **状态码**（§5）—— 创建是 201
5. **方法 + 方法检查**（§6）
6. **字段大小写**（§7）
7. **请求体严格**（§8）—— 用 `decodeJSONBody`，不要自己 `Decode`
8. **更新 `api.md`** —— 有测试守着，忘了会红
9. **跑测试** —— 上面标 ✅ 的会自动告诉你漏了哪条

## 与文档的一致性

`api.md` 描述的是**当前契约**，`internal/server/apidoc_test.go` 两个方向都守：
路由必须写进文档，文档里也不能留下已删除的路径。改了路由却不改文档，CI 会失败并点名。

带日期的设计与计划（`docs/superpowers/specs/`、`docs/superpowers/plans/`，以及
`docs/notes/`）是**历史快照**，不回填——它们记录的是当时的决定。
