# CubePilot API — Bruno collection

`/api/v1/*` 的可执行接口文档。用 [Bruno](https://www.usebruno.com/) 打开这个目录即可
(集合是纯文本 `.bru` 文件,可 diff、可进 git)。

契约以 [docs/cubepilot/api.md](../../docs/cubepilot/api.md) 为准;本集合是它的可运行副本,
每条请求的 `docs` 块都复述了该端点的关键约束和易错点。

## 准备

1. **打开通道** —— API 是 ClusterIP,host 打不到:

   ```bash
   kubectl -n <namespace> port-forward svc/cubepilot-api 8080:8080
   ```

2. **填密钥** —— 集合根目录的 `.env`(已被 `.gitignore` 忽略):

   ```
   llmApiKey=sk-...
   ```

   只有 `2-provision/01-add-llm` 用它。不填的话那条会返回 400 —— 那是**正确的**:
   `apiKey` 与 `public` 二选一,都不给就是 400。

3. **（可选）在 Bruno 里选 `local` 环境。**

   环境下拉在**请求面板的右上角**，默认显示 `No Environment`。
   ⚠️ 它**只有在你打开了一个请求之后才出现** —— 刚打开集合、左侧只看到目录树时，
   右上角是没有这个控件的。先点开任意一个请求，比如说 `1-read/01-agent-status`。

   **不选也能用**：集合根目录的 `collection.bru` 用 `vars:pre-request` 定义了同一组
   变量作为兜底，所以只读请求在 `No Environment` 状态下照常工作
   （`environments/local.bru` 里的值优先级更高，要改配置改那边）。

## 打远端

改 `baseUrl` 即可 —— 集合里所有路径都是相对的。用 `environments/remote.bru`
（已建好，填地址就行），或在 gui 的环境下拉里选 `remote`，或命令行覆盖：

```bash
bru run 1-read --env remote
bru run 1-read --env-var baseUrl=https://cubepilot.example.com   # 临时改
```

⚠️ **`baseUrl` 填「入口」地址，不要带 `/api/v1` 后缀** —— 路径已经含有它。

### 四个会让人以为「远端坏了」的坑

1. **`baseUrl` 必须指向 cubepilot-api 本身，不能指向 Portal（nginx）。**
   nginx 只反代 `/api/`（见 `web/nginx.conf`），`/healthz` 会落到 SPA 兜底
   —— **返回 200 + HTML**。只判状态码的测试会**假阳性通过**，所以
   `0-health/01-healthz` 断言的是「200 且不是 HTML」。
   如果你的远端只暴露了 Portal，就把 `/healthz` 这条跳过，其余照常。

2. **SSE 必须关代理缓冲。** 远端 ingress / nginx 少了
   `proxy_buffering off`（参考 `web/nginx.conf`），`3-chat/01-send-message`
   会表现为「卡很久、然后一次性全出来」—— 流式效果没了。
   ≥ `api.md` §1 对反向代理有明确要求。

3. **远端必须是含 `/api/v1` 的新代码。** 判别方法：打
   `/api/v1/agent/status`，返回 404 就是旧版（本仓库的 kind 部署就经历过这一步）。

4. **`X-CubePilot-User` 是自报的，阶段一无认证。** 改 `user` 的值就等于换个人
   —— 在你的 local 集群上无所谓，在**共享或生产环境上是越权**。
   只在自己能负责的环境上这么用。

自签 HTTPS 证书可能要关 Bruno 的 SSL 校验（Preferences → SSL/TLS Verification）。

## 目录

| 目录 | 内容 | 会写数据吗 |
|---|---|---|
| `0-health` | 存活探针(唯一不在 `/api/v1` 下的端点) | 否 |
| `1-read` | 全部只读端点,不加热实例,永远秒回 | 否 |
| `2-provision` | 加 provider → 选模型 → 开通实例 → 装技能 | **是** |
| `3-chat` | 对话 SSE、会话、历史、停止、审批 | **是**(会真调 LLM) |
| `4-tasks` | 任务 CRUD / 触发 / 报告 | **是** |
| `5-llm-admin` | 改 / 删 provider | **是** |
| `6-skills-admin` | 发布 / 卸载技能 | **是** |

每组内按 `seq` 顺序执行;`3-chat` 和 `4-tasks` 会通过后置脚本把
`sessionId` / `taskId` 自动串给后面的请求。

## 命令行

```bash
bru run --env local                                   # 全跑
bru run 1-read --env local                            # 只跑一组(推荐:先这个)
bru run 4-tasks --env local --reporter-html out.html  # 出报告
```

**只有 4 个端点会「加热」实例**(冷启动一个 Pod,可能数十秒或直接 503):
`GET /sessions`、`GET /sessions/{key}/messages`、`POST /messages`、`POST /inspect`。
其余全部秒回 —— 所以 `1-read` 可以在实例还没 Ready 时先跑。

> **503 `instance warming failed` 不是故障**,等一会儿重试即可。
> 这是本集合里最常遇到的"假失败"。

## 两个 Bruno 的坑(踩过了)

1. **文件 body 必须写 `@file(...)`**:

   ```
   body:file {
     file: @file(./fixtures/sample-skill.tar.gz)
     @contentType: application/gzip
   }
   ```

   少了 `@` 只会把**路径字符串**当正文发出去,你会得到
   `{"error":"empty skill tar"}` 且完全看不出原因。

2. **`bru run` 不会跳过 SSE 请求**(4.1.0 实测;官方文档说会跳)。
   它照常执行但会**等整条流结束**才返回。想看逐帧增量请用 gui。
   `3-chat/01-send-message` 因此是集合里最慢的一条。

3. **`bru.setEnvVar()` 会把值写回 `environments/<env>.bru` 文件**。
   `3-chat` 和 `4-tasks` 用它串联 `sessionId` / `taskId`，所以跑完一遍之后
   那个文件会被改写 —— 它是集合的一部分，`git status` 会脏。
   串联脚本已改成优先 `setEnvVar`、没有环境时退回运行时变量 `setVar`。

## 完整走一遍对话 / 审批 / 问答

见 **[WALKTHROUGH.md](WALKTHROUGH.md)** —— 用 GUI 逐步走「对话 → 写审批 → ask_user 问答」
三条流程，含要发什么提示词、会停在哪个事件、以及在**另一个标签页**上怎么解。

## SSE 怎么看

`3-chat/01-send-message` 在 Bruno 里返回的是**原始帧文本**
（`event:` 一行 + `data:` 一行的 JSON，空行分隔）—— Bruno 不做特殊渲染，
所以你看到的是协议本身。这是好事：能看见 `text_replace` 之类的细节，
但帧一多就糊。

**读法**：每帧看 `data` 里的 `type`，语义见 `api.md` §7。真正的要点只有两条：

- `message_delta` 是**追加**（`text += delta`）
- `text_replace` 是**替换**（`text = delta`，整段快照）

**用户最终看到的内容** = 最后一个 `text_replace` 的 `delta`，
加上它之后的 `message_delta`。忽略最后一个 `text_replace` 之前的全部 delta。

### 解码器

帧太多时，把响应面板的内容存成文件，交给解码器回放：

```bash
python3 scripts/decode-sse.py turn.sse          # 或在 Bruno 里复制粘贴到文件
curl -sN ... -d '{...}' | python3 scripts/decode-sse.py -
```

它会打印：sessionId、工具调用与结果、**被 `text_replace` 丢弃的内容**
（最值得看的部分 —— 模型常把推理甚至答案喷在 delta 里）、最终回复、终态。

实测一条「数一下集群里有几个 namespace」的回合，delta 流里出现过：

```
The user is asking me to count the number of namespaces in the cluster
and to answer with only a number. Let me run kubectl.The answer is 7.
```

这 140 字符就是被 `text_replace` 丢掉的 —— 前端若按追加处理，
用户界面上会留下这段英文推理。解码器会把它单独标出来。

## 已知问题

这些是**后端**的 bug,不是集合的问题 —— 集合刻意把它们暴露出来:

- **`3-chat/06-submit-approval` 必须用 canonical 形式的 key**。
  用你传给 `/messages` 的短 key 会 404,而 `/turn` / `/abort` 两个端点却接受短 key
  —— 契约不一致。([issue #180](https://github.com/suanova/cubepilot/issues/180))
- **`1-read/10-audit`**:被**拒绝**的写操作仍会留下一条 `status: "executed"`
  —— 同一个命令可能出现两条互相矛盾的记录。(同上)

## 清理

跑完写入组后,集群里会留下:provider、实例的 `selectedModel`、任务(已删)、
`bruno-demo` 技能、若干会话。provider 和实例是可复用的,技能目前**没有 unpublish 端点**,
要清掉得删 Skill CR。
