# 文件大参数实现（C2）

正式合同：[client-file-large-arguments](../comet/specs/client-file-large-arguments/spec.md)。本批只扩大模型write/edit参数容量，不改变工作区安全、文件上限或授权发布。

## 单一预算策略

`internal/agentlimits/tool_arguments.go`：

- `ToolArgumentsBytes("write"|"edit") = 65536`；所有其他/未知工具为8192。
- `MaxToolCallArgumentsTotal = 131072`。
- 计量整个原始arguments JSON的UTF-8字节，含转义/路径/字段；恰好相等允许，超限拒绝。

Workspace schema中的content/old_text/new_text最大长度从共享65536上限生成。字段的JSON Schema字符长度只是附加提示，真正的整JSON字节门禁在Agent Loop实时校验和checkpoint验证；不把字符数当字节数。目录/工作区内可信执行器仍保留独立1MiB文件、32处编辑、6KiB预览等边界。

`validateModelMessage`和`validateCheckpointMessage`都按工具名核对单次预算并核对整个响应累计预算。原有UTF-8、tool identity、角色、协议、hash/路径及严格工具解析不变。模型transport已有128KiB参数delta和1MiB单调用累计上限足够，本批不扩大。

旧会话小参数仍可读取。没有新增checkpoint格式字段，不改变其schema；扩大后的长调用可保存/恢复且恢复不重放。历史完成工具参数可沿已有机制归一化，不修改待授权候选。

## 验证

- `TestLargeArgumentsToolPolicy`：仅write/edit获得65536，未知/不同大小写等保持8192。
- `TestLargeArgumentsLiveAndCheckpointBoundaries`：精确上限/+1字节、UTF-8及转义、累计131072及超限，实时/checkpoint一致。
- `TestLargeArgumentsAuthorizedHTTPWriteEditAndRestore`：fake HTTP普通/SSE，真实workspace >8KiB create和edit，批准前无副作用，批准后准确发布，checkpoint保存恢复不重放。使用recent-only隔离不相关后台Observer HTTP请求。
- 精确门禁通过：agentlimits、workspace、agentloop，包含既有4096四工具预算回归。
- 三包完整测试与vet通过。没有重新运行不相关C1矩阵、真实provider或平台宽检查。

没有实现分块写入、append、patch或超大文件；参数预算与128k模型输出额度是不同限制。
