# 安全 mkdir 与统一文件副作用设计（C6，已实现并验证）

正式合同：[client-file-mkdir](../comet/specs/client-file-mkdir/spec.md)。C6 的生产垂直入口与直接测试已实现；下面同时记录实际结构、已有证据和未运行平台验证，不代替独立候选验收。

## 创建计划

输入`path`、`parents=false`。拒绝`.`、归档树及别名、不安全路径；已存在普通目录可直接返回unchanged，其他入口类型拒绝。Prepare仅检查，不创建；冻结已存在父入口的身份和缺失路径链。完整目标、已有锚点与计划范围必须在预览中可见，不能用截断路径获得授权。

Commit在首次创建前持久化WAL；重新打开原路径核对父身份，并验证已打开父句柄仍位于当前root且不在归档。Unix复用`verifyUnixArchiveLocation`/`checkArchivePublishParent`等句柄检查，以mkdirat非覆盖创建；Windows使用root-relative native FILE_CREATE，并保持祖先句柄无delete sharing，不能用os.MkdirAll拼接绝对路径替代。每步都遵守冻结计划，其他进程抢先创建、父替换或迁移等不自动接受为本次新建。

目录默认私人权限；不开放chmod/chown。成功创建项立即计入已知副作用。全部完成才是completed；中途失败/取消留下目录时保守unknown，不删除回滚。需明确记录已知创建项与可能尚未发生的计划，不能宣称未知为未发生。不新增后台清理或恢复重放。

和既有归档一样，这不是跨进程强锁或文件系统CAS。Unix最终位置核对与系统调用间仍有外部不协作进程的竞争窗口，不能宣称子树快照或绝对线性化保证；必须保留现有最靠近副作用的句柄/身份检查和保守结果语义。

## 统一记录

新建小型共享file-effect模型，供workspace、Loop、Controller和agentsession使用，避免继续叠加Path/ArchivePath特殊字段：

- 操作名与记录格式版本。
- 可选source与必要target端点，各含安全相对path、file/directory kind与可选版本。入口元数据版本与原始content hash不可混同；旧记录没有版本时不伪造。
- 受影响入口/子树范围；父列表、祖先metadata及范围find/search由统一影响规则失效。
- 有界的计划目录链及已知创建目录集合，明确区分计划与实际。
- tool call ID、completed/unknown与稳定结果码仍属于WAL/receipt包装。

write/edit/archive和mkdir均使用该模型，保留各自实际安全合同。legacy write/edit映射为单文件目标；legacy archive映射为source与固定归档target；旧数据未记载的创建项不能猜作已创建。统一类型中不存文件正文、原始文件身份或绝对root。

C6首次持久化版本升级：record payload v3→v4、dirty payload v2→v3；两个加密容器仍v1。冻结旧v3/dirty2 DTO并保留已有v1/v2迁移，旧格式中新字段即使为空也拒绝；未来认证版本仍ErrVersionUnsupported且不可清理。checkpoint若复用既有开放kind引用结构则无需无意义升级；如确实添加字段，必须同步严格版本兼容，不能暗改v1语义。

恢复只展示/失效证据，不执行文件操作。现有`BeforeFilePublication`可继续作为首次文件系统副作用前的门禁，不因创建目录而绕过。Controller的completed回执仍需匹配稳定事件、call ID和工作区引用；记录unknown时保留准确已知创建信息，不从缺失结果推导完成。

## 集成与验收边界

注册mkdir、默认确认/YOLO、活动/预览与中断事实保留。扩展`context_effects.go`的副作用识别，不能在长历史回收时抹掉mkdir事实。统一影响规则保留历史操作回执，只使受影响的文件/目录/metadata/find/search观察过期。

本批包括安全mkdir底层、workspace、Loop/TUI、Controller、Session兼容与直接测试，不能只交横向记录结构。测试使用临时根与fake provider，覆盖已有/新建/多级、拒绝/YOLO、父冲突/归档别名、部分创建、取消、WAL失败、版本迁移/未来版本拒绝及恢复不重放。只在垂直稳定后跑相关包、定向race和必要平台编译；不运行数据库/Compose，不实现copy/move或其回滚。

## 实际实现

代码位于 `clients/cli-go/internal/`：

- `securefile/mkdir.go`：`Root.PrepareMkdir(ctx, path, parents)` 返回 root 绑定、不透明的 `MkdirPlan`；只保留原路径、已有父锚点和内部身份。`Root.Mkdir(ctx, plan)` 单次消费计划，返回 `MkdirResult{Outcome, Created}`。`Created` 是本次已明确成功的创建前缀长度，不是从磁盘重新推断的数量。
- `securefile/mkdir_unix.go`：非覆盖 `mkdirat(0700)`，每步前重新打开原路径、核对已持有父身份、检查 root/归档位置；每次成功后先记录副作用，再 reopen/fsync。同步、关闭或后续创建失败都不删除回滚。
- `securefile/mkdir_windows.go`：复用 root-relative `FILE_CREATE` 目录原语；已有和新建祖先句柄保持无 delete sharing；创建成功标记早于句柄转换。其余平台 fail closed。
- `workspace/mkdir.go`：严格参数解码（包含拒绝 null 布尔、大小写字段别名及重复键）、已有目录 unchanged、冻结授权及完整范围预览、提交时单次校验与队列。路径超过已有预览/结果预算或无法保留完整 2 KiB 历史事实时在 prepare 拒绝，不截断路径授权。
- `fileeffects/effect.go`：值类型 `Effect`，含 `SchemaVersion=1`、`Operation`、`Source`、`Target`、`Scope` 和 `Directories`。端点版本使用 `entry-v1:` / `sha256:` 显式区分。`Directories{Anchor, Count, Created}` 与目标路径共同确定唯一完整前缀链；`PlannedPaths` / `CreatedPaths` 只在需要展示时展开，持久化不重复存储各级长路径。write/edit/archive 不推测旧记录缺失的创建信息。
- `agentloop`：WAL 使用值拷贝 `FileWriteAhead{ToolCallID, Effect}`；结果、活动、历史投影保留 mkdir 计划和真实创建前缀。`InvalidateFileEffect` 统一失效观察。新副作用引用使用操作 kind；旧 write/edit 事实还通过明确 operation/outcome 识别保护，不能被后续 metadata 或其他副作用抹掉。现有 checkpoint v1 的开放 kind 与工具结果承载完整事实，未添加 checkpoint 字段或改变容器版本。
- `agentsession`：live `FileReceipt` / `FileWriteAhead` 只使用统一 `Effect`，Path/ArchivePath 留在冻结 legacy DTO。record v1/v2、v3 与 dirty v1、v2 先按旧 DTO 严格解码及校验后 upcast；未记录的入口版本/创建项保持未知。加载旧 dirty 不重写证据；认证后的未来 record/dirty payload 或容器版本保持 `ErrVersionUnsupported`，不可当作损坏数据清理。
- `agentcontroller`：继续采用 `BeforeFilePublication` 加密 dirty 门禁；稳定 turn/call ID、workspace 引用及稳定事件全部匹配才生成回执。mkdir 还要求 checkpoint 中的实际 effect 与 WAL 计划一致。恢复仅说明与失效：已有创建前缀保留；只有 WAL 时明确“无已确认创建项不代表未发生，计划路径可能已创建”。不重放。
- `agentui`：注册创建目录名称；冻结创建目标/锚点/范围完整分页展示，末页显示后才可批准，避免4/10行摘录截掉长路径；拒绝/取消随时可用。YOLO文案包含mkdir，结果保留计划层数、已知创建层数和unknown说明。

## 已运行证据与限制

- 具名集合：`Mkdir|FileEffect|Archive|FileMutation|Payload|WorkspaceDefinitions|WorkspaceAllToolParsers|WorkspaceToolSchema|WorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls|CompactToolProse` 在七个受影响包通过，保留原 4096 四调用预算断言。
- 首次集合只有 `TestRecordPayloadMigrationRejectsMalformedAndBoundsVersions` 的旧迁移步数断言失败（旧值 2，实际 v1→v4 为 3）；已更新为 3，并保留“当前版本减步数等于 1”的边界断言，再次通过。
- `fileeffects`、`securefile`、`workspace`、`agentloop`、`agentsession`、`agentcontroller`、`agentui` 整包串行运行一次全部通过。之后两项局部收尾（中间父列表保守失效、安全前导空格路径与旧 DTO 严格校验区分）用相关具名集合复核通过；没有重复宽门禁。
- Linux 临时根与 fake provider 覆盖实际单/多级创建、拒绝/YOLO/WAL 失败/待授权取消、父替换和移出 root/移入归档、第二次创建失败、创建后取消/fsync 失败、实际部分创建的保存恢复、仅 WAL 崩溃恢复无重放、旧版本与未来认证版本。
- Darwin/Windows amd64 仅 `securefile` 的 `go test -c` 交叉编译通过；产物在 `/tmp/edu-agent-c6-compile.EYyrSS/`。未声称运行原生测试。AIX 非本批支持/验收平台，没有改动基线 `unix.Linkat` 问题。
- `git diff --check`通过。父代理C6的七包vet及Mkdir/FileEffect定向race通过；候选阶段补齐长路径完整分页后，agentui具名及整包、command/dashboard集成均通过。连续文件效果恢复已由[有界日志](client-file-effect-journal.md)修复，最终record/dirty均v6、容器/checkpoint均v1。没有全仓/数据库/Compose/真实provider矩阵或commit/push。
- 最终生产输入摘要：`589a4cb6c37252bf3783d602a27a98ec3a2dc00b32f780ec4598023ca8d89060`；精确命令和输入清单见 `/tmp/edu-agent-c6-handoff.md`。
