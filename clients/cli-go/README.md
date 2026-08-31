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

`make cli-cross-build` compiles Linux, macOS, and Windows binaries for amd64 and arm64 with `CGO_ENABLED=0` and `-trimpath`. Cross-builds prove compilation only. `.github/workflows/cli-platform.yml` runs SHA-bound root-confinement, credential, hidden/line input, Ctrl-L, and clear checks on native Linux, macOS, and Windows runners and uploads one artifact per runner. Hidden input calls the production `ReadSecret`: Linux and macOS use a real PTY with an echo-mode check, while Windows uses ConPTY. A missing native mechanism fails the job; missing native artifacts remain a release blocker.

`make cli-release` writes binaries and `SHA256SUMS` under `clients/cli-go/dist/`. The release directory contains no configuration, credentials, or learning content.

## 交互式中文 TUI

在支持全屏控制的交互终端中直接运行 `edu-agent`，即可打开中文主控制台。方向键或 `j/k` 移动，Enter 选择，界面显示的字母可直接打开 AI 学习助手、结构化学习、知识导入、目标、进度、复习、配对和设置等常用流程。

设置页可在配对前管理客户端请求超时、输出颜色和本地 AI 模型。模型配置支持 OpenAI、DeepSeek、OpenRouter、Ollama 及自定义 OpenAI 兼容端点；可调整 Base URL、模型名称、上下文窗口、请求超时和最大工具轮数，并直接执行连接测试。API Key 使用隐藏输入并按“提供商 + Base URL”绑定到独立的系统凭据槽，不会写入 `config.json`、命令输出或日志，也不会在切换端点时发送给另一个服务。Ollama 和无鉴权的自定义 loopback 端点可不配置 API Key；云端预设和远程自定义端点必须读取当前端点绑定的 Key，系统凭据后端不可用时会失败关闭。服务器地址和设备凭据仍通过配对流程变更。

已配对客户端更换服务器时有两条明确路径：旧服务器可用时先安全注销并撤销远端设备；旧服务器不可用时可选择“仅清除本地配对”，保留客户端和模型设置，同时明确警告远端设备可能仍有效。损坏或不一致的本地配对状态只显示修复、设置和退出入口；Enter 本身不会确认撤销、删除凭据或保存长期偏好。

主控制台是现有命令分发器上的交互层。显式子命令为脚本和恢复流程保留原有稳定输入输出；非 TTY、`TERM=dumb` 或显式传入子命令时不会输出全屏控制序列。普通动作结束后按 Enter 返回主控制台，退出时保留最近一次动作的退出码。

## 客户端 AI 学习助手

“AI 学习助手”使用客户端本地配置的 OpenAI 兼容模型，不读取或修改服务端教学模型配置。模型自身不持久化学习状态；Agent Loop 通过受限工具从服务端读取知识目录、学习进度、路线、复习和已接纳的长期偏好。用户输入、模型回答、单轮和单次工具调用、工具参数、工具结果及每次发给模型的上下文均有独立硬上限；越界响应会失败关闭，不会放大为无界服务端读取或模型流量。所有模型、工具、错误和确认文本在渲染前都会移除终端及双向文本控制字符。

Agent TUI 顶部只显示产品标识，把运行状态、估算上下文、模型名称和键位帮助放在消息输入区下方。输入区是按内容增长的有界多行 composer：`Enter` 发送，`Ctrl+J` 或 `Alt+Enter` 换行，字符计数显示 `8000` 上限；暂停跟随 transcript 时，底部状态会提示有新消息并保留回到底部的快捷键。宽终端还会在右侧显示学习概览：当前 Agent 状态、服务端权威学习目标、会话状态、路线进度、当前 Activity 与估算活跃时间；它在启动、完整 turn 结束或 `Ctrl+R` 时刷新，读取失败不阻断对话，窄终端会完全折叠侧栏。侧栏不显示任何 opaque ID、凭据、隐藏推理或原始工具参数，也不持久化学习状态副本。

Agent的唯一写工具是提出长期偏好候选。执行前，TUI会把候选内容、理由、类别、敏感性和稳定性完整放入可滚动区域，并固定显示确认控件；拒绝不会产生服务端写入。确认后也只会创建Memory候选，后续准入和隐私处理继续由服务端合同控制。响应丢失时会保留原 `operation_id` 并只允许幂等重试核对，不允许把未知结果改称“取消保存”；写入成功后的模型续答失败则会明确报告候选已提交并退出确认状态。退出Agent会话会取消在途模型和工具请求，也不会把对话或模型响应写入本地文件。

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
edu-agent config set [--timeout DURATION] [--color never|auto|always]
edu-agent model show
edu-agent model preset openai|deepseek|openrouter|ollama|custom
edu-agent model set [--base-url URL] [--model NAME] [--context-window N] [--timeout DURATION] [--max-tool-rounds N]
edu-agent model test
edu-agent model key delete --confirmed
edu-agent agent
edu-agent clear
edu-agent version
```

Pairing codes are read without echo from a TTY or as one line from non-TTY stdin. There is no `--code` flag. Pairing output never includes the device token.

`learn` resumes exclusively from the server-provided `SessionView.work_item`. Its interactive commands are `:ask`, `:answer`, `:quiz`, `:resume`, `:assessment`, `:progress`, `:route`, `:reviews`, `:clear`, `:end`, `:complete`, `:quit`, and `:help`. `:answer` starts a multiline block terminated by a line containing only `.`. Plain text is an answer only while awaiting a response, and is a follow-up question only while showing a free answer. `:quit` exits the client without ending the server session.

The CLI uses the server's allowed actions and assessment decisions. Provisional feedback is not shown as accepted evidence and must be confirmed, overridden, or voided before feedback can be acknowledged. Objective activities use the deterministic server assessment path; open activities request a frozen proposal. A successful mutation is immediately followed by a fresh session read. A version conflict refreshes the work item but never automatically replays an answer or decision.

Free answers are explicitly non-scoring. Enter on a free answer calls `resume_focus`; converting a free answer to a quiz follows the normal attempt and assessment flow, then still requires an explicit resume after feedback.

## Local State

普通配置保存在 `os.UserConfigDir()/edu-agent/config.json`。其中包括全有或全无的配对字段（服务器 URL、设备 ID、显示名称），以及可独立存在的客户端请求超时、颜色和非敏感 AI 模型设置。它不包含设备 token、模型 API Key 或学习内容。未配对客户端也可保存客户端与模型偏好。

模型 API Key 通过平台凭据后端按“提供商 + 规范化 Base URL”的不可逆指纹分槽保存：Linux Secret Service、macOS Keychain、Windows 当前用户 DPAPI 保护文件。切换模型名称可继续使用同一端点凭据；切换提供商或 Base URL 只会读取新端点自己的凭据，绝不会把旧 Key 发送给新端点。Linux 或 macOS 的系统凭据服务不可用时，模型密钥操作会失败关闭，不会降级为明文配置文件。Ollama 与无鉴权的自定义 loopback 端点不要求 Key；远程自定义端点与云端预设一样必须使用端点绑定的 Key。

On Unix, the credential is a separate `0600` file under a `0700` directory. Security-sensitive reads use no-follow file handles and reject symlinks, non-regular files, and broad permissions. Markdown directory imports hold an open root handle and resolve every relative component with `openat` plus `O_NOFOLLOW`. On Windows, imports reject intermediate reparse points and require each final resolved file handle to remain strictly below the resolved root; the separate credential payload is protected with current-user DPAPI before it is written.

`EDU_AGENT_TOKEN` is a process-only override and is never persisted. It is accepted only when a complete local config/credential pair already exists and `EDU_AGENT_TOKEN_SERVER` plus `EDU_AGENT_TOKEN_DEVICE_ID` explicitly match that pair; it cannot replace a missing half or bypass a binding mismatch.

Pairing first writes a fail-closed pending journal, then saves the credential and atomically publishes ordinary configuration. The journal is removed only after both halves are durable. Any failed publication or compensation leaves startup blocked until `edu-agent device forget-local` removes config, credential, and journal. The command changes local state only; the remote device may remain valid and must be revoked from another paired device.

## Connection And Terminal Boundaries

The default server is `http://127.0.0.1:8080`. Plain HTTP to a non-loopback host is rejected unless explicitly approved with `--allow-insecure-http`; every such network command prints a warning. URLs with embedded credentials, query strings, or fragments are rejected. Redirects are disabled.

The default color mode is `never`. `edu-agent clear`, interactive `:clear`, and Ctrl-L clear only the visible application viewport in a TTY and redraw a neutral `>` prompt. They do not clear terminal scrollback, shell history, OS audit records, remote terminal logs, server events, projections, or credentials. Non-TTY clear emits no control sequence and returns a diagnostic error. The implementation does not execute `clear`, `cls`, a shell, or another external command.

Text entered directly in a shell command, including `goal set` text, may be retained by shell history. Interactive `learn` keeps answers and free questions out of argv and does not create a persistent input history.

This is an online client. Network failures do not create an offline business queue, and the CLI does not persist Markdown, goals, activities, attempts, answers, assessments, free questions, free answers, routes, evidence, progress, cursors, or pending operations. Proposal input is `go-cli-context-v1` and contains only authoritative work-item records plus canonical retrieval IDs, ranges, slices, and hashes returned by the server.
