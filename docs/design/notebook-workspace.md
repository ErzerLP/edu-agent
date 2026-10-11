# Notebook 工作区：信息架构与分批实施设计

状态：Shape 待确认，调查基线 `feat-notebook-ui` / `1d819c8`。正式行为见 [brief](../comet/specs/notebook-workspace/brief.md) 与 [spec](../comet/specs/notebook-workspace/spec.md)；视觉依据为[用户认可方向的静态原型](notebook-workspace-prototype.html)。D1–D4 均为**默认，待用户确认**。本次只新增这三份文档，保留工作树中原有未跟踪原型，不写产品代码、测试、OpenAPI、migration、Runtime 验证记录，不进行 Git 暂存／提交。

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

**涉及文件：** 修改 `clients/web/src/main.tsx`、`pages.tsx`、`teaching-page.tsx`、`components/workspace-shell.tsx`、`styles.css`；新增 `notebook-home-page.tsx`、`notebook-workspace-page.tsx`、`components/notebook/{notebook-header,navigation-drawer,notebook-card,notebook-layout,classroom-panel,goal-selector,sources-panel,mobile-panel-tabs}.tsx` 与 `lib/use-notebook-sources.ts`；按需抽取 SpaceEditor／课堂主体。测试修改 `learning.spec.ts`、`workspace.spec.ts`、`release.spec.ts` 和所有受影响配对／菜单 helper，新增 `notebook-workspace.spec.ts`。

**执行步骤：**

- [ ] 先用一个真实场景明确失败：配对 → 新建区 → 卡片进入 → 恢复会话 → 正式答案／反馈；保留旧深链接断言。
- [ ] 接入卡片墙与空间管理入口，完成菜单／旧路由兼容，再抽取课堂主体嵌入；来源列表用现有 collections／tree，按集合懒加载，不读取全库正文来填卡片。
- [ ] 处理归档、缺能力、空数据、跨区／迟到响应及基本窄屏标签；同步迁移破坏的旧用例。
- [ ] 交接 `WorkspaceContext`、`SourceSelection`／`CitationTarget` 的前端边界与不支持状态，并更新设计中已落地的文件名。

**后端改动：** 默认无；仅已有 API 组合。新路由处在现有 SPA 前缀内，若实测静态分发不满足再修对应 HTTP 路由测试／分发，不能扩大 catch-all。无 OpenAPI／migration；G0 不纳入。

**延期：** 四种来源 modal、可核验选择问答／引用整合到批次 2；新 Studio 到批次 3；完整移动／可访问性／发布矩阵到批次 4。已有功能的键盘可用性和被破坏测试不延期。

**退出标准：** `npm run check`、受影响草稿／教学／导航单测、`npm run build`；Chromium 的新工作区主场景与受影响 learning／workspace／release 场景通过；390／1280px 无整页横溢出；同标签页切区恢复原草稿，双标签页互不覆盖；旧目标管理、课堂、进度、离线入口可达。记录真实文件映射与尚不支持入口，生产路径完整，工作树可审查。

**重型证据：** 全部三引擎、全部旧页和发行矩阵延到 Candidate，原因是后续样式与路由还会变化；这不豁免本批最小真实后端课堂验证。

### 批次 2：四种来源、引用定位与按所选来源问答（7–10 人日）

**结果：** 用户在工作区完成 PDF／文本／网址／搜索候选的审阅导入，选来源追问并定位核验片段，原课堂范围保持冻结。

**范围：** 父验收 5–12、相关 13、21–27；D1、D4 确认后落实 G1、G2、G3。目标研究旧页仍可恢复旧 run。来源选择／生成清单合同在本批冻结，供批次 3 复用。

**涉及前端／测试：** `components/reference-import.tsx`、`content-blocks.tsx`、`pdf-source.tsx`、`mentor-panel.tsx`、`tutor-history.tsx`、`research-page.tsx`、`knowledge-page.tsx` 的必要抽取；新增 notebook 下 `add-source-dialog.tsx`、`source-list.tsx`、`source-discovery.tsx`、`source-viewer.tsx`、`mentor-composer.tsx`，`api/notebook-sources.ts`；修改 `api/{references,research,mentor,conversations}.ts` 与 `lib/reference-import.ts`。新增 `notebook-sources.spec.ts`；扩展 references／pdf／mentor／tutor-history 用例与 `clients/web/scripts/test-server.mjs`。

**后端单列：** 修改／扩展 `server/internal/knowledge/{domain,scopes,import_preview}.go`、`knowledge/postgresstore/{store,research,content,privacy}.go`，新增 `knowledge/source_catalog.go`、`knowledge/postgresstore/source_catalog.go` 与来源元信息 migration；新增 `httpapi/source_discovery.go`／`source_catalog.go`，复用 `reference_sources.go`、`settings/search.go`、`integrations/websearch`。修改 `mentorrun/{types,conversation,references,core}.go` 及 worker 的必要快照路径、`httpapi/{mentor_runs,tutor_history,api,knowledge_spaces}.go`，应用 composition 按需要注入端口；`server/api/openapi.yaml`、生成 `clients/web/src/api/schema.d.ts` 及合同测试。G3 使用 checkpoint 不单独建选择表。

本批的关键测试文件包括现有 `server/internal/knowledge/postgresstore/{import_preview_test,scopes_test}.go`、`server/internal/mentorrun/{references_test,research_test,research_privacy_test,conversation_test}.go`、`httpapi/{references_test,research_test,mentor_runs_test}.go`，以及拟新增 source_catalog／source_discovery／source_selection 的领域和 HTTP 测试；新增 `server/testutil/webnotebook/main.go` 及其宿主边界测试，供真实来源浏览器场景使用。这些是文件边界建议，不要求重跑所有历史测试才能开始局部反馈。

**风险：** 高，来自权限、网络边界、选择／历史隔离与清除竞态，而非 modal 外观。新增受区路径需同步 `scopedKnowledgePath` 等分类；不能漏掉 server 归属门禁。

**执行步骤：**

- [ ] 先锁定 G1–G3 DTO、权限与错误语义，增加来源身份／范围 unit／HTTP 用例和真实 import→问答→citation 的失败场景；范围变更及空选择必须覆盖。
- [ ] 串行交付来源 metadata／列表／详情与原导入适配，再交付受限搜索候选和“选中才抓取”，保留 PDF 部分确认、重名身份审阅及 unknown 原操作核对。
- [ ] 导师请求接选择清单、上下文隔离、核验 citation；零来源不引用资料，来源撤权后停止展示，研究／课堂旧引用接统一查看器。
- [ ] 验证同名双集合、跨区伪造、历史版本、选择变化、无目标问答（D4）、两标签、响应丢失与隐私清除；记录供 Studio 使用的范围／引用合同。

**延期：** Studio 生成与笔记到批次 3；卡片聚合增强 G0 不做；大批次导入沿原任务页，不把所有任务控制搬进 modal。

**退出标准：** 前端类型／相关 import、PDF、mentor、conversation、citation 单测及构建；新来源与保留 references／pdf／mentor 局部 Chromium 场景通过。`cd server && go test ./api ./internal/knowledge/... ./internal/research ./internal/mentorrun ./internal/transport/httpapi` 中受影响测试通过（DB skip 另列）；独立 PostgreSQL 跑 metadata 升级、导入幂等／冲突、选择范围与清除竞态新集成用例；新增 API 生成一致性通过。实际模型请求中不含未选资料，冻结课堂引用未变。独立网络 stub 证明搜索摘要不能被导入／引用为已读正文。

**重型证据：** 全量历史 DB／三浏览器门禁延到 Candidate；本批来源权限、SSRF 既有相关契约和新清除／范围验证不可延期。新 DB 场景名称与运行命令交接给批次 4 的候选门禁。

### 批次 3：四类 Studio 产物形成可保存的真实流程（12–18 人日）

**结果：** 用户能生成并重开指南、简报与测验，保存笔记及转来源；正式测验通过既有学习领域记录作答和评估。

**范围：** 父验收 5、10–12、14–25；落实 G4、G5、G6。D2、D3、D4 以及批次 2 的选择／引用合同是前置。按以下三个**串行可运行切片**推进，不能只提交通用表／类型却无入口：3a 保存／编辑／重开笔记与转来源；3b 有引用的指南／简报生成及恢复；3c 生成测验、正式作答和进度读回。

**涉及前端／测试：** 新增 `components/notebook/{studio-panel,studio-artifact-viewer,studio-document,studio-note-editor,studio-quiz}.tsx`、`api/studio.ts`，复用 `content-blocks.tsx`、`content-tools.tsx` 的安全呈现／版本展示与正式答案组件；必要改 `studio-page.tsx` 加工作区入口而保留旧 library。新增 `notebook-studio.spec.ts`、`api/studio.test.ts`，扩展工作区／草稿与确定性模型 fixture。

**后端单列：** 建议在 `server/internal/learningcontent/` 新增 `studio.go`、`studio_store.go`、`studio_sources.go`，旧 artifact 合同不变；新增 `mentorrun/studio.go` 并修改 types／worker／store／隐私与任务快照所需路径，新增 `httpapi/studio.go`；测验编排建议放 `server/internal/learningstart/studio_quiz.go`，只经 learning／tutoring 的正式事务与 proposal 验证端口，不跨模块裸写表。应用 `composition.go`／`workers.go` 注入与运行；learningcontent 的清除／验证／轮换接入新产物；G6 连接 knowledge 导入及 G1 metadata。新增 Studio migration（包含 artifact／revision／operations／quiz mapping 与 run kind），更新 OpenAPI、生成类型、api／httpapi 合同测试。

清除落点包括 `learningcontent/store.go` 的 `RedactTx`／`Remaining` 与 `rotation.go`，由现有 `learning/postgresstore/privacy.go` owner 调用；新增表不能只在新 handler 里屏蔽。拟新增 `learningcontent/studio_test.go`／`studio_store_test.go`、`learningstart/studio_quiz_test.go`、`mentorrun/studio_test.go`、`httpapi/studio_test.go` 和 `server/api/openapi_studio_test.go`，并补 migration 升级与知识导入派生关系测试。

**风险：** 高，是本改造最大工作量。新独立正文、目标可空、加密／清除、生成租约与正式多题活动映射同时涉及；不能用降低原表 NOT NULL、复用占位活动或前端评分缩短实现。若 3c 发现必须引入新评分／教学状态语义，返回 Shape 独立拆 child。

**执行步骤：**

- [ ] 3a 从保存笔记的 HTTP→持久化→列表／重开场景做起，同时覆盖 409 保留草稿、隐私屏障和版本到来源 preview／confirm；给后续生成建立真实产物读取路径。
- [ ] 3b 增受限生成任务和引用验证，产物 commit 与运行成功保持一致；补充取消／重启／partial／unknown／预算场景，学习指南与简报有各自结果结构，不只是不同标题。
- [ ] 3c 生成有效题集后明确开始正式承载；逐题真实 Activity／Attempt／Assessment，actual help 与 allowed_help 一致；从进度／反馈读取相同记录，原课堂和未提交答案继续可恢复。
- [ ] 扩展跨 owner 隐私 scrub／verify、加密轮换与 replay；旧正文库、旧 mentor／CLI 请求不受新 enum／必填字段影响。交接新状态／错误映射和门禁必需测试名。

**延期：** 音频／视频、思维导图、闪卡、自动笔记、实时双向来源同步始终非目标；全引擎和全量视觉收尾到批次 4。

**退出标准：** 前端类型、Studio／草稿／答案／citation 局部单测与构建；Chromium 四产物端到端、重启重开、原课堂共存、丢响应一次作答与来源定位通过。后端 `go test` 覆盖 api、learningcontent、mentorrun、learningstart、learning、tutoring、knowledge 及 httpapi 的受影响路径；真实 PostgreSQL 覆盖新 migration 升级、版本并发、幂等、run 取消／发布竞争、来源撤权／全局清除不可复活、正式评估／进度一致性；OpenAPI 生成一致性、相关旧请求兼容通过。不用只依赖 fake UI 的测试证明正式记分。

**重型证据：** 全量重建／完整历史 DB 矩阵及全浏览器延到稳定候选；新评分桥接所触及的重放／证据一致性、清除和迁移在本批必须提供真实证据。三种切片各有运行入口和局部退出记录，不能到批末才发现底座无法生成产物。

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
