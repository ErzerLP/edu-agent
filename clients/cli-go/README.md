# edu-agent Go CLI

`clients/cli-go` is an independent Go module for the online `edu-agent` client. It uses only the public HTTP/OpenAPI boundary and does not import server internals.

可恢复大批导入使用 `knowledge import jobs`：`new` 自动扫描并分段上传，`list/show` 找回原任务，`resume` 校验原来源后续传，`preview/confirm/continue --all` 按完整计划逐批发布，`cancel` 保留已入库结果并清理暂存。所有入口明确指定 `--space` 和 `--collection`；`new` 可用 `--id` 保留创建身份，列表可用 `--cursor` 翻页。TUI使用 `jobs browse --collection UUID`，也可从原导入向导来源页按F7进入本集合任务页，复用来源扫描和身份审阅组件。任务最多128 MiB/1000篇，有效24小时，单文件和请求上限不变；详情见[任务恢复与留存契约](../../docs/design/import-jobs.md)。

## 资料导入向导

主菜单 `i` 或 `knowledge import wizard` 打开独立全屏导入页面。学习区入口预选当前区，全局入口先选择区与已关联集合。来源页支持路径输入、Tab 补全、F2 浏览目录、F4 粘贴 UTF-8 原文；F3/F5 修改集合/学习区，F6 返回已有清单。扫描后空格多选，Ctrl+D 选择/排除同目录及子目录，`/` 搜索，`v` 查看正文，`e` 修改远端相对路径，Enter 请求服务端预览。包含/排除规则为逗号分隔的相对路径 glob；`private/**` 排除子树。

预览不发布版本。身份冲突按候选序号选择“更新原资料并保留历史”，或输入“新资料”“跳过”；章节改写、拆分和合并有明确选项，身份不会按同名文件自动继承。长候选、正文、差异可滚动和搜索；`v` 读取完整原资料。确认页 Ctrl+S 原子提交，Esc 返回修改；网络等待可取消，草稿与结果只保留在当前进程。结果未知时 `c` 核对原操作，`r` 仅重试同一确认；未查到不等于提交失败。结果页 `v` 进入资料详情，`g` 带本次资料进入目标管理，再由用户选择新建/编辑及是否加入资料，不自动建立目标、路线或证据。

脚本入口不进入全屏或等待交互：

```sh
edu-agent knowledge import scan --include '*.md,*.txt' --exclude 'private/**' ./notes
edu-agent --space 区ID knowledge import preview --collection 集合ID --request preview.json
edu-agent --space 区ID knowledge import confirm --collection 集合ID --request confirm.json
edu-agent --space 区ID knowledge import operation --collection 集合ID --id 原操作ID
edu-agent knowledge import help
```

扫描返回逐项 `status/reason/selected/document`；取明确选择的 `ready` 文档组成预览请求。`preview.json` 使用 `operation_id`、`expected_parent_revision_id`（空集合显式 `null`）、`source` 和 `documents`。`ready` 返回回执，`review` 返回身份候选；填写既有 document/node resolutions 和 review receipt，以新 operation 重新预览。`confirm.json` 为 `{"request": 原预览请求, "receipt": 预览回执}`。确认与核对返回相同的持久 summary，包含实际目标、文档 ID 和新增/更新/未变化计数；本地排除、错误和不支持项不计入成功。

只支持 Markdown、UTF-8 纯文本和粘贴；纯文本转换保留原始内容、相对来源名、类型及 SHA-256，不公开绝对路径。单文件含转换后内容至多4 MiB，扫描至多5000项/64层，批次至多1000篇/12 MiB内容，最终 HTTP JSON 仍受16 MiB限制。回执15分钟内有效，服务重启或输入/目标/父版本/隐私 generation 改变需重新预览；已经提交的原操作可跨重启核对。旧 `knowledge import 路径` 仍保留，非 TTY 遇身份审阅输出结构化审阅并退出；需要可靠人工确认与原操作核对时使用新入口。完整契约见 [导入向导设计](../../docs/design/import-wizard.md)。

## Build

Go 1.26.6 is required.

```sh
make cli-test
make cli-vet
make cli-build
./clients/cli-go/bin/edu-agent version
```

当前版本的受支持运行与发布平台为 Linux 和 macOS。`make cli-cross-build` 仍可能生成包含 Windows 的交叉编译产物，但交叉编译只证明代码可编译，不代表 Windows 获得支持或兼容性承诺。`.github/workflows/cli-platform.yml` 在 native Linux 与 macOS runner 上运行绑定 SHA 的 root-confinement、credential、hidden/line input、Ctrl-L、clear 和 Session 安全检查，并按 runner 上传证据；缺少支持平台的原生机制或 artifact 仍是发布阻塞项。

`make cli-release` writes binaries and `SHA256SUMS` under `clients/cli-go/dist/`. The release directory contains no configuration, credentials, or learning content.

## 交互式中文 TUI

在支持全屏控制的交互终端中直接运行 `edu-agent`，即可打开中文主控制台。方向键或 `j/k` 移动，Enter 选择，界面显示的字母可直接打开 AI 学习助手、结构化学习、知识导入、目标、进度、复习、配对和设置等常用流程。

设置页可在配对前管理客户端请求超时、输出颜色和本地 AI 模型。模型配置支持 OpenAI、DeepSeek、OpenRouter、Ollama 及自定义 OpenAI 兼容端点；可调整 Base URL、模型名称、上下文窗口、最大输出 tokens、无响应超时和可选工具轮数保护值。新模型配置默认总窗口 `272000`、最大输出 `128000` tokens；这些默认值不是上限。窗口至少 `4096`，最大输出接受任意可表示的正整数，模型无响应超时接受任意可表示的正时长（例如 `10m`、`30m`），不再固定封顶；保留类型和溢出校验，已有显式配置不自动覆盖。每次请求保留 5% 安全余量，必要时收缩当次输出额度；默认不代表所有 provider/model 支持该容量。工具轮数默认不限制，配置 `0` 表示持续运行到模型给出最终回答、需要用户交互、用户取消、超时或上下文管理无法继续；正整数只作为用户自选保护值，客户端不设置固定最大值。流式请求的无响应超时从请求发出后开始计算，并在收到任意 SSE 响应字节（包括心跳、隐藏推理、工具增量和跨分片内容）时重置；持续活跃的 SSE 不受固定总时长限制。非流式请求仍以该值限制整次等待。API Key 使用隐藏输入并按“提供商 + Base URL”绑定到独立的系统凭据槽，不会写入 `config.json`、命令输出或日志，也不会在切换端点时发送给另一个服务。Ollama 和无鉴权的自定义 loopback 端点可不配置 API Key；云端预设和远程自定义端点必须读取当前端点绑定的 Key，系统凭据后端不可用时会失败关闭。服务器地址和设备凭据仍通过配对流程变更。

已配对客户端更换服务器时有两条明确路径：旧服务器可用时先安全注销并撤销远端设备；旧服务器不可用时可选择“仅清除本地配对”，保留客户端和模型设置，同时明确警告远端设备可能仍有效。损坏或不一致的本地配对状态只显示修复、设置和退出入口；Enter 本身不会确认撤销、删除凭据或保存长期偏好。

已配对后，主菜单按 `b` 进入持续学习工作台；`z/g/o/i/l/w` 也可直接打开对应页面。同一全屏内的常用流程为：选择学习区 → 资料集合/文档/章节 → 冻结资料范围 → 新建目标 → 明确新建或选择教学会话 → 作答、反馈、自由问答、恢复原焦点 → 查看复习。保存目标不会创建或切换教学。方向键或 Tab 选择底部可见操作，Enter 执行；`1–7` 切页，`PgUp/PgDn` 滚动正文，`?` 打开帮助。多字段表单用 Tab/Shift+Tab 切字段，枚举可用左右键选择；多行输入中 Enter 换行、Ctrl+S 提交、Esc 保留草稿返回，普通字母不会触发导航。目标生命周期、资料共享/解除引用等操作保留确认。

请求异步执行，Esc 取消等待；取消不代表服务端事务回滚，应刷新确认。返回页面会重新读取服务端，草稿、选中项和滚动位置仅保留在当前进程，按学习区隔离；答案草稿还绑定教学会话、目标和活动。保留最多约 256 个页面、每页 32 个输入草稿，达到上限明确拒绝新增，不静默淘汰或写入明文文件。过小终端可用 Esc 返回或 Ctrl+C 退出。

资料页复用集合、资料树和冻结范围接口；全局共享目录明确标出来源，关联后才纳入本区。目标页支持结构化编辑、历史修订、资料绑定和独立生命周期；学习页按明确会话显示题目、讲解、帮助等级、反馈、评估覆盖/作废、自由问答及恢复焦点。学习区和教学目标保持可辨，教学会话选择与 AI 聊天历史分开；聊天仍通过主菜单恢复入口与 Agent 内 F2 picker 管理。

导入页嵌入原完整向导，沿用扫描、身份审批、预览和原子确认，不解析命令输出。`F10` 返回工作台并保留草稿；切区须先返回外壳，各区草稿独立。再次进入会重查集合并使旧预览失效，未知提交仍保留原操作用于核对。结果页 `g` 以本次回执资料建立冻结范围并打开目标草稿，不自动保存或开始教学；也可在资料页选定范围后绑定已有目标。`t` 与 `i` 均进入嵌入向导。

非默认区资料、目标、教学依服务端能力开放；旧服务不支持时明确报错，不读取默认区数据冒充结果。非默认区跨目标复习总览、导入后台任务、AI 规划、进度总览和 Agent 区域绑定不在本外壳实现；合法空结果与服务错误分别显示。

显式子命令保留脚本输入输出；非 TTY、`TERM=dumb` 或显式传入子命令时不会输出全屏控制序列。设置等原命令动作结束后仍按 Enter 返回主控制台。工作台取消仅释放自身请求，不接管或取消 Agent 会话的本地任务。实现与验收记录见[工作台设计](../../docs/design/learning-workbench.md)。

## 客户端 AI 学习助手

“AI 学习助手”使用客户端本地配置的 OpenAI 兼容模型，不读取或修改服务端教学模型配置。模型自身不持久化学习状态；Agent Loop 通过受限工具从服务端读取知识目录、学习进度、路线、复习和已接纳的长期偏好。Agent Loop 不再对模型工具轮数、单响应工具调用数量、单 turn 总工具调用数或用户问询次数设置固定上限，而是像 Codex 一样持续执行“模型推理 → 工具结果 → 再推理”，直到模型给出最终回答、需要用户交互、用户取消、超时或上下文管理无法继续。用户输入、模型回答、工具参数、工具结果投影、模型响应体及每次发给模型的上下文仍有独立资源和协议边界；越界响应会失败关闭，不会放大为无界内存、服务端读取或模型流量。所有模型、工具、错误和确认文本在渲染前都会移除终端及双向文本控制字符。

在 Agent 对话中按 `Ctrl+O` 展开/收起执行详情和模型接口实际返回的推理文本或摘要，默认折叠，展开后随 SSE 更新。没有可读推理时明确提示未提供，并显示阶段、耗时和最近收到响应正文数据的时间（包括心跳）；客户端不会推测或补写思考链。推理与最终回答分开，仅在当前 TUI 内存中保留，不进入加密会话历史、checkpoint、Source、自动标题、日志、长期记忆或后续模型请求；切换 Session、重启和退出后不可恢复。显示缓存总正文预算为 1 MiB，优先回收旧请求正文，当前请求超限明确标注截断但不中断模型执行；这不是模型 token 或超时配置上限。取消或失败后仍可查看尚保留的部分，并保留请求真实终态。已经显示的内容可能被外部录屏或终端记录捕获。

新 Agent Session 默认在稳定轮次后使用系统钥匙串保护的本地密钥自动加密保存；当前支持平台为 Linux 与 macOS，Windows Session 恢复不属于本版本的支持或验收范围。可通过 `edu-agent agent resume` 打开共享 picker，或用 `resume --last`、`resume <UUID|标题>` 恢复；运行中的 TUI 空闲时按 `F2` 可打开同一套 picker，并支持当前/全部工作区范围、搜索、恢复、重命名、二次确认删除和新建 Session。provider endpoint 变化时，确认面板中 `Enter` 允许向新 provider 发送历史上下文，`L` 只在本地打开并继续阻止模型请求和自动标题，`Esc` 取消切换；仅本地打开后仍可查看 transcript、重命名或删除。工作台“AI助手与模型”设置以及 `edu-agent model set --session-history auto|off`/`config set --session-history auto|off` 只控制后续新 Session；`off` 会跳过存储打开，但不影响恢复和删除已有历史。`--no-save` 仅让当前新 Session 不落盘且不修改配置；历史不会按时间自动删除，达到硬上限也不会自动淘汰，可用 `agent sessions delete <UUID> --confirmed` 删除普通或可定位的损坏 Session；极端损坏且无可信 UUID 时，只能在明确选择后使用 UI 显示的 `storage:<id>` locator。已认证的未来版本必须升级客户端处理，当前版本不会恢复、修改或删除。`agent sessions clear --confirmed` 可清理全部本地 Session。自动标题会额外向当前模型提供商发送有界、清理后的已提交用户文本和安全最终回答，不包含工具调用或结果、工具参数、workspace/server 来源、回执、错误原文或推理；恢复后的下一模型请求会把历史上下文发送到当前 provider，端点身份变化时必须先明确确认。恢复继续绑定保存的工作区；旧 root 不可用或不安全时只恢复本地对话并禁用全部文件工具，绝不回退到当前目录，历史文件正文也不代表磁盘当前内容。跨进程恢复和 TUI 切换都会把 YOLO、旧文件授权与未完成交互重置为安全默认。平台钥匙服务不可用时不会回退到明文，而是明确降级为不可恢复的未保存会话。`agent sessions clear` 只清除本地 Agent Session store，不清除服务端事件、Nocturne、终端 scrollback、Shell history、provider retention 或 OS backup。

Agent TUI 不再设置独立顶部状态栏或全宽分隔线，transcript 直接使用页面顶部空间；产品标识在宽终端合并到右侧概览标题，在窄终端进入消息输入区下方的状态区。输入区是按内容增长的有界多行 composer：`Enter` 发送，`Ctrl+J` 或 `Alt+Enter` 换行，字符计数显示 `8000` 上限。普通对话状态下，鼠标滚轮、`↑/↓` 与 `PgUp/PgDn` 均可滚动 transcript；向上离开底部后暂停自动跟随并提示有新消息，滚回底部后恢复跟随。选择面板激活时 `↑/↓` 仍用于移动选项焦点，`PgUp/PgDn` 和鼠标滚轮继续查看 transcript。宽终端右侧概览显示当前 Agent 状态、服务端权威学习目标、会话状态、路线进度、当前 Activity 与估算活跃时间；它在启动、完整 turn 结束或 `Ctrl+R` 时刷新，读取失败不阻断对话，窄终端会完全折叠侧栏。侧栏不显示任何 opaque ID、凭据、隐藏推理或原始工具参数，也不持久化学习状态副本。

### 大窗口与长回答

主请求默认采用 272k 总窗口和 128k 最大输出；预留完整输出与 5% 安全余量时，输入总预算为 130400 tokens，包含系统规则、工具定义、记忆和消息。普通及流式助手正文上限为 1 MiB（字节不是 token），合法长文本可在有界 Session 配额内完整保存和恢复。长历史优先保留原文，必要时仅对较早助手正文作带来源、哈希和可见降级标记的请求投影，不改写保存原文或拆开工具调用组；`context_compaction=off` 不静默裁剪。标题与后台记忆整理继续使用独立小输出预算。具体资源边界及未验证范围见 [大窗口设计](../../docs/design/client-agent-large-context.md)。

### 本地 Shell 与受管任务

Linux/macOS Agent 现在提供 `shell` 和 `task`。Shell 使用启动客户端的 OS 用户权限，可访问工作区外路径、网络和已安装工具，支持管道、重定向及脚本；**文件工具的逐次确认和 YOLO 均不约束 Shell**。默认 cwd 是保存的工作区/启动目录，但不是路径围栏；不可用时须显式指定有效 cwd，不自动回退。可指定 Shell 和环境变量覆盖/删除；默认选可用的启动环境 `$SHELL`，否则 `/bin/sh`，不额外注入钥匙串凭据。

每次 `shell` 均独立，默认以 `shell -c` 启动，不自动附加 login/交互参数，也不继承前次的 `cd/export/alias`。`wait_ms` 默认250毫秒、最多30000毫秒，0表示立即后台返回；它不是执行超时，`timeout_ms=0` 默认不限制执行总时长。`task` 支持 `list/status/read/search/wait/input/close_input/stop/interrupt/eof/resize`；任务ID、真实状态、退出码和输出可用性分别返回。取消已存在任务的等待只停止等待，取消新 Shell 首次前台等待会发起停止；后台返回后的模型错误或轮次结束不会杀任务。正常退出客户端先停止任务和普通组内子进程，再关闭会话；主动脱离进程组的进程不保证收尾，清理不完整会明确报告。

`stdin=true` 保持输入管道，可多次 `task input` 后显式 `close_input`，累计内容可超过64KiB；它可用于通过脚本完整写入超过单次参数预算的文件。单次 `shell/task` 的整 JSON 参数仍不超过64KiB，输入结果说明管道已接受的字节数，不代表子进程已处理，部分/未知输入不能自动重传。不新增通用分块上传协议。

后台任务在空闲 Session 切换后继续归属原 Session；未收尾期间保留原 Session 在用锁，其他客户端不能恢复/删除。同客户端可安全切回，任务收尾后释放闲置锁。按 **F5** 打开当前 Session 的任务面板，不依赖模型调用成功。`↑/↓` 选任务、`Tab` 切 stdout/stderr、`PgUp/PgDn` 按原始字节翻页、`Ctrl+↑/↓` 滚动、`Home` 回到开头、`/` 输入字面检索、`n` 继续匹配/扫描、`s` 停止；`Esc/F5` 只关闭面板，不取消当前模型轮次。输出安全呈现，不直接回放终端控制序列。模型与 UI 使用独立、非破坏字节游标。

`pty=true` 创建真实控制终端并保持输入，默认24行80列，`rows/cols`可指定1..4096；不允许同时显式`stdin=false`。需要持续Shell状态时可显式运行`exec /bin/sh -i`，继续使用同一task_id；新task不继承它的环境或cwd。stdout/stderr合并为stdout流，`task interrupt/eof`发送当前VINTR/VEOF终端字节，受程序termios控制，不保证结束；`resize`要求rows/cols。`close_input`仍仅用于pipe。恢复可读PTY历史，但不能附着或控制旧进程。

F5按 **i** 进入PTY行式输入（不是原始按键直通/全屏终端模拟器），草稿最多4096字节且不回显、不直接送模型；Enter发送一行，Ctrl+C终端中断，空草稿时Ctrl+D终端EOF，Esc先离开输入，F5关闭查看，Ctrl+Q始终退出客户端。进入输入模式及窗口变化会更新PTY尺寸。输入不加入模型工具或恢复重放队列；**程序回显属于真实输出，可能加密保存并被模型后续读取**。部分输入报告实际接受数，不自动重传；关闭面板不保证撤销在途输入。job-control子进程无法确认收尾时明确报告清理不完整，不冒险向已释放的PID/PGID追发信号。

pipe输出 stdout/stderr 各自有序，PTY为单一合并流；内存预算保留已有前缀，持久 Session 另以独立加密产物保存完整的可保留范围，超过内存的部分仍可分页读取。达到保存预算或写失败会继续排空管道并标明缺口，不自动淘汰或暗中杀命令。任务状态、当前可读量、已保存量和完整性分别显示，退出码为0不等于完整输出已保存。新建与 `resume` 均可指定 `--task-max-records`（默认256）、`--task-max-running`（默认16）、`--task-output-limit`（每任务内存默认8388608字节）、`--task-total-output-limit`（客户端内存默认67108864字节）和 `--task-saved-output-limit`（每任务保存正文默认134217728字节）。产物另有集中可注入的密文上限：每Session256MiB、每profile1GiB及8192个产物文件；不占用聊天record的64MiB配额。

`task search` 使用 `stream/needle/offset/limit` 做区分大小写的字面检索，needle最多512字节，limit为命中数（最多100），单次扫描最多1MiB，用 `next_offset` 继续；跨分段匹配和输出缺口都会明确处理。模型与F5检索游标彼此独立。稳定历史仍只保存任务元数据，不保存原始命令/env/stdin或输出正文；消费任务WAL前另外保存认证加密的调用身份摘要，即使输出保存失败、首轮未提交即中断，也不能以相同调用ID再次执行。标记失败保留WAL，合法新ID不被永久阻断；身份标记使用同一Session产物配额并随Session delete/clear清理。原始输出只进入认证加密的独立产物，不自动进入标题/聊天压缩。重启读取已保存范围，不恢复进程控制；缺少最终结算的任务报告未知，不重跑或向旧PID发信号。Session delete/clear同时清理关联产物并遵守原密钥撤销边界；具有任务产物的零已提交轮次Session不会误当空Session自动删除。`--no-save` 不创建自动输出临时文件，Shell 自身的显式写文件仍有效。C1–C3已提供相应能力，本段不表示整个issue#1已交付。见 [实施计划](../../docs/design/client-local-tools-delivery.md)。

### 本地工作区与文件工具

Agent 会在 Session 启动时固定一个本地工作区：`edu-agent agent` 默认使用 Agent 启动目录，也可通过 `edu-agent agent --workspace PATH` 显式指定。模型获得相对路径上的 `stat` 元数据检查、`find` 路径发现、`list`、`read`、`search`、`write`、`edit` 文本工具、`apply_patch` 多文件文本补丁、`mkdir` 目录创建、`copy` 普通文件流式复制及目录递归复制、`move` 文件或目录安全移动，以及 `archive` 文件/目录归档、`restore_archive` 显式恢复和 `purge_archive` 永久归档清理工具；不提供归档外通用永久 delete。Shell/task 是上述独立本地执行通道，不继承以下结构化文件工具限制。内容访问和修改拒绝源链接、junction、reparse point、绝对路径及工作区逃逸；`stat`可以仅报告末端链接类型，但不跟随它。除专用归档目录禁止普通写入外，隐藏文件、`.git`、`.comet`、`.env` 遵循普通文件规则，读取到的内容可能发送给当前配置的本地或远端模型 provider。

`stat` 默认只读元数据，入口版本不代表文件内容或整个目录快照；可选 `hash=true` 只在1MiB内计算普通文件原始SHA256，不返回正文。`find` 支持basename或工作区相对路径glob，独立`**`跨零或多层；默认保留隐藏文件、跳过归档树、不读正文，并明确标记扫描/结果截断。详见 [stat](../../docs/design/client-file-stat.md) 和 [find](../../docs/design/client-file-find.md)。

`search` 现在支持 `output=content|files|count`：文件列表/统计模式不返回正文，不完整时以 `counts_partial` 标记局部结果；`context=1..3` 可为content附加去重、有界邻近行。新 `glob` 支持组件 `**`，不改变旧include/exclude语义。`find/search` 可显式设置 `respect_gitignore=true` 读取工作区内有界分层规则；默认仍不启用，错误规则不会被当作空规则扩大范围，ignore也不是权限保护。详见 [检索增强设计](../../docs/design/client-file-search-enhancement.md)。

#### 可续扫的目录与检索

`list/find/search` 可通过完整原参数与 `next_cursor` 继续同一查询，覆盖超过单目录2000项及单次扫描/结果窗口；扫描过程中可能先返回无正文进度页，不能据此判断无匹配。`scan_finished` 表示扫描结束，`scan_complete` 表示范围完整，`more` 表示还有扫描或未返回结果；永久缺口在 `scan_error` 中披露。content可继续同文件的后续匹配，files每文件一次，count按匹配行计数。缩短模型/历史投影时只保留完整定位并重算游标，不会跳过未返回项。

游标仅当前workspace实例有效，不是跨进程快照：参数不一致明确拒绝；目录、实际读取正文或采用的忽略规则变化（含原先缺失的规则）会使 `cursor_stale`。Linux/macOS使用原生目录变化观察并复核正文hash，观察不可用不静默降级。关闭、恢复/F2、空闲10分钟或回收后为 `cursor_expired`，须明确新建查询而非冒充续页。查询状态只在内存，不自动落盘；历史结果不代表磁盘现在的状态。

查询保留预算为当前客户端设置，新建/resume/F2均生效：`--file-query-memory-limit`默认64MiB、最高1GiB；`--file-query-entry-limit`默认100000计费单位、最高1000000；`--file-query-max-records`默认16、最高64。计费是逻辑保留预算而非精确Go峰值堆。记录满只回收最旧已结束查询，所有查询活跃时拒绝新增；到终止性容量/深度限制明确不完整，不以空结果掩盖。search单文件正文仍为1MiB。详见 [C7合同](../../docs/comet/specs/client-file-query-pagination/spec.md)。

`write`/`edit`/`apply_patch`/`shell`/`task` 的单次完整 arguments JSON 上限为 64 KiB，其他工具为 8 KiB，一次模型响应的参数总量为 128 KiB；包含路径与 JSON 转义，不能理解为 64 KiB 净正文。`write` 仍限制为1MiB，预览/结果另有预算；不提供通用分块上传协议。详细说明见 [文件大参数设计](../../docs/design/client-file-large-arguments.md)。

局部 `edit` 已支持独立预算内的大文本，默认原文件和完整候选各64MiB，可用 `--file-edit-limit BYTES` 配置，新建/resume/F2切换使用当前客户端设置。模型只提交最多32处精确唯一且不重叠的old_text/new_text及完整expected_hash；保留BOM、未修改原字节、替换换行、权限、授权、版本复核、WAL及原子发布。候选顺序构造，预算包含BOM和归一后的真实字节；仍全文读取/候选/发布，不承诺固定内存GB级编辑。超限可调预算或改用Shell脚本，write/stat.hash/search不随之扩大。短预览有界且明确截断，不新增逐页审批；完整diff由下述独立产物保留。详见 [C5合同](../../docs/comet/specs/client-large-file-edit/spec.md)。

#### 多文件补丁与完整差异

`write/edit/apply_patch` 在授权前冻结并保留完整diff；无法完整生成或保留时不发布目标。远隔edit/patch修改生成多hunk，保留BOM、原换行及末尾无换行标记，不追求最小diff或固定内存流式算法。

`apply_patch` 接受严格JSON的 `patch` 与 `expected_hashes`。补丁使用 `*** Begin Patch`/`*** End Patch`，支持 `*** Add File: PATH`（`+`正文）、`*** Update File: PATH`（裸`@@`及空格/`-`/`+`行）、`*** Delete File: PATH`（文本归档）；可用`*** End of File`约束EOF。`expected_hashes`必须且仅覆盖所有更新/删除文件，使用read获得的完整`sha256:`版本。拒绝Move、附加定位语法、输入no-newline标记、二进制、fuzzy以及歧义/重叠/路径冲突。

最多16个文件，全部预检后一次授权；依次写每文件WAL、复核和发布，再保存真实结算。失败、冲突、取消或未知立即停止余项；已完成项不回滚，删除仍使用实际归档位置。完整逐项receipt保留原版本、执行器确认的结果版本、路径、归档位置、完成/未变更/未知/未尝试及独立保存错误。不存在可恢复批准或自动重放。

**F6** 打开完整diff/receipt浏览器，待授权时优先选当前diff；`↑/↓`选产物，`PgUp/PgDn`按4096原始字节翻页，`Home`回首部，`/`检索、`n`续扫、`r`刷新，`Esc/F6`返回原对话/授权状态。人工查看不调用模型，不新增末页审批；原mkdir/copy/move门槛保持。模型用`artifact list/read/search`独立访问，读取/检索游标对应实际返回的原始字节；差异数据本身不证明已执行。

持久Session使用与任务输出相同的Session-owned认证加密blob，diff/receipt有独立版本化目录；raw diff/patch参数不进入稳定聊天或自动标题。重启只读恢复、provider发送门禁及delete/clear密钥撤销不变。仅有孤儿片段不能冒认为完整结果，也不会作为空Session自动删除。`--no-save`只保留有界内存，退出不可恢复，不创建临时历史文件。任务与文件产物共享底层Session/profile密文配额，满额不自动淘汰。

新建/resume/F2使用当前客户端资源设置：`--file-diff-limit`默认128MiB（同时为单产物上限）、`--file-patch-limit`默认64MiB（原文总量与候选总量分别约束）、`--artifact-memory-limit`默认256MiB、`--artifact-max-records`默认256条/Session。diff与内存预算最高1GiB、记录最高8192；单项新增仍受write预算，更新仍受edit预算。保存失败明确可见，不能用退出码、文件修改成功或短预览代表完整产物已保存。详见 [C6合同](../../docs/comet/specs/client-file-patch/spec.md)。

`read` 已采用独立的完整读取预算，默认64MiB，可在新建或 `resume` 时传 `--file-read-limit BYTES`，F2切换/新建沿用当前客户端预算。例如 `edu-agent agent --workspace PATH --file-read-limit 134217728`。仍是安全全文读取、完整原始SHA256与有界行窗口，不是GB级固定内存/低IO实现；超过预算明确失败，可调预算或使用Shell。`offset/limit` 定位行，`byte_offset` 续读长行；恰在线尾的输入可规范化为下一行实际起点。必须按实际返回的 `next_offset/next_byte_offset` 并携带 `expected_hash` 连续读取；模型投影进一步缩短时也重算游标，不跳过未返回字节。`content_hash` 包含原始BOM/换行，`hash_scope=whole_file` 明确不是片段hash。读取预算不改变stat.hash/search/write的1MiB上限或edit的独立预算，也不扩大模型参数/结果预算；新旧版本不能静默混读。详见 [C4合同](../../docs/comet/specs/client-large-file-read/spec.md)。

`mkdir` 可创建空目录，或显式使用 `parents=true` 创建冻结的缺失目录链；已有普通目录返回未变更，不覆盖其他入口。中途失败不删除回滚，已知创建前缀随统一回执保存；只有WAL的崩溃会明确说明计划路径可能已创建，恢复不重放。详见 [mkdir设计](../../docs/design/client-file-mkdir.md)。

#### 递归目录与大文件复制

`copy` 继续使用 `source/destination/expected_version`，版本来自stat；可复制普通文件（含超过32MiB的二进制）或完整目录树，不经模型传输文件正文。目标根必须不存在且父目录已经存在；不覆盖、不合并、不跟随源链接，也不复制特殊入口或归档树。文件固定缓冲复制并计算实际hash，保留普通rwx但不传播特殊权限；新目录0700，不承诺目录元数据、ACL/xattr或硬链接镜像。

授权前完整枚举并冻结清单，F6优先显示本次 `plan_id`；仍按既有短确认摘要规则批准整个计划，不新增逐文件或F6末页审批。执行前复核全计划，再逐项确认意图、发布、结算。冲突、源变化、取消、保存失败或未知立即停止余项，已完成目标保留，不自动回滚或重放。

目录复制用独立分段日志绕开而非扩大旧32项dirty上限。`batch_id`（亦为`receipt_id`）指向`b_`追加JSONL，模型`artifact read/search`和F6均可独立分页检索：plan不证明执行，pending没有actual为unknown，未开始项为not_started。`receipt_bytes/receipt_saved_bytes/receipt_error`将实际可读量与已确认保存分开；日志保存失败不抹去本进程已知结果。重启仅恢复认证前缀，不能提升孤儿尾部或恢复批准；WAL消费前另确认持久调用身份，同ID不能重跑，合法新ID仍可执行。`--no-save`仅保留有界内存日志；目标文件属于用户要求的副作用，不因退出回滚。

新建/resume/F2均采用当前复制预算：`--file-copy-limit`默认1GiB总文件字节；`--file-copy-plan-limit`默认64MiB、最高1GiB；`--file-copy-entry-limit`默认100000项、最高1000000；`--file-copy-journal-limit`默认256MiB日志保留内存、最高1GiB；`--file-copy-max-records`默认256记录、最高8192，不自动淘汰。完整清单同时受现有单产物和Session/profile加密配额限制；逻辑内存预算不等同于精确Go峰值堆。详见 [C8合同](../../docs/comet/specs/client-recursive-copy/spec.md) 与 [实施设计](../../docs/design/client-local-tools-delivery.md)。

`move` 使用同样的三个字段，支持普通文件和整个目录（包括非空目录）；不读取正文、不限制为32MiB，内部链接随目录保留但不遍历。仅同文件系统、不覆盖、父目录必须存在；拒绝归档、自身后代和不安全大小写/身份别名，不以复制后删除兜底。入口版本不是子树快照，也不是跨进程CAS。详见 [move设计](../../docs/design/client-file-move.md)。

`write`/`edit`/`apply_patch`/`mkdir`/`copy`/`move`/`archive`/`restore_archive` 默认逐操作显示冻结预览并等待用户授权。按 `F4` 可在 TUI 内切换“逐次确认”和仅当前 Session 生效的 `YOLO`；`YOLO` 只跳过确认，不放宽固定工作区、链接、版本检查、原子发布、归档保护和取消校验，切换模式也不会自动批准已经等待确认的修改。

`mkdir`、`copy`、`move` 的冻结短预览用PgUp/PgDn完整分页，末页显示后才能批准；独立F6清单/回执不增加末页门槛。持久Session在每次文件副作用前保存计划、执行后保存真实结算；连续变更不会覆盖此前记录。普通文件日志仍受16KiB与32项回执容量约束；递归copy只用一个根事实，逐项计划/结算进入独立分段日志。容量或持久化失败会阻止后续变更，不静默裁剪或降级继续写入；仅有WAL的崩溃仍诚实报告unknown。record payload现为v9、dirty为v10，容器仍v1；清理使用严格Effect v4、恢复为v3、目录copy为v2，其余操作仍v1。冻结旧record v8/dirty v9及更早DTO，旧版本不因新验证器而接受清理、恢复或过去非法的目录copy事实，不放宽未来版本边界。恢复不重放文件或本地任务操作。详见 [文件效果日志](../../docs/design/client-file-effect-journal.md) 及 [实施设计](../../docs/design/client-local-tools-delivery.md)。

普通工作区删除请求通过 `archive` 实现：首次提交时创建工作区内 `.edu-agent-archive/`，将普通文件（包括二进制）或整个非空目录移动到 `<UTC时间>-<随机ID>/<原相对路径>`。不覆盖旧归档，不复制后删除，不自动清理、过期或恢复；可通过下述专用工具显式恢复或明确确认永久清理，磁盘占用不会自动释放。归档树禁止 `write/edit/apply_patch/mkdir/copy/move/archive` 修改；`list/read/stat/find` 可查看，普通 `search/find` 默认跳过，显式指定归档路径时可搜索文本。目录内部链接原样保留但不跟随；入口元数据校验不是整个子树快照或跨进程强锁。跨文件系统或安全移动不受支持时报错，失败可能留下空归档容器；结果未知时提示核查源和目标，不自动重试。Session 恢复保留归档回执且不重放操作。正常文本编辑和客户端内部临时文件/会话存储清理不属于这项“禁止永久删除用户文件”的约束。

#### 归档定位与恢复

用`list/find/read/stat`显式指定`.edu-agent-archive`范围分页查看旧归档和内容，超过2000项仍可继续；缺失、扫描不完整与游标失效不会被当作空归档。`restore_archive`严格要求`source/destination/expected_version`，版本来自当前stat。源必须是归档树中确切文件或目录入口（不能是归档根/容器本身）；目标明确指定，旧无manifest归档不从布局猜原位置。

恢复只做同工作区、同文件系统的不覆盖移动，目标父目录须已存在；不合并、覆盖、创建父目录、复制后删除或清理空容器。普通文件/二进制没有32MiB限制，目录内部链接整体保留但不解引用。归档根与两端父身份在授权前冻结、执行重开复核；普通move及写工具的归档保护不放宽。入口版本不是正文hash、子树快照或跨进程CAS，新目标版本需重新stat。

确认面板用PgUp/PgDn查看完整恢复预览，任意页均可批准，不新增末页门槛；拒绝/取消不移动入口。结果区分完成、未变化和未知，晚取消不掩盖已发生的移动；未知核查归档源和目标，不自动重试/回滚。持久Session先WAL、后真实结算；消费WAL前保存加密不可重放身份，重启只恢复历史事实，不恢复批准或执行器。--no-save不落自动历史；restore的实际文件移动不因退出撤销。Linux/macOS为支持范围，其它平台明确不支持；本批仅有Linux运行与Darwin交叉编译证据。详见 [C9合同](../../docs/comet/specs/client-archive-restore/spec.md)。

#### 用户主动永久归档清理

`purge_archive`严格要求归档内确切`path`及当前stat的`expected_version`，可选一个容器、文件或子目录，但拒绝归档根、工作区根、归档外及通配全部。**每次必须明确批准，YOLO不能跳过**；授权前完整冻结有界后序清单，F6/artifact可独立分页检索，不新增末页或逐项审批。

只在可证明的规范归档、同挂载边界内删除入口，不读取普通文件正文，内部链接仅删除链接自身。执行前重检完整计划，逐项先保存意图、再删除与结算；变化、取消、失败、未知或保存失败即停止余项，不纳入新增文件，不回滚已删除项，也不顺带清理未选父容器。冻结的多个硬链接共享inode时，删除首项引起后续版本变化会停止，需核对后显式新操作。

结果区分计划、尝试、完成、未变化、未知与not_started，并保存完整清单/追加日志。`logical_bytes_removed`只是已确认删除普通文件的逻辑字节；物理释放量unknown，硬链接、稀疏、打开文件和快照可能影响它。重启仅恢复认证事实，pending无actual仍未知，不恢复批准或自动续执行；消费WAL及再次恢复后旧调用ID仍不可重放，新ID可明确执行。`--no-save`自动清理历史只在内存，不妨碍用户批准的真实删除。

历史名称`--file-copy-plan-limit`、`--file-copy-entry-limit`、`--file-copy-journal-limit`、`--file-copy-max-records`同时用于复制与清理，新建/resume/F2使用当前设置；`--file-copy-limit`仅限制复制字节，不限制清理大文件。无自动过期、后台清理、归档外delete或安全擦除承诺。详见 [C10合同](../../docs/comet/specs/client-archive-purge/spec.md)。C1–C10已完成Linux批次及候选检查，限定范围独立源码审查未留确认的生产阻断；macOS原生和Runtime仍未验收。最终证据与限制见[候选交接](../../docs/design/client-local-tools-handoff.md)。

归档底层实现支持 Linux/macOS/Windows 的不覆盖移动；当前变更的 macOS/Windows 证据为交叉编译，原生归档运行未验证，这不扩大整客户端既有平台支持范围。

对服务端数据，Agent 的唯一写工具仍是提出长期偏好候选。执行前，TUI会把候选内容、理由、类别、敏感性和稳定性完整放入可滚动区域，并固定显示确认控件；拒绝不会产生服务端写入。确认后也只会创建Memory候选，后续准入和隐私处理继续由服务端合同控制。响应丢失时会保留原 `operation_id` 并只允许幂等重试核对，不允许把未知结果改称“取消保存”；写入成功后的模型续答失败则会明确报告候选已提交并退出确认状态。退出Agent会话会取消在途模型请求并收尾受管本地任务，按保存模式处理加密Session历史；不会自动把对话或模型响应另写为工作区业务文件，`--no-save`不落自动历史。

## Commands

```text
edu-agent pair [--server URL] [--name NAME]
edu-agent device status
edu-agent device forget-local
edu-agent logout
edu-agent knowledge import <file-or-directory>
edu-agent goal set <text>
edu-agent learn
edu-agent assessment show|confirm|override|void
edu-agent route [--history] [--limit N] [--cursor CURSOR]
edu-agent progress [--all]
edu-agent evidence [--node ID] [--limit N] [--cursor CURSOR]
edu-agent reviews [--due-before RFC3339] [--limit N] [--cursor CURSOR]
edu-agent config show
edu-agent config set [--timeout DURATION] [--color never|auto|always] [--session-history auto|off]
edu-agent model show
edu-agent model preset openai|deepseek|openrouter|ollama|custom
edu-agent model set [--base-url URL] [--model NAME] [--context-window N] [--max-tokens N] [--context-compaction auto|recent-only|off] [--reasoning-effort auto|none|minimal|low|medium|high|xhigh|max] [--session-history auto|off] [--timeout DURATION] [--max-tool-rounds N]  # 0=unlimited
edu-agent model test
edu-agent model key delete --confirmed
edu-agent agent [--workspace PATH] [--no-save]
edu-agent agent resume [SESSION] [--last] [--all]
edu-agent agent sessions delete SESSION --confirmed
edu-agent agent sessions clear --confirmed
edu-agent clear
edu-agent version
```

Pairing codes are read without echo from a TTY or as one line from non-TTY stdin. There is no `--code` flag. Pairing output never includes the device token.

`learn` resumes exclusively from the server-provided `SessionView.work_item`. Its interactive commands are `:ask`, `:answer`, `:quiz`, `:resume`, `:assessment`, `:progress`, `:route`, `:reviews`, `:clear`, `:end`, `:complete`, `:quit`, and `:help`. `:answer` starts a multiline block terminated by a line containing only `.`. Plain text is an answer only while awaiting a response, and is a follow-up question only while showing a free answer. `:quit` exits the client without ending the server session.

The CLI uses the server's allowed actions and assessment decisions. Provisional feedback is not shown as accepted evidence and must be confirmed, overridden, or voided before feedback can be acknowledged. Objective activities use the deterministic server assessment path; open activities request a frozen proposal. A successful mutation is immediately followed by a fresh session read. A version conflict refreshes the work item but never automatically replays an answer or decision.

Free answers are explicitly non-scoring. Enter on a free answer calls `resume_focus`; converting a free answer to a quiz follows the normal attempt and assessment flow, then still requires an explicit resume after feedback.

## Local State

普通配置保存在 `os.UserConfigDir()/edu-agent/config.json`。其中包括全有或全无的配对字段（服务器 URL、设备 ID、显示名称），以及可独立存在的客户端请求超时、颜色和非敏感 AI 模型设置。它不包含设备 token、模型 API Key 或学习内容。未配对客户端也可保存客户端与模型偏好。

模型 API Key 通过平台凭据后端按“提供商 + 规范化 Base URL”的不可逆指纹分槽保存：Linux 使用 Secret Service，macOS 使用 Keychain。切换模型名称可继续使用同一端点凭据；切换提供商或 Base URL 只会读取新端点自己的凭据，绝不会把旧 Key 发送给新端点。系统凭据服务不可用时，模型密钥操作会失败关闭，不会降级为明文配置文件。Ollama 与无鉴权的自定义 loopback 端点不要求 Key；远程自定义端点与云端预设一样必须使用端点绑定的 Key。Windows 凭据与 Session 后端不属于当前版本的支持范围。

On Linux and macOS, the credential is protected by the platform key service, while local security-sensitive files use `0600` under `0700` directories. No-follow reads reject symlinks, non-regular files, and broad permissions. Markdown directory imports hold an open root handle and resolve every relative component with `openat` plus `O_NOFOLLOW`. Windows behavior is not part of the current support contract.

`EDU_AGENT_TOKEN` is a process-only override and is never persisted. It is accepted only when a complete local config/credential pair already exists and `EDU_AGENT_TOKEN_SERVER` plus `EDU_AGENT_TOKEN_DEVICE_ID` explicitly match that pair; it cannot replace a missing half or bypass a binding mismatch.

Pairing first writes a fail-closed pending journal, then saves the credential and atomically publishes ordinary configuration. The journal is removed only after both halves are durable. Any failed publication or compensation leaves startup blocked until `edu-agent device forget-local` removes config, credential, and journal. The command changes local state only; the remote device may remain valid and must be revoked from another paired device.

## Connection And Terminal Boundaries

The default server is `http://127.0.0.1:8080`. Plain HTTP to a non-loopback host is rejected unless explicitly approved with `--allow-insecure-http`; every such network command prints a warning. URLs with embedded credentials, query strings, or fragments are rejected. Redirects are disabled.

The default color mode is `never`. `edu-agent clear`, interactive `:clear`, and Ctrl-L clear only the visible application viewport in a TTY and redraw a neutral `>` prompt. They do not clear terminal scrollback, shell history, OS audit records, remote terminal logs, server events, projections, or credentials. Non-TTY clear emits no control sequence and returns a diagnostic error. The implementation does not execute `clear`, `cls`, a shell, or another external command.

`goal set` 只保存一句话目标并返回目标版本，不读取、创建或切换教学会话，不要求先导入资料或配置模型。TUI 按 `o` 或运行 `goal browse` 打开目标管理，支持列表、搜索、状态筛选、分页、详情、多行编辑、资料选择和状态操作；按 `g` 保留一句话快捷保存。`goal help` 列出脚本参数，使用 `--space UUID` 指定稳定归属。保存失败在编辑页面保留输入和重试身份；版本冲突后可读取远端内容，再明确选择下一次保存依据。资料选择可以累积多个集合、文档或章节，并冻结具体范围版本。

教学入口 `learn browse [--space UUID]` 按名称和编号选择目标、会话；列表显示阶段、路线位置和可继续状态。`learn start --goal UUID` 新建，`learn --session UUID` 继续，`learn show --session UUID` 只读历史，`learn list --goal UUID` 输出会话列表。普通 `learn` 优先使用本进程在该区明确选择的会话，否则打开 picker，不猜测全局最新会话。

原生教学页 F2 可在请求等待中切换会话；`:switch`、`:space`、`:pause`、`:complete` 分别切会话、切区、暂停目标、结束本次教学，互不等价。Esc / `:quit` 返回但不结束服务端教学。`:answer` 的多行草稿及正在编辑的一行按 session/activity 隔离，回到原题可继续；`:discard` 清除多行草稿。草稿不落盘，重启不恢复。取消不保证服务器事务回滚；重新进入查询原 session，CLI 不自动重放答案。非默认区的复习资格使用本会话 work_item；跨目标复习总览不在此入口提供。

`offline prepare --session UUID [--space UUID]` 从所选会话签发；未给 ID 时使用进程选择或 picker。已保存离线 intent 的重试复用原请求，sync 按原授权归属，不跟随当前页面改目标；现有加密离线存储与签名信任链不变。详见[教学续学契约](../../docs/design/tutoring-sessions.md)。

Text entered directly in a shell command, including `goal set` text, may be retained by shell history. Interactive `learn` keeps answers and free questions out of argv and does not create a persistent input history.

This is an online client. Network failures do not create an offline business queue, and the CLI does not persist Markdown, goals, activities, attempts, answers, assessments, free questions, free answers, routes, evidence, progress, cursors, or pending operations. Proposal input is `go-cli-context-v1` and contains only authoritative work-item records plus canonical retrieval IDs, ranges, slices, and hashes returned by the server.
