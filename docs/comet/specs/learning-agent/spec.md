# 教学 Agent 系统完整规格

## 产品目标

系统是单用户自托管的通用个人学习助理。用户导入任意领域的 Markdown 知识，系统建立结构化知识地图，通过诊断、路线、讲解、主动回忆、评估和复习帮助用户学习，并在多个客户端之间维持可信、可解释、可纠正的连续状态。

项目最初服务于技术面试学习，但课程范围不是硬编码约束。Go 是主要实现语言，不限制系统教授的内容。

## 交付边界

**第一个可运行里程碑 M1。** M1 必须独立部署和使用。它包含 Go 模块化单体服务、PostgreSQL、在线 Go CLI、一次性配对、设备令牌、PostgreSQL canonical Markdown revision、确定性 Markdown 树索引、带 revision 引用的检索、可信最小教学闭环、Learning Event Log、Learner Model、基础查询投影、固定兼容版本的 Nocturne Memory sidecar，以及明确的 OpenAI-compatible profile。

**后续 Supervisor 波次。** M1 之后分别交付离线操作队列与同步裁决、Fast Note Sync/Obsidian 发布与显式导入、MCP transport，以及 Agent 知识维护与风险审批。

**当前 change 之外。** Rust CLI、Web UI、PDF/网页内置解析、多用户和高级个性化调度不属于当前 change 的承诺。

## 仓库与服务端架构

```text
edu-agent/
├── server/
│   ├── cmd/edu-agentd/
│   ├── internal/
│   │   ├── app/
│   │   ├── identity/
│   │   ├── knowledge/
│   │   ├── learning/
│   │   ├── tutoring/
│   │   ├── integrations/
│   │   │   ├── llm/
│   │   │   ├── nocturne/
│   │   │   └── notesync/
│   │   ├── transport/
│   │   │   ├── httpapi/
│   │   │   └── mcp/
│   │   └── platform/
│   │       ├── postgres/
│   │       ├── outbox/
│   │       ├── config/
│   │       └── observability/
│   ├── api/openapi.yaml
│   └── migrations/
├── clients/
│   └── cli-go/
├── deploy/compose.yaml
├── contracttests/
└── docs/
```

代码按业务能力分包，不采用全局 controller/service/repository 横向分层。调用方向是 `transport -> application/domain -> ports -> adapters`，端口接口由消费方定义。HTTP、MCP 和 CLI 使用相同应用用例，transport 只做协议转换、认证、校验和错误映射。

`identity`、`knowledge`、`learning` 和 `tutoring` 分别拥有自己的业务数据，不存在拥有所有实体的通用 Repository。同步是各 aggregate 的应用协议，不是拥有跨模块业务数据的独立领域。M1 是一个 Go 进程，不引入内部网络 RPC、服务发现、分布式事务或消息中间件。

## 数据所有权

Go/PostgreSQL 的 `knowledge` 模块拥有 canonical Markdown、revision、节点身份和 lineage。文件与 Obsidian 只是导入或发布视图。

Go/PostgreSQL 的 `learning` 模块拥有目标、路线、Activity、Attempt、Assessment、Evidence 和复习状态。Go/PostgreSQL 的 `tutoring` 模块拥有教学 focus 与状态机，这些状态不依赖某个模型会话上下文。Go/PostgreSQL 的 `identity` 模块拥有设备、令牌与授权。

Nocturne Memory 拥有经过准入的个人偏好、长期背景和非权威摘要。它不得保存目标、掌握度、答题、路线、误区结论、复习队列或同步状态的权威副本。Go 服务只保存 Nocturne 写入意图、外部引用、状态、回执和删除 generation，不复制其长期记忆正文作为第二权威源。

Go/PostgreSQL Outbox 拥有外部写入意图与回执，采用至少一次投递、幂等、可审计语义。

## 知识文档与树索引

### Canonical revision

导入 Markdown 文件或目录时，服务创建不可变 `KnowledgeRevision` 和文档 revision。PostgreSQL 保存 canonical Markdown、内容哈希、父 revision、来源、创建者和时间。导出使用 YAML frontmatter 与不影响 Obsidian 渲染的隐藏节点标记保存文档和节点身份。

再导入时优先匹配显式身份。缺失身份时才使用路径和标题祖先进行候选匹配，不确定结果必须审阅。

### 确定性索引

Markdown AST 决定标题层级、正文范围、节点身份映射和源引用。LLM 可以生成节点摘要、候选先修关系和检索选择，但这些都是可失效、可重建的派生工件，不能改变节点身份。同一 canonical revision 的全量重建必须得到相同节点树。

移动或改名且语义未变时保持节点身份。重大改写、拆分或合并创建新的 node revision 与 `NodeLineage`。旧学习证据保持绑定原 revision，不自动转移。Agent 可以提出折扣或 provisional 迁移建议，批准后才生效。

### 检索

检索先在文档或主题层选择候选，再逐层在节点摘要和结构上推理，最后读取正文。结果必须包含查询上下文版本、每层候选、选择理由、被选节点、`knowledge_revision_id`、`node_revision_id`、正文范围，以及降级或截断标记。

验收使用固定语料和期望候选，不以模型生成文案是否相同作为判断标准。

## 教学编排

### 显式状态机

M1 状态机如下：

```text
Idle -> GoalReady -> Diagnostic -> RouteActive
RouteActive -> ActivityIssued -> AwaitingResponse -> Evaluating
Evaluating -> Feedback -> AdvanceOrReview -> RouteActive|Completed
RouteActive|ActivityIssued|AwaitingResponse
  -> FocusSuspended -> FreeQuestion -> FreeAnswer -> FocusResumed
  -> FocusFrame.saved_state
```

`FocusFrame` 保存进入自由问答前的 `saved_state`、`route_revision_id`、`route_step_id`、`focus_node_revision_id` 和 Activity/Attempt 上下文。恢复时在一个事务中回到该保存状态和原上下文，除非用户显式结束当前 Activity 或切换目标。

LLM 只产生讲解、题目、评估和路线调整 proposal。确定性编排器验证状态转移、revision 引用、结构化 schema 和写入权限。

### Activity 与评估

Activity 发给用户前冻结目标 node revision、知识 revision、题目正文、题目类型、rubric revision、难度、允许帮助、route revision 和 policy version。

用户提交形成 Attempt。模型评估形成不可变 `AssessmentArtifact`，其中保存逐 rubric 项结论、引用的答案片段、知识引用、模型标识、模型参数、提示 revision、重试信息和风险标志。

模型工件不能直接修改 Learner Model。确定性客观题可以自动接纳。满足版本化结构化 rubric、引用完整且无风险标志的高置信评估可以自动接纳。低置信、知识引用不足、结构不完整或存在争议的开放式评估保持 `provisional`。用户可以确认、覆盖或作废 Assessment，系统保存原因和操作者。

阅读、讲解、用户提问和模型回答只记录 exposure，不直接增加掌握状态。自由问答默认不计分。用户或 Agent 只有显式执行“转为测验”并冻结新的 Activity/rubric 后，后续回答才可能形成 Evidence。

## 学习记录、投影与复习

### 记录模型

核心实体包括 `LearningSession`、`FocusFrame`、`GoalRevision`、`RouteRevision`、`RouteStep`、`Activity`、`Attempt`、`AssessmentArtifact`、`AcceptedEvidence`、`MisconceptionHypothesis`、`MasteryProjection`、`ReviewSchedule`、`LearningEvent` 和 `ProjectionCheckpoint`。

误区是带证据、置信状态和反证能力的 hypothesis，不是不可修改的用户事实。

### 事件与命令

客户端提交 operation 或 observation。服务端在一个事务中使用 `(device_id, operation_id)` 写入 Inbox 并永久去重，检查 aggregate、expected version、Activity 和知识 revision，生成服务端 canonical Learning Event，更新需要 read-your-writes 的核心投影，并写入必要 Outbox。

客户端不能生成评分、掌握更新、路线 revision 或其他权威事件。离线 `occurred_at` 是不可信展示信息，服务端同时记录可信 `received_at`。

### 重放与纠错

“可重建”只指由 Learning Event Log 派生的学习投影，不包括 Nocturne 内容或模型生成摘要。重建输入包含事件 schema version、严格 event sequence、不可变知识/node revision、reducer、接纳 policy、复习 policy、Assessment 和 Evidence 引用。

Projection 保存 checkpoint、`as_of_event_seq` 和版本。schema 变化必须提供 upcaster 或明确停止支持旧版本。业务纠错通过 `AssessmentOverridden`、`EvidenceInvalidated`、`EventRedacted` 等补偿事件完成。派生投影不能被直接手工改值。

### 掌握与复习

M1 使用 `unseen / learning / provisional / retained`，并同时展示有效证据数量、类型、帮助程度、最近时间和不确定性。单次 Assessment 不能直接建立 `retained`，至少需要跨时间的主动回忆证据。

M1 使用版本化固定间隔策略，默认阶梯为可配置的 `1d / 3d / 7d / 14d / 30d`。`ReviewPresented` 不等于成功，只有后续 accepted evidence 才能推进复习状态。系统不在缺少校准数据时声称精确预测遗忘。未来策略必须保留版本以支持历史解释和重放。

## Nocturne Memory 集成

### 部署与契约

Nocturne 是 M1 正式 sidecar，使用固定版本和镜像 digest。它可以与 Go 服务共用 PostgreSQL 实例，但必须使用独立数据库、账号和迁移生命周期，禁止跨库读取业务表。Go 服务通过受认证的适配器调用固定 REST/MCP capability，并运行契约测试。上游升级前必须重新验证 capability matrix。Nocturne Dashboard 可以部署，但不是教学业务管理界面。

### 记忆准入

生命周期是 `MemoryCandidate -> admitted|rejected -> MemoryRecord -> superseded|deleted`。每个 Candidate 包含来源事件、提议内容、敏感级别、有效期、候选 URI、提议者和理由。

用户明确陈述、稳定、非敏感的交互偏好或时间约束可以按版本化规则自动 admitted。模型推断、敏感信息、长期背景和生成摘要必须先审阅。生成摘要标记为非权威模型工件，不得伪装成用户陈述。原始聊天、完整答题、掌握度、误区结论、路线和复习队列禁止写入 Nocturne。

### Outbox 与降级

外部写入使用至少一次 Outbox。每条消息包含幂等键、实体 revision 和 generation。`applied` 表示固定契约已确认，`queued` 表示教学事务成功且长期记忆写入等待重试，`rejected` 表示策略或永久契约错误且不再自动重试。

旧 revision 或旧 generation 的消息不得覆盖新值或复活已删除记忆。超过重试预算的消息进入死信并可人工重放。

## 隐私清除

隐私清除和业务纠错是不同操作。清除时先建立 tombstone/generation barrier，再取消相关未执行 Outbox/Inbox payload，清除或加密销毁活动数据库中的可识别正文和引用，清除索引、缓存和查询投影中的内容，在 Nocturne 中解除所有路径并清理相关 orphan 与历史版本，重新查询验证后生成逐存储位置的删除回执。

删除回执记录备份最长 30 天的最终不可恢复截止时间。事件只保留最小审计元数据，正文使用可 redaction 的 payload 引用。重放遇到 redacted 事件时按版本化 no-op 或补偿语义处理。系统不能承诺清除已发送给外部模型供应商的数据，也不能在备份截止时间前宣称备份已彻底清除。

## API、CLI 与未来 Web

### HTTP 与模型 profile

OpenAPI 是 HTTP 契约。M1 的模型 profile 明确约定 Chat Completions 风格消息、结构化 JSON 输出、错误语义、超时和最低上下文要求。原生 JSON Schema 与流式输出可以有能力协商，工具调用不是核心教学流程的必需能力。fake model server 用于确定性测试。

### Go CLI

M1 Go CLI 支持配对、设备状态、注销、导入知识、设置目标、开始或继续教学、回答、查看 rubric 评估、确认或覆盖 provisional 结果、自由提问、显式转为测验、恢复 focus，以及查看路线、证据和复习。

CLI 默认使用低颜色、无显眼学习标识的文本模式并支持快速清屏。清屏只清除应用当前显示，不承诺清除终端 scrollback、Shell 历史或系统日志。

### 查询投影

面向 CLI 和未来 Web 的查询返回时间线、会话、当前及历史路线、知识节点状态、最近有效证据、误区 hypothesis、今日与未来复习、`as_of_event_seq`、projection version、knowledge revision，以及 rebuilding、degraded 和 incomplete 标记。

统计时间只能标记为估计值，不能把不可信离线设备时钟当作精确学习时长。

## 后续波次契约

### 离线同步

离线客户端只保存服务端预签发 Activity 和 client operation/observation。每项包含 `operation_id`、`device_id`、`device_seq`、`aggregate_id`、`expected_version`、`activity_revision`、`occurred_at` 和 payload schema version。

服务端分别返回操作是否存档和证据是否接纳。重复操作不重复计分。过期知识 revision 的 Attempt 可以保留审计，但默认 provisional。多设备完成同一 Activity 时按明确 policy 处理，不按设备时钟静默覆盖。

### Fast Note Sync/Obsidian

系统固定 Fast Note Sync 最低版本、测试版本和 API capability matrix。发布 Markdown 包含 source revision，Outbox 使用单调 revision，旧消息不能覆盖新笔记。Fast Note Sync 不是 canonical 知识存储或冲突协调器。

Obsidian 修改只通过显式导入进入系统。系统根据 base、local 和 remote revision 生成三方差异，冲突必须审阅。

### MCP

MCP 只包装现有应用用例，共用设备身份、授权、审计和错误语义。通用 Agent 不能绕过 Assessment 接纳、知识修改审批或 Memory Candidate 准入策略。

### Agent 知识维护

Agent 先生成带来源、base revision、diff、lineage 影响和风险等级的 proposal。新增和局部低风险修订可以按规则自动应用。移动、重大改写、拆分、合并和删除必须审批。应用后产生新 canonical revision，回退通过新的反向 revision 完成而不删除历史。与学习证据相关的 lineage 迁移必须独立审批。

## 安全与部署

服务默认监听 loopback。非 loopback 部署应由 HTTPS 反向代理终止 TLS。显式不安全开关仅供受信网络并产生持续告警。

首次配对码由服务器本地管理命令生成，具有高熵、短 TTL、一次性和尝试限流。设备令牌使用高熵随机值，只在签发时返回，数据库只保存哈希。令牌包含设备身份和作用域，可以单独吊销。

Nocturne、Fast Note Sync 和模型凭据只保存在服务端 secret/config provider 中，不返回客户端、不进入日志或导出。HTTP 与 MCP 在进入应用用例前执行统一认证、授权、速率限制和审计。

## 验证策略

领域与应用规则使用单元测试。PostgreSQL 使用真实数据库集成测试，不维护 SQLite 生产方言。模型使用 fake server 验证 schema 错误、低置信、超时、重试和模型升级回归。

知识树比较增量更新与全量重建，学习投影比较增量 reducer 与从零重放。Inbox/Outbox、设备认证、Nocturne、删除 fence、Fast Note Sync 和离线同步执行重复、乱序、并发与故障注入测试。外部依赖按固定版本运行契约测试，升级不自动视为兼容。
