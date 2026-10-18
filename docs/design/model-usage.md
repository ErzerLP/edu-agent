# 用量台账与取消预算：设计

状态：Shape 已确认（2026-10-18）。对应 [brief](../comet/specs/model-usage/brief.md)、[spec](../comet/specs/model-usage/spec.md)；依赖和 Ready 判定见 [Supervisor](learning-generation-shape.md)。U1 确认统计全部服务端模型调用，不含 CLI 本地直连；U2 确认工具调用上限默认不限、可配置。

## 1. 现状与方案

已核对 `server/internal/mentorrun/core.go` 的 `budgetTransport.RoundTrip`：它同时承担外发门禁、调用计数和余额扣减，不能直接整段删除。`ObserveUsage` 当前为空；`studio.go` 用总 Token 返还预留值，不能提供完整台账。`research.go` 的 `researchReserve` 连搜索和网页抓取也扣请求次数，不能把旧 `RequestsUsed` 当模型调用数。

`server/internal/integrations/llm/client.go` 与 `responses.go` 已解析输入、输出、推理和总 Token，但整数字段无法完整表达“未报告”和零的区别。`packages/agentcore/modelclient` 用于服务端导师及 CLI，只支持 Chat Completions；本次不更改其协议。

选择“服务端统一外发观察 + 持久调用台账”。仅在各任务结果里累计会漏掉重试和失败，按供应商账单回填又不能保证任务归属，均不采用。复用既有 HTTP client／transport 的装配点，移除累计预算检查，保留身份、目的地指纹、来源链接、privacy ReadPermit 等全部外发门禁。

## 2. 统计口径

| 项目 | 定义 |
| --- | --- |
| 任务／单次生成 | 一次用户动作对应的根任务；多阶段 Studio 用 job ID，对话用 run ID，评估用既有 task ID。后台摘要有自己的任务，关联 conversation，但不并入触发轮次重复累加。 |
| 一次调用 | 一个服务端开始外发的模型 HTTP 尝试。协议回退和网络重试是新尝试；查询、重放、解析同一响应不是新调用。此数字不等同供应商成功受理次数。 |
| 输入／输出 | provider 明确报告的 input/prompt 与 output/completion。对已知包含推理的 output 直接保存；若某适配协议分开报告，则统一后再呈现。 |
| 推理 | 仅保存计数，作为输出的子项；缺字段就是未知，不以零初始化。绝不保存隐藏推理文本。 |
| 合计 | 输入 + 已包含推理的输出；两项任一未知则完整合计未知。保留 provider total 供一致性校验，不能把不明口径的 total 拆成猜测值。 |
| 聚合 | 每个字段分别返回 `known_sum`、`unknown_calls`；调用数同时展示处理中／结果未知数。所有字段未知的调用仍计入次数。 |
| 时间 | 落库 UTC，按外发开始时间归桶。查询传 IANA 时区；默认浏览器时区，周从周一开始，区间为 `[from,to)`。跨时区／夏令时由服务端统一计算。 |
| 模型 | 按实际使用的 provider、服务端目的地标识、model 和 api_style 归类；配置后续改名不重写历史。页面显示可读模型，不暴露密钥或含私人信息的 endpoint。 |

不将 token estimator 结果写入实际用量。合法但没有 usage 的拒绝响应保留未知；提供 usage 的 schema 错误或输出截断照样入账。总量与细项明显矛盾时保留原始数字的结构化审计值及 `usage_inconsistent` 标记，页面不生成伪精确合计；不存整个响应包。

研究里的搜索／抓取不产生模型 Token，也不进入“模型调用次数”；其次数预算同样取消，原有 URL、重定向、响应大小、来源数和授权约束保留。模型探测、多次 structured-output 修复、知识目录选择与摘要等模型请求都必须计数。按 U1，用量页明确不含 CLI 本地直连。

## 3. 数据模型与 migration 草案

建议新增 `server/internal/modelusage/`，提供 Begin／Finish／查询端口，归 learning privacy owner，不新建一套清除控制面。下列名称是拟新增，不代表仓库已有实现。

| 表 | 主要字段与约束 |
| --- | --- |
| `learning_model_tasks` | `id`、`privacy_generation`、可空 `space_id`、`source`（web／cli／server）、可空 `actor_device_id`、`kind`、`origin_id`、`created_at`；同代次、任务类型、origin 唯一；保护任务关联信息。 |
| `learning_model_calls` | `id`、`task_id`、`privacy_generation`、`stage`、`attempt_key`、`attempt_no`、`dispatch_state`、开始／结束时间、provider/model/api_style、配置指纹；输入／输出／推理／总 Token 为 nullable bigint，且非负；每字段有 reported 状态。 |
| 调用约束 | `(task_id, attempt_key)` 唯一；同一外发尝试的 Finish 幂等，不能以应用重试复用 attempt_key；关联 task 与 call 的 generation 必须相同。 |
| 查询索引 | `(privacy_generation, started_at, id)`、`(privacy_generation, space_id, started_at)` 的等价关联索引、task ID 与模型索引；明细 cursor 分页。 |

首版直接聚合台账，不建需要独立回补的永久累计表；以后添加缓存也必须按 generation 分区、可删除且可由同一台账重算。无学习区的模型探测／后台操作用明确的全局归属，不伪造一个学习区。

来源与设备归属从首版 migration 起提供：`source` 必填，标识任务发起来源；调用继承根任务的来源与设备。独立后台任务用 `server`，无发起设备时 `actor_device_id` 为空，不伪造设备。归属由服务端受理上下文确定。后续 [#122](https://github.com/ErzerLP/edu-agent/issues/122) 的 CLI 网关任务使用 `cli` 和已认证设备，直接写入此结构，无需为接入另改数据库结构；本地直连不入账。

关系标识和计数仅用于查询；任务标题、可读目的地等如确需保存，使用现有 AEAD 机制加密并纳入轮换，不存源输入／内容哈希来代替正文。AAD 绑定用途、实体、代次。只允许 owner 写计数，客户端不能提交“已消费 Token”。

拟 `000041_model_usage.sql`：新表、索引、隐私登记及合法状态约束；编号在 Build 重新分配。旧 run JSON 的余额字段暂不物理删除，旧 checkpoint 密文不批量重写。已有用量字段若不能证明来自一次唯一实际响应，不导入真实台账。旧详情显示“启用统计前未记录”，统计页明确起始时间，不能给旧任务虚构一条调用。

## 4. 调用路径与状态

```text
业务受理：确定根任务和用途
  → 原有身份／scope／来源／目的地／generation 门禁
  → prepared（持久登记，尚未外发）
  → dispatching（开始外发，计一次尝试）
  → received / failed / cancelled / outcome_unknown
  → 用量字段独立结算；业务结果仍按其领域校验
```

`prepared` 未进入外发可标为 `not_dispatched`，不计次数；进入 `dispatching` 后崩溃无法知道供应商是否接收，保留一次尝试和未知用量，不声称实际账单为零。数据库和外部模型不能跨系统 exactly-once，设计只保证本地尝试不重复计数。任务恢复重新调用时建立新 attempt，旧未知记录保留；不会为了补齐统计而重发同一模型请求。

开始外发前登记失败则不发送，报告存储不可用；返回后结算暂时失败保留 dispatching，由恢复过程转未知或用可信回执补写。worker 租约丢失后仍可结算**原 call 的计数**，但需 owner/generation 门禁，不能提交业务正文；privacy generation 变化后连旧计数也不能回写。回调重复使用 CAS，只允许未知字段被同一调用的可信报告补全。

## 5. 接入点与 API 草案

| 路径 | 接入方式 |
| --- | --- |
| 导师共享 modelclient | 在服务端 settings／app 装配带 call context 的 transport，响应流旁路读取 usage；不修改 `packages/agentcore` 或 CLI。最终 usage 到达才结算；不要求 `ObserveUsage` 单独承担计数。 |
| Studio、评估、教学、知识选择 | `settings.StudioClient`、`TeachingClient` 和 `integrations/llm` 共用同一计数端口；解析 nullable usage，覆盖 Chat Completions 与 Responses。 |
| 设置探测 | `settings/probe.go` 每个真正的模型请求单独尝试，归全局用途 `model_probe`；搜索探测不进入模型台账。 |
| 未来 #117／#118 | 强制传 task、stage、attempt_key、generation；自动摘要有独立 task，Studio 多 run 共享同一个 job 根任务。 |
| 后续 #122 模型网关 | 默认 CLI 模型请求经服务端，归 `cli` 与已认证设备，复用台账及工具调用上限设置；保留不统计的本地直连。独立 Shape 在 U 之后，不在 U/T/S 实现网关或 CLI 改造。 |

计数只在一个外发层产生，业务结果层仅附加结果关联。流式响应采用受限 SSE／JSON usage 观察器，不缓冲整个输出、不改变背压、不记录正文；正常 `Close`、错误和取消都结算。完成标志和输出截断属于业务结果，不能抹去 usage。所有自动协议回退必须经过此层。

建议新增查询端点：

| API | 行为 |
| --- | --- |
| `GET /v1/learning/usage` | `from,to,timezone,grain=day|week|month,model,space_id`；返回可见范围合计、各字段未知数、coverage 起始时间。无 space 为全局，仅汇总有权读的区与可见的全局用途。 |
| `GET /v1/learning/usage/tasks` | 同筛选条件，cursor 分页；列表展示用途、发生时间、可访问的任务关联与用量。 |
| `GET /v1/learning/usage/tasks/{taskID}/calls` | 单次生成的阶段与逐次调用；旧无台账任务可由任务页显示未记录，不创建虚构 call。 |

沿用 `learning:read`、设备撤权、CSRF／Origin 和 `no-store` 等约束；有 space 时校验 Header 与筛选归属一致。全局用途只公开必要计数；配置探测的配置细节仍需原 settings 权限。不给学习端签发管理权限。UI 菜单新增“用量”，任务／产物只链接到此页，不把后台 ID 或统计字段塞进学习流程。

## 6. 取消预算及兼容

| 项目 | 新行为 | 旧客户端／旧数据 |
| --- | --- | --- |
| `research_requests`、`research_tokens` | Web 移除，执行不读取；`Limits.Validate` 删除它们与 output 的耦合 | 旧设置文件／更新继续接受合法旧整数；GET 暂保留旧值并声明 deprecated、无执行意义。省略字段不抹掉其他设置。 |
| `request_budget`、`token_budget` | 新请求可省略；不扣减、不检查是否足够 | 旧非负整数可解析，仍拒绝类型错误、负数、溢出；任何合法数值都不限制累计消耗。 |
| `requests_left`、`tokens_left` | 不再用于客户端决策 | 旧响应保留数字类型，使用兼容值 0；不使用巨大的“无限”伪余额。新 Web 不显示。 |
| `requests_used`、`cost_unknown` | 新页面读台账 | 旧字段只作旧 DTO 兼容，不能作为新版精确统计；不把历史搜索次数混进模型调用数。 |
| 新任务状态 | 不再进入 `paused_budget` | 正常完成、失败、停止和等待输入／审批状态继续有效。 |
| 旧 `paused_budget` | 新 Web 呈现“已暂停，可继续” | 底层允许保留 legacy 状态以兼容旧 CLI；用户发 `continue_budget` 时作为 `resume` 别名，不读增额，不自动恢复。 |
| `retry_start` | 保留既有恢复语义 | 接受但忽略附带预算；仍核验原操作、资料、身份及目的地。 |

新 `resume` 命令仅用于可恢复的暂停任务；旧终态对话停止后沿原语义发起新轮次，不能因取消预算变成任意重放。Studio 的 checkpoint 继续语义由 S 提供。升级前正在执行的任务在现有租约边界切换，不让新旧 worker 同时消费一个任务；发布采用停止接单、排空／取消旧 worker、升级后恢复受理的顺序。

**幂等兼容**：为新操作增加规范化哈希版本；历史操作先按旧版本完整载荷核对回执，不先删预算字段重算导致冲突。新操作哈希仍绑定用户提交载荷，包括收到的兼容字段；虽然预算无执行意义，同 operation 改载荷仍冲突。缺省与显式字段的规范化规则随 API 固定，不能随配置变化。

`output_tokens` 继续写入协议对应的单次输出字段（Chat Completions 的 max_tokens／对应适配字段、Responses 的 max_output_tokens），推理消耗在其内。`context_tokens` 包含输入、输出预留和安全余量。不得通过设置假大上限绕过它们。取消任务预算后仍保留有意义的有限计划、一次修复策略和失败后用户重试，不能用累计请求阈值伪装预算。

### 每轮工具调用上限（U2）

设置新增“每轮工具调用上限”，拟字段 `max_tool_steps`：`null` 表示不限，默认 `null`；设置值只能是正整数，拒绝零、负数、小数、错误类型和整数溢出，不用零或假大数表达不限。此项适用于服务端导师对话、研究等 agent 工具循环，与模型 HTTP 调用次数、Token 台账及旧预算余额分开。

默认不限时不因步数停止，也不增加默认生效的重复调用检测；重复检测可作为以后的可选项。设置上限后，达到时参考 opencode 的收尾方式：收回工具，最后一步仅让模型用文字说明当前进展和还缺什么，再进入可继续的暂停状态。界面显示“继续”，用户点击后重新计数，从已有进展接着执行；不直接报上限错误，不要求增额或追加 Token。收尾模型调用照常进入台账。

Studio 分段生成的工作量由确认后的大纲决定，不读取此上限，不因它暂停。该设置不能用于恢复累计请求或 Token 预算。#122 落地后 CLI 与网页共用此设置，CLI 接入留给该独立 Shape。

兼容时旧设置缺少此字段按不限读取，更新其他设置而省略此字段时保留已有值；旧任务及 checkpoint 没有工具步数时不从 `requests_used` 或旧预算余额推导。恢复旧预算暂停仍遵从上文用户触发规则。新增暂停与旧 `paused_budget` 区分，旧 CLI 的服务端请求继续兼容，不以本 child 要求 CLI 更新。设置、服务端循环及暂停／继续展示归 U-C。

## 7. 隐私、并发与旧数据

`server/internal/learning/postgresstore/privacy.go` 的 `redactLearningTypedPayloads`／`verifyLearningTypedPayloads` 纳入台账、关联与可能的聚合缓存；按 owner 现有 gate 接入，不能只删 task 留 call。`server/internal/app/mentor_rotation.go` 的轮换组合需覆盖新增加密字段。

用量不是财务档案，本轮没有跨隐私清除永久保留的要求：清除后删除关联明细与其合计，全局只呈现当前代次。归档学习区保留既有可读统计；失去读取权限时从可见聚合中剔除。普通删除对话时删除可读标题／跳转关系，最小数字可按学习区保留至隐私清除，不保存被删除正文或重建其索引。

查询和缓存以 principal、origin、代次、学习区／过滤器隔离；响应 ReadPermit 使 clear 后迟到响应不能重新显示。自动探测同样需取得当前代次并经过统计 gate，不因没有 space 绕过清除。并发 worker 使用唯一 attempt 和终态 CAS；任务累计通过查询计算，不做易丢更新的 read-modify-write。

## 8. 垂直批次与退出标准

### U-A：一次对话的真实用量可查

- **结果**：用户完成一次服务端对话，在独立用量页查看该任务的调用明细、区累计和全局累计。
- **范围**：新台账／查询／隐私／轮换、导师出口及 Web 页面；首版 migration 即含来源和设备归属，连同 app 装配一起交付。
- **延期**：其他模型出口、取消预算、旧暂停恢复；UI 明示此批统计覆盖范围，不能称全部已接入。
- **退出标准**：真实 HTTP 模型桩覆盖成功、缺 usage、推理计数、超时、双回调与重启；查询聚合不重复；清除后迟到结果不能回写。相关 Go package、数据库迁移和页面检查通过，更新覆盖说明。
- **重型证据**：多 provider 真实模型和跨浏览器放到候选；本批已有一条真实生产入口而非只有表或 mock。

#### U-A 实现覆盖（2026-10-18）

- `000041_model_usage.sql` 已落地任务／调用台账、索引、状态与代次关联约束，以及 learning typed payload 隐私登记。`source`、可空设备归属从首版提供。部署启用时间单独保存在 `model_usage_coverage`，不回填历史余额。
- `modelusage.BeginTx` 在既有导师门禁事务内登记；`Dispatch` 在实际 HTTP 尝试前计一次；响应观察器在读取结束或关闭时 `Finish`。重复结算只补未知字段，租约恢复将未外发记录标为 `not_dispatched`、已外发记录标为 `outcome_unknown`，不重发模型请求。
- 生产入口包含共享 modelclient 的导师交流，以及当前 Web 学习方向对话的 `generateCited` 分支。后者原已使用服务端 llm 适配器，因此本批也读取其 Chat Completions／Responses nullable usage；没有接入其他用途，也没有修改 agentcore 协议。
- SSE 观察最多保留一个受限事件，非流式 JSON 只提取受限的 usage 对象；不保存正文。输入、输出、推理及 provider total 分别可空；推理作为输出子项，矛盾报告保留数字并使完整合计未知。
- 查询接口要求明确的 `from,to`，复用 IANA 时区、周一周界与 `[from,to)`；汇总在同一数据库快照内计算，任务／调用用绑定身份、代次和筛选的游标分页。当前 `learning:read` 可以读取所有学习区（含归档区）；原导师任务跳转还受原设备及正文可读状态约束。
- 台账只存计数、模型标识、目的地摘要和必要 UUID 归属，不存标题、原始端点或新的密文，因此轮换没有新增重封装字段；新表已纳入轮换停机锁。隐私清除删除任务并级联删除调用，验证器检查两表，旧代次不能结算。
- Web `/app/usage` 提供合计、每项未知数、日／周／月、模型与学习区筛选、任务和调用明细，并标明仅覆盖导师对话、统计起点及隐私清除语义。浏览器使用既有身份／代次隔离和 no-store 请求。
- U-B 的其他出口，以及 U-C 的预算取消、工具上限和旧暂停兼容尚未实施；本批保留现有预算执行行为。真实多 provider、CLI 网关和多浏览器证据仍不属于本批。

### U-B：全部服务端模型出口可追溯

- **结果**：Studio、研究、评估、知识处理、教学、模型探测均可按任务和模型查用量。
- **范围**：settings／llm／selector／tutormodel 的服务端装配及调用用途；查询页显示全覆盖起点。
- **延期**：预算字段和旧暂停语义移除留给 U-C；CLI 直连遵从 U1。
- **退出标准**：逐个生产模型出口映射到台账；流式、非流式、Chat Completions／Responses、兼容回退各有相关验证；搜索请求不误计为模型调用。检查缺 context 的出口并修复，不要求改共享包协议。
- **重型证据**：最终候选抽查真实 DeepSeek 推理 usage、截断响应及不同 API style；不以桩数据证明 provider 一定返回推理字段。

### U-C：无累计预算、可选工具上限与继续

- **结果**：所有入口无预算输入，旧小预算不阻断，旧暂停可由用户继续；工具调用默认不限，自设上限后说明进展并可继续。
- **范围**：mentorrun 的 worker／core／command／query／research／assessment_ports／study_exercise_ports、settings 字段与校验、HTTP 兼容和相关 Web 表单；包含工具步数、收回工具后的收尾、暂停及重新计数继续。
- **延期**：长对话与分段生成留给 T/S，明确当前旧上下文限制；CLI 共用设置和模型网关留 #122，不新增默认重复检测。
- **退出标准**：极小旧预算、字段省略、旧哈希回执、旧暂停升级、停止后不再派发均通过最小验证；工具上限缺省／不限时不因步数暂停，正整数校验、到达上限收尾说明、点击继续重新计数及 Studio 不受影响均有相关验证；保留输出／窗口／权限门禁；旧 CLI 请求兼容检查通过，无 CLI 源码改动。文档和 OpenAPI 在 Build 同步。
- **重型证据**：child Candidate 才核对完整 U spec、代表性旧 CLI 和真实 provider；Supervisor 完成前不能宣称 T/S 场景通过。
