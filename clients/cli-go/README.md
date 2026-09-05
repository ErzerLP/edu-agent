# edu-agent Go CLI

`clients/cli-go` is an independent Go module for the online `edu-agent` client. It uses only the public HTTP/OpenAPI boundary and does not import server internals.

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

设置页可在配对前管理客户端请求超时、输出颜色和本地 AI 模型。模型配置支持 OpenAI、DeepSeek、OpenRouter、Ollama 及自定义 OpenAI 兼容端点；可调整 Base URL、模型名称、上下文窗口、无响应超时和可选工具轮数保护值。工具轮数默认不限制，配置 `0` 表示持续运行到模型给出最终回答、需要用户交互、用户取消、超时或上下文管理无法继续；正整数只作为用户自选保护值，客户端不设置固定最大值。流式请求的无响应超时从请求发出后开始计算，并在收到任意 SSE 响应字节（包括心跳、隐藏推理、工具增量和跨分片内容）时重置；持续活跃的 SSE 不受固定总时长限制。非流式请求仍以该值限制整次等待。API Key 使用隐藏输入并按“提供商 + Base URL”绑定到独立的系统凭据槽，不会写入 `config.json`、命令输出或日志，也不会在切换端点时发送给另一个服务。Ollama 和无鉴权的自定义 loopback 端点可不配置 API Key；云端预设和远程自定义端点必须读取当前端点绑定的 Key，系统凭据后端不可用时会失败关闭。服务器地址和设备凭据仍通过配对流程变更。

已配对客户端更换服务器时有两条明确路径：旧服务器可用时先安全注销并撤销远端设备；旧服务器不可用时可选择“仅清除本地配对”，保留客户端和模型设置，同时明确警告远端设备可能仍有效。损坏或不一致的本地配对状态只显示修复、设置和退出入口；Enter 本身不会确认撤销、删除凭据或保存长期偏好。

主控制台是现有命令分发器上的交互层。显式子命令为脚本和恢复流程保留原有稳定输入输出；非 TTY、`TERM=dumb` 或显式传入子命令时不会输出全屏控制序列。普通动作结束后按 Enter 返回主控制台，退出时保留最近一次动作的退出码。

## 客户端 AI 学习助手

“AI 学习助手”使用客户端本地配置的 OpenAI 兼容模型，不读取或修改服务端教学模型配置。模型自身不持久化学习状态；Agent Loop 通过受限工具从服务端读取知识目录、学习进度、路线、复习和已接纳的长期偏好。Agent Loop 不再对模型工具轮数、单响应工具调用数量、单 turn 总工具调用数或用户问询次数设置固定上限，而是像 Codex 一样持续执行“模型推理 → 工具结果 → 再推理”，直到模型给出最终回答、需要用户交互、用户取消、超时或上下文管理无法继续。用户输入、模型回答、工具参数、工具结果投影、模型响应体及每次发给模型的上下文仍有独立资源和协议边界；越界响应会失败关闭，不会放大为无界内存、服务端读取或模型流量。所有模型、工具、错误和确认文本在渲染前都会移除终端及双向文本控制字符。

新 Agent Session 默认在稳定轮次后使用系统钥匙串保护的本地密钥自动加密保存；当前支持平台为 Linux 与 macOS，Windows Session 恢复不属于本版本的支持或验收范围。可通过 `edu-agent agent resume` 打开共享 picker，或用 `resume --last`、`resume <UUID|标题>` 恢复；运行中的 TUI 空闲时按 `F2` 可打开同一套 picker，并支持当前/全部工作区范围、搜索、恢复、重命名、二次确认删除和新建 Session。provider endpoint 变化时，确认面板中 `Enter` 允许向新 provider 发送历史上下文，`L` 只在本地打开并继续阻止模型请求和自动标题，`Esc` 取消切换；仅本地打开后仍可查看 transcript、重命名或删除。工作台“AI助手与模型”设置以及 `edu-agent model set --session-history auto|off`/`config set --session-history auto|off` 只控制后续新 Session；`off` 会跳过存储打开，但不影响恢复和删除已有历史。`--no-save` 仅让当前新 Session 不落盘且不修改配置；历史不会按时间自动删除，达到硬上限也不会自动淘汰，可用 `agent sessions delete <UUID> --confirmed` 删除普通或可定位的损坏 Session；极端损坏且无可信 UUID 时，只能在明确选择后使用 UI 显示的 `storage:<id>` locator。已认证的未来版本必须升级客户端处理，当前版本不会恢复、修改或删除。`agent sessions clear --confirmed` 可清理全部本地 Session。自动标题会额外向当前模型提供商发送有界、清理后的已提交用户文本和安全最终回答，不包含工具调用或结果、工具参数、workspace/server 来源、回执、错误原文或推理；恢复后的下一模型请求会把历史上下文发送到当前 provider，端点身份变化时必须先明确确认。恢复继续绑定保存的工作区；旧 root 不可用或不安全时只恢复本地对话并禁用全部文件工具，绝不回退到当前目录，历史文件正文也不代表磁盘当前内容。跨进程恢复和 TUI 切换都会把 YOLO、旧文件授权与未完成交互重置为安全默认。平台钥匙服务不可用时不会回退到明文，而是明确降级为不可恢复的未保存会话。`agent sessions clear` 只清除本地 Agent Session store，不清除服务端事件、Nocturne、终端 scrollback、Shell history、provider retention 或 OS backup。

Agent TUI 不再设置独立顶部状态栏或全宽分隔线，transcript 直接使用页面顶部空间；产品标识在宽终端合并到右侧概览标题，在窄终端进入消息输入区下方的状态区。输入区是按内容增长的有界多行 composer：`Enter` 发送，`Ctrl+J` 或 `Alt+Enter` 换行，字符计数显示 `8000` 上限。普通对话状态下，鼠标滚轮、`↑/↓` 与 `PgUp/PgDn` 均可滚动 transcript；向上离开底部后暂停自动跟随并提示有新消息，滚回底部后恢复跟随。选择面板激活时 `↑/↓` 仍用于移动选项焦点，`PgUp/PgDn` 和鼠标滚轮继续查看 transcript。宽终端右侧概览显示当前 Agent 状态、服务端权威学习目标、会话状态、路线进度、当前 Activity 与估算活跃时间；它在启动、完整 turn 结束或 `Ctrl+R` 时刷新，读取失败不阻断对话，窄终端会完全折叠侧栏。侧栏不显示任何 opaque ID、凭据、隐藏推理或原始工具参数，也不持久化学习状态副本。

### 本地工作区与文件工具

Agent 会在 Session 启动时固定一个本地工作区：`edu-agent agent` 默认使用 Agent 启动目录，也可通过 `edu-agent agent --workspace PATH` 显式指定。模型获得相对路径上的 `list`、`read`、`search`、`write`、`edit` 文本工具，以及 `archive` 文件/目录归档工具；不提供永久 delete、通用 move、copy、patch、shell、进程或网络工具。源链接、junction、reparse point、绝对路径和工作区逃逸会被拒绝。除专用归档目录禁止普通写入外，隐藏文件、`.git`、`.comet`、`.env` 遵循普通文件规则，读取到的内容可能发送给当前配置的本地或远端模型 provider。

`write`/`edit`/`archive` 默认逐操作显示冻结预览并等待用户授权。按 `F4` 可在 TUI 内切换“逐次确认”和仅当前 Session 生效的 `YOLO`；`YOLO` 只跳过确认，不放宽固定工作区、链接、版本检查、原子发布、归档保护和取消校验，切换模式也不会自动批准已经等待确认的修改。

删除请求只通过 `archive` 实现：首次提交时创建工作区内 `.edu-agent-archive/`，将普通文件（包括二进制）或整个非空目录移动到 `<UTC时间>-<随机ID>/<原相对路径>`。不覆盖旧归档，不复制后删除，不自动清理、过期或恢复；用户自行手动恢复或删除归档，磁盘占用不会自动释放。归档树禁止 `write/edit/archive` 修改；`list/read` 可查看，普通 `search` 默认跳过，显式指定归档路径时可搜索文本。目录内部链接原样保留但不跟随；入口元数据校验不是整个子树快照或跨进程强锁。跨文件系统或安全移动不受支持时报错，失败可能留下空归档容器；结果未知时提示核查源和目标，不自动重试。Session 恢复保留归档回执且不重放操作。正常文本编辑和客户端内部临时文件/会话存储清理不属于这项“禁止永久删除用户文件”的约束。

归档底层实现支持 Linux/macOS/Windows 的不覆盖移动；当前变更的 macOS/Windows 证据为交叉编译，原生归档运行未验证，这不扩大整客户端既有平台支持范围。

对服务端数据，Agent 的唯一写工具仍是提出长期偏好候选。执行前，TUI会把候选内容、理由、类别、敏感性和稳定性完整放入可滚动区域，并固定显示确认控件；拒绝不会产生服务端写入。确认后也只会创建Memory候选，后续准入和隐私处理继续由服务端合同控制。响应丢失时会保留原 `operation_id` 并只允许幂等重试核对，不允许把未知结果改称“取消保存”；写入成功后的模型续答失败则会明确报告候选已提交并退出确认状态。退出Agent会话会取消在途模型和工具请求，也不会把对话或模型响应写入工作区或其他本地文件。

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
edu-agent model set [--base-url URL] [--model NAME] [--context-window N] [--context-compaction auto|recent-only|off] [--reasoning-effort auto|none|minimal|low|medium|high|xhigh|max] [--session-history auto|off] [--timeout DURATION] [--max-tool-rounds N]  # 0=unlimited
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

Text entered directly in a shell command, including `goal set` text, may be retained by shell history. Interactive `learn` keeps answers and free questions out of argv and does not create a persistent input history.

This is an online client. Network failures do not create an offline business queue, and the CLI does not persist Markdown, goals, activities, attempts, answers, assessments, free questions, free answers, routes, evidence, progress, cursors, or pending operations. Proposal input is `go-cli-context-v1` and contains only authoritative work-item records plus canonical retrieval IDs, ranges, slices, and hashes returned by the server.
