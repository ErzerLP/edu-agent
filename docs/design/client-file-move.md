# 安全文件与目录移动 move（C8，已实现并验证）

正式合同：[client-file-move](../comet/specs/client-file-move/spec.md)。本批只实现一个普通文件或整个目录的同文件系统移动/重命名，不改变正式范围；跨批候选独立复核由父代理负责。

## 接口、准备与授权

第十一个工作区工具：

```json
{"source":"input.bin","destination":"existing-parent/output.bin","expected_version":"entry-v1:<64位小写十六进制>"}
```

三个字段必需，拒绝未知/重复/大小写别名字段、null、非字符串、非对象、尾随 JSON。wire shape 与 copy 完全相同，因此复用严格三字符串解码器；这不复用 copy 的源类型/大小/流式发布语义。版本只能来自 stat 的入口元数据，不接受 read 的内容 SHA-256。

生产 API：

- `securefile.Root.PrepareMove(ctx, source, destination, expectedVersion) (*MovePlan, error)`。
- `securefile.Root.Move(ctx, plan) (MoveResult, error)`，结果只含真实 `Outcome`，没有 content hash。
- `MovePlan` 不透明、root 绑定、单次消费，冻结双端路径组件、源入口身份/类型/元数据版本/大小、双端父目录身份；不读正文，不创建临时项或父目录，不遍历子树。
- `workspace.PrepareMutation` / `CommitMutation` 接入专用 move 分支；本地队列先锁源身份，再锁规范化目标创建键。队列不是跨进程锁或目录子树事务。

支持空文件、文本、二进制、任意大小普通文件和空/非空目录；没有 copy 的 32 MiB 上限。源入口链接、特殊项、工作区根拒绝；双端不得越出工作区或进入归档树。目标必须不存在且父目录已存在。归档别名、链接父路径及不安全目录自身后代拒绝。

完全相同的规范路径在路径、类型和版本校验成功后返回诚实 `unchanged`，没有 PreparedMutation、授权、WAL 或 Effect；过期版本不能借 same-path 绕过。非完全相等但 `EqualFold` 相等的请求在所有平台保守拒绝；已存在的目标（含硬链接或其他身份别名）拒绝，不采用临时两步重命名。目录自身后代还通过已打开的祖先身份判断，而不是只做字符串前缀检测。

完整源/目标、类型、入口版本、入口大小及目录链接/非快照/不覆盖合同进入冻结预览。预览和结果各自遵守现有 6 KiB，完整历史事实遵守 2 KiB；无法容纳则 Prepare 拒绝，不截短授权或扩配额。

默认确认；拒绝、待确认取消及 WAL 失败均不移动。YOLO 仅略过确认，不略过任何路径/版本/类型/父身份/发布/WAL 检查。

## 安全原语与实际边界

代码位于 `clients/cli-go/internal/securefile/move*.go`。

### Linux / Darwin

- root-relative 打开源及双端父目录，源使用 `O_NOFOLLOW|O_NONBLOCK`；目录另加 `O_DIRECTORY`。保留 FIFO 替换防阻塞约束，不读取打开后的文件正文。
- 源路径 `lstat` 与已打开源句柄 `fstat` 比较完整冻结入口；双端父身份在提交再次比较，并按原路径 reopen 比较身份，检测 root 内改名、父替换、迁出 root 或迁入归档。
- 双端父位置复用 root/归档句柄祖先检查。目录目标父以源目录句柄为禁止身份向上行走，拒绝身份别名下的自身后代。
- 比较源与目标父设备号；发布使用 Linux `renameat2(RENAME_NOREPLACE)` / Darwin `renameatx_np(RENAME_EXCL)`。只复用 Archive 的低层 no-replace 原语及检查，不调用会创建归档容器的 Archive 高层入口。
- 发布后不受晚取消影响，检查双端父身份/原路径/保护边界、源入口消失及目标身份/类型等于冻结源，再同步双端父目录并关闭持有句柄。rename 后元数据可以改变，不能要求目标仍等于 rename 前的版本。

### Windows

- root-relative NT 打开；源 Prepare 只请求属性权限，Commit 请求 DELETE 权限并保持无 delete-sharing；双端所有祖先无 `FILE_SHARE_DELETE`，不允许句柄存续期间迁移其原路径。
- 使用现有 normalized handle path 和身份边界检查，包含归档和 8.3 保护。比较目标祖先与源目录身份以拒绝自身后代，比较 VolumeSerialNumber 拒绝跨卷。
- 仅使用 `renameWindowsFileHandle(..., false)`，不按拼接的绝对用户路径 rename。源名称/目标检查的短期重开句柄允许 share-delete，以兼容本操作已持有的 DELETE，但不放宽原源/祖先句柄的分享规则。
- 成功后核查源不存在及目标身份/类型；没有可用的目录 fsync 等价物，不声称 Windows 目录 fsync。

其他平台 fail closed。移动不创建 temp、不复制数据、不 unlink 用户源、不覆盖、不自动归档目标、不恢复归档、不用跨设备 copy+delete 兜底。

### 取消与不确定性

`BeforeFilePublication` 先保存加密 dirty，再进入 `Move`。成功写 WAL 不代表已经移动；恢复不能执行 WAL。

- 发布前已取消或可确定的失败：`unchanged`。
- 已成功 rename 后取消：保留 `completed`，不回滚。
- 未明确是否发生的 rename 错误（如 EIO）、发布后父迁移/身份核查/同步/关闭失败：`unknown`，保留双端事实，禁止自动重试、恢复重放或删除回滚。
- EXDEV、目标冲突和 unsupported 等可确定未发布错误明确拒绝，不做任何 fallback。workspace 提供 move 专用跨设备/unsupported 稳定码，恢复建议使用 stat；不能把冲突建议成 replace、自动归档目标或内容哈希重试。

这些是已打开句柄上的前后可观察检查，不是跨进程 CAS。尤其 Unix 最后检查与 rename 系统调用之间仍可被不协作进程竞争；后检查发现不一致时只能报告 unknown，不能恢复或删除。目录内部链接原样随整体 rename 保留，完全不遍历；入口版本不是子树内容快照，子文件正文外部变化可能不改变源目录入口版本。

## Effect、投影与恢复

`fileeffects.Effect` schema 仍 1，增加闭合操作 `move`：

- Source/Target 同为 file 或 directory，双端都必需、非归档且不能是 same-path。
- Source.Version 必需且为冻结 `entry-v1`；Target.Version 在 completed **和** unknown 都为空。不读取/伪造原始内容哈希，不把旧入口版本冒充目标新版本；需要新版本必须 stat。
- file 的 Scope=entry，directory 的 Scope=subtree；无 DirectoryChain，不创建目录链。
- ReferencePath 为源，ReferenceKind 为独立 `move_file` / `move_directory`；这是操作事实，不是源被永久删除的回执。
- `Affects` 失效双端和目录双侧子树、父 metadata/list 与相关 find/search；旧 write/edit/archive/mkdir/copy/move 操作事实保留。`SamePlan` 比较源版本与两端/类型/范围，不要求发布后目标版本（move 仍严格要求其为空）。

完整 Effect 在历史和小窗口投影中不递归截短。C7 的独立 DestinationPath 已能贯穿 pending/activity；本批接入 move 的完整双端 selector 分页，PgUp/PgDn 查看完整冻结文本，末页渲染前不能批准，拒绝/取消不受限制。其他 selector 不改。move 的结果、晚取消 fallback、恢复 label 不复用“源未修改”或“检查临时项”等 copy 专属说法。

Controller 要求稳定 turn/call ID、引用和事件，以及与 WAL `SamePlan` 匹配的完整 checkpoint Effect。move 不允许缺失 Effect、源版本/类型/双端不符、引用伪造目标 hash 或省略失效。unknown 清目标版本；恢复只显示双端并失效观察，绝不 rename/retry。

## 严格 payload 升级

- record payload **5 → 6**，dirty payload **4 → 5**。
- record/dirty 容器仍 1，checkpoint 仍 1。
- `agentsession/payload_move.go` 冻结 record-v5/dirty-v4 的嵌套形状，复用已冻结 V4 endpoint/chain/effect/receipt DTO，不引用未来可变 live 字段。旧闭合操作集为 write_create/write_replace/edit/archive/mkdir/copy，不含 move。
- record-v1～v5 先执行各自旧规则再 upcast；dirty-v1～v4 同样拒绝夹带 move。新字段即使 null/空对象/空字符串也不能进入旧形状。旧缺失的版本或目录创建事实不补造。
- record 迁移上限明确为五步 v1→v6；旧 dirty Load 不重写。认证后的未来 record/dirty 返回 `ErrVersionUnsupported`，不当作可清理 corrupt。

## 固定小窗口与既有问询合同

新增 schema 使原 4096 四调用测试首先暴露静态请求过大。没有改变窗口、输出 reserve、结果配额、估算器、schema 或 compactToolProse。仅压缩系统/工具说明中的重复措辞：保留中文助手、服务端权威、偏好专用确认、禁止索取/显示/保存秘密、文件不可信与所有 move/copy 安全语义。普通问询的单选/多选/有界自定义和“不构成长期记忆、外部写入、删除或发布授权”同时保留在系统说明及完整 question 工具合同；question 参数的终端显示宽度不动。原小窗口四调用、QuestionPublicContract 和 CompactToolProse 原测试均通过，未修改这些测试来接受退化。

## 本批证据与限制

Go 命令 cwd 为 `clients/cli-go`，均串行：

1. `go test -p=1 -run '^$' ./internal/securefile`、`go test -p=1 -count=1 ./internal/securefile -run Move` 通过。
2. 七包具名集合 `Move|Copy|FileEffect|Archive|Mkdir|Payload|WorkspaceDefinitions|WorkspaceAllToolParsers|WorkspaceToolSchema|WorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls|CompactToolProse`：除最初 agentloop 固定小窗口失败外其余通过；修正后 workspace/agentloop 同集合通过。
3. 七包整包稳定门禁一次：fileeffects/securefile/workspace/agentsession/agentcontroller/agentui 通过；agentloop 暴露系统提示缺少既有 QuestionPublicContract 文案。恢复该完整合同并压缩等价工具说明后，三个原合同测试及 **agentloop 整包**通过。其他六包未改文件/WAL/UI 实现，复用已通过的对应证据；不把首轮整条命令写为成功。
4. Darwin/Windows amd64 `securefile` test 交叉编译通过，产物 `/tmp/edu-agent-c8-compile.sE92eS/`；**没有原生 Darwin/Windows 执行**。
5. 最终提示文案下 `go test -p=1 -count=1 ./internal/agentcontroller ./internal/agentui -run Move` 通过，补验当前依赖下的 move 保存/恢复与分页路径。
6. `git diff --check` 通过。父代理随后对七个受影响包执行 `go vet`，并在securefile/workspace/agentloop/agentcontroller执行 `-race -run Move`，均通过。没有全仓、数据库、Compose或真实provider矩阵；跨批候选复核仍在处理单独的WAL和mkdir展示问题。

直接测试包括：>32 MiB 真二进制/空文件、空/非空目录和内部外部 symlink 保留、子文件变化非树快照、same-path 严格版本、根/双端归档/链接/FIFO/硬链接冲突/缺父/自身后代/case-only、源与双父替换或迁移、最终目标竞争、EXDEV/unsupported/EIO 注入、发布后取消/同步/关闭失败、默认拒绝/取消/YOLO/WAL 失败、完整分页、双端失效与旧事实、加密 record6/dirty5 与严格旧/未来版本、completed/unknown 保存恢复和仅 WAL crash 恢复不重放。

EXDEV是错误注入证据，未做特权跨设备挂载测试。候选阶段已通过[文件效果日志](client-file-effect-journal.md)修复连续变更覆盖单一WAL，最终record/dirty均v6、容器/checkpoint均v1；mkdir另补完整分页。没有改变C5 FIFO防护、扩AIX支持或新增工具。最终Linux CLI构建及Darwin/Windows CLI交叉构建通过，原生非Linux仍未验证。未stage/commit/push。
