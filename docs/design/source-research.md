# 智能搜集资料：设计

状态：Shape 文档定稿（2026-10-20）；R-A 实现覆盖见 §14（2026-10-21），R-B 尚未实施。对应 [brief](../comet/specs/source-research/brief.md)、[spec](../comet/specs/source-research/spec.md)，依赖 [mentor-web-search 设计](mentor-web-search.md)；交付顺序见 [Supervisor](search-shape.md)。§1–§11 保留设计合同，具体类型、表与接口以 §14 和 OpenAPI 为准。

## 1. 产品流程与已有入口

“添加来源”中的“搜索”替换为“智能搜集”。输入框提示“想找什么资料？可以说明用途、基础和希望覆盖的内容。”点击“开始搜集”即进入规划和检索；不出现“确认计划”按钮，不把需求发送到导师会话。

开始按钮旁显示：**“将根据你的描述搜索并读取网页，进度和结果保存 7 天；你可以随时停止。只有确认导入的资料才会加入来源。”** 展开说明：“搜索词会发送给搜索服务，需求和读到的内容会用于模型整理。已发出的请求可能产生用量，停止无法撤回。”用户点击开始构成本任务的外发动作，无额外同意弹窗。

进度面板按实际工作显示“规划方向 → 搜索与阅读 → 筛选与整理”，同时可见方向列表、当前搜索词、页面标题／域名、读取／失败数和已找到候选数；事件可展开回看，不展示模型推理。固定显示“停止搜集”。关闭面板或 SSE 断线不等于停止，任务继续，来源区显示运行入口；刷新从持久事件和快照恢复。

结果分成“候选资料”和“整理报告”。候选显示标题、网址、格式、入选理由、可用范围及已加入状态，新增项默认全选；报告可展开阅读，另有“将整理报告也加入来源”，默认不选。底部显示“将加入 N 份资料，另含／不含整理报告”，统一“确认导入”。空选时禁用；仅选报告允许导入。成功后可以“再找一些”，并可补充一句要求。

替换文件为 `clients/web/src/components/notebook/source-discovery.tsx`、`clients/web/src/api/source-discovery.ts`，入口在 `add-source-dialog.tsx`。后端替换 `server/internal/transport/httpapi/source_discovery.go` 对 `research.Discover` 的使用；旧 `POST /v1/knowledge/source-discovery` 直接移除，无别名、旧预算字段或旧结果转换。普通 URL／上传入口沿原合同继续使用。

## 2. 运行模型与 #22 的关系

新增 `SourceResearchJob`，归属学习区，不要求目标或导师对话。每轮 `ResearchRound` 使用现有 mentorrun worker 的新 `kind=source_research`，复用租约、准入、取消、操作回执、加密和代次门禁；不另起独立队列服务。新类型绕开旧 `research.State` 的四候选和目标绑定，checkpoint 只存阶段、ID、游标及有界摘要，正文在 M-A 的网络资料表中。

#22 的 `mentorrun.runResearch`、目标研究和自动开始学习保持其独立流程。`server/internal/mentorrun/start.go`、CLI 的 `goal.go`／`study.go` 有现有调用方；删除整条 #22 流程会超出本次范围。共用 `Fetcher.Fetch`、`pdfsource.Parse`、引用原文匹配和受限知识写入；不复制其 `MaxSources=4`、`AutoAdopt`、自动绑定目标参考等产品行为。复用低层时不得让旧自动采纳权限流入新 job。

模型选用 settings 的 effective mentor，按当前 mentor／teaching 配置和 Chat Completions／Responses 风格执行，沿用带推理工具关联处理。开始时保存模型及搜索配置指纹；继续或下一轮重新核验。连接仅修改资源上限时按有效值重算窗口；禁用则停止外发，目的地或模型变化按既有外发目的地确认流程处理，不静默替换。这不是首次搜集的计划确认。

拟新增 `server/internal/mentorrun/source_research.go`（阶段编排）、`source_research_results.go`（候选／报告）、`source_research_commands.go`（选择／继续／补充），以及对应 HTTP／Web 文件。共同使用 M-A 的网络宿主、台账和导入服务，不复刻第二套抓取或 PDF 管线。

## 3. 规划、迭代与质量筛选

1. **冻结输入。**保存用户需求（1–4000 字符）、本轮规模、当前配置及 privacy generation；只读该 job 的历史结果。既有来源仅使用同一区可读的来源标识、URL／指纹做导入去重，不将正文或用户历史交给研究模型。
2. **规划方向。**模型通常给出 2–5 个互补方向及简短目的，可按窄主题减少，方向数不超过可阅读页数；不为了凑数制造方向。需求点名的练习、规范或官方文档等资料类型分别规划方向，通过提示词优先定位发布机构、大学课程或教材作者等权威来源，具体网站由模型按主题选择。每条有稳定 direction ID，直接显示并执行。用户不用确认。
3. **搜索。**调用 M-A 的 `search_web`，每次最多 10 条摘要；query 绑定方向。规范化重复 query 不重发，不使用预算倒计时。合法空结果继续后续方向；Brave 合法 search 响应省略 web 结果组时按空结果处理，损坏响应仍报错。当前与暂存方向的待读线索总数不超过本轮尚可读取数量的两倍，超出先按相关性和独立性筛掉，并标记还有未读线索。选页时优先本轮较少尝试的注册域名（含其子域名），同域名每轮至多尝试 3 个 URL。
4. **读取。**每个方向开始时，将剩余阅读名额按尚未处理的方向向上均分；额度按尝试数扣减，失败也占用。方向没有结果、线索耗尽或已获得其候选份额时释放未用额度，供后续方向使用；候选仍不足时轮转复用保留的线索。在事务中为首个不同 URL 预留阅读名额，再调用 `read_web`；每页独立保存快照与解析缺口，失败分类保存。重定向使用同一名额；重读已存 cursor 不占新网页名额。401／403／429 或禁止保存后，本轮跳过该注册域名；重定向每跳也执行这项额外限制，不放宽公网、DNS、存储或解析规则。只将有界片段交给模型，记录实际已看范围，不能宣称模型读完服务端保存的全部内容。
5. **筛选。**模型输出结构化 `include/exclude`、简短具体理由、支持片段及缺口；服务端验证页面存在、正文可用、版本和引用。从已保存的有界正文中按需求与当前方向选择片段，不局限于第一页 cursor；只有实际加入模型输入的片段登记为可引用证据。提供最多 5 个最相关的已有候选短标题，供模型识别同一文献的镜像、不同格式或摘要入口；标题不是事实证据或指令，不把此提示当作确定性语义去重保证。按需求匹配、原始出处／权威性、时效、文本质量和独立性排序。资料可各自满足一个方向，互相补充，无需每篇同时包含所有资料类型；基础概念不因年代久远自动淘汰。低质、只给摘要、禁止保存、纯扫描、打不开或无关结果不能成为可导入候选；理由不伪装成客观可信度百分比。
6. **补方向与整理。**根据未覆盖需求选择下一个方向或改写查询，优先复用保留线索，避免同一站点转载挤满清单。每完成一个方向，可生成并校验对应报告部分后保存。遍历方向且候选达到上限时可以结束；候选不足则在阅读上限内继续。报告逐节只用至多两条已筛选短引文，正文至多 220 字符，概括具体内容与使用顺序，不展开长综述；只在结果冻结前替换尚未展示为最终候选的排序。
7. **结束。**覆盖足够、阅读名额耗尽、或一轮遍历待处理方向都没有新增可读线索时结束检索，写清缺口，再完成报告。不得用无结果的循环无限发新查询。没有固定搜索次数／Token／金额预算；上述是任务完成与无进展判定，不是导师全局重复工具检测设置。

同一执行轮以串行为默认，便于停止、排序和可复现计数；复用全局并发限制，页面任务不另起无限并发。一个页面失败不使整轮丢失；提供商鉴权失效、持续限流、连接不可用、模型失败或存储满则暂停／失败并保留结果，不自动耗用新请求反复重试。

去重分三层：抓取前规范化 URL（移除 fragment、统一 host／默认端口，仅剔除明确跟踪参数，不删除可能影响内容的 query）；抓取后合并最终 URL 别名；解析后比较正文／PDF 指纹。已计入的首次失败或别名读取不退回名额，实际 GET 次数另计。重复或相似转载可归组，只保留一个代表候选，不能当作多份独立支持；未经校验的页面 canonical 标签不能决定身份。

候选 ID 按 job＋稳定正文版本确定。跨轮已读网页优先复用，已导入版本标为已加入，不计本轮新增候选；用户取消项记录在选择中，不因模型再次发现而恢复勾选。后续获得同 URL 新内容只成为新候选，旧引用不变，界面说明版本不同。

## 4. 状态、停止、恢复与补充

每个 job 同时最多一个活动轮；job 版本控制用户命令，进度另用递增 `event_seq`，选择另用 `selection_version`，避免每个页面完成都使用户勾选冲突。一个 round 的阶段与状态分开：

```text
stage  = planning | searching | reading | screening | reporting
status = queued | running | stopping | stopped | paused_tool_steps |
         completed | failed | outcome_unknown
```

| 动作／事件 | 持久语义 |
| --- | --- |
| 开始 | 创建 job、round=1 和运行；记录操作回执后排队。重放同操作返回原任务。 |
| 进度完成 | 已校验片段、筛选或报告部分写入后才增加完成数；事件与相应状态同事务提交。 |
| 停止 | 锁定当前 round、撤销 dispatch epoch／租约、取消模型及网络请求；先显示“正在停止”，屏障提交后为 stopped。停止后不再发“最后一次总结”调用。 |
| 完成与停止竞态 | 同一状态 CAS 决定；完成先提交则返回已完成，停止先提交则拒绝迟到结果，已保存部分保留。 |
| 工具上限 | `search_web`／`read_web` 累计到 `Limits.MaxToolSteps` 时收回工具，保存进展，允许受限且记账的无工具说明；用户停止优先于该说明。继续后步数重计，但本轮 30 页名额不重置。 |
| 原轮继续 | stopped／failed／outcome_unknown／paused_tool_steps 可继续；校验同一输入、权限、配置、generation，创建新的执行 epoch，从首个未提交步骤开始，已提交资料和报告部分不重跑。 |
| 恢复过期租约 | 未外发步骤可重新排队；已派发但无结果只标未知，不自动重发。用户继续后可重试未知步骤，生成新 attempt，原来占用的同 URL 名额不再增加。 |
| 再找一些 | 仅当前轮非活动时受理；新增 round、重新读取当前规模设置，继承需求＋可选补充、已读索引、排除／导入记录及旧报告。新一轮可以围绕缺口补充，不延续已达上限的旧轮。 |
| 导入与新一轮 | 只从一个固定结果快照导入。新一轮不能修改正在导入的旧候选或报告；导入回执映射到确切版本。 |
| 删除／到期 | 停止全部活动，撤销预览，删除敏感载荷并使 API 不再返回内容；旧操作不能复活 job。 |

停止时可展示并导入已校验候选；已有报告部分标“未完成”，未覆盖章节列明。停止前没有报告部分时只显示“整理报告尚未生成”，不能导入空报告；用户继续后再完成。正常结束即使零候选，也有说明检索范围与缺口的报告，不伪造引用。

“继续”按钮描述“继续这次搜集”；“再找一些”描述“开始补充搜集”。若原轮不完整，补充轮明确从已有结果重新规划，原轮不会被暗中恢复。已停止轮的已成功部分可被引用，不复制未完成的待执行队列。

## 5. 候选、报告与进度模型

| 对象（拟新增） | 关键字段与不变量 |
| --- | --- |
| `SourceResearchJob` | `id,space_id,actor_device_id,generation,version,request,created_at,expires_at,current_round_id,selection_version`；请求密文；无需 goal ID |
| `ResearchRound` | `id,job_id,number,run_id,status,stage,execution_epoch,limits_snapshot,configuration,read_attempted,read_succeeded,read_failed`；number 在 job 内唯一，最多一个活动轮 |
| `ResearchDirection` | `id,round_id,title,purpose,state`；可见计划／调整记录，不保存隐藏推理 |
| `ResearchCandidate` | `id,job_id,discovered_round_id,document_revision_id,reason,quality_notes,coverage,status,rank`；引用 M-A 的 WebDocument，不能只含 URL 摘要 |
| `ResearchSelection` | `candidate_id,selected,explicitly_changed,import_receipt?`；新最终候选默认 true；已加入项不能再次创建；report 选择钉住 `report_revision_id`，默认空 |
| `ResearchReportRevision` | `id,job_id,round_id,version,status,sections[],gaps[],based_on_revision?`；成功发布的版本不可变，旧引用不会跟随新版本漂移 |
| `ResearchReportSection` | `id,ordinal,title,body,citations[],status,coverage`；只对校验成功的部分显示已完成，保存事实与引用同时发生 |
| `ResearchResultSnapshot` | `id,job_id,round_id,version,candidate_ids[],report_revision_id?,status`；完成或停止时冻结，可用于预览和导入 |
| `ResearchEvent` | `job_id,seq,type,round_id,created_at,payload`；payload 加密，只有 UI 所需搜索词／页面／阶段，不含推理或密钥 |

运行中“已找到候选”可以显示暂定结果；进入 completed／stopped／failed 等非活动状态才冻结导入清单，避免用户点击时排序淘汰刚选内容。每轮最终新增候选不超过上限，全部默认勾选，用户的既有取消选择优先于新排序。结果清单分页展示，不能把所有正文随 job GET 返回。stopped／failed 后继续会形成新的结果快照；此前成功的报告部分按原版本复用，新增内容另存，不改变用户已经导入的版本。

报告使用 M-A 的网络引用类型，ID 为 `<section_id>:<n>`，例如 `overview:1`，正文对应 `[overview:1]`；Web 报告 renderer 解析此格式，不硬套导师仅数字的 `citationParts`。资料事实引用精确片段，重要冲突并列不同依据，推断／教学示例明确区分。只校验真实片段和位置，不声称算法证明了事实真实性。

报告按需求组织为少量章节，每次只读该节证据，预留输出与 schema 空间；大资料不一次塞入模型。逐节保存并核验，最后有界汇总只能复用已验证部分，不引入未读来源。单次输出超限、响应截断或引用无效不能算成功；保留前面已成功部分。所有整理、修复和上下文压缩都记账。

补充轮生成新的报告 revision，引用旧轮快照时仍保留旧文档身份和时间；旧报告仍可查看。新报告默认不被选中替代用户已经选定的旧报告，界面显示“已有新版报告”，用户自行更换。报告选项与候选选择在服务端保存；未发送的补充需求草稿只留标签页内存。

## 6. 预览、确认与逐项导入

导入使用两种内容：外部候选的原文快照，及用户选择的确切报告版本。先冻结 `result_snapshot_id + selection_version` 生成预览，显示将保存的资料数、报告、读取缺口及已加入项；报告不占外部资料名额，默认最多 10＋1。一次跨轮选择也最多按当前预览的资料上限分批，界面明确分批数量，不是累计上限。

候选默认全选包含可用但部分解析的 PDF；对应预览另列“仅保存可用文本”，默认未接受。用户取消该 PDF 或显式接受后才可确认；不偷偷取消默认勾选，也不默默保存部分 PDF。普通静态 HTML 明示覆盖缺口；禁止留存、无正文或纯扫描失败项只能留在失败列表，不能成为候选。

统一“确认导入”后由网页按固定预览顺序逐项调用受限保存命令，不新增另一个后台导入队列：每项有服务端预分配的稳定 operation ID，重试／刷新查回执，成功项跳过；中途断线剩余项标为未完成，由用户“重试未完成项”继续。保留成功项，不做全部回滚，也不自动换掉失效资料。每项提交前重新校验快照、预览、权限、generation 和用户接受部分内容的状态；续作预览过期需重新查看当前预览，不能无提示续用旧同意。

外部资料复用 M-A 的 `web-import-previews`／`web-imports` 与 knowledge 正式新增流程；不重新抓网页、不信任客户端正文。相同快照的已有导入回执返回已有来源；同 URL 新正文首版另存独立来源，不覆盖旧知识身份。写入当前区私有集合，返回来源目录可用的确切版本；不自动勾选问答或成为目标参考。

报告作为一份 Markdown 派生来源，标题带“整理报告”，元数据 `derived_from` 增加 `kind=source_research`、job／round／report revision、生成时间、模型归属及 `partial` 标记。正文附“由 AI 根据所列资料整理”，不冒充网站原文。只保存报告也允许。

报告保存时将每条被引用证据的标题、URL、读取时间、quote、页码、内容 hash 和对应正式来源身份（若已导入）嵌入 knowledge 的不可变附件。只复制实际引用的短片段，不复制未选网页全文、未选 PDF 原件或整个搜集 checkpoint；未选网页不建立独立来源／索引。用户预览明确“报告会保留引用片段和原网页地址”。

清除 job 后，正式报告仍能显示这些证据片段；未单独导入的网页不再提供全文或 PDF 页图，界面说明只保存了引文。已单独导入者可跳转当时正式版本。清除某个正式来源不会通过报告跳转恢复其全文；全局隐私清除同时清除派生报告与附件。最小引文附件是用户确认保存报告的一部分，有独立保留周期，不能隐藏成缓存。

## 7. 网页设置与数量口径

在 `settings-page.tsx` 增加“智能搜集资料”分组；服务端 `settings.View`／`Update`／stored 结构新增 `source_research`，与模型 Limits 分开，用原 `expected_revision` 更新。

| 字段草案 | 用户标签 | 默认／范围 | 生效时点 |
| --- | --- | --- | --- |
| `source_research.max_sources_per_round` | 每次推荐资料数 | 10；整数 1–20 | 新任务或“再找一些”的新轮 |
| `source_research.max_pages_per_round` | 每次最多阅读网页数 | 30；整数 1–100 | 同上 |
| 原 `limits.max_tool_steps` | 每轮工具调用上限 | null 不限；正整数 | 每个新的工具执行轮或明确继续，沿原合同 |

要求 `max_sources_per_round <= max_pages_per_round`，非法值拒绝并解释，不静默裁剪。页面说明：“资料数量不含可选的 1 份整理报告；一个 PDF 文件按一个网页计算，读取失败也占阅读名额。”不新增金额／Token／累计请求预算输入。

“阅读 30 页”是至多尝试 30 个规范化 URL，不保证 30 个成功；PDF 物理页数及部分提取另显。已读快照复用不占新名额，失败重试仍关联原 URL 名额但每个实际 HTTP 请求进入实际计数。开始和“再找一些”冻结当前两项设置；原轮继续沿旧规模，不能靠重复继续不断刷新 30 页名额。

资料数量达到上限只限制本轮候选／导入名额；可继续在本轮阅读范围内找更好证据。多轮累积资料和模型用量无预算上限，但仍受现有存储、并发、逐页超时、PDF 资源、单次输出和上下文窗口约束。

## 8. PDF、安全与数据发送边界

全部网络及 PDF 边界继承 [M 设计 §6–§7](mentor-web-search.md#6-加入来源pdf-与正式保存)，不放宽文件限制来凑满 10 份；沿 `Fetcher.Fetch` 进行逐跳 DNS／公网检查和保存限制检查，再调用现有 `pdfsource.Parse`。网页 PDF 仍是 1 MiB 传输／2 MiB 解码，文本层和最多 100 物理页等解析限制独立生效；不支持 OCR、登录下载或付费墙绕过。

本 job 的模型上下文天然只含用户主动提交的需求和公开资料；不加载私人学习内容，不提供 companion、记忆、目标修改或知识导入工具。search/read 参数、登记资源及 owner 在服务端检查。恶意网页中的检索词、链接、工具要求、角色声明和模型输出都不能增加权限；报告引用也不能触发自动抓取或自动导入。

抓取失败／禁止存储的响应不得进入模型或候选；查询、URL 和摘要同样视为可能敏感，不写明文日志、错误、用量明细或埋点。UI 只渲染安全文本／受限 Markdown；不得加载原网页脚本、HTML 或远程图片。权限／generation 核验覆盖 SSE 和 PDF PNG 响应，不能只检查 job 创建。

## 9. 隐私与用量

job 保存七天，起算为最近一次用户主动开始、继续、补充或导入动作；自动步骤、轮询、打开页面和事件重连不延长。活动 job 在到期时也要停止并清除。UI 显示确切保留截止时间；到期前用户可以导入所需结果，已确认的 knowledge 来源不会随 job 删除。关联 run 的 `expires_at` 与网络资料到期由 job 的期限统一驱动，续作时同事务更新，不能让旧 run 的七天计时提前清掉仍有效的任务证据。

learning owner 保存加密需求、计划、查询、事件、网络快照、PDF、候选、选择、报告、预览和回执。删除 job／学习区及到期清除覆盖这些正文、索引和内存缓存，并撤销所有轮次的读取／派发许可；同步更新 `server/internal/learning/postgresstore/privacy.go` 的 scrub 和 verify，以及新表密钥轮换。全局清除同时覆盖 knowledge 的正式 PDF、报告附件及索引；旧代次、旧事件 cursor、租约或回执不可恢复内容。

已发生用量归根任务 `kind=source_research,origin_id=job_id,source=web`，各轮共享根，明细包含 round／阶段但不含需求或网页标题。新增 run→job 关联必须同步 `modelusage.RecoverTx` 的活跃租约判断，避免将仍在运行的 job 调用误结算为未知。搜索采用 M-A 的搜索台账，公开规划、筛选、报告、压缩、修复、工具上限说明全部进入模型台账；导入触发的实际模型调用也沿相同根归集。

读页计数从持久 reservation／结果计算，不依赖浏览器累计。搜索、模型、工具、URL 和 HTTP 跳转分别统计；已发失败／取消照常记录，usage 缺失保持未知，不按文本长度编造 Token。不设置累计预算，不把资料数量或失败循环控制包装成费用余额。

## 10. API／OpenAPI 草案

新增前缀 `/v1/learning/source-research`，带学习区 Header、真实网页身份和现有 CSRF／scope／privacy read permit。读取需 `learning:read`；运行／选择需 `learning:write`，并验证当前区归属；读取已有来源去重信息需 `knowledge:read`。导入还需 `knowledge:write`、`research:adopt`。不提供从浏览器提交原始工具调用的通用接口。

| 方法与路径 | 请求／结果 |
| --- | --- |
| `GET /v1/learning/source-research/capabilities` | 模型／搜索可用性、保存能力、有效规模、PDF 资源限制及导入权限；无旧 `request_budget`。 |
| `POST /v1/learning/source-research/jobs` | `{operation_id,request}`；读取当前设置，受理即开始。202 返回 `{job_id,version,round_id,run_id,expires_at}`，不返回待确认计划。 |
| `GET /v1/learning/source-research/jobs`、`/jobs/{id}` | 分页列表／状态、轮次、方向、有界进度摘要、有效动作、选择版本、结果快照及用量任务链接；不返回所有正文。 |
| `GET /jobs/{id}/events?after=…` | SSE 顺序事件，可按 seq 重连；事件被清理时回读快照，不能触发重跑。 |
| `GET /jobs/{id}/candidates?round_id=…&cursor=…` | 已冻结候选／当前暂定状态、理由、coverage、selection、导入结果；网页全文通过 M-A 的 owner 受限 resolver 读取。 |
| `PATCH /jobs/{id}/selection` | `{operation_id,expected_selection_version,items:[{candidate_id,selected}],report_revision_id:null或UUID}`；只改指定项，防止覆盖其他标签页未提交选择。 |
| `GET /jobs/{id}/reports/{revisionID}` | 分页 sections、引用、完成与覆盖缺口；旧版可读，引用绑定实际片段。 |
| `POST /jobs/{id}/commands` | `{operation_id,expected_version,round_id,kind:"stop"|"resume"|"delete"}`；resume 不接受新需求或重置阅读上限。 |
| `POST /jobs/{id}/rounds` | `{operation_id,expected_version,additional_request?}`；“再找一些”，仅非活动时受理，读当前设置并新增一轮，202 返回新 round/run。 |
| `POST /jobs/{id}/import-previews` | `{operation_id,result_snapshot_id,expected_selection_version}`；freeze 选择及报告，返回 `preview_id,preview_version,expires_at,items[{item_id,kind,operation_id,requires_partial_accept,already_imported}]`；每次不超过配置资料数＋1 报告。 |
| `POST /v1/learning/web-imports` | 外部资料逐项确认，沿 M-A 字段／语义；R 预览适配到同一服务端 preview store，不另外复制资料。 |
| `POST /jobs/{id}/report-imports` | `{operation_id,preview_id,expected_preview_version,report_revision_id,storage_consent:true,accept_partial}`；只接受该预览中的报告，保存正文和最小引文附件，返回来源回执。 |
| `GET /jobs/{id}/imports?preview_id=…` | 逐项 `pending/imported/already_imported/failed` 及正式来源身份；刷新查回执后只重试未成功项。 |
| `GET /v1/settings`、`PUT /v1/settings` | 扩展 `source_research` 的两项参数与范围，沿现有 revision 写入合同；不加入每任务预算。 |

表中 `/jobs/...` 简写均以前缀 `/v1/learning/source-research` 展开。错误沿 M 设计；409 包含任务已活动／选择版本冲突／预览过期，给用户可操作文案。“停止”不等待长请求自然结束才撤销派发许可；响应后重连不会继续旧任务。

正常完成结果草案：

```text
{job_id, round_id, result_snapshot_id, status:"completed",
 limits:{max_sources_per_round:10,max_pages_per_round:30},
 progress:{read_attempted:30,read_succeeded:24,read_failed:6},
 candidates:[{id,document_revision_id,reason,coverage,selected:true}],
 report:{revision_id,status:"complete",section_count,citation_count},
 selection:{version,report_revision_id:null},
 available_actions:["import","more","delete"],usage_task_id,expires_at}
```

这些数字仅用于协议举例，不要求恰好读满或保证成功数量。OpenAPI 在 Build 定义严格对象、枚举、长度／范围、引用联合类型及 409 语义，同步 Web API 生成类型和运行时校验。新增全部路径纳入 nginx 白名单、学习区中间件和 `server/internal/deploy` 检查；删除旧 source-discovery 白名单及专用 capability 字段，不扩大通用代理路径。

## 11. Migration 草案

拟新增 `000052_source_research.sql`，以 Build 最新编号为准；本次不创建 SQL。无旧发现任务转换、旧研究 checkpoint 升级或接口兼容层；新表从空状态开始，现存 #22 数据不搬入新工作流。

| 表／变更 | 必要字段与约束 |
| --- | --- |
| `learning_source_research_jobs` | job／space／device／generation／version／selection_version／到期时间；加密需求和去重摘要，显式关联 active round |
| `learning_source_research_rounds` | job／round number／run／generation／epoch／状态／规模快照／有界阶段 payload；同 job 序号唯一，部分唯一索引限制一个活动轮 |
| `learning_source_research_candidates`、`learning_source_research_selections` | 候选→WebDocument revision，理由及质量记录密文，选择版本与导入回执；跨轮同快照候选唯一 |
| `learning_source_research_reports`、`learning_source_research_sections` | 不可变报告 revision 与有界分段正文、引用、覆盖；部分保存与完整发布不同状态 |
| `learning_source_research_results`、`learning_source_research_events` | 冻结结果版本和顺序事件；敏感 payload 加密，job 清除级联 |
| 现有运行／操作 | 扩展 kind 和 `source_research_job_id`／round 关联；共享命令去重，快照只持引用；不是在旧 Research JSON 内不断扩容 |
| M-A 网络／预览合同 | owner 扩展 source_research；附件到期沿 job，不沿短执行 run；复用逐项导入回执，加入 report 引文附件类型 |
| settings／knowledge | settings 持久 payload 增加两项参数；knowledge 正式派生来源增加报告 provenance 和最小引文附件，按原不可变文档提交 |

所有新增 learning 表使用 owner 写屏障、同 generation 复合关联、scrub／verify 和轮换登记。WebDocument 用 owner ID 复用到同 job 的多轮，不能共享到另一学习区；每个公开查询／阅读尝试仍保留其实际轮次归属。PDF 不存入单体 checkpoint，列表不读附件。

R-A 即建立轮次与报告版本关系，以免 R-B 重定公共契约；未开放的补充行为不是 API 别名。R-B 仅在确有结构缺口时另加 migration，不改已交付 SQL。

## 12. 垂直批次与退出标准

### R-A：首次搜集、双结果与确认导入

**结果：**用户描述需求后直接获得多方向搜集的候选与报告，运行中可停止，结果可选择并确认加入来源；设置影响新任务，用量和清除可查可用。

**范围：**新 job／首轮、规划／搜索／PDF 阅读／筛选／分段报告、事件、停止、部分结果导入、两项设置、逐项回执、隐私与用量；移除旧关键词发现入口。原工具上限暂停的继续能力随现有合同接通。

**延期：**stopped／failed 原轮的完整恢复、“再找一些”和跨轮用户体验交给 R-B；R-A 界面只开放已实现动作，不能把停止后的“重新开始”伪装成继续。轮次／报告版本数据合同本批已确定。

**退出标准：**

- 一个真实入口完成需求 → 至少两个检索方向 → 读正文 → 候选理由＋引用报告 → 取消一项＋可选报告 → 确认导入；另覆盖零结果、部分 PDF、只保存报告及先停止再导入已完成部分。
- 确定性验证阅读名额（含失败／重定向）、无结果结束、单次窗口、工具上限、SSRF、提示词注入权限隔离、报告引用、逐项幂等、旧 epoch 和跨区访问；真实 PostgreSQL 验证附件／报告／选择清除和与 knowledge 同事务写入。
- 受影响 `mentorrun`、`research`、`knowledge/postgresstore`、`modelusage`、`settings`、`transport/httpapi`、`migrations`、`api`、`deploy` 包的相关检查及 Web 类型／构建通过；未改的 PDF 解析边界复用 M-A 有效证据，不重复全扫。
- Chromium 首轮闭环、刷新进度、停止后无新增请求可运行；新增路径经过 nginx，旧 discovery 路径不再可用。文档／OpenAPI／schema 同步，工作树达到可提交状态。

**重型证据：**真实搜索＋演示模型的资料相关性／理由／报告质量、移动端全流程及跨进程清除竞态留完整 Candidate；原因是需完整结果、部署及外部环境。安全、资源和实际计数断言不得延期。

### R-B：继续原轮与再找一些

**结果：**停止／失败后继续未完工作，或启动补充轮；旧结果、取消项、导入回执和报告版本保持，新结果可继续导入。

**范围：**resume／more 命令、轮次恢复、跨轮去重、选择保留、增量报告与历史版本、设置生效时点、同根用量及相关 UI；补齐完整 28 条合同。

**延期：**无本次已承诺行为延期；CLI、登录网页、OCR 和累计预算仍是非目标。

**退出标准：**

- 可演示读到部分页停止 → 刷新 → 继续至结果 → 导入 → 修改设置 → 再找一些；只读未完成页面，新轮使用新规模，旧取消项保持，新候选默认选中。
- 多标签双击补充只创建一轮；租约过期的未知请求不自动重发；继续同轮不重置阅读名额；新报告不覆盖旧版；已成功导入不重做，单独保存报告后删除 job 仍可核对最小引文。
- 对修改的 run 恢复、台账根关联、候选／报告和 HTTP／Web 做定向回归及必要 PostgreSQL 事务测试，复用未变更 M-A／R-A 证据；新增 Chromium 补充闭环通过，文档与可用动作一致，工作树可提交。

**重型证据：**所有批次完成后才做本 child Candidate：按 28 项逐项核对、真实 Responses 带推理与 Brave 场景、移动端／键盘、真实 PDF 页定位、服务重启／停止／清除交错。失败回到对应批次，不为了清空无关历史告警扩展范围。

## 13. 验收重点与风险

- 工具轮次、搜集轮次、URL 阅读数、HTTP 请求数和模型次数容易混淆；必须分别保存，不能把“继续”变成隐蔽扩容或重复收费。
- 报告保留引文而未选网页不进入来源，涉及独立 provenance 与清除周期；既要可核对，也不能暗存未选全文。
- 资料质量和引用位置校验不同：测试能证明实际读过、版本正确、没有越权，真实模型抽查才能评估方向、理由和整理是否有用。达不到质量预期不能通过取消引用校验修复。

## 14. R-A 实现覆盖

### 14.1 运行与存储

- 新增 `mentorrun/source_research{,_types,_store,_results,_commands,_imports}.go`。首次请求创建学习区 job、首轮和 `kind=source_research` 运行，使用原 worker 的准入、租约、派发门禁和退避。目标研究 #22 保留原入口与行为。
- `000052_source_research.sql` 建立 job、round、candidate、selection、report、section、result、event、preview、import 表；运行关联 job，正文记录使用独立加密载荷和 generation 关联。checkpoint 保存方向、资源 ID、去重索引、有限进度及游标，PDF／网页正文继续由 M-A 的 `learning_web_*` 保存。
- 规划输出最多 5 个方向；串行搜索每次最多 10 条线索，待读队列不超过剩余阅读名额的两倍。读取名额在外发前提交；重定向共用名额，失败不退款。实际 HTTP 请求独立记录，停止后的数值结算不提交迟到正文。
- #180 在 `source_research_selection.go` 增加注册域名分散、每轮 3 URL 上限与拒绝域名记忆；`Policy.BlockedDomains` 仅接受受信宿主设置，并在重定向每跳执行。各方向按剩余尝试额度分配，失败参与扣减，提前结束的额度归还；不足时轮转复用有界线索。小规模规划建议合并同类资料方向，避免过度限定高校 PDF。
- URL 规范化只去除 fragment、默认端口和明确跟踪参数；已知最终 URL 别名不重复排队；相同正文指纹合并候选。模型逐页判断相关性、出处、时效、文本质量及独立性，服务端校验所选短引文，内部以相关性排序，不向用户输出可信度百分比。
- 达到阅读上限、模型判断已覆盖，或遍历方向无新增可读内容时结束。候选达到目标数量仍先完成本次方向遍历；遍历后达到目标即可结束。跨任务已有来源只读取当前区可见的指纹与确切身份，不把私人正文送入搜集模型。

### 14.2 进度、停止与工具上限

- 运行阶段、方向、搜索词、页面、读取／失败计数与事件一并持久化。网页通过 SSE 接收顺序事件，断线重连并回读快照；关闭面板不停止运行，来源区保留运行入口。
- 停止命令在同一事务撤销 epoch／租约并冻结已提交结果，取消在途请求，不再发出总结模型调用。完成先提交时保留完成结果；旧 epoch 不能写入候选或正文。
- 搜索与读取使用现有 `Limits.MaxToolSteps`。达到上限保留待执行队列并暂停；“继续这次搜集”重置工具执行计数，沿用本轮阅读名额；本轮已导入的候选仍占本轮资料名额，不能通过继续刷新。继续时若目的地改变，显示新模型／搜索目的地并要求明确确认。
- 暂停时发布的部分报告不再修改；继续时复制已验证章节到新 revision。停止／失败的完整恢复、补充轮及“再找一些”留待 R-B，接口和 UI 不开放这些动作。

### 14.3 双结果、预览与正式导入

- 最终候选默认全选，报告默认不选。选择使用独立版本；明确取消报告与未修改报告有不同语义。改变选择会撤销旧预览的待执行权限，已成功导入的回执继续有效。
- 报告逐方向保存，最多 5 个章节。模型每节只接收最多两条筛选阶段已核验的连续短引文（每条最多 240 字符），输出至多 220 字符及两个短引用编号。服务端还原原文、版本、位置与 hash，正文引用改为 `[section-N:n]`；Web 可以打开对应网络快照和 PDF 页图。
- 搜索没有正文时返回空候选与确定性的检索范围／缺口报告，不伪造引用。部分 PDF 默认仍勾选；预览必须额外接受“仅保存可用文本”。部分报告也需显式接受，空报告不能导入。
- 服务端预览冻结结果与选择，分配稳定逐项 operation ID；浏览器按顺序确认，刷新查回执，保留成功项并允许重试未完成项。网页候选复用 `web_imports.go`；不重新抓取网页，不接受客户端正文，不自动勾选问答来源或目标参考。
- 报告使用 knowledge 的正式新增事务，来源类型为 Markdown 派生报告，记录 `derived_from.kind=source_research`、job／round／revision、模型及生成时间。最小引文附件落在现有不可变 `knowledge_source_metadata.evidence`，包含标题、地址、读取时间、quote、页码、hash 以及已导入的正式身份。删除 job 后这些引文随正式报告保留；未选网页全文和 PDF 不进入正式来源。跳转已保存来源仍须重新核验版本与访问权限。

### 14.4 设置、协议与生命周期

- `settings.source_research`：资料数默认 10（1–20），阅读数默认 30（1–100），资料数不得大于阅读数；沿 `expected_revision` 保存，新任务冻结规模。网页明确 PDF 按文件计数、失败占阅读名额、报告另计，不增加累计预算字段。
- 新 HTTP 前缀为 `/v1/learning/source-research`，包含 capabilities、jobs、events、candidates、reports、selection、commands、import-previews、report-imports、imports。OpenAPI、Web 生成类型与 Zod、学习区中间件、nginx 白名单同步；旧 discovery 接口、预算字段、Web 入口和专用测试删除，没有别名。普通 URL 的域名策略独立校验。
- 模型与 Brave 请求归入同一 `source_research` 用量根；`modelusage.RecoverTx` 识别 job 的活动运行租约。未知 usage 继续保留未知，网页实际 GET 次数独立于搜索与模型调用。
- job 的七天保留期仅由开始、明确继续或导入推进；关联 run 与网络快照同步到期。删除／到期清除临时证据、报告、选择、预览与操作载荷；全局隐私清除同时覆盖正式报告、引文和 PDF。新密文表已加入 learning scrub／verify 与密钥轮换。

### 14.5 验证与真实输出规模

#180 的计算机网络需求、5/12 与 10/30 真实复测、历史失败 URL 请求头对照和回归检查见 [覆盖改进报告](../development/research-coverage.md)。下述 3/8 数据保留为最初 R-A 实现的历史验证，不作为 #180 的两档结果。

确定性验证使用 PostgreSQL、httptest 搜索／网页及模型桩，覆盖首轮与两方向、默认选择、取消后导入、稳定回执、只保存报告、报告删除后引文保留、部分 PDF、失败／重定向／SSRF 的阅读计数、停止与旧 epoch、跨区访问、窗口拒绝、工具暂停继续、不可变部分报告、无结果结束及隐私清除。

真实验证使用配置目录提供的 deepseek-v4-flash、Responses 和 Brave，规模为 3 份资料／8 页，单次输出硬上限始终为 2048，使用 low 推理。首次报告输入仍含多个片段菜单，第二个报告调用达到上限；此次失败没有发布截断章节。改成只提供筛选后的短引文，并限制一两句话后，完整复验通过，8 页均读取成功。

| 最终复验阶段 | 模型调用数 | 各次输出 token（包含提供商计入的推理） |
| --- | ---: | --- |
| 规划 | 1 | 126 |
| 逐页筛选 | 8 | 723、510、656、520、331、440、555、460 |
| 报告章节 | 2 | 381、321 |
| 合计 | 11 | 5023；无未知输出，无 `output_budget_exceeded` |

最终复验使用 3 次 Brave 请求；4 条记录通过逐页筛选，结果冻结受 3 份新增资料上限约束。报告覆盖官方取消／超时概念和示例，未通过筛选的方向给出缺口说明。首次失败使用 9 次模型、2 次搜索；两次实际验证合计 **20 次模型、5 次搜索**，没有超过本次授权的 25／15 次上限。真实验证不会因普通测试运行而自动触发，需显式设置 `SOURCE_RESEARCH_LIVE=1`；凭据不写入测试或日志。

检查记录：受影响 Go 包测试及 `go vet -p=1` 通过，包含 API／nginx 路径检查与 `TestWorkspaceProxyAllowlist`。`mentorrun` 整包通过（1344.655 秒），随后仅对修改处做定向回归，最终选择／导入／工具暂停回归通过（49.158 秒）。Web `npm run check`、生产构建、`web_release` 服务端构建通过，4 个相关前端测试文件共 15 项通过。

Chromium 单用例通过（用例 16.0 秒，总计 20.4 秒）：需求输入 → 实际检索进度 → 刷新恢复 → 两个默认选中的候选及默认不选的引用报告 → 取消一项 → 预览并确认 → 来源列表仅出现所选资料且不自动勾选问答；另开任务停止，验证停止后没有新增模型调用。浏览器使用隔离 PostgreSQL schema 和确定性模型／网页假服务；没有运行全项目测试或三浏览器矩阵。修复了选择保存期间勾选短暂回弹的问题，保存失败时回滚，查询读取与选择版本保持一致。

### 14.6 实施选择与后续验证

- 物理表名和类型沿本节落点；报告引文使用现有不可变知识元数据附件，不另外复制网页全文。状态接口只返回进度与身份，候选和报告单独分页。
- 工具上限提示和零证据说明由已保存事实确定性生成，不为这些提示额外调用模型。模型筛选与引用位置校验仍不能证明网站事实真实。
- 首次真实验证暴露并修复了推理占满单次输出的问题；保留该失败及最终输出 token 作为证据，不以提高输出上限掩盖它。
- R-B、CLI、完整移动端矩阵、真实 PDF 页图质量与跨进程清除交错仍按 §12 留到对应批次／完整 Candidate，本批没有宣称这些重型证据已通过。
