---
generated_from_state_version: 9
---

# Verification

## Current result

- Result: **Passed**
- Assurance: **skill-coordinated**
- Goal cycle: 1
- Iteration: 1
- Verifier attempt: 2
- Completed: 2026-08-28T13:52:47.154Z
- Summary: A1-A75全部passed；10个child均archive/pass/finish=merge并集成，candidate与operations聚合证据一致，无行为输入漂移或正式合同缺口。

## Acceptance

| ID | Result | Source | Criterion | Reason |
| --- | --- | --- | --- | --- |
| A1 | passed | brief.md | A1：部署配置能启动 Go 服务与 PostgreSQL；服务默认只监听 loopback，非 loopback 明文监听必须显式启用并产生安全告警，健康接口区分存活、就绪和依赖降级。 | 部署、默认loopback、安全告警及分层健康检查证据完整。 |
| A2 | passed | brief.md | A2：本地管理命令生成高熵、短时、一次性配对码；CLI 换取独立设备令牌后可认证，配对码重放失败，令牌仅保存哈希并可按设备吊销和限流。 | 一次性配对、令牌哈希、限流和按设备吊销均已验证。 |
| A3 | passed | brief.md | A3：系统定义并通过 fake server 验证明确的 OpenAI-compatible profile；不满足必需能力的端点在启动或模型探测时返回可诊断错误，密钥不进入日志、导出或客户端缓存。 | 固定模型profile、错误分类、fake server和密钥脱敏均成立。 |
| A4 | passed | brief.md | A4：导入 Markdown 文件或目录会在 PostgreSQL 创建不可变 canonical revision；导出后仍是 Obsidian 可读 Markdown，并携带可移植的文档和节点标识。 | Markdown导入创建不可变canonical revision并可携带稳定身份导出。 |
| A5 | passed | brief.md | A5：确定性 Markdown AST 能从同一 canonical revision 重建相同节点树；移动/改名保持语义身份，重大改写、拆分或合并生成 lineage，不确定映射不会自动迁移。 | 确定性AST、稳定身份、保守lineage及歧义审阅均已覆盖。 |
| A6 | passed | brief.md | A6：固定测试语料上的树检索返回期望候选章节、逐层选择轨迹和 revision 引用；运行时不需要向量数据库，LLM 摘要变化不会改变节点身份。 | 固定语料检索返回冻结revision、分层轨迹和确定性候选。 |
| A7 | passed | brief.md | A7：教学编排器按显式状态机推进，并能暂停到自由问答后恢复原 route revision、step 和 focus；自由问答只有显式转为冻结 Activity 后才可能计分。 | 显式教学状态机及自由问答后的原上下文恢复均成立。 |
| A8 | passed | brief.md | A8：每个 Activity 冻结知识 revision、节点、rubric、难度和帮助规则；Assessment 保存逐项结论、答案证据、知识引用、模型与提示版本及置信状态，模型输出不能直接修改 Learner Model。 | Activity冻结完整引用，Assessment保存证据且模型无直接写权限。 |
| A9 | passed | brief.md | A9：只有 accepted evidence 能更新 `unseen / learning / provisional / retained` 状态和复习计划；低置信或争议评估保持 provisional，可确认、覆盖或作废，阅读和讲解不会直接增加掌握状态。 | 仅accepted Evidence推进掌握与复习，provisional可审阅纠正。 |
| A10 | passed | brief.md | A10：相同 Learning Event Log、不可变知识 revision 和 reducer/policy 版本能重建相同学习投影；纠错通过补偿、覆盖或 redaction 事件完成，投影重建不会恢复已失效证据。 | 增量投影与从零重放一致，补偿和redaction不复活旧事实。 |
| A11 | passed | brief.md | A11：两个在线设备提交带幂等键和 expected version 的操作后收敛到同一服务端状态；重复操作不重复计分，版本冲突返回机器可读结果而不静默覆盖。 | Inbox幂等、expected version冲突及多设备收敛均已验证。 |
| A12 | passed | brief.md | A12：用户明确陈述且稳定的非敏感偏好可按规则进入 Nocturne；模型推断、敏感信息和生成摘要先成为 Memory Candidate，审阅状态、来源、有效期和敏感级别可查询。 | Nocturne分级准入、Candidate审阅和禁止内容边界完整。 |
| A13 | passed | brief.md | A13：固定版本的 Nocturne sidecar 随部署栈启动并通过契约测试；故障时教学状态保持可用，长期记忆写入返回 `applied / queued / rejected`，恢复后按幂等和 generation fence 处理而不复活已删除记忆。 | 固定Nocturne契约、降级、unknown对账及generation fence成立。 |
| A14 | passed | brief.md | A14：在线 Go CLI 可完成配对、导入知识、开始或继续学习、回答、查看评估与进度、自由提问并返回原 focus；默认是低颜色、无显眼学习标识的文本模式并支持快速清屏。 | Go CLI覆盖完整在线教学流程并符合低可见终端要求。 |
| A15 | passed | brief.md | A15：稳定查询 API 返回学习时间线、当前路线、知识节点状态、证据、复习计划和投影元数据，包括 `as_of_event_seq`、知识 revision、projection version 与降级状态，无需解析 Nocturne 或原始聊天。 | 稳定查询API返回路线、证据、复习及完整投影元数据。 |
| A16 | passed | brief.md | A16：隐私清除先建立 tombstone/generation barrier，取消相关待处理写入，并清除活动库、索引、缓存和 Nocturne 活动数据；删除回执列出结果和最多 30 天的备份不可恢复截止时间，截止前不宣称备份已彻底清除。 | 隐私barrier、owner scrub、远端排空和删除回执流程完整。 |
| A17 | passed | brief.md | A17：后续离线 CLI 只排队带设备序号和 revision 的 client operation/observation；联网后服务端生成 canonical event，并分别返回操作存档结果与证据是否接纳，过期知识 revision 不会静默计分。 | 离线操作独立存档与Evidence裁决，过期revision不会静默计分。 |
| A18 | passed | brief.md | A18：Fast Note Sync 适配器按固定契约发布带 source revision 的 Markdown，旧 Outbox 消息不能覆盖新版本；Obsidian 修改只经显式导入，基于 base revision 生成三方差异并在冲突时等待审阅。 | NoteSync单调发布、三方审阅和review_import回发闭环成立。 |
| A19 | passed | brief.md | A19：MCP transport 与 HTTP/CLI 调用同一应用用例和授权策略；通用 Agent 接入后使用同一知识、学习状态和 Nocturne namespace，不创建另一份业务真值。 | HTTP、CLI和MCP共用应用用例、授权及唯一业务真值。 |
| A20 | passed | brief.md | A20：Agent 知识维护先生成带来源和风险等级的 proposal；新增和局部低风险修改可按策略应用，移动、重大重写、拆分、合并和删除必须审批，应用后形成新 revision、lineage 和可回退历史。 | 知识维护proposal、风险审批、lineage和反向revision均成立。 |
| A21 | passed | specs/learning-agent/spec.md | 系统是单用户自托管的通用个人学习助理。用户导入任意领域的 Markdown 知识，系统建立结构化知识地图，通过诊断、路线、讲解、主动回忆、评估和复习帮助用户学习，并在多个客户端之间维持可信、可解释、可纠正的连续状态。 | 系统形成可部署、可使用的通用个人学习助理。 |
| A22 | passed | specs/learning-agent/spec.md | 项目最初服务于技术面试学习，但课程范围不是硬编码约束。Go 是主要实现语言，不限制系统教授的内容。 | 课程领域未硬编码为技术面试、Go或其他固定分类。 |
| A23 | passed | specs/learning-agent/spec.md | **第一个可运行里程碑 M1。** M1 必须独立部署和使用。它包含 Go 模块化单体服务、PostgreSQL、在线 Go CLI、一次性配对、设备令牌、PostgreSQL canonical Markdown revision、确定性 Markdown 树索引、带 revision 引用的检索、可信最小教学闭环、Learning Event Log、Learner Model、基础查询投影、固定兼容版本的 Nocturne Memory sidecar，以及明确的 OpenAI-compatible profile。 | M1服务、PostgreSQL、CLI、教学闭环和Nocturne均完整交付。 |
| A24 | passed | specs/learning-agent/spec.md | **后续 Supervisor 波次。** M1 之后分别交付离线操作队列与同步裁决、Fast Note Sync/Obsidian 发布与显式导入、MCP transport，以及 Agent 知识维护与风险审批。 | 离线、NoteSync、MCP和知识维护后续波次均已归档集成。 |
| A25 | passed | specs/learning-agent/spec.md | **当前 change 之外。** Rust CLI、Web UI、PDF/网页内置解析、多用户和高级个性化调度不属于当前 change 的承诺。 | 未引入Rust CLI、Web UI、多用户或内置PDF网页解析。 |
| A26 | passed | specs/learning-agent/spec.md | 代码按业务能力分包，不采用全局 controller/service/repository 横向分层。调用方向是 `transport -> application/domain -> ports -> adapters`，端口接口由消费方定义。HTTP、MCP 和 CLI 使用相同应用用例，transport 只做协议转换、认证、校验和错误映射。 | 代码按业务能力和Ports/Adapters组织，transport保持薄层。 |
| A27 | passed | specs/learning-agent/spec.md | `identity`、`knowledge`、`learning` 和 `tutoring` 分别拥有自己的业务数据，不存在拥有所有实体的通用 Repository。同步是各 aggregate 的应用协议，不是拥有跨模块业务数据的独立领域。M1 是一个 Go 进程，不引入内部网络 RPC、服务发现、分布式事务或消息中间件。 | 各业务owner边界明确，未引入万能Repository或内部RPC。 |
| A28 | passed | specs/learning-agent/spec.md | Go/PostgreSQL 的 `knowledge` 模块拥有 canonical Markdown、revision、节点身份和 lineage。文件与 Obsidian 只是导入或发布视图。 | canonical Markdown、revision、节点身份和lineage归knowledge所有。 |
| A29 | passed | specs/learning-agent/spec.md | Go/PostgreSQL 的 `learning` 模块拥有目标、路线、Activity、Attempt、Assessment、Evidence 和复习状态。Go/PostgreSQL 的 `tutoring` 模块拥有教学 focus 与状态机，这些状态不依赖某个模型会话上下文。Go/PostgreSQL 的 `identity` 模块拥有设备、令牌与授权。 | learning、tutoring和identity分别拥有其权威业务状态。 |
| A30 | passed | specs/learning-agent/spec.md | Nocturne Memory 拥有经过准入的个人偏好、长期背景和非权威摘要。它不得保存目标、掌握度、答题、路线、误区结论、复习队列或同步状态的权威副本。Go 服务只保存 Nocturne 写入意图、外部引用、状态、回执和删除 generation，不复制其长期记忆正文作为第二权威源。 | Nocturne仅保存准入长期记忆，不承载教学业务权威副本。 |
| A31 | passed | specs/learning-agent/spec.md | Go/PostgreSQL Outbox 拥有外部写入意图与回执，采用至少一次投递、幂等、可审计语义。 | PostgreSQL Outbox提供至少一次、幂等、可审计投递语义。 |
| A32 | passed | specs/learning-agent/spec.md | 导入 Markdown 文件或目录时，服务创建不可变 `KnowledgeRevision` 和文档 revision。PostgreSQL 保存 canonical Markdown、内容哈希、父 revision、来源、创建者和时间。导出使用 YAML frontmatter 与不影响 Obsidian 渲染的隐藏节点标记保存文档和节点身份。 | 知识revision保存完整来源、父版本、hash和canonical正文。 |
| A33 | passed | specs/learning-agent/spec.md | 再导入时优先匹配显式身份。缺失身份时才使用路径和标题祖先进行候选匹配，不确定结果必须审阅。 | 再导入优先显式身份，非确定映射必须进入审阅。 |
| A34 | passed | specs/learning-agent/spec.md | Markdown AST 决定标题层级、正文范围、节点身份映射和源引用。LLM 可以生成节点摘要、候选先修关系和检索选择，但这些都是可失效、可重建的派生工件，不能改变节点身份。同一 canonical revision 的全量重建必须得到相同节点树。 | AST决定树与范围，LLM派生工件不影响canonical身份。 |
| A35 | passed | specs/learning-agent/spec.md | 移动或改名且语义未变时保持节点身份。重大改写、拆分或合并创建新的 node revision 与 `NodeLineage`。旧学习证据保持绑定原 revision，不自动转移。Agent 可以提出折扣或 provisional 迁移建议，批准后才生效。 | 移动保留身份，重大变化建立lineage且证据不自动迁移。 |
| A36 | passed | specs/learning-agent/spec.md | 检索先在文档或主题层选择候选，再逐层在节点摘要和结构上推理，最后读取正文。结果必须包含查询上下文版本、每层候选、选择理由、被选节点、`knowledge_revision_id`、`node_revision_id`、正文范围，以及降级或截断标记。 | 检索响应包含逐层候选、理由、revision、范围及降级标志。 |
| A37 | passed | specs/learning-agent/spec.md | 验收使用固定语料和期望候选，不以模型生成文案是否相同作为判断标准。 | 验收比较固定结构证据，不依赖模型自由文本一致。 |
| A38 | passed | specs/learning-agent/spec.md | M1 状态机如下： | 主教学链和自由问答子链的状态与转换完整实现。 |
| A39 | passed | specs/learning-agent/spec.md | `FocusFrame` 保存进入自由问答前的 `saved_state`、`route_revision_id`、`route_step_id`、`focus_node_revision_id` 和 Activity/Attempt 上下文。恢复时在一个事务中回到该保存状态和原上下文，除非用户显式结束当前 Activity 或切换目标。 | FocusFrame冻结全部必要上下文并在事务中精确恢复。 |
| A40 | passed | specs/learning-agent/spec.md | LLM 只产生讲解、题目、评估和路线调整 proposal。确定性编排器验证状态转移、revision 引用、结构化 schema 和写入权限。 | LLM仅生成proposal，确定性编排器控制状态与写入。 |
| A41 | passed | specs/learning-agent/spec.md | Activity 发给用户前冻结目标 node revision、知识 revision、题目正文、题目类型、rubric revision、难度、允许帮助、route revision 和 policy version。 | Activity在展示前冻结知识、路线、rubric、帮助和策略。 |
| A42 | passed | specs/learning-agent/spec.md | 用户提交形成 Attempt。模型评估形成不可变 `AssessmentArtifact`，其中保存逐 rubric 项结论、引用的答案片段、知识引用、模型标识、模型参数、提示 revision、重试信息和风险标志。 | AssessmentArtifact不可变保存逐项结论、引用及模型元数据。 |
| A43 | passed | specs/learning-agent/spec.md | 模型工件不能直接修改 Learner Model。确定性客观题可以自动接纳。满足版本化结构化 rubric、引用完整且无风险标志的高置信评估可以自动接纳。低置信、知识引用不足、结构不完整或存在争议的开放式评估保持 `provisional`。用户可以确认、覆盖或作废 Assessment，系统保存原因和操作者。 | 客观题与高置信开放题接纳规则及人工纠正流程完整。 |
| A44 | passed | specs/learning-agent/spec.md | 阅读、讲解、用户提问和模型回答只记录 exposure，不直接增加掌握状态。自由问答默认不计分。用户或 Agent 只有显式执行“转为测验”并冻结新的 Activity/rubric 后，后续回答才可能形成 Evidence。 | 阅读、讲解和自由问答仅记录exposure，转测验后才可计分。 |
| A45 | passed | specs/learning-agent/spec.md | 核心实体包括 `LearningSession`、`FocusFrame`、`GoalRevision`、`RouteRevision`、`RouteStep`、`Activity`、`Attempt`、`AssessmentArtifact`、`AcceptedEvidence`、`MisconceptionHypothesis`、`MasteryProjection`、`ReviewSchedule`、`LearningEvent` 和 `ProjectionCheckpoint`。 | 正式规格要求的学习、评估、投影与事件实体均已实现。 |
| A46 | passed | specs/learning-agent/spec.md | 误区是带证据、置信状态和反证能力的 hypothesis，不是不可修改的用户事实。 | 误区按带证据、可反证的版本化hypothesis管理。 |
| A47 | passed | specs/learning-agent/spec.md | 客户端提交 operation 或 observation。服务端在一个事务中使用 `(device_id, operation_id)` 写入 Inbox 并永久去重，检查 aggregate、expected version、Activity 和知识 revision，生成服务端 canonical Learning Event，更新需要 read-your-writes 的核心投影，并写入必要 Outbox。 | 操作在同一事务完成去重、版本检查、事件、投影和Outbox。 |
| A48 | passed | specs/learning-agent/spec.md | 客户端不能生成评分、掌握更新、路线 revision 或其他权威事件。离线 `occurred_at` 是不可信展示信息，服务端同时记录可信 `received_at`。 | 客户端不能生成权威事件，服务端记录可信received_at。 |
| A49 | passed | specs/learning-agent/spec.md | “可重建”只指由 Learning Event Log 派生的学习投影，不包括 Nocturne 内容或模型生成摘要。重建输入包含事件 schema version、严格 event sequence、不可变知识/node revision、reducer、接纳 policy、复习 policy、Assessment 和 Evidence 引用。 | 重放输入绑定事件版本、严格序列、不可变引用和策略版本。 |
| A50 | passed | specs/learning-agent/spec.md | Projection 保存 checkpoint、`as_of_event_seq` 和版本。schema 变化必须提供 upcaster 或明确停止支持旧版本。业务纠错通过 `AssessmentOverridden`、`EvidenceInvalidated`、`EventRedacted` 等补偿事件完成。派生投影不能被直接手工改值。 | 投影版本、checkpoint、upcaster及补偿事件语义完整。 |
| A51 | passed | specs/learning-agent/spec.md | M1 使用 `unseen / learning / provisional / retained`，并同时展示有效证据数量、类型、帮助程度、最近时间和不确定性。单次 Assessment 不能直接建立 `retained`，至少需要跨时间的主动回忆证据。 | 四态掌握模型及跨时间主动回忆retained门槛正确。 |
| A52 | passed | specs/learning-agent/spec.md | M1 使用版本化固定间隔策略，默认阶梯为可配置的 `1d / 3d / 7d / 14d / 30d`。`ReviewPresented` 不等于成功，只有后续 accepted evidence 才能推进复习状态。系统不在缺少校准数据时声称精确预测遗忘。未来策略必须保留版本以支持历史解释和重放。 | 固定间隔策略版本化，ReviewPresented本身不推进复习。 |
| A53 | passed | specs/learning-agent/spec.md | Nocturne 是 M1 正式 sidecar，使用固定版本和镜像 digest。它可以与 Go 服务共用 PostgreSQL 实例，但必须使用独立数据库、账号和迁移生命周期，禁止跨库读取业务表。Go 服务通过受认证的适配器调用固定 REST/MCP capability，并运行契约测试。上游升级前必须重新验证 capability matrix。Nocturne Dashboard 可以部署，但不是教学业务管理界面。 | Nocturne版本、镜像digest、独立数据库和能力矩阵均锁定。 |
| A54 | passed | specs/learning-agent/spec.md | 生命周期是 `MemoryCandidate -> admitted\|rejected -> MemoryRecord -> superseded\|deleted`。每个 Candidate 包含来源事件、提议内容、敏感级别、有效期、候选 URI、提议者和理由。 | MemoryCandidate到Record再到superseded或deleted生命周期完整。 |
| A55 | passed | specs/learning-agent/spec.md | 用户明确陈述、稳定、非敏感的交互偏好或时间约束可以按版本化规则自动 admitted。模型推断、敏感信息、长期背景和生成摘要必须先审阅。生成摘要标记为非权威模型工件，不得伪装成用户陈述。原始聊天、完整答题、掌握度、误区结论、路线和复习队列禁止写入 Nocturne。 | 仅明确稳定非敏感偏好可自动准入，其余必须审阅。 |
| A56 | passed | specs/learning-agent/spec.md | 外部写入使用至少一次 Outbox。每条消息包含幂等键、实体 revision 和 generation。`applied` 表示固定契约已确认，`queued` 表示教学事务成功且长期记忆写入等待重试，`rejected` 表示策略或永久契约错误且不再自动重试。 | 记忆Outbox绑定幂等键、revision、generation及三态结果。 |
| A57 | passed | specs/learning-agent/spec.md | 旧 revision 或旧 generation 的消息不得覆盖新值或复活已删除记忆。超过重试预算的消息进入死信并可人工重放。 | 旧revision和generation被fence，死信支持受控人工重放。 |
| A58 | passed | specs/learning-agent/spec.md | 隐私清除和业务纠错是不同操作。清除时先建立 tombstone/generation barrier，再取消相关未执行 Outbox/Inbox payload，清除或加密销毁活动数据库中的可识别正文和引用，清除索引、缓存和查询投影中的内容，在 Nocturne 中解除所有路径并清理相关 orphan 与历史版本，重新查询验证后生成逐存储位置的删除回执。 | 清除先建立barrier，再清理全部owner与Nocturne引用。 |
| A59 | passed | specs/learning-agent/spec.md | 删除回执记录备份最长 30 天的最终不可恢复截止时间。事件只保留最小审计元数据，正文使用可 redaction 的 payload 引用。重放遇到 redacted 事件时按版本化 no-op 或补偿语义处理。系统不能承诺清除已发送给外部模型供应商的数据，也不能在备份截止时间前宣称备份已彻底清除。 | 删除回执、最小审计、redacted replay及外部限制如实表达。 |
| A60 | passed | specs/learning-agent/spec.md | OpenAPI 是 HTTP 契约。M1 的模型 profile 明确约定 Chat Completions 风格消息、结构化 JSON 输出、错误语义、超时和最低上下文要求。原生 JSON Schema 与流式输出可以有能力协商，工具调用不是核心教学流程的必需能力。fake model server 用于确定性测试。 | OpenAPI与模型profile明确消息、JSON、错误、超时和能力协商。 |
| A61 | passed | specs/learning-agent/spec.md | M1 Go CLI 支持配对、设备状态、注销、导入知识、设置目标、开始或继续教学、回答、查看 rubric 评估、确认或覆盖 provisional 结果、自由提问、显式转为测验、恢复 focus，以及查看路线、证据和复习。 | CLI完整支持配对、学习、评估裁决、自由问答和查询。 |
| A62 | passed | specs/learning-agent/spec.md | CLI 默认使用低颜色、无显眼学习标识的文本模式并支持快速清屏。清屏只清除应用当前显示，不承诺清除终端 scrollback、Shell 历史或系统日志。 | CLI默认低颜色、中性文本并正确声明清屏能力边界。 |
| A63 | passed | specs/learning-agent/spec.md | 面向 CLI 和未来 Web 的查询返回时间线、会话、当前及历史路线、知识节点状态、最近有效证据、误区 hypothesis、今日与未来复习、`as_of_event_seq`、projection version、knowledge revision，以及 rebuilding、degraded 和 incomplete 标记。 | 查询覆盖时间线、路线、节点、证据、误区和复习元数据。 |
| A64 | passed | specs/learning-agent/spec.md | 统计时间只能标记为估计值，不能把不可信离线设备时钟当作精确学习时长。 | 学习时间明确为估算且不使用不可信设备时钟排序。 |
| A65 | passed | specs/learning-agent/spec.md | 离线客户端只保存服务端预签发 Activity 和 client operation/observation。每项包含 `operation_id`、`device_id`、`device_seq`、`aggregate_id`、`expected_version`、`activity_revision`、`occurred_at` 和 payload schema version。 | 离线项包含预签发Activity和完整操作、设备及版本字段。 |
| A66 | passed | specs/learning-agent/spec.md | 服务端分别返回操作是否存档和证据是否接纳。重复操作不重复计分。过期知识 revision 的 Attempt 可以保留审计，但默认 provisional。多设备完成同一 Activity 时按明确 policy 处理，不按设备时钟静默覆盖。 | 离线响应区分存档与Evidence，重复和陈旧项按策略处理。 |
| A67 | passed | specs/learning-agent/spec.md | 系统固定 Fast Note Sync 最低版本、测试版本和 API capability matrix。发布 Markdown 包含 source revision，Outbox 使用单调 revision，旧消息不能覆盖新笔记。Fast Note Sync 不是 canonical 知识存储或冲突协调器。 | NoteSync版本矩阵、source revision和单调发布语义完整。 |
| A68 | passed | specs/learning-agent/spec.md | Obsidian 修改只通过显式导入进入系统。系统根据 base、local 和 remote revision 生成三方差异，冲突必须审阅。 | Obsidian变更仅经显式三方审阅进入canonical知识。 |
| A69 | passed | specs/learning-agent/spec.md | MCP 只包装现有应用用例，共用设备身份、授权、审计和错误语义。通用 Agent 不能绕过 Assessment 接纳、知识修改审批或 Memory Candidate 准入策略。 | MCP共用认证、审计和应用服务且不能绕过业务门禁。 |
| A70 | passed | specs/learning-agent/spec.md | Agent 先生成带来源、base revision、diff、lineage 影响和风险等级的 proposal。新增和局部低风险修订可以按规则自动应用。移动、重大改写、拆分、合并和删除必须审批。应用后产生新 canonical revision，回退通过新的反向 revision 完成而不删除历史。与学习证据相关的 lineage 迁移必须独立审批。 | Agent proposal绑定来源、diff和风险，高风险操作必须审批。 |
| A71 | passed | specs/learning-agent/spec.md | 服务默认监听 loopback。非 loopback 部署应由 HTTPS 反向代理终止 TLS。显式不安全开关仅供受信网络并产生持续告警。 | 服务默认loopback，非loopback明文必须显式启用并持续告警。 |
| A72 | passed | specs/learning-agent/spec.md | 首次配对码由服务器本地管理命令生成，具有高熵、短 TTL、一次性和尝试限流。设备令牌使用高熵随机值，只在签发时返回，数据库只保存哈希。令牌包含设备身份和作用域，可以单独吊销。 | 配对码与设备令牌满足高熵、短期、哈希存储和独立吊销。 |
| A73 | passed | specs/learning-agent/spec.md | Nocturne、Fast Note Sync 和模型凭据只保存在服务端 secret/config provider 中，不返回客户端、不进入日志或导出。HTTP 与 MCP 在进入应用用例前执行统一认证、授权、速率限制和审计。 | 外部凭据仅存服务端，HTTP与MCP统一认证授权和审计。 |
| A74 | passed | specs/learning-agent/spec.md | 领域与应用规则使用单元测试。PostgreSQL 使用真实数据库集成测试，不维护 SQLite 生产方言。模型使用 fake server 验证 schema 错误、低置信、超时、重试和模型升级回归。 | 单元、真实PostgreSQL、fake模型及固定依赖契约策略均成立。 |
| A75 | passed | specs/learning-agent/spec.md | 知识树比较增量更新与全量重建，学习投影比较增量 reducer 与从零重放。Inbox/Outbox、设备认证、Nocturne、删除 fence、Fast Note Sync 和离线同步执行重复、乱序、并发与故障注入测试。外部依赖按固定版本运行契约测试，升级不自动视为兼容。 | 重建对比、幂等、乱序、并发、故障注入和外部锁定证据完整。 |

## Checks

| Check | Command | Working directory | Status | Exit | Duration |
| --- | --- | --- | --- | ---: | ---: |
| Integrated server test suite | test -count=1 ./... | server | passed | 0 | 2180 ms |
| Integrated server race suite | test -race -count=1 ./... | server | passed | 0 | 9410 ms |
| Integrated server go vet | vet ./... | server | passed | 0 | 179 ms |
| Integrated CLI test suite | test -count=1 ./... | clients/cli-go | passed | 0 | 19693 ms |
| Integrated CLI race suite | test -race -count=1 ./... | clients/cli-go | passed | 0 | 63184 ms |
| Integrated CLI go vet | vet ./... | clients/cli-go | passed | 0 | 111 ms |
| Integrated operations coordinator tests | test -count=1 ./... | contracttests/operations | passed | 0 | 208 ms |
| Integrated operations coordinator race tests | test -race -count=1 ./... | contracttests/operations | passed | 0 | 1291 ms |
| Integrated operations coordinator go vet | vet ./... | contracttests/operations | passed | 0 | 61 ms |
| Integrated qualification runner shell syntax | -n scripts/test-operations-candidate.sh scripts/test-postgres-candidate.sh scripts/test-notesync-candidate.sh contracttests/nocturne/run-compose-e2e.sh | . | passed | 0 | 3 ms |
| Integrated Git diff whitespace check | diff --check | . | passed | 0 | 4 ms |

## Blockers

_None._

## Risks and skipped work

_None reported._

## Previous iterations

| Goal cycle | Iteration | Attempt | Outcome | Unresolved | Summary | Completed |
| ---: | ---: | ---: | --- | --- | --- | --- |
| 1 | 1 | 2 | pass | — | A1-A75全部passed；10个child均archive/pass/finish=merge并集成，candidate与operations聚合证据一致，无行为输入漂移或正式合同缺口。 | 2026-08-28T13:52:47.154Z |

## Conclusion

A1-A75全部passed；10个child均archive/pass/finish=merge并集成，candidate与operations聚合证据一致，无行为输入漂移或正式合同缺口。
