# Outcome

构建一个单用户自托管的通用个人学习助理。系统以独立 Go 服务保存可信学习记录、知识库和跨设备状态，以 Nocturne Memory 保存受控的长期个人记忆，以低可见度 CLI 作为首个客户端，并在后续通过 HTTP/MCP 和 Obsidian 集成扩展到其他客户端。

# Scope

系统导入任意领域的结构化 Markdown，建立可追踪、无向量的层级知识索引，并执行由诊断、路线、讲解、主动回忆、评估、反馈和间隔复习组成的结构化教学闭环。版本化学习事件、证据和查询投影构成可信 Learner Model。

第一个可运行里程碑交付 Go 服务、PostgreSQL、在线 Go CLI、知识核心、最小教学闭环和 Nocturne Memory 集成。后续 Supervisor 波次交付离线同步、Fast Note Sync/Obsidian、MCP 和 Agent 知识维护。服务端保留未来 Web 所需的稳定查询 API，但当前 change 不实现 Web UI。

# Non-goals

第一个里程碑不实现 Rust CLI、离线学习、MCP、Fast Note Sync 或 Agent 自动维护知识文档。当前 change 不实现桌面端、移动端、Web UI、多租户、组织或管理员系统，也不内置固定面试课程、PDF 或网页解析。

系统不依赖向量数据库，不完整复刻 PageIndex、Nocturne Memory 或 Fast Note Sync，不承诺模型评分绝对正确、精确预测遗忘或兼容所有自称 OpenAI-compatible 的端点。CLI 清屏只覆盖应用当前显示，不承诺清除终端 scrollback、Shell 历史、系统审计或远程终端日志。

# Acceptance examples

- A1：部署配置能启动 Go 服务与 PostgreSQL；服务默认只监听 loopback，非 loopback 明文监听必须显式启用并产生安全告警，健康接口区分存活、就绪和依赖降级。
- A2：本地管理命令生成高熵、短时、一次性配对码；CLI 换取独立设备令牌后可认证，配对码重放失败，令牌仅保存哈希并可按设备吊销和限流。
- A3：系统定义并通过 fake server 验证明确的 OpenAI-compatible profile；不满足必需能力的端点在启动或模型探测时返回可诊断错误，密钥不进入日志、导出或客户端缓存。
- A4：导入 Markdown 文件或目录会在 PostgreSQL 创建不可变 canonical revision；导出后仍是 Obsidian 可读 Markdown，并携带可移植的文档和节点标识。
- A5：确定性 Markdown AST 能从同一 canonical revision 重建相同节点树；移动/改名保持语义身份，重大改写、拆分或合并生成 lineage，不确定映射不会自动迁移。
- A6：固定测试语料上的树检索返回期望候选章节、逐层选择轨迹和 revision 引用；运行时不需要向量数据库，LLM 摘要变化不会改变节点身份。
- A7：教学编排器按显式状态机推进，并能暂停到自由问答后恢复原 route revision、step 和 focus；自由问答只有显式转为冻结 Activity 后才可能计分。
- A8：每个 Activity 冻结知识 revision、节点、rubric、难度和帮助规则；Assessment 保存逐项结论、答案证据、知识引用、模型与提示版本及置信状态，模型输出不能直接修改 Learner Model。
- A9：只有 accepted evidence 能更新 `unseen / learning / provisional / retained` 状态和复习计划；低置信或争议评估保持 provisional，可确认、覆盖或作废，阅读和讲解不会直接增加掌握状态。
- A10：相同 Learning Event Log、不可变知识 revision 和 reducer/policy 版本能重建相同学习投影；纠错通过补偿、覆盖或 redaction 事件完成，投影重建不会恢复已失效证据。
- A11：两个在线设备提交带幂等键和 expected version 的操作后收敛到同一服务端状态；重复操作不重复计分，版本冲突返回机器可读结果而不静默覆盖。
- A12：用户明确陈述且稳定的非敏感偏好可按规则进入 Nocturne；模型推断、敏感信息和生成摘要先成为 Memory Candidate，审阅状态、来源、有效期和敏感级别可查询。
- A13：固定版本的 Nocturne sidecar 随部署栈启动并通过契约测试；故障时教学状态保持可用，长期记忆写入返回 `applied / queued / rejected`，恢复后按幂等和 generation fence 处理而不复活已删除记忆。
- A14：在线 Go CLI 可完成配对、导入知识、开始或继续学习、回答、查看评估与进度、自由提问并返回原 focus；默认是低颜色、无显眼学习标识的文本模式并支持快速清屏。
- A15：稳定查询 API 返回学习时间线、当前路线、知识节点状态、证据、复习计划和投影元数据，包括 `as_of_event_seq`、知识 revision、projection version 与降级状态，无需解析 Nocturne 或原始聊天。
- A16：隐私清除先建立 tombstone/generation barrier，取消相关待处理写入，并清除活动库、索引、缓存和 Nocturne 活动数据；删除回执列出结果和最多 30 天的备份不可恢复截止时间，截止前不宣称备份已彻底清除。
- A17：后续离线 CLI 只排队带设备序号和 revision 的 client operation/observation；联网后服务端生成 canonical event，并分别返回操作存档结果与证据是否接纳，过期知识 revision 不会静默计分。
- A18：Fast Note Sync 适配器按固定契约发布带 source revision 的 Markdown，旧 Outbox 消息不能覆盖新版本；Obsidian 修改只经显式导入，基于 base revision 生成三方差异并在冲突时等待审阅。
- A19：MCP transport 与 HTTP/CLI 调用同一应用用例和授权策略；通用 Agent 接入后使用同一知识、学习状态和 Nocturne namespace，不创建另一份业务真值。
- A20：Agent 知识维护先生成带来源和风险等级的 proposal；新增和局部低风险修改可按策略应用，移动、重大重写、拆分、合并和删除必须审批，应用后形成新 revision、lineage 和可回退历史。

# Constraints and invariants

PostgreSQL 是 Go 教学服务中 canonical Markdown、知识 revision、学习事件、投影、设备、Inbox 和 Outbox 的权威存储。客户端只提交操作或观察，只有服务端可以生成 canonical Learning Event。LLM 只能提出讲解、题目、评估、路线和知识修改 proposal，确定性编排器、验证器和策略层决定状态转移与写入。每条计分证据必须绑定 Activity、rubric、答案、知识 revision、模型/提示版本和接纳状态。

Go 数据库是目标、路线、掌握证据、误区假设、复习与同步状态的唯一权威源，Nocturne 不保存这些业务真值的副本。所有外部副作用使用 PostgreSQL 事务 Outbox，按至少一次投递设计，并具有幂等键、单调 revision、generation fence、重试和死信状态。外部集成固定兼容版本、声明 capability matrix 并运行契约测试。

初始复习策略是版本化、可解释的固定间隔，不声称精确预测遗忘。以后可以在不破坏历史重放的前提下增加 FSRS 等策略。

# Decisions

产品采用独立教学核心并提供 HTTP/MCP 适配的混合形态，专用客户端与通用 Agent 共用业务状态。系统面向单用户自托管，服务端为 Go 模块化单体，按 `identity`、`knowledge`、`learning`、`tutoring` 和 `integrations` 组织，使用 Ports/Adapters 且不设置万能 Repository。只有 Learning Event Log 使用 append-only 事件模型，知识、设备和配置使用普通事务模型。

PostgreSQL 是知识文档 canonical revision 和教学业务数据的权威源，文件目录和 Obsidian 是导入或发布视图。知识树先由 Markdown AST 确定性构建，LLM 只生成可替换的摘要、关系 proposal 和检索选择。知识移动或改名可以保持身份，重大改写、拆分或合并使用保守 lineage，旧证据冻结且迁移建议必须审批。

第一个可运行里程碑采用在线 Go CLI，并直接集成固定版本的 Nocturne Memory sidecar。教学流程由显式状态机控制，自由问答默认不计分，只有显式转为测验后才产生可接纳证据。模型评估风险分级接纳，低置信或争议开放题保持 provisional。Nocturne 采用分级准入，明确、稳定、非敏感偏好可自动进入，推断、敏感信息和摘要先审阅。

OpenAI 接入使用明确兼容 profile，不把工具调用作为核心教学流程的必需能力。隐私清除立即覆盖活动数据，备份最长保留 30 天并显示最终不可恢复截止时间。Fast Note Sync、MCP、离线和知识维护属于首个里程碑之后的独立波次。Rust CLI 仅作为未来候选。

# Open questions

无。

# Verification expectations

每个业务模块需要领域和应用单元测试，PostgreSQL、Nocturne、模型端点和 Fast Note Sync 需要集成或契约测试。固定模型替身用于验证状态机、结构化输出失败、超时、重试和低置信评估，真实模型偶然输出不能成为唯一验收依据。

事件重放必须比较增量投影与全量重建，知识索引必须比较增量更新与全量重建。Inbox/Outbox、令牌、Nocturne 降级、删除 fence 和 Fast Note Sync 发布需要重复、乱序、并发与故障注入测试。CLI 验证 Linux、macOS 和 Windows 的基本终端行为。
