# Go CLI M1 完整规格

## 产品与边界

Go CLI 是单用户自托管教学系统的第一个原生客户端。它提供在线、低可见度、可恢复的终端学习体验，使用用户导入的任意领域 Markdown 和服务端教学状态，不硬编码技术面试、Go 或其他课程。可执行文件名为 `edu-agent`，服务端管理进程继续使用 `edu-agentd`。

客户端位于独立 module `clients/cli-go`。它只依赖公开 HTTP/OpenAPI，不得导入 `server/internal`、直接访问 PostgreSQL、直接调用 Nocturne 或模型端点，也不得复制服务端 reducer、Assessment acceptance、Mastery、review scheduling 或 identity policy。客户端生成 operation/request UUID、采集输入、组装公开 proposal context、展示响应并调用服务端协议；服务端独占 canonical entity、event、状态转移、评分、Evidence 和复习日期。

M1 是在线客户端。除普通配置和设备凭据外，CLI 不持久化知识正文、Goal、Route、Activity、Attempt、答案、Assessment、Evidence、FreeQuestion、FreeAnswer、进度、cursor 或待发送 operation。网络不可用时不建立离线业务队列。

## 模块与构建

`clients/cli-go` 使用独立 `go.mod`，module path 为 `github.com/edu-agent/edu-agent/clients/cli-go`。推荐目录包含 `cmd/edu-agent`、`internal/command`、`internal/api`、`internal/config`、`internal/credentials`、`internal/terminal` 和 `internal/workflow`。接口由消费方定义，HTTP DTO 保持窄范围手写并由 OpenAPI 契约测试防漂移，不抽取共享领域 module。

根 Makefile 分别提供 server 和 CLI 的 fmt、test、race、vet 与 build，并提供组合 check。CLI与server固定使用Go 1.26.6；candidate必须记录实际toolchain，缺少精确toolchain时明确blocked。CLI release构建使用`CGO_ENABLED=0`、`-trimpath`和注入的version/commit，可交叉编译Linux、macOS和Windows的amd64/arm64；cross-build只证明编译，不替代原生平台行为证据。发布目录、压缩包和checksum不包含本地配置、token或学习内容。

客户端优先使用 Go 标准库。UUID、Unicode NFC、TTY/raw terminal 和平台 credential 保护可以使用窄依赖；依赖不得引入后台遥测、自动更新、Shell 执行或 CGO 发布要求。

## 命令面

命令树固定为下列用户能力，普通命令可以追加非敏感的 `--server`、`--timeout`、`--color`、`--verbose` 等全局设置，但 secret 不得成为命令参数。

```text
edu-agent pair [--server URL] [--name NAME]
edu-agent device status
edu-agent device forget-local
edu-agent logout
edu-agent knowledge import <file-or-directory>
edu-agent goal set <text>
edu-agent learn
edu-agent assessment show|confirm|override|void
edu-agent route
edu-agent progress
edu-agent evidence
edu-agent reviews
edu-agent clear
edu-agent version
```

`learn` 是交互式教学入口。内部命令使用 `:` 前缀：`:ask`、`:answer`、`:quiz`、`:resume`、`:assessment`、`:progress`、`:route`、`:reviews`、`:clear`、`:end`、`:complete`、`:quit` 和按需 `:help`。普通文本只在 `AwaitingResponse` 解释为答案；在 `FreeAnswer` 输入普通文本解释为追问并通过 `ask_free_question` 复用 active frame；其他状态不猜测意图，而是显示当前 allowed actions。

`:answer` 进入多行 block，使用单独一行 `.` 结束，不创建临时文件。`:quit` 只退出进程，不结束服务端 Session。`:end` 显式结束当前 Activity，`:complete` 只在服务端允许的状态完成 Session。切换 Goal、结束 Activity 或完成 Session 可能失效 FocusFrame，CLI 必须在提交前显示紧凑确认。

## 配置、凭据与连接安全

默认 server URL 为 `http://127.0.0.1:8080`。配置优先级为显式非敏感 flag、`EDU_AGENT_*` 环境变量、用户配置文件和内置默认值。配置使用 `os.UserConfigDir()/edu-agent`，严格 JSON 解码、原子替换并拒绝 symlink。普通配置保存 URL、device ID、display name、timeout、color 和显式 insecure 选择，不保存 token 或学习正文。

设备 token 使用独立 `CredentialStore`。Unix backend 使用用户私有目录 `0700`、文件 `0600`、同目录临时文件、fsync 与原子 rename，并拒绝 symlink、非普通文件和过宽权限。Windows backend 使用当前 Windows 用户绑定的数据保护或 Credential Manager；不以 `chmod 0600` 伪装 ACL 安全。`EDU_AGENT_TOKEN` 只作为当前进程的显式自动化覆盖，不落盘。

配置文件和credential backend不能组成跨存储原子事务。pair采用fail-closed可补偿流程：先持久化credential，再原子发布引用同一server/device的普通配置；配置发布失败时立即删除新credential。删除补偿失败会留下不可用orphan而不是可登录配置。每次启动先检查credential/config双向存在、device ID和server binding一致；orphan、missing half或mismatch均拒绝联网。

`device forget-local`是正式修复入口：显示当前本地binding摘要、要求交互确认、删除config和credential，不联网，并明确“远端设备可能仍有效，需用其他已配对设备吊销”。该命令也用于pair补偿失败fixture收敛。故障注入覆盖credential写前/后、配置rename前/后、补偿失败和forget-local cleanup。

`pair` 在 TTY 中无回显读取配对码，在非 TTY 中从 stdin 读取一行。默认不提供 `--code`，避免 Shell history 和 process list。配对成功只输出 server、device name 和 device ID，不输出 token。pairing exchange 的 transport/response-loss 结果具有歧义时不得自动重发已消费代码；CLI 报告 `pairing_result_unknown`，要求通过新的本地配对码恢复，并提醒用已有设备检查或吊销可能已创建的设备。

URL 只允许 `http` 或 `https`，拒绝 userinfo、query、fragment 和空 host。HTTP 仅允许 loopback host；非 loopback 必须是 HTTPS，除非用户显式保存 `allow_insecure_http=true`。显式不安全连接在每次联网命令输出稳定短警告。HTTP client 禁止自动 redirect，尤其不得把 Bearer token 发送到另一个 origin。系统 CA 是默认 TLS trust；自定义 CA 通过 Go 支持的标准环境或后续窄配置提供，不提供 `skip-verify`。

`device status`调用readiness、model capabilities和devices list，以本地device ID标识当前设备并显示scope/degraded状态，不显示credential。若当前token已被吊销，认证会先失败，CLI只能报告`authentication_failed`和“possibly revoked”，不能宣称读取到了authoritative revoked字段。`logout`先调用DELETE current device；只有远端成功或确认已不存在后才删除本地token/config。远端不可达或未确认错误时保留凭据并报告注销未完成。

## HTTP 客户端契约

OpenAPI 3.1 是唯一 client/server 共享协议。每个 request 设置固定 `User-Agent`、`Accept: application/json`、正确 `Content-Type`、总 deadline 和 bounded response body。客户端严格解码成功 DTO 与 `{error:{code,message,request_id}}`，拒绝错误 content type、过大 body、畸形 JSON、重复顶层值或与目标 DTO 不匹配的响应。

Bearer token 只进入 Authorization header，不进入日志、trace、错误、panic、fixture snapshot 或 verbose output。集中 redactor 同时保护 pairing code、device token、Markdown、答案、FreeQuestion/FreeAnswer、Assessment quotes、model/upstream raw body 和服务端 secret。默认日志只允许 operation/request/entity ID、HTTP method/path、状态码、稳定错误类别和耗时。

重试使用封闭方法表。连接错误、EOF、HTTP 502/504可以重试；429只有合法`Retry-After`且不超过总deadline时重试；503只有稳定code位于显式transient allowlist时重试，`content_redacted`、`privacy_clear_in_progress`和`projection_unavailable`永不自动重试。全部请求总尝试最多两次而非两次追加重试。

学习、知识和Assessment写操作只有在已有固定operation/request ID、序列化body字节完全相同时才能重放。pairing不自动重试。identity resolution只允许transport级同ID重放；`stale_identity_review`重新取得review并要求确认，`revision_conflict`停止并显示current head，`idempotency_conflict`停止并报告本地operation/body不一致，其他409按稳定code处理。logout不自动重试，用户再次执行时远端404可视为已不存在。版本冲突后先重新读取authoritative state，绝不自动重放用户答案、Goal切换、Assessment override或知识resolution。

退出码稳定区分成功、参数/本地输入错误、认证/授权、并发/业务冲突、依赖不可用和内部/协议错误。错误文本采用 `error[code]`、可空 request ID 和一个下一动作，不打印 raw response。

## Markdown 导入

`knowledge import` 接受一个 Markdown 文件或一个目录。文件导入使用 basename 作为 canonical relative path；目录导入以该目录为 root，递归读取 `.md` 文件，按 NFC relative slash path 排序。非 Markdown 文件忽略，目录没有 Markdown 时失败。

导入拒绝 symlink、socket、device、FIFO、路径逃逸、反斜杠 canonical path、NUL/控制字符、无效 UTF-8、非 NFC、空/dot segment、重复路径和 NFC + Unicode case-fold 冲突。客户端在网络请求前执行服务端公开限制：JSON body 16 MiB、单文档 4 MiB、最多 1000 文档、路径 512 code point/1024 UTF-8 bytes 和节点预算可由服务端最终裁决。本地绝对 root 不进入 source；source 使用稳定 `go-cli-m1` 标识或显式非敏感 label。

CLI 先读取 knowledge head。空库仍显式发送 `expected_parent_revision_id:null`，其他情况发送当前 revision ID。每次普通尝试生成 operation ID；transport uncertainty 保留相同 ID 和字节一致请求。`revision_conflict` 显示 current head，不自动重新基线或覆盖。

`identity_review_required` 不提交 revision、head 或 operation。CLI 显示 path、locator、reason code、候选 stable/revision ID、score 和经类型化筛选的 evidence。Document resolution 支持 preserve/new；Node resolution 支持 preserve/new/rewrite/split/merge。所有 resolution 需要非空 reason，preserve/rewrite/split/merge 要求明确 source ID。resolution 请求使用新的 operation ID，携带服务端返回的 basis hash、review operation ID 和 receipt；CLI 不伪造 receipt、不把路径/标题相似度自动提升为 preserve。

Review receipt属于服务进程内短期能力。若resolution收到`stale_identity_review`或服务重启使receipt失效，CLI不得自动重用旧decision；它用原始文档、同expected parent和原review operation重新请求review，展示新receipt及候选，并要求用户重新确认。fake HTTP和真实PostgreSQL聚焦测试覆盖review响应后服务重启。

## Session 恢复视图

现有`SessionView`增加required nullable字段`work_item`。`GET /v1/tutoring/sessions/current`和按ID查询使用相同schema：active stable state成功响应必须为对象，Completed by-ID必须为null；current无active session保持404。该变更是更新后的OpenAPI契约，严格拒绝未知响应字段的旧客户端必须重新生成。

`SessionWorkItem`是closed schema，包含`allowed_actions`及状态所需的`goal_revision`、`route_revision`、`activity`、`attempt`、`assessment`、`assessment_decision`、`free_question`、`free_answer`。GoalReady只要求Goal；Diagnostic要求Goal；RouteActive要求Goal+Route；ActivityIssued/AwaitingResponse要求Goal+Route+Activity；Evaluating再要求Attempt；Feedback再要求唯一Assessment和latest Decision。Idle、FocusSuspended、FocusResumed和AdvanceOrReview不得作为committed query终态，观察到即`projection_unavailable`。

Feedback不把Assessment ID写入Session reducer。Assembler必须以Session AttemptID反查数据库唯一Assessment。Decision version 1的`replaces_decision_id`必须为空；每个version N>1必须精确引用同一Assessment的version N-1；version连续后取最高Decision。缺失、多条、断裂或ownership不匹配均fail closed。Decision为provisional时application service拒绝acknowledge-feedback、end-activity、switch-goal及任何离开Feedback的tutoring action，直到confirm、override或void产生resolved Decision；`:quit`不调用服务端所以仍可用。

FreeQuestion typed record新增不可变`session_aggregate_version`，等于创建该Question的提交后session version，并通过新migration和唯一约束绑定session/frame/version。Work item按`(session_id, active_focus_frame_id, session_aggregate_version DESC)`选择current Question，再仅按该Question ID读取零或一个Answer。FreeQuestion state要求Answer不存在；FreeAnswer要求Answer存在且`free_answer.free_question_id`匹配。attached quiz Activity必须精确引用返回pair；任何历史frame或前一追问拼接均`projection_unavailable`。

`allowed_actions`和`allowed_assessment_decisions`由服务端纯函数按规范状态矩阵和已验证上下文导出。上下文不完整时整体work item失败，不能通过静默少列动作掩盖损坏。`present_review`要求current focus存在due review；`record_assessment`要求Activity+Attempt；`convert_free_answer_to_quiz`要求有效frame和current QA。provisional的confirmability必须用current Activity、Attempt和AssessmentArtifact执行服务端`ConfirmableAssessment`规则，不得只看disposition。CLI只把集合用于显示，apply/decision仍再次验证。

```text
GoalReady: start_diagnostic, switch_goal
Diagnostic: apply_route, switch_goal
RouteActive: apply_route, issue_activity, [present_review], record_exposure, ask_free_question, complete_session, switch_goal
ActivityIssued: present_activity, ask_free_question, end_activity, switch_goal
AwaitingResponse: submit_attempt, ask_free_question, end_activity, switch_goal
Evaluating: record_assessment, end_activity, switch_goal
Feedback + provisional + confirmable: no tutoring actions; decisions confirm, override, void
Feedback + provisional + not-confirmable: no tutoring actions; decisions override, void
Feedback + accepted|overridden: acknowledge_feedback, end_activity, switch_goal; decisions override, void
Feedback + voided: acknowledge_feedback, end_activity, switch_goal
FreeQuestion: record_free_answer, resume_focus, switch_goal
FreeAnswer: ask_free_question, convert_free_answer_to_quiz, resume_focus, switch_goal
Completed: no actions and no assessment decisions
```

QueryStore在一个repeatable-read read-only transaction内按固定顺序调用learning和tutoring owner的caller-DBTX reader，不得直接读取对方私有表或开启嵌套transaction。先取得`learning_generation`和`tutoring_generation`，二者必须相等；随后只读取一次active projection head/generation ID，并比较projection Session与authority Session的ID、state、aggregate version和focus。owner generation不等、gate关闭、scrub sentinel或response permit cancellation统一为HTTP 503 `content_redacted`；projection/head/checkpoint、record ownership/version损坏统一为HTTP 503 `projection_unavailable`。HTTP response permit持有到完整JSON缓冲即将写出。

Work item 只用于当前受认证设备恢复，不进入 timeline、日志、缓存或 Nocturne。EventRedacted 后旧正文不会重新出现。OpenAPI 为 current/by-ID session 的真实 503、closed `SessionWorkItem` 和各 nested schema 建立契约。

## Goal 与 Session

`goal set` 创建新的 GoalRevision，source 固定为 `go-cli-m1`。CLI 先读取 current session。没有 active session时使用新 GoalRevision创建 Session；存在 active session 时显示 current state 和 focus invalidation 影响，得到确认后以 current aggregate version执行 switch-goal。CLI 不创建未提示的第二个 active session，也不在本地保存 pending goal。

`learn` 首先读取 current SessionView/work item。没有 session 时提示 Goal 文本并执行 goal set。`GoalReady` 自动执行 `start_diagnostic`；`Diagnostic` 进入 route proposal；`Completed` 显示紧凑结果并要求显式 goal set 才开始新 Session。

每次自动 action 使用新 canonical lowercase UUID。一次 HTTP uncertainty 的重试保持同一 UUID 和 payload。每次成功响应后立即重新读取 work item，不以本地预计状态代替服务端状态。

## Proposal context 与教学循环

CLI 使用版本化 `go-cli-context-v1` 作为 proposal `input` 对象。该对象只组合 authoritative work item、Goal 文本、RouteStep、Activity/Attempt/rubric、用户当前 FreeQuestion 和 knowledge retrieval 返回的 canonical slice/range/hash；所有 ID 必须来自服务端。CLI 不生成 knowledge quote、Assessment quote/range/hash 或 canonical entity ID。

Route proposal 使用 Goal 文本查询当前 immutable knowledge revision。至少一个 retrieval hit 才可请求 route；degraded/truncated retrieval 显示 reason 后允许用户继续或退出。NodeRevisionIDs 来自 stable hit 顺序，input 保存 goal 和 bounded canonical snippets。服务端冻结 Goal/Knowledge/Node reference并验证 route proposal。CLI 显示 ordinal、紧凑 node label、teaching intent 和 completion condition后调用 apply-route。

RouteActive 时 CLI 可以基于当前 focus node 请求 Explanation proposal并调用 record-exposure。Explanation 明确显示为不可计分。随后请求 Activity proposal、显示 prompt、rubric item 数量、difficulty 和 allowed help，再调用 issue-activity。服务端 materialize Activity 后，work item 必须返回 immutable Activity；CLI 显示该 Activity 并调用 present-activity进入 AwaitingResponse。

AwaitingResponse 接受单行普通文本或`:answer`多行block。提交前用户选择Activity allowed help中的一项；只有`none`出现在集合时才默认`none`，集合只有一个非none值时也必须显示并确认，其他情况要求显式选择。CLI调用submit-attempt；响应不确定时使用相同operation ID对账。version conflict后丢弃自动提交计划、刷新work item并要求用户确认是否重新输入，不能自动提交第二个Attempt。

Evaluating 对 objective Activity 调用不带 proposal ID 的 record-assessment，由服务端确定性评分。对 open Activity，CLI 用 work item Activity、Attempt、rubric、answer和冻结 KnowledgeReference请求 assessment proposal，再把 proposal ID交给 record-assessment。CLI不能自行选择 accepted/provisional，也不能修改 confidence、risk flag或Evidence。

Feedback显示rubric item、`pass/partial/fail/unassessed`、confidence、risk flags、current disposition和Evidence状态。accepted只表示服务端current decision已接纳；confirmable provisional只提供confirm/override/void，不可confirm的provisional只提供override/void；`:quit`始终可用但不调用服务端。provisional不显示或发送acknowledge-feedback、end-activity或switch-goal。current Decision resolved后用户才能确认反馈并调用acknowledge-feedback。

RouteActive时CLI读取due reviews。若current focus node存在到期ReviewSchedule，用户可选择present-review；该操作仍先生成frozen Activity proposal，并以review Activity进入同一answer/assessment链。`ReviewPresented`本身不显示为成功，也不推进review step。provisional或voided review assessment不推进schedule；只有accepted Evidence使服务端推进，CLI随后通过reviews/progress验证新due date。

## Assessment 决策

`assessment show` 只显示 current work item AssessmentArtifact和AssessmentDecision；不存在时返回稳定 not-found/invalid-state。confirm仅对当前 provisional decision提交 current session expected version和decision version。

`assessment override`要求非空 reason，并逐 rubric item选择新 conclusion。CLI从 immutable AssessmentArtifact复制 rubric item ID、answer quote/range/hash、knowledge reference ID、knowledge quote/range/hash和可选原 misconception candidate；用户只能改变 conclusion、reason和显式候选文本。复制前逐字段确认 artifact仍属于 current Activity/Attempt，不能从屏幕文本重新计算或构造另一份引用。

`assessment void`要求非空 reason。confirm/override/void的409 disposition/version conflict使CLI刷新work item并重新显示current decision，不能自动覆盖。任何决定成功后重新查询Node/Evidence/Review以展示服务端实际结果。

## 自由问答与 Focus

`:ask QUESTION`在RouteActive、ActivityIssued或AwaitingResponse调用ask-free-question。服务端原子保存FocusFrame和FreeQuestion。CLI重新读取work item；FreeQuestion必须只有active frame内当前Question且Answer为空。CLI使用该question和current knowledge revision执行retrieval，生成FreeAnswer proposal并调用record-free-answer，再要求服务端返回精确关联该Question的Answer。显示文本时明确其为不计分exposure。

FreeAnswer提供`:resume`、`:quiz`、`:ask`和`:quit`。默认Enter等同于`:resume`，但attached quiz完成后不自动resume。追问从FreeAnswer调用ask-free-question并复用active frame；进入新FreeQuestion后旧Answer不得继续出现在work item。

`:quiz`读取work item的FreeQuestion/FreeAnswer ID和text，基于相同knowledge revision请求Activity proposal，再调用convert-free-answer-to-quiz。attached Activity正常经历present、Attempt、Assessment和Feedback；acknowledge-feedback后返回FreeAnswer，用户再显式resume。FreeQuestion、FreeAnswer、转换动作本身不创建Evidence。

`:resume`只调用resume-focus。`focus_frame_invalidated`显示稳定错误并清除本地显示中的旧work item，不构造混合revision。`:end`、goal switch和session complete都先提示会使active frame失效。

## 路线、进度、证据与复习

`route`有active session时直接显示work item精确RouteRevision及step。无active session时调用Routes `current_only=true`，显示“每个route ID的current revision”分页集合，并明确这不是账户级唯一current route；稳定顺序为projection event sequence再route revision ID，默认limit 50、最大200，存在next cursor时显示truncated/next提示。显式history模式使用原Routes分页。CLI不从路线文本推断完成状态。

`progress`先读取ProjectionStatus，再把current session 404视为正常的“无active session”。有active session时直接使用work item route；没有时使用Routes `current_only=true`的稳定分页集合，不声称存在账户级唯一路线。默认只聚合当前页涉及的NodeView/Evidence/Reviews并显示next cursor/truncated；`--all`仍受配置的总页/节点预算，超预算明确截断。completed后仍可查看全局Mastery、Evidence、Misconception和Review；estimated active time只在有稳定session view时显示。

`evidence`和`reviews`直接映射稳定分页查询，支持bounded limit和cursor。只读`stale_cursor`可以在明确提示“结果已变化”后从第一页重新查询一次；CLI不把旧页与新generation拼接。estimated active time始终带“estimated”标记和sample count。

所有projection响应都检查`as_of_event_seq`、projection version、knowledge revision、generation、rebuilding、degraded、incomplete和reason code。任一完整性flag非正常时仍可显示服务端允许的内容，但必须在同一屏显示低调警告，不得声称完全最新。

## 低可见度终端行为

默认`color=never`，不显示logo、banner、emoji、spinner、动画、终端title、桌面notification或“面试训练”等显眼标签。提示符固定为中性`>`，标题使用`Current`、`Question`、`Result`、`Next`等短标签。知识和模型原文保持原语言，CLI不对内容做课程分类或翻译。

显式`--color=auto`只在stdout是TTY、`TERM != dumb`且没有`NO_COLOR`时使用少量8色强调；`always`仍不得把控制序列写入结构化文件，`never`和非TTY始终纯文本。交互输入不写持久history。

TTY中的`:clear`和Ctrl-L由当前进程清除可见viewport、把光标移到左上并仅重绘空白`>`，不得重印Activity、答案、rubric、route或progress。Windows优先使用Virtual Terminal Processing，不可用时使用受测试的Console API；Unix/macOS使用受测试的CSI。非TTY的`clear`不输出ANSI并返回可诊断结果。实现不得执行外部`clear`、`cls`或Shell。

README明确：clear不删除terminal scrollback、Shell history、OS audit、remote terminal log、服务端event、projection或credential。用户在Shell命令行直接输入Goal等文本仍可能进入Shell history；交互`learn`用于避免答案和问题出现在argv。

## 错误与降级

稳定错误显示为`error[code]`、可空request ID和一个next action。`authentication_failed`提示重新pair但不打印token；`forbidden`显示缺失scope；`revision_conflict`显示current head；`version_conflict`刷新session；`assessment_disposition_conflict`刷新decision；`focus_frame_invalidated`说明不可恢复；`stale_proposal`重新读取work item后允许重新请求；`stale_cursor`只用于读页重启；`content_redacted`丢弃当前正文显示；`privacy_clear_in_progress`不排队写入；`rate_limited`遵守Retry-After但不无限循环；`internal_error`和上游错误只显示request ID和稳定类别。

Nocturne未启用或不可用不阻止知识、Session和projection查询。模型不可用时，依赖route/activity/open assessment/free answer/explanation的操作保持原authoritative state并允许稍后重试；objective assessment仍可使用服务端确定性路径。CLI不得把proposal失败显示为Goal、Activity或Assessment已提交。

## OpenAPI 对齐

OpenAPI为devices list声明`devices:read`、device revoke声明`devices:manage`、model capabilities声明`model:probe`。Knowledge和Learning/Tutoring路由列出实际read-permit产生的503错误。Routes增加strict boolean `current_only` query并保持稳定分页排序。`SessionView.work_item`字段required且类型为object或null；allowed action与assessment-decision enum及Goal/Route/Activity/Attempt/Assessment/Decision/FreeQuestion/FreeAnswer nested schema均为`additionalProperties:false`。新migration为FreeQuestion增加session aggregate version及归属/唯一约束。

CLI契约测试从OpenAPI读取实际path、method、scope、request discriminator、response status和关键schema字段，防止手写DTO静默漂移。OpenAPI测试和生产handler测试必须同时更新。

## 验证

CLI领域单元测试覆盖命令解析、配置优先级、URL安全、credential可补偿保存/权限/orphan检测、redaction、renderer、clear、TTY/非TTY、closed retry matrix和退出码。Fake HTTP测试覆盖pair、device、knowledge normal/replay/revision conflict/identity review、review后服务重启、所有Session恢复状态、route/activity/assessment proposal、objective/open、provisional不显示ack/end/switch及confirmability决策、confirm/override/void、frame-scoped free QA/quiz/resume、version conflict、429、各类503、content redaction和malformed response。

`contracttests/fakellm`从仅capability probe扩展为可编程strict HTTP fixture，按proposal_type和scenario确定性返回route、activity、assessment、free_answer和explanation，并能产生accepted、provisional、risk、malformed与transient结果。一个纯Go one-shot loss proxy按method/path/operation ID只丢第一次post-commit响应，后续同body重放转发。

Server聚焦测试证明work item在同一projection/generation/read-gate snapshot读取正确typed records，双owner generation相等，projection/authority一致，Attempt反查唯一Assessment和连续predecessor链，frame-scoped QA不拼接历史，并分别验证provisional拒绝acknowledge-feedback、end-activity和switch-goal；跨session/route/activity/attempt/frame/version损坏fail closed，privacy barrier取消缓冲响应，EventRedacted后不恢复正文。OpenAPI验证closed schema、scope、current_only和真实错误。

真实M1黑盒门禁拆成独立场景而非单个长脚本。主流程覆盖pair、Markdown、goal、route、explanation、Activity和accepted assessment；输入场景覆盖`:answer`多行和非默认allowed help；provisional场景在Feedback退出进程并由另一个已配对CLI confirm/override；focus场景覆盖free answer、attached quiz、feedback后显式resume；review场景证明ReviewPresented和provisional/void不推进、accepted Evidence才推进；response-loss场景以数据库只读断言证明同operation只有一个Inbox结果、event批次和Evidence。每个进程使用隔离config home。

候选门禁按分层策略执行CLI完整test/race/vet/build、三平台cross-build、server受影响package/OpenAPI/migration检查，以及上述PostgreSQL黑盒场景。`.github/workflows/cli-platform.yml`在push、pull_request和workflow_dispatch触发ubuntu-latest、macos-latest、windows-latest原生矩阵，使用Go 1.26.6，运行credential round-trip/cleanup、pair无回显、line input、Ctrl-L和clear，并上传包含candidate SHA、runner OS/arch、`go version`、命令和结果的artifact。缺少任一平台通过artifact时验收blocked；cross-build不替代。Compose只运行pair/import/query和Nocturne/model degraded smoke，不接入fake model完成教学；完整模型流程属于actual-server黑盒harness。

## 非目标

离线queue/device sequence/sync裁决、Fast Note Sync/Obsidian、MCP、Agent知识维护、Rust、Web/TUI、多用户、内置PDF/网页parser、客户端模型key、客户端Nocturne访问、privacy grant签发和release signing不属于Go CLI M1。仅用于三平台CLI验收的checked-in workflow属于本child，不扩展为全仓release pipeline。后续能力必须继续通过服务端权威状态和OpenAPI扩展，不能把M1本地文件演变成第二业务真值。
