# Learning Core 完整规格

## 模块边界与权威数据

`server/internal/learning` 拥有 GoalRevision、RouteRevision、RouteStep、Activity、Attempt、AssessmentArtifact、AcceptedEvidence、MisconceptionHypothesis、LearningEvent、Inbox、MasteryProjection、ReviewSchedule、查询 projection 与 replay 语义。`server/internal/tutoring` 拥有 LearningSession、FocusFrame、教学状态和纯状态转换规则。HTTP、PostgreSQL、knowledge reader 与模型 adapter 只实现消费方定义的窄端口。

PostgreSQL 是 learning 与 tutoring 数据的唯一权威存储。模型 proposal、HTTP payload 和未来客户端缓存都不是业务真值。Nocturne 不属于本 child，也不得成为掌握度、路线、Attempt、误区或复习的重放输入。

跨 learning/tutoring 的命令由一个应用事务拥有。各模块保留自己的表和领域规则，并通过共享 DBTX 的 owner store 参与同一事务；禁止跨模块直接查询对方表、两个事务拼接或创建覆盖所有实体的通用 Repository。learning 定义 `KnowledgeReferenceResolver`，由 knowledge 应用 adapter 通过公开的 frozen revision/tree 能力验证 knowledge revision、node 和 node revision 所属关系，learning 不直接读取 knowledge 表。

## 不可变实体与版本

GoalRevision 包含 goal ID、revision、用户目标正文、来源、actor device、创建时间和前一 revision。修改目标只创建新 revision。RouteRevision 冻结 goal revision、knowledge revision、route policy version、有序 RouteStep 和来源 proposal；每个 step 引用不可变 node revision、教学意图与完成条件。路线调整创建新 RouteRevision，旧 Activity 和 Evidence 不重新绑定。

Activity 在发给用户前创建并冻结 Activity ID/revision、goal revision、route revision/step、knowledge revision、一个或多个 node/node revision、题目正文、题型、rubric revision 与完整 rubric、难度、允许帮助、activity policy、assessment policy 和 review policy。创建后不能覆盖。`objective` rubric 包含确定性答案规则；`open` rubric 包含有序 item、判定标准与所需知识引用。

Attempt 不可变，包含 Activity revision、答案 payload 引用、实际帮助级别、认证 device、可空不可信 `occurred_at`、可信 `received_at` 和 payload hash。帮助级别固定为 `none / hint / scaffold / answer_revealed`；`answer_revealed` 只形成 exposure，不能形成 accepted Evidence。

AssessmentArtifact 不可变，包含 Assessment ID、Attempt/Activity、逐 rubric 结论、答案 quote 的 UTF-8 byte range 与 hash、knowledge revision/node revision/source range/slice hash、整数 confidence、风险标志、可信 model ID 与参数、prompt revision、proposal input hash、尝试次数与错误类别。模型生成 artifact 后仍不具有写 Learner Model 的权限。

Assessment disposition 是 append-only 生命周期：`provisional / accepted / overridden / voided`。确认、override 或 void 创建新 decision record 和事件，不修改原 artifact。override 必须给出非空理由和完整替代 rubric 结论；旧 disposition 已产生 Evidence 时先追加 EvidenceInvalidated，随后只有可计分的替代结论才能创建 replacement Evidence。

AcceptedEvidence 不可变，引用 disposition、Assessment、Attempt、Activity、goal/route、knowledge/node revision、rubric、Evidence kind、题型、outcome、帮助级别、可信时间及 acceptance/reducer/review policy。M1 Evidence kind 固定为 `practice_recall / review_recall`，二者都是 active recall；阅读、讲解、FreeAnswer 和 answer-revealed 只形成 exposure，不创建 Evidence。EvidenceInvalidated 只通过新事件使其失效；原 record 保留供审计和重放。

MisconceptionHypothesis 是有来源 Evidence、反证能力与状态的版本化 hypothesis，不是用户事实。Assessment proposal 可携带非权威 misconception candidate，但 `record_assessment` 只把 candidate 保存进 artifact；只有对应 disposition 产生有效 Evidence 时，确定性 reducer 才按 node revision、rubric item 与规范化 candidate hash 生成 server-owned identity 和 hypothesis revision。首份相关 fail/partial Evidence 产生 `proposed`，再次来自不同 Activity 的当前有效 fail/partial Evidence 产生 `supported`，后续相反 pass Evidence 产生 `challenged`，满足 retained 条件的反证产生 `resolved`；Evidence 失效时从剩余历史重算。每次变化引用导致它的 Evidence 并追加 MisconceptionHypothesisRevised，它不直接提升或降低 mastery。

## Operation、Inbox 与并发

所有权威写请求使用 operation envelope：UUID `operation_id`、payload schema version、aggregate type/ID、必填 `expected_version`、可空 `occurred_at` 和 discriminator payload。认证 device ID 只来自 bearer credential，不信任 body 中的设备声明。创建 aggregate 的 expected version 为 0。

服务对规范化完整请求计算 SHA-256。`learning_inbox` 以 `(device_id, operation_id)` 永久唯一：同 key 与相同 hash 返回首次终态结果并标记 `replayed=true`；同 key 与不同 hash 返回 `idempotency_conflict`。成功和可判定业务拒绝保存终态结果；内部错误、context cancellation 或数据库故障整体回滚且不消费 operation ID。

authority aggregate 由端点固定，不由客户端自由选择：goal 创建/修订使用 goal aggregate；session 创建和所有 session action 使用 path/session aggregate；assessment decision 使用该 Assessment 所属 session aggregate；proposal request 不增加 aggregate version，只冻结引用的 current version。请求中出现 aggregate type/ID 时必须与端点和数据库 ownership 完全一致，否则返回 invalid request。跨 goal/session 的 switch-goal 按 `(aggregate_type, aggregate_id)` 稳定顺序锁全部 head。

每个命令先取得 operation advisory lock，再按上述稳定顺序锁 aggregate head 并比较 expected version。不同 operation 对同一 aggregate 使用同一 expected version 时恰好一个提交，其他返回 `version_conflict`，响应包含 aggregate type/ID、expected/current version 和当前 as-of event sequence；服务不得自动 rebase 或静默覆盖。

一个成功事务原子写入 Inbox、不可变业务 record、canonical events、aggregate head、tutoring focus、需要 read-your-writes 的 projection/checkpoint 和必要的事务 Outbox。Outbox adapter 必须提供调用方事务内写入能力；不能在教学事务提交后再调用当前 pool-only `Enqueue` 模拟原子性。

同一 operation 的 event ordinal 使用固定因果顺序：先写目标业务 record 事件；再写对旧结果的 EvidenceInvalidated 等补偿；再写 replacement EvidenceAccepted；再写 MisconceptionHypothesisRevised；最后写 TutoringStateChanged、RouteAdvanced 或 LearningCompleted。create-session 依次写 LearningSessionStarted 与 GoalReady state；apply-route 写 RouteRevisionCreated 后写 RouteActive；issue/present/submit 分别写 ActivityIssued、ActivityPresented、AttemptSubmitted 后写目标 state；record-assessment 写 AssessmentRecorded、disposition、Evidence/误区，再写 Feedback；acknowledge-feedback 先写 AdvanceOrReview，再写 RouteAdvanced 和 RouteActive 或 LearningCompleted。Focus 操作的特殊顺序在状态机章节冻结。projector 在完整 event batch 之后更新 checkpoint。

## Canonical Learning Event Log

`learning-event-v1` envelope 包含全局 `event_seq`、UUID event ID、event type、event schema version、aggregate type/ID/version、认证 device、operation ID、operation ordinal、可信 `received_at`、不可信可空 `occurred_at`、payload ID 和 payload hash。payload 独立保存在可 redaction 表；event header 与最小审计引用不可变。

全局顺序由事务锁定的单例 `learning_event_clock` 分配，而不是普通 PostgreSQL sequence 的预分配值。持有 clock row lock 的事务写完全部事件、projection checkpoint 和 head 后才提交，因此较大 event sequence 不会先于较小 sequence 对查询可见，回滚也不会留下可被 checkpoint 跨过的洞。

同一 aggregate 的 `(aggregate_type, aggregate_id, aggregate_version)` 唯一，同一 operation 的 ordinal 唯一。多事件 operation 按确定性领域顺序分配 ordinal 和连续 aggregate version；reducer 只按 `event_seq ASC` 处理，UUID、数据库自然顺序、模型返回顺序和客户端 `occurred_at` 都不能决定重放顺序。

M1 事件至少覆盖 GoalRevisionCreated、RouteRevisionCreated、LearningSessionStarted、TutoringStateChanged、ActivityIssued、ActivityPresented、AttemptSubmitted、AssessmentRecorded、AssessmentMarkedProvisional、AssessmentAccepted、AssessmentOverridden、AssessmentVoided、EvidenceAccepted、EvidenceInvalidated、ExposureRecorded、ReviewPresented、FocusSuspended、FreeQuestionAsked、FreeAnswerRecorded、FocusResumed、RouteAdvanced、LearningCompleted、MisconceptionHypothesisRevised 和 EventRedacted。

event schema version 必须有显式 decoder/upcaster registry。M1 原生版本为 1；未知版本使目标 projection rebuild 以 `unsupported_event_schema` 失败，旧 active generation 保持可读并标记 incomplete，不能忽略或猜测事件。EventRedacted 在本 child 只表达业务 no-op/补偿语义和待清理 payload 引用，底层隐私清除由后续 child 实现。

## 教学状态机与 FocusFrame

状态集合包含主链 `Idle / GoalReady / Diagnostic / RouteActive / ActivityIssued / AwaitingResponse / Evaluating / Feedback / AdvanceOrReview / Completed`，以及自由问答子链 `FocusSuspended / FreeQuestion / FreeAnswer / FocusResumed`。主链固定为 `Idle -> GoalReady -> Diagnostic -> RouteActive -> ActivityIssued -> AwaitingResponse -> Evaluating -> Feedback -> AdvanceOrReview -> RouteActive|Completed`；自由问答固定为 `RouteActive|ActivityIssued|AwaitingResponse -> FocusSuspended -> FreeQuestion -> FreeAnswer -> FocusResumed -> saved_state`。FocusSuspended 与 FocusResumed 可以是同一 command batch 内仍保留事件的瞬时状态。

合法 action 与提交后状态固定如下。create-session 从 Idle 写到 GoalReady；start-diagnostic 为 GoalReady 到 Diagnostic；apply-route 引用已验证 Route proposal，从 Diagnostic 或 RouteActive 到 RouteActive；issue-activity 从 RouteActive 到 ActivityIssued；present-activity 从 ActivityIssued 到 AwaitingResponse；submit-attempt 从 AwaitingResponse 到 Evaluating；record-assessment 从 Evaluating 到 Feedback；acknowledge-feedback 经 AdvanceOrReview 到 RouteActive 或 Completed。present-review 只在 RouteActive 合法，记录 ReviewPresented 并发行 frozen review Activity，提交后为 ActivityIssued。

ask-free-question 从 RouteActive、ActivityIssued 或 AwaitingResponse 依次写 FocusSuspended、FreeQuestionAsked 和 FreeQuestion state；从 FreeAnswer 追问时复用 active frame 并回到 FreeQuestion，不嵌套 frame。record-free-answer 从 FreeQuestion 写 FreeAnswerRecorded、ExposureRecorded 和 FreeAnswer state。resume-focus 从 FreeQuestion 或 FreeAnswer 依次写 FocusResumed 并精确恢复 saved state。convert-free-answer-to-quiz 从 FreeAnswer 创建 attached Activity 并进入 ActivityIssued；该 Activity 完成 feedback 后经 AdvanceOrReview 返回 FreeAnswer，frame 保持 active，直到显式 resume。

end-activity 在 ActivityIssued、AwaitingResponse、Evaluating 或 Feedback 合法，终止当前 Activity、原子失效任何保存该 Activity 的 frame，并回到 RouteActive；attached quiz 执行 end-activity 时同样失效 active frame，不返回 FreeAnswer。switch-goal 在除 Completed 外的状态合法，终止当前 Activity、失效 frame、创建或选定新 GoalRevision 并落到 GoalReady。complete-session 只在 RouteActive 或 AdvanceOrReview 合法，失效 active frame 并落到 Completed。重复终态、其他前态或 path/session ownership 不匹配返回 invalid transition/conflict，不调用模型、不写 event。

从 RouteActive、ActivityIssued 或 AwaitingResponse 进入自由问答时，事务原子创建不可变 FocusFrame，保存 `saved_state`、goal/route revision、route step、focus knowledge/node revision、可空 Activity/Attempt、saved aggregate version 和创建 event sequence。M1 同一 session 只允许一个 active frame；显式 resume 逐字段恢复，end-activity、switch-goal 或 complete-session 依上述规则使旧 frame 失效，之后返回 `focus_frame_invalidated`，不能恢复混合 revision。

“转为测验”必须引用当前 free question/answer 和新的 Activity proposal，创建具有冻结 rubric 的新 Activity。该 Activity 的后续 Attempt 才可能形成 Evidence；自由问题、模型回答和转换动作本身均不计分。

阅读和讲解使用 `explanation` proposal 或直接 frozen knowledge reference，并由 `record-exposure` action 在 RouteActive 中追加 ExposureRecorded，状态保持 RouteActive。exposure artifact 冻结正文、来源和 knowledge/node revision，但没有 rubric、Attempt、Assessment 或 Evidence；因此正常教学中的阅读、讲解和 FreeAnswer 都有可执行且不可计分的记录路径。

## 模型 Proposal 与确定性验证

`POST /v1/tutoring/proposals` 支持 `route / activity / assessment / free_answer / explanation`。请求以 `(device_id, request_id)` 和完整 input hash 幂等；同 key 不同 hash 返回 idempotency conflict。实现使用 durable request record 与跨模型调用的单 owner/lease，防止并发重复请求提交不同 artifact。

proposal envelope 包含 schema version、input hash、proposal type、冻结 aggregate version、goal/route/activity/attempt/knowledge revision 引用和 server-generated proposal ID。Route proposal 只能给出有序 node revision 与教学意图；Activity proposal 给出题目、题型、rubric item、难度与帮助规则；Assessment proposal 给出 rubric conclusion（允许显式 `unassessed`）、rubric completeness、答案 quote/range、knowledge quote/range/hash、confidence、risk flags 和可选 misconception candidate；FreeAnswer/Explanation proposal 给出正文与 frozen knowledge 引用。模型不得给出 canonical entity ID、aggregate/event version、状态、acceptance、Evidence、mastery、misconception ID 或 review date。

adapter 复用现有 OpenAI-compatible structured Chat，但必须严格二次解码数组 item、enum、数值范围、字符串长度与格式。可信 model ID、参数和 prompt revision 来自服务配置，不接受模型自报。每个 request 最多两次模型尝试；只对 timeout、rate-limited、transport-unavailable 和 HTTP 5xx 重试一次，除 429 外的 HTTP 4xx、malformed JSON、schema mismatch 和领域校验失败不重试；artifact 保存每次类别。

应用 proposal 前重算 input hash，并验证 aggregate expected version、所有 immutable reference、knowledge/node membership、route step、quote 与 frozen answer/knowledge UTF-8 bytes 逐字一致、range/hash、重复项、枚举、数量和长度。无法解析、未知或跨 revision ID、重复 rubric item、非法/越界 range 或伪造 hash 的输出整份返回 `proposal_rejected` 或 `stale_proposal`，不产生 artifact、不允许部分生效。可成功解码但未覆盖全部 frozen rubric、含 `unassessed` item、缺充分答案/知识支持或有风险的 Assessment 仍保存完整 artifact，标记 `rubric_complete=false` 或对应 risk flag，并进入 provisional。

模型 timeout、rate limit、transport error、畸形 JSON、schema mismatch 或无合法 proposal 不改变权威 aggregate。调用方可以用新的 request ID 重试；同 request ID 的 durable attempt 语义按原结果返回。

## Assessment 接纳策略

`assessment-acceptance-v1` 对 objective Activity 使用冻结 answer rule 确定性评分，可直接形成 accepted disposition。`answer_revealed` Attempt 始终不接纳。

开放题自动接纳必须同时满足：artifact 可成功解码且所有 frozen rubric item 均存在；没有 `unassessed`；每项 conclusion 为 `pass / partial / fail`；答案 evidence 为 frozen Attempt 的精确非空 byte slice；所需 knowledge citation 属于 Activity 的 frozen knowledge revision/node 并匹配 source range 与 slice hash；整数 confidence `>= 850`；risk flags 为空；没有互相冲突的 item。无法解析、unknown/cross-revision reference、重复 item、非法 range/hash 按 proposal hard rejection 处理并保持 Evaluating；可解析但 rubric 不完整、含 unassessed、低 confidence、支持不足或有风险时保存 artifact 并进入 provisional，不生成 Evidence、不推进 review。

risk flag 集固定为 `incomplete_rubric / insufficient_answer_evidence / insufficient_knowledge_support / conflicting_evidence / ambiguous_rubric / unsafe_content / schema_repaired / stale_context / retry_exhausted`。未知 flag 视为 schema error。模型 confidence 只是 policy 输入，不是概率承诺。

Assessment disposition 转换固定为：`provisional -> accepted|overridden|voided`；当前 `accepted` 或已经产生 replacement Evidence 的 `overridden` 仍可转为 `overridden|voided`，以支持历史纠错。confirm 只接受当前 provisional 的原 rubric 结论；override 要求非空理由、完整替代结论和当前 disposition version；void 关闭当前裁决。任何被替代 decision 已产生 Evidence 时，先追加 EvidenceInvalidated，再由 confirm 或可计分 override 创建 replacement Evidence；void 不创建替代 Evidence。陈旧 version、重复终态或 ownership 不匹配返回 `assessment_disposition_conflict`。

## Mastery、provisional 与复习

`mastery-reducer-v1` 的 baseline 只读取当前有效 accepted Evidence。没有有效 Evidence 为 unseen；有 Evidence 但 retained 条件不成立为 learning；至少两次成功 active recall、来自不同 Activity、可信 received time 相隔至少 24 小时、帮助不高于 hint，并且其后没有有效 fail/partial，才为 retained。单次 Assessment、重复 operation、exposure、模型回答或 ReviewPresented 不能 retained。

查询 `state` 使用 `unseen / learning / provisional / retained`。存在未决 provisional Assessment 时 `state=provisional`，同时返回由 accepted Evidence 计算且未被提升的 `baseline_state`；解决未决 Assessment 后 state 回到重新计算的 baseline。provisional 不创建 Evidence、不改变复习 step/date。

projection 同时返回有效 Evidence 数量、类型、outcome、帮助分布、最近可信时间、未决 Assessment 数量和 uncertainty reason。EvidenceInvalidated、AssessmentOverridden、AssessmentVoided 或 EventRedacted 后，reducer 从剩余有效历史重算，不能通过减计数猜测以前状态。

`fixed-interval-v1` 阶梯为 1d、3d、7d、14d、30d。首次 accepted active-recall pass 建立 step 0 与 received_at+1d；只有在 due_at 或之后收到的 pass 且帮助不高于 hint 才推进下一 step；提前 pass 保留 Evidence 但不推进；fail/partial 或高帮助结果重置 step 0 与 +1d。ReviewPresented 只记录展示事实。所有日期基于可信 received_at；policy/version 与 interval snapshot 可解释历史。

## Projection、重放与查询一致性

`learning-projection-v1` 包含 session/focus、current/historical route、timeline、node mastery、Evidence、misconception、review 和统计视图。增量处理与从零 replay 使用同一纯 reducer；派生表不能由普通应用 API 直接改值。

projection 使用 generation。active generation 查询期间，rebuild 在新 generation 从 event_seq 1 开始；rebuild 冻结 target high-water，完成后取得 event clock lock、追平尾部、比较 checkpoint 与投影 deterministic fingerprint，再原子切换 head。失败或 cancellation 删除/标记新 generation，不影响旧 generation。

每个查询在一致性读中冻结 active generation 和 checkpoint，并返回 `as_of_event_seq`、projection version、mastery reducer、assessment/review policy、适用 knowledge revision、`rebuilding`、`degraded`、`incomplete` 和稳定 reason codes。checkpoint 小于当前已提交 event high-water 时 incomplete=true。未知 event schema、缺失已 redacted payload 或 projector error 使 rebuild 失败并显式报告。

timeline 按 event_seq 游标稳定分页。route 历史按 revision 与 event sequence；node query 以 immutable node revision 为 key，不沿 knowledge lineage 自动复制 Evidence。review 按 due_at、node revision、stable ID 排序。查询不解析 Nocturne 或原始模型 prompt。

`estimated-active-time-v1` 只使用同一 session 中按 received_at 排序的用户交互事件，相邻正间隔每段最多计 5 分钟，系统/模型事件不计；结果返回 estimated=true、algorithm version 和样本范围。客户端 occurred_at 只展示并明确标记 untrusted，不能决定精确时长、事件顺序或复习日期。

## HTTP 与 OpenAPI

写端点为 `POST /v1/learning/goals`、`POST /v1/tutoring/sessions`、`POST /v1/tutoring/proposals`、`POST /v1/tutoring/sessions/{sessionID}/actions` 和 `POST /v1/learning/assessments/{assessmentID}/decisions`。session action 使用严格 discriminator，只接受 `start_diagnostic / apply_route / issue_activity / present_activity / submit_attempt / record_assessment / acknowledge_feedback / present_review / record_exposure / ask_free_question / record_free_answer / convert_free_answer_to_quiz / resume_focus / end_activity / switch_goal / complete_session`；每种 action 的前后态和 event batch 服从状态机章节。

读端点为 `GET /v1/tutoring/sessions/current`、`GET /v1/tutoring/sessions/{sessionID}`、`GET /v1/learning/timeline`、`GET /v1/learning/routes`、`GET /v1/learning/nodes/{nodeRevisionID}`、`GET /v1/learning/evidence`、`GET /v1/learning/reviews` 和 `GET /v1/learning/projections/status`。列表使用有界 cursor/limit，默认 50、最大 200；cursor 绑定查询种类和 projection generation，stale cursor 返回明确 conflict。

写请求需要 `learning:write`，读请求需要 `learning:read`。migration 为现有未吊销第一方 token 增加二者，新的配对 token 默认包含二者。认证、设备限流、scope 和审计必须在 learning/tutoring use case 前执行。日志不得包含 goal 正文、问题、答案、rubric、model prompt/response、Evidence quote 或 free question/answer。

JSON 拒绝未知字段。普通 body 最大 1 MiB，答案/自由问题/goal/rubric 另有 OpenAPI 字符和字节上限；超限返回 413。OpenAPI 3.1 使用 `additionalProperties:false`、discriminator、真实 response schema、`x-required-scope` 和所有实际 400/401/403/404/409/413/429/500/503 响应。

写响应返回 operation status、replayed、aggregate version、first/last event sequence、projection as-of、当前 tutoring state 和 typed result，并分别报告 operation archived 与 Evidence disposition。模型不可用返回 503 `model_unavailable` 且不改变 aggregate；read API 在模型不可用时仍工作并可标 degraded。

## 错误语义

稳定领域码至少包括 `invalid_request`、`not_found`、`idempotency_conflict`、`version_conflict`、`invalid_transition`、`activity_state_conflict`、`knowledge_reference_invalid`、`stale_proposal`、`proposal_rejected`、`assessment_disposition_conflict`、`focus_frame_invalidated`、`unsupported_event_schema`、`projection_unavailable` 和 `stale_cursor`。认证、授权、限流、payload 与 internal error 延续现有 envelope。

409 conflict 只返回解决冲突所需的 stable ID、expected/current version、as-of sequence 或 current disposition，不返回 request hash、答案、模型内容或其他敏感 payload。500 只记录 request ID 与脱敏类别。

## PostgreSQL schema 与装配

`000003_learning_core.sql` 新增 event clock、aggregate head、Inbox、event envelope/payload、goal/route/step、session/focus frame、proposal request/artifact、Activity、Attempt/payload、Assessment/item/decision、Evidence/invalidation、misconception revision、projection generation/head/checkpoint 及各 query projection 表。immutable ID、aggregate version、operation ordinal、request hash ownership、knowledge references、active FocusFrame 与 active projection generation 具有数据库约束；普通 store API 不提供历史 UPDATE/DELETE。

`app.Run` 在 migration 后组合 learning/tutoring PostgreSQL stores、knowledge reference adapter、可选 TutorModel adapter、deterministic policies、application coordinator 和 HTTP API。模型未配置时权威写入、objective grading、查询和 replay 保持可用；需要模型 proposal 的端点返回明确 unavailable，不改变既有 readiness required/optional 语义。

## 验证

领域 fixture 覆盖全部状态边、每个非法前态、FocusFrame 三个来源状态、重复追问、精确恢复、frame invalidation、Activity 冻结、自由问答转测验、objective/open acceptance 矩阵、override/void、Evidence invalidation、provisional overlay、retained 24 小时边界、五级 interval 与 estimated time。

fake model 覆盖成功、timeout、rate limit、malformed JSON、schema mismatch、低 confidence、每个 risk flag、缺答案 quote、缺知识支持、未知/跨 revision ID、重复/越界 range、stale input hash 和第二次尝试。断言模型错误不写 aggregate，验收比较 proposal schema、ID、hash、event、state、Evidence 和 projection fingerprint，不比较自由文本。

PostgreSQL 集成测试在 `TEST_DATABASE_URL` 隔离 schema 中运行 migration、same-operation replay/conflict、两个设备 expected-version race、event-clock 提交顺序、多事件 ordinal、每个写点故障回滚、read-your-writes、checkpoint 不跨未提交事件、rebuild tail catch-up、失败 generation 保留旧查询，以及增量/全量 fingerprint 一致。没有数据库时明确 skip，不能用 SQLite 或 mock 宣称这些验收通过。

HTTP/OpenAPI 测试覆盖 scope、strict JSON、body limit、完整响应 schema、日志脱敏、cursor、冲突和模型降级。普通 Go test、vet、govulncheck、OpenAPI/YAML 解析、migration checksum、gofmt、git diff check 与错误级 diagnostics 必须通过。
