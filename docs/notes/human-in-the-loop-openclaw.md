结合你前面的实践（OpenClaw 2026.6.33），目前可以比较明确地总结：

OpenClaw 实现 Human-in-the-loop（HITL）的核心不是写一个普通 tool，而是利用 registerTrustedToolPolicy() 在 tool 执行前拦截，再结合消息/状态/重新触发机制完成审批闭环。

整体架构如下：

                 User Request
                      |
                      v
                OpenClaw Agent
                      |
                      v
              LLM decides tool call
                      |
                      v
        +-----------------------------+
        | registerTrustedToolPolicy() |
        |        evaluate()           |
        +-----------------------------+
                      |
          +-----------+------------+
          |                        |
          v                        v
      safe tool              require approval
          |                        |
          v                        v
     execute tool          create pending request
                                   |
                                   v
                         notify human
                                   |
                    +--------------+--------------+
                    |                             |
                    v                             v
              Approve                       Reject
                    |
                    v
        continue / retry / inject instruction
                    |
                    v
             execute approved action
1. Plugin 层：声明 trusted policy

首先创建 OpenClaw plugin：

目录：

~/.openclaw/plugins/tool-approval

安装后：

~/.openclaw/extensions/tool-approval

关键文件：

openclaw.plugin.json
index.js
openclaw.plugin.json

重点不是 package.json，而是：

{
  "id": "tool-approval",
  "name": "Tool Approval",
  "description": "Human approval for dangerous tools",
  "version": "0.1.0",

  "contracts": {
    "trustedToolPolicies": [
      "require-confirmation"
    ]
  },

  "extensions": [
    "./index.js"
  ]
}

这里声明：

这个 plugin 有一个 trusted tool policy。

2. 注册 tool policy

index.js:

export default {

  id:"tool-approval",

  name:"Tool Approval",

  register(api){

    api.registerTrustedToolPolicy({

      id:"require-confirmation",

      description:
        "Require human approval before tool execution",


      async evaluate(event, ctx){

        console.log(
          "tool request:",
          event
        );


        return {
          block:true,
          blockReason:
             "waiting human approval"
        };
      }

    });

  }
};

执行流程：

tool call
    |
    v
evaluate(event, ctx)
    |
    +-- return allow
    |
    +-- return block
3. 判断哪些操作需要审批

不要所有 tool 都拦截。

例如：

允许：

ls
pwd
cat

需要审批：

rm
kubectl delete
terraform destroy
ssh
git push

例如：

async evaluate(event, ctx){

  const tool =
      event.toolName;


  const params =
      event.params;


  if(tool==="exec"){

     const cmd=params.command;


     if(
       cmd.includes("rm") ||
       cmd.includes("delete")
     ){

        return {
          block:true,
          blockReason:
            "dangerous command"
        };
     }
  }


  return {
    block:false
  };
}
4. 保存 pending approval

因为 OpenClaw 的 policy 是同步决策点：

它不是：

await human.click()

所以需要自己保存状态。

例如：

const pending = new Map();


evaluate(event,ctx){

 const id =
   crypto.randomUUID();


 pending.set(id,{
    event,
    ctx,
    createdAt:
      Date.now()
 });


 return {
   block:true,
   blockReason:
     `approval required id=${id}`
 };

}

保存内容：

approval id
 |
 +-- toolName
 |
 +-- params
 |
 +-- session
 |
 +-- runId
 |
 +-- user
5. 通知人审批

这里 OpenClaw 给你几个方向。

你之前 dump API 时已经看到：

registerCommand
registerHttpRoute
registerControlUiDescriptor
registerAgentEventSubscription
enqueueNextTurnInjection

可以组合。

方式 A：聊天审批（最简单）

例如：

Agent:

需要执行:

kubectl delete pod test

approval id:
abc123

回复:
approve abc123

用户：

approve abc123

plugin 收到：

approve event
方式 B：CLI 审批

注册：

api.registerCommand({
  name:"approve"
})

用户：

openclaw approve abc123
方式 C：Web UI 审批

使用：

registerControlUiDescriptor

实现：

Pending Actions

--------------------------------
kubectl delete namespace test

[Approve] [Reject]
--------------------------------
6. Approval 后如何继续执行？

这是 HITL 最关键的部分。

注意：

return {
 block:true
}

意味着：

当前 tool call 已经失败结束。

OpenClaw 不会自动暂停等待。

所以 approve 后需要：

推荐方案：

重新注入下一轮 agent：

使用：

enqueueNextTurnInjection()

例如：

审批成功：

继续执行刚才被批准的操作

Agent 新一轮：

好的，我已获得批准，现在执行 tool

流程：

第一次：

Agent
 |
 tool call
 |
 policy block
 |
 pending approval


第二次：

human approve
 |
 enqueueNextTurnInjection
 |
 agent continue
 |
 tool call
 |
 policy allow
 |
 execute
7. 一个完整 HITL plugin 架构

最终：

tool-approval plugin

                    +----------------+
                    | Trusted Policy |
                    +----------------+
                            |
                            v
                    evaluate(tool call)
                            |
          +-----------------+----------------+
          |                                  |
          v                                  v
       allow                             require approval
          |                                  |
          v                                  v
    execute tool                    Approval Manager
                                           |
                 +-------------------------+
                 |
        +--------+--------+
        |                 |
      approve           reject
        |
        v
enqueueNextTurnInjection()
        |
        v
Agent continues
8. 你现在已经验证完成的部分

你已经跑通：

部分	状态
plugin manifest	✅
extension install	✅
plugin loading	✅
register(api)	✅
registerTrustedToolPolicy	✅
policy hit	✅

剩下：

部分	下一步
pending storage	实现
approval channel	实现
approve command/UI	实现
resume execution	研究 enqueueNextTurnInjection
9. 推荐下一步研究顺序

不要直接做 UI，建议：

打印完整 event/context
console.log(JSON.stringify(event,null,2))
console.log(JSON.stringify(ctx,null,2))

确认：

session id
run id
tool call id
实现内存版 pending：
Map()
实现 CLI:
/approve <id>
用：
enqueueNextTurnInjection

恢复 agent

最后再接：
飞书审批卡片
Web UI
Slack/Teams
企业审批系统

你现在做的这个方向，本质上就是给 OpenClaw 加一个 AWS Bedrock Agent Guardrails / Copilot Studio Approval / LangGraph interrupt 类似的人工决策节点。你已经走到了正确的扩展点。

---

# 10. 实测验证补充（2026-08-12 会话 · 真实 gateway 验证结论）

本节在真实 gateway（Docker `openclaw:local`，OpenClaw 2026.6.33）上实测后补充，**修正前面 §2/§6 的方案结论**。

## 10.1 核心修正：`evaluate()` 直接返回 `requireApproval`，比 `block` 更优

前面方案是 `evaluate()` 返回 `{block: true}`，再手动 pending Map + CLI 审批 + `enqueueNextTurnInjection` 续跑。**实测证明不需要这么复杂**：

`registerTrustedToolPolicy` 的 `evaluate()` **可以直接返回 `{requireApproval: {...}}`**（源码 trusted-tool-policy.ts 明确处理 `decision.requireApproval`），此时 OpenClaw **原生暂停等待**操作人审批：

- 批准（allow-once / allow-always）→ 工具**原地继续执行**，无需手动续跑
- 拒绝 / 超时 → fail-closed（`timeoutBehavior: "deny"`），默认不执行
- **不需要** pending Map、CLI `/approve` 命令、`enqueueNextTurnInjection`

```js
api.registerTrustedToolPolicy({
  id: "require-approval-policy",
  description: "Require approval before shell/MCP tool calls",
  async evaluate(event, ctx) {
    const toolName = event?.toolName;
    if (!["bash", "exec", "shell"].includes(toolName) && !String(toolName).includes("__")) {
      return {}; // 放行（MCP 工具名含 "__" 分隔符）
    }
    return {
      requireApproval: {
        title: `确认调用：${toolName}`,
        description: `工具: ${toolName}\n参数: ${JSON.stringify(event?.params ?? {})}`,
        severity: "warning",
        timeoutMs: 600_000,       // 10 分钟，超时 fail-closed
        timeoutBehavior: "deny",
        allowedDecisions: ["allow-once", "allow-always", "deny"],
      },
    };
  },
});
```

> **为什么不用 `api.on("before_tool_call")` typed hook？** 实测本环境（2026.6.33 + Docker）里 typed hook **注册了但不分发**（钩子从不触发）；而 `registerTrustedToolPolicy` 正常分发。故本环境用 trusted policy 路线。openclaw.plugin.json 需声明 `contracts.trustedToolPolicies`，否则注册被拒。

## 10.2 实测验证结果

| 场景 | 结果 |
|---|---|
| Shell 工具（exec） | ✅ `evaluate()` 触发 → requireApproval → 审批弹窗 → 批准后执行 |
| MCP 工具（`echo-server__echo`） | ✅ 同上（MCP 工具名格式 `<serverName>__<toolName>`，按 `__` 匹配） |
| 超时 | ✅ fail-closed deny |
| 审批面 | ✅ 弹窗出现在 OpenClaw 自身 UI（桌面 / Control UI） |

## 10.3 部署坑（本环境实测）

1. **插件必须装成 global（不能 `--link`）**：
   - `openclaw plugins install --link <path>` → origin=config、installPath=源目录 → **gateway 不加载**
   - `openclaw plugins install <path>`（不带 `--link`）→ origin=global、installPath=`extensions/` → **gateway 加载**
   - 判别：`openclaw plugins list` 里 source 应为 `global:<name>/index.js`

2. **CLI 命令加载插件 ≠ gateway 加载了**：`openclaw plugins list` / `inspect` 会在 CLI 进程里加载插件（register 会跑），易误判为"gateway 已加载"。判别：看 gateway 自己日志 `/tmp/openclaw/openclaw-<date>.log` 里 `http server listening (N plugins: ...)` 或 `plugins.allow` 发现消息是否包含该插件。

3. **gateway 日志位置**：容器内 `/tmp/openclaw/openclaw-<date>.log`（docker logs 可能看不到新内容，stdout 不一定实时进 docker logs）。

## 10.4 审批面行为

- **审批广播给所有已连接审批面**（OpenClaw 桌面 / Control UI）+ `approvals.plugin` 转发目标。要"只在飞书"，需抑制本地 UI 面（当前无配置开关）。
- `approvals.plugin`（`enabled / mode / agentFilter / targets`）可把插件审批 prompt 转发到聊天渠道（如飞书），但**不抑制**本地 UI。
- 飞书 channel 的 `approvalCapability` 为 `feishuApprovalAuth`（仅授权，非完整投递适配器）。

## 10.5 关键架构差异：exec approval vs 插件 requireApproval

**为什么原生 exec approval 能在飞书直接审批，而 requireApproval 同会话 `/approve` 死锁？**

| | 原生 exec approval | 插件 requireApproval |
|---|---|---|
| 模型 | **非阻塞 pending** | **阻塞 blocking** |
| 等待时 | exec 立即返回 "approval-pending" 工具结果（`buildExecApprovalPendingToolResult`），agent 继续 | 工具调用 `await plugin.approval.waitDecision`，agent run 卡死 |
| 会话 | 不阻塞 | `state=processing` 卡住（stalled，queueDepth 堆积） |
| 同会话 `/approve` | 会话空闲 → 正常处理 → **生效** | 消息排队空转 → **死锁**（直到 run abort / 超时释放会话） |
| 批准后 | exec 结果作为 followup 发回 | 无此机制（需外部面解除） |

**结论**：飞书（IM）同聊审批要可用，须复刻 exec approval 的 **异步 pending + followup** 模型——审批请求**不阻塞 agent**，审批走独立通道解决，结果 followup 注入。这是设计文档「第二阶段 IM 审批卡片」的关键实现方向，而不是沿用阻塞的 `requireApproval`。

## 10.6 测试资产

- 插件：`~/.openclaw/plugins/tool-approval-hook/`（trusted policy + requireApproval，global 安装到 `extensions/`）
- 最小 MCP echo server：`~/.openclaw/mcp-test/echo-server.mjs`（零依赖 NDJSON stdio MCP server，暴露一个 `echo` 工具）
- 注册：`openclaw mcp add echo-server --command node --arg <绝对路径>`