# Issue #1：本地工具能力交付计划（v2）

状态：用户已确认方案并授权实施；本文件是实施设计，不表示各批次已经完成或通过验收。本次 ambient resume probe 返回 `none`，不自行启动或切换 Comet workflow；后续若进入 Runtime，以其正式 continuation 为准。

## 用户结果与不变量

补齐完整本机 Shell、可管理长任务/stdin/PTY、可继续访问的大输出、大文件范围读取与局部修改、多文件文本补丁和完整 diff、目录检索续页、递归复制、归档恢复与主动清理。支持 Linux/macOS，不扩大 Windows 承诺。

Shell 使用启动客户端的 OS 用户权限，不增设命令白名单、工作区围栏、网络限制或默认逐命令审批。结构化文件工具原有 root/no-follow/expected_hash/授权/原子发布合同保持。文件确认模式不约束 Shell，工具说明与 TUI 必须披露。

一次等待结束不等于命令结束；执行状态、控制能力和输出可用性分别报告。正常退出先停止受管任务并结算，再关闭资源；重启不重放命令、不向旧 PID 发信号。模型请求失败不能撤销或抹掉已发生的本地副作用。

持久 Session 的长结果采用独立版本化加密记录，归属原 Session 并遵守其删除/密钥边界；不塞入 transcript、不保存原始调用参数、环境或 stdin。`--no-save` 自动采集的输出仅在内存保留，不创建临时历史文件；Shell 显式写文件不受此开关限制。

## 优化决策

- 普通较大文本采用完整读取、局部精确替换、完整候选和既有安全原子发布；预算与单次参数预算分离。超大文件使用 Shell/脚本，不先建设通用流式编辑引擎。
- `write(content)` 保持兼容；超过单次参数预算的内容通过受管 Shell 的多次 stdin 输入或本地生成路径构造，验证必须覆盖累计输入超过 64 KiB，而不只验证短命令生成大文件。
- patch 使用严格文本上下文和版本，不静默 fuzzy fallback。逐文件发布，不承诺跨文件原子事务或自动回滚；删除沿用归档。
- 长结果存储仅负责追加、范围分页、检索、保留状态；不承担内容上传、执行或持久查询数据库。
- 目录查询保存遍历状态，可限定游标仅在当前进程有效；失效、扫描不完整和结果未返回完不能混淆。
- 不新增强制逐页审批，不默认自动清理历史或归档，不绕过已有文件副作用日志容量/持久化失败门禁。

## 垂直交付单元

| 单元 | 结果与范围 | 延期 | 退出标准 |
| --- | --- | --- | --- |
| C1 | Shell、客户端级受管任务、管道 stdin、状态/等待/停止、进程内输出分页、模型与 TUI 接入 | 持久完整输出、PTY | 正常管道/重定向/外部 cwd/env；等待返回不杀进程；直接 UI 可看/停任务；>64 KiB 多次输入；子进程取消；失败 turn 不遗失任务事实 |
| C2 | 完整输出加密留存、分页检索、恢复事实与可用性 | PTY 重附着/守护进程 | 长结果不依赖 transcript；配额/缺口可见；重启读已保存范围，未知不重跑；no-save 零自动输出落盘 |
| C3 | PTY 输入、中断、EOF、resize | Windows、跨进程重附着 | 真实终端程序持续交互和结束，终端流与管道区别正确 |
| C4 | >1 MiB 范围读与长行续读 | GB 级低 I/O 承诺 | 后半文件可读，版本变化可见，完整 hash 不冒充局部 hash |
| C5 | 大文本局部 edit | 通用流式替换引擎 | 多处精确替换、失败不发布、版本复核和 BOM/换行/权限兼容 |
| C6 | 多文件patch及write/edit完整diff | 二进制 patch、Git index、跨文件事务 | 多 hunk 可续览；逐文件结果完整；无新增逐页审批 |
| C7 | list/find/search 续扫续页 | 持久查询数据库、全局快照 | >2000 项可继续；扫描/结果/遗漏/失效准确 |
| C8 | 递归和较大二进制 copy | 跟随源链接、完整 ACL/xattr | >32 MiB 文件可复制；冲突/源变化/部分完成可核对 |
| C9 | 定位并恢复已有归档 | 自动推断不可信原路径 | 默认不覆盖，可信回执或显式目标，实际位置清楚 |
| C10 | 用户发起归档清理 | 自动过期、后台清理 | 不跟随链接、逐项真实结算，不将逻辑大小冒称释放空间 |

C2/C3 依赖 C1；C5 复用 C4；C6 复用 C2/C5；C8/C9 复用 C7；C10 复用归档定位。默认一次一个写入批次，不能以底层函数或 mock 代替完整生产入口。

## C1 管理器边界

`internal/localexec` 只拥有本机进程和进程内输出，不导入 agentloop/controller/workspace/store。controller 注入客户端级 manager 和稳定 owner；Session 关闭/切换不直接销毁共享 manager。实际客户端 shutdown 才 Close。

计划 API：

- `New(Options) *Manager`
- `Start(ctx, owner, callID, StartArgs) (Snapshot, error)`：同 owner+callID 去重；ctx 只约束启动/调用等待，不作为进程生命期。
- `Status(owner, taskID) (Snapshot, error)`、`List(owner) []Snapshot`
- `Wait(ctx, owner, taskID, duration) (Snapshot, error)`：等待取消不停止任务。
- `Read(owner, taskID, stream, offset, limit) (OutputPage, error)`：stdout/stderr 独立字节偏移，非破坏读取。
- `WriteInput(ctx, owner, taskID, data) (InputResult, error)`、`CloseInput(owner, taskID) error`
- `Stop(ctx, owner, taskID) (Snapshot, error)`、`Close(ctx) error`

StartArgs 包含 command/cwd/shell/env（覆盖或删除）/stdin（是否保持管道打开）/timeout_ms（0 无总超时）。非交互非 login，正常 OS 路径/权限，无客户端额外围栏。失败返回稳定 code，不把 raw OS error/命令/env 放入错误或日志。仅支持 Linux/macOS，其他平台明确 unsupported。

Snapshot 包含 task_id/state/reason/exit_code（仅已知时）/实际 Shell/时间/控制能力/输出已接收与保留量/缺口/清理不完整标志；没有可恢复执行对象。OutputPage 保留原字节对应的 offset/next_offset（模型对非法 UTF-8/不安全控制字符使用 base64；TUI 安全文本呈现），工具投影若缩短必须同步调整游标或返回原位置。读取水位随运行增长；配额后保留已有前缀并继续排空，不能无限增长或自动删旧数据。默认预算集中可注入：256 个任务记录、16 个并发任务、每任务 8 MiB 输出、总 64 MiB 输出；拒绝额外启动前无副作用。停止宽限默认 2 秒，所有等待和输入受有界调用 context 控制。

进程组独立，停止先 TERM 再 KILL，报告可证明的 group 清理结果；主动脱离组的进程不作绝对清理承诺。主进程退出与输出 EOF/组内子进程收尾分开，不让继承管道的子进程造成无限 Wait。stdin 默认关闭，显式 stdin=true 后才保持输入以支持多次写入；取消输入不能谎称全部已写或自动重传。

## 验证与证据

按 docs/development/testing-strategy.md，从相关具名测试开始，通过后继续生产接入。稳定 C1 才运行受影响包与定向 race/端到端；不运行服务端、PostgreSQL、Compose 或付费模型。Linux 本机子进程证据不替代 macOS 原生证据。未运行/失败/环境限制如实记录，不连续重跑无变化命令。

## 实施记录

- 初始工作区：main@b65ad72，干净。
- C1–C6 已实现并通过各自 Linux 批次门禁；C7–C10 尚未实施。没有创建、归档或代替 Runtime 验收工作流，整个 issue 未完成。

### C1：已实现的垂直路径

`command` 创建客户端级 localexec manager → `agentcontroller` 绑定 Session owner/写入 dirty v7 意图 → `agentloop` 注册并执行 shell/task、专用预算投影 → 模型获得任务状态和非破坏输出字节页；F5 经 controller 直接读/停，不依赖 provider。原始调用参数及输出正文不写入稳定 history/context ledger。失败轮次保留任务事实，后台任务不会随模型失败被撤销。

切走仍有未收尾任务的 Session 时，以引用计数 lease 同时保留原 handle/store 及跨进程锁；切回借用 `Handle.Load` 并重新走正常预检，不解锁重开。失败目标只释放借用引用。每客户端至多一个按需 reaper，在任务 supervisor 退休后释放闲置 lease；退出先完成任务收尾，再释放当前/parked 资源。外部恢复/删除保持拒绝，本客户端可安全切回。无后台任务时保持既有切换路径。

小窗口保持所有工具及完整 schema，仅采用更紧凑的系统说明；不提高用户的4096配置或削弱参数/授权校验。实际覆盖4096窗口中的真实Shell调用及后续模型回复。

### C1：运行证据与修正

环境：Go1.26.6，Linux/amd64。以下 Go 命令在 `clients/cli-go` 执行：

- 全包门禁：`go test -count=1 -timeout=120s ./internal/localexec ./internal/agentlimits ./internal/agentsession ./internal/agentloop ./internal/agentcontroller ./internal/agentui ./internal/command`。首次 localexec/agentlimits/agentloop/agentcontroller/agentui 通过；发现两处本次引入的兼容问题：v5迁移测试仍固定期待v6，以及新增系统说明挤满4096窗口。
- 修正保留旧payload字节和严格旧字段验证，只将迁移后的目标版本断言改为当前dirty版本；小窗口缩短冗余说明而不删工具或改schema/预算。分别通过 `TestJournalDirtyV5MigrationFreezesOldShapeAndPreservesBytes`、`TestLocalExecutionSmallContextKeepsCompleteToolSet`、`TestAgentLaunchPassesContextCompactionMode`。
- 受影响包重验：`go test -count=1 -timeout=120s ./internal/agentsession ./internal/agentloop ./internal/agentcontroller ./internal/command` 全部通过；复用未失效的内核/agentlimits证据。任务面板补充空历史可用性说明后，具名测试及 `go test -count=1 -timeout=60s ./internal/agentui` 通过。
- `go vet` 上述七个包通过；UI最后一处呈现变更后单独重验vet通过。
- `go test -race -count=1 -timeout=90s ./internal/agentloop ./internal/agentcontroller -run '^(TestLocalExecution|TestLocalSessionLease)'` 通过；复用内核未变动时的 `go test -race -count=1 -timeout=60s ./internal/localexec` 通过证据，不重跑全仓race。
- 真实进程场景覆盖管道/重定向、外部cwd、环境覆盖/删除、非零退出、启动失败、超时、子进程停止、配额和分页、累计160KiB stdin、取消等待、模型失败保留事实，以及加密Session中后台任务跨Session往返/锁/删除/退出。UI测试覆盖provider忙时直接读停、独立游标、安全呈现及过期响应隔离。
- Linux完整CLI构建及 `version` 运行通过；Darwin/arm64完整CLI交叉构建通过。构建产物位于 `/tmp/edu-agent-c1-build.7dpDCk`，不纳入仓库。
- `git diff --check` 通过；会话范围 `lens_diagnostics(mode=all,severity=error)` 对已诊断85文件报告0错误，不是全项目扫描。旧store_helpers增量undefined告警已核实为陈旧诊断，相关定义/迁移测试及最新文件检查均有效。

相关七个包全部Go文件与go.mod/go.sum按路径排序后sha256列表的汇总摘要：`3dd077fa2065d88147e6fb6ae93fab68f899833f9b2345e45943529339f186c1`。最终文档/解释性注释不改变已验证运行行为。

### C1：保留限制与后续

- macOS尚无原生运行证据，Darwin waitid/进程组机制必须在受支持macOS runner完成原生验证；交叉构建不是运行通过。未扩大Windows支持。
- 不保证主动setsid脱离进程组的进程被清理；孤儿僵尸或无法确认EOF明确报告cleanup/output不完整，不制造成功。
- C1自动采集输出仅内存保留；重启控制/输出不可访问不代表旧进程仍运行或已成功结束。当前F5明确显示该限制，不自动重跑。
- 未运行服务端、PostgreSQL、Compose、付费模型、全平台矩阵或最终独立Verifier；本批次没有相关服务端/数据库改动，macOS及整体验收证据仍待对应边界。
- C1 当时的下一写入批次为C2；其实际交付记录见下。

## C2：完整输出留存与检索

结果：持久Session中，模型的`task read/search`与F5可读超出内存预算的保存范围，并在重启后访问已认证输出；保存状态和执行结果分开。范围为Session-owned加密blob、任务journal、恢复绑定、模型投影与F5检索；延期PTY、通用上传、全文索引与最终多平台验收。

### 实现边界

- `agentsession.Handle`新增ReadArtifact/WriteArtifact/ListArtifacts；不增加横向数据库或导出key。flat私有文件名绑定storageID/opaque name，独立kindArtifact v1与HKDF域隔离、随机revision/nonce、原子CAS发布。单blob最大512KiB；独立密文配额Session256MiB/profile1GiB/8192文件。根目录总扫描扩展但原核心目录预算仍单独校验；产物不计入对话64MiB预算。
- `localexec`的journal使用256KiB原始分段和严格18字段v1 metadata；只重写末段，先保存数据再发布可恢复prefix。保存原调用身份的一向摘要用于拒重，不保存原命令/env/stdin/PID。失败或不确定发布不推进已确认prefix，不重试、不扫描孤儿段冒认成功；最终执行结算是独立的一次保存尝试。
- BindArchive在目标预检完成/切换提交后绑定owner；原manager已有任务不会被历史覆盖。后台任务直到最终保存尝试结束才释放lease。已保存终态只读恢复；无final的记录恢复unknown。零已提交轮次但存在任务产物/目录加载失败的Session不误当空Session自动删除。
- 保存采用有界同步分段IO，不持有manager全局锁或回调controller；普通quota/返回的IO失败后继续排空，不因保存失败杀命令。OS底层不可中断IO不宣称可强制取消；未结算时保留资源并报告实际状态。原内存上限保持；新`--task-saved-output-limit`默认128MiB/任务。
- 有界、区分大小写的字面search支持512字节needle、最多100命中/页、单次1MiB扫描及跨段/重叠匹配。投影删去命中时游标回退到真正返回的范围；原始输出不进入stable checkpoint/ledger/title。F5的`/`检索、`n`继续，不使用模型或其游标；返回消息按Session generation和面板epoch隔离。
- delete保持wrapped-key-first并清理关联产物；clear推进generation后撤销旧handle读取。no-save完全不绑定产物backend。目录损坏在模型list的history_error和F5中可见，不把失败加载伪装为无历史。

### C2 验证记录

环境Go1.26.6、Linux/amd64；Go命令目录`clients/cli-go`。

- 叶子单元：`go test -count=1 -timeout=90s ./internal/agentsession -run '^TestArtifact'`（0.404s）与`go test -count=1 -timeout=90s ./internal/localexec -run '^TestPersistentOutput'`（0.440s）通过。
- 真实加密store/Shell→关闭重开→超内存分页和尾部检索、provider变更零发送、no-save零文件、clear撤销、未提交任务归档存活，以及模型/面板检索与投影的`go test -count=1 -timeout=90s ./internal/agentcontroller ./internal/agentloop ./internal/agentui ./internal/command -run '^TestLocalOutput'`通过。
- 额外不确定发布回归`TestPersistentOutputUncertainMetadataDoesNotReplay`通过；修正文档中错误的“写失败一定未提交”假设，真实后端允许unknown。
- 新schema/说明曾使4096窗口完成首次Shell后无法续答；仅精简冗余系统说明，不删工具/约束或提高配置。`TestLocalExecutionSmallContextKeepsCompleteToolSet`修正后通过。
- L4：`go test -count=1 -timeout=120s ./internal/localexec ./internal/agentsession ./internal/agentloop ./internal/agentcontroller ./internal/agentui ./internal/command`全部通过，对应六包vet通过。
- 定向race：`go test -race -count=1 -timeout=120s ./internal/localexec ./internal/agentcontroller ./internal/agentsession -run '^(TestPersistentOutput|TestLocalOutput|TestLocalSessionLease|TestArtifact)'`全部通过。
- Linux完整CLI构建/version运行及Darwin/arm64完整CLI交叉构建通过，产物`/tmp/edu-agent-c2-build.IlaSqQ`不纳入仓库。`git diff --check`通过；会话已诊断103文件的mode=all error检查为0，不是全项目扫描。
- 清除一个原先未引用且持续被增量诊断阻塞的私有publicationOutcome方法，不改DTO字段/编码/版本；现有agentsession全包回归通过。

上述六包Go文件和go.mod/go.sum按路径排序后的sha256列表汇总：`6118c3b1e6afd7303372076ea8450c9fcc31783bd9cd20be17eb2eed8079b037`。macOS仍缺原生运行证据，未做服务端/数据库/Compose/付费模型或最终Verifier；当前批次没有相关输入。C2当时的下一批次为C3，交付记录见下。

## C3：PTY交互

结果：正常Shell可选择真正PTY；模型与F5可持续输入、发送当前VINTR/VEOF及resize，输出明确合并，历史模式可恢复但不能控制。生产入口已实现并通过Linux批次检查，仍缺macOS原生及最终Verifier证据。

### 实现与边界

- `localexec.StartArgs/Snapshot`增加PTY/Rows/Cols；24×80默认，1..4096。复用已有creack/pty分配slave，但cmd.Start仍受manager启动屏障；使用Setsid+Setctty、CLOEXEC/nonblocking master副本，read/write独立关闭。ioctl通过SyscallConn保护FD身份。
- 复用原调度、输入gate、输出journal和Session lease。PTY只映射stdout，独立stderr请求明确拒绝；Linux正常末slave关闭EIO视为EOF，强制关闭/超时不能冒称完整。
- 中断/EOF发送当前termios字节，不等于保证信号生效/退出/永久关闭stdin；disabled/raw/canonical分别保持OS语义。CloseInput只支持pipe。显式交互Shell同task保留cd/export，新task不继承。
- Stop在未reap leader固定身份期间使用原root pgrp TERM/KILL；额外检查整个PTY session和必要的terminal hangup。不能证明job-control成员结束或存在僵尸时报告CleanupIncomplete，不追杀已释放PID/PGID。主动setsid脱离仍不保证收尾。
- 输出metadata v2严格21字段，增加PTY/Rows/Cols；v1严格保留18字段/原枚举并转换为pipe。加密容器与record v6/dirty v7不变。resize经原journal保存尺寸；保存失败与已发生的终端操作分开报告。
- 模型增加shell pty/rows/cols及task interrupt/eof/resize；严格拒绝action无关字段/不合法组合，沿用task_input无正文意图，不放宽WAL失败门禁。取消已发生前的resize不继续执行。
- F5的i为显式行式输入，草稿4096字节且不回显；Enter送行、Ctrl+C终端控制、空草稿Ctrl+D终端EOF、Esc离开输入、Ctrl+Q退出。人工输入按owner直接发送，不进入模型工具/重放队列，不借用当前model turn的WAL；程序回显仍可能保存并被后续模型读取。尺寸只在显式输入模式随窗口同步，单纯查看不会调整程序终端。
- 消息以Session generation、面板epoch和独立请求序号隔离；关闭面板不撤销已发出输入，部分输入只报告接受数、不自动重传。不是全屏终端模拟器。

### C3验证记录

Go1.26.6/Linux amd64；以下Go命令在clients/cli-go运行。

- 叶子内核：`go test -count=1 -timeout=90s ./internal/localexec -run '^TestPTY'`通过（0.681s）。
- 父代理新增模型schema/resize/input/merged read、控制WAL/取消、F5隐私/中断/EOF/resize/迟到回复、真实store/controller恢复及交互Shell状态隔离的定向测试通过。4096窗口新增schema曾挤占续答预算；仅精简冗余说明，未删工具或schema约束，原小窗口闭环回归已通过。
- L4：`go test -count=1 -timeout=120s ./internal/localexec ./internal/agentloop ./internal/agentcontroller ./internal/agentui ./internal/command`五包全过，同五包vet通过。
- race：`go test -race -count=1 -timeout=120s ./internal/localexec ./internal/agentcontroller ./internal/agentloop ./internal/agentui -run '^(TestPTY|TestLocalSessionLease)'`四包全过。
- Linux完整CLI构建和version运行、Darwin/arm64完整CLI及localexec测试二进制交叉构建、Windows/amd64 CLI交叉构建通过。后者不增加平台支持；macOS测试二进制未原生运行。产物`/tmp/edu-agent-c3-build.cTiXuO`不提交。
- `git diff --check`通过；mode=all的本会话86个已诊断文件无error，不是项目全扫。开发时缺失定义的旧告警经重读、primary LSP和实际编译确认已解决，没有以重复定义或忽略真实错误通过门禁。

上述五包Go文件及go.mod/go.sum按路径排序的sha256列表汇总：`4d60f6fa0ef56e365f68b81f905b4514e9fb990bd23081ce349d23ed4a9c17a5`。C4–C10尚未交付，最终整体恢复/隐私复核与macOS原生证据仍待完成。

## C1/C2恢复边界修正：WAL-only调用身份

在继续文件批次前，最小复现确认：首轮任务WAL已保存、Shell实际执行，但输出metadata写失败，随后在稳定checkpoint之前中断；恢复只保留计数notice、消费WAL，协议检查又无已提交ToolCall，原call_id可被重新执行。真实临时计数文件从`x`变为`xx`，并非测试环境失败。

修正不改变“不重放”的既有用户范围，不新增进程重附着：

- 消费含LocalEffects的WAL前，必须确认每个调用身份的独立加密标记。逻辑名`local_call_<sha256>`、v1固定正文只含版本及摘要，不存原调用ID、命令、env、stdin或PID；复用agentsession产物认证、配额、generation与删除边界，record/dirty schema保持不变。
- 标记只能按确认后的相同正文幂等复用。部分或未知保存失败不消费WAL、不重跑操作；后续恢复可认证已发布前缀。损坏/未来标记失败关闭，不覆盖旧证据。
- BeforeLocalExecution识别已记录身份；工具返回operation_outcome=unknown/replay=false，无进程记录时明确state unknown和输出不可访问，不把任务仍在running与旧输入结果未知混为一谈。合法新ID仍可运行；stop安全路径不被记录失败阻断。
- 零已提交轮次但拥有身份标记的Session不自动当空Session删除；no-save不创建标记。人工PTY输入仍不属于模型重放队列。

验证：`go test -count=1 -timeout=60s ./internal/agentcontroller ./internal/agentloop -run '^TestLocalRecovery'`相关场景通过；故障fixture先漏建active WAL而失败，补齐真实MarkDirty前置条件后定向通过，未放宽断言。覆盖二次恢复/立即关闭保留、原ID不执行、新ID可执行、失败保留WAL及已确认标记、格式/隐私与未知投影。

`go test -count=1 -timeout=120s ./internal/agentcontroller ./internal/agentloop`、两包vet及`go test -race -count=1 -timeout=90s ./internal/agentcontroller ./internal/agentloop -run '^(TestLocalRecovery|TestLocalSessionLease|TestPTY)'`均通过。Linux完整CLI构建/version运行通过，产物`/tmp/edu-agent-recovery-build.etq5GY/edu-agent`不提交；无新增平台接口，C3平台机制证据复用，不宣称本次运行了macOS原生。mode=all检查本会话39个已诊断文件无error，diff检查通过。

两包Go文件及go.mod/go.sum排序sha256列表汇总：`5b43fafb6879f4236f4ee814498a095b3a5b9125ef4acbfe85e6128326a66f03`。后续继续C4，不将此修正扩成独立测试基础设施项目。

## C4：大文件范围读取

已交付既有read入口的独立文件预算，默认64MiB、CLI `--file-read-limit BYTES`。新建、resume、picker新建及同客户端F2切换都使用当前客户端设置；不将其保存成Session权限或旧资源配置。write/edit、stat.hash、search的1MiB预算及原参数/结果预算保持，C5尚未实现。

实现仍是securefile安全全文snapshot、全文件UTF8/binary检查和原始SHA256（包括BOM/换行）；仅保留所选行窗口引用，不再创建全部行切片。内存/IO仍随文件大小增长，不能冒称流式或GB级低IO。超预算明确给出限制与调参/Shell替代，不返回局部hash。行/行内字节范围匹配实例预算，支持>1MiB长行和>百万行定位；零ReadFileBytes的旧完整Limits配置继承原FileBytes。

read专用live/history/recall和当前轮预算投影保留真实UTF8字节前缀，重算行/字节游标，保留完整hash及截断原因。路径无法放入有界投影时显式省略而非生成截短路径。原始起点恰在线尾时，先规范化实际正文起点（不扩大请求行窗口）；修正了缩短后指回上一行的可复现缺陷。完整结果元数据/一个rune都无法放入workspace结果预算时明确失败，不制造永久空成功页。

Linux验证（Go1.26.6，命令目录clients/cli-go）：

- `go test -count=1 -timeout=90s ./internal/workspace ./internal/agentloop -run '^TestLargeFileRead'` 与command/controller的同前缀定向测试通过。覆盖真实大文件后段、长行/百万行、完整hash/版本变化、UTF8及预算、模型按投影cursor读取下一页、客户端活动、CLI预算实际限制、resume/F2新建与切回配置。
- 首个内核测试将链接父目录拒绝固定为link_not_allowed；Linux O_DIRECTORY|O_NOFOLLOW实际返回not_directory。仅允许这两种拒绝码，保留不返回正文/路径泄漏的断言；定向重验通过。
- `go test -count=1 -timeout=120s ./internal/workspace ./internal/agentloop ./internal/agentcontroller ./internal/command`：workspace/controller/command通过，发现agentloop最小投影漏保留truncation_reason；补齐并移除冗余起始坐标，保持续读坐标。旧四调用budget fixture同时把无换行长行错误标作next_offset=20，修成同一行真实字节终点及按返回前缀推进的断言；agentloop全包重验通过，未提高模型预算或隐藏工具。
- 四个受影响包 `go vet` 通过；`go test -race -count=1 -timeout=90s ./internal/agentcontroller ./internal/agentloop -run '^TestLargeFileRead'`通过。
- Linux完整CLI构建/version运行、Darwin/arm64完整CLI交叉构建通过，产物`/tmp/edu-agent-c4-build.pHxlfs`不提交；仍无macOS原生运行证据。diff检查通过，mode=all对本会话已诊断75文件无error，不是全项目扫描。

四包Go文件及go.mod/go.sum排序sha256列表汇总：`a85e257f5a91900898d59873feebb57609c4eca86010a756f6b6e46c1f3bd895`。未运行数据库、Compose、全仓/全平台矩阵或付费模型；下一垂直批次C5大文本局部edit。

## C5：大文本局部编辑

已交付既有edit入口的独立原文/候选预算，默认64MiB，CLI `--file-edit-limit BYTES`；新建、resume、F2新建/切回沿用当前客户端设置，不保存为Session权限或旧预算。read继续独立配置，write/stat.hash/search保持1MiB，参数仍64KiB整JSON/批128KiB和最多32处精确替换。

- prepareEdit安全全文读取与完整原始SHA256校验不变。所有old_text对同一原文精确唯一匹配、排序拒绝重叠；先减全部旧范围，再累计归一后new_text，含BOM核查实际候选字节与溢出。顺序Builder构造候选，不为每次替换重新复制全文，不增加fuzzy。
- write和edit各自冻结其处理预算；commit的入口身份读取、版本读取及securefile.Publish最终ExpectedLimit复核都使用同一预算，避免prepare支持大文件而发布仍受1MiB限制。路径/归档/链接保护、候选/预览hash、授权、取消、权限、临时文件/fsync/原子发布及WAL/实际结算保持。
- 当前diff算法未改成完整多hunk或流式算法；只限制变化块预览正文构造为约limit+1字节，保留既有截断前缀和marker，避免分配必然丢弃的完整变化块字符串。仍有全文行数组、候选与IO，不宣称固定内存GB级编辑。完整diff及patch由C6交付，不新增逐页审批。
- 错误明确原文/候选处理预算及调参/Shell替代。CLI读取和编辑旗标共用数值解析器，但资源配置相互独立，当前实例工具说明显示实际edit额度。

Linux验证（Go1.26.6，命令目录clients/cli-go）：

- 叶子 `go test -count=1 -timeout=90s ./internal/workspace -run '^TestLargeFileEdit'`通过0.389s，覆盖>1MiB跨边界/远端32处替换、完整hash、预算冻结、成长/缩小、BOM/换行/权限、取消/拒绝/冲突/篡改及预览兼容。旧C4不扩大其他工具测试显式配置edit为旧额度，schema测试更新额度描述。
- 父代理 `go test -count=1 -timeout=90s ./internal/agentloop ./internal/agentcontroller ./internal/command -run '^TestLargeFileEdit'`通过，验证模型→授权→真实大文件发布→客户端成功活动、拒绝/取消/WAL失败不修改、外部变化拒绝、已修改后模型失败保留、真实加密store结算/恢复、CLI独立小read大edit预算、resume/F2实际执行不绕过当前小edit预算。
- `go test -count=1 -timeout=120s ./internal/workspace ./internal/agentloop ./internal/agentcontroller ./internal/command`四包全过，同四包vet通过。
- `go test -race -count=1 -timeout=90s ./internal/workspace ./internal/agentloop ./internal/agentcontroller -run '^(TestLargeFileEdit|TestLargeFileRead)'`三包通过。
- Linux完整CLI构建/version运行和Darwin/arm64完整CLI交叉构建通过，产物`/tmp/edu-agent-c5-build.CBxxzC`不提交；仍无macOS原生运行证据。diff检查通过，mode=all对本会话已诊断81文件无error，不是全项目扫描。

四包Go文件及go.mod/go.sum排序sha256列表汇总：`57c091477ba78cdfb220cbf3b381dc1a9763f5330d8e2d4e8a312a9482d38fdd`。C5当时的后续为C6–C10，未扩大测试为数据库、Compose或全平台矩阵。

## C6：补丁与完整差异

已贯通真实workspace准备、完整diff保留、一次授权、逐项WAL/发布/结算、模型artifact与F6浏览、加密恢复。未建立新Comet workflow；本节是Linux实现证据，不是Runtime、macOS原生或整个Issue最终验收。

### 实现与边界

- `workspace`在write/edit准备时冻结FullDiff；edit复用已知raw replacementRange，扩展3行context并合并邻接窗口，远端修改保留多hunk。write按共同前后行收敛单变化区；生成保留BOM/CRLF/无末LF，预算检查包含header/marker，不输出假完整的截断diff。仍有全文候选、行索引和IO，不是流式或最小diff承诺。
- `apply_patch`严格支持Begin/End Patch、Add/Update/Delete，Update只接受裸@@、精确唯一上下文及可选End of File；拒绝Move、输入no-newline标记、二进制、fuzzy、重叠/乱序以及规范化路径、大小写/祖先冲突。每个更新/删除必须携带完整hash。最多16文件，原文总和与候选总和分别默认64MiB；单项新增仍FileBytes，更新仍EditFileBytes。删除另外冻结内容hash并在原归档队列锁内复核，不把内容hash冒充entry-v1，不承诺跨进程CAS。
- 外层prepared只能一次ClaimPatchItems，直接Commit拒绝。Agent Loop生成域分离的逐项调用身份，按输入顺序BeforeFilePublication→CommitMutation→独立取消域的AfterFilePublication；没有跨文件事务或回滚。完成/未知先保护本轮副作用并逐项失效旧workspace证据，模型或保存失败不能抹去真实修改。
- 完整receipt分别保留每项路径、操作、归档位置、冻结版本、执行器确认的结果版本、publication outcome和独立保存错误；余项标not_started。Inline投影只能删整项，完整receipt可独立读取。准备的diff不代表实际执行，历史receipt不恢复批准或候选。
- `localartifact`仅保存不可变diff/receipt，不伪装为task，不建设上传或事务平台。默认128MiB/结果、256MiB仅内存累计、256记录/manager；生产每Session一实例。256KiB分段，严格v1 metadata绑定owner摘要/完整hash/段hash，全部段成功后才发布目录；未知写不重试、不删除证据。持久模式不缓存正文，Read/Search重新认证；Bind完整验证目录，失败不覆盖原绑定。孤儿片段不提升为完整结果；零对话且有片段或目录错误不误作空Session自动删除。
- controller在目标预检完成/切换提交后绑定当前Session handle，复用C2密钥、配额、delete/clear与privacy-generation fence；record v6/dirty v7未改变。`--no-save`只用内存，原始patch/检索参数、完整diff与artifact读出的正文不进入稳定checkpoint/ledger/标题。普通write/edit的既有有界预览仍按原投影合同处理。
- 模型`artifact list/read/search`与F6使用独立原始字节位置；进一步裁剪时重算next_offset，压缩metadata后仍尝试返回可容纳正文。F6优先选择pending diff，分页/检索/刷新不调用模型、批准或取消原pending；generation/epoch/request拒绝迟到回复。原mkdir/copy/move末页门槛不变，没有新的末页批准条件。
- CLI增加file-diff-limit（128MiB，兼作单产物上限）、file-patch-limit（64MiB）、artifact-memory-limit（256MiB）、artifact-max-records（256）。diff/内存最高1GiB、记录最高8192；当前客户端配置贯穿新建、resume、picker新建与F2，不保存为历史权限。write/edit/apply_patch/shell/task共享整JSON64KiB政策，批参数仍128KiB。

### C6验证记录

Go1.26.6/Linux amd64；Go命令目录为clients/cli-go。

- 内核`TestArtifact`、`TestCompleteDiff`、`TestPatchPlan`通过。完整diff使用独立hunk应用器重建候选；严格拒绝、预算、BOM/混合换行/EOF、Claim防篡改和一次性、授权后源变化均有具名测试。
- `TestFileArtifact`/`TestFilePatch`验证模型真实多文件→授权前完整diff→逐项WAL/结算→search/read→checkpoint、部分冲突/取消/WAL失败/结算失败/模型失败、YOLO、同调用身份不绑定另一份diff、真实Session加密三文件发布及删除归档、重启只读/provider门禁/clear、孤儿证据保留、no-save零历史文件、CLI预算和resume/F2当前预算；F6具名UI测试通过。
- 首次七包全量：`go test -count=1 -timeout=180s ./internal/localartifact ./internal/workspace ./internal/agentlimits ./internal/agentloop ./internal/agentcontroller ./internal/agentui ./internal/command`。五包通过；workspace旧测试仍期待11工具/禁patch，agentloop无Shell的4096四调用场景被新增schema挤满。更新严格预期为12工具（未放宽约束），小窗口仅收敛说明，保留问询显示列限制；不隐藏工具、不改输入/输出/执行预算。四调用断言保留至少512输出及完整总预算，而非固定必须恰好512。
- 受影响workspace/agentloop/agentcontroller全量重验通过；其余包复用未失效证据。七包go vet通过。最后receipt版本/保存错误字段补齐后，两相关包具名race测试及vet再次通过。
- `go test -race -count=1 -timeout=180s ./internal/localartifact ./internal/workspace ./internal/agentloop ./internal/agentcontroller ./internal/agentui -run '^Test(Artifact|CompleteDiff|PatchPlan|FileArtifact|FilePatch)'`全部通过；最后两包`^Test(FilePatch|FileArtifact)`定向race再次通过。
- Linux完整CLI构建/version实际运行及Darwin/arm64完整CLI交叉构建通过；产物`/tmp/edu-agent-c6-build.tf00an`不提交。仍无macOS原生运行证据，不扩大Windows。未运行服务端/数据库/Compose/付费模型或最终独立Verifier。
- `git diff --check`通过。七个相关包全部Go文件和go.mod/go.sum路径排序sha256列表汇总为`9dc912b1a3f58e959159b8e2426fef5a411dc73b544a63473e0157c9613dd982`。

C6提交只作checkpoint。下一主线为C7目录检索续页，随后C8递归复制、C9归档恢复、C10归档清理及最终独立恢复/隐私复核。
