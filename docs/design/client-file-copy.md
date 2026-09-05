# 安全流式文件复制 copy（C7，已实现并验证）

正式合同：[client-file-copy](../comet/specs/client-file-copy/spec.md)。本批只增加普通文件复制，不实现 move、目录复制、覆盖、父目录隐式创建、永久删除或归档恢复。以下描述当前生产实现，不代替独立候选验收。

## 生产接口与授权

第十个工作区工具：

```json
{"source":"input.bin","destination":"existing-parent/copy.bin","expected_version":"entry-v1:<64位小写十六进制>"}
```

三个字段都必需且非 null 字符串；拒绝未知字段、重复键、大小写字段别名、非对象和尾随 JSON。`expected_version` 必须来自 stat 的入口元数据，不接受 read 的原始内容哈希。源必须为普通文件，允许二进制及空文件，最多 32 MiB（包含边界），独立于文本读取的 1 MiB 限制。

`workspace.PrepareMutation` / `CommitMutation` 调度到 `workspace/copy.go`。Prepare 规范化双端路径，检查归档保护、源及父身份、目标不存在和父目录存在。`securefile.CopyPlan` 不透明、root 绑定且单次消费；冻结原始路径组件、源入口身份/元数据版本/大小/权限及双方父身份，不读取正文、不创建临时文件或目录。授权预览包含双端、入口版本、大小、权限策略和不覆盖/不建父目录合同。

完整预览、结果及完整 2 KiB 历史副作用事实不能放入现有预算时，Prepare 拒绝；没有扩大正文、参数、结果、checkpoint 或历史配额。TUI 的 copy 预览专门分页显示，窄屏不再只显示前几行就允许批准：PgUp/PgDn 查看完整冻结内容，末页渲染后才能批准；随时可拒绝/取消。其他工具的 selector 行为未改动。YOLO 仅跳过确认，不能绕过版本、类型、位置、大小或 WAL。

模型系统说明已移除旧的 copy 禁令，明确复制的 32 MiB、stat 版本、保留源、双端归档禁止、不覆盖及父目录已存在限制。小窗口压缩工具说明时这些限制仍在系统说明中；原 4096 四调用门禁保持通过。

## 原语与磁盘事实

代码：`clients/cli-go/internal/securefile/copy.go`、`copy_unix.go`、`copy_windows.go`、`copy_other.go`。

- `Root.PrepareCopy(ctx, source, destination, expectedVersion) (*CopyPlan, error)`。
- `Root.Copy(ctx, plan) (CopyResult, error)`；结果含 `Outcome` 和仅 completed 时有效的实际 `ContentHash`。
- 固定 32 KiB 缓冲，读取冻结长度并额外探测最多 1 字节以发现增长；边复制边 SHA-256，不 `ReadAll`，不经模型或 WAL 保存正文。
- 临时文件由客户端随机命名，独占创建在已存在的目标父目录下。流式写入、设置普通权限、同步后，重新检查源入口/已打开源、双端父身份、原路径位置和归档边界，再以 no-replace 发布。
- 源只打开读取，从不修改或删除。Unix 只保留 `Perm()` 普通 rwx 位，不传播 setuid/setgid/sticky；Windows 使用普通 `FileMode.Perm()` 的原生权限映射（主要为只读属性），不宣称复制 POSIX ACL 或所有 rwx 语义。不复制 ACL、xattr、稀疏布局或硬链接关系。
- Unix：root-relative `openat`、`O_NOFOLLOW|O_NONBLOCK` 源读取，避免 FIFO 替换导致阻塞；父目录按原路径重新打开比较身份，复用 root/归档句柄位置核查；临时文件使用 `O_CREAT|O_EXCL`。发布使用 Linux/Darwin 专用 no-replace rename，不采用 overwrite rename 或源 copy+delete。目标竞争失败不覆盖。
- Windows：root-relative NT 创建/打开；祖先句柄无 delete sharing，源使用 pinned read share，临时文件 exclusive handle；归档别名检查复用 normalized handle path/identity（包括 8.3 保护）。发布使用不替换的 handle rename。没有目录 fsync 等价物，不宣称已做 Windows 目录 fsync。
- 其他平台 fail closed。没有修改基线 AIX `Linkat` 支持问题。

### 取消与清理

WAL 在首次临时文件创建/写入前持久化；WAL 失败、未批准和待批准取消不进入提交。提交中的早取消/普通失败，在目标尚未发布且临时项能安全清理时为 unchanged。晚取消不能抹掉已发布结果。

Unix 清理前核查目标父的身份、原路径与 root/归档位置，并比较仍打开的临时 fd 与当前临时名字的设备/inode、普通入口及链接数量。只 unlink 仍匹配的客户端临时项；临时名字被替换、父迁移或无法确认拥有权时不清理，保守 unknown。Windows 按独占临时句柄清理，不按用户提供的名字删除；rename 返回不确定状态后不发出 delete。

不明确的临时创建错误、不明确 rename、发布后的父同步/关闭/身份检查失败、无法安全清理的临时项均保留 unknown；目标版本清空，不从候选哈希推断成功，不自动重试、恢复重放或删除回滚。结果中的 `source_unchanged` 表达本操作未修改源，不是跨进程源文件完整快照的承诺。

这些前后检查是可观察变化检测，不是跨进程 CAS、完整快照或对不协作进程的全局锁。Unix 最终身份核查与 rename/unlink 之间仍存在系统调用级竞争窗口；不夸大保证。父目录被外部迁移时，可能留下必须由用户核查的客户端临时项。

## 副作用、历史与恢复

`fileeffects.Effect` 的新操作 `copy`：必要 Source、Target 均为 file，Scope=entry，无 Directories 计划。Source.Version 保存冻结的 entry-v1；completed 的 Target.Version 为实际流式哈希，unknown 为空。`SamePlan` 允许发布后获知目标版本，但必须严格匹配源路径/源版本和目标路径。

copy 的 ReferencePath/Kind 为目标路径/`copy`。`Affects` 只影响目标、相关父目录/metadata/list/find/search 观察，不把源伪报为修改。copy 与既有 write/edit/archive/mkdir 操作事实不被后续观察覆盖；长历史回收和小窗口投影保留完整 Effect，不递归截短端点。结果、活动和 UI 使用独立 `DestinationPath`，不借用 `ArchivePath`。

Controller completed/unknown 回执仍要求稳定 turn、call ID、事件和工作区引用。copy 额外强制 checkpoint 的完整 Effect 存在并与 WAL 两端和源版本一致；completed 目标哈希还必须匹配引用。恢复只保留事实和失效观察，显示双端、源未由本操作修改、unknown 不重试；不会执行复制。测试包含真实复制后将目标由测试模拟用户删除，再恢复确认不会重新创建。

## 严格持久化兼容

- record payload **v4 → v5**，dirty payload **v3 → v4**。
- record/dirty 加密容器仍 v1，checkpoint 仍 v1；没有新增 checkpoint 字段。
- `agentsession/payload_copy.go` 冻结 record-v4、dirty-v3 的嵌套 endpoint/chain/effect/receipt DTO 及原有闭合操作集：write_create/write_replace/edit/archive/mkdir。先校验旧允许集及形状再 upcast，不允许旧 schema 夹带 copy。
- 已有 record-v1/v2/v3 和 dirty-v1/v2 继续使用冻结旧 DTO；拒绝把 copy 包装成旧操作格式，保留实际已记录/未记录的版本和创建信息。迁移最多四步，v1→v5；旧 dirty 读取不重写证据。
- 认证后的未来 payload 或容器版本仍返回 `ErrVersionUnsupported`，不降格为可清理损坏；没有开放任意 operation 来规避版本门禁。

## 已运行证据

1. 原语最小编译：`go test -p=1 -run '^$' ./internal/securefile` 通过；随后 `go test -p=1 -count=1 ./internal/securefile -run Copy` 通过。
2. 七包具名集合 `Copy|FileEffect|Archive|Mkdir|Payload|WorkspaceDefinitions|WorkspaceAllToolParsers|WorkspaceToolSchema|WorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls|CompactToolProse`：首轮仅 workspace 旧九工具 allowlist 失败（仍把 copy 列为禁用）。按新十工具合同更新该 fixture。后续修改输入涉及 securefile/workspace/agentloop/agentui，这四包同集合复核通过；fileeffects/agentsession/agentcontroller 首轮证据复用。
3. 七包整包串行一次：fileeffects、securefile、workspace、agentloop、agentsession、agentcontroller、agentui 全部通过。
4. Darwin/Windows amd64 `securefile` 仅 `go test -c` 交叉编译通过；产物目录 `/tmp/edu-agent-c7-compile.z0I5Lc/`。没有声称运行原生平台测试。
5. `git diff --check` 通过。父代理随后对七个受影响包执行 `go vet`，并在securefile/workspace/agentloop/agentcontroller执行 `-race -run Copy`，全部通过。没有运行全仓、数据库、Compose 或真实 provider；跨批候选验收尚未完成。

直接测试覆盖文本/二进制/>1 MiB/32 MiB/32 MiB+1/空文件/特殊权限剥离、非普通入口/链接/FIFO/归档及别名/缺父/目标冲突/旧版本、准备零副作用、源写入或替换、双方父替换/内外迁移/迁入归档、随机临时项替换、创建/写入/同步/关闭/rename 故障和取消分类；另覆盖默认确认/拒绝/YOLO/WAL 失败/晚取消/unknown、两端 UI 分页、目标观察失效、历史事实、严格迁移/未来版本、真实保存恢复无重放。

候选阶段已通过[文件效果有界日志](client-file-effect-journal.md)修复连续变更覆盖单一WAL的问题，最终record/dirty均v6、容器/checkpoint均v1。copy无目录复制、delete或归档恢复；C8的独立move见[设计](client-file-move.md)。mkdir也已在独立UI修复中复用完整预览分页。本次交付未stage/commit/push。
