# Outcome

交付可由后续 Go CLI 和 MCP 共用的可信教学核心：服务以显式状态机推进目标、路线、Activity、Attempt、Assessment、Evidence 与复习，通过 append-only Learning Event Log 和版本化 reducer 形成可重建、可纠正、可查询的 Learner Model。

# Scope

本 child 严格承接 Supervisor A7-A11、A15、A29、A38-A52、A63-A64。它新增 `learning` 与 `tutoring` 业务模块、PostgreSQL migration `000003_learning_core.sql`、教学模型 proposal adapter、受认证 HTTP/OpenAPI 契约、事件与不可变业务记录、read-your-writes 投影、全量重建，以及时间线、路线、节点掌握、Evidence、复习和会话查询。

写操作使用认证设备、调用方 operation ID、目标 aggregate 和必填 expected version。服务端才可生成 canonical Learning Event、Assessment 接纳结果、Evidence、掌握状态、路线 revision 与复习计划。模型只生成带冻结上下文的 route、Activity、Assessment、free-answer 和 explanation proposal，确定性应用服务另行验证和应用。

# Non-goals

本 child 不实现 Nocturne Memory、隐私清除执行、离线队列、Go CLI、MCP、Fast Note Sync、知识自动维护、Web UI 或多用户。它不迁移 knowledge lineage 上的旧 Evidence，不实现 FSRS，不把模型生成文案、原始聊天或 Nocturne 内容作为事件重放输入，也不让服务端读取客户端文件系统。

本 child 不承诺真实模型评分绝对正确或精确学习时长；时间统计只提供基于服务端接收时间的带版本估算。PostgreSQL 是唯一生产方言，不增加 SQLite 实现。

# Acceptance examples

- A1：表驱动状态机覆盖父级全部状态和非法跃迁；从 RouteActive、ActivityIssued 或 AwaitingResponse 暂停后，显式恢复在一个事务中还原同一 route revision、step、focus node、Activity 和 Attempt 上下文。
- A2：Activity 发出后，即使 knowledge head、路线、rubric 或策略随后变化，原 Activity、Attempt 与 Assessment 仍引用冻结版本；未知或跨 knowledge revision 的 proposal 整份拒绝且不写事件。
- A3：客观题及满足 `assessment-acceptance-v1` 的高置信开放题可产生 accepted Evidence；低置信、引用不足、结构不完整或带风险的开放题不计分并保持 provisional，用户可确认、覆盖或作废。
- A4：阅读、讲解和自由问答只形成 exposure；显式“转为测验”创建新的冻结 Activity，只有该 Activity 的后续 Attempt 才可能产生 Evidence，完成后仍需显式恢复原 FocusFrame。
- A5：相同事件、不可变 artifact、knowledge 引用与 reducer/policy 版本从零重放，得到与增量处理相同的 mastery、review、route、timeline 和 focus 投影；EvidenceInvalidated、AssessmentOverridden 与 EventRedacted 不会在重建时复活旧效果。
- A6：相同 `(device_id, operation_id)` 和请求 hash 重放原结果且不新增事件或计分；相同 key 不同 hash 返回 idempotency conflict；两个设备用同一 expected version 并发时恰好一个成功，另一个收到含 current version 的冲突。
- A7：Mastery 仅由当前有效 accepted Evidence 推进；单次证据不能 retained，至少两次相隔 24 小时且来自不同 Activity 的低帮助主动回忆成功才可 retained，ReviewPresented 本身不推进固定间隔。
- A8：查询 API 返回稳定时间线、当前及历史路线、会话 focus、节点状态、Evidence、误区 hypothesis 与复习队列，并统一携带 `as_of_event_seq`、projection/reducer/policy version、knowledge revision、rebuilding、degraded 和 incomplete 信息。
- A9：投影重建写入新 generation，失败时旧 generation 继续可读；在 event clock 高水位锁下追平尾部并原子切换，不能向查询暴露空表、混合 generation 或越过未提交事件的 checkpoint。
- A10：严格 fake model 覆盖成功、低置信、风险标志、缺引用、schema 错误、timeout、未知 ID、跨 revision、重复或越界 quote；模型失败或 proposal 校验失败不改变权威状态。
- A11：HTTP 在进入应用用例前执行 bearer 认证、设备限流、`learning:read`/`learning:write` scope、严格 JSON 和审计；OpenAPI 完整描述真实请求、响应和稳定错误 envelope。
- A12：真实 PostgreSQL 测试覆盖 migration、原子 Inbox/Event/实体/投影写入、event sequence 提交顺序、并发 expected version、故障回滚、projection rebuild 和增量/全量结果比较；无数据库时明确 skip。

# Constraints and invariants

`learning` 拥有目标、路线、Activity、Attempt、Assessment artifact、Evidence、误区与学习投影；`tutoring` 拥有 LearningSession、FocusFrame 和显式状态机。跨模块命令只能由一个应用事务协调，owner store 通过共享 DBTX 写各自表，不能使用两个独立事务或建立万能 Repository。

事件 envelope 与可 redaction payload 分表；每个事件具有全局单调 `event_seq`、aggregate version、operation ordinal、schema version、可信 `received_at` 和不可信可空 `occurred_at`。事务锁定单例 event clock 分配顺序，确保 checkpoint 顺序不超越尚未提交的低序号事件。

Assessment artifact、原始 Attempt 和 accepted Evidence 不可覆盖。确认、覆盖、作废、Evidence 失效与业务 redaction 通过新事件表达。`provisional` 是非计分审阅覆盖层：它可以作为展示状态，但不提升 baseline mastery、不推进 review，并同时返回 baseline state 与未决原因。

模型不得生成 canonical ID、事件、状态、Evidence、掌握值或复习日期。proposal 应用前必须重新验证 input hash、aggregate version、knowledge membership、rubric、quote/range/hash、枚举、数量和长度；任何一项失败都整份拒绝。

# Decisions

M1 使用 `learning-event-v1`、`learning-projection-v1`、`mastery-reducer-v1`、`assessment-acceptance-v1`、`fixed-interval-v1` 与 `estimated-active-time-v1`。未知事件 schema 明确停止重建并标记 projection incomplete，不猜测旧语义。

权威写入和模型生成分成两阶段。`POST /v1/tutoring/proposals` 生成并保存不可变、幂等的模型 proposal artifact，但不修改 aggregate；后续 goal/session/action 或 assessment decision 命令在单一 PostgreSQL 事务中验证并引用 proposal。单个 proposal 请求最多两次模型尝试，类别和可信模型/提示元数据写入 artifact。

开放题自动接纳要求所有 rubric 项结构完整、答案 quote 和知识引用逐字可验证、无风险标志且整数 confidence 至少 850/1000。用户 override 产生不可变替代裁决；若旧裁决已有 Evidence，先追加 EvidenceInvalidated，再按替代裁决生成新 Evidence。void 不生成替代 Evidence。

复习阶梯固定为 `1d / 3d / 7d / 14d / 30d`。成功且到期的 accepted active-recall Evidence 推进阶梯；提前成功不推进，失败或 partial 重置为 1d，ReviewPresented 不推进。Evidence 失效后从剩余有效历史重新计算。

HTTP 使用 `learning:read` 和 `learning:write` 两个 scope；migration 为现有未吊销第一方设备令牌补齐 scope。写面为 goal、session、proposal、session action 与 assessment decision，读面为 current session、timeline、routes、node mastery/evidence、reviews 和 projection status。

# Open questions

无。该 child 没有新增用户可见决定；全部行为严格派生自已确认的 Supervisor Shape。

# Verification expectations

领域单元测试必须覆盖完整状态转换矩阵、FocusFrame 三种保存前态、proposal 验证、评估风险矩阵、掌握与复习 reducer、补偿事件及估算时间。固定 clock、UUID source、knowledge fixture 和 fake model 保证只比较结构、ID、状态、引用、hash 与投影指纹，不比较模型自由文本。

应用与 HTTP 测试必须覆盖幂等、expected version、read-your-writes、scope、严格 JSON、错误映射、日志脱敏和 OpenAPI payload。PostgreSQL 集成测试使用 `TEST_DATABASE_URL` 隔离 schema，执行并发、回滚、event-clock、checkpoint、generation 切换与从零重放；普通测试、vet、漏洞扫描、OpenAPI/YAML 和错误级诊断始终运行。
