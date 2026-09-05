# 工作区 stat 设计（C3）

正式合同：[client-file-stat](../comet/specs/client-file-stat/spec.md)。已实现于Go CLI；不读取或修改服务端数据。

## 入口与证据

`stat({path, hash:false})`接受`.`和显式归档范围。返回`exists`；存在时返回`entry_type`、RFC3339Nano `mtime`与opaque `entry_version`，普通文件另有`size`。缺失与权限/路径错误区分。目录不递归计量；默认不读取任何正文，链接/特殊入口只报告类型。

底层`securefile.Root.Stat`返回`EntryInfo`，入口版本与归档的`entry-v1`元数据版本一致，供后续copy/move使用；模型不可见原始Identity或绝对根。`Root.HashEntry`只为普通文件有界流式计算原始字节SHA256，前后核对元数据与路径身份、检查取消；Unix使用非阻塞/no-follow打开，不能被换成FIFO后永久阻塞。workspace的hash上限仍为1MiB，大二进制文件可查看元数据但不能突破hash预算。

所有stat引用使用`entry_metadata`，其证据digest是完整返回投影的SHA256，即使`hash=true`也不混用raw文件hash或`entry_version`。同路径/受影响父目录的元数据会随相关文件变化失效；归档影响原入口/子树/父范围。Session沿用已有引用格式，不升级schema，也不重放观察或副作用。

## 生产集成和验证

workspace schema/executor、Loop read-only调度/预算投影/活动、TUI标签与checkpoint均已接通，不请求写授权。Linux具名Stat测试、securefile/workspace/agentloop/agentui受影响包测试及vet通过；4096四工具回归保持，后续小窗口说明投影见[大窗口设计](client-agent-large-context.md)。底层Darwin/Windows amd64测试二进制只交叉编译，未进行原生运行。未测试真实provider、数据库或Compose。
