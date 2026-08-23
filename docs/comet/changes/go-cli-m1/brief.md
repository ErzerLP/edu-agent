# Outcome

交付第一个可日常使用的在线 Go CLI，使单用户能够从终端完成配对、知识导入、设置目标、开始或继续教学、回答、查看和纠正评估、自由提问、恢复原 focus，以及查看路线、证据、进度和复习。CLI 默认采用无横幅、无颜色、紧凑中性的文本模式，并把服务端现有的知识、学习和记忆能力组合成可恢复的 M1 用户流程。

# Scope

在 `clients/cli-go/` 建立独立 Go module 和 `edu-agent` 二进制。CLI 只通过 OpenAPI 定义的 HTTP 边界调用 Go 服务，不导入 `server/internal`。本 child 同时允许三类完成CLI所必需的最小服务端增量：`SessionView.work_item`恢复查询；provisional Feedback离态门禁与规范allowed decision；Routes `current_only`过滤。恢复查询在受learning/tutoring privacy read gate保护的repeatable-read事务中返回当前Goal、Route、Activity、Attempt、Assessment/Decision和自由问答上下文。

CLI 提供一次性配对、设备状态与注销、Markdown 文件或目录导入、目标设置、交互式 `learn`、评估确认/覆盖/作废、路线、进度、证据和复习查询。客户端负责编排现有 proposal/action 协议，但所有 canonical ID、状态转移、评分、Evidence、Mastery、review date 和知识引用校验仍由服务端决定。

# Non-goals

本 child 不实现离线操作队列、离线阅读或答题、跨设备离线合并、Rust CLI、TUI 仪表盘、Web UI、MCP、Fast Note Sync、Obsidian 发布/回写、Agent 知识维护、PDF/网页解析、多用户、客户端模型调用或客户端长期学习数据副本。CLI 不提供远程配对码签发、privacy erasure grant 签发或服务端 secret 管理。

# Acceptance examples

- A1：`clients/cli-go` 是独立 Go module，生成 `edu-agent` 二进制，不能导入 `server/internal`；根 Makefile 能分别格式化、测试、vet、race 和构建 server 与 CLI。
- A2：`edu-agent pair` 从 TTY 无回显读取配对码，或从非 TTY 标准输入读取一行；它不接受默认 `--code` 参数，不把配对码或返回 token 写入 stdout、日志、错误或进程参数。
- A3：配对成功以fail-closed可补偿流程保存独立token和普通配置：先写credential，再原子写server URL、device ID与display name；配置失败时删除新credential。启动时检测orphan credential、缺失配置和device mismatch并拒绝联网。`device forget-local`经确认后只清理本地残留并明确不代表远端吊销。Unix权限至少`0700/0600`且拒绝symlink；Windows使用当前用户绑定保护。
- A4：默认 server URL 为 `http://127.0.0.1:8080`。非 loopback 明文 HTTP 默认拒绝，只有显式 insecure 选择才允许并在每次联网命令显示稳定警告；URL 拒绝内嵌 credential、query 和 fragment，HTTP redirect 不得携带 Bearer token。
- A5：`edu-agent device status`在认证成功时显示当前设备、服务可达性、readiness/model degraded状态和scope，但不显示token；credential被拒绝时只报告authentication failed/possibly revoked。`logout`先吊销服务端设备再删除本地凭据；`device forget-local`只删除本地状态并持续声明远端可能仍有效。
- A6：`edu-agent knowledge import <file-or-directory>` 只读取规范 UTF-8 Markdown；目录递归、相对 slash 路径和文档顺序确定，拒绝 symlink、路径逃逸、NUL、重复/case-fold 冲突和服务端限制之外的批次，客户端不向服务端发送本地绝对路径。
- A7：知识导入先读取 head 并显式发送 expected parent。相同 operation 的不确定响应使用同一 operation ID 对账；revision conflict 不自动覆盖。identity review required 时 CLI 显示候选并要求 preserve/new/rewrite/split/merge 的显式决策和理由，以新的 resolution operation 提交原 receipt。
- A8：`edu-agent goal set <text>` 创建 GoalRevision；有 active session 时在明确确认后切换该 session 的 goal 并使旧 focus 按服务端规则失效，没有 active session 时创建新 session。CLI 不通过本地文件保存 goal 或 session 正文。
- A9：现有session查询把`work_item`作为required nullable字段：active stable state返回对象，Completed by-ID返回null。对象包含按状态必需的Goal、Route、Activity、Attempt、Assessment/current Decision、frame-scoped Free QA，以及按current Assessment confirmability规范化的`allowed_actions`和`allowed_assessment_decisions`。
- A10：`work_item` 在同一 repeatable-read read-only transaction 中按固定顺序锁 learning/tutoring owner read gate，要求两个 learner generation 相等，再冻结一次 active projection generation并通过各 owner 的 caller-DBTX reader读取typed records；projection/authority、record归属或version不一致时返回 `projection_unavailable`，gate关闭、generation不等或barrier取消响应时返回`content_redacted`，绝不返回混合状态。
- A11：`edu-agent learn`只从服务端work item恢复。Feedback通过Attempt ID反查唯一Assessment，并验证version 1无predecessor、version N精确替换同Assessment的N-1后取最高Decision。FreeQuestion持久化创建时session aggregate version，按active frame和该单调键选择当前问题；Answer与attached quiz精确绑定该Question。
- A12：没有 session 时 `learn` 提示目标并创建 goal/session；GoalReady 自动执行 start diagnostic；Diagnostic 使用当前 knowledge head 和确定性 retrieval hits 请求 route proposal，显示紧凑路线并应用服务端验证后的 RouteRevision。
- A13：RouteActive 使用冻结 route step、goal 和 canonical retrieval slices 生成 explanation/exposure 与 Activity proposal；CLI 记录不可计分 exposure，显示当前题目/rubric 摘要，应用 Activity 后进入 AwaitingResponse。任何模型 proposal 都不能由 CLI 直接变成 canonical 状态。
- A14：AwaitingResponse 接受单行答案或显式多行 answer block，并让用户选择 Activity 允许的 help level；仅当 `none` 在 allowed-help 中才可默认使用，否则必须显式确认允许值。提交只产生 Attempt。version conflict 后 CLI 刷新 work item，但不自动重放答案或生成新 operation 造成重复计分。
- A15：Evaluating 对 objective Activity 使用服务端确定性 assessment 路径，对 open Activity 使用冻结 Activity、Attempt、rubric 和 canonical knowledge slices 请求 assessment proposal；record assessment 后显示每个 rubric item、结论、confidence、risk flags、disposition 和是否产生 Evidence。
- A16：provisional Assessment不显示为已计分。current Decision仍为provisional时，服务端拒绝所有会离开Feedback的tutoring action，包括acknowledge、end activity和switch goal；CLI只允许退出进程或执行服务端判定可用的confirm/override/void。不可confirm的provisional只提供override/void，决定后重新读取work item。
- A17：override 只让用户修改 rubric conclusion、可选 misconception candidate 和理由，答案/知识 quote、UTF-8 range、hash 与 reference ID 从 immutable AssessmentArtifact 保留，不能由 CLI 猜测或重算成另一份来源。
- A18：已解决的 Feedback 经用户确认后执行 acknowledge feedback，服务端决定 RouteActive 或 Completed；CLI 不自行推进 route step、Mastery 或 review schedule。due review 使用 present-review 创建 frozen review Activity，只有后续 accepted Evidence 推进 authoritative review，provisional 或 voided 结果不推进。
- A19：在 RouteActive、ActivityIssued 或 AwaitingResponse 输入 `:ask` 会保存服务端 FocusFrame、创建 FreeQuestion、生成并记录 FreeAnswer。自由问答明确标记为不计分，默认可回到原 focus，追问复用同一 active frame。
- A20：FreeQuestion work item 只返回 active frame 内当前 Question且 Answer必须为空；FreeAnswer返回该Question及`free_answer.free_question_id`精确匹配的Answer。`:quiz`使用这对ID请求Activity并执行convert-to-quiz，attached Activity的IDs也必须匹配；只有其后续Attempt/Assessment可能形成Evidence，完成后仍需显式`:resume`。
- A21：`:resume` 只调用服务端 resume-focus；focus_frame_invalidated 时明确报告不可恢复且不拼接旧/新上下文。`:quit` 只退出客户端，不结束服务端 session；结束 Activity、切换 goal 和完成 session 是独立显式动作。
- A22：所有 learning 写操作生成 canonical lowercase UUID、schema version 1、正确 aggregate/path ownership 和 current expected version。transport/5xx 不确定结果只用同一 operation/request ID 有界重试；pairing response 不确定时不盲重试已消费代码。
- A23：`route`、`progress`、`evidence`和`reviews`使用稳定查询API。active session使用work item精确Route；无active session时`current_only=true`返回每个route ID的current revision分页集合，按稳定event/ID顺序显示且明确不是账户级唯一路线。Progress在session 404后仍显示全局Mastery、Evidence、Misconception和Review。
- A24：查询结果存在 rebuilding、degraded、incomplete、truncated 或 stale metadata 时，CLI 显示低调但不可忽略的 reason code、as-of event sequence、projection version 和 knowledge revision；estimated time 始终标记为估算。
- A25：课程、目标、检索词、题目和回答均来自用户输入、canonical Markdown 或模型 proposal；CLI 不硬编码技术面试、Go 或任何课程分类。
- A26：默认启动不显示 logo、欢迎横幅、emoji、动画、终端标题或显眼学习标识，默认 `color=never`，提示符为中性 `>`；只有显式设置才启用少量基本颜色，`NO_COLOR` 和非 TTY 始终纯文本。
- A27：交互 TTY 中 `:clear` 与 Ctrl-L 由当前进程清除可见 viewport 并只重绘空白中性提示符；`edu-agent clear` 提供同样动作。非 TTY 不输出清屏控制序列，也不调用外部 `clear`/`cls`。文档明确不承诺删除 scrollback、Shell history、系统或远程终端日志。
- A28：CLI 不持久化 Activity、Attempt、答案、Assessment、FreeQuestion、FreeAnswer、知识正文、路线或进度；M1 联网失败时不排队业务操作，只保留无正文配置和设备凭据。
- A29：HTTP client 使用固定 User-Agent、总超时、响应 body 上限、严格 content type 和类型化 error envelope；不得记录 Authorization、pairing code、token、答案、知识正文、model/upstream raw body 或服务端 secret。
- A30：401、403、404、409、413、422、429、500、503 以及 version/revision/idempotency/assessment/focus/stale-cursor/content-redacted 冲突映射为稳定短错误和非零退出码。自动重试只允许连接/EOF、502/504、合法Retry-After和明确allowlist的transient 503，总尝试最多两次；redacted/privacy/projection 503、pairing、logout和任何确定性冲突不自动重试。
- A31：OpenAPI 增加 `SessionWorkItem` 完整 closed schema、实际 read-gate 503 响应，以及 devices/model 路由真实 `x-required-scope`；CLI 窄 DTO 与 action/decision discriminator 由契约测试防止漂移。
- A32：CLI 单元和 fake HTTP 测试覆盖命令解析、URL/凭据故障注入与补偿、pairing、import review及服务重启后的stale receipt恢复、状态机每个可恢复状态、objective/open assessment、provisional门禁、自由问答/quiz/resume、并发冲突、重试分类、redaction、无敏感输出和TTY/非TTY renderer；`contracttests/fakellm`扩展为按proposal type返回确定性严格工件。
- A33：真实 PostgreSQL、实际 HTTP server、可编程 strict fake model和one-shot post-commit response-loss proxy组成多个黑盒场景：主教学、多行答案与非默认help、provisional重启后confirm/override、自由问答到attached quiz再显式resume、due review的present/provisional/accepted推进，以及同operation replay不重复Inbox/event/Evidence。两个CLI进程使用隔离配置并分别配对。
- A34：Linux、macOS和Windows cross-build通过但不作为行为证据。`.github/workflows/cli-platform.yml`在push、PR和workflow_dispatch上运行ubuntu/macos/windows原生矩阵，输出绑定candidate commit、OS/arch、Go version、命令和结果的artifact，执行credential round-trip/cleanup、pair无回显、line input、Ctrl-L和clear；任一原生证据缺失则blocked。
- A35：`edu-agent version`、CLI README、根 Makefile 和本地 release target 给出可复现构建方式、支持的平台、配置位置、在线限制、清屏边界和凭据处理；发布产物不包含配置、token 或学习内容。
- A36：现有Compose只验收部署连通、pair/import/query和Nocturne/model degraded语义，不承担完整模型教学流程；完整route/activity/assessment/free-answer由A33 actual-server harness验证。依赖模型的proposal失败时权威状态不变并允许稍后重试。

# Constraints and invariants

服务端 PostgreSQL 始终是 Goal、Session、Activity、Attempt、Assessment、Evidence、Mastery、Review 和知识 revision 的权威源。CLI 只能生成 operation/request ID、采集用户输入、读取本地 Markdown、展示响应和调用已有应用协议，不能生成 canonical event、评分、Evidence、Mastery、路线 revision、review date 或 memory admission。

OpenAPI 是 client/server 共享契约。CLI 使用独立 DTO 和 HTTP client，不共享服务端内部 Go 类型。任何包含用户或模型正文的恢复查询都必须服从现有 privacy generation/read-permit 语义。客户端不得通过本地缓存绕过 `content_redacted`。

默认模式优先低可见度和数据最小化。交互输入不写持久 history，命令输出不包含 secret，非 loopback HTTP 必须显式选择。所有自动重试都受幂等 ID 和有限预算约束。

# Decisions

二进制名称为 `edu-agent`，服务端保持 `edu-agentd`。命令式操作与单一交互式 `learn` 共存；`learn` 内部命令使用 `:` 前缀，普通文本只在服务端状态允许时解释为答案或追问。默认无颜色、无 banner、无持久交互历史。

客户端位于独立 module `clients/cli-go`，优先使用标准库 `flag`、`net/http`、`encoding/json` 和小型终端抽象；只为 UUID、Unicode normalization、TTY 和平台 credential 保护增加窄依赖。根目录不建立共享领域 module。

设备 token 与普通配置分离。保存采用fail-closed可补偿顺序而非跨backend原子事务；启动时拒绝orphan credential、缺失配置和device mismatch。Unix使用严格权限和原子文件，Windows使用当前用户绑定的系统保护；环境token只作为不落盘覆盖。配对码不进入argv。

继续学习所需正文通过现有`SessionView`的required nullable `work_item`提供，并补充provisional Feedback离态门禁和Routes `current_only`过滤；不增加“一次请求完成整轮教学”的万能端点。CLI仍调用既有retrieval、proposal、action、assessment decision和projection query。

`goal set` 复用唯一 current active session：存在 session 时显式 switch goal，不创建无提示的第二条 active session；不存在时创建 session。CLI 不维护本地 session registry。

# Open questions

- [blocking] CONFIRM: 确认按上述范围交付独立在线Go CLI，并允许本child补充`SessionView.work_item`、provisional Feedback离态门禁、Routes `current_only`、FreeQuestion单调顺序字段、三平台原生CI门禁和本地credential修复命令；默认无颜色且不持久化学习内容，离线、MCP、Fast Note Sync、Rust、Web与知识自动维护继续留在后续child。

# Verification expectations

开发期遵循 `docs/development/testing-strategy.md`：先运行CLI具名测试和受影响package，恢复视图改动只运行learning/tutoring/httpapi聚焦测试；稳定批次后才扩大。Builder candidate运行CLI完整test/race/vet/build、server受影响test/vet、OpenAPI契约、三平台cross-build、三平台原生credential/terminal矩阵，以及分场景真实PostgreSQL+可编程fake model+response-loss proxy黑盒门禁。Compose只在最终wiring稳定后运行一次；Nocturne overlay、OCI和privacy erasure输入未改变时复用已通过证据。
