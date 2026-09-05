# 客户端本地文件操作设计

本文件记录五个文本工具的基础设计；新增文件/目录安全归档、归档目录保护与会话恢复扩展见 [client-file-archive.md](client-file-archive.md)。归档不是通用 move/delete，不改变下述文本工具的内容合同。

## 目标与边界

本设计实现 `client-file-operations` 正式规格：交互式 Go CLI Agent 在单个固定工作区内使用结构化 `list`、`read`、`search`、`write`、`edit` 工具处理 UTF-8 文本文件。工作区、参数、资源、链接、版本、原子发布和取消由客户端执行器强制；system prompt 只指导模型，不承担安全控制。

首版不加入 shell、删除、移动、复制、多文件 patch、二进制处理、工作区外授权、持久 yolo 配置或服务端 API。工作区内不设置 `.git`、`.comet`、`.env` 等特殊 denylist。

## 现有调用链

```text
command.App.runAgent
  -> agentloop.New
  -> agentui.Runner.Run
  -> Session.contextPlan
  -> modelclient request with tools
  -> Session.processCalls
  -> tool result appended to Session
  -> next sibling / next model request
```

现有 `Session.processCalls` 按 assistant tool-call 数组顺序执行；`ask_user_question` 与长期偏好确认会保存 calls、当前位置和已完成 events，并通过类型化 resolver 从下一兄弟工具恢复。文件 mutation 复用该暂停结构，但使用独立类型和 resolver。

当前 `appendToolResult` 固定把结果标记为 `AuthorityServerSnapshot` 并参与 server generation invalidation。文件结果必须使用新的本地工作区 authority，不能走该默认路径。

## 包与职责

### `internal/workspace`

新增高层工作区包，负责：

- 五个工具的 schema 和严格参数解析；
- slash 相对路径、UTF-8、二进制、BOM、换行和内容版本；
- `list/read/search` 的有界行为；
- `write/edit` 的 prepare、预览、版本校验和 commit；
- 同文件 mutation 串行化；
- 稳定错误码和模型结果；
- 不向结果暴露绝对工作区根或底层 OS 错误。

建议文件：

```text
internal/workspace/
  workspace.go
  definitions.go
  types.go
  errors.go
  path.go
  text.go
  read.go
  search.go
  mutation.go
  edit.go
  diff.go
  queue.go
```

### `internal/securefile`

扩展现有 `securefile.Root`，提供 root-bound 的目录枚举、regular file snapshot 和原子发布。现有路径型 `AtomicWrite` 保持配置/凭据语义，不改成普通工作区写入。

Unix 使用 root FD、逐组件 `openat`、`O_NOFOLLOW`、发布前 parent ancestry 复验和 parent-FD 相对发布。Windows 使用保留的 root/parent handle、`NtCreateFile` 的 handle-relative component open 与 `FILE_OPEN_REPARSE_POINT`、handle-relative rename/cleanup；禁止在安全关键路径重新拼接 `root.path` 后调用路径型 open/create/remove/rename。原生 Windows 验收必须覆盖 junction、reparse、ADS、盘符、大小写/8.3/硬链接别名、ACL 和 root/parent exchange。

高层 `workspace` 禁止直接用 `filepath.Join(absRoot, modelPath)` 调用 `os.ReadFile`、`os.WriteFile`、`filepath.WalkDir` 或字符串路径 `Rename`。

### `internal/agentloop`

扩展 `Options` 和 `Session` 注入可选 workspace executor。工具面改为每个 Session 动态组合：

```text
现有 Tools()
+ workspace.Definitions()（仅 workspace 成功建立时）
```

文件只读结果使用独立 append/projection 路径。文件 mutation 增加第三种 pending interaction，不复用普通 question 或 preference resolver。

### `internal/agentui`

增加：

- F4 文件模式 selector；
- Session 默认 confirm，可切换 yolo；
- 专用文件 mutation selector；
- footer 中持续可见的 `文件 确认` / `文件 YOLO`；
- 工作区安全短标签；
- `Ctrl+O` 中的相对路径、读取范围、搜索进度、预览/diff 和稳定错误码。

## 工作区生命周期

`edu-agent agent` 使用独立 `flag.FlagSet` 解析可选 `--workspace PATH`。未提供时在启动时调用 `os.Getwd()`。命令层把输入解析为绝对路径并打开固定 root；Session 之后不依赖进程 cwd。

工作区初始化失败时普通 Agent 对话仍可启动，但：

- 不向模型暴露五个文件工具；
- TUI 显示脱敏 `workspace_unavailable`；
- 不返回绝对路径或原始 OS 错误。

`--workspace` 和 yolo 都是 Session 状态，不写入 `config.Config` 或 `AgentConfig`。

## 模型路径合同

目录根使用 `.`。其他模型路径必须是 UTF-8 slash 相对路径。

拒绝：

- 绝对路径和盘符/UNC/device path；
- 空组件、内部 `.`、`..`；
- 反斜杠；
- NUL、控制字符和双向控制字符；
- Windows ADS、保留设备名、尾随点/空格和平台非法字符；
- 超过 4096 UTF-8 bytes、1024 runes 或 64 个组件。

文件路径不得为 `.`；目录参数可以为 `.`。返回模型的路径统一为 slash 相对路径。

`list` 可以把 symlink、junction 或 reparse entry 标记为 link，但 `read/search/write/edit` 不跟随它们。

## 固定资源限制

限制集中在 `workspace.Limits`；测试可以注入更小数值，生产不由模型放宽。

| 限制 | 首版值 |
| --- | ---: |
| 单次 list 条目 | 200 |
| list/read/search 模型结果 | 6 KiB |
| read 单次行数 | 200 |
| 可读取/检查单文件 | 1 MiB |
| search 匹配数 | 100 |
| search 文件数 | 2,000 |
| search 总扫描量 | 16 MiB |
| search 最大深度 | 64 |
| search 单行预览 | 512 bytes |
| edit replacements | 32 |
| mutation preview/diff | 6 KiB |
| 工具时间 | `Options.ToolTimeout` |

当前模型 tool arguments 单调用上限为 8 KiB，因此首版 `write` 正文天然受该边界约束；schema 仍显式限制并返回稳定错误。

## 内容和版本

内容版本使用磁盘原始 bytes 的 SHA-256，包含 UTF-8 BOM、CRLF/LF、最终换行和全部空白。模型只看到 `sha256:<hex>`，不看到 inode、volume ID 或绝对路径。

读取顺序：

1. 安全打开 regular file；
2. 检查大小；
3. 有界读取；
4. 重新检查同一 handle 的 identity/size/mtime；
5. 计算 SHA-256；
6. 验证 UTF-8；
7. NUL 或明显控制字符内容返回 `binary_file`。

`read` 返回 1-based 行范围、内容版本、完整性、截断原因和下一 offset。首行超过结果预算时在 UTF-8 rune 边界返回行前缀，并提供同一行的 byte offset 继续位置。

## 工具行为

### `list`

- 只列直接子项；
- 包含 dotfiles，不应用 `.gitignore` 隐藏；
- 稳定排序；
- 区分 file/directory/link/other；
- 结果达到条目或字节限制时返回可恢复截断；
- 单项检查失败只增加 skipped 或返回稳定局部错误，不泄漏绝对路径。

### `read`

- 只读 regular UTF-8 text；
- 支持 1-based line offset 和 line limit；
- 结果达到 line/byte limit 时显式给出 next position；JSON 转义导致结果二次收缩时，必须把实际 UTF-8 前缀重新映射为准确的 line/byte continuation，不能把跨行字节数错误附加到初始行；
- 跨页可携带 expected version，版本变化返回 `content_changed`。

### `search`

- 支持 literal/regex、smart/sensitive/insensitive case、路径范围和 include/exclude；
- regex 使用 Go RE2；
- handle-relative 遍历，不用 `filepath.WalkDir`；
- 包含 dotfiles 和 `.git`/`.comet` 普通文件；
- 跳过 link、binary、invalid UTF-8 和超限文件并返回计数；
- 匹配按 path/line/column 稳定排序；
- 无匹配是成功空结果；
- files/bytes/matches/result/deadline 任一达到上限时显式标记不完整。

### `write`

公开模式：

- `create`：目标必须不存在，不接受 expected hash；
- `replace`：目标必须存在，expected hash 必填。

`create` 授权后若目标被创建，返回 `already_exists`，不自动升级为覆盖。`replace` 在 commit 前重新校验内容版本。

### `edit`

- expected hash 必填；
- 1–32 个 replacement；
- 每个非空 `oldText` 必须在同一原始内容中精确、唯一匹配；
- 所有 range 基于同一原始 snapshot；
- 重叠、嵌套、缺失、多匹配和 no-op 均在写入前失败；
- 首版不做 Unicode/引号/dash/尾随空白 fuzzy fallback；
- 候选内容保留 BOM 和主换行风格；
- 返回有界 diff、首个变化行和新版本。

## 安全发布

mutation 分为 `Prepare` 与 `Commit`。

`Prepare`：

- 严格解析参数和路径；
- 安全读取当前状态；
- 校验 expected version；
- 生成确定候选 bytes 和有界 preview/diff；
- 不创建临时文件，不持锁跨越用户授权等待。

`Commit`：

1. 进入同目标 FIFO；
2. 重新安全读取目标；
3. 校验 expected/base version；
4. 确认 candidate 与预览一致；
5. 在目标 parent 内创建临时文件；
6. 写入并 flush；
7. 使用 create-no-replace 或 replace 进行同目录原子发布；
8. sync parent；
9. 返回最终 hash 和 side-effect 状态。

Unix create 使用同目录 `linkat + unlinkat` 的 no-replace 发布，replace 使用 parent-FD `renameat`。Windows 使用同一安全 parent handle 下的临时 handle 与 `NtSetInformationFile` 完成 create-no-replace 或 replace。临时文件 cleanup、parent sync 或底层 syscall 结果不能确认时返回 `outcome_unknown`；create 为缺失 parent 建目录后若后续失败，也不能再声称磁盘完全 `unchanged`。

权限合同限定为文件访问控制，不承诺复制所有宿主元数据：Unix replace 在 Commit 重新读取并保留当前 `0777` permission bits，新 regular file 使用 `0644`、新目录请求 `0755`；Windows replace 保留目标 DACL，新文件和目录使用 parent 的正常 ACL 继承。owner/group、SACL、扩展属性、文件标志和时间戳不属于首版兼容性合同；实现不得把它们误报为已保留。

同文件进程内 mutation 使用 FIFO；等待授权期间不持锁。不同文件当前仍由 Session 顺序执行，不为本 change 改成并行调度。

### 外部并发限制

首版不实现跨进程文件锁或版本控制。执行器会在紧邻原子发布前重新读取并校验 expected hash；在该检查前观察到外部修改时返回 `content_changed`。

便携文件系统没有“仅当目标 inode/hash 仍等于 X 且 parent 仍属于固定 root 才 rename”的条件式原语，因此非协作进程恰好发生在最终 expected-hash/parent-ancestry 复验与 rename 之间的修改或 parent move 不能获得跨进程线性化 CAS 保证。实现必须把两个窗口缩到最小、以保留的 root/parent handle 阻止路径重新解析、保持同进程 file-ID FIFO，并在 syscall 或 cleanup 结果无法判定时返回 `outcome_unknown`；不得宣称存在跨进程强锁。这与正式非目标“跨进程文件锁、多人协同编辑或文件版本控制”一致。

## 授权状态机

```text
SessionStart -> ConfirmIdle
ConfirmIdle --F4--> ModeSelector
ModeSelector --选择 yolo 并 Enter--> YoloIdle
ModeSelector --Esc--> 原模式
YoloIdle --F4/选择 confirm--> ConfirmIdle

ConfirmIdle --write/edit prepared--> WaitingFileAuthorization
WaitingFileAuthorization --允许--> Revalidate -> Commit -> Continue
WaitingFileAuthorization --拒绝--> authorization_denied -> Continue
WaitingFileAuthorization --Esc--> discard pending turn -> Stopped

YoloIdle --write/edit prepared--> Revalidate -> Commit -> Continue
```

每个 mutation 在参数、路径、版本和预览准备完成后冻结当前模式：

- 已进入确认的调用不会因稍后切换 yolo 自动获批；
- 已冻结 yolo 的调用不会因切回 confirm 被重新暂停；
- pending 文件授权期间 F4 不重解释当前请求；
- 新 Session 始终回到 confirm。

进入 yolo 需用户主动在 F4 selector 中选择并 Enter。警告显示：工作区内隐藏文件、`.git`、`.comet` 和秘密文件无额外保护，内容可能发送给当前 provider；yolo 只取消确认，不取消其他安全校验。

## 专用 pending 与 resolver

新增独立类型：

```go
type FileAuthorizationMode string // confirm | yolo
type FileMutationOperation string // write_create | write_replace | edit
type FileMutationResolution string // approve | decline

type PendingFileMutation struct {
    CallID      string
    Tool        string
    Operation   FileMutationOperation
    Path        string
    PreviewKind string
    Preview     string
    Truncated   bool
    BaseVersion string
}
```

`Result` 增加 `PendingFileMutation`。Session 增加：

```go
FileAuthorizationMode()
SetFileAuthorizationMode(mode)
ResolveFileMutation(ctx, callID, resolution)
CancelPendingFileMutation(callID)
```

内部 pending 保存 prepared mutation，UI 不能构造或替换 candidate。

普通 `ask_user_question` answer 和 preference resolver 永远不能调用文件 commit。pending kind、call ID 和 `pendingResolving` 共同拒绝 resolver 类型混用、重复提交和迟到事件。

### 拒绝与 Esc

- `decline`：不执行 mutation，写入绑定原 call ID 的 `authorization_denied` tool result，继续兄弟工具和模型；
- 文件 selector `Esc`：停止整个 pending turn，清除未完成模型历史，不执行兄弟工具或下一模型；不等价于 decline。

## 已发布副作用与取消

原子发布是文件 mutation 的线性化点：

- 发布前观察到取消：目标必须保持不变；
- 发布成功后晚到取消：成功事实胜出，不能显示“文件未修改”；
- syscall 结果无法判定：返回 `outcome_unknown`。

现有 `discardTurn` 会删除当前 turn 的全部 assistant/tool history。Session 必须记录已提交文件效果：

- 尚无 committed/unknown mutation 时沿用完整 discard；
- 已有 committed/unknown mutation 时保留对应 assistant call 和原 call ID tool result；
- 丢弃未执行兄弟工具；
- 不再发起模型请求；
- 追加确定性本地 fallback：“文件修改已完成；后续处理已停止”或“文件发布结果无法确认，请重新读取”；
- 使下一轮不会遗忘真实磁盘副作用。

## 本地 authority 与上下文

新增：

```text
AuthorityWorkspaceSnapshot
FreshnessWorkspaceObserved
FreshnessWorkspaceSuperseded
WorkspaceReference{Path, ContentHash, Kind}
```

文件结果：

- 不携带 server generation；
- 不参与 server invalidation；
- 不成为 Knowledge/Learning/Review/Memory/Nocturne 权威数据；
- 同路径新版本只把旧 workspace source 标为 superseded；
- 历史 source 仍可按 exact opaque memory ID 回查，但明确标记历史版本；
- observer/reflector 不得从文件正文生成 user intent、constraint、preference 或 authorization；
- compaction 只保存相对路径、版本、范围和有界摘要，不保存原始隐藏工具参数。

当前轮次的 workspace tool result 采用最多四调用的公平预算，而不是让第一个大结果耗尽累计预算。超限投影按工具保留可恢复合同：`list` 保留条目样本与 `next_offset`，`read` 保留 UTF-8 内容片段、hash 与继续 offset，`search` 保留匹配样本与扫描统计，`write/edit` 保留操作、发布结果、版本和有界 preview/diff；所有失败同时保留稳定 `message` 与恢复 `suggestion`。在受支持的最小 `ContextWindow=4096` 且启用 workspace 时，下一请求保留 512 tokens 输出预算；若当前完整工具轮仍超限，只在请求投影中把已经完成的 assistant tool-call arguments 替换为 `{}`，Session 原始执行记录和 workspace authority 不变。

现有 context 代码中“非 session 即 server-derived”的分支必须改为显式 authority 判断。

## TUI 设计

### F4 模式 selector

选项：

1. `逐次确认`：每个 write/edit 前显示预览或 diff；
2. `YOLO`：工作区内 write/edit 不再逐次确认，仍保留全部安全校验。

Esc 关闭，不改变模式。

### 文件 mutation selector

选项：

1. `允许此次修改`；
2. `拒绝此次修改`。

面板显示操作、相对路径、有界新文件预览或 diff、截断状态、版本摘要和 `Esc 停止当前轮次`。不提供 custom input。

### 持续状态

footer 高优先级显示：

```text
文件 确认
文件 YOLO
```

工作区只显示安全短标签，不显示绝对路径。YOLO 状态在窄布局中不得先于普通 model name 被裁掉。

`Ctrl+O` 继续作为统一详情开关；Activity 增加 presentation-safe 文件详情，不能把原始 arguments 塞进 `Event.Detail`。所有五个文件工具在 filesystem 工作前先从严格解析并规范化后的参数发布相对路径；`read` 在读取前、取得 bounded snapshot 后和结果完成时原地更新同一 call ID，`search` 在首个文件、每 32 个文件、每 256 KiB、每 16 个匹配或最终状态更新，且每次搜索最多 64 个事件。取消或超时必须把同一 call ID 更新为 stopped/failed，不能让已显示卡片停留在 running。详情只含相对路径、范围、返回/扫描字节、扫描计数、continuation、稳定错误码和最多 6 KiB 的最终 preview/diff；TUI 再限制为最多 4 KiB、32 个显示行。慢读取/搜索状态显示等待时间、相对路径或扫描文件/字节、超时预算与 `Esc` 提示。

## 稳定错误码

至少包括：

```text
invalid_arguments
invalid_path
path_outside_workspace
not_found
already_exists
not_directory
unsupported_type
link_not_allowed
permission_denied
invalid_utf8
binary_file
file_too_large
regex_invalid
content_changed
replacement_missing
replacement_not_unique
replacement_overlap
no_changes
authorization_denied
cancelled
timeout
outcome_unknown
workspace_unavailable
internal_error
```

错误正文不得包含绝对根、环境变量、设备 token、客户端凭据、临时路径或未请求正文。截断不是错误，而是成功结果中的 `complete=false` 和结构化原因。

## 垂直批次

### 批次 1：工作区与只读工具

结果：默认 cwd/`--workspace` 固定工作区，模型可完整走通 `list/read/search -> tool result -> 下一模型回答`；本地 authority 正确；初始化失败仍可普通对话。

延期：write/edit、授权、yolo、atomic publish、mutation race。

退出标准：

- 相对路径、越界、link 和脱敏错误成立；
- list dotfiles/link 类型/稳定排序；
- read 可恢复分页；
- search 有 bounds、取消和稳定排序；
- 文件结果不再标记 server snapshot；
- 取消后不执行 sibling 或下一模型；
- `.git/.comet/.env` 按普通路径读取。

### 批次 2：写入、编辑、授权与 yolo

结果：用户可批准/拒绝 create/replace/edit，也可切换 yolo；expected hash、同文件 FIFO 和原子发布成立。

延期：完整平台矩阵、context compaction 大历史、候选门禁。

退出标准：

- create/replace/edit schema 严格；
- confirm 默认、普通 question 不授权；
- deny 绑定原 call ID；
- yolo 只跳过确认；
- edit 零/多匹配、重叠和冲突无副作用；
- publish 前 cancel 不变，publish 后 late cancel 不覆盖成功；
- 已发布 mutation 后 turn 失败不会被 discard 遗忘。

### 批次 3：集成与跨平台收口

结果：安全路径、authority/compaction、取消、TUI 详情、窄终端、Unix/Windows 发布语义和文档形成稳定候选。

退出标准：

- observer/reflector 不把文件内容升级为指令或 server facts；
- workspace source 不参与 server invalidation；
- Ctrl+O、footer、慢搜索和窄终端符合规格；
- Unix symlink/目录交换和 Windows reparse/junction/ADS 原生测试；
- BOM、LF/CRLF、binary、invalid UTF-8、limits 稳定；
- CLI README 与 `agent --workspace` usage 同步。

## 测试策略

实现期间只运行能够证伪当前批次的具名测试，再运行受影响 package。批次稳定后才运行一次适用的 package/vet/race/build。

核心具名测试应覆盖：

- path 绝对/`..`/control/ADS/device；
- list/read/search bounds、排序、pagination、cancel；
- Unix/Windows deterministic link/reparse swap；
- create/replace expected hash；
- edit 同原始 snapshot、unique、overlap、no-op；
- mutation FIFO 与外部修改冲突；
- confirm pause、原 call ID、deny、Esc、resolver 隔离；
- yolo 默认/切换/冻结/非持久；
- committed mutation 后 cancel/follow-up model failure；
- workspace authority、server invalidation 隔离和 compaction；
- TUI F4、preview/diff、Ctrl+O、footer、YOLO warning、窄终端。

候选门禁仅覆盖受影响客户端 packages、CLI build、定向 race 和 `git diff --check`。无需 PostgreSQL、Compose、OpenAPI、migration 或服务端黑盒，除非实现意外扩大边界。Windows reparse/junction/ADS/DACL/硬链接结论必须来自原生 Windows runner，Linux cross-compile 只证明编译兼容；关键 Windows fixture 不得以 `t.Skip` 形成成功路径，SHA-bound evidence 必须记录该 check 的 skip 数并要求为零。
