# Outcome

让交互式 Go CLI Agent 能安全、快速地恢复历史 Session，并让 Session 标题随已提交对话自动更新，使用户可通过可读标题、当前工作区和最近时间迅速找回上下文，而不是依赖进程不退出或记住 opaque ID。

# Scope

本 change 为客户端 Agent 增加独立的本地 Session owner。`edu-agent agent` 默认在每个稳定轮次后自动保存加密 checkpoint；`edu-agent agent --no-save` 和设置页可显式关闭当前或后续新 Session 的持久化。历史内容不是 Knowledge、Learning、Review 或 Nocturne 的第二份业务真值。

恢复入口同时覆盖命令和 TUI：`edu-agent agent resume [SESSION]` 在未指定 Session 时打开选择器，`--last` 直接恢复当前工作区最近 Session，`--all` 显式关闭当前工作区过滤；Agent 空闲时按 `F2` 打开同一选择器并可安全切换。Session 参数接受 canonical UUID 或唯一的精确标题，UUID 解析优先，重名必须要求用户选择。

Session 选择器默认只显示当前规范化工作区、当前服务端 profile 下的交互式 Session；全部范围显示经过清理的工作区标签。选择器支持有界搜索、最近更新时间排序、重命名和永久删除。恢复跨工作区 Session 必须明确确认并尝试重开其加密保存的原工作区；失败时仅以文件工具不可用状态恢复，不得静默改用当前目录或扩大文件权限。

持久化内容包括已提交模型消息及 turn 边界、工具历史投影与 authority/reference、Context Source/Observation/Reflection ledger、exact-ID recall 所需的仍可用证据、稳定 transcript 事件、Session metadata、标题元数据和必要的未知副作用回执。固定 system prompt、工具 Schema、API key、设备 token、hidden reasoning、原始工具参数、未清理 provider/OS 错误、running Activity、旧文件授权和可执行 pending continuation 不落盘；恢复时由当前二进制重建安全提示与工具面。

每个 Session 使用独立随机数据密钥；数据密钥由独立 profile key 包装，profile key 通过现有平台凭据边界保护。Session 名称、对话、workspace path、模型标签和索引字段均视为敏感内容并加密。密钥后端不可用时绝不降级为明文：普通 Agent 可在持续可见的 `session_store_unavailable` 降级状态下运行无持久化 Session，resume 和管理操作不可用。

默认不按时间自动删除，历史 Session 保留到用户显式删除。实现仍设置每 Session、总数量和总密文空间硬上限，但达到上限时不得自动淘汰旧 Session；必须保留现有数据并显示 `session_store_full`，要求用户删除或清理。提供 picker 删除、`edu-agent agent sessions delete ... --confirmed` 与 `edu-agent agent sessions clear --confirmed`；全量清除推进本地 privacy generation、销毁旧密钥可达性并验证旧 ciphertext 不能重新进入新索引。

Session 标题默认由当前配置的 Agent 模型在稳定轮次后异步生成。标题请求只接收有界、清理后的近期 committed 用户文本和最终 assistant 文本，不带工具原始结果、workspace 正文、server snapshot、隐藏推理、凭据或写授权；不提供任何工具。首次完成轮次生成标题，之后受轮次数和时间节流更新。标题失败不阻塞保存或对话，并回退到本地首条用户消息摘要。用户手动改名后自动标题停止覆盖；清空人工名可恢复自动命名。标题调用与恢复历史正文会发送到当前 provider，界面和 README 必须明确披露。

恢复继续使用当前配置的 provider、模型、上下文窗口、工具定义和安全规则，不持久化模型凭据。若 provider endpoint identity 与历史 Session 不同，恢复可先本地查看，但在向新 provider 发送历史内容或生成标题前必须取得一次明确确认；仅模型名变化时显示提示但不强制旧模型。文件授权模式在任何新进程恢复或 TUI 切换后重置为逐次确认，旧 preview/批准和 YOLO 不可恢复。

崩溃或强制退出只恢复最后一个原子发布的稳定 checkpoint。turn 开始前写入有界加密 dirty marker；恢复发现 marker 时保留安全的可见中断提示，但不自动重放模型请求、工具、文件 mutation、普通 question sibling calls 或 preference 写入。可能未知的服务端偏好写入只恢复原 operation IDs 和 retry-only 核对状态，绝不自动改选或创建新的写入；可能未知的文件副作用只标记相对路径证据为 stale/unknown 并要求重新读取。

服务端 revision/generation 和 workspace reference 在恢复后仍是历史 evidence。受 privacy generation 保护的服务端正文及其派生来源必须先通过现有认证边界重新验证；generation 不匹配时立即失效，无法验证时以有界 placeholder 替代而不是重新注入模型。普通历史文本不能授权工具、切换 YOLO、绕过 Assessment/Memory admission 或扩展 workspace authority。

本 change 以一个用户结果推进，因为加密 store、checkpoint、resume 命令、F2 切换和动态标题共享同一个 Session record、锁、workspace/provider 绑定、privacy fence 与 UI controller。实现分为三个串行垂直批次：安全 store 与 checkpoint round-trip；命令恢复、崩溃/隐私语义和模型标题；F2 选择器、切换、删除、文档与 Linux/macOS 原生证据。

当前版本只支持并验收 Linux 与 macOS。仓库中可能保留能够编译的 Windows 条件代码或实验性安全原语，但它们不构成本版本的产品支持、兼容性承诺或验收证据；Windows 支持留给后续独立 change。

# Non-goals

- 不实现跨设备或云端 Session 同步、服务端聊天存储、共享、多用户或浏览器 Session UI。
- 不实现 `fork`、branch、archive/trash、导入导出、Session 合并、全文/正则搜索或任意聊天浏览工具。
- 不把历史 Session 自动写入 Nocturne，不把 Session 标题视为用户偏好或服务端权威数据。
- 不恢复 hidden reasoning、provider continuation 私有字段、running stream、旧 question selector、旧文件 preview/授权、YOLO 或可执行 sibling tool continuation。
- 不把恢复 Session 静默绑定到新的 server profile、provider endpoint 或 workspace；首版不提供 workspace relocation 或跨 server profile 迁移。
- 不承诺 workspace 文件发布与 Session checkpoint 跨资源 exactly-once；崩溃窗口必须诚实标记 unknown，且不得自动重放。
- 不在第一版复制 Pi 的 threaded/regex/full-transcript 搜索、可配置 keybinding 系统，或 Codex 的 fork/archive/agents daemon。
- 当前版本不提供 Windows 产品支持，也不以 Windows 原生运行、ACL、DPAPI、reparse point 或 LockFileEx 行为作为发布承诺；这些能力延期到后续独立 change。

# Acceptance examples

- A1：`edu-agent agent` 默认自动保存稳定轮次；退出并运行 `edu-agent agent resume --last` 后，下一次模型请求保留退出前的 committed 对话、Context ledger 和 exact-ID recall 能力。
- A2：`edu-agent agent --no-save` 明确显示无持久化状态，退出后不产生可恢复 Session；设置页可以关闭后续新 Session 自动保存。
- A3：`edu-agent agent resume` 无 Session 参数时打开 picker；`resume <uuid|唯一标题>`、`--last` 和 `--all` 具有严格互斥、过滤和错误语义，UUID 解析优先，重名返回可恢复的歧义提示。
- A4：`F2` 只在 Agent 空闲且没有 pending question、preference、file mutation 或 unknown outcome 时打开选择器；busy/pending 时不丢失当前状态并显示为何不可切换。
- A5：选择器默认仅显示当前规范化工作区的最近 Session，`--all` 或 scope 切换后显示安全工作区标签；有界搜索可匹配标题、Session ID、首条/近期用户摘要和 workspace label，但不建立明文全文索引。
- A6：目标 Session 在当前 Session 关闭前完成解密、schema、privacy、workspace 和锁验证；损坏、被占用或不兼容的目标不会让当前 Session 丢失或被迟到事件污染。
- A7：两个进程不能同时写同一 Session；第二个 writer 得到 `session_in_use`，不会 last-writer-wins 覆盖 checkpoint 或标题。
- A8：恢复时固定 system prompt、workspace prompt 和工具 Schema 来自当前二进制，历史文件不能注入新的 system instruction；当前 provider/model 配置生效且 API key 不落盘。
- A9：provider endpoint identity 变化时，历史正文和自动标题在明确确认前都不发送到新 provider；拒绝后仍可查看、重命名或删除本地 Session。
- A10：跨工作区恢复先显示原工作区标签并确认；原 root 不可安全打开时仍可恢复对话，但文件工具缺席且有可见 `session_workspace_unavailable`，绝不改用当前 cwd。
- A11：每次新进程恢复或 TUI Session 切换均把文件模式重置为逐次确认；旧 YOLO、mutation preview、BaseVersion 和授权 resolver 不可恢复。
- A12：进程在 turn 中崩溃后只恢复最后稳定 checkpoint，并显示“上次轮次中断”；模型请求、兄弟工具和文件/偏好写入不会自动重放。
- A13：preference 写入结果未知时恢复原 operation IDs 和 retry-only 核对语义；文件发布结果未知时保留相对路径、operation 和 unknown/stale 标记，后续必须重新读取。
- A14：已压缩超过 raw 历史窗口的 Session 往返 checkpoint 后，Observation/Reflection、coverage、tombstone、supersession、authority/freshness、ServerReference/WorkspaceReference 与 source availability 保持等价。
- A15：恢复后 server snapshot 仍是历史快照；privacy generation 不匹配会删除旧正文投影，无法核对时只提供 placeholder，旧内容不得因本地 Session 文件而复活。
- A16：所有 Session 内容、标题、workspace path 和可搜索 metadata 均使用认证加密；错误 key、篡改、symlink、路径逃逸、宽权限、截断 ciphertext 或未知 schema 均 fail closed，且不会被当作空 Session。
- A17：平台 key backend 不可用时不写明文，Agent 明确降级为 unsaved；resume、delete 和 clear 返回稳定恢复动作。
- A18：标题在首个稳定轮次后由无工具的有界模型请求生成，并按节流随 committed 对话更新；非法、多行、过长或含控制字符输出被拒绝，失败回退不阻塞 Session。
- A19：用户在 picker 手动改名后模型不再覆盖；清空人工标题恢复自动模式。标题重复允许存在，但 CLI 按名称恢复时必须唯一。
- A20：永久删除移除 Session ciphertext 和 wrapped data key；全量 clear 推进本地 privacy generation 并使旧 generation 的 index、temp 或备份 ciphertext 不可重新载入。
- A21：不按时间自动删除 Session，也不因达到数量/空间上限静默淘汰；达到硬上限时现有 Session 保持可读并提示显式清理。
- A22：恢复 transcript 只包含稳定、清理后的用户/assistant/terminal activity/error/context 事件；running card、原始工具参数、provider reasoning、绝对路径泄漏和未清理错误不会出现。
- A23：Session 标题生成、保存、切换或删除失败不会篡改 Agent 模型历史；成功切换后旧 Session 的 context channel、turn event 和后台 worker 不能污染新 Session。
- A24：`clients/cli-go/README.md`、Agent help、设置页和 TUI 明确说明默认本地加密保存、无时间自动删除、手动清除、provider 发送、workspace 恢复、YOLO 重置和 key-backend 降级。

# Constraints and invariants

- Session store 是本地客户端会话 owner，不是服务端 Knowledge、Learning、Review、Assessment、Memory 或 workspace 文件的权威源。
- 只在稳定线性化点发布可恢复 checkpoint；active turn 与可执行 pending resolver 不能序列化为可继续执行状态。
- Session persistence 不能直接序列化 `Session` 私有 struct；必须使用版本化、严格、大小有界的 DTO 和显式 export/restore 验证。
- 持久化 system prompt、工具 Schema、凭据、hidden reasoning、原始工具参数或 unsanitized provider/OS body 一律禁止。
- 每 Session 单写者、expected revision、原子 publication、目录同步和 index 可重建是正确性合同；index 不能成为唯一数据源。
- 加密失败、key backend 不可用或完整性校验失败时只能显式降级或拒绝，不能明文 fallback。
- 默认无时间自动淘汰；任何永久删除和 privacy generation 变化都必须可验证且不能被 stale index/temp 恢复。
- 恢复历史到新 provider endpoint、跨 workspace 或 privacy generation 未核对时，必须先维持最小权限并取得必要确认/验证。
- 文件权限、workspace confinement、Assessment acceptance、Memory admission 和普通问询权限隔离不因 Session 恢复而改变。
- Native Linux 与 macOS 证据必须验证 key backend、跨进程锁、私有权限、symlink/no-follow、原子保存、篡改拒绝和删除；交叉编译只证明编译，不属于当前版本的原生行为证据。

# Decisions

- 历史 Agent Session 默认自动保存；提供 `--no-save` 和持久设置关闭入口，不增加首次同意门槛。
- Session 不按时间自动删除，直到用户显式删除；达到硬上限不自动淘汰。
- 标题默认由当前 Agent 模型异步生成并更新；请求严格有界、无工具，失败回退到本地摘要；人工标题优先。
- 首版同时交付 `edu-agent agent resume ...`、`--last`、`--all` 和 TUI `F2` picker/switch。
- 默认按当前 workspace 过滤，全部范围显式开启；跨 workspace 恢复使用原 root，不静默替换。
- 使用平台 key backend 保护独立 profile key，并以 per-Session data key 加密内容；不可用时仅允许可见的 unsaved 降级。
- 恢复使用当前 provider/model 与当前安全规则；provider endpoint identity 变化需要一次明确确认。
- 文件 YOLO、旧 mutation authorization 和普通 pending interaction 不跨进程或 Session switch 恢复。
- 首版使用一个 Native change 和三个串行垂直批次，不建立 Supervisor children；原因是所有交付共享同一 checkpoint/store/controller 与隐私边界。
- 当前版本的支持与验收平台固定为 Linux 和 macOS；Windows 明确延期，不作为当前候选、Verifier 或 Archive 的阻塞项。

# Open questions

无。默认保存、保留期限、标题来源和恢复入口已由用户确认；其余安全选择按最小权限和 fail-closed 原则固定。

# Verification expectations

- `internal/agentsession` 聚焦测试覆盖认证加密、per-Session key、strict schema、wrong key/tamper、原子 publication、expected revision、single-writer lock、dirty marker、index rebuild、quota、delete/clear generation 和损坏隔离。
- `internal/agentloop` 聚焦测试覆盖 stable checkpoint export/restore、20+ turn compaction、exact-ID recall、authority/freshness、server/workspace invalidation、unknown side-effect receipt、YOLO 重置、provider change gate 和 active/pending export 拒绝。
- `internal/command` fake 依赖覆盖新建、`--no-save`、picker、UUID/标题/歧义、`--last`、`--all`、workspace unavailable、store unavailable、delete/clear、help 和 Dashboard 复用。
- `internal/agentui` fake conversation/store 覆盖 F2 idle gate、搜索/scope、rename/delete、目标预验证、safe switch、late event isolation、恢复 transcript、provider/workspace confirmation和 `46×18` 最小终端。
- fake OpenAI-compatible server 覆盖标题请求无工具、输入/输出边界、节流、失败回退、provider endpoint 变化确认前零发送，以及恢复后的下一次请求上下文等价。
- 受影响 Go 包运行最小测试、定向 race、vet 和 build；稳定候选再运行完整 CLI test/race/vet/build、non-TTY help 和 `git diff --check`。
- `.github/workflows/cli-platform.yml` 绑定 candidate SHA，在 native Linux 与 macOS 上运行 Session key backend、私有权限、跨进程锁、symlink/no-follow、原子恢复、篡改拒绝和 clear/delete matrix，并上传原始命令、Go 版本、skip count 与结果；任何 required case skip 都是 blocked。
