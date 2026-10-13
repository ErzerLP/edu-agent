# Notebook 工作区：信息架构与分批实施设计

状态：Shape 已确认（2026-10-13）：用户认可对话式原型 v2，Q1–Q5 采用建议默认值。批次 1、2、3（含 3c 单题会话方案 A）已合并 main；来源与 Studio 继续复用，中栏改为对话式学习。正式行为见 [brief](../comet/specs/notebook-workspace/brief.md) 与 [spec](../comet/specs/notebook-workspace/spec.md)。本轮有效设计与计划为下方“方向变更”一节；后续旧设计及实施记录仅作历史追溯。本次仅改三份文档，不暂存／提交。

## 方向变更（2026-10-12）

本节是当前 Shape 的实现设计。用户已确定“在 AI 对话中学习”，这里的协议、持久化选择和 Q1–Q5 默认值仍待确认；不得把它们当作已发布能力。调查工作树为 `docs-shape-chat` / `493aaa5`，无既有未提交修改；批次 1、2、3（含 3c）已合并 main。v2 原型预期位于 `/home/yunyue/.config/edu-agent-demo/prototype/v2/index.html`，调查时不存在，按用户决定设计。旧批次的测试结果不构成本轮验收。

### A. 方案选择与新信息架构

推荐“现有 mentorrun 对话 + 系统维护目标 + 3c 单题会话桥接”。对话负责交流和卡片顺序，learning／tutoring 继续拥有活动、答案、评估与证据；learningstart 编排 owner 端口，不让模型直接写教学事实。

| 方案 | 取舍 |
| --- | --- |
| **推荐：从 3c 抽出单题发布桥接，接入有类型的对话卡片** | 复用真实评估／复习，不受旧课堂路线不能重复节点的约束；需补卡片身份、系统目标和自动评估编排。 |
| 将旧 TeachingWorkspace 整体嵌入聊天气泡 | UI 仍要求开课、生成路线、活动、获取反馈，无法兑现连续对话及改选后出题；不采用。 |
| 每轮伪装一个隐藏 Studio 测验，或另建聊天评分表 | 前者污染产物列表、绑死题组顺序与目标门槛，后者复制权威记录；均不采用。复用桥接逻辑不等于强迫聊天题创建 Studio 产物。 |

```text
首页：学习区卡片墙 → 创建（名称、学习方向）
学习区：来源 | 对话 | Studio
  来源：列表／勾选、添加／搜索、审阅、引用定位
  对话：消息流 + 练习卡 + 反馈卡 + 单一聊天输入
        新建／切换对话；练习卡内答案、实际帮助等级、提交
  Studio：指南／简报／测验／笔记及已保存版本
顶栏：返回、区名、学习区设置（名称、方向、生命周期）、菜单
手机：来源／对话／Studio，默认对话
菜单：进度与复习、评估与证据、历史及原辅助功能
```

移除新主流程中的 GoalSelector、GoalComposer、SessionPicker、“今天想学会什么”和目标搜索分页；不新增“先开课”的门槛。不展示“勾选只用于之后发起的追问与生成，课堂继续使用已冻结资料”“未选来源，不引用资料”等解释。来源个数、实际生成状态、缺少权限／模型、必要授权和失败恢复可以简短呈现。导入审阅、真实帮助选择、评估复核不能随文案精简一起删除。

新学习方向是用户明确输入的数据，不是知识来源或自动外部搜索词。宽泛方向没有天然总题数，不展示“方向完成 80%”；进度继续展示实际记录、已完成活动和到期复习。

### B. 现有生产能力核对

下列路径相对仓库根目录。调查以代码为准；旧第 5 节 G1–G6 是实施前缺口，现已交付，不再作为本轮待建底座。

| 链路／证据 | 已具备 | 本轮不能直接复用之处 |
| --- | --- | --- |
| `server/internal/mentorrun/conversation.go` 的 NewConversation／Submit／ReadConversation | goal 可空，多轮、保存／临时、分页、目的地变更确认；SubmitTurn 有 source_selection、operation_id、expected_version；原载荷先查回执。 | 会话可绑定一个 teaching_session_id；TutorTurn 只有消息、Output、manifest、citations，没有多张练习／反馈的有类型引用。保存的卡片不能只挂短期 run checkpoint。 |
| `mentorrun/source_selection.go`、`store.go`、`core.go` | 受理时 FreezeSourceSelectionTx；加密 manifest、文档／片段 hash 核验；sameSourceScope 只回放相同范围历史，撤权阻止读回／外发；selectedTool 仅 read_references／ask_user／confirm_focus。 | prepareSelected 明确排除 goal、课堂、memory／companion；completeSelected 只接 answer/citations。改 prompt 或让前端从文本解析题目均不足以支持教学。 |
| `learningstart/start.go` 与 `mentorrun/start.go` | 研究开学／批准参考开学：Prepared 校验、knowledge context、CreateSession、route/activity proposals、ApplyAction、EnsureTx。模型准备与事务发布分离。 | 依赖明确目标与研究／参考上下文；是现有开学入口，不是“每轮用当前勾选出题”的直接 API。新流程复用发布原则，不自动启动搜索／采纳研究。 |
| `learningstart/studio_quiz.go`、`learningcontent/studio_quiz_store.go` | 3c 单题专用会话；PublishReferencesTx（session 限定）、BindStartContextTx、真实引用映射、route/activity proposal 校验、内容发布；题目映射与幂等回执同事务。 | 绑定 Studio artifact/version/question；首题用 GetGoal 当前修订核验，后题要求前题 CompletedRoute。必须抽出通用单题内核，保留 Studio 包装和既有请求语义。 |
| `tutoring/state.go`、`learning/{command,proposal,assessment}.go` | 活动／提交／评估／反馈状态机；Activity 保存 goal、rubric、资料和版本；Attempt、AssessmentDecision、AcceptedEvidence 与操作重放完整。 | 不能把聊天字符串直接转 Attempt 或 accepted。现有 SubmitAttempt 只校验 help 枚举及 allowed_help，没有对话提示的持久最低帮助等级。 |
| `transport/httpapi/learning_content.go`、`clients/web/src/teaching-page.tsx` | content answers 绑定内容版本并调用 ApplyAction；UI 已有答案草稿、原操作核对、帮助、评估读取。 | 答案提交后目前另点“获取教学反馈”，开放题先 Propose assessment 再 record_assessment；新体验需要可恢复的自动编排。反馈确认不能自动确认 provisional。 |
| `learning/{progress,review_session}.go` 与对应 postgresstore | 进度按真实投影与 goal 修订关联，ReviewTaskID 绑定 goal/node；ReviewSource 以原 task/evidence/attempt 建复习承载，不复制证据。 | 系统目标需归到学习区／方向展示，不能混入主界面目标选择器；不改 reducer 或把每题 CompletedRoute 当方向掌握。 |
| `learningspace/domain.go`、`learning/goals.go`、`learning/command.go` | 学习区仅 name/description/status/version；GoalRevision 支持 source、previous_revision_id、管理状态和乐观版本。 | 无独立学习方向、无“系统管理目标”归属／幂等绑定。不能把已有 description 或私人 goal 文本直接改名后作为新方向外发。 |
| `identity/{domain,service,web}.go`；`transport/httpapi/admin.go` | 各业务 profile、scope 存储与 WebScopes 投影；管理后台独立会话。 | 无学习端全权组合；管理面签发处理器只接受 user/agent，新增 enum 本身不会形成可用签发入口。 |

### C. 学习方向 → 后台目标的维护

**推荐一个学习区一个系统管理 goal，方向修订对应 goal revision，活动再携带具体教学意图。** 不按聊天轮数创建目标，不让模型自由生成并切换一批 goals。方向无需被用户改写为精确完成标准；每题的 route step teaching_intent、completion_condition 和 rubric 承担可评估的小目标。

方向不复用 description：两者已有语义不同，而且 CLI 不应被迫改变。建议由 learning owner 新增按 `space_id + privacy_generation` 隔离的方向 head／revision 及 managed-goal binding，方向正文加密；空间 ID 与生命周期仍归 learningspace。新 Web 创建／设置经应用服务组合两个 owner 端口，单事务保存名称和方向，失败不留下半个新学习区。新字段与端口名须在 C2 锁定；旧学习区 create/update 缺省仍兼容，不把方向变成 CLI 必填项。

建议方向长度为 1–2000 Unicode 字符，创建时必填、设置可改；仅裁剪首尾空白，不用模型“润色”后替换原意。方向 revision 有操作回执、expected version、正文 hash，改区名不产生新方向修订。新表和操作 hash 的清除／轮换随 learning owner，不在 query、日志或索引重复保存正文。

| 触发 | 后台动作 | 对旧记录的处理 |
| --- | --- | --- |
| 创建区／保存方向 | 保存方向 revision；不调用模型、不创建 tutoring session。 | 不复制已有目标或成绩。 |
| 第一次发起学习对话／生成测验 | 应用服务在区与方向版本锁下 ensure managed goal，经 learning.CreateGoal 及必要的状态命令建立可用目标；source 标记为系统方向派生，binding 唯一约束防并发重复。 | 不是空字符串／默认占位目标；内容来自该区已确认方向。 |
| 继续交流／仅改选来源 | 使用当前方向绑定的 goal revision；按轮次保存方向和来源快照。 | 不为每轮增加目标修订，不改原目标 reference head。 |
| 保存新的方向 | 新方向 revision；下一次学习写入惰性 ensure 同 goal 的新修订并记录 previous_revision_id。并发按版本冲突处理。 | 在途请求按受理版本完成；已发布题及评估仍绑定旧 goal revision。 |
| 旧区第一次启用 | 按 Q1 展示以区名预填的方向输入并一次确认；不把私人描述／旧目标自动送模型。 | 旧手动目标、旧课堂、旧测验保持独立身份与 URL，不合并、归档或删除。 |
| 归档／恢复／隐私清除 | 由空间写门禁禁止新增学习动作；恢复核验有效身份与 generation；清除按 owner 移除方向／binding／新卡片数据。 | 不以方向更新触发历史删除；清除后旧操作不得重建已清数据。 |

创建 goal 与修订必须经 owner 的命令／事件／操作回执，不直接写 learning 表。binding 标记系统管理归属；通用目标编辑／生命周期入口不能绕过方向版本直接改这些新目标，旧手工目标继续原行为。来源不能塞进 goal description；每题的资料绑定放 session context，保持真实知识引用。自动目标不从 LLM 自述生成掌握度、deadline、能力自评或“已完成”状态。方向可以宽泛而每题具有具体可验证标准。

新学习 conversation 建议保持 space 级、`goal_id`／`teaching_session_id` 不作为固定教学绑定；新增显式 study 模式，每轮由服务端解析系统 goal revision。这样修改方向无需重建聊天历史，单题会话也不会覆盖当前聊天。旧 conversation 缺省仍是原导师语义，不自动升级。保存历史时附方向版本与 managed goal revision；新轮回放同时匹配来源与方向版本，避免旧方向的提示压过新决定。

进度沿既有 goal 修订与 node 规则聚合，标明历史归属，不自动跨修订 carryover。UI 将系统目标的技术标签映射为学习方向／练习名称；原进度 API、CLI 查询字段不变。复习继续用原 task/evidence/attempt 建立承载，首版通过已有复习页完成，不新增一套聊天复习调度。

### D. 对话出题 → 内嵌作答 → 评估 → 反馈

#### D.1 owner、协议与持久身份

以下均为**拟新增**，不是现有端点承诺。C2/C3 实施时先锁定 DTO 和错误合同，再修改 OpenAPI 和生成类型。

| 部分 | 最小设计 |
| --- | --- |
| study conversation／turn | NewConversation 增可选模式，SubmitTurn 的 study 模式必须显式 source_selection（含空选择）与方向版本；响应扩展有类型 items：text／exercise_ref。旧 output/citations 保留，普通 mentor 不接受新模式工具。 |
| 模型输出 | 受限结构：讲解及核验 citations，加可选一题 candidate（prompt、rubric、allowed_help、citation 引用、教学意图）。复用 Studio 闭合 schema、字符／数量边界与引用校验，首版开放文字题。模型不能提供权威 goal/session/activity/assessment ID 或完成状态。 |
| 对话卡片关联 | learningcontent 扩展 owner 端口，持久 `space / conversation / turn(run) / exercise_id / session / activity / content artifact + version / direction revision / source manifest` 关系。每轮最多一题有唯一约束；正文复用加密内容版本，引用不可指向短期内存。 |
| 单题发布内核 | 在 learningstart 从 publishStudioQuestionTx 抽出接受“已核验题目、确切 goal revision、来源 manifest、来源 origin”的事务端口。Studio 保留题组顺序及原入口，聊天调用同内核、不创建隐藏 Studio 产物。 |
| 作答与评估入口 | 卡片读取给出服务端核验后的活动／内容版本。拟加窄 `/v1/learning/exercises/{id}/answers` 与 operation 查询，内部复用 content-answer 校验／ApplyAction，并原子登记自动评估任务；不让客户端自己编排一串不持久的 POST。 |
| 反馈投影 | 对话卡片关联只保存 Attempt／assessment 身份和顺序，不存第二份成绩。轮次／卡片读取从 learning.Feedback／Session 查询投影显示 pending、待复核、完成等状态，刷新重开可恢复。 |
| 学习提示 | 为卡片新增有版本的帮助记录及受限提示动作；服务端记录已提供帮助下限，提交端核验 allowed_help 与该下限。外部帮助由用户如实申报，不宣称能自动侦测用户所有外部行为。 |

新方向／卡片／映射／评估任务都属于 learning 隐私 owner；来源正文和集合关系仍由 knowledge 管理；保存历史沿 mentorrun 现有加密与清除路径。挂靠哪个包不能改变清除 owner。对话删除按原合同删除聊天正文／关联呈现，不倒删已形成的学习事实；练习仍可由进度／评估页找回。全局清除则覆盖所有 owner 及恢复任务。

#### D.2 生产路径与状态

```mermaid
sequenceDiagram
  participant U as 对话界面
  participant M as mentorrun
  participant B as learningstart 桥接
  participant L as learning / tutoring
  U->>M: 发送（方向版本、当前来源、operation）
  M->>M: 保存输入范围，生成并核验讲解／题目
  M->>B: 发布单题（已核验候选、具体版本）
  B->>L: CreateSession → route/activity proposal → ApplyAction
  B-->>M: 已提交的活动／内容／卡片身份
  M-->>U: 讲解 + 可作答卡片
  U->>B: 卡片提交（答案、实际帮助、operation）
  B->>L: SubmitAttempt + 登记评估任务
  B-->>U: 提交回执／评估中
  B->>L: Propose assessment → record_assessment
  L-->>U: 反馈卡、证据状态、进度读回
```

1. **受理轮次**：核验 actor、区、方向版本、来源选择、模型目的地与预算；持久化本轮 manifest 和 direction/goal revision。模型只输出候选，不拥有正式写权限。
2. **发题事务**：重新核验隐私 generation、活动 owner 和可读资料；CreateSession → session 限定 PublishReferencesTx → BindStartContextTx → Propose route／activity → ApplyAction apply_route／issue_activity → EnsureTx。发布卡片映射、运行结果与历史引用一致提交；模型调用在事务外。只有有完整会话／内容的题才能出现提交按钮。沿既有 present_activity 动作进入可答状态，展示回执可重放，不能跳状态机。
3. **单题与版本**：复用每题独立会话，因此新来源、新知识节点甚至重复节点可用于下一题，不修改旧路线。通用桥接接收确切 goal revision，不再在中间偷读 latest。对新系统目标模式，方向已更新也允许旧题按受理修订发布／作答；旧 Studio／CLI 请求的版本冲突规则保留。Studio 适配见 F。
4. **提交事务**：客户端提交卡片 ID、内容／活动版本、答案、实际 help、generation 与原 operation。服务端解析权威会话、校验来源仍可用、帮助下限和 expected version，然后复用 SubmitAttempt；同事务登记唯一 `attempt_id` 的评估任务。丢响应先查询原 operation，不能靠新 UUID 再提交。
5. **自动评估**：当前内容 API 没有“提交即评估”，须新增可恢复的应用编排。复用 mentorrun 的持久任务／租约／预算机制，增加受限评估任务模式及学习领域端口；输入只能是该 Activity、Attempt、rubric、原 references。任务绑定提交者、generation、模型目的地／配置指纹和有界预算，执行前重验权限与来源；配置改变沿用目的地确认，撤权或取消不删除已提交答案。开放题 Propose assessment → record_assessment，客观题按原确定性路径。request_id／operation_id 在任务创建时确定，重启重复执行只读回原结果；模型失败保留 Attempt，重试不新建答案。
6. **反馈与继续**：反馈卡读真实 AssessmentDecision、依据和 Evidence。用户“继续”仅调用 acknowledge_feedback；provisional 仍受 guardFeedbackExit 约束，需要复核时给出短操作入口，不能自动调用 confirm/override。待处理卡不阻止普通聊天。Studio 逐题开始仍须前题真实完成；对话可暂存题目，不伪造 CompletedRoute。
7. **进度／复习**：题目生成、展示或“稍后再做”没有 Attempt/Evidence；仅 accepted Evidence 按原 reducer／review policy 生效。每张卡可跳对应反馈记录，进度／复习读同一记录并覆盖重放验证。

| 用户所见 | 权威状态／恢复依据 | 允许操作 |
| --- | --- | --- |
| 生成中 | run 已受理，尚无题目映射 | 取消／核对；不能作答 |
| 练习 | 已发布 session/activity/content，present 成功 | 输入、帮助、提交、稍后再做 |
| 稍后再做 | 卡片焦点暂存；原 session 不造完成状态 | 继续聊天／之后恢复同题，不记分 |
| 提交待核对 | 原 operation 结果未知 | 查询或重发相同载荷，保留原草稿 |
| 评估中／评估失败 | 已有 Attempt；任务 pending／failed | 核对、按预算重试同一答案，不再提交答案 |
| 反馈待处理 | provisional／不可确认或待复核原因来自领域 | 进入原复核入口；不自动产生 accepted |
| 反馈／已完成 | 已记录评估；ack 后按原状态机结束单题会话 | 查看依据、继续；完成题目不等于掌握方向 |

首版每个对话同时一张待答卡；稍后再做只转移卡片焦点，未答记录可恢复，恢复前处理当前待答焦点，不让多题答案共用草稿键。卡片在同一 turn 后有稳定位置，评估迟到更新该卡，不追加一份重复“成绩消息”。已保存历史不能因 run TTL 到期丢失卡片身份。

**帮助下限的新增边界。** 当前前端帮助选择与 allowed_help 校验不足以保证聊天提示不可降级。所有本系统提供的、绑定该题的提示经卡片帮助动作记录等级，正文交付与帮助记录一致；提交与提示并发需锁定活动状态，先交付帮助则提高最低等级，提交先成功则提示不得伪装为提交前帮助。普通聊天不自动携带当前题正文／rubric；用户要针对本题提示时走该动作。若用户自行在聊天或外部寻求帮助，仍应在卡片申报实际等级，不能把模型对帮助程度的猜测当安全证明。

帮助下限、绑定内容版本及评估任务登记必须落在这些新活动共用的领域提交路径／owner 端口：直接调用原 content answers 或 tutoring actions 也不能绕过校验或漏建任务。旧未映射活动继续原流程；不能只在新 exercise HTTP handler 中加检查。候选回执重放先识别已经成功的提交，不能因之后的状态变化重新创建 Attempt。

### E. 来源选择、历史与评估资料一致性

保持 G3 的显式来源 manifest，不把左栏写成长期 goal reference policy。每轮提交捕获本标签页当时的选择；加载失败阻止依赖资料的请求，明确空选择使用独立枚举，旧请求缺省继续旧合同。来源翻页不自动扩大选择，“全选已加载”不得冒充全区。

| 场景 | 新轮次／出题／Studio | 已发题与反馈 |
| --- | --- | --- |
| A 改为 B | 新受理请求只读 B；在途 A 请求按 A 完成。 | A 题仍按 A 的 activity/rubric/references 评估，不能读 B 或资料 latest。 |
| A 文档更新版本 | 只使用本次选择中确切 revision；不在 worker 内追 latest。 | A 旧版本引用与 hash 不变，旧版本可读才可继续。 |
| 不选来源 | 按 Q2 一般交流；资料题／指南／简报／测验入口要求添加／选择资料。 | 原题不依赖当前勾选，可按自身已保存版本提交评估。 |
| 来源撤权／解绑／清除 | 受理、外发与返回正文均重新核验 owner／链接和 generation。 | 不以题目保存过为由绕过撤权；读回／评估停止，显示不可用，不拿新来源补位。 |
| 修改方向但来源不变 | 新轮匹配新方向版本，系统 goal 修订由服务端解析。 | 原活动与反馈仍按旧方向／目标修订保存。 |

study 模式扩展 prepareSelected 的**窄上下文**：当前方向作为用户意图、当前轮核验来源、同方向同 manifest 的安全历史，加无正文的学习状态（活动身份、待答／已答、帮助状态）。不得恢复整个旧 goal/session snapshot、通用跨区工具或记忆全文读取。新专用模式允许发布练习的编排端口，但不能放开原 selectedTool 白名单给任意写工具。

继续使用 sameSourceScope 的保守相等策略；新增卡片引用、反馈、摘要也必须携带来源和方向 provenance。改选后可保留屏幕上的旧消息，发送上下文时排除不匹配轮次及其来源衍生内容；首版不做跨范围“自动总结”，也不把旧练习反馈作为新 prompt 隐藏拼接。允许用户明确重新输入自己的问题，不承诺识别所有手动复制的文本出处。

**对旧题获取提示**与评估不同：提示是新的对话动作，也受当前勾选约束。首版仅在当前选择与该题范围兼容（保守按 manifest 相等）时允许携带题目正文获取提示；否则提供“选择这道题的资料后再获取提示”的简短入口，不能暗中恢复 A 并外发。旧题评估的专用模型请求始终使用 A，且不注入新 B 对话或新方向，这是用户已决定的评估一致性例外。

长期记忆候选可通过既有 memory 工具／面板形成并审阅，但新 study 模式首版不自动读取长期记忆全文，也不从课程正文生成长期偏好。显式“记住这个偏好”只传用户指定文本走候选流程；记忆仍为全局语义，不授予跨区资料读取权。scope 齐全不等于模型可以直接批准记忆。

### F. Studio 与已交付代码的复用／替换

| 区域 | 继续复用 | 需要替换／删除的部分 |
| --- | --- | --- |
| 批次 1 卡片墙／三栏／菜单 | notebook-home-page、notebook-header/layout/mobile tabs、生命周期、DraftStore、安全导航。 | 中栏 ClassroomPanel 的目标／会话选择、GoalSelector 与建立目标入口从新路径移除；菜单“管理学习区与目标”改“学习区设置”。仅无引用后删除 Notebook 专属文件，不删除旧领域组件。 |
| 批次 2 G1/G2 | 来源目录与 provenance、四入口、搜索先选后读、PDF 缺口、导入预览／确认与 operation。 | D1 横幅、“尚未自动用于目标”类说明换成实际保存／可选状态；必要导入确认按钮不删除。 |
| 批次 2 G3 | SourceSelectionProvider、manifest、引用解析、历史范围过滤、撤权／清除检查、SSE／预算／回执。 | MentorComposer 不再作为课堂尾部的第二个输入；扩展为主 ChatPanel，移除来源问答与课堂讨论双模式 UI。旧无模式 API 不改变。 |
| 批次 3a/3b G4/G6 | 笔记版本、转来源、指南／简报 schema、引用、生成恢复、加密与清除。 | 移除目标前置提示；生成读取当前选择，原持久产物和引用不迁移重写。 |
| 批次 3c G5 | quiz schema、题组与活动映射、单题发布、答案／评估／进度／复习事实、逐题顺序与原操作核对。 | 新 Web 不传用户选择的 goal；应用层按方向解析目标。StudioQuiz 不再嵌套完整 TeachingPage 控制台，复用 InlineExerciseCard／FeedbackCard。删除新主流程中的“正式作答”等文案。 |
| 旧课堂与目标、CLI | 旧 URL、操作事件、学习／评估 API、原内容库和 CLI 缺省合同。 | 不作 Git revert，不删除历史目标／会话／migration；新入口不再引导用户管理目标。 |
| 配对 | 旧 profiles、WebScopes 白名单、设备／会话、管理后台认证与 privacy grant。 | 新增显式学习端全权签发入口，旧“UI 一体化不能组合 profile”限制被替代；不把旧 user scope 静默扩大。 |

Studio 新 Web 通过方向模式的生成参数／适配端口，由服务端 ensure goal，仍保存确定的 goal_id/goal_version 与 source manifest。方向更新后，已经生成的系统目标题组允许按其已保存 goal revision 开始／继续；需修正 3c 首题依赖最新 GetGoal 的假设，按受理模式选择精确修订，而不是全局放松旧版本检查。旧手选目标的测验、API 与 CLI 保持原核验规则。题组属于创建时方向，不能把旧题挂到新的目标修订。

学习指南／简报不需要伪造目标；笔记仍不调用模型。自动题不进入 Studio 保存列表，只有用户主动发起的测验才是 Studio quiz。新内嵌卡片可以复用在 Studio 详情，题组的前题完成规则保留，避免将 UI 改造变成更换测验语义。

### G. 组件与路由变化

| 位置 | 拟调整及责任 |
| --- | --- |
| `clients/web/src/notebook-home-page.tsx` 与现有 SpaceEditor | 新建表单增加方向；旧区 Q1 补方向使用同验证，不复用 GoalComposer。 |
| `notebook-workspace-page.tsx` | 管理区、面板、对话定位与来源选择；不拥有目标选择／评分。StudyConversationController 负责轮次、取消、恢复；卡片控制器负责题目操作。 |
| `components/notebook/chat-panel.tsx`、`conversation-timeline.tsx`（拟新增） | 有类型消息、卡片、统一输入与历史分页；复用 MentorPanel 的协议控制部分，避免直接嵌整个旧课堂。 |
| `inline-exercise-card.tsx`、`feedback-card.tsx`（拟新增） | 从 teaching-page／StudioQuiz 提取必要答案、实际帮助、反馈和 operation 控制；草稿键含 conversation/exercise/activity/content version。 |
| `space-settings.tsx`（拟新增） | 名称、学习方向、生命周期；不展示系统目标管理。 |
| `source-selection.tsx`、来源查看器、`api/conversations.ts`／`api/studio.ts` | 当前选择进入每个新请求；引用导航扩展 exercise/feedback 类型，由 owner 解析，不猜 ID。 |
| `main.tsx`、`styles.css`、NotebookLayout | 主面板名改 chat／对话，保留 query 兼容；移动焦点、单滚动消息区、固定输入与卡片独立表单。 |

| 路由（含 /app） | 新行为／兼容 |
| --- | --- |
| `/app/` | 卡片墙与名称／方向创建表单。 |
| `/app/spaces/:spaceId?panel=chat&conversation=:id` | 默认聊天工作区；conversation 校验区、principal 与 generation。首版沿当前设备的保存会话列表恢复，不承诺跨设备合并历史；多标签选择与草稿独立。 |
| `?panel=classroom`、旧 `?goal=...` | classroom 作为 chat 的兼容别名；goal 若存在仍核验归属，提供旧目标历史入口，不能变成系统方向绑定或静默忽略跨区错误。新链接不再生成 goal query。 |
| `/app/spaces/:spaceId/manage` | 学习区设置，方向与名称可改；移除主界面目标搜索与管理列表。旧目标详情／研究页保持直达。 |
| `/app/spaces/:spaceId/chat[/:conversationId]` | 保存的 study 会话可定位到同一工作区消息；旧 mentor／临时历史继续原读取语义，不能自动改成 study。 |
| `/app/spaces/:spaceId/learn/:sessionId` | 若为新卡片承载，可解析到对应卡片；旧课堂保持原 TeachingPage 的兼容视图及动作，不强制转成新对话，不重开会话。 |
| `/app/spaces/:spaceId/goals/:goalId[...]` | 保留原目标详情／历史／研究恢复；从历史相关入口到达，不在新对话显示选择器。 |
| `/app/spaces/:spaceId/studio`、`/app/content/:id`、进度／反馈等 | 旧内容库与版本链接保留；新四产物仍在工作区 Studio，不冒用旧 content ID。 |

浏览器前后退、刷新、原参数与错误边界列入验收。URL 不放正文、答案、方向或来源选择全集；来源切换只改面板与焦点，不重建运行。服务端 SPA 资产／API 边界沿现有实现，不为新 UI 扩大 catch-all。

### H. “学习端全权”配对档案

建议内部值 `learning_full`，展示名“学习端全权”；仅管理员明确签发新码，交换一次后获得以下**去重后的既有 scope 组合**：

| 能力 | scopes |
| --- | --- |
| 学习区、方向、对话、Studio、活动、进度／复习 | `learning:read`、`learning:write` |
| 导入、集合／参考审阅、笔记转来源 | `knowledge:read`、`knowledge:write`、`knowledge:approve`、`imports:web`、`references:manage` |
| 研究采纳／搜索链路 | `research:adopt`，并沿用上行知识权限和原外发授权 |
| 评估复核 | `learning:approve`、`assessment:web` |
| 长期记忆候选、审阅、更正 | `memory:read`、`memory:write`、`memory:web` |
| 学习侧数据状态、设备列表／撤销 | `privacy:read`、`devices:read`、`devices:manage` |

这是 import、references、research、assessment、memory 档案的学习业务并集，不是给 profile 字符串一个“绕过鉴权”的特殊含义。现有 WebScopes 已有这些显式标记的保留分支；新增档案要同时测试原 token 和实际 WebPrincipal 的有效 scopes，不能只测 pairingProfileScopes。

**明确不授予：** `privacy:erase`、`privacy:device`、管理后台身份、任意通配 scope；按 Q5 也不授予 `settings:write`／`settings:probe`／`model:probe`。全局清除仍要求设备绑定的一次性 grant，学习端全权不能签发该 grant。devices:manage 只保留现有撤销设备能力，不获得签发配对码或管理后台登录能力；记忆为全局单用户语义，不假装按区隔离。

新档案不改变 user／agent／其他旧 profile，不升级存量 token。权限足够后仍保留导入审阅、外部请求授权、目标模式的具体写门禁、候选审阅和评估决定；模型建议不能代替用户批准，界面不为每个学习功能要求再换一张配对码。

**可用签发入口：** 在现有本地管理后台配对下拉及 `adminCreatePairingCode` 白名单增加 learning_full；沿用管理后台 Cookie、Origin、CSRF、限流和本地 transport。学习端只兑换码和展示有效能力，无自授入口。涉及 `server/internal/identity/{domain,service,web}.go` 和 `server/internal/transport/httpapi/admin{.go,ui/...}`；不修改 `clients/cli-go` 或 `server/cmd`。当前管理面私有 HTTP 合同不在公共 OpenAPI，现有兑换 DTO 只交换码，scopes 已为字符串数组，纯 profile 组合不需要 SQL migration 或公共 OpenAPI 改动。

### I. 后端缺口清单（本轮 H1–H8）

“是”表示后续 Build 必须补合同或迁移；本次均只设计。新增 migration 在 Build 按最新序号分配，绝不修改 000034–000037 等已交付迁移。

| 编号／现状 | 最小方案 | OpenAPI | migration | 风险 |
| --- | --- | --- | --- | --- |
| **H1 方向／系统目标**：区无方向，goal 无系统绑定 | 独立版本方向与 managed-goal binding；创建／设置组合 owner；学习写入幂等 ensure goal/revision。 | 是：可选方向／能力与版本合同，旧请求保持兼容 | 是：方向、binding、操作身份及 owner 门禁 | 高：重复目标、方向并发、历史成绩被重绑、清除漏项。 |
| **H2 学习对话**：selected 路径仅问答，turn 无卡片 | 显式 study 模式、方向受限上下文、结构化题目候选、turn items；保存历史携带方向／来源及卡片引用。 | 是：conversation/turn/snapshot DTO | 是：会话模式／查询需要的持久标记；加密 body 版本也需兼容，不只改 schema | 高：误开通用工具、旧正文外发、历史 TTL／模式兼容。 |
| **H3 内嵌题发布**：3c 只接受 Studio 题组 | 抽取通用单题事务桥接，新增对话题映射与读回／operation；题目继承确切版本。 | 是：exercise 卡片读取与操作合同 | 是：映射、唯一约束、回执；复用现有活动／内容正文 | 高：孤儿卡片、重复会话、跨 owner 原子性、首题 latest 假设。 |
| **H4 提交到反馈**：手动获取评估，无对话帮助下限 | 答案提交与评估任务原子登记；受限 worker 调用原 proposal/assessment；帮助记录与反馈查询投影。 | 是：窄答案／帮助／任务状态／恢复合同 | 是：评估任务、帮助事件／版本；若复用 run kind 须扩约束 | 高：重复 Attempt／评估、重启失联、帮助降级、provisional 误通过。 |
| **H5 动态来源**：已有 G3 manifest/hash/历史隔离 | 将相同约束贯穿 study、卡片帮助、生成、评估及反馈读回；评估只读原题快照。 | 扩展随 H2–H4；无需新“切换课堂资料”API | 无独立 migration，复用 H2/H3 版本记录 | 高：改选后旧反馈泄漏、并发撤权／清除、错误读取 latest。 |
| **H6 Studio 方向适配**：quiz 要求手选 goal/version | 新方向模式服务端解析目标；系统题组开始时读保存修订；UI 复用内嵌卡，保留旧调用校验。 | 是：新增可选模式／方向版本、互斥校验 | 无独立业务表；生成正文版本及 H1 绑定足够 | 中高：破坏旧题顺序、旧测验版本冲突语义或把方向更新重绑成绩。 |
| **H7 权威记录呈现／隐私恢复**：进度／复习已有，新对象未纳入 | 系统方向标签与历史映射，learning/knowledge/mentorrun 清除／Remaining／轮换／重放接入；保留旧 ReviewSource。 | 否：已有进度事实足够；卡片扩展归 H3/H4 | 无独立 migration，相关约束随 H1–H4 | 高：后台“隐藏目标”变丢失记录、新任务或历史密文在清除后复活。 |
| **H8 学习端全权**：profiles 分散，管理签发仅 user/agent | 新 profile 的明确 scope 并集、管理 UI 签发、WebScopes 与负权限验证。 | 公共 OpenAPI 否；管理面私有 DTO／UI 是 | 否：既有 scope 数组可存 | 中高：错误扩大旧设备权限、误授 settings／privacy／管理面，或只改 enum 无入口。 |

H1–H4 是主要新增能力，H5/H7 是必须随生产路径完成的边界，不能统一延期到最后。G0 卡片真实汇总／最近排序仍为额外增强，不包含在本轮工时。零来源可评估练习、多题一次提交、跨设备聊天合并、智能跨来源历史摘要不在默认计划。

### J. 旧测试迁移策略与本轮证据

旧断言先分“被替代入口”与“仍有效领域事实”。改变定位／按钮用词后仍验证同一个 operation、Attempt、来源版本和权限结果，不能删掉安全断言换成“卡片可见”。

| 用例／层次 | 保留与迁移 |
| --- | --- |
| `notebook-workspace.spec.ts` | 将创建目标、目标分页／草稿、中栏课堂及 D1 横幅断言改为方向创建／设置、连续聊天、内嵌卡与方向／来源切换；保留双标签隔离、归档、迟到响应、菜单焦点、分页目录、引用核验、unknown。 |
| `notebook-sources.spec.ts` | 保留真实导入／搜索／部分解析与原操作核对；仅更新“正式导入／尚未自动用于目标”定位文案；补改选后出题与后台 A/B 输入检查。 |
| `notebook-studio.spec.ts` | 删除“没有目标测验禁用”和“必须先选目标”的期待；改为有方向可生成，系统目标惰性创建、旧方向题组继续、逐题提交与反馈；保存、转换、409、撤权、CSRF、草稿用例保留。 |
| `workspace.spec.ts`、`learning.spec.ts`、`review.spec.ts` | 旧课堂／目标深链接作为兼容场景保留原教学变更、帮助、评估、复习与 CLI 读回；首页 helper 改方向创建，不能把所有旧业务测试重写成新消息可见测试。 |
| `mentor.spec.ts`／`tutor-history.spec.ts` | 旧临时／保存、目的地、预算、SSE、回执不变；新增 study 保存重开、方向／manifest 历史过滤，旧无模式请求继续原行为。 |
| identity／admin HTTP | 精确比较学习端全权 token/WebPrincipal scopes；单次配对走真实导入→对话→Studio→记忆候选。独立断言 settings（默认）、admin、无 grant 清除被拒绝；旧 profiles 权限不变。 |
| learningstart／learning／learningcontent 的 PostgreSQL | 复用 `TestPostgreSQLStudioQuizFormalSessions`、`TestPostgreSQLProgressScopePaginationAndReplay`、`TestPostgreSQLReviewCarrierPreservesSourceAndReplay` 等真实路径；新增聊天题 A、改选 B 后评估仍 A、方向改版、重复提交、帮助并发、任务重启与清除不可复活。 |
| 合同／migration／候选 | 新字段缺省兼容、旧加密 body、从 main 升级及重复 Apply、轮换与 privacy Remaining；新增 DB 必需用例接入 release 门禁，skip/retry 不当通过。 |

每批先做受影响包、DTO／前端类型、最小真实 HTTP／PostgreSQL 与 Chromium 流程。C3 的核心评估／幂等／隐私不能用 mock 冒充；模型 fixture 用于稳定断言，真实模型 smoke 另记录结构、引用和实际反馈链，不替代浏览器／DB。只有新增变更或失败影响既有结果才重跑对应检查。

稳定候选再做 390／768／1024／1280／1440px、浅深色、IME／软键盘、键盘焦点、axe／对比度及适用三引擎，运行项目现有 `make web-release-check`／`make web-release-candidate`。按 evidence key／输入 hash／环境判定可复用证据，旧 main 的 Chromium 数量不是新实现已通过数量。CLI 只验证兼容，不改代码、交互或帮助。

### K. 新分批计划与工作量

本目录继续作为 Supervisor Shape。C1–C4 为可独立演示的 child，C5 汇总候选；每个 child 10–30 项正式验收，超过工作流阈值重新拆分。计划覆盖父 spec 30 项；文档责任沿工作流：brief 记结果／决定，spec 记可观察行为，本节记技术方案，verification 由 Runtime 生成。本次不另写实现代码或新计划文件。

依赖建议 **C1 → C2 → C3 → C4 → C5**。C1 技术上可先行，C4 依赖 C2 系统方向和 C3 卡片／桥接；OpenAPI、migration、composition、共享 UI 默认串行。一次只推进一个明确批次，不先交付横向表／SDK 再长期等待 UI。以下估算为一名熟悉仓库的工程师人日，含相关测试、迁移、审阅修复，不含确认等待、部署或外部环境采购。

#### C1：一次配对完成学习侧操作（2–3 人日）

- **结果：** 管理员可签发“学习端全权”，用户一次兑换后使用现有来源、研究、Studio、评估与记忆候选入口。
- **范围：** H8；identity、admin handler/UI、WebScopes／能力展示与局部浏览器。父 spec 26–29；不动 CLI、不扩大旧档案。
- **延期：** 新方向、聊天出题到 C2/C3；模型／搜索配置写权限按 Q5 独立。
- **退出标准：** 真实管理签发→浏览器兑换→导入／Studio／记忆候选可用，所有相关 scope 与 WebPrincipal 一致；旧 profiles 不变；管理后台、settings、无 grant 清除负权限成立。受影响 Go／Web 检查和真实 HTTP/DB／Chromium 通过，文档记录结果。
- **重型证据：** 三引擎全套、全仓兼容到 C5；精确权限正负矩阵及 grant 隔离本批必跑，不能只测 identity 枚举。

#### C2：按学习方向持续对话（5–7 人日）

- **结果：** 新区填写方向，旧区可补方向，在主消息流学习，修改方向／勾选后下一轮使用新输入；保存对话可重开。
- **范围：** H1/H2、H5/H7 的对话部分；方向与系统目标、study 纯讲解模式、历史保存、ChatPanel、路由／设置、移除旧主入口。父 spec 1–14、27–30。最小新迁移与旧字段兼容同批完成。
- **延期：** 自动出练习到 C3，Study 模式本批不显示不可作答的“练习卡”；Studio 取消目标门槛到 C4，旧已保存产物仍可读。
- **退出标准：** 创建／修改方向、并发 ensure 只有一个系统 goal、方向更新形成修订；A→B 与方向变化的模型输入隔离；保存重开、模型目的地、unknown 与撤权／清除有效。类型／受影响 Go 测试、方向 migration 升级和真实 PostgreSQL、主对话 Chromium 场景通过；确认没有主界面手工目标门槛。
- **重型证据：** 全历史矩阵与三引擎到 C5；方向 owner 清除／轮换、旧 create/update／conversation 兼容本批必跑。

#### C3：对话内一题走完学习闭环（8–12 人日）

- **结果：** 导师讲解并插入卡片，用户答题／选择实际帮助／提交后自动收到反馈，进度与复习读到同一事实。
- **范围：** H3/H4、H5/H7 的全部练习边界；learningstart 通用单题桥接、learningcontent 关联、持久评估任务、帮助下限、卡片 UI、操作恢复与反馈。父 spec 11–19、25、27–30；不重写评分算法。
- **延期：** 零来源可评估题、多题单次提交、聊天中自动编排整套复习；既有复习页继续可用。
- **退出标准：** 真实来源→有效题→Attempt→AssessmentDecision→Evidence／进度／到期复习；改选 B／改方向后旧 A 题评估仍 A；响应丢失一次 Attempt，重复任务不重复评估，提示并发不可降低 help。服务重启、provisional、来源撤权／全局清除均有真实 DB 证据；Chromium 卡片完整闭环及一次受预算约束的真实模型链路证据，旧 3c 桥接兼容绿色。更新缺口结论与 child 验收。
- **重型证据：** 全仓 DB 历史／三引擎到 C5；本批的正式评估、重放、帮助、跨 owner 原子性与清除不能延期。若该闭环无法在一个候选控制验证范围，先回 Shape 拆子 change，不用半个卡片假作完成。

#### C4：Studio 全部接入方向与内嵌作答（2–4 人日）

- **结果：** Studio 无需选目标即可生成测验，使用共享练习／反馈卡；四种产物和笔记转来源继续完整可用。
- **范围：** H6 及必要 H7；生成方向适配、精确目标修订、StudioQuiz UI 和文案、相关 DTO／引用／历史读取。父 spec 20–25，回归 11–13、16–19、27–29。
- **延期：** 新产物类型、自动笔记、笔记双向同步与 G0 卡片统计。
- **退出标准：** 方向生成题组→逐题作答→反馈，方向修改后旧题组仍按原版本开始／继续；旧手选目标测验请求语义不变。指南／简报引用、笔记版本转来源、取消／重开、unknown、草稿和跨区权限通过受影响检查及真实 Chromium／DB 流程。
- **重型证据：** 适用三引擎和全部产物历史矩阵到 C5；原 3c 数据读回、首题版本兼容、幂等与失权本批必跑。

#### C5：手机与完整候选验收（3–5 人日）

- **结果：** 手机、键盘和桌面均可完成对话学习，旧入口与 CLI 兼容，父 spec 有逐项证据。
- **范围：** 父 spec 1–30 收口，尤其 6、28–30；移动布局／可访问性必要修复，旧测试迁移，release 必需用例与 evidence key。不新增业务或评分状态。
- **延期：** 无已承诺行为延期；真实环境缺失作为未验收项登记，不写通过。
- **退出标准：** 相关局部检查绿色后，稳定输入下完成 web-release-check／candidate、适用三引擎、真实资产／深链接、所需 DB 迁移／重放／隐私、CLI 读回兼容。无跳过／重试伪通过，完整 spec 对应证据，Runtime 后续按流程 Verify。
- **重型证据：** 本批集中收口。复用条件满足的局部证据，不重复无变化全扫；输入变化按 evidence key 判定失效，补验真实缺口。

合计 **20–31 人日**（约 4–6 个单人工作周），不是撤销前三批再重建。主要不确定性是 C3 的持久自动评估、帮助竞态与跨 owner 提交；建议另留 3–5 人日风险余量，不能把余量记作新增功能授权。若 Q2 改成零来源可评估、Q3 改成多设备共享历史，需重新评估知识依据／数据所有权，不套用此估算。

### L. 待确认与本次核验边界

待确认问题集中于 [brief 的 Q1–Q5](../comet/specs/notebook-workspace/brief.md#用户已确认2026-10-13采用建议默认值)：旧区补方向、零来源练习、默认保存对话、自动出题节奏、配置写权限范围。本节按建议默认值具体化，未重复请求用户确认已经决定的“对话为主、无手工目标、练习记分、来源实时选择、管理／清除独立、不动 CLI”。

本次只调查代码并更新三份文档；文档核验检查职责、引用、验收编号／替代关系、缺口与批次覆盖以及修改范围。不运行产品测试、数据库迁移、模型或浏览器，不生成新的“测试通过”结论，不创建 verification，不暂存／提交。

---

## 历史设计与交付记录（v1）

以下第 1–8 节保留旧 Shape、批次 1–3 实施记录和当时证据，便于追溯，**不是本轮 Build 的有效要求或剩余计划**。其中“中栏目标／课堂”、D1 范围横幅／冻结课堂语义、D4 手工目标门槛及“配对能力不可组合”已由上节替代；旧 28 项编号与 28–41 人日估算不再用于本轮。旧第 8 节批次 4 的未完成候选义务合并到 C5；当时尚未实现／当前分支等措辞只表示当时状态。

## 1. 方案与拆分结论

推荐“新工作区壳层 + 既有领域组件 + 有界后端补充”：卡片墙与三栏重排用户入口，课堂状态机、来源 owner、正式答案和旧业务页面继续复用。只抽出嵌入新工作区必需的页面片段，不顺带重构所有旧页。

考察的替代方案：仅给现有页面换皮，成本较低但仍是多页跳转，无法兑现来源勾选与 Studio 四种产物；完全重写前后端，隔离更彻底但会重复教学、安全和恢复逻辑，超出本次需要。原型源码只证明布局与交互方向，其中生成定时器、假来源、百分比和按钮 alert 都不是生产能力。

本需求有壳层、来源、Studio 等独立结果，符合工作流拆分条件。建议登记一个 Supervisor 与四个有依赖的 child，按文末四批串行推进；批次 3 再有界拆为三个可运行切片。父 spec 28 项，不因同处一个页面而把协议、迁移、模型生成和跨浏览器验收塞进一个普通 change。共享 `main.tsx`、`styles.css`、OpenAPI、生成类型、migration 和 composition 的写入不并行。

## 2. 信息架构与状态归属

```text
首页：学习区卡片墙
  └─ 学习区：来源 | 课堂 | Studio
       来源：资料列表 → 添加／审阅 → 原文与引用定位
       课堂：目标选择／建立 → 课堂选择／开课 → 讲解、练习、反馈、复习
             └─ 追问／导师交流（正式答案控件独立）
       Studio：学习指南、测验、简报、笔记 → 结果查看／作答／保存
左上菜单：全部学习区、进度与复习、评估与证据、导师历史、长期记忆、任务中心、
          知识结构与维护、NoteSync、离线、原内容库、设置、数据与设备
```

### 2.1 数据与使用范围

| 对象 | 权威 owner／现有模型 | 新界面的含义 |
| --- | --- | --- |
| 学习区 | `learningspace` | 唯一笔记本实体；不新建 notebook 表，不复制目标或知识。 |
| 来源 | `knowledge` 的 collection、revision、document／node，研究来源另有来源元数据 | 列表按文档组织，保留集合和版本身份；一个集合可以有多份文档，不把集合数显示成来源数。 |
| 目标、活动、答案、评估、证据、进度 | `learning` | 中栏和测验读写正式领域路径；界面不计算新掌握分。 |
| 课堂与学习焦点 | `tutoring` | 继续／恢复指定会话；正式活动的引用冻结，不因换标签改变。 |
| 导师会话／运行 | `mentorrun` | 沿用保存／临时、多轮、目的地确认、预算、回执与恢复；新增每轮来源范围。 |
| 现有课堂正文 | `learningcontent` | 与活动绑定的阅读／练习正文、版本、选段加工、收藏、导出；继续保留。 |
| 新 Studio 产物 | 建议仍由 `learningcontent` 所属学习隐私 owner 管理，使用独立产物表 | 指南、简报、笔记不假造 activity／session；测验结果关联正式课堂／活动。 |
| 在线草稿与左栏选择 | `DraftStore`／工作区内存 | 仅本标签页；选择是请求输入意图，不是目标参考政策或新持久化授权。 |

【D1】必须在 UI 上区分三种范围：区内已保存资料；左栏“用于下一次追问／生成”的选择；课堂已冻结资料。来源选择预置为本次进入时可用的正式资料，首次外发仍通过明确的发送／生成动作及原有授权检查；之后新导入的资料不悄悄加入既有选择，用户可全选或单选。取消勾选只取消后续使用意图，不删除资料、不解除集合引用。零选与尚未加载完成不同，不能把失败／空数组回退成全库。

分页／懒加载时“全选”须明确是“全选已加载来源”，未加载项不计入已选数，也不在翻页后悄悄加入请求；真正全区选择需完整枚举获准条目并在提交前显示实际数量。首次导入没有可写集合时，对话框说明将创建本区私有资料集合，随用户明确添加流程创建并保留原操作身份；已有集合可选择，不能向全局默认集合静默写入。该过程不自动共享集合或采纳为课堂参考。

开课继续从目标已正式采用的参考／现有研究开学路径建立上下文，不能直接把左栏勾选当作参考审批。无参考目标可保存；要用于课堂，提供“设置课堂资料”入口复用已有 preview／confirm。后续显式教学变更只影响规则允许的后续内容，原活动仍冻结。

### 2.2 首页与空状态

- 卡片显示名称、描述／状态及真实更新时间；复用学习区创建、编辑、归档、恢复。创建成功进入新学习区，显示添加来源与建立目标入口，不自动发起研究或课堂。
- 当前学习区 API 只有元数据、搜索、状态和游标，按 ID 排序；没有来源／目标计数、最近学习时间或最近排序。第一批保留真实分页顺序，隐藏原型“最近”伪排序和缺少依据的计数；时间只能标“学习区更新”，不能标“最近学习”。如要完全兑现卡片统计／最近学习排序，见 G0，可作为后续单独确认的增强，四批估算不含它。
- 目标选择保留搜索、状态与分页入口，不截取前 100 个当全部；进入学习区默认显示目标与继续学习，已明确会话的深链接直接恢复该会话。禁止为“智能续学”静默采用另一个目标。
- 【D4】无目标时，来源与非测验 Studio 可工作，中栏显示建立目标和独立来源问答；这是建议默认，用户若要求所有操作先绑定目标，需同步本页及相关 child 合同。缺权限、缺模型、缺来源各有行动提示，不能一律显示空列表。

## 3. 路由与旧链接兼容

继续使用 TanStack Router 的 `/app` basepath。采用原有学习区路径作为三栏入口，避免引入一套 `/notebooks` 身份或 URL。路由中的 ID 是导航选择，服务端仍以认证与实体归属核验。

| 对外 URL（含 basepath） | 最终行为与兼容策略 |
| --- | --- |
| `/app/` | 新学习区卡片墙，替换旧首页的大目标输入区；目标输入移到区内。 |
| `/app/spaces/:spaceId` | 新三栏容器；建议 query `goal`、`panel=classroom\|sources\|studio`、`source`、`source_version`、`artifact`、`artifact_version` 定位子视图；UUID／版本／枚举严格校验。正文、问题与选择全集不写 URL。 |
| `/app/spaces/:spaceId/learn/:sessionId` | 原地址直接渲染同一个三栏壳及原课堂；session 决定真实 goal，冲突参数报错，不覆盖或重开。面板定位 query 可沿用上行。 |
| `/app/spaces/:spaceId/manage`（拟新增） | 原 `SpacePage` 管理能力承载页，供完整目标列表、生命周期和区设置使用；工作区菜单和目标选择可到达。旧 `/spaces/:id` 仍提供这些操作入口。 |
| `/app/spaces/:spaceId/goals/:goalId` | 保留原目标详情／编辑／历史页；增加“进入课堂工作区”入口，原管理用例无需全部改成三栏测试。 |
| `/app/spaces/:spaceId/goals/:goalId/research?start=…` | 保留现有研究／开课恢复页；新添加来源与它共享能力，但不以 redirect 替代其运行恢复语义。 |
| `/app/spaces/:spaceId/studio` | 保留原“课堂内容库”能力与筛选／收藏／指定版本；新增明显入口回到工作区 Studio。不能把旧 reading／exercise 列表直接替换成四种新产物而丢失原数据。 |
| `/app/content/:artifactId?space=…&version=…` 与 `/content/:artifactId` | 继续原课堂内容详情及服务器现有跳转，保留空间和版本；新独立 Studio 产物使用工作区 query，避免冒用旧 artifact 合同。 |
| `/app/progress`、`/app/spaces/:spaceId/feedback[/:attemptId]` | 原进度、复习、评估页面及筛选不变，菜单及中栏可到达。 |
| `/app/spaces/:spaceId/chat[/:conversationId]` | 原导师历史保留；新问答保存后仍有明确会话归属。 |
| `/app/spaces/:spaceId/knowledge?goal=…&session=…` | 保留知识结构、维护、资料集合管理、参考预览确认；同时承担“设置课堂资料”高级入口。 |
| `/app/runs`、`/app/runs/import/:jobId`、`/app/runs/run/:runId` | 保留任务过滤和 `space`／`collection`／`draft` 上下文。 |
| `/app/spaces/:spaceId/notesync[/:reviewId]` | 保留 `collection`／`operation` 和原审阅。NoteSync 当前映射默认区／默认集合，别的区明确提示不支持，不把默认区数据伪装成当前区。 |
| `/app/memory`、`/app/settings`、`/app/settings/data`、`/app/settings/devices` | 保留参数、权限和完整业务页面，从菜单进入；长期记忆仍是既有全局语义。 |
| `/app/offline`（以及 RootLayout 现有 `/offline` 分支） | 保留独立离线 shell，不套在线 SessionProvider；不扩大离线同步范围到新 Studio。 |

除服务器已有 `/app`、`/content` 规范化跳转外，首版不新增大范围 redirect。菜单在区内使用当前区；首页点击依赖区的功能时显示区选择，不能偷偷回落默认区。只有原有明确以默认区为语义的入口保留该语义并标示。

现有 `server/internal/transport/httpapi/web.go` 的 SPA 分发已接受 `spaces/` 下无点路径，新增 manage 不需扩大服务器 catch-all；需补合法直达／刷新测试，并继续拒绝错误 API、静态资产和非学习路径。无效 ID／版本／跨区组合显示错误，不扩大查询范围。切换面板用 replace 更新可恢复定位，选择目标／课堂及打开产物保留有意义的历史；引用返回记录触发点与原面板。

## 4. 原型区域到现有前端的复用映射

`clients/web/src` 当前是 React 19、TanStack Router／Query、React Hook Form／Zod、OpenAPI fetch、Tailwind 4；主要样式集中在 `styles.css`，`components/ui/button.tsx` 等是少量 shadcn 风格组件。不需要更换框架或新增 UI 库。

| 原型区域 | 可复用页面／组件 | 可复用 API 封装与真实接口 | 需要新建／抽取 |
| --- | --- | --- | --- |
| 顶栏、名称、返回首页、头像／菜单 | `components/workspace-shell.tsx`、`SpaceSwitcher`、`lib/session.tsx`、`common.tsx`、`ui/button.tsx` | `api/client.ts`、`runtime.ts`；`/v1/web/session`、`/v1/learning-spaces/{spaceID}` | `notebook-header.tsx`、`navigation-drawer.tsx`；头像是设备会话入口，不能虚构 Google 账号。保留主题与退出。 |
| 首页卡片墙、新建、搜索 | `pages.tsx` 的 `HomePage`、`SpaceEditor`、分页与生命周期表单 | GET／POST `/v1/learning-spaces`、PUT `/{spaceID}` | `notebook-home-page.tsx`、`notebook-card.tsx`；抽取 SpaceEditor，保留版本冲突流程。 |
| 三栏容器 | `WorkspaceShell`、`TeachingWorkspace` 原布局／移动标签的经验 | 不新增领域 API | `notebook-workspace-page.tsx`、`notebook-layout.tsx`、`mobile-panel-tabs.tsx`；避免把旧三栏教学页整体嵌套成九栏。 |
| 来源列表／全选 | `knowledge-page.tsx` 的集合、tree、冻结范围读取 | `api/references.ts`；collections、revisions tree／export、scopes tree／export | `sources-panel.tsx`、`source-list.tsx`、`use-notebook-sources.ts`；按文档与版本规范化列表，选择独立于 ReferenceSelection。 |
| 添加 PDF／文本／网址 | `components/reference-import.tsx`、`pdf-source.tsx`、`import-review.tsx`、`import-page.tsx` | `api/references.ts`、`pdf.ts`、`import-jobs.ts`；reference-sources、imports previews／confirm／operations、import-jobs | `add-source-dialog.tsx`，抽取同一个导入控制器供旧页和对话框共用；不是在 modal 复制一套 submit 逻辑。大批次转原可恢复导入页。 |
| 搜索发现 | `research-page.tsx` 的来源状态、run 恢复与授权文案；`settings-page.tsx` | `api/research.ts`、`mentor.ts`、`settings.ts`；目标研究 runs／sources／decisions | `source-discovery.tsx`；现有研究会先读全部候选，不能原样充当“选中后读取”的搜索列表，需 G2。 |
| 左栏原文与引用定位 | `content-blocks.tsx` 的 `SourceViewer`／`SafeMarkdown`、`pdf-source.tsx` 的 `PDFCoverage`／`PDFPageViewer`；研究页 SourceViewer | `api/content.ts` citation、`pdf.ts`、`research.ts`；content sources、run source detail、PDF pages | `source-viewer.tsx` 与统一导航回调；提取现有对话框内的展示部分，旧页仍可用对话框。 |
| 目标条、阶段、路线进度 | `GoalComposer`、`pages.tsx` 的 GoalPage／生命周期、`SessionPicker`、`progress-page.tsx` 的 ProgressPanel | `api/runtime.ts`、`start.ts`、`progress.ts`、`teaching.ts`；goals、sessions、start、progress | `classroom-panel.tsx`、`goal-selector.tsx`；阶段取服务端状态，进度环只展示有分母的路线完成量。 |
| 讲解／活动／答案／反馈 | `teaching-page.tsx` 的 TeachingWorkspace、ContentPage；`content-blocks.tsx`、`change-panel.tsx`、`feedback-page.tsx` | `api/teaching.ts`、`feedback.ts`、`changes.ts`；sessions actions／operations、content answers、评估接口 | 抽取 `teaching-activity.tsx`、`answer-composer.tsx` 等内嵌片段，保留控制器／回执核对及当前活动绑定，不重写评分。 |
| 底部导师输入／快捷追问 | `mentor-panel.tsx`、`tutor-history.tsx`、TeachingWorkspace 的 free question 与帮助控件；`content-editor.tsx` | `api/mentor.ts`、`conversations.ts`；goal runs、conversations turns、run events／commands | `mentor-composer.tsx`，显式区分“课堂讨论／调整”和“基于所选来源追问”；逐步统一呈现，不混用两种后端动作。来源追问需 G3。 |
| Studio 四入口、结果列表 | `studio-page.tsx` 的库查询／加载错误／筛选经验；`content-tools.tsx` | `api/content.ts` 现有 library 仅 reading／exercise，不能直接满足新入口 | `studio-panel.tsx`、`studio-artifact-viewer.tsx`、新 `api/studio.ts`；G4–G6。 |
| 学习指南／简报查看 | `SafeMarkdown`、可复用的正文块／引用呈现 | 现有 content sources 可服务旧活动；新产物要新增引用读取合同 | `studio-document.tsx`；独立产物数据模型与生成运行。 |
| 测验作答 | 正式答案、帮助、评估及回执组件 | 现有 tutoring proposal／actions 与 content answers 是评分底座，不是任意题库上传接口 | `studio-quiz.tsx`，只关联服务端创建的正式活动；G5。 |
| 笔记编辑／保存／转来源 | `DraftStore`、表单、导入预览控件 | 没有独立笔记 CRUD；转来源复用 knowledge imports | `studio-note-editor.tsx`；G4、G6，不借用 memory 或正文选段改写 API。 |
| 菜单原页面 | `progress-page.tsx`、`feedback-page.tsx`、`tasks-page.tsx`、`knowledge-page.tsx`、`notesync-page.tsx`、`offline-page.tsx`、`settings-page.tsx`、`memory-page.tsx`、`data-page.tsx`、`tutor-history.tsx` | 各自原 `api/*` | 主要改导航入口与返回工作区链接，功能保留；`knowledge-structure.tsx`／`knowledge-maintenance.tsx` 不重写。 |

以上新文件建议放在 `clients/web/src/components/notebook/`，页面放 `src/`，状态 hook／纯转换放 `src/lib/`，请求放 `src/api/`；执行者可合并很小的纯展示组件，不得把页面控制器再次堆成巨文件。每个 child 交接稳定 props／DTO 后，后继再消费。

### 4.1 工作区内部接口边界

- 工作区控制器只拥有当前区、目标／会话选择、面板、来源选择与引用定位；正式答案控制器仍拥有活动版本、实际帮助、pending operation 及核对回执。
- `SourceSelection` 建议为确切 collection／revision／document（可含 node）条目集合；不能只传数组下标、标题或 URL。查找与引用还区分 stable document ID 和 document revision ID。
- `CitationTarget` 为有类型的 union：旧课堂 artifact/version/reference、研究 run/source/revision/fragment、新导师 run/citation、新 Studio artifact/version/citation；同一来源查看器按类型请求 owner，不互相猜测 ID。
- 来源查看事件只切面板／焦点，不更改勾选。选中列表和运行输入按身份、区、选择指纹隔离，权威正文按版本缓存；列表成功不代表所有来源正文都成功读取。
- 旧 TeachingWorkspace 已有知识、学习、导师三列，必须抽取主体并保留原动作。原 `ask_free_question` 会推动教学状态；新按来源对话复用 `mentorrun`，不能将聊天按钮误接成正式教学 action 或改变原帮助等级。

## 5. 后端能力映射与缺口

判断依据是当前代码和 OpenAPI，不以历史设计文档里“计划实现”当作现状。下表中“足够”指可复用真实路径；“小幅新增”仍必须完整跨越认证、领域、transport 与测试；“较大新增”不是前端换文案可完成。

| 能力 | 现状证据与可用 API | 判断 | 最小方案／缺口 |
| --- | --- | --- | --- |
| 学习区创建／改名／归档／列表 | `learningspace/postgresstore/store.go`；`/v1/learning-spaces`、`/{spaceID}`；有 version、operation、搜索和分页 | **现有 API 足够** | 复用；卡片扩展统计／最近学习排序见 G0。 |
| 集合与区内来源列表 | `knowledge/scopes.go`、`postgresstore/scopes.go`；collections、revision tree／export、scope snapshot | **基本列表足够；丰富元信息需小幅新增** | B1 按集合懒加载文档名称；完整统一来源状态／网址出处见 G1。 |
| 文本／PDF 导入与任务恢复 | `reference_sources.go`、`import_preview.go`、`import_jobs.go`；source-capabilities、reference-sources、imports preview／confirm／operations、import-jobs | **现有 API 足够** | 保留原解析／预览／批准及未知结果恢复。PDF 当前 4 MiB、100 页、16000 字节提取文本，无 OCR，能力响应为准。 |
| URL 抓取 | `research/fetch.go` 与 `httpapi/reference_sources.go`；POST reference-sources | **获取足够；持久元信息需小幅新增** | 现有通用 URL 导入把地址／覆盖写入 Markdown 文本，不能当可靠结构字段，见 G1。 |
| Brave 搜索与目标研究 | `integrations/websearch`、`settings/search.go`、`mentorrun/research.go`；goal research／runs／sources／decisions | **目标研究足够；原型搜索需小幅新增** | 已可搜、抓、综合、采纳，最多 4 个来源；但会抓取所有候选且绑定目标。原型先选候选再读取及 D4 无目标搜索需 G2。 |
| 导师对话与历史 | `mentorrun/conversation.go`、`core.go`、`references.go`；conversations、turns、runs、SSE | **普通对话足够；按勾选对话需小幅新增、较高风险** | 独立会话支持 goal 可选；无目标时现有 core 去掉 read_references，目标内则读取正式 reference policy。Create／SubmitTurn 没有本轮来源选择；普通 Snapshot.output 是字符串，无通用核验引用列表。见 G3。 |
| 引用／片段／PDF 定位 | `learningcontent/sources.go`、`knowledge/postgresstore/content.go`；content sources、research source detail、PDF pages | **课堂／研究足够；导师及新产物需小幅新增** | 复用字节范围／hash／历史版本核验，新引用随 G3、G4；不能解析模型文本里的“[1]”就视为真引用。 |
| 学习指南 | `learningcontent/library.go` 只分 reading／exercise；`domain.go` 的 Revision 强依赖 goal/session/activity | **需要较大新增** | 无独立学习指南类型、生成请求或持久结果；复用正文 renderer 和引用底座，见 G4。 |
| 简报 | research 有 Synthesis(points/gaps/examples)，属于研究 run，并非任意勾选来源的独立简报 | **需要较大新增** | 可复用综合及引用验证思想，不把 research 输出直接冒充简报，见 G4。 |
| 笔记 | content_edit 是既有课堂 artifact 的局部加工；memory 是全局长期记忆，不是笔记 | **需要较大新增（共享产物底座后增量较小）** | 新笔记 CRUD／版本、区内列表／读取及隐私语义，见 G4、G6。 |
| 测验 | 已有正式 Activity／Attempt／Assessment／Evidence、content answers、free-answer 转测验；没有从任意多来源生成可保存的独立多题测验 | **需要较大新增编排；评分底座足够** | 生成经验证的题目集并建立正式承载／活动，逐题走现有领域，见 G5。不能把 JSON 题目直接提交给 answers。 |
| 课堂／评估／复习 | tutoring sessions／proposals／actions／operations，learning feedback／progress／reviews；`teaching-page.tsx` 与 `progress-page.tsx` 已消费 | **现有 API 足够** | UI 重组，保留冻结、正式答案、帮助等级、复核及复习承载；新测验桥接单列。 |
| 知识检索 | POST `/v1/knowledge/retrievals` 支持 revision 或 scope_snapshot；`knowledge/retrieval.go` 有引用片段、hash、截断／降级 | **检索底座足够；运行范围适配需小幅新增** | 绑定 G3 的范围，所有模型工具读取贯彻同一选择；不能省略 scope 回落 collection head。 |

### 5.1 缺口清单与最小合同方向

下面的接口／表名均是**拟新增设计**，不是当前可调用能力。相关 child 开始前需锁定 OpenAPI 细节、状态及错误合同；本 Shape 不修改它们。新字段采用可选扩展，旧调用缺省保持原语义；新 Web 使用能力协商，旧服务不支持时明确禁用，不回落成不受限生成。

| 编号 | 现状与最小方案 | OpenAPI／migration | 风险与归属 |
| --- | --- | --- | --- |
| G0（增强，四批外） | 首页没有真实汇总／最近学习排序。最小必需界面先用已有元数据；若另行要求原型计数和最近排序，增加区分页汇总与服务端稳定排序，按权限计算 distinct 文档、目标和学习事件时间。 | 当前方案均否；增强时 OpenAPI 是，SQL migration 通常不必，仅经性能证据再加索引。 | 不能用当前页条数冒充总数、updated_at 冒充最近学习，不能客户端跨页排序冒充全局顺序。 |
| G1 | 通用导入缺结构化 URL／搜索 provenance，研究元数据只在 `knowledge_source_revisions`，PDF 有独立 metadata。增加文档版本级 `source_metadata`（建议独立 sidecar），保存来源类型、实际 URL、解析器、覆盖、指纹、可选派生版本；服务端抓取回执绑定正文后才能写入。列表新增受区／集合权限限制的分页 `/v1/knowledge/sources` 与详情；B1 的 tree 列表随后切换到此合同。旧资料未知字段保持未知，不无损回填承诺。 | **OpenAPI 是；migration 是**，新增 metadata／索引和 knowledge owner 清除验证；正文仍在原文档 payload，不复制。 | 中：元信息不能信任用户伪造的标题或 Markdown 前缀；共享集合、同名文档与被解除关联必须正确隔离。 |
| G2 | 新增有界 `/v1/knowledge/source-discovery` 搜索候选请求，仅接受用户确认的公开关键词／域策略，复用 settings.SearchClient 与 Brave adapter，首版沿用每次最多 4 个候选；候选不落正式库。勾选后调用已有 reference-sources 获取正文，接入 G1 与原导入预览确认。旧目标研究运行完整保留。 | **OpenAPI 是；独立 migration 否**，正式出处使用 G1；候选仅本标签页，不增加持久搜索任务。 | 中：沿用研究授权、限流、请求预算和网络校验；搜索按钮本身不能向外发送整个私人目标。取消／失败需准确，不能宣称零外部费用。 |
| G3 | 为导师 Create 和 conversation SubmitTurn 增可选 `source_selection`（明确“无来源”或确切条目），请求被受理时固化经 owner 核验的版本清单／hash 至加密 run body；后续读取、重试、历史和模型工具共用此范围。返回结构化已核验 citations 并新增 run citation 读取；不改 goal/session reference heads。 | **OpenAPI 是；预计 SQL migration 否**，复用 JSON 加密 checkpoint 与既有历史存储；需正文 schema 兼容／密钥轮换测试。若发现必须变更索引／存储合同，child 更新设计再实施。 | 高：空／缺省区分、旧历史泄漏、跨区越权、清除竞态和伪造引用。D1、D4 为前置。 |
| G4 | 学习指南／简报／笔记共享独立 Studio artifact／revision／operation 存储，仍由 learningcontent 领域及 learning 隐私 owner 管理，goal 可空，保留 source manifest、引用、模型／输入指纹、加密正文与版本。拟 `/v1/learning/studio/artifacts` 列表／创建、`/{id}` 读取／笔记保存、`/{id}/revisions`、`/{id}/sources/{citationID}`、`/operations/{id}`。生成复用 mentorrun 预算／租约／取消／加密运行，增加受限 Studio 任务类型和产物发布路径。 | **OpenAPI 是；migration 是**，新产物／版本／幂等关联及 run kind 约束扩展；不去掉旧 learning_content_artifacts 的 session/activity NOT NULL 和唯一性。 | 高：三种产物并非已有类型别名；新正文／标题／来源关系须加密、清除、撤权、重启恢复、版本冲突均完整。D4 决定无目标可用性。 |
| G5 | 多题测验是 G4 的 quiz 产物加 learning/tutoring 编排。冻结选择，生成并校验每题 prompt、rubric、帮助与真实来源；明确开始时在选定目标创建专用正式测验承载，建立题目到活动映射。逐题发出与提交，结果沿现有 Attempt／Assessment／Evidence 路径读回。 | **OpenAPI 是；migration 是**，题目／正式活动映射并入 G4 的新增迁移；不另建评分与进度事实。 | 高：当前 answers 必须对应真实活动／会话／版本，原 free-answer 转测验不等于通用多题测验。D2 为前置；不能覆盖当前课堂或减弱 allowed_help。 |
| G6 | 笔记“转为来源”使用指定已保存版本，服务端读取原笔记并形成导入候选／原文 hash，再走原 knowledge preview／confirm；G1 记录派生自笔记 ID／版本。操作回执绑定该版本；再次转换用新版本重新预览。 | **OpenAPI 是**（笔记版本到导入候选／回执）；**无额外独立 migration**，复用 G1 与 G4 的 provenance／操作表。 | 中高：跨 owner 读／写、隐私清除与引用环；转换是快照复制，无实时同步，不把笔记标作外部权威。D3 为前置。 |

### 5.2 需要实施者特别遵守的边界

**来源范围。** 现有 `POST /v1/knowledge/scopes` 要求 `knowledge:write`；不能为普通只读来源问答悄悄授予这个权限。G3 最小路径在 run 自己的加密正文保存经过 knowledge 只读校验的范围清单，调用 owner 的有界读取／检索适配；不修改已有 scopes API 权限。需要教学持久范围时仍走原有获准流程。无目标会话当前明确禁用 read_references，新增来源工具只能读新清单，不临时伪造 goal ID。

**历史与引用。** 新请求不得把历史中未选资料的正文、工具结果或旧回答作为新来源上下文。选择指纹变化时在 UI 明示新资料上下文，并隔离不兼容历史（可新建对话分支）；旧历史仍可单独查看。原目标、成绩和长期记忆工具如保留，不能成为绕过来源范围读取资料正文的通道；长期记忆的既有授权另行遵守。模型输出的 citation ID 必须匹配本次实际交付片段的版本、字节范围／hash；未经验证的文本不能获得可点击“依据”徽标。

**正式测验。** 测验支持逐题作答和逐题实际帮助，不承诺原型静态单按钮能一次提交任意题集。新编排必须验证目标、资料和知识点映射；资料足以做摘要却不能映射到可评估活动时，明确“无法建立正式测验”并保留其他产物。不能伪造节点、rubric、Evidence；重新作答要创建明确的新正式尝试，原记录不覆盖。独立测验会话用于避免替换正在进行的课堂，开始／恢复后仍以已有正式会话 URL 承载，中栏显示测验来源入口。

**笔记与清除。** 保存笔记不需要模型；只有用户明确选择生成才消耗模型预算。转为来源是用户确认的独立副本，编辑笔记不会更新副本。全局清除须覆盖 learning 与 knowledge 两边及派生映射，不能因可重放操作或生成 worker 在途而复活。普通清除导师运行不删除此前明确保存的 Studio 产物／正式来源，界面说明这种区别；现有 owner 屏障仍决定最终可读性。

**权限与网络。** 配对档案不能因 UI 一体化被合并升级：普通 learning 读写、references／imports、research、assessment、settings、memory 等 scope 继续单独核验。研究 profile 具有 `research:adopt` 并不等于拥有通用集合审批；references profile 才能做相应参考导入。新端点应明确要求既有最小权限并在能力响应表达，不能用换路由绕过 `references:manage`／`knowledge:approve`。G2 发现候选复用 research 授权，后续通用 URL 导入仍遵守现有导入权限；不足时引导合法重新配对。

**安全呈现与数据恢复。** 复用 `learningClient` 的同源 Cookie、CSRF／principal／generation 头及 `unwrap` 运行时验证；新来源和 Studio 同样 no-store。`SafeMarkdown` 禁原始 HTML、外部图自动加载及不安全链接，PDF 继续服务端 PNG。所有 mutations 保持不自动重试；unknown 先查原 operation，409 保留草稿并读新版本，401／403／清除屏障隐藏旧正文。草稿键包括身份、区、目标／会话、活动或产物版本，移动端面板隐藏不能产生新的运行连接或丢失 draft。

## 6. 视觉与移动端规范

视觉以浅灰蓝画布、白色圆角面板、低密度顶栏、彩色学习区卡片和 Studio 方块为主。保留产品“知络／edu-agent”身份，不加载 Google 字体网络资源或复制 Google 图标。

| 项 | 实施基准 |
| --- | --- |
| 浅色令牌（来自原型） | 背景 `#f0f4f9`；面板 `#ffffff`；分隔线 `#dde3ea`；正文 `#1f1f1f`；次要正文 `#5f6368`；浅层背景 `#e9eef6`；主色 `#0b57d0`；主色浅底 `#d3e3fd`。 |
| 状态 | 成功正文／底 `#146c2e`／`#c4eed0`；部分／警示 `#8a5a00`／`#ffe8b3`；引用高亮 `#fde293`。必须加文字／图标，真实渲染后检查对比度。 |
| 卡片／Studio | 学习区底色 `#e6f4ea`、`#e8f0fe`、`#fef7e0`、`#fce8e6`；Studio 笔记 `#f3e8fd`。颜色可由 ID 稳定映射，不新建颜色存储配置。 |
| 圆角与阴影 | 面板／卡片 16px，Studio tile 14px，行／输入 12px，modal 24px，pill 999px；阴影沿原型的轻微 1–3px 黑色透明层，hover 适度加强。 |
| 字体与字号 | 本机 `Noto Sans SC`／`PingFang SC`／`Microsoft YaHei`／system-ui；不要求安装专有字体。控件 14px／1.6，阅读正文约 16–17px／1.7–1.8；面板标题 16px，区名 20px，首页标题桌面 44px、手机 32px；元信息 12–13px。 |
| 尺寸／间距 | 顶栏 64px，面板头 56px；外边距／栏间距 16px，正文 padding 16–24px；间距基数 4／8／12／16／24／32。卡片墙 max-width 1200px，卡片 min-width 230px、gap 16px、padding 20px。 |
| 桌面列 | 原型 `minmax(260px,22%) minmax(0,1fr) minmax(300px,26%)` 为宽屏参考；中栏活动可读宽度优先，不能复用旧 `minmax(520px,1fr)` 造成新壳横溢出。支持收起侧栏，按钮有可见名称与展开状态。 |
| 深色 | 继承现有主题开关和语义 CSS 变量，映射新卡片／panel／状态；原型未给深色值，不能直接把浅色字／底照搬。后续实际对比度验证为退出标准。 |

原型在 900px 以下转底部标签；建议生产在 **1100px 以下**采用单面板标签以保证三栏总最小宽度，900px 以下严格贴近原型手机布局。这是可用性优化，不新增第四种信息架构。验证 390、768、1024、1280、1440px，尤其检查阈值附近。

底栏高度 64px 加 safe-area，内容用 `100dvh` 与可滚动面板，底部预留输入／软键盘空间。小屏输入字号至少 16px，主要触控区域至少 44px。来源列表与详情在同一标签内切换，可返回列表；点引用先切来源并聚焦位置，关闭恢复触发引用的课堂／Studio。桌面来源详情可占左栏下部或展开 drawer，不能将中栏答案挤出可操作范围。

菜单／modal 限制焦点、Esc 关闭、关闭返回触发控件；危险批准默认取消；移动标签有 tablist／tab／tabpanel、aria-selected 和键盘导航。隐藏面板没有可聚焦子元素，草稿与运行订阅由外层状态维持。消息更新使用节制的 live region；用户读旧消息时不强制滚到底部；动画遵守 reduced motion。`styles.css` 通过 notebook 根类限定新增布局，避免全局 `.panel`／`.main` 样式破坏旧页与离线 shell。

## 7. 测试迁移与发布证据

### 7.1 浏览器用例影响清单

当前 `clients/web/tests/browser/` 有 14 个 `.spec.ts`。多数配对 helper 断言旧首页“今天想学会什么？”，部分从首页填目标；全局导航改为 drawer 后即使旧路由保留，这些步骤也必须改。不要简单全局替换文案，需重新表达“配对 → 选区 → 目标／业务入口”。

| 文件 | 影响／迁移方式 |
| --- | --- |
| `learning.spec.ts` | 高：首页目标创建改区内、卡片墙／区管理入口、主题／退出进菜单；保留生命周期、IME、双标签隔离、分页、409 与重启断言。完整管理场景可转 `/spaces/:id/manage`，目标深链接保留。 |
| `workspace.spec.ts` | 高：旧教学三列和导师栏结构改动、移动 tab／宽度检查、部分 Studio 入口变化；按命名 region 定位活动／答案／导师／来源；保留正式答案、帮助规则、unknown、内容版本、评估、变更及恢复语义。旧 `/learn` 和 `/content` 仍是核心回归。 |
| `mentor.spec.ts`、`tutor-history.spec.ts` | 中高：设置／任务／历史入口与首页目标建立变动；原会话、多轮、预算、SSE、目的地、临时保存／清除断言可沿旧路由保留。另加 G3 选择变化与引用隔离。 |
| `references.spec.ts`、`pdf.spec.ts` | 中：旧 knowledge 页用例大体保留，更新配对入口；新增工作区 modal、来源查看、历史 PDF 页和跨标签返回场景，不删除原导入审批／缺口／权限断言。 |
| `review.spec.ts` | 中：配对后的首页标题、返回课堂与菜单入口受影响；原 progress／learn 深链接保留，跨区三目标、复习承载和 CLI 读回继续测试。 |
| `knowledge-structure.spec.ts` | 中：配对 helper、课堂去维护再返回的入口变化；知识列表／图／维护页保留，原题草稿／清除断言保留。 |
| `tasks.spec.ts` | 低至中：任务入口改 drawer；大批导入 URL 与任务详情保留，原来源变化／重启／取消／设备隔离断言保留。 |
| `memory.spec.ts` | 中：配对标题、首页目标入口和导师内嵌候选可能变化；原 memory／data／devices 路由保留，不能弱化批准、远端回执和清除。 |
| `notesync.spec.ts`、`settings.spec.ts` | 低至中：菜单／配对步骤更新；原页面、默认映射／授权／密钥不回显／显式探测保留。 |
| `offline.spec.ts` | 低：入口变化；独立离线 shell／夹具保持，在线新草稿不能被迁入离线库。继续既有 Chromium 完整离线与其他引擎能力降级。 |
| `release.spec.ts` | 扩展：首页、三栏、manage 和 query 直达／刷新；保留 Cookie 配对、真实 `/app/assets`、no-store、错误 API／资源边界。 |

新增建议用例 `notebook-workspace.spec.ts`（壳层、D1 范围与恢复）、`notebook-sources.spec.ts`（四种来源和引用）、`notebook-studio.spec.ts`（四种产物、D2／D3、隐私）。第一批即抽公共配对／选区 helper，可放 `tests/browser/notebook-fixture.ts`，无需使业务测试依赖完整的 UI 创建流程；需要验证创建本身的场景仍走真实 UI。

定位优先可访问名称和稳定 region；重复的“下一页”“保存”等需作用域限定。仅在没有合适语义时使用 testid，不拿它代替可访问性。每批同步修好自己破坏的用例；第四批做完整迁移收尾，不允许前三批长期留红。模型 fixture 应校验调用收到的真实范围／引用和正式领域记录，不只断言出现某段自由文本。

### 7.2 检查分层与命令

在已有锁定依赖和合法测试环境可用时，最小前端检查为 `cd clients/web && npm run check`、`npm test -- <受影响文件>`；构建用 `npm run build`。`make web-check` 会跑全部前端类型／单测，可在批次交接使用。**不要为了构建调用 `make web-build` 自动安装依赖**，该 target 含 `npm ci`；缺依赖时按项目授权规则处理。

浏览器局部命令示例（在 `clients/web` 执行）：`WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 npm run test:browser -- notebook-workspace.spec.ts --project=chromium`。需要独立 `TEST_DATABASE_URL`、fixture 所需构建／模型端点等，环境由原 test-server／webdatabase 机制建立；没有数据库或浏览器即记录未验证，不能把 skip 算通过。新来源／Studio 用例须提供确定性搜索／模型 fixture，新端点不依赖真实 Brave 服务才能证明合同。

来源网络夹具不能只在 localhost 起一个站点后修改生产允许列表。已有 `research.NewFetcherWithNetwork` 和 HTTP options 的 `ReferenceFetcher` 可供受信宿主注入；建议新增仅测试使用的 `server/testutil/webnotebook/main.go`，组合真实 HTTP handler、PostgreSQL owner 与生产前端资产，为来源浏览器用例注入固定搜索响应及 DNS／dial 测试依赖。地址／重定向校验仍执行，生产二进制不注册放宽私网的环境开关。`test-server.mjs` 对该文件选择测试宿主，候选脚本构建它并标明此证据范围；`release.spec.ts` 与安全 HTTP 契约仍使用普通发行二进制验证真实拒绝边界。仅拦截浏览器响应可辅助交互测试，不能冒充完整来源导入证据。

后端只跑受影响 package 的领域／transport／OpenAPI 检查，涉及持久化、跨 owner、并发与清除必须在本批完成相关 PostgreSQL 集成测试；这些关键安全证据不能全部推迟到 Candidate。数据库测试选择新用例和现有相关路径，不默认全仓扫。新增 migration 验证空库与现有基线升级；public DTO 增补后运行生成一致性，review `schema.d.ts`，旧 CLI 请求保持兼容。

### 7.3 `scripts/check-web-release.mjs` 的实际影响

- 无参数是**局部发行检查**：检查锁定依赖、OpenAPI 生成一致性、`make web-check`、指定 Go 非数据库契约、vet、真实前端与 `web_release` Go 构建、缺资产必须失败。它不跑浏览器全矩阵，不能称作完整验收。
- `--candidate` 要求独立 `TEST_DATABASE_URL`，构建 webdatabase 和生产 CLI，运行真实 PostgreSQL／空库研究／CLI 同一教学／竞态／SSE 指定用例；随后按每个浏览器、每个 `.spec.ts` 独立 schema／进程执行 Chromium、Firefox、WebKit。新增 spec 会自动被枚举，不能在普通自动枚举文件中留下条件 skip。
- `scripts/web-release-results.mjs` 拒绝无用例、skip、重试、预期失败等伪通过；offline 的非 Chromium 完整流程按 Playwright 原有 grepInvert 排除，保留其能力降级验收，不能借此排除新工作区用例。
- 现脚本只对 `learning.spec.ts`／`settings.spec.ts` 关闭 workspace／mentor fixture，其余打开；NoteSync／Nocturne 也按文件名决定。新增三份 notebook spec 会获得 workspace／mentor fixture，但 **Brave discovery／Studio 回答场景仍须扩充 `clients/web/scripts/test-server.mjs`、`workspace-model.mjs` 的 fixture，并接入上述来源测试宿主**。若拆分／重命名文件，需要同步这些名字判定；优先保留旧文件名以降低门禁漂移。
- 当前 Go candidate 只选 `httpapi`／`mentorrun` 的特定 `TestPostgreSQL…` 前缀，不会自动覆盖新增 learningcontent／knowledge 测试。批次 2／3 交接就要记录局部 DB 证据；批次 4 将 G1–G6 关键 DB 场景加入候选步骤和必需用例校验，不能仅新增测试文件就宣称门禁覆盖。
- 证据含 commit、源码输入 hash、环境、lock hash、逐步日志 hash，运行期间输入变动会失效。共享样式／路由更新会使早先浏览器完整矩阵不能当作最终候选证据。只复用输入和环境匹配的局部结果，最终完整矩阵在候选稳定后跑；外部服务与人工验收继续按 `docs/development/issue-37-acceptance.md` 区分。
- “不动 CLI”指不改 CLI 产品代码／交互，不删当前候选里 CLI 兼容性构建和读回检查。Shape 本次不执行上述产品测试，不产生运行通过结论。

## 8. 分批实施计划（供后续智能体接手）

目标：交付父 spec 的完整 Notebook 工作区。架构：新 Web 壳层复用原领域，独立来源选择与 Studio 补充由服务端验证。技术栈沿用 React／TypeScript／TanStack／Tailwind 4／Go／PostgreSQL。执行者先读 brief、spec、本设计与[开发工作流](../development/development-workflow.md)，每次只拥有一个 in-progress 批次；不以本计划代替用户的 Shape 确认。无需安装新 UI 依赖，也不要求使用不存在的执行技能。

统一约束：不改 CLI；不放松配对／scope／CSRF／Origin；草稿只在标签页；正式答案／帮助规则不变；范围冻结、隐私清除、版本／幂等为各批共同退出条件。下列文件是预期允许范围，新增 migration 文件名在 Build 开始时按最新序号分配，不改历史 migration。工时为一名熟悉项目的实施者人日，含局部验证与审阅修复，不含等待确认／外部环境／未授权安装。

依赖：**批次 1 → 批次 2 → 批次 3 → 批次 4**。四批均为垂直结果；若 D1–D4 尚未确认，相关接口不得由后续实施者凭默认值直接发布。

### 批次 1：真实卡片墙、三栏课堂与菜单（5–7 人日）

**结果：** 用户从卡片墙进入真实学习区，查看既有来源，在中栏建立／恢复目标课堂并提交答案，从左上菜单进入原页面。

**范围：** 父验收 1–4、13–15，及本批涉及的 22–28。新壳、卡片墙、基本来源 tree 列表、可嵌入课堂和导航。Studio 右栏可进入原课堂内容库；四个新产物入口标明尚未交付，不假生成。左栏来源选择尚未接入新语义时不提供看似生效的勾选控件，标明当前仅浏览／课堂冻结范围。

**实际涉及文件：** 以下路径以 `clients/web/` 为根；文档修改为本文件。

| 分组 | 文件 |
| --- | --- |
| 新增页面／读取 hook | `src/notebook-home-page.tsx`、`src/notebook-workspace-page.tsx`、`src/lib/use-notebook-sources.ts` |
| 新增组件 | `src/components/notebook/{notebook-header,navigation-drawer,notebook-card,notebook-layout,classroom-panel,goal-selector,sources-panel,studio-panel,mobile-panel-tabs}.tsx` |
| 修改页面／壳／样式 | `src/main.tsx`、`src/pages.tsx`、`src/teaching-page.tsx`、`src/components/workspace-shell.tsx`、`src/styles.css` |
| 新增浏览器测试／helper | `tests/browser/notebook-workspace.spec.ts`、`tests/browser/notebook-fixture.ts` |
| 修改旧浏览器测试 | `tests/browser/{learning,workspace,release,mentor,tutor-history,references,pdf,knowledge-structure,tasks,memory,notesync,settings,review}.spec.ts`、`tests/browser/offline-fixture.ts`；`offline.spec.ts` 本身未修改 |

**执行步骤：**

- [x] 先用一个真实场景明确失败：配对 → 新建区 → 卡片进入 → 恢复会话 → 正式答案／反馈；保留旧深链接断言。
- [x] 接入卡片墙与空间管理入口，完成菜单／旧路由兼容，再抽取课堂主体嵌入；来源列表用现有 collections／tree，按集合懒加载，不读取全库正文来填卡片。
- [x] 处理归档、缺能力、空数据、跨区／迟到响应及基本窄屏标签；同步迁移破坏的旧用例。
- [x] 交接 `WorkspaceContext`、`SourceSelection`／`CitationTarget` 的前端边界与不支持状态，并更新设计中已落地的文件名。

**本次执行计划（2026-10-12）：** 已获批次 1 实施授权，在现有 worktree 串行实施，不暂存／提交，不安装依赖。

1. 新增 `tests/browser/notebook-fixture.ts` 提供真实 Cookie 配对、菜单操作和有 CSRF 的夹具 API；`notebook-workspace.spec.ts` 先断言首页“你的学习区”、新建后进入三栏、建立并恢复课堂及正式反馈，再运行 Chromium 确认因新界面缺失失败。夹具只准备真实来源，不拦截教学／评分成功响应。
2. `NotebookHomePage()` 复用 `SpaceEditor({space?, onCreated?})` 和游标分页；卡片只显示真实 `Space` 字段。`/spaces/:id/manage` 保留 `SpacePage`；`NavigationDrawer({spaceId?, onClose, ...})` 在无当前区时要求明确选区，保留主题／退出与原所有业务入口。
3. `NotebookWorkspacePage({spaceId, sessionId?, search})` 组合 `NotebookLayout` 的三个持久挂载面板。`NotebookPanel = 'sources' | 'classroom' | 'studio'`；只将 UUID 目标和枚举面板写入 query。`GoalSelector({spaceId, goalId?, onSelect})` 保留搜索、状态与分页；`ClassroomPanel` 复用 `GoalComposer`、`SessionPicker`、`TeachingPage({..., embedded: true})`，课堂主体保持原提交／核对／帮助／草稿控制器。
4. `useNotebookSources(spaceId)` 只读区内 collections，展开某集合才读指定 revision tree；列表保留 collection/revision/document 身份与 PDF／未知覆盖提示。`SourcesPanel` 不提供选择框；`StudioPanel` 四项显示“尚未交付”，原内容库提供真实链接。移动标签隐藏面板但不卸载，抽屉使用原生 modal dialog 的焦点限制与 Escape。
5. 先运行新场景，再迁移旧首页、菜单、管理页、教学列的定位。执行 `npm run check && npm run build`、草稿／教学／相关导航单测，重新构建发行服务与独立 schema 工具，只跑受影响 Chromium 文件；每组通过即停。记录真实文件映射、失败修复、通过次数及延期入口。

**后端改动：** 默认无；仅已有 API 组合。新路由处在现有 SPA 前缀内，若实测静态分发不满足再修对应 HTTP 路由测试／分发，不能扩大 catch-all。无 OpenAPI／migration；G0 不纳入。

**延期：** 四种来源 modal、可核验选择问答／引用整合到批次 2；新 Studio 到批次 3；完整移动／可访问性／发布矩阵到批次 4。已有功能的键盘可用性和被破坏测试不延期。

**退出标准：** `npm run check`、受影响草稿／教学／导航单测、`npm run build`；Chromium 的新工作区主场景与受影响 learning／workspace／release 场景通过；390／1280px 无整页横溢出；同标签页切区恢复原草稿，双标签页互不覆盖；旧目标管理、课堂、进度、离线入口可达。记录真实文件映射与尚不支持入口，生产路径完整，工作树可审查。

**重型证据：** 三引擎全矩阵与候选发行检查延到 Candidate，原因是后续样式与路由还会变化；本批完成新工作区及受影响旧页面的 Chromium 局部回归，不替代最终发布证据。

#### 批次 1 实施记录（2026-10-12）

**改动摘要与状态边界。** 首页使用真实学习区卡片墙，保留搜索、状态、游标分页与新建，创建成功进入新学习区；卡片只显示真实元数据及“学习区更新”时间，不把集合数当来源数，也不伪造最近学习排序。`/spaces/:spaceId` 与 `/spaces/:spaceId/learn/:sessionId` 使用新工作区；原区管理移到 `/spaces/:spaceId/manage`，目标、课堂、知识、任务、进度、评估、导师、内容库、设置与离线旧深链接继续可刷新。发行静态分发实测满足新路由，后端无需修改。

`NotebookWorkspacePage({spaceId, sessionId?, search})` 承担设计中的 `WorkspaceContext` 边界，没有新增全局状态 store：身份／代次来自 `useIdentity`，区／会话来自路由；query 只接受 UUID `goal` 和 `NotebookPanel = 'sources' | 'classroom' | 'studio'`。指定会话时以服务端冻结目标为准，冲突 query 明确报错；无目标／会话时要求明确选择，不默认采用首项。中栏复用 `GoalComposer`、`SessionPicker` 和 `TeachingPage({embedded: true})`，抽出的 `TeachingActivity` 同时用于原课堂呈现与嵌入呈现；正式答案、帮助等级、未知结果核对、内容版本、选段加工、教学变更及标签页内存草稿沿用原控制器。冻结资料与知识在中栏的折叠区，导师位于活动下方，避免再嵌套旧教学三列。

来源按集合／版本／文档保留身份，展开集合才读取现有 tree；不调用全库 export 来填卡片。PDF 显示真实覆盖报告，无法推断的类型／覆盖显示未知。请求带身份／区前缀与取消信号；撤去来源读取权限后立即隐藏缓存目录，跨区迟到响应不会进入新区。`SourceSelection` 与新 `CitationTarget` 尚未实现：来源无勾选控件，课堂引用仍用原冻结范围及原资料查看器，不把左栏浏览解释为资料采用。归档区保留历史浏览，阻止新建目标／课堂及已有课堂写入。

布局沿原型使用浅灰背景、白色圆角面板、稳定卡片配色、胶囊按钮及三栏比例；小于 1100px 用底部标签，隐藏面板保持挂载且移除其可聚焦内容。原生 modal dialog 提供菜单焦点限制、Esc 与关闭后的焦点恢复；菜单内保留主题、退出及全部原页面，无当前区时要求明确选区。已有 NoteSync 仍是默认区／默认集合映射，菜单明示其边界。

**尚未交付的入口。** 右栏学习指南、测验、简报、笔记均显示“尚未交付”，仅原 Studio 内容库链接可用；没有假生成、假进度或定时器成功。四种来源添加 modal、用于问答／生成的来源选择与统一引用定位留给批次 2；新 Studio 产物留给批次 3。资料导入、研究、参考预览／确认、历史 PDF／原文和原课堂内容库继续从旧页面进入。

**失败场景与旧用例迁移。** 实施前新浏览器场景因缺少新首页失败，记录在 `/tmp/notebook-batch1-red-corrected.json`。后续新增目录撤权断言发现缓存目录仍可见，修复权限展示分支；手机截图及顶栏几何断言发现旧全局 flex 样式使品牌越出 64px 顶栏，修正限定于新壳的排列样式。下列旧测试保留原业务断言，仅迁移入口／布局定位；通过后不再反复全跑。

| 旧测试 | 迁移原因与保留范围 |
| --- | --- |
| `learning.spec.ts` | 目标建立移到区内，新建区自动进入，区管理移到 manage；主题、切区、退出从菜单操作。保留 IME、生命周期、分页、409、重启与双标签草稿隔离。 |
| `workspace.spec.ts` | 原学习／导师子标签改为中栏正文，冻结来源需展开；原嵌套列宽键盘断言改为新来源栏收起／展开断言。保留 13 个正式答案、帮助、丢响应、评估、内容版本、教学变更等业务场景。 |
| `release.spec.ts` | 增补工作区、manage、面板 query、课堂、目标和内容库直达／刷新；保留 no-store、真实资产和错误 API／资源边界。 |
| `mentor.spec.ts`、`tutor-history.spec.ts`、`memory.spec.ts` | 配对后先明确进入学习区再建立目标，导航从菜单；记忆用例按区 ID 进入，避免前一隐私清除场景擦除区名后依赖旧名称。原预算、SSE、审批、回执、远端清除保持。 |
| `knowledge-structure.spec.ts` | 更新配对标题、冻结来源展开和返回首页菜单入口，保留维护往返的原题草稿与清除后缓存／历史不恢复正文断言。 |
| `references.spec.ts`、`pdf.spec.ts`、`review.spec.ts`、`notesync.spec.ts`、`tasks.spec.ts`、`offline-fixture.ts` | 更新配对后首页断言；原深链接、审批、覆盖缺口、任务／复习／离线恢复继续执行。 |
| `settings.spec.ts` | 更新配对标题，并将过期的 Key 字段定位改为现有“新 Key（留空保留同连接旧值）”；不改设置产品语义，保留不回显、显式探测和权限隔离。 |

**检查命令与结果。** 在本 worktree 执行，未安装依赖。`node_modules` 为只读符号链接，原 `npm run build` 因 Vite 尝试写入依赖内 `.vite-temp` 返回 EACCES；随后使用 native config loader 成功，未改构建配置。Go 缓存及 schema 工具也放在获准可写的 `/tmp`，没有写入用户家目录缓存。

| 工作目录／命令 | 结果 |
| --- | --- |
| `clients/web`：`npm run check` | 通过；最后一次检查包含全部新增页面、组件、hook 与迁移测试。 |
| `clients/web`：`npm run build -- --configLoader native` | 通过，包含内部类型检查；构建仍有 Zod PURE 注释与单 chunk 大于 500kB 提示。最终构建日志 `/tmp/notebook-batch1-build-final.log`。 |
| `clients/web`：`npx vitest run --configLoader native src/lib/drafts.test.ts src/api/teaching.test.ts src/api/start.test.ts src/api/content.test.ts src/components/content-blocks.test.tsx` | 5 个文件、13 个单测通过。 |
| `server`：`GOCACHE=/tmp/notebook-batch1-go-cache go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过，在前端最终构建后重新嵌入发行资产。 |
| `server`：`GOCACHE=/tmp/notebook-batch1-go-cache go build -o /tmp/notebook-batch1-webdatabase ./testutil/webdatabase` | 通过；独立 schema 包装工具使用 `/tmp` 路径替代不可写的家目录缓存。 |
| `server`：`GOCACHE=/tmp/notebook-batch1-go-cache go build -o /tmp/notebook-batch1-webreview ./testutil/webreview`；`clients/cli-go`：`GOCACHE=/tmp/notebook-batch1-go-cache go build -o /tmp/notebook-batch1-cli ./cmd/edu-agent` | 通过，仅为原复习测试宿主及生产 CLI 兼容性读回构建，未修改其代码。 |
| `clients/web`：`/tmp/notebook-batch1-webdatabase npm run test:browser -- 'tests/browser/notebook-workspace.spec.ts$' --project=chromium --reporter=line,json` | 新增 7 个场景全部通过：真实开课／答案／反馈、390／1280 无横溢出、菜单／旧路由、跨区与双标签草稿、懒加载／撤权、归档恢复、迟到来源响应。 |
| `clients/web`：对受影响旧文件逐个运行同一 schema 包装的 `npm run test:browser -- 'tests/browser/<文件>.spec.ts$' --project=chromium --reporter=line,json`；离线用 `npm run test:offline -- --project=chromium --reporter=line,json` | 旧 14 个 spec 的 44 个场景均有通过记录；只重跑修正或样式影响的场景，计数不重复累计。见下表。 |
| 根目录：`git diff --check` | 通过；未暂存／提交／推送。 |

浏览器环境：`TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable`、`WEB_CHROMIUM_PATH=/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome`、`WEB_RELEASE_MATRIX=1`，端口 32929。依 `scripts/check-web-release.mjs` 对 `learning`／`settings` 设置 `WEB_WORKSPACE_FIXTURE=0 WEB_MENTOR_FIXTURE=0`，其余设为 1；`notesync` 设 `WEB_NOTESYNC_FIXTURE=1`，`memory` 设 `WEB_NOCTURNE_FIXTURE=1`。复习另设 `WEB_REVIEW_FIXTURE=/tmp/notebook-batch1-webreview`、`EDU_AGENT_INTEGRATION_CLI=/tmp/notebook-batch1-cli`；离线脚本设置自己的签名／离线 fixture。每个文件使用独立 schema 和服务进程，未触碰其他 Docker 容器。

| Chromium 文件 | 通过场景数 |
| --- | ---: |
| `notebook-workspace.spec.ts` | 7 |
| `learning.spec.ts`／`workspace.spec.ts` | 5／13 |
| `release.spec.ts`／`references.spec.ts`／`pdf.spec.ts` | 1／1／1 |
| `mentor.spec.ts`／`tutor-history.spec.ts`／`settings.spec.ts` | 1／1／1 |
| `knowledge-structure.spec.ts`／`tasks.spec.ts`／`notesync.spec.ts` | 3／2／1 |
| `memory.spec.ts`／`review.spec.ts`／`offline.spec.ts` | 5／2／7 |

局部证据目录 `/tmp/notebook-batch1-browser-evidence/` 保留逐次 JSON、日志、截图；`accepted-summary.json` 按用例合并原通过记录与必要的 focused 重测，合计 51 个不同场景，均通过现有 `checkBrowserResults(..., 'chromium')` 校验，无 skip、retry、预期失败。早期 `workspace` 文件名匹配也包含新 notebook 文件，计数时只取对应旧 suite；后续命令加路径末尾 `$` 避免这一情况。最终 390／1280 截图位于 `notebook-workspace-artifacts/`；早期失败报告仍作为排查证据，不视为整轮全绿。此记录为局部退出证据，不是候选发布矩阵。

**用户决策与风险。** 批次 1 没有新增待用户决策的问题；D1–D4 继续留给后续批次，不因本次壳层实施视为确认。仅验证 Chromium 和本地确定性模型／远端夹具，没有验证真实外部模型／搜索。三引擎完整矩阵、Go 全量／race、Compose、完整可访问性与最终发布证据未执行，按计划后移；新旧样式共存仍需后续候选阶段复核，现有 chunk 体积提示未在本批扩展为拆包重构。

### 批次 2：四种来源、引用定位与按所选来源问答（7–10 人日）

**结果：** 用户在工作区完成 PDF／文本／网址／搜索候选的审阅导入，选来源追问并定位核验片段，原课堂范围保持冻结。

**范围：** 父验收 5–12、相关 13、21–27；D1、D4 确认后落实 G1、G2、G3。目标研究旧页仍可恢复旧 run。来源选择／生成清单合同在本批冻结，供批次 3 复用。

**涉及前端／测试：** `components/reference-import.tsx`、`content-blocks.tsx`、`pdf-source.tsx`、`mentor-panel.tsx`、`tutor-history.tsx`、`research-page.tsx`、`knowledge-page.tsx` 的必要抽取；新增 notebook 下 `add-source-dialog.tsx`、`source-list.tsx`、`source-discovery.tsx`、`source-viewer.tsx`、`mentor-composer.tsx`，`api/notebook-sources.ts`；修改 `api/{references,research,mentor,conversations}.ts` 与 `lib/reference-import.ts`。新增 `notebook-sources.spec.ts`；扩展 references／pdf／mentor／tutor-history 用例与 `clients/web/scripts/test-server.mjs`。

**后端单列：** 修改／扩展 `server/internal/knowledge/{domain,scopes,import_preview}.go`、`knowledge/postgresstore/{store,research,content,privacy}.go`，新增 `knowledge/source_catalog.go`、`knowledge/postgresstore/source_catalog.go` 与来源元信息 migration；新增 `httpapi/source_discovery.go`／`source_catalog.go`，复用 `reference_sources.go`、`settings/search.go`、`integrations/websearch`。修改 `mentorrun/{types,conversation,references,core}.go` 及 worker 的必要快照路径、`httpapi/{mentor_runs,tutor_history,api,knowledge_spaces}.go`，应用 composition 按需要注入端口；`server/api/openapi.yaml`、生成 `clients/web/src/api/schema.d.ts` 及合同测试。G3 使用 checkpoint 不单独建选择表。

本批的关键测试文件包括现有 `server/internal/knowledge/postgresstore/{import_preview_test,scopes_test}.go`、`server/internal/mentorrun/{references_test,research_test,research_privacy_test,conversation_test}.go`、`httpapi/{references_test,research_test,mentor_runs_test}.go`，以及拟新增 source_catalog／source_discovery／source_selection 的领域和 HTTP 测试；新增 `server/testutil/webnotebook/main.go` 及其宿主边界测试，供真实来源浏览器场景使用。这些是文件边界建议，不要求重跑所有历史测试才能开始局部反馈。

**风险：** 高，来自权限、网络边界、选择／历史隔离与清除竞态，而非 modal 外观。新增受区路径需同步 `scopedKnowledgePath` 等分类；不能漏掉 server 归属门禁。

**执行步骤：**

- [x] 先锁定 G1–G3 DTO、权限与错误语义，增加来源身份／范围 unit／HTTP 用例和真实 import→问答→citation 的失败场景；范围变更及空选择必须覆盖。
- [x] 串行交付来源 metadata／列表／详情与原导入适配，再交付受限搜索候选和“选中才抓取”，保留 PDF 部分确认、重名身份审阅及 unknown 原操作核对。
- [x] 导师请求接选择清单、上下文隔离、核验 citation；零来源不引用资料，来源撤权后停止展示，研究／课堂旧引用接统一查看器。
- [x] 验证同名双集合、跨区伪造、历史版本、选择变化、无目标问答（D4）、两标签、响应丢失与隐私清除；记录供 Studio 使用的范围／引用合同。

**延期：** Studio 生成与笔记到批次 3；卡片聚合增强 G0 不做；大批次导入沿原任务页，不把所有任务控制搬进 modal。

**退出标准：** 前端类型／相关 import、PDF、mentor、conversation、citation 单测及构建；新来源与保留 references／pdf／mentor 局部 Chromium 场景通过。`cd server && go test ./api ./internal/knowledge/... ./internal/research ./internal/mentorrun ./internal/transport/httpapi` 中受影响测试通过（DB skip 另列）；独立 PostgreSQL 跑 metadata 升级、导入幂等／冲突、选择范围与清除竞态新集成用例；新增 API 生成一致性通过。实际模型请求中不含未选资料，冻结课堂引用未变。独立网络 stub 证明搜索摘要不能被导入／引用为已读正文。

**重型证据：** 全量历史 DB／三浏览器门禁延到 Candidate；本批来源权限、SSRF 既有相关契约和新清除／范围验证不可延期。新 DB 场景名称与运行命令交接给批次 4 的候选门禁。

#### 2a 实施记录（2026-10-12）

**结果与范围。** 交付 G1 来源目录、PDF／文本／网址导入与来源查看。添加对话框直接复用 `ReferenceImport`，保留服务端预览／确认、PDF 部分接受、同名身份审阅和 unknown 原操作核对。普通导入不会创建目标／课堂或更改已冻结参考。2b 搜索发现、2c 按选择问答与批次 3 Studio 未交付，左栏没有看似生效的勾选控件。按“回执领域回归 → sidecar／DB → HTTP/OpenAPI → 前端 → Chromium”的计划完成；不创建第二套导入控制器。

**公共合同。**

| 项目 | 2a 合同 |
| --- | --- |
| 来源列表 | `GET /v1/knowledge/sources`，`knowledge:read`，`X-Learning-Space-ID` 沿用默认区兼容语义；可选 `X-Knowledge-Collection-ID` 进一步限定集合。只列当前区已关联集合的 head 文档，不能把可发现但未关联的共享集合当作获准正文。`limit` 默认 30、范围 1–100；不透明 `cursor` 绑定区及集合过滤，按集合 UUID／文档 UUID 排序，返回 `items` 与可选 `next_cursor`，没有伪造总数。分页反映读取时的 head，不是冻结选择快照。 |
| 文档身份 DTO | `collection_id`、`collection_name`、`revision_id`（集合修订）、`document_id`（稳定身份）、`document_revision_id`、`path`、`metadata`。同名文档仍保留完整集合／版本身份。`metadata: null` 表示未知；不从路径、标题或 Markdown 前缀推断来源。 |
| 元信息 | `kind: text/pdf/url`，可选 `locator/final_url/media_type`，`parser/coverage/fingerprint/status`，可选 `derived_from: {artifact_id, version}`。纯文本 `provided_text` 指用户提供的内容；指纹为规范正文 hash。PDF 指纹为原件 hash，URL 为实际抓取指纹。`parsed/partial` 不代表知识正确、版式完整或已正式采用到课堂。派生字段仅预留服务端写入，客户端不能自报。 |
| 历史详情 | `GET /v1/knowledge/sources/{revisionID}/documents/{documentRevisionID}`；必须显式传集合头，再核验区关联与集合修订归属。读取指定文档 payload，不遍历整个集合正文。返回身份／元信息、PDF 逐页报告及节点片段（节点身份、标题、规范文本 UTF-8 字节／行位置、实际物理页）。单片段展示最多 4096 字节，UTF-8 边界不截断字符，`truncated` 区分展示预算与解析缺口。PDF 无文本页和元信息块不冒充原文片段。 |
| 网页回执 | `reference-sources` 的非 PDF 成功结果新增 `import_markdown` 与 `source_receipt`。服务端绑定确切正文 hash、实际抓取元信息、区与 knowledge 隐私代次后签发；预览／确认原样提交两者。失败、禁止留存或无正文不发回执。重启后旧抓取回执需重新获取；未知提交先查原 operation，不能直接新建操作重发。PDF 继续原件解析与既有指纹／URL 回执校验。 |
| 权限与错误 | 两个读端点都有 `knowledge:read`、knowledge response read permit、owner generation gate 和 `no-store`。缺详情集合头、非法 ID／游标／分页或伪造抓取回执为 400；无 scope 为 403；版本／文档不属于所选集合、未关联或解除关联为 404；不回落最新版。隐私屏障沿用 `content_redacted`，归档区可读，写入继续服务端拒绝。预览／确认／同载荷重放／不同载荷冲突沿用原合同。新路径同步 scoped／collection 路由分类与部署代理白名单。 |
| 能力与旧引用 | `source-capabilities.source_catalog` 表明新目录能力；旧服务明确显示不可用。课堂引用继续从 content citation 接口核验原文 hash／片段范围，研究引用打开时通过原 run/source 接口核对确切 revision／fragment，三者共用查看器对话框与焦点恢复；不把旧引用改查最新目录。 |

**Migration 与隐私。** 只追加 `000034_source_metadata.sql`：`knowledge_source_metadata(document_revision_id PK/FK, metadata JSONB)`，仅存元信息，不复制正文。复用现有 snapshot 的 `(revision, document)` 唯一索引分页；新增 owner write gate 和 immutable trigger，写入与导入提交同事务。无历史文本／网页回填；已保存的可信 PDF 报告可在读取时呈现已有字段。相同历史正文版本保持原元信息（包括未知）。`StoreKnowledgeContent` 删除 sidecar，`VerifyRedacted` 把其残留计入；旧版本的正文、原件仍由原 owner 清除。

**新增文件映射。**

- `server/migrations/000034_source_metadata.sql`（migration）、`source_metadata_test.go`（33→34 升级与旧正文保留）。
- `server/internal/knowledge/source_catalog.go`、`source_catalog_test.go`（DTO、回执、片段）；`postgresstore/source_catalog.go`、`source_catalog_test.go`（列表／详情、集合与共享隔离、幂等、不可变）。
- `server/internal/transport/httpapi/source_catalog.go`、`source_catalog_test.go`（读端点、scope／owner 屏障）；`server/api/openapi_source_catalog_test.go`（DTO 与公共合同）。
- `clients/web/src/api/notebook-sources.ts`、`.test.ts`；`components/notebook/add-source-dialog.tsx`、`source-viewer.tsx`；`tests/browser/notebook-sources.spec.ts`。
- `server/testutil/webnotebook/main.go`、`main_test.go`：仅测试二进制，要求 `web_candidate_<32 hex>` schema，用既有 `NewFetcherWithNetwork` 将一个固定公开测试域接入可控 HTTP 页面；生产二进制与 SSRF 校验不变。

**修改文件映射。**

- **OpenAPI**：`server/api/openapi.yaml`；`npm run generate` 生成 `clients/web/src/api/schema.d.ts`。
- `server/internal/knowledge/{domain,service,import_preview,research}.go`：导入元信息、签名回执与请求 hash；`postgresstore/{store,content,privacy}.go`：原子持久化／读取、课堂引用元信息、隐私清除。
- `server/internal/transport/httpapi/{reference_sources,knowledge_spaces}.go`：抓取回执、能力与路由分类；`references_test.go`：真实 HTTP 回执与来源合同。`server/migrations/migrations_test.go` 更新最新版本断言；`postgresstore/maintenance_integration_test.go` 验证 sidecar 清除。
- `clients/web/src/api/references.ts`、`lib/{reference-import,use-notebook-sources}.ts`、`lib/reference-import.test.ts`：回执传递、目录分页与测试；`components/reference-import.tsx`：三个入口共用原审阅控制器；`components/notebook/sources-panel.tsx`、`notebook-workspace-page.tsx`：来源目录与归档行为；`components/content-blocks.tsx`、`research-page.tsx`：旧引用共用查看器。
- `clients/web/tests/browser/notebook-workspace.spec.ts`：原 tree 定位改为新目录，保留同名、撤权与迟到响应断言；`clients/web/scripts/test-server.mjs` 支持选择独立测试宿主。`deploy/web/nginx.conf` 仅增加锚定的新来源读路径与能力路径。

**检查记录。** 所有 Go／DB／浏览器命令先 `umask 022`；沙箱下 `GOCACHE=/tmp/notebook-b2a-go-cache`，辅助工具编译到 `/tmp/notebook-b2a-webdatabase` 和 `/tmp/notebook-b2a-webnotebook`，不写只读用户缓存目录，不操作 Docker 容器。

| 检查 | 命令与结果 |
| --- | --- |
| 合同生成 | `cd clients/web && npm run generate` 通过；保留既有 `ActionExposureRequest` discriminator 警告。 |
| 前端 | `npm run check` 通过；`npm run build -- --configLoader native` 通过（其内也运行 check），仅既有 Zod 注释与大 chunk 提示。`npm test -- --configLoader native src/lib/reference-import.test.ts src/api/notebook-sources.test.ts src/api/content.test.ts src/lib/drafts.test.ts`：4 文件、11 测试通过。 |
| Go 受影响包 | `cd server && go test ./api ./internal/knowledge/... ./internal/transport/httpapi`：knowledge、llmselector、postgresstore 非 DB、httpapi 通过；首次 API 检查发现代理白名单缺新路径，修复后 `go test ./api -run '^TestWebReleaseProxyCoversClientAPI$'` 通过，其余 API 结果复用。新增 read scope／owner gate 与片段回归分别定向通过。未设置 DB 的包测试不充当 DB 验收。 |
| Vet | `go vet ./api ./internal/knowledge/... ./internal/transport/httpapi ./testutil/webnotebook` 通过。 |
| 真实 PostgreSQL | `TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable go test -p=1 ./migrations ./internal/knowledge/postgresstore ./internal/transport/httpapi -run '^(TestSourceMetadataMigrationUpgrade\|TestEmbeddedMigrationsAreOrderedAndUnique\|TestPostgreSQLSourceCatalogPermissionsPaginationAndReceipts\|TestPostgreSQLImportPreviewConfirmAndReplay\|TestPostgreSQLImportConfirmRollsBackWholeBatch\|TestPostgreSQLKnowledgeSpacesIsolationAndFrozenVersions\|TestPostgreSQLPDFVersionsCitationsAndScope\|TestPostgreSQLKnowledgeMaintenanceProposalLifecycle\|TestPostgreSQLReferencesCookieImportCollectionAndScope)$'`：三包通过。另 `-p=1 ./internal/transport/httpapi -run '^TestPostgreSQLResearchCookieSourcesAndDecisions$'` 通过，验证研究历史读取／采用原边界。 |
| Chromium 来源 | `WEB_NOTEBOOK_FIXTURE_BINARY=/tmp/notebook-b2a-webnotebook /tmp/notebook-b2a-webdatabase npm run test:browser -- notebook-sources.spec.ts --project=chromium`：最终构建下 1 场景通过（9.0s），覆盖三种添加、unknown 核对、PDF 部分确认、真实重定向 URL、片段／页图／手机／重开。 |
| Chromium 回归 | 普通发行服务，`WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0 /tmp/notebook-b2a-webdatabase npm run test:browser -- references.spec.ts pdf.spec.ts notebook-workspace.spec.ts --project=chromium`：9 场景通过，无 skip/retry。 |
| Chromium 旧引用 | 同一普通发行服务与 fixture，`/tmp/notebook-b2a-webdatabase npm run test:browser -- workspace.spec.ts -g '旧会话真实阅读、版本、讨论、丢响应作答、反馈及续学' --project=chromium`：1 场景通过（25.2s），包含历史来源核验、共用查看器／焦点恢复及原课堂流程。首次加起始锚点的过滤表达式未匹配到用例，移除锚点后实际执行通过。 |

浏览器命令共同使用上述 `TEST_DATABASE_URL` 与 `WEB_CHROMIUM_PATH=/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome`；发行服务由 `go build -tags web_release -o edu-agentd ./cmd/edu-agentd` 构建。日志／产物在 `/tmp/notebook-b2a-*`，不纳入提交。

**2b／2c 接口约定。** 2b 候选不进入此目录；必须经过真实 `reference-sources` 读取、留存许可、正文回执和用户确认后才成为正式来源。2c 冻结 `{collection_id, revision_id, document_id, document_revision_id}`，发起请求时由 owner 重新核验权限／版本，不能以目录缓存、`next_cursor` 或客户端片段代替冻结与引用校验，也不把此列表用于修改课堂参考政策。`derived_from` 留给批次 3 的可信笔记版本转换。

**决策与限制。** 无待用户决策的语义问题。未运行三引擎矩阵、Go 全量、race、Compose 或真实外部搜索／模型研究浏览器流程；未把这些记为通过。目录不是总量统计或选择生效功能。片段展示预算与 PDF 无 OCR／版式限制仍明确可见；未更改 CLI、原件导出或自动共享语义。

#### 2b 实施记录（2026-10-12）

**结果与范围。** “＋ 添加来源”增加搜索发现：填写公开关键词与可选域名限制 → 勾选明确外发确认 → 点击搜索 → 查看标记为“摘要，未读取正文”的候选 → 勾选并授权读取正文 → 原 `ReferenceImport` 解析报告／预览／确认 → G1 来源目录。无目标的学习区可执行此流程。搜索不会创建目标、课堂、模型调用、持久任务或正式来源；没有新增 migration。旧目标研究运行及其持久化、读取和采纳路径保留。候选默认不勾选，改变关键词／域名会作废外发确认和旧候选；重试需用户重新明确发起。

**公共合同。**

| 项目 | 2b 合同 |
| --- | --- |
| 发现请求 | `POST /v1/knowledge/source-discovery`，闭合 DTO `{keywords, external_consent: true, policy: {mode, domains}}`；拒绝未知字段、重复键、未确认请求。关键词为非空单行，最多 300 UTF-8 字节；不接收 goal、笔记、资料正文或 collection 上下文。请求体最多 4096 字节。`X-Learning-Space-ID` 沿原默认区兼容规则，拒绝不适用的集合头；归档区禁止搜索。 |
| 域策略 | `supplement` 要求空域列表；`restrict` 要求 1–5 个精确公开小写域名，不隐式包含子域。复用研究政策校验及 `site:` 查询构造，最终查询不超过 Brave 的 1024 字节限制。返回前按公开 URL 与域策略过滤、去片段并去重；不因过滤减少结果而补搜。 |
| 候选响应 | `{items, request_budget: 1, requests_used: 1, cost_estimate: "unknown"}`；`items` 最多 4 项，可为空，每项仅 `{title, url, summary, status: "summary_only"}`。没有正文、fragments、revision、import_markdown 或 source_receipt，不能把摘要交给导入或引用作为已读正文。响应 `no-store`，服务端不保存搜索请求或结果。 |
| 权限 | 发现要求全部 `learning:write`、`knowledge:read`、`knowledge:write`、`research:adopt`；沿用设备限流、精确 Origin、Cookie CSRF、学习区分类及 knowledge owner 响应屏障。`research` 配对可调用发现接口；正文抓取仍须 `references:manage`／`knowledge:write`，通用预览确认仍须原导入审批权限。未扩大任何配对档案权限。 |
| 预算与网络 | 每次发现最多一次供应商请求，发送前预留，不能超过 `settings.Limits.ResearchRequests`；20 秒总超时，无自动重试，搜索并发受同一配置中的 `Concurrency` 限制，设备限流仍单独执行。无模型调用，因此不消耗模型 token 预算。生产使用 `settings.SearchClient` 与原 Brave adapter；`settings/search.go`、公网 HTTPS 搜索 transport、重定向拒绝及 SSRF 校验均未放宽。此预算只覆盖发现；后续正文读取是独立授权的原 reference-sources 请求。 |
| 能力 | `source-capabilities.source_discovery` 返回 `{available, reason, max_candidates: 4, request_budget: 1}`；`reason` 为 `missing_scope`、`search_not_configured` 或空串。可用表示配置与权限满足，不表示已探测连通；读取能力不发外部请求。旧服务缺字段时前端明确提示升级。 |
| 正文与 G1 | `reference-sources` 新增可选 `policy`，仅适用于 URL；不传时保持原 supplement 行为，传 restrict 时逐跳核验所确认的域限制。仍走原实际抓取、留存许可、正文回执、PDF 部分接受及预览／确认；来源元信息为真实 URL／PDF，搜索摘要不进入 G1。非 PDF 成功正文缺绑定回执时阻止预览；禁止保存／失败项没有可导入正文。 |
| 内存与取消 | 候选、输入和已读取但未确认的导入草稿只存在当前标签页 `DraftStore`；按 origin／principal／generation／space／collection 隔离。关闭或卸载搜索组件取消当前请求，忽略迟到响应；再次打开可见本标签页已完成候选／导入草稿，外发确认需重新勾选。取消批量读取时，已完成项保留待审阅，未完成项不伪报成功。刷新不承诺恢复。 |

**错误语义。** 输入／确认／策略错误为 400（`invalid_request` 或 `research_policy_rejected`）；缺 scope、CSRF 或 Origin 不匹配为 403；归档区沿用 409。设备限流、搜索并发占满、请求预算耗尽、供应商 429 均为 429，分别保留 `rate_limited`、`source_discovery_busy`、`source_discovery_budget_exceeded`、`source_discovery_rate_limited`。取消为 408 `source_discovery_cancelled`，超时为 504 `source_discovery_timeout`，配置／网络不可用为 503 `source_discovery_unavailable`，供应商鉴权、重定向、异常及坏响应为 502 的对应分类；不回传供应商错误正文或凭据。隐私屏障继续原 `content_redacted` 等错误。浏览器断开时不保证还能收到取消响应；界面保留取消事实并提示已发出请求可能产生费用。失败不自动重试，也不把未取得候选当作成功空列表。

**新增文件映射。**

- `server/internal/research/discovery.go`、`discovery_test.go`：公开请求／摘要 DTO、有界 transport、超时取消、政策过滤；独立 HTTP stub 使用真实 Brave adapter 验证外发内容、预算与失败。
- `server/internal/transport/httpapi/source_discovery.go`、`source_discovery_test.go`：发现路由、能力、权限／owner gate／限流／并发、真实 settings adapter 注入和正文域限制。
- `server/api/openapi_source_discovery_test.go`：实际请求／响应 DTO、闭合输入和外发确认的 OpenAPI 合同。
- `clients/web/src/api/source-discovery.ts`、`source-discovery.test.ts`：生成类型约束、独立响应校验、确认与字节预算；`components/notebook/source-discovery.tsx`：候选选择、授权外发、取消及逐项正文读取。

**修改文件映射。**

- **OpenAPI**：`server/api/openapi.yaml`；`npm run generate` 生成 `clients/web/src/api/schema.d.ts`。
- `server/internal/transport/httpapi/{api,reference_sources,knowledge_spaces}.go`：受信搜索依赖端口、能力挂载、可选正文域策略、学习区路径分类；`deploy/web/nginx.conf` 增加锚定的发现路径白名单。
- `server/internal/knowledge/source_catalog_test.go`：候选摘要不能取得正文回执或有效研究引用。
- `clients/web/src/api/references.ts`：正文读取传递 abort signal／可选域策略；`lib/reference-import.ts`、`reference-import.test.ts`：URL 与搜索共用实际正文转换，禁止摘要／禁止保存项进入导入。
- `clients/web/src/components/notebook/add-source-dialog.tsx`、`components/reference-import.tsx`：第四入口接原导入控制器，保留预览、身份审阅、确认及 unknown 核对。
- `server/testutil/webnotebook/{main,main_test}.go`：仅隔离 schema 的宿主注入搜索网络；真实 settings／Brave／抓取／数据库与 Web 路径不替换。生产二进制没有测试网络入口。
- `clients/web/tests/browser/notebook-sources.spec.ts`：新增搜索正文导入、禁止保存、未确认不请求、取消、失败、空候选及 CSRF 场景，保留原三种来源完整场景。
- 本设计文档记录本批合同、验证和 2c 约定。

**检查记录。** 所有 Go／DB／浏览器命令先 `umask 022`；Go 使用 `GOCACHE=/tmp/notebook-b2b-go-cache`。用户缓存目录不在沙箱可写范围，故等价地将 `webdatabase` 与 `webnotebook` 编译到 `/tmp/notebook-b2b-webdatabase`、`/tmp/notebook-b2b-webnotebook`。浏览器使用 `TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable`、`WEB_CHROMIUM_PATH=/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome`；各组由 webdatabase 创建独立 schema 并串行执行，未操作 Docker 容器。

| 检查 | 命令与结果 |
| --- | --- |
| OpenAPI | `cd clients/web && npm run generate` 通过；既有 `ActionExposureRequest` discriminator 警告保留。`TestSourceDiscoveryContract` 和完整 `./api` 测试通过，含代理白名单覆盖。 |
| 前端 | `npm run check`、`npm run build -- --configLoader native` 通过；构建内再次执行 check。仅既有 Zod 注释／大 chunk 提示。`npm test -- --configLoader native src/api/source-discovery.test.ts src/lib/reference-import.test.ts src/api/notebook-sources.test.ts`：3 文件、9 测试通过。 |
| Go 包 | `cd server && go test ./api ./internal/knowledge/... ./internal/research ./internal/settings ./internal/transport/httpapi ./internal/integrations/websearch ./testutil/webnotebook` 全部通过。未设置 DB 的 package 测试中数据库用例 skip，不将其记作数据库验收。新增授权、并发／设备限流与真实 settings/Brave HTTP 用例另有定向通过证据。 |
| Vet | `go vet ./api ./internal/knowledge/... ./internal/research ./internal/settings ./internal/transport/httpapi ./internal/integrations/websearch ./testutil/webnotebook` 通过。 |
| 真实 PostgreSQL | `TEST_DATABASE_URL=... go test -p=1 -v ./internal/transport/httpapi -run '^(TestPostgreSQLReferencesCookieImportCollectionAndScope\|TestPostgreSQLResearchCookieSourcesAndDecisions)$'`：2 项通过（9.367s），验证原 Cookie 导入、来源回执／目录、参考范围及旧研究读取／采纳。使用上述真实 DSN，非 skip；与浏览器数据库检查串行。 |
| Chromium 来源 | `WEB_NOTEBOOK_FIXTURE_BINARY=/tmp/notebook-b2b-webnotebook /tmp/notebook-b2b-webdatabase npm run test:browser -- notebook-sources.spec.ts --project=chromium`：3 场景通过（13.9s），无 skip／retry；真实来源目录在搜索及预览后为空，正式确认后仅保存获准正文，摘要 sentinel 不出现在来源查看器。 |
| Chromium 回归 | 普通发行二进制 `go build -tags web_release -o edu-agentd ./cmd/edu-agentd`，`WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0 /tmp/notebook-b2b-webdatabase npm run test:browser -- notebook-workspace.spec.ts references.spec.ts --project=chromium`：8 场景通过（48.0s），无 skip／retry。涵盖真实课堂、正式答案、移动布局、双标签／切区、撤权隐藏、归档、迟到响应和原导入 unknown 核对。 |

先写失败场景：新 HTTP 路由返回 404、缺 DTO／前端转换器、OpenAPI 缺路径，以及 Chromium 找不到搜索按钮。实现后均通过。开发中修复了新增配置测试的受保护目录权限、OpenAPI 错误 schema 引用及新响应缺少显式 `no-store`，通过后未重复运行无输入变化的检查。日志／临时产物均位于 `/tmp/notebook-b2b-*`，不纳入工作树提交。

**留给 2c 的约定。** 发现候选不是所选正式来源，不提供可冻结版本或引用身份；只有原导入确认后的 G1 `{collection_id, revision_id, document_id, document_revision_id}` 可以作为 2c 的待核验输入。2c 仍须在发起请求时由 owner 冻结、核验来源与引用，不能引用 discovery summary、客户端勾选或临时抓取报告。左栏选择生效／来源问答和 Studio 均未在本批实现；课堂既有冻结范围不变。

**决策与限制。** 无待用户决策的语义问题。未运行真实 Brave smoke（真实调用 0 次）、真实外部模型、三引擎矩阵、Go 全量／race 或 Compose；未把它们记为通过。搜索结果无跨刷新恢复与任务核对接口，外部实际费用未知；网页格式／留存限制和 PDF 解析缺口仍按原流程展示。

#### 2c 实施记录（2026-10-12）

**结果与范围。** 交付 G3 来源选择问答、可核验引用及无目标问答。左栏支持单选／全选已加载来源；首次进入勾选当前已加载条目，后续导入或分页不会静默增加选择。选择属于当前标签页／学习区，只影响随后提交的请求（D1），不修改 goal／session reference heads 或正在进行的课堂。中栏底部复用导师运行控制，提供“换个讲法／补前置知识／举个例子／要提示”；快捷按钮填写问题，由用户发送。首次发送才创建真实 conversation，可不绑定 goal（D4），不会创建占位目标。保存历史采用原有显式同意；临时交流仍遵守原进程内恢复限制。Studio 产物未实施。

**G3 请求合同。** `POST /v1/learning/goals/{goalID}/runs`（普通 mentor 类型）与 `POST /v1/learning/conversations/{conversationID}/turns` 增加可选 `source_selection`：

| 输入 | 行为 |
| --- | --- |
| 不提供字段 | 保持旧导师上下文／工具行为；不会把显式选择的旧轮次混入缺省对话。 |
| `{"mode":"none","items":[]}` | 明确零来源，只有同范围聊天与一般性解释；提示“未选来源，不引用资料”，引用必须为空。 |
| `{"mode":"items","items":[...]}` | 1–100 个 G1 身份，每项为 `{collection_id, revision_id, document_id, document_revision_id}`；同集合的同文档不能重复。 |
| `null`、缺 items、未知字段、重复 JSON 键、无效身份或 mode／条目数矛盾 | 400；不能解释为缺省行为。 |

沿用原 `learning:write`、设备／学习区／goal／conversation 归属、版本、Origin／CSRF、预算与目的地确认门禁；非空选择额外从数据库核验 `knowledge:read`。来源 owner 在受理事务内核验学习区关联、集合／历史 revision、文档及文档 revision，跨区或拼接身份统一返回 404 `not_found`；已清除内容为 409 `content_redacted`，权限失效为 403 `run_forbidden`。保留原操作 hash／回执核对，重试不跟随新勾选或集合 head；请求体上限 64 KiB，覆盖 100 条身份及原提示。

**固化与上下文。** `source_manifest` schema 1 包含学习区、knowledge 隐私代次、按集合／文档身份排序的确切版本和规范全文 SHA-256，以及整个清单的 SHA-256。写入既有 JSON 加密 run body；保存轮次复用既有加密 history，未新增 SQL 表、索引或 migration。临时正文仍只在原进程内存。续跑、读取、工具、历史发送和引用核验均使用这个清单；集合 head 更新不会改变原运行。只有 manifest hash 完全相同的旧轮次可发送到模型，缺省旧历史也被隔离。模型只收到所选文档的有界片段、身份／元信息和同范围聊天；不读取整个集合，也不注入目标、课堂、记忆或 companion 上下文。工具仅开放 `read_references`、`ask_user`、`confirm_focus`，其中 `read_references` 只能重读本轮清单；即使模型自行提出其他工具也拒绝执行。上下文超预算以既有 `history_context_limit` 停止，不自动扩大来源或截掉选择。

**引用与撤权。** 模型最终输出 `{"answer":"…[1]","citations":[{"source":1,"fragment":1}]}`，两个序号只能指向本次服务端给定数组。服务端重新读取历史版本，生成 `MentorCitation`：`id`、完整 `source` 身份／元信息、带 node 版本和字节／行范围的 `fragment`、`document_sha256`、`slice_sha256`。非法 JSON、空 citations 值、越界或重复引用使运行失败（`invalid_source_citation`），不会显示原始模型 JSON。保存和每次读取再次核验版本、片段身份／范围／原文和 hash。

新增 `GET /v1/learning/runs/{runID}/citations/{citationID}`，需要 `learning:read` 与 `knowledge:read`，按原 run 的设备和学习区核验，拒绝客户端指定其他集合／片段；响应 `no-store`。保存的 conversation 可在短期 checkpoint 到期后从加密历史读取引用。Snapshot／TutorTurn 同时返回经过核验的 `source_manifest`／`citations`。来源撤权、解除关联或隐私清除后隐藏整个受影响轮次正文及工具记录；旧客户端缓存不能作为新的读取凭据。前端只将服务端 citations 中的编号变成按钮，点击再走 citation GET，左栏滚动／聚焦高亮片段，手机切换到来源标签。运行快照与引用查看器分别每 2.5 秒／5 秒重新核验，历史沿用 5 秒核验；收到权限错误即隐藏正文。

外发期间仅保留 token／来源关联读锁，防止核验到发出之间撤权或解除关联已提交；不跨模型网络 I/O 持有隐私 owner 代次锁。共享 ReadPermit 纳入 knowledge owner，隐私屏障可取消在途请求；后续 checkpoint／history 写入仍通过现有代次与清除门禁，迟到输出不能复活正文。

**文件映射（本次新增）。**

- 服务端：`server/internal/knowledge/source_selection.go`、`knowledge/postgresstore/source_selection.go` 定义 DTO、固化与精确读取；`server/internal/mentorrun/source_selection.go` 实施选择上下文、工具范围、引用生成／读取。
- 测试：`server/internal/mentorrun/{source_selection_test,source_selection_privacy_test}.go`、`server/internal/transport/httpapi/source_selection_test.go`、`server/api/openapi_source_selection_test.go`。
- 前端：`clients/web/src/api/source-selection.ts` 及其单测、`components/notebook/{source-selection,mentor-composer}.tsx`、`components/mentor-answer.tsx`。

**文件映射（本次修改）。**

- `server/internal/knowledge/postgresstore/source_catalog.go` 复用精确文档读取；`mentorrun/{types,store,conversation,references,core,worker,query,command}.go` 串接受理、模型、重试、历史与读取校验；`transport/httpapi/{mentor_runs,tutor_history}.go` 增加输入和 citation 路由。
- **OpenAPI：** `server/api/openapi.yaml`；生成 `clients/web/src/api/schema.d.ts`，更新 `api/{mentor,conversations}.ts` 响应解析。新路径已被 `mentorPath` 的 run 前缀归类，HTTP 单测确认不进入客户端 collection 选择；`deploy/web/nginx.conf` 明确记录既有 run 白名单覆盖 citation，既有 API／代理合同测试检验覆盖。
- `clients/web/src/components/{mentor-panel,tutor-history}.tsx`、`components/notebook/{notebook-layout,sources-panel,source-viewer}.tsx`、`notebook-workspace-page.tsx`、`styles.css` 接入选择／输入／定位；`scripts/test-server.mjs` 添加真实 HTTP 确定性模型响应；`tests/browser/{notebook-workspace,notebook-sources}.spec.ts` 覆盖新行为并更新旧“无勾选”断言。
- 本设计文档记录 G3 与批次 3 的衔接。

**验证结果。** 所有数据库／浏览器检查串行，先 `umask 022`。使用提供的 PostgreSQL 32995 与 Chromium 路径；Go 缓存为 `/tmp/notebook-b2b-go-cache`。沙箱不能写用户 cache，独立 schema 包装器编译到 `/tmp/notebook-b2c-webdatabase`，未改其协议。

| 检查 | 命令与结果 |
| --- | --- |
| OpenAPI／前端 | `cd clients/web && npm run generate` 通过；`npm run check` 与 `npm run build -- --configLoader native` 通过。保留既有生成器 discriminator 提示、Zod 注释及 bundle 大小警告。 |
| Vitest | `npm test -- --configLoader native src/api/mentor.test.ts src/api/conversations.test.ts src/api/notebook-sources.test.ts src/api/source-selection.test.ts`：4 文件、9 测试通过，覆盖选择身份、显式 none 与只链接核验编号。 |
| Go 局部包／静态检查 | `cd server && go test ./api ./internal/mentorrun ./internal/knowledge/... ./internal/transport/httpapi` 与相同包的 `go vet` 通过；此轮未设置 DB，不将数据库 skip 计为验收。另运行引用身份／范围／hash 篡改单测通过。 |
| PostgreSQL | 下述组合命令 22 个场景通过；新增工具／同范围历史夹具重复了 tool call ID，被原防重放门禁拒绝，改为唯一 ID 后定向运行 `-run '^TestPostgreSQLSourceSelectionToolsAndSameScopeHistory$'` 通过（3.70s）。合计 23 个顶层场景通过，含 8 个新增选择场景；已通过项未重跑。 |
| Chromium | 所选来源问答新增场景通过（32.3s），原 notebook-workspace／mentor／tutor-history 9 场景通过（1.2m），无 retry／skip。来源搜索 2 场景通过，三种添加方式在修正旧无勾选断言后定向通过（9.1s），合计 13 个 Chromium 场景通过。 |

数据库候选命令为 `TEST_DATABASE_URL=... go test -p=1 ./internal/mentorrun ./internal/knowledge/postgresstore ./internal/transport/httpapi -run '^TestPostgreSQL(SourceSelection|TutorHistory|MentorRealCoreRecoveryAndIdempotency|MentorInteractionBudgetAndStaleApproval|MentorPrivacyBarrierClearsSavedAndInFlightTemporary|ReferencesStartWithoutSearchAndKeepHistory|SourceCatalogPermissionsPaginationAndReceipts|MentorCookieHTTPAndSSERecovery|ReferencesCookieImportCollectionAndScope)' -count=1 -v`。新增必需用例前缀为 `TestPostgreSQLSourceSelection`，覆盖同名集合、实际模型请求／工具不含未选正文与标题、同范围历史与改选隔离、空选择、跨区伪造、历史版本续跑／幂等重试、旧历史隔离、冻结课堂不变、清除在途竞态及轮换后的历史 citation。交由批次 4 将其纳入 candidate 必需用例检查。

浏览器先构建发行二进制和 `-tags web_release ./testutil/webnotebook` 来源夹具，使用 `WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0 /tmp/notebook-b2c-webdatabase npm run test:browser -- ... --project=chromium`；来源三场景另设 `WEB_NOTEBOOK_FIXTURE_BINARY=/tmp/notebook-b2c-webnotebook`。新增确定性模型响应从实际请求中的已核验来源构造引用，浏览器操作走真实导入、conversation、run 与 citation HTTP，无前端假输出。日志及失败定位文件位于 `/tmp/notebook-b2c-*`。开发中修复了跨模型请求持有 owner 读锁会阻碍隐私屏障的问题；清除竞态测试验证修复。旧来源测试的“没有勾选”断言更新为实际选择行为。

**批次 3 约定。** Studio 使用同一前端当前选择，只在发起新生成时提交 SourceSelection；运行受理时调用同一 owner 固化规则，保存 SourceManifest 和核验 citations，而不是保存集合 head 或目录缓存。重试和生成后的读取不得重采当前勾选，空选择始终显式。产物持久范围／引用读取需沿 G4–G6 另行提供正式承载，不能把临时 run 当作产物库；可复用片段核验和来源查看器，不能信任模型或前端拼接引用。笔记转来源仍走 G1 的确认导入与 `derived_from` 合同。

**决策与限制。** 无需新增用户语义决策。单轮最多 100 个确切文档；片段沿 G1 每段最多 4096 UTF-8 字节，截断标记和上下文预算如实保留，模型不能引用未提供范围。未使用真实外部模型或付费搜索，未跑三引擎、Go 全量、race、Compose；不宣称这些检查通过。已展示正文的撤权感知受前端重新核验周期影响，服务器每次读取与外发均执行权限门禁。CLI／agentcore 协议、课堂冻结范围及原 SSRF／CSRF 校验不变。

### 批次 3：四类 Studio 产物形成可保存的真实流程（12–18 人日）

**结果：** 用户能生成并重开指南、简报与测验，保存笔记及转来源；正式测验通过既有学习领域记录作答和评估。

**范围：** 父验收 5、10–12、14–25；落实 G4、G5、G6。D2、D3、D4 以及批次 2 的选择／引用合同是前置。按以下三个**串行可运行切片**推进，不能只提交通用表／类型却无入口：3a 保存／编辑／重开笔记与转来源；3b 有引用的指南／简报生成及恢复；3c 生成测验、正式作答和进度读回。

**涉及前端／测试：** 新增 `components/notebook/{studio-panel,studio-artifact-viewer,studio-document,studio-note-editor,studio-quiz}.tsx`、`api/studio.ts`，复用 `content-blocks.tsx`、`content-tools.tsx` 的安全呈现／版本展示与正式答案组件；必要改 `studio-page.tsx` 加工作区入口而保留旧 library。新增 `notebook-studio.spec.ts`、`api/studio.test.ts`，扩展工作区／草稿与确定性模型 fixture。

**后端单列：** 建议在 `server/internal/learningcontent/` 新增 `studio.go`、`studio_store.go`、`studio_sources.go`，旧 artifact 合同不变；新增 `mentorrun/studio.go` 并修改 types／worker／store／隐私与任务快照所需路径，新增 `httpapi/studio.go`；测验编排建议放 `server/internal/learningstart/studio_quiz.go`，只经 learning／tutoring 的正式事务与 proposal 验证端口，不跨模块裸写表。应用 `composition.go`／`workers.go` 注入与运行；learningcontent 的清除／验证／轮换接入新产物；G6 连接 knowledge 导入及 G1 metadata。新增 Studio migration（包含 artifact／revision／operations／quiz mapping 与 run kind），更新 OpenAPI、生成类型、api／httpapi 合同测试。

清除落点包括 `learningcontent/store.go` 的 `RedactTx`／`Remaining` 与 `rotation.go`，由现有 `learning/postgresstore/privacy.go` owner 调用；新增表不能只在新 handler 里屏蔽。拟新增 `learningcontent/studio_test.go`／`studio_store_test.go`、`learningstart/studio_quiz_test.go`、`mentorrun/studio_test.go`、`httpapi/studio_test.go` 和 `server/api/openapi_studio_test.go`，并补 migration 升级与知识导入派生关系测试。

**风险：** 高，是本改造最大工作量。新独立正文、目标可空、加密／清除、生成租约与正式多题活动映射同时涉及；不能用降低原表 NOT NULL、复用占位活动或前端评分缩短实现。若 3c 发现必须引入新评分／教学状态语义，返回 Shape 独立拆 child。

**执行步骤：**

- [x] 3a 从保存笔记的 HTTP→持久化→列表／重开场景做起，同时覆盖 409 保留草稿、隐私屏障和版本到来源 preview／confirm；给后续生成建立真实产物读取路径。
- [x] 3b 增受限生成任务和引用验证，产物 commit 与运行成功保持一致；补充取消／重启／partial／unknown／预算场景，学习指南与简报有各自结果结构，不只是不同标题。
- [x] 3c 生成有效题集后明确开始正式承载；逐题真实 Activity／Attempt／Assessment，actual help 与 allowed_help 一致；从进度／反馈读取相同记录，原课堂和未提交答案继续可恢复。
- [x] 扩展跨 owner 隐私 scrub／verify、加密轮换与 replay；旧正文库、旧 mentor／CLI 请求不受新 enum／必填字段影响。交接新状态／错误映射和门禁必需测试名。

**延期：** 音频／视频、思维导图、闪卡、自动笔记、实时双向来源同步始终非目标；全引擎和全量视觉收尾到批次 4。

**退出标准：** 前端类型、Studio／草稿／答案／citation 局部单测与构建；Chromium 四产物端到端、重启重开、原课堂共存、丢响应一次作答与来源定位通过。后端 `go test` 覆盖 api、learningcontent、mentorrun、learningstart、learning、tutoring、knowledge 及 httpapi 的受影响路径；真实 PostgreSQL 覆盖新 migration 升级、版本并发、幂等、run 取消／发布竞争、来源撤权／全局清除不可复活、正式评估／进度一致性；OpenAPI 生成一致性、相关旧请求兼容通过。不用只依赖 fake UI 的测试证明正式记分。

**重型证据：** 全量重建／完整历史 DB 矩阵及全浏览器延到稳定候选；新评分桥接所触及的重放／证据一致性、清除和迁移在本批必须提供真实证据。三种切片各有运行入口和局部退出记录，不能到批末才发现底座无法生成产物。

#### 3a 实施记录（2026-10-12）

**结果与范围。** Studio 笔记入口支持无目标新建、明确保存、区内列表、重开、编辑与版本冲突比较；学习指南／测验／简报继续显示“尚未交付”。草稿、未知保存请求及导入预览沿现有 DraftStore 留在标签页内存；切面板／切区后可恢复，刷新前沿原 beforeunload 提醒，不写浏览器持久存储。409 保留草稿，用户读取并比较服务端新版后才能以新基础版本保存；丢响应时先核对原 operation，不重新编号重复创建。读取撤权后隐藏列表／编辑器，清除当前区笔记草稿和操作身份；身份／隐私代次切换沿 SessionProvider 清除全部缓存与草稿。实现计划见 [3a 计划](notebook-studio-3a-plan.md)。

**G4 公共合同。** 所有新路径显式要求 `X-Learning-Space-ID`，返回 `no-store`，保留原配对／Cookie／Origin／CSRF 门禁。手动 note 不需要模型、目标、来源读取权限，也不从当前勾选伪造引用。`goal_id` 可为 null；有值时必须属于本区，保存后不能借编辑笔记改绑目标。

| 接口 | DTO 与语义 |
| --- | --- |
| `GET /v1/learning/studio/artifacts` | `learning:read`；`limit` 默认 30、1–100；opaque `cursor` 绑定学习区，按稳定 artifact UUID 分页。返回 `items[{artifact_id,goal_id,kind,version,title,updated_at}]` 和可选 `next_cursor`，不伪造总量。 |
| `POST /v1/learning/studio/artifacts` | `learning:write`；闭合 `StudioSave`：`operation_id,expected_version,privacy_generation,kind,goal_id,title,markdown` 全部明确提供，仅 goal 可为 null；创建 expected_version=0，kind 仅 note。标题非空、单行、最多 200 Unicode 字符；正文非空、有效 UTF-8、无 NUL、最多 64000 UTF-8 字节。继承 HTTP 1 MiB 上限与重复 JSON 键拒绝。 |
| `GET /v1/learning/studio/artifacts/{artifactID}` | `learning:read`；可选 `version`，未提供读最新已保存版本，显式值必须大于零。返回 `StudioRevision{artifact_id,learning_space_id,goal_id,kind,version,privacy_generation,actor_device_id,created_at,body}`；不借用课堂 artifact／session／activity。 |
| `PUT /v1/learning/studio/artifacts/{artifactID}` | `learning:write`；同一 StudioSave，expected_version 必须为当前版本。原子追加 revision 并更新 latest_version，绝不原地改正文。创建／保存统一 200，成功响应就是已提交版本。 |
| `GET /v1/learning/studio/artifacts/{artifactID}/revisions` | `learning:read`；返回最近 100 个版本的 `version,status=committed,created_at`，倒序。历史正文通过 GET 的 version 参数读取，不把摘要当正文。 |
| `GET /v1/learning/studio/operations/{operationID}` | `learning:read`；限定当前设备／学习区，返回原成功写入的 StudioRevision，即使后续已另存新版也返回原版本。此批只有同步 note 写入，没有假 queued／running 状态。 |

`body` schema 1 包含 title／markdown、空 citations；为 3b 预留 `source_manifest`（复用 G3 SourceManifest）、`citations`（同 G3 核验引用形状）、`model`、`input_fingerprint`。这些字段和标题正文一起 AES-GCM 加密，公开 note 写入 DTO 不允许提交它们。读取发现非 note、未知 body schema 或提前写入的生成来源字段时拒绝读取，不能未经来源验证开放未来类型。

错误沿正文 owner 映射：400 `invalid_learning_content`（非法／缺失／null 字段、非法分页版本或未交付 kind）；403 `learning_content_forbidden` 或原 scope 拒绝；404 `learning_content_not_found`（不存在、跨区、无对应版本／本设备操作）；409 `learning_content_conflict`（期望版本或同操作不同载荷冲突）。缺加密密钥为 503 `learning_content_key_unavailable`，未知正文协议为 422 `learning_content_upgrade_required`。隐私屏障沿原 `content_redacted`／`privacy_clear_in_progress`，归档区写入沿 `learning_space_archived`；未新建冲突或权限旁路。

**存储、版本和清除。** 只追加 **`server/migrations/000035_studio.sql`**，创建 `learning_studio_artifacts`、`learning_studio_revisions`、`learning_studio_operations`。artifact 只存身份、可空 goal、kind、隐私代次和版本指针；revision 存认证密文、设备与时间；operation 存设备／操作主键、原结果版本与请求 hash。数据库允许未来四种 kind，当前服务和 Web 合同只开放 note；run kind／quiz mapping 留给后续追加迁移。旧 `learning_content_artifacts` 的 session／activity NOT NULL 与唯一约束不变。

同设备操作先取事务 advisory lock，hash 绑定学习区、目标 artifact 和完整请求；同操作同载荷读回原版本，不同载荷 409。不同操作编辑同 artifact 通过行锁与 expected_version CAS 只允许一个成功。每次写入重验实时 token／设备、learning 隐私 owner 代次及区归档状态；请求显式携带的旧 privacy_generation 在清除完成后仍不能创建新副本。三个新表均有 learning owner 写入 trigger；revision／operation 不可变。`learningcontent/store.go` 的 `RedactTx` 按 operation→revision→artifact 删除，`Remaining` 计入三表，由原 `learning/postgresstore/privacy.go` owner scrub／verify 调用。`rotation.go` 对新 revision 加独占锁并按 Studio AAD 重封装；AAD 绑定 artifact／版本／代次／区／goal／kind／设备，密钥轮换不改身份、内容或版本。

**G6 快照导入合同。** `POST /v1/learning/studio/artifacts/{artifactID}/import-candidate` 请求闭合 `{version}`，要求 `learning:read + knowledge:read + knowledge:write`；版本必须已保存且属于当前区。HTTP 在 learning／knowledge response read permit 下通过 `StudioGet` 正式读取端口取得原笔记，再经 knowledge `PrepareNoteSource` 签发候选；没有跨 owner 裸写表。该端口先读取 knowledge 的来源代次，再读取笔记，避免清除之间把旧正文签入新代次；此阶段无需已选集合。

候选 DTO 为 `{derived_from:{artifact_id,version},title,sha256,document:{path,markdown,source_receipt}}`，SHA-256 绑定原保存正文；路径默认 `note-{artifactID}-v{version}.md`。回执沿 G1 签名绑定确切正文 hash、区、knowledge 代次与服务端 provenance。前端使用原 `ReferenceImport` 控制器选择／创建集合、显示原文、预览差异、审阅身份并确认；确认仍要求 `knowledge:approve`，复用原 operation 查回执和 unknown 恢复。客户端不能自报 derived_from 或修改已签正文；路径变更与身份选择仍按原导入规则重新预览。

确认后 G1 metadata 为 `kind=text,coverage=user_note_snapshot,derived_from={artifact_id,version}`；明确显示个人理解、不是外部权威。副本独立归 knowledge owner，后续编辑笔记不修改副本文档／集合 head／课堂冻结资料，不自动增加左栏选择。不同版本需重新获取候选、预览和确认。全局清除删除 Studio 正文及 knowledge 副本／provenance，旧候选回执因代次改变失效；进程重启后未确认候选按 G1 规则重新读取，未知提交先查原 import operation。

**文件映射。**

- 新增服务端：`learningcontent/{studio,studio_store}.go`；`knowledge/note_source.go`；`transport/httpapi/studio.go`；上述 Studio migration。
- 新增测试：`api/openapi_studio_test.go`、`migrations/studio_test.go`、`knowledge/note_source_test.go`、`httpapi/{studio_test,studio_integration_test}.go`。真实存储／轮换／全局清除测试通过 HTTP 集成夹具执行，不用 mock 替代数据库。
- 修改服务端：`learningcontent/{store,rotation}.go`；`httpapi/{api,learning_space}.go`；**OpenAPI `server/api/openapi.yaml`**、既有代理合同测试与 `deploy/web/nginx.conf`，新 Studio 前缀列入区路由分类及代理白名单，集合头仍只接受原 knowledge 路由。
- 新增 Web：`src/api/studio.ts`、`studio.test.ts`、`components/notebook/studio-note-editor.tsx`、`tests/browser/notebook-studio.spec.ts`。
- 修改 Web：生成 `api/schema.d.ts`，`api/notebook-sources.ts` 派生说明，Studio 面板、工作区、样式；原 AddSourceContent／ReferenceImport 增加服务端候选入口；DraftStore 增加范围清除及其回归，工作区旧“尚未交付”计数由四项更新为三项，保留原业务断言。

**检查结果。** 所有 Go／数据库／浏览器命令先 `umask 022`；Go 缓存使用 `/tmp/notebook-b2b-go-cache`。真实数据库为用户提供的 `127.0.0.1:32995/edu_agent_test`；数据库与浏览器严格串行，隔离 schema，不操作 Docker。`/home/yunyue/.cache` 不可写，故 `webdatabase` 等价构建到 `/tmp/notebook-b3a-webdatabase`。

| 检查 | 命令与结果 |
| --- | --- |
| OpenAPI | `cd clients/web && npm run generate` 通过；既有 ActionExposureRequest discriminator 提示保留。`TestStudioContract`、`TestWorkspaceProxyAllowlist` 通过。 |
| 前端 | `npm run check`、`npm run build -- --configLoader native` 通过；最终构建自带类型检查。仅既有 Zod 注释与大 chunk 提示；日志 `/tmp/notebook-b3a-build-final.log`。 |
| Vitest | `npm test -- --configLoader native src/api/studio.test.ts src/api/notebook-sources.test.ts src/lib/reference-import.test.ts src/lib/drafts.test.ts` 13 项通过；新增撤权范围清除后只定向重跑 `src/lib/drafts.test.ts`，4 项通过，合计 14 个不同用例。 |
| Go 局部包 | `go test ./api ./internal/learningcontent/... ./internal/learning/... ./internal/knowledge/... ./internal/transport/httpapi` 与相同包 `go vet` 通过。后续闭合 DTO／G6 修正只重跑 `./internal/knowledge ./internal/transport/httpapi ./api`，测试与 vet 通过。未配置 DB 的跳过不算数据库证据；日志 `/tmp/notebook-b3a-go-tests.log`、`-go-updated.log`、`-vet.log`、`-vet-updated.log`。 |
| 真实 PostgreSQL | `TEST_DATABASE_URL=… go test -p=1 ./migrations ./internal/transport/httpapi -run '^(TestStudioMigrationUpgrade\|TestPostgreSQLStudio)' -count=1 -v`：34→35 升级、迁移重放和旧约束通过；集成夹具修正及 G6 修正后定向 `-run '^TestPostgreSQLStudio'` 通过（3.46s）。覆盖创建重放／不同载荷冲突、真实并发 409、历史／操作读取、快照 preview/confirm／G1 派生关系、跨区隔离、密文与不可变、轮换读回、全局屏障／scrub／旧请求不可复活。最终日志 `/tmp/notebook-b3a-db-final.log`。 |
| Chromium | `WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0 /tmp/notebook-b3a-webdatabase npm run test:browser -- 'tests/browser/notebook-studio.spec.ts$' 'tests/browser/notebook-workspace.spec.ts$' --project=chromium --reporter=line`：11 项通过（1.3m），Studio 3＋工作区 8，无 skip／retry。日志 `/tmp/notebook-b3a-browser-studio-workspace-final.log`。来源 3 项使用 `WEB_NOTEBOOK_FIXTURE_BINARY=/tmp/notebook-b3a-webnotebook` 完成，复用下述部分轮次的逐项成功记录。 |
| 差异检查 | `git diff --check` 通过；没有暂存、提交或推送。 |

浏览器环境使用指定 `TEST_DATABASE_URL` 与 `WEB_CHROMIUM_PATH=/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome`。先构建 `go build -tags web_release -o edu-agentd ./cmd/edu-agentd`、`go build -o /tmp/notebook-b3a-webdatabase ./testutil/webdatabase`、`go build -tags web_release -o /tmp/notebook-b3a-webnotebook ./testutil/webnotebook`。Studio 三场景分别验证 CSRF／读取撤权清草稿；保存→刷新重开→真实并发 409→比较保留草稿→指定 v1 预览确认→查看来源→再编辑不改变副本；双标签／切区草稿隔离及保存丢响应后原操作核对。

初次最终组合误给三个文件都设置来源专用宿主；`notebook-sources` 的三场景已完成且无失败，但该宿主未装配 learningcontent／课堂，随后 Studio 遇到 404。已中断误配轮次，没有为测试扩展专用宿主；保留 `/tmp/notebook-b3a-browser-final.log` 中来源三项的成功证据，Studio／工作区改用普通发行二进制，得到上面的 11 项完整通过。合计 14 个不同 Chromium 场景有通过记录；误配命令整体退出 130，不称为整轮成功，已通过来源场景未重复执行。

开发先形成缺路由、缺 DTO、缺 OpenAPI 和草稿范围清除的失败检查；浏览器捕获新空学习区候选错误依赖默认集合，已改用 knowledge SourceGeneration；原集合预览／确认权限未放宽。更新后的失败用例均做定向验证，原日志保留，不把失败轮次描述为全部通过。

**3b／3c 接口衔接。** 复用本批 artifact 列表／指定版本读取、不可变 revision、操作身份与清除／轮换承载；新增生成发布正式事务端口时，必须由 learningcontent owner 验证并写入 SourceManifest／citations／模型和输入指纹，和运行成功原子发布。G3 来源清单冻结、精确历史读取与逐次权限／hash 核验继续适用；在这些校验交付前，不扩展本批 note DTO，不开放 `sources/{citationID}` 或生成端点。3b 再追加受限 run kind、预算／租约／取消／恢复；3c 再追加 quiz 映射并调用现有学习领域正式 Activity／Attempt／Assessment，不能降低旧正文约束或新建评分事实。

**决策与限制。** 无新增待用户决策的语义问题。版本菜单展示最近 100 版，API 仍可读取／转换任意存在的指定版本；列表按稳定 UUID 分页。草稿及未确认候选不承诺跨刷新／重启恢复，已保存笔记可重开。撤权可见性由前端 5 秒重新读取及原会话刷新推进，服务器每次读取和写入实时核验。3b／3c、真实外部模型／搜索、三引擎矩阵、Go 全量／race、Compose 和候选发布门禁未执行；本批不宣称完整批次 3 或父 spec 已验收。

#### 3b 实施记录（2026-10-12）

**结果与范围。** 指南与简报通过右栏发起真实、加密持久的 Studio 运行，可取消、核对原操作、重开产物和重新生成新版本。复用 D1 当前来源选择与 D4 可空目标；零来源生成拒绝。测验仍显示“尚未交付”，没有正式作答／评分新路径。计划见 [3b 计划](notebook-studio-3b-plan.md)。

**生成与读取合同。** 新增 `POST /v1/learning/studio/generations`，闭合 DTO：`operation_id,privacy_generation,kind,goal_id,goal_version,artifact_id,expected_version,source_selection,request_budget,token_budget` 全部明确提供。kind 仅 guide／brief；goal_id 可 null，此时 goal_version=0；新产物 artifact_id=null、expected_version=0，重新生成指定现有 ID 与当前版本，目标／类型不能改绑；目标与产物身份要求规范小写、非零 UUID，以保持加密 AAD 与数据库身份一致。所有路径要求明确学习区、no-store，并保留 scope、Cookie、Origin／CSRF、隐私和归属门禁。生成需要 learning:write 与实时 knowledge:read；无模型、无来源、无写权限分别显示原因。

受理返回 202 `MentorReceipt`。`GET /v1/learning/operations/{operationID}` 核对受理结果；同操作同载荷返回原运行，不能跟随后来勾选。`GET /v1/learning/runs?task_kind=studio` 恢复当前设备区内生成记录，快照／取消／预算继续沿原 runs API。客户端对结果未知的受理保留原 DTO／操作身份；重新发送仍是原请求。取消前重读版本；只有明确 version_conflict 才有限次重读并重发同一停止意图，网络未知不自动换操作。

产物沿 3a 列表、指定版本读取和 revisions 接口。新增 `GET /v1/learning/studio/artifacts/{artifactID}/sources/{citationID}?version=N`，版本必填；只允许读取该已保存版本实际持有的引用。列表、正文、历史入口、操作读取和引用读取均重新校验完整 manifest 及历史来源，撤权／解除关联／清除不能退回缓存或最新来源。note DTO 仍只允许手动笔记；不能用 PUT note 覆盖生成产物，生成产物也不能通过 note import-candidate 转来源。

**两种正文。** `StudioBody` schema 1 保留 note 的 title／markdown 和既有加密字段；生成时 markdown 为空，新增 coverage 与互斥的 guide／brief。指南为 `guide{key_points,terms,learning_order,misconceptions}`；简报为 `brief{overview,differences,gaps}`，以一页式综述为提示目标。各组均为 `[{text,citations:[id]}]`，1–8 项，每条 1–10 个核验引用，单条最多 1500 字符。指南学习顺序明确标为“生成建议”。模型只能返回服务器提供的 source／fragment 数组序号；owner 去重建立 1 起始引用 ID，固定文档身份、node 版本、字节／行范围、正文与双 hash。每条内容必须有引用；引用证明出处／原片段一致性，不宣称机器已证明论断正确。

模型给出的 coverage 仅允许 complete／partial；任何已知部分解析、空片段来源或截断片段都强制 partial，模型不能升级为完整。有效 partial 仍作为可读取的版本发布，运行同时为 partial；截断的模型输出、未完成响应和不合 schema 的 JSON 一律不发布。生成只发送选中来源的有界核验片段／元数据和输出 schema，不发送目标正文、课堂、记忆、聊天历史或未选文档；目标只作权限与归属绑定。输入指纹绑定生成命令和冻结 manifest，模型名称另存；这些内容全部随正文加密。

**任务、事务与错误。** migration **000036_studio_generation.sql** 只扩展 mentor session kind 为 studio；复用 000035 的 artifact／revision／operation 与 mentor 加密 checkpoint，不改旧课堂 artifact 的 NOT NULL／唯一约束，不新增隐私 owner。学习内容 owner 在 `StudioPublishTx` 中再次校验目标版本和来源；追加 revision／关联 operation 与 run 的 succeeded／partial 结算同一事务，取消／失败不会留下孤立产物。

Studio 复用现有预算 HTTP transport、租约、并发、取消和跨 owner ReadPermit。只走服务端 `settings.StudioClient` → `llm.Client`，显式复用教学连接时尊重其 api_style。Responses 使用 json_schema strict；Chat 使用 json_object，并保留相同的服务端 schema、闭合 JSON、重复键、正文限制和引用核验。`RequireCompletion` 仅由 Studio 显式启用，原 mentor／CLI 的 agentcore 协议和默认行为未改。

| 状态／分类 | 语义 |
| --- | --- |
| queued／running | 原持久排队和租约运行；无模拟进度。 |
| paused_budget／budget_exhausted | 预留预算不足，不发模型请求；可明确补充原运行预算。 |
| succeeded／studio_committed | 完整、核验通过的产物版本与运行一起提交。 |
| partial／source_coverage_partial | 有效结构与引用已保存，但资料覆盖不完整。 |
| failed／output_budget_exceeded | Chat length 或 Responses max_output_tokens；不得发布截断 JSON。 |
| failed／response_not_completed、schema_mismatch、invalid_source_citation | 未完成、结构错误、非法引用分别记录；不降级为成功。 |
| cancelled／user_stopped 或 context_changed | 明确取消、权限／上下文变化；迟到内容不能发布。 |
| failed／lease_expired + result_unknown | 重启发现已发出请求但无确认结果；不自动重复付费请求。cost_unknown 独立表达费用未核实。 |
| failed／studio_version_conflict | 生成期间产物基版失效，旧历史不覆盖。 |

日志只记录 run_id、kind、stage 和固定 error_category，不打印请求、来源、原始错误、响应、推理或凭据。已接收的失败响应与网络未知分开结算。运行 checkpoint 沿原七天期限，产物 revision 独立持久；过期运行状态可见，保存产物从列表重开。现有 learning owner 的 scrub／Remaining 与密钥轮换自动覆盖新生成正文，新增 DB 场景验证了在途清除与历史轮换。

**文件映射。**

- 新增：`server/internal/learningcontent/{studio_generation,studio_publish,studio_generation_test}.go`；`server/internal/mentorrun/{studio,studio_test,studio_recovery_test,studio_privacy_test}.go`；`server/internal/settings/{studio,studio_test}.go`；`server/internal/integrations/llm/studio_test.go`；`server/internal/transport/httpapi/studio_generation_test.go`；`server/api/openapi_studio_generation_test.go`。
- 新增前端：`clients/web/src/components/notebook/{studio-generation,studio-document}.tsx`；新增本实施计划；**新增 migration 000036**。
- 修改服务端：`learningcontent/{store,studio,studio_store}.go`、`mentorrun/{types,store,query,worker}.go`、`integrations/llm/client.go`、`transport/httpapi/studio.go`、`app/app.go`。
- **OpenAPI：** `server/api/openapi.yaml`；已执行 `npm run generate` 更新 `clients/web/src/api/schema.d.ts`。新路径由 studioPath 归类，合同测试确认不走 collection 选择；`deploy/web/nginx.conf` 明确记录既有 studio 白名单覆盖生成和引用。
- 修改前端：`api/{studio,studio.test,mentor,tasks}.ts`、`components/notebook/{studio-panel,source-selection,source-viewer,sources-panel,notebook-layout}.tsx`、`notebook-workspace-page.tsx`、`tasks-page.tsx`；`scripts/test-server.mjs` 添加真实 HTTP 确定性生成夹具；`tests/browser/{notebook-studio,notebook-workspace}.spec.ts` 扩展／迁移验收。

**检查结果。** 所有 Go／DB／浏览器命令先 `umask 022`，GOCACHE 为 `/tmp/notebook-b3b-go-cache`。DB 为指定 32995 PostgreSQL，独立 schema、严格串行，无 Docker 操作。沙箱不可写用户 cache，因此包装器等价编译到 `/tmp/notebook-b3b-webdatabase`，来源宿主为 `/tmp/notebook-b3b-webnotebook`。发行二进制必须在前端构建及资产复制完成后再编译。

| 检查 | 已执行结果 |
| --- | --- |
| OpenAPI／前端 | `npm run generate`、`npm run check`、`npm run build -- --configLoader native` 通过。仅既有 discriminator、Zod 注释和大 chunk 提示。最终构建含当前类型检查，日志 `/tmp/notebook-b3b-build-final.log`。 |
| Vitest | `npm test -- --configLoader native src/api/studio.test.ts src/api/source-selection.test.ts src/api/mentor.test.ts`：3 文件、9 项通过，日志 `/tmp/notebook-b3b-vitest.log`。 |
| Go／vet | `go test ./api ./internal/learningcontent/... ./internal/mentorrun ./internal/integrations/llm ./internal/transport/httpapi ./internal/settings` 通过；对应 `go vet` 通过。后续分类／覆盖校验改动定向复测 learningcontent、mentorrun 和 vet 通过。无 DB 环境时 skip 不计为 DB 证据。日志 `/tmp/notebook-b3b-go*.log`、`-vet*.log`。 |
| PostgreSQL | `go test -p=1 ./internal/mentorrun -run '^TestPostgreSQLStudioGeneration…' -count=1 -v` 已验证发布／幂等、未选数据不外发、partial／截断／未完成／schema／非法引用、预算、queued 恢复／unknown 不重发、取消／事务回滚、版本／轮换／撤权、全局清除。各次运行只补新增或失效场景，日志 `/tmp/notebook-b3b-db-*.log`。 |
| Chromium 新增流程 | 指定 Chromium 路径，`WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0 /tmp/notebook-b3b-webdatabase npm run test:browser -- 'tests/browser/notebook-studio.spec.ts$' --project=chromium --grep '所选来源生成指南' --reporter=line`：1 项通过，20.1s，无 retry／skip；日志 `/tmp/notebook-b3b-browser-generation-current-assets.log`。 |
| Chromium 既有回归 | 相同环境执行 Studio／workspace 两文件，`--grep-invert '所选来源生成指南'`：11 项通过，1.3m；笔记 3 项、工作区 8 项，无 retry／skip。日志 `/tmp/notebook-b3b-browser-regression.log`。 |
| Chromium 来源回归 | 同一 schema 包装器、指定 Chromium，并设置 `WEB_NOTEBOOK_FIXTURE_BINARY=/tmp/notebook-b3b-webnotebook`，执行 `notebook-sources.spec.ts`：3 项通过，14.7s，无 retry／skip。日志 `/tmp/notebook-b3b-browser-sources.log`。共 15 个不同 Chromium 场景通过。 |

最后定向数据库命令：`TEST_DATABASE_URL=… go test -p=1 ./migrations ./internal/mentorrun ./internal/transport/httpapi -run '^(TestStudioMigrationUpgrade|TestPostgreSQLStudioGenerationVersionsRotationAndRevocation|TestPostgreSQLStudioVersionsSnapshotRotationAndErasure)$' -count=1 -v`：全部通过（1.49s／3.33s／3.60s）。覆盖迁移重放／旧约束、生成历史轮换与撤权后不再外发或发布、3a 笔记／清除回归；日志 `/tmp/notebook-b3b-db-final.log`。生成类共 6 个不同顶层 DB 场景；加升级与 3a 回归共 8 个。首次发布／选择隔离场景通过（3.15s）的记录在执行输出中，未为补日志重复运行。最后任务中心无目标 Studio 预算入口修正由构建内类型检查及已有无目标预算 DB 场景覆盖，未重复无关浏览器场景。最终加密身份核对发现大小写 UUID 可能造成 AAD 不一致，新增红测试后严格拒绝非规范身份；`TestStudioTargetRejectsNonCanonicalEncryptedIdentity` 与 `TestStudioStructuredClaims`、learningcontent vet 通过，合法身份路径复用既有 DB／浏览器证据。

开发失败记录保留：缺新类型／路由／校验的红测试；浏览器先暴露未启用模型、settings 档案无来源权限的夹具遗漏，之后捕获取消版本过期。取消修复后一次并发构建嵌入了旧前端，改为依赖顺序构建后完整通过。失败轮次不计通过；没有放宽权限、版本或引用门禁。

**真实模型 smoke（必须项已完成）。** 从指定 acceptance 环境文件加载凭据，只调用 2 次，无额外探测／自动重试。生产 `StudioModelRequest` → `llm.Client.Chat` → `DecodeStudioCandidate` → `VerifyStudioSources`，使用公开自编的两份奇偶数片段；Responses、deepseek-v4-flash、输出上限 8192、上下文 32768，smoke 请求超时上限 120 秒（两次实际均低于 16 秒）。临时 overlay 测试／日志均在 `/tmp/notebook-b3b-smoke*`，不含 Key、正文或隐藏推理。服务端配置注入另由 `TestStudioUsesSharedResponsesAndConfiguredBudget` 验证：共享教学 Responses、8192 原样传入，旧 mentor 保持 Chat。

| 产物 | 延迟 | input | output（含推理） | reasoning | total | 校验 |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| guide | 15467 ms | 2102 | 2750 | 1961 | 4852 | strict + 来源引用通过，coverage=partial，2 个引用 |
| brief | 15224 ms | 1809 | 2632 | 1890 | 4441 | strict + 来源引用通过，coverage=partial，2 个引用 |

**3c 约定与限制。** quiz 仍未开放；后续需独立 schema 和正式 Activity／Attempt／Assessment 映射，不能复用本批生成建议作为评分事实。没有新增待用户决定的语义问题。历史菜单沿 3a 展示最近 100 版；每次最多 100 个文档，有界片段和上下文预算如实限制。真实 smoke 验证生产模型协议／结构／引用，不代替真实外部模型的全套浏览器／重启验收。未跑三引擎、Go 全量、race、Compose 或部署，不宣称父 spec／整个批次 3 已完成。

#### 3c 实施记录（2026-10-12）

**结果与边界。** G5 采用已批准方案 A：一个测验的每道题各有专用单题正式会话，同属用户明确选择的目标。解决原路线不允许重复知识节点、确认最后一步反馈即结束会话的约束；不新增评分、教学状态或进度事实。笔记、指南、简报、测验四类入口均已可用。实施计划见 [3c 计划](notebook-studio-3c-plan.md)。

**生成合同。** `POST /v1/learning/studio/generations` 的 kind 增加 quiz，必须提供非空 goal_id 和精确 goal_version；无目标时前端提示先选择或建立目标，禁用生成，后端拒绝，不创建占位目标。受理后冻结 D1 来源；quiz 向模型发送选中来源的核验片段和所选目标正文，目标作为数据单独传入，不接受其中更改输出协议的指令。指南／简报的模型输入保持原状。来源、目标版本、生成命令绑定输入指纹；目标版本由服务端写入加密正文，不由模型提供。

模型输出 title／coverage／questions，默认提示生成 3 道开放文字题，合同允许 1–8 题。每题必有 prompt（1–1500 字符）、rubric（1–10 项，唯一 id 与非空 criterion）、allowed_help（非空、去重、仅 none／hint／scaffold／answer_revealed）和 1–10 个真实引用。闭合 JSON、重复键、字段限制、来源序号、片段范围与双 hash 校验沿 3b；不合格不发布，已知来源覆盖缺口仍强制 partial。Responses／RequireCompletion、8192 输出预算和包含推理的用量结算沿 Studio 客户端；截断／未完成明确失败。保存的 `body.quiz={goal_version,questions}` 沿原 artifact/revisions 读取与重开，新版本独立于旧版本的正式记录。

**开始、映射与读回合同。** 新增三条路径，均要求明确学习区，并保留原 scope、Origin／CSRF、隐私、归属和 response ReadPermit：

| 路径 | 合同 |
| --- | --- |
| `GET /v1/learning/studio/artifacts/{artifactID}/quiz?version=N` | 必填版本；核验保存来源后，读取该版本真实会话状态、完成标志和 Attempt ID。不触发创建／模型。 |
| `POST /v1/learning/studio/artifacts/{artifactID}/quiz/start` | 闭合 DTO：operation_id／version／privacy_generation／goal_version／question。用户明确开始指定题；首题验证冻结目标版本，后题要求前题真实完成路线。 |
| `GET /v1/learning/studio/quiz-operations/{operationID}` | 仅当前设备、当前区的原操作；丢响应后核对实际映射和状态。 |

追加 **migration 000037_studio_quiz.sql**，新增 `learning_studio_quiz_questions`（artifact/version/question → 唯一 session/activity/content artifact）与 `learning_studio_quiz_operations`（device/operation → 请求 hash 与题目映射）。只存关联和操作身份，不存成绩／完成度／进度。按操作 advisory lock 再锁产物；同操作同载荷重放，同题不同操作复用同一承载，改载荷冲突。每次只创建下一道题；题目、正式会话、正式活动、正文及映射同事务提交。

`learningstart/studio_quiz.go` 使用 knowledge 的正式冻结参考端口、learning 的 CreateSession／Propose／ApplyAction，以及 learningcontent 的 EnsureTx。每题路线只含一个真实知识节点；活动固定使用已保存 prompt／rubric／allowed_help／核验引用，通过既有 proposal 验证，不跨模块裸写学习表。来源上下文仅绑定新专用会话，不改变原目标／课堂参考头。

Web 的 `studio-quiz.tsx` 复用原 TeachingPage：present_activity → content answers → Attempt → assessment proposal／record_assessment → Assessment／Evidence → acknowledge_feedback。实际帮助等级必须显式符合 allowed_help；不允许 none 时初始选择为空，提交禁用，服务器同样拒绝非法帮助。开始未知保留原 DTO／operation；答案未知复用既有原操作核对，禁止换操作静默重复提交。测验完成度仅由各会话 `Completed && CompletedRoute` 汇总；确认后从真实 Feedback 找回原 Attempt。主动终止未完成路线不会计为完成。

进度与会话列表只读添加“测验名 · 第 n 题”标签，来自原正式路线；Evidence、统计与重放仍是原 learning 投影。CurrentSession 排除专用测验会话；原课堂版本、活动、未提交草稿独立保留。明确的会话链接仍可读取这些正式会话。

**错误、隐私与轮换。** 非法 DTO／缺目标／非法题号为 400；产物、目标版本、操作载荷或顺序冲突为 409，保留 learning 的 version_conflict／invalid_transition 分类。无权限／来源撤权按原 403，跨区或不存在为 404；隐私屏障／content_redacted 按原映射。真正的网络未知才保留待核对状态。生成错误分类、取消和未完成不发布沿 3b。正文和来源读取继续实时核验，不回退缓存。映射／操作表有原隐私门禁和不可变触发器，由 learning owner 调用 learningcontent.RedactTx／Remaining 清除并验证。映射不含新密文；题集正文与正式内容复用原密钥轮换，已验证轮换后两题仍可读，清除后旧操作不能复活。

**文件映射。**

- 新增服务端：`learningcontent/{studio_quiz,studio_quiz_store,studio_quiz_test}.go`、`learningstart/studio_quiz.go`、`learning/postgresstore/studio_quiz.go`、`mentorrun/studio_quiz_test.go`、`transport/httpapi/studio_quiz.go`（均在 `server/internal/`）；新增 **migration 000037**。
- 新增前端：`clients/web/src/components/notebook/studio-quiz.tsx`；新增本批实施计划。
- 修改服务端：learningcontent 的生成／发布／读取／清除、mentorrun Studio 请求与测试、learning 会话列表／当前会话／进度、httpapi 注入／路由／合同、app 注入与 migration 升级测试。
- **OpenAPI：** `server/api/openapi.yaml` 与合同测试；`npm run generate` 更新 `clients/web/src/api/schema.d.ts`。新路径由 studioPath 分类，`deploy/web/nginx.conf` 的既有 Studio 白名单覆盖三条路径并补充说明。
- 修改前端：`api/studio.ts` 与测试、Studio panel／generation／document、确定性模型 fixture、notebook-studio／notebook-workspace 浏览器场景。

**检查结果。** 命令先 `umask 022`；Go 使用 `/tmp/notebook-b3c-go-cache`。指定 32995 PostgreSQL 以 `-p=1` 或独立 schema 包装器串行运行。沙箱不可写用户 cache，包装器／来源宿主／复习夹具等价构建到 `/tmp/notebook-b3c-{webdatabase,webnotebook,webreview}`；生产 CLI 仅为既有 review 验收编译到 `/tmp/notebook-b3c-cli`，产品代码未改。Chromium 使用指定 chromium-1247 路径；发行二进制在前端构建嵌入资产后编译。

| 检查 | 结果 |
| --- | --- |
| OpenAPI／Web | `npm run generate`、`npm run check`、`npm run build -- --configLoader native` 通过；仅既有 discriminator、Zod 注释／chunk 提示。日志 `/tmp/notebook-b3c-{generate,check,build}-final.log`。 |
| Vitest | `npm test -- --configLoader native src/api/studio.test.ts src/api/teaching.test.ts src/lib/drafts.test.ts`，3 文件 13 项通过，覆盖完整测验重开解析／引用、帮助与草稿。日志 `/tmp/notebook-b3c-vitest-final.log`。 |
| Go／vet | `cd server && go test ./api ./internal/learningcontent/... ./internal/learningstart/... ./internal/learning/... ./internal/tutoring/... ./internal/mentorrun ./internal/transport/httpapi` 的指定包通过；相同包的 `go vet` 通过。修复 YAML 说明后定向重跑 api/httpapi，其余包保留同状态通过证据。日志 `/tmp/notebook-b3c-go-final.log`、`-go-contract-final.log`、`-vet-final.log`。无 DB 时的 skip 不算 DB 验收。 |
| Migration | `TEST_DATABASE_URL=… go test -p=1 ./migrations -run '^TestStudioQuizMigrationUpgrade$' -count=1 -v`：36→37 与重复 Apply 通过（1.41s）；早期开发日志 `/tmp/notebook-b3c-db-first.log` 同时保留当时尚未修复的正式完成标志失败。 |
| PostgreSQL | `TEST_DATABASE_URL=… go test -p=1 ./internal/mentorrun ./internal/learning/postgresstore ./internal/transport/httpapi -run '^(TestPostgreSQLStudioQuizFormalSessions\|TestPostgreSQLStudioGenerationVersionsRotationAndRevocation\|TestPostgreSQLStudioVersionsSnapshotRotationAndErasure\|TestPostgreSQLProgressScopePaginationAndReplay\|TestPostgreSQLReviewCarrierPreservesSourceAndReplay)$' -count=1 -v`：5 项通过，覆盖开始重放、冲突／顺序、两题正式评估／Evidence、丢响应一次 Attempt、help、原课堂、进度重放、轮换／撤权／清除不可复活。日志 `/tmp/notebook-b3c-db-final.log`。 |
| Chromium Notebook | `WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 WEB_NOTESYNC_FIXTURE=0 /tmp/notebook-b3c-webdatabase npm run test:browser -- 'tests/browser/notebook-(studio\|workspace).spec.ts$' --project=chromium --reporter=line`：13 项通过（2.2m），含四种产物、目标选择、生成来源、逐题帮助、开始与答案丢响应、反馈／进度、原课堂草稿、保存重开；无 retry／skip。日志 `/tmp/notebook-b3c-browser-notebooks.log`。 |
| Chromium 原课堂 | 相同 fixture／schema 包装，执行 `tests/browser/workspace.spec.ts$`：13 项通过（2.2m），含帮助限制、动态进度、旧会话与丢响应、开放评估／证据、历史作废、切区迟到响应、教学变更与内容版本；无 retry／skip。日志 `/tmp/notebook-b3c-browser-workspace.log`。 |
| Chromium 复习 | 相同包装，增加 `WEB_REVIEW_FIXTURE=/tmp/notebook-b3c-webreview EDU_AGENT_INTEGRATION_CLI=/tmp/notebook-b3c-cli`，执行 `tests/browser/review.spec.ts$`：2 项通过（49.7s），包含多区多目标、真实复习承载、生产 CLI 读回及全局清除；无 retry／skip。日志 `/tmp/notebook-b3c-browser-review.log`。 |
| Chromium 来源 | 独立 schema，增加 `WEB_NOTEBOOK_FIXTURE_BINARY=/tmp/notebook-b3c-webnotebook`，执行 `tests/browser/notebook-sources.spec.ts$`：3 项通过（12.7s）；无 retry／skip。日志 `/tmp/notebook-b3c-browser-sources.log`。共 31 个不同 Chromium 场景通过，通过后停止。 |

开发失败记录保留：正式完成标志最初从 authority 而非投影读取，已改为真实 Session.CompletedRoute；浏览器原课堂快照早于 present 提交，已修正取样时机；完整测验解析曾把 goal_version 误当题组，已加红测试并修复；OpenAPI 带逗号说明已加引号，合同重新通过。失败轮次不计通过，没有放宽权限、版本、帮助或来源校验。

**真实模型 smoke。** 指定 acceptance 环境只在 shell 读入，未输出或保存凭据。实际共 2 次调用，未自动重试，低于 8 次上限；最后一次使用生产 `StudioQuizModelRequest`（明确目标＋两份公开自编奇偶数来源）→ Responses `deepseek-v4-flash` → RequireCompletion → DecodeStudioCandidate／VerifyStudioSources。输出上限 8192，耗时 26309 ms；input 1656、output 4686（含 reasoning 4130）、total 6342。闭合 schema、每题 rubric／help 与真实来源引用全部通过，coverage=complete、2 个去重引用。临时 overlay 和只含计量的日志在 `/tmp/notebook-b3c-smoke*`；最终日志 `/tmp/notebook-b3c-smoke-final.log`。不把该 smoke 等同于真实模型浏览器全链验收。

**交给批次 4。** 将 `TestStudioQuizMigrationUpgrade`、`TestPostgreSQLStudioQuizFormalSessions` 及前批 G1–G6 必需场景接入 candidate 检查并验证无 skip／retry；完成移动尺寸、键盘／焦点、可访问性、三引擎矩阵与全套旧入口迁移。真实模型评分全链、服务重启浏览器恢复、完整历史 DB 矩阵和部署候选仍由批次 4 收口。本批未跑 Go 全量／race／Compose，未改 CLI／agentcore，未操作 Docker，未提交或推送。没有新增待用户决定的评分／状态语义。

### 批次 4：移动、可访问性、旧回归与候选发布（4–6 人日）

**结果：** 同一工作区在手机、键盘和三个浏览器中可完成全流程，旧业务页面仍通过回归，候选门禁真实覆盖新增服务端与界面行为。

**范围：** 父验收 26–28 及 1–25 的最终整体验收；不新增产品能力。收尾焦点／软键盘／滚动／主题、未覆盖旧测试迁移与 Candidate 证据。

**涉及文件：** `styles.css`、notebook 布局／mobile tabs／drawer／viewer／输入组件的必要修正；`clients/web/tests/browser/*.spec.ts`、`playwright.config.ts`（仅矩阵所需）、`clients/web/scripts/test-server.mjs`；`scripts/check-web-release.mjs`、`scripts/web-release-results.mjs` 及其测试（仅新增 DB 必需场景／fixture 判定所需）、`server/api/web_release_test.go`／`httpapi/web_release_test.go` 的深链接契约；同步最终设计和验收引用，verification 仍由 Runtime 生成。

**后端改动：** 默认无新增业务／migration；仅修复前三批阻碍本合同验收的问题和补候选 DB 检查接入。若新增用户可见能力或改变权限／所有权，返回 Shape，不借“收尾”扩大范围。

**执行步骤：**

- [ ] 用 390／768／1024／1280／1440px 与浅／深主题验证主场景、modal、长标题／代码／公式；键盘完整走菜单、来源、引用返回、测验与笔记，校验 IME、soft keyboard、安全区与 reduced motion。
- [ ] 在新三栏／modal 上运行现有 axe 标签集与实际颜色对比度检查，保留人工焦点／阅读顺序验证；修复真实问题，不跳过业务断言以消除失败。
- [ ] 对照 14 份旧 spec 完成全部迁移；新增 G1–G6 PostgreSQL 必需用例进入 release 脚本，保持 skip／重试拒绝规则，验证错误 API 与资源不返回 SPA。
- [ ] 冻结候选输入，运行局部发行后完整候选；按父 spec 28 项逐项映射结论，记录 evidence key、输入 hash、环境、日志与人工／外部未验证项，交给独立 Verifier。

**退出标准：** 相关局部检查绿色后执行 `make web-release-check`、具备独立测试数据库的 `make web-release-candidate`；新增 DB 必需场景确实运行、三引擎每份适用 spec 无 skip／重试／预期失败。真实 assets／深链接、离线隔离、CLI 读回兼容有效。最终 brief/spec 与实现一致，未验证范围如实登记，Runtime 依流程验收；人工／外部未通过项不能由自动化 exit 0 覆盖。

**重型证据：** 本批就是重型证据收口点，完整矩阵在稳定候选运行；没有输入变化不重复全扫。若局部修复改变源码输入，先重跑受影响检查，再按 evidence key 判定候选证据哪些失效；不把旧 hash 的完整报告直接当最终报告。

### 交接与总风险

四批估算合计 **28–41 人日**，不是四个前端美化任务；其中第三批占主要不确定性。每批交接须包含实际涉及文件、API／migration 变化、验收映射、运行过的检查与未运行项、后继可依赖的接口及已知限制。Build 执行者获得各自授权后按仓库规则提交；本次文档工作树保持未暂存。

主要风险是 D1–D4 未决造成协议返工、旧课堂控制器抽取损坏正式答案／草稿、选择范围被历史／工具绕过、新 Studio 持久化漏接隐私／轮换、测验错误绕开正式评估，以及全局 CSS／首页文案影响广泛旧浏览器用例。对应控制已分别放入各批最小退出标准；不把这些风险推给一次最终“全量通过”的口头承诺。
