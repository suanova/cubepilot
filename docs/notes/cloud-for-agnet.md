Today 1:26 PM

解释一下 ：

CubeStack战略升级从"给AI提供算力"走向"给Agent提供云"双主线战略Cloud for Agents × Agents for Cloud终极形态Agent-Native Cloud核心跃迁智算平台→企业Agent云
战略从智算平台到企业Agent云CubeStack下一阶段的核心任务，是完成一次产品范式升级——从以资源和AI Job为中心的智算平台，演进为以Agent为第一等对象的企业Agent云，使企业智能体能够像云原生应用一样被开发、交付和规模化运营。CloudfoforAgents让成为智体的业级开发行环。为Agent提供的行基础设、工、发布理弹性保。AgentsfoforCloud让智能体重新定义云的使用和管理方式。CubePilot升级为平台智能入口，用戶从操作资源转向描述目标。双主线汇合为Agent-Native Cloud：既为Agent提供云，也让Agent使用和管理云。
趋势AI平台正从"模型服务"走向"智能体生产化"三类主流厂商的升级方向高度一致，共同印证平台演进的结构性趋势。模型厂商（DeepSeek/OpenAI/Anthropic）从单模型能力走向Model + Harness +工具+运行环境的联合优化，推出自有Agent平台与工具调用框架，强化端到端智能体执行能力。☁️公有云厂商（AWAWS/Azure/GoogleCloud）将AI平台全面升级为企业Agent平台，覆盖运行环境、工具集成、身份治理、调试观测与规模化运营，形成完整Agent生命周期管理能力。私有云/混合云厂商从"模型部署平台"向"Agent基础设施"演进，提供私有化推理、Agent编排、治理审计及与企业IT系统的深度集成，满足数据安全与合规需求。趋势判断：AI平台的下一代竞争，不只是模型服务能力，而是企业智能体的开发、运行、交付和治理能力。
行业从AIWoWorkload到Agent是平台演进大势各类厂商升级方向高度趋同，Agent基础设施正成为新的竞争主战场。厂商类型过去重点当前升级方向模型厂商大模型、推理APIModel + Harness + Agent Runtime公有云厂商AI Studio、MaaS、模型服务Agent Platform、Runtime、Governance私有云厂商GPU、模型部署、RAG私有化Agent基础设施、治理与安全企业应用厂商Copilot、助手可执行任务的业务Agent过的平问题模型不跑起来资不用理迟是否标未来的平台核心问题Agent能否安全、稳定、可治理地完成任务？能否规模化运营？结论：Agent将成为AI云平台的下一代核心对象。CubeStack需从"支撑AI Workload"升级为支撑企业智能体全体全生命周期。
产品范式Agent成为平台第一等对象CubeStack已从"给用戶资源"升级为"给AI工作负载提供运行环境"。下一阶段是平台最高层抽象的跃迁——Agent不再是AI工作负载中的一个应用场景，而是平台新的第一等对象。当前→下一阶段：平台视⻆转变当前平台视⻆下一阶段平台视⻆资源申请目标表达AI JobAgentService模型推理智能体执行Token服务Agent执行服务运维监控AgentOps用戶操作云Agent使用和管理云上述转变意味着平台的核心抽象层次全面上移：从基础设施语义（资源、Job、Token）跃迁至业务语义（目标、AgentService、AgentOps），为企业客戶提供真正以智能体为中心的云平台体验。
进路线从资源云→AI云→Agent云CubeStack下一阶段不是推翻重来，而是在现有基础上持续升级产品抽象，沿清晰路线图分代演进。阶段核心对象用戶关注点产品形态V1资源云GPU / CPU / VM / Storage我要资源智算基础设施V2 AI云Model /Training / Inference我要训练和部署模型AI智算云V3 Agent云（下一阶段）Agent / Runtime / Harness /Tool/ Memory我要开发并运行智能体企业Agent PaaSV4 Agent-Native CloudGoal /Task / Agent我要完成业务目标智能体原生云V1资源云基础算力与存储资源V2AI云模型训练与推理平台V3Agent云Agent运行时与工具链V4V4AgentNative原生化Agent云平台
当前CubeStack处于V2→V3的关键转折点当前正处于产品演进最关键的窗口期，战略重心聚焦于将企业Agent云做扎实。当前位置V2→V3关键转折点已完成轻量私有云+异构算力+高速存储底座，AI工作负载运行环境成熟。近期目标企业Agent云将Agent像云原生应用一样被开发、运行、发布、治理和规模化运营。中期目标Agent-Native Cloud让Agent成为云平台的使用入口和运维主体，逐步走向V4形态。近期不急于跳到V4，而是把V3做深做实：让企业客戶真正能在CubeStack上以生产级标准开发和运营智能体。
上传日志
联系客服
功能更新
帮助中心

这 7 点其实是在讲同一件事，只是从不同层次反复描述：

CubeStack 不想再只是一个“管理 GPU、模型、AI Job 的平台”，而是要变成一个“让企业可以生产、运行、管理 Agent 的云平台”。

再往后一步，甚至希望 Agent 本身成为使用和管理云的主要入口。

我把这套战略拆开讲，你会比较容易理解。

一、先用一句话理解整个战略

可以把 CubeStack 的演进理解成：

资源云
  ↓
“给我 GPU / CPU / 存储”
  ↓
AI 云
  ↓
“帮我训练 / 部署 / 推理模型”
  ↓
Agent 云
  ↓
“帮我开发 / 运行 / 发布 / 管理 Agent”
  ↓
Agent-Native Cloud
  ↓
“我告诉云我要完成什么目标，
 Agent 自己使用和管理云”

所以它不是简单地：

GPU 平台 → Agent 平台

而是：

基础设施抽象层不断上移。

以前用户面对的是：

GPU
VM
Storage
Network
Pod
Job
Model
Inference

以后用户面对的是：

Agent
AgentService
Task
Goal
Tool
Memory
Runtime
AgentOps

这就是整个材料真正想表达的东西。

二、第一点：Cloud for Agents × Agents for Cloud 是什么？

这是整套战略最核心的一句话。

1. Cloud for Agents

意思是：

云是给 Agent 用的。

就像 Kubernetes 是给云原生应用提供运行环境一样：

Kubernetes
    ↓
Pod / Service / Config / Storage
    ↓
运行微服务

未来 CubeStack：

CubeStack
    ↓
Agent Runtime
Agent Tool
Agent Memory
Agent Identity
Agent Gateway
Agent Observability
Agent Governance
    ↓
运行企业 Agent

比如企业开发一个：

“故障自动诊断 Agent”

它可能需要：

Agent
 ├── LLM
 ├── Kubernetes Tool
 ├── Prometheus Tool
 ├── CMDB Tool
 ├── Ticket Tool
 ├── Memory
 ├── Credential
 └── Runtime

企业不应该自己把这些东西一个个拼起来。

CubeStack 应该提供：

Agent 的云基础设施。

这就是 Cloud for Agents。

三、Agents for Cloud 又是什么意思？

这个方向更有意思。

它不是：

云给 Agent 用。

而是：

Agent 来使用云。

例如以前运维人员这样操作：

kubectl get pod
kubectl describe pod
kubectl logs
kubectl scale
kubectl rollout restart

以后用户可能只说：

“帮我把这个服务恢复到正常状态。”

Agent 自己：

发现异常
 ↓
查询 Pod
 ↓
查询日志
 ↓
查询 Prometheus
 ↓
分析原因
 ↓
发现 OOM
 ↓
修改 Deployment
 ↓
扩容
 ↓
观察指标
 ↓
确认恢复

这里用户根本不需要知道：

Pod
Deployment
Node
GPU
Storage

用户表达的是：

Goal / Task

而 Agent 操作的是：

Cloud

所以：

用户
 ↓
Goal
 ↓
Agent
 ↓
Cloud API
 ↓
Infrastructure

这就是 Agents for Cloud。

四、两条线最终合起来，就是 Agent-Native Cloud

因此：

                  ┌───────────────┐
                  │     Agent     │
                  └───────┬───────┘
                          │
              ┌───────────┴───────────┐
              ↓                       ↓
       Cloud for Agents        Agents for Cloud
              ↓                       ↓
       云给 Agent 提供能力       Agent 使用/管理云
              └───────────┬───────────┘
                          ↓
                  Agent-Native Cloud

所以 Agent-Native Cloud 并不是简单地“云里部署 Agent”。

真正的含义是：

Agent 既是云平台服务的消费者，又逐渐成为云平台的操作者。

五、第二点：为什么说从“智算平台”变成“企业 Agent 云”？

这是产品定位发生变化。

现在的智算平台一般关注：

GPU
CPU
Storage
Network
Model
Training
Inference
Job

例如用户的需求是：

“给我 8 张 GPU。”

或者：

“部署一个 DeepSeek 推理服务。”

平台关心的是：

资源够不够？
GPU 有没有？
容器在哪里运行？
模型有没有部署？
推理吞吐多少？

这还是 AI Workload-centric。

未来用户的需求变成：

“我要一个能够自动分析 Kubernetes 故障的 Agent。”

平台需要解决：

Agent 开发
Agent Runtime
Agent Model
Agent Tools
Agent Memory
Agent Identity
Agent Permission
Agent Deployment
Agent Version
Agent Observability
Agent Evaluation
Agent Governance
Agent Audit
Agent Scaling

所以平台管理对象发生了变化：

过去：


GPU → Job → Model → Inference




未来：


Agent → Runtime → Tool → Memory → Task
六、第三、四点：为什么行业都在往 Agent Platform 走？

这里其实是在给 CubeStack 的战略找“行业依据”。

它认为三类厂商都在发生类似变化。

① 模型厂商

过去：

OpenAI
DeepSeek
Anthropic


核心产品：
Model API

现在越来越强调：

Model
+
Tool Calling
+
Harness
+
Agent Runtime
+
Execution Environment

也就是说：

模型本身越来越不是完整产品。

模型需要：

模型
+
工具
+
上下文
+
执行环境
+
Memory
+
反馈

才能真正完成复杂任务。

② 公有云

AWS / Azure / Google Cloud 过去主要是：

GPU
VM
Kubernetes
AI Studio
Model API

现在开始提供：

Agent Platform
Agent Runtime
Agent Tool
Identity
Observability
Governance
Evaluation

原因很简单：

企业真正需要的不是：

“我有一个 LLM。”

而是：

“我有一个能替我完成工作的数字员工。”

③ 私有云

私有云的特殊性是：

企业数据不能随便出去。

所以企业需要：

私有模型
+
私有 Agent
+
企业内部工具
+
企业数据
+
权限
+
审计
+
安全

这恰好是 CubeStack 这种企业私有云/混合云平台可以发挥优势的地方。

七、第五点：Agent 成为“第一等对象”是什么意思？

这个概念非常重要。

所谓 First-class Object / 第一等对象，你可以理解为：

平台 UI、API、权限、生命周期、调度、监控等机制，都会围绕这个对象设计。

比如 Kubernetes 的第一等对象：

Pod
Deployment
Service
ConfigMap
Secret

它们都有：

API
CRUD
Lifecycle
Status
Event
Permission
Monitoring

如果 CubeStack 把 Agent 变成第一等对象，那么以后可能出现：

Agent
AgentService
AgentRuntime
AgentTool
AgentMemory
AgentTask
AgentPolicy
AgentCredential

例如：

kind: AgentService


metadata:
  name: k8s-ops-agent


spec:
  model: deepseek-vX
  tools:
    - kubernetes
    - prometheus
    - alertmanager
  memory:
    enabled: true
  replicas: 3

这时候 Agent 就和 Kubernetes Deployment 类似了。

你不是：

“找几个 GPU，然后起几个 Pod，再部署模型，再配置 MCP……”

而是：

创建一个 AgentService。

平台负责下面的一切。

八、第六点：V1 → V2 → V3 → V4 是最容易理解的一部分

这张表其实非常好理解。

阶段	平台核心对象	用户说什么
V1 资源云	GPU / CPU / VM / Storage	我要资源
V2 AI 云	Model / Training / Inference	我要跑模型
V3 Agent 云	Agent / Runtime / Tool / Memory	我要运行 Agent
V4 Agent-Native Cloud	Goal / Task / Agent	我要完成目标
V1：Resource Cloud

本质：

IaaS。

用户：

给我 8 张 H100。

平台：

GPU
VM
Storage
Network
V2：AI Cloud

平台开始理解 AI。

用户：

我要训练一个模型。

或者：

我要部署一个 LLM 推理服务。

平台开始提供：

Training
Inference
Model Registry
GPU Scheduling
AI Workload

这就是现在 CubeStack 的主要能力。

V3：Agent Cloud

平台开始理解 Agent。

用户：

我要部署一个 IT 运维 Agent。

平台提供：

Agent Runtime
Agent Framework
Tool
Memory
Model
Identity
Deployment
Observability
Governance

这是材料认为 CubeStack 现在应该重点做的事情。

V4：Agent-Native Cloud

这就更进一步。

用户甚至不需要说：

创建一个 Agent。

而是：

“帮我把这个 Kubernetes 集群的 GPU 利用率提高到 70%。”

然后：

Goal
 ↓
Agent
 ↓
观察云
 ↓
分析
 ↓
执行
 ↓
验证
 ↓
继续优化

这个时候：

Agent 成为云的操作主体。

这才叫真正的 Agent-Native。

九、第七点：为什么现在“不急着做 V4”？

我认为这是这份战略里比较务实的一点。

它实际上是在说：

不要现在就喊 Agent-Native Cloud，然后实际上连 Agent Platform 都没做好。

当前 CubeStack 大概处于：

V2
AI Cloud
    │
    │ ← 当前
    ↓
V3
Agent Cloud
    │
    ↓
V4
Agent-Native Cloud

因此近期应该把 V3 做扎实。

十、那 V3 到底应该做什么？

如果把这份战略翻译成产品需求，我认为核心应该是下面这套东西：

                 CubeStack Agent Cloud
                         │
       ┌─────────────────┼─────────────────┐
       ↓                 ↓                 ↓
   Agent Dev         Agent Runtime      AgentOps
       │                 │                 │
   Harness             Deploy           Observe
   SDK                  Scale            Evaluate
   Prompt               HA               Audit
   Workflow             GPU              Trace
                         │
       ┌─────────────────┼─────────────────┐
       ↓                 ↓                 ↓
     Tools            Memory            Identity
       │                 │                 │
     MCP             Vector DB          RBAC
     K8s              Context           Secret
     Storage          Long-term         Policy
     IT systems

再下面：

                Existing CubeStack
                       │
       ┌───────────────┼──────────────┐
       ↓               ↓              ↓
      GPU           Storage        Network
       │               │              │
       └───────────────┼──────────────┘
                       ↓
                 Infrastructure

所以 Agent Cloud 并不是重新造一个平台。

而是：

把现有 CubeStack 的基础设施能力，向上封装成 Agent 所需要的基础设施。

十一、CubePilot 在这里扮演什么角色？

这里也非常关键。

现在传统云平台的交互方式：

用户
 ↓
控制台
 ↓
点 GPU
点 VM
点 Storage
点 Network
点 Kubernetes

CubePilot 希望变成：

用户
 ↓
自然语言 / Goal
 ↓
CubePilot
 ↓
Agent
 ↓
CubeStack API
 ↓
Cloud

比如以前：

创建 4 张 GPU 的 VM。

以后：

“部署一个 7B 模型的推理服务，要求延迟低于 100ms，并且自动扩缩容。”

CubePilot/Agent 自己决定：

GPU
 ↓
节点
 ↓
模型
 ↓
Runtime
 ↓
Deployment
 ↓
Autoscaling
 ↓
Monitoring

所以 CubePilot 的角色实际上是从：

Cloud Console

逐渐变成：

AI/Agent Interface to Cloud

十二、这和你之前问我的 OpenClaw / MCP / Agent 有什么关系？

其实关系非常大。

你之前一直在问：

OpenClaw + Skills + MCP 能不能直接变成 K8s Agent、Storage Agent？

从这个战略的角度看，答案是：

Agent 本身不是最难的。

真正难的是把 Agent 放到企业生产环境以后需要的东西全部补齐：

                    Agent
                      │
       ┌──────────────┼──────────────┐
       ↓              ↓              ↓
      LLM           Tools          Memory
       │              │              │
       │            MCP/K8s         DB
       │            Storage         Vector
       │            IT systems
       │
       └──────────────┬──────────────┘
                      ↓
                Agent Runtime
                      ↓
       ┌──────────────┼──────────────┐
       ↓              ↓              ↓
   Identity        Policy          Sandbox
       ↓              ↓              ↓
     RBAC          Approval        Isolation
                      ↓
                 AgentOps
                      ↓
       ┌──────────────┼──────────────┐
       ↓              ↓              ↓
   Observe        Evaluate         Audit
                      ↓
                 CubeStack
                      ↓
          GPU / K8s / Storage / Network

这也是为什么企业 Agent 平台不能简单理解成：

“OpenClaw + 几个 Skills”。

OpenClaw 更像 Agent 的一个执行框架/个人 Agent 平台。

而企业 Agent Cloud 要解决的是：

几十、几百、几千个 Agent 如何安全地运行。

十三、因此这份战略真正的核心竞争点是什么？

我觉得可以浓缩成 4 层：

                    V4
            Agent-Native Cloud
         “Agent 管理 Cloud”
                    ↑
                    │
                    │
                    V3
               Agent Cloud
          “Cloud 运行 Agent”
                    ↑
                    │
                    │
                    V2
                 AI Cloud
          “Cloud 运行 AI Workload”
                    ↑
                    │
                    │
                    V1
                Resource Cloud
          “Cloud 提供基础资源”

所以 CubeStack 真正想抢的，不只是：

GPU Cloud / AI Infrastructure

而是进一步抢：

Enterprise Agent Infrastructure

十四、如果把整份材料翻译成“大白话”

其实就是这句话：

过去 CubeStack 解决的是“企业怎么把 GPU 和 AI 模型跑起来”；现在要解决的是“企业怎么把 AI Agent 真正生产化”；未来进一步解决“企业怎么让 Agent 自己使用和管理云”。

对应三个问题：

过去：
GPU 能不能跑？
模型能不能跑？
        ↓
AI Cloud


现在：
Agent 能不能稳定跑？
能不能部署？
能不能扩容？
能不能接企业系统？
能不能审计？
能不能治理？
        ↓
Agent Cloud


未来：
我只告诉你业务目标，
Agent 能不能自己完成？
        ↓
Agent-Native Cloud

所以如果让我评价这份战略，我会说：

战略主线是清楚的，而且 V2→V3 的定位比较合理。

但真正落地时，最大的挑战不是“做一个 Agent”，而是把 Agent Runtime + Tool/MCP + Identity/RBAC + HITL + Sandbox + Memory + Observability + Evaluation + Governance + Lifecycle 这些东西真正平台化。

尤其是你之前一直关注的 HITL、MCP Gateway、Agent 隔离、Skills、多 Agent 编排，其实恰好都属于这个 V3 Agent Cloud 应该解决的问题，而不是 V4 才需要考虑的东西。

Today 2:35 PM
V3：Agent Cloud

平台开始理解 Agent。

用户：

我要部署一个 IT 运维 Agent。

平台提供：

Agent Runtime

Agent Framework

Tool

Memory

Model

Identity

Deployment

Observability

Governance

这是材料认为 CubeStack 现在应该重点做的事情。

目前有类似的产品吗？

AWS / Azure / Google Cloud 过去主要是：

GPU

VM

Kubernetes

AI Studio

Model API

现在开始提供：

Agent Platform

Agent Runtime

Agent Tool

Identity

Observability

Governance

Evaluation

公有云厂商已经提供这些 agent platform 了吗？这些 platform 是如何使用的？

是的，而且现在 AWS / Azure / Google Cloud 已经不只是“有一些 Agent SDK”了，而是在做你材料里 V3 所描述的完整 Agent Cloud / Agent Platform。

如果拿 CubeStack 的 V3 定义来对照，三家公有云已经非常接近：

                    V3 Agent Cloud
                         │
        ┌────────────────┼────────────────┐
        ↓                ↓                ↓
   Agent Runtime       Tool             Memory
        ↓                ↓                ↓
   Deployment         MCP/API          Session
   Scaling            Gateway          Long-term
        ↓                ↓                ↓
   Identity         Governance       Observability
        ↓                ↓                ↓
              Evaluation / Ops

目前比较值得重点研究的是：

云厂商	Agent 平台	我认为的定位
AWS	Amazon Bedrock AgentCore	最像“Agent 基础设施平台”
Microsoft	Microsoft Foundry Agent Service	最像“企业 Agent PaaS”
Google Cloud	Vertex AI Agent Engine	Agent Runtime + Agent 生命周期平台

而且三家的思路已经高度趋同。下面我分别拆开。

1. AWS：Amazon Bedrock AgentCore

如果你的目标是研究：

CubeStack V3 应该长什么样？

我建议第一优先级研究 AWS AgentCore。

AWS 已经把你材料中的那些东西拆成了一个完整的产品体系：

Amazon Bedrock AgentCore
│
├── Runtime
├── Gateway
├── Memory
├── Identity
├── Policy
├── Observability
├── Evaluations
├── Browser
├── Code Interpreter
└── Agent Registry

AWS 自己对 AgentCore 的定位就是生产级 Agent 平台，而且强调可以使用任意模型 + 任意 Agent Framework。

尤其值得注意的是：

AgentCore Runtime

它不是：

“给你一个 EC2。”

而是：

“给你一个专门运行 Agent 的 Runtime。”

Agent 可以是：

Strands
LangGraph
LangChain
CrewAI
LlamaIndex
自研 Agent

然后交给 AgentCore Runtime 托管。

AWS 会负责：

部署
Session
Isolation
Scaling
Runtime

甚至 Runtime 采用面向 Agent session 的消费模型。

这已经非常接近你材料里的：

Agent Runtime

2. AgentCore Gateway：这个和你之前研究 MCP Gateway 特别相关

这个我认为你尤其应该关注。

AWS 的设计不是：

Agent
 ↓
直接访问几十个 API

而是：

                 Agent
                   │
                   ↓
            AgentCore Gateway
                   │
        ┌──────────┼──────────┐
        ↓          ↓          ↓
      MCP         API       Lambda
        ↓          ↓          ↓
       Tool       Tool       Tool

Gateway 负责把：

API
Lambda
existing services
MCP

转换成 Agent 可以发现和调用的 Tool。

AWS 官方明确把 Gateway 定义成：

secure way for agents to discover and use tools

并支持把已有 API / Lambda 转成 Agent-compatible tools。

这其实和你之前问我的：

MCP Gateway 应该做什么？

高度重合。

3. 更关键的是 AWS 把 Policy 放到了 Tool Call 这一层

这个非常值得 CubeStack 学。

假设 Agent 想执行：

kubectl delete pod xxx

传统 Agent：

LLM
 ↓
Tool
 ↓
直接执行

AWS 的思路：

Agent
 ↓
Tool Call
 ↓
Gateway
 ↓
Policy
 ↓
Allow / Deny
 ↓
Tool

AgentCore Policy 可以在 Gateway 拦截 Tool Call，并按照策略决定是否允许执行。AWS 甚至支持用自然语言描述策略，再转换为 Cedar policy。

这对于你一直关注的：

HITL / Tool Approval / Agent 安全

特别重要。

未来 CubeStack 如果做 Agent Cloud，我认为这几乎是必需能力：

Agent
  ↓
Tool
  ↓
Policy Engine
  ├── Allow
  ├── Deny
  └── Require Approval
           ↓
          Human

也就是说：

HITL 不应该只是 Agent UI 上弹一个确认框，而应该成为 Tool Authorization / Policy 的一部分。

4. AgentCore Identity

这又对应你材料里的：

Identity

Agent 不能拿一个超级管理员账号到处跑。

应该有：

User
 ↓
Agent Identity
 ↓
Tool Identity
 ↓
Resource Permission

例如：

K8s Agent
 ↓
Agent Identity
 ↓
Kubernetes RBAC
 ↓
只能：
  get/list/watch pods
不能：
  delete pod

AWS AgentCore Identity 专门解决 Agent 访问 AWS 和第三方系统时的身份、OAuth、API Key 等问题。

这就是传统：

IAM

向：

Agent IAM

演进。

5. AgentCore Memory

也已经不是让你自己搞：

Redis
Postgres
Vector DB
Embedding
RAG

AWS 提供：

AgentCore Memory
│
├── Short-term memory
└── Long-term memory

可以让 Agent 跨 session 保留信息。

所以材料中的：

Memory

已经是云厂商提供的标准基础设施能力。

6. AgentCore Observability

这个也非常关键。

传统应用：

Request
 ↓
Service
 ↓
DB

你看：

latency
error
CPU
memory

但 Agent：

User
 ↓
LLM
 ↓
Tool A
 ↓
LLM
 ↓
Tool B
 ↓
LLM
 ↓
Tool C
 ↓
Answer

真正需要观察的是：

为什么调用 Tool A？
为什么没调用 Tool B？
用了哪个模型？
每一步耗时多少？
Tool 参数是什么？
Tool 返回了什么？
最终为什么做这个决定？

AWS AgentCore Observability 就是针对这种 Agent trajectory 做 tracing / debugging / monitoring。

7. AgentCore Evaluations

这是传统 Kubernetes / AI 平台里比较缺失的一层。

普通应用：

成功 / 失败

Agent：

成功返回 ≠ 做对了

比如：

“帮我恢复这个 Kubernetes 服务。”

Agent 返回：

“已恢复。”

但是实际上：

Pod
虽然 Running


但是：
错误率 30%

所以 Agent 必须有：

Evaluation
 ↓
Task completion
Tool selection
Tool accuracy
Safety
Quality

AWS AgentCore 已经提供 Agent Evaluation，并支持生产流量持续评估、内置 evaluator 和自定义 evaluator。

8. 所以 AWS AgentCore 基本就是你材料里的 V3

你可以直接这么对照：

CubeStack V3	AWS AgentCore
Agent Runtime	AgentCore Runtime
Agent Tool	Gateway / Tools
MCP	Gateway
Memory	AgentCore Memory
Model	Bedrock / 任意模型
Identity	AgentCore Identity
Deployment	Runtime
Observability	AgentCore Observability
Governance	Policy / Identity
Evaluation	AgentCore Evaluations
Agent Registry	Agent Registry
Sandbox	Browser / Code Interpreter / Runtime

所以：

这不是“类似产品”，AWS 已经在做材料描述的东西了。

9. Microsoft：Microsoft Foundry Agent Service

Azure 的路线也非常完整。

微软现在叫：

Microsoft Foundry Agent Service

它定位就是一个托管的 Agent 平台，可以：

Build
Deploy
Scale
Operate

Agent。

它的架构大致是：

Microsoft Foundry
│
├── Models
│
├── Agent Service
│    ├── Agent Runtime
│    ├── Tools
│    ├── Memory
│    ├── MCP
│    ├── Identity
│    ├── Security
│    ├── Observability
│    └── Evaluation
│
└── Governance

这和 AWS 已经非常像。

10. Azure 的一个特点：Hosted Agent

这个很值得 CubeStack 学。

你可以自己写 Agent：

my_agent.py

可以使用：

Agent Framework
LangGraph
OpenAI Agents SDK
Anthropic Agent SDK
GitHub Copilot SDK
自己的代码

然后：

Agent code
 ↓
Container
 ↓
Foundry Agent Service
 ↓
Managed Agent Runtime

微软负责：

Endpoint
Scaling
Identity
Observability
Lifecycle

官方文档明确说 Hosted Agent 可以把你自己的 Agent code 打成 container，然后交给 Foundry 托管运行。

这就很像：

“Agent 的 Kubernetes / PaaS。”

11. Azure 的 Tool 模型也很成熟

可以直接接：

Web Search
File Search
Code Interpreter
Azure Functions
OpenAPI
MCP
Browser Automation
Logic Apps
Custom Function

尤其是：

MCP Server

可以直接作为 Agent Tool 接入。

所以如果你做 CubeStack：

CubeStack Agent
      ↓
MCP Gateway
      ↓
K8s MCP
Storage MCP
Monitoring MCP
Network MCP
ITSM MCP

这个架构和 Azure Foundry 的思路是高度一致的。

12. Azure 更强调 Enterprise Identity

例如：

Agent
 ↓
Microsoft Entra ID
 ↓
RBAC
 ↓
Azure Resource

甚至每一个 Hosted Agent 都可以拥有自己的 dedicated Entra identity。

这对于企业 Agent 特别重要。

因为最终企业会问：

“这个 Agent 到底是谁？”

不是：

“这个 Pod 是谁？”

13. Azure Evaluation 甚至已经做到 Trace Evaluation

这点非常有意思。

比如：

Agent
 ↓
OpenTelemetry
 ↓
Application Insights
 ↓
Agent Trace
 ↓
Evaluation

可以直接拿生产 Agent trace 来做 evaluation。

也就是说：

Observability 和 Evaluation 已经开始合并。

这其实是 AgentOps 的一个重要趋势：

Agent Execution
      ↓
    Trace
      ↓
 ┌────┴────┐
 ↓         ↓
Debug     Evaluate
 ↓         ↓
Optimize  Improve
14. Google：Vertex AI Agent Engine

Google 走的路线也很类似。

现在：

Vertex AI Agent Engine

已经是 GA。

它的定位是：

managed runtime for deploying AI agents to production

而且支持：

ADK
LangGraph
CrewAI
其他 framework

以及不同模型。

15. Google 的 Agent Engine 可以理解成：
Vertex AI Agent Engine
│
├── Runtime
├── Sessions
├── Memory Bank
├── Code Execution
├── Security
├── Scaling
├── Observability
├── Evaluation
└── A2A

例如：

Agent
 ↓
Agent Engine Runtime
 ↓
Session
 ↓
Memory Bank
 ↓
Tool
 ↓
Model

Google 已经提供 Session 和长期 Memory Bank。

甚至现在 SDK 里已经有：

agent_engines.runtimes
agent_engines.sessions
agent_engines.memories
agent_engines.a2a_tasks

这种非常明显的：

Agent First-class Resource

设计。

这其实和你材料里的：

Agent 成为平台第一等对象

非常吻合。

16. 三家的产品其实已经可以画成这样
                 Enterprise Agent Cloud
                          │
       ┌──────────────────┼──────────────────┐
       │                  │                  │
      AWS               Azure              Google
       │                  │                  │
 AgentCore         Foundry Agent       Vertex AI Agent
                      Service              Engine
       │                  │                  │
       ↓                  ↓                  ↓
    Runtime            Runtime            Runtime
    Gateway            Tools              Tools
    Memory             Memory             Sessions
    Identity           Entra ID           Memory Bank
    Policy             RBAC               Security
    Observability      App Insights       Observability
    Evaluation         Evaluation         Evaluation
    Sandbox             Hosted Agent       Code Execution

所以你材料里面写：

AWS / Azure / Google Cloud 正在从 AI Platform 升级为 Agent Platform

这个判断是成立的，而且从 2025→2026 的产品演进看已经不是趋势预测，而基本已经发生了。

17. 但三家的“使用方式”其实很值得理解

它们并不是让你：

“在控制台点一下，AI 自动帮我造一个超级 Agent。”

更典型的使用模式是：

                 Developer
                     │
                     ↓
              Agent Framework
                     │
          ┌──────────┼──────────┐
          ↓          ↓          ↓
        Model       Tool       Memory
          │          │          │
          └──────────┼──────────┘
                     ↓
                Agent Code
                     │
                     ↓
              Cloud Agent Platform
                     │
        ┌────────────┼────────────┐
        ↓            ↓            ↓
     Runtime      Identity     Observability
        ↓            ↓            ↓
      Scale        Policy       Evaluation
                     ↓
                 Production

也就是说：

开发者依然负责 Agent 的“大脑”，云平台负责 Agent 的“身体、基础设施和企业治理”。

18. 举个你更容易理解的例子：Kubernetes 运维 Agent

假设我要做一个：

Kubernetes Operations Agent

第一步：开发 Agent

你自己写：

agent = Agent(
    model="DeepSeek",
    instructions="""
    你是 Kubernetes SRE。
    负责分析故障并恢复服务。
    """,
    tools=[
        kubernetes_tool,
        prometheus_tool,
        alertmanager_tool
    ]
)
第二步：交给 Agent Platform

例如 AWS：

Agent
 ↓
AgentCore Runtime

或者 Azure：

Agent
 ↓
Foundry Hosted Agent

或者 Google：

Agent
 ↓
Vertex AI Agent Engine
第三步：Tool 接入
Agent
 ↓
MCP / Gateway
 ├── Kubernetes
 ├── Prometheus
 ├── Alertmanager
 ├── Grafana
 └── ITSM
第四步：权限
Agent Identity
      ↓
Policy
      ↓
K8s RBAC

例如：

允许：
  get pod
  get logs
  get deployment
  scale deployment


禁止：
  delete namespace
  delete node


需要人工审批：
  delete pod
  rollout restart
  modify deployment
第五步：生产运行
User
 │
 │ “线上服务异常”
 ↓
Agent
 │
 ├── query Prometheus
 ├── query K8s
 ├── read logs
 ├── analyze
 ├── propose fix
 ├── policy check
 ├── HITL approval
 ├── execute
 └── verify
第六步：AgentOps

平台记录：

谁调用的？
 ↓
哪个 Agent？
 ↓
用了哪个 Model？
 ↓
调用了哪些 Tool？
 ↓
Tool 参数？
 ↓
Tool 返回？
 ↓
做了什么决策？
 ↓
有没有 HITL？
 ↓
最终任务成功了吗？
 ↓
花了多少 Token？
 ↓
用了多少时间？

这就是所谓：

Production Agent Platform

而不是简单的：

LLM API + Prompt。

19. 这也解释了 CubeStack V3 真正应该和普通 Agent Framework 拉开什么差距

这是我认为你这份战略材料里最值得注意的地方。

如果 CubeStack 只是做：

Agent SDK
+
Prompt
+
Workflow
+
MCP

那么它很容易和：

LangGraph
CrewAI
AutoGen
OpenAI Agents SDK
Strands

竞争。

这不是 CubeStack 最擅长的地方。

CubeStack 真正有优势的是：

              CubeStack Agent Cloud
                       │
                       ↓
               Existing Cloud
                       │
       ┌───────────────┼────────────────┐
       ↓               ↓                ↓
      GPU           Kubernetes       Storage
       ↓               ↓                ↓
      VM            Network          Security
       ↓               ↓                ↓
              Enterprise Infra

然后向上：

                Agent Cloud
                    │
      ┌─────────────┼─────────────┐
      ↓             ↓             ↓
   Runtime        Gateway       AgentOps
      ↓             ↓             ↓
   Sandbox         MCP          Policy
   GPU             Tool         Audit
   Scale           Identity     Eval
                    ↓
                 HITL

也就是：

不要和 LangGraph 比谁的 Agent 编排 API 更漂亮。

而应该做：

企业 Agent 的基础设施云。

20. 甚至可以把 CubeStack V3 对标 AWS AgentCore

我会建议你们内部直接做这样一张竞品能力矩阵：

能力	CubeStack V3	AWS AgentCore	Azure Foundry	Google Agent Engine
Agent Runtime	🎯	✅	✅	✅
Agent Deployment	🎯	✅	✅	✅
Agent Scaling	🎯	✅	✅	✅
MCP	🎯	✅	✅	✅
Tool Gateway	🎯	✅	✅	△
Agent Identity	🎯	✅	✅	✅
Policy	🎯	✅	✅	✅
HITL	🎯	△/可组合	Preview/可组合	△
Memory	🎯	✅	✅	✅
Sandbox	🎯	✅	✅	✅
Observability	🎯	✅	✅	✅
Evaluation	🎯	✅	✅	✅
Agent Registry	🎯	✅	✅	△
GPU/AI Infra	CubeStack优势	AWS优势	Azure优势	GCP优势
Kubernetes	CubeStack优势	AWS优势	Azure优势	GCP优势
私有化	CubeStack核心优势	弱	弱	弱
异构 GPU	CubeStack核心优势	AWS生态	Azure生态	GCP生态

这里就能看出 CubeStack 的真正机会：

公有云已经把 V3 Agent Cloud 做出来了，但它们的底座是公有云；CubeStack 可以把类似能力带到企业私有云 / 混合云 / 国产 GPU / Kubernetes 环境。

21. 我尤其建议你们重点研究 AWS AgentCore

因为它和你前面那份战略 PPT 的对应关系几乎是 1:1：

CubeStack PPT                    AWS AgentCore


Agent Runtime            ←→     Runtime


Tool                     ←→     Gateway


Memory                   ←→     Memory


Identity                 ←→     Identity


Deployment               ←→     Runtime


Observability            ←→     Observability


Governance               ←→     Policy


Evaluation               ←→     Evaluations


Agent Registry            ←→     Agent Registry


Sandbox                  ←→     Browser / Code Interpreter

甚至 AWS 已经进一步提出了 AgentCore Harness：把 model、tools、skills、instructions 等 Agent 要素组合起来，通过非常简单的接口交给 AgentCore 的 Runtime、Memory、Gateway、Identity、Observability 等基础设施运行。

这已经非常接近你材料说的：

“让 Agent 像云原生应用一样被开发、交付和规模化运营。”

最后给你一个判断

如果你们现在正在讨论 CubeStack V3，我会把它定义得比材料里的描述再精确一点：

V3 不是“在 CubeStack 上增加一个 Agent 功能”，而是把 CubeStack 从 AI Workload Cloud 变成 Agent Workload Cloud。

也就是：

V2


GPU
 ↓
AI Job
 ↓
Model
 ↓
Inference

变成：

V3


Agent
 ↓
Agent Runtime
 ↓
Tools / MCP
 ↓
Memory
 ↓
Identity / Policy
 ↓
Deployment / Scaling
 ↓
Observability / Evaluation
 ↓
HITL / Governance

然后 V4 才是：

User
 ↓
Goal
 ↓
Agent
 ↓
Agent decides
 ↓
Cloud APIs
 ↓
Infrastructure

所以如果让我给 CubeStack V3 定一个非常具体的 Benchmark，我不会问：

“我们有没有 Agent Builder？”

我会问：

“一个企业把自己写好的 Agent 交给 CubeStack 后，除了模型和业务 Tool 本身，Agent 从开发到生产所需要的 Runtime、Identity、MCP、Policy、HITL、Memory、Sandbox、Observability、Evaluation、Deployment、Scaling、Audit，是否全部可以由 CubeStack 提供？”

如果答案是 Yes，那才真正进入了你这份战略所说的 V3 Agent Cloud。