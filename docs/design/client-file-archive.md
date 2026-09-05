# 工作区安全归档设计

正式用户结果见 `../comet/specs/client-file-archive/spec.md`。本次唯一结果是：Agent 对文件和整个目录的删除意图一律经专用归档工具处理，没有永久删除能力；归档由用户手动恢复或清理。

## 工具与协议

保留现有五个工具，新增 `archive({"path":"相对路径"})`，拒绝未知参数和工作区根。只有固定工作区成功打开时暴露。每次 Prepare 使用加密随机 ID 和 UTC 时间生成 `.edu-agent-archive/<timestamp>-<id>/<source>`，最终路径仍满足统一路径长度和深度预算。目的地不是模型参数。

MutationPresentation 增加归档目标及入口类型，以同一冻结候选驱动预览、提交和写前记录。archive 复用独立文件授权 pending/resolver：默认确认，YOLO 只跳过确认。预览只展示源、目标、类型及手工清理说明，不读取或复制正文。

## 底层安全 API

在 `internal/securefile/archive*.go` 增加：

```go
const ArchiveDirectory = ".edu-agent-archive"
type ArchiveEntry struct {
    Kind EntryType
    Size int64
    Identity string
    Version string
}
type ArchiveResult struct {
    Outcome PublishOutcome
    DirectoriesCreated bool
}
func (r *Root) CheckArchiveWritePath(ctx context.Context, relative string) error
func (r *Root) InspectArchiveSource(ctx context.Context, relative string) (ArchiveEntry, error)
func (r *Root) Archive(ctx context.Context, source, destination string, expected ArchiveEntry) (ArchiveResult, error)
```

ArchiveEntry.Version 是入口身份、类型、大小和修改时间等元数据的 opaque 版本，不是内容 SHA-256。普通文件和目录均不需要整文件/整子树读取；目录后代的外部写入不受该入口版本保护，不宣称树快照。

CheckArchiveWritePath 对归档根及后代返回专用保护错误，并防止 Windows 大小写/短文件名别名绕过。write/edit 在 Prepare 和 Commit 检查；archive 源使用同一保护。文本发布还设置 `PublishOptions.ProtectArchive=true`，在已打开的发布父目录上再次核验保护，避免仅检查路径字符串；该选项默认 false，已有配置、凭据、Session 的发布、Root.Delete 和临时文件清理不变。

安全移动以 root/parent handle 为边界；提交重新核验源入口类型和版本。Unix 使用同文件系统的 no-replace rename（Linux renameat2、macOS 相应 no-replace 原语），Windows 使用不覆盖的 handle-relative rename。未知平台/原语不可用时失败关闭，不采用普通覆盖 rename 或 copy+delete。新建归档目录仅在提交发生，拒绝归档根链接及别名逃逸。不删除失败时留下的空归档容器；有容器副作用时结果明确说明，不假报磁盘完全未变。

源为目录时整体移动，不递归处理子项或跟随目录内部链接。源路径本身不接受链接/设备等特殊条目。工作区根不可移动。每次目标前缀唯一且目的地不覆盖。

## workspace 接入

- `archive.go` 承担 Prepare/Commit 和归档结果，不将大分支继续堆入文本 mutation。
- `types.go` 增加 ToolArchive、prepared 的归档入口快照和展示信息。
- `definitions.go` 和 mutation 路由仅新增 archive，不增加 delete、cleanup、restore 或通用 move。
- `write/edit` 统一检查归档保护，保留其他 `.git/.comet/.env` 的原规则。
- search 从非归档范围开始时跳过归档根；显式搜索归档范围允许。list/read 保持只读可见性。
- 成功返回源、归档目标、入口类型、completed；未知结果保留两端路径并停止自动推进。归档后不返回伪造的源文件 content_hash。

## Agent 与恢复

修改必须覆盖工具投影、活动详情、授权 UI、取消后的确定性 fallback，以及 Session 写前记录和回执。归档源消失时使原路径/目录后代内容及相关列表、搜索证据失效，不能仅生成一个“新文件 hash”来代表目录归档。

写前记录保存源、固定归档目标和入口类型。持久化扩展兼容既有 Session：record payload 从 v2 升至 v3，dirty payload 从 v1 升至 v2，两类加密容器仍为 v1。冻结旧 DTO，读取 record v1/v2 和 dirty v1 后在内存迁移，旧格式不能夹带新归档字段；已认证的未来版本仍归类为不支持，不能误当可清理的损坏记录。归档字段不得让旧 write/edit receipt 验证失败。恢复未知操作只提示检查两端，不自动重放归档。已完成归档后的 Esc 或模型续答失败必须保留真实副作用回执。

workspace 证据使用 `archive_file` / `archive_directory` 区分归档操作事实与文件内容，持久化入口类型仍为 `file` / `directory`，两者不混用，也不伪造目录内容哈希。

## 实施与验证

1. 更新正式规格与本设计。
2. 底层 archive 和保护 API，局部 securefile 测试；不碰原有删除/凭据 API。
3. workspace 六工具路由及保护，文件/目录/二进制/碰撞/拒绝/取消测试。
4. Agent 授权、结果、上下文和 Session 恢复闭环。
5. 仅运行受影响 package 与窄集成测试，稳定后一次 vet/build 和定向 race；平台原生未执行时如实记录。

不修改服务端、数据库、依赖和锁文件，不自动提交/推送，不创建实际用户文件归档进行演示。测试数据全部位于临时目录。

### 本次验证结果与限制

- Linux：securefile、workspace、agentloop、agentsession、agentcontroller、agentui、command 的相关包测试分批通过。新增六工具后的 4096-token 四调用回归保持原预算，不放宽安全余量；授权提示仍保留 provider 数据流告知。
- 归档稳定边界执行一次 `go test -race -p=1 -count=1 ./internal/securefile ./internal/workspace ./internal/agentloop ./internal/agentcontroller -run 'Archive|MutationQueue|FileMutation'`，通过。
- securefile、workspace、agentloop、agentsession、agentcontroller、agentui 的 `go vet` 通过；Linux CLI 构建通过。
- macOS/Windows amd64：securefile 测试二进制和完整 CLI 交叉编译通过；没有原生归档运行证据。Windows 8.3 别名测试在原生短文件名不可用时会明确 skip，不能当作通过。
- 未运行服务端、PostgreSQL、Compose 或真实 provider/TUI 手工交互，不将这些未执行项声称为通过。
