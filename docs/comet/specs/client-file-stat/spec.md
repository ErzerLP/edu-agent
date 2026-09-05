# Capability: 文件元数据 stat

结果：Agent 能不读取正文检查工作区入口是否存在、类型及版本，为安全 copy/move 提供前提。

## 合同与验收

1. 接口仅接受 path 和可选 hash（默认false），路径工作区相对，允许根 `.`；未知字段拒绝。
2. 不存在返回 exists=false；权限错误、不安全路径、无法验证等不能伪装为不存在。
3. 返回 exists、entry_type、普通文件size、mtime及opaque entry_version；目录不递归计算总大小。
4. entry_version 是入口元数据版本，不是 content_hash，也不承诺子树内容快照。
5. 默认不读正文，包括大二进制文件和目录；检查FIFO/设备等不能阻塞。
6. 链接/特殊入口可只报告类型，但不解引用，不因此赋予读取/修改能力。
7. hash=true仅普通文件按现有1MiB预算读取并计算raw bytes SHA256；二进制无需返回正文，超限明确失败，不以元数据hash兜底。
8. 根逃逸、链接父目录、junction/reparse/ADS与Windows路径别名适用现有安全政策。
9. 允许显式检查归档；stat不创建目录、不请求写授权；输出有界并标识为工作区观察事实。
10. 元数据/内容变化后证据可正确失效；集成覆盖工具注册、执行、投影、UI名称与checkpoint。

延期：目录大小递归汇总、MIME/内容解析、hash超大文件、外部路径。优先在securefile提取可复用安全入口检查，不能直接复用带归档来源禁令的InspectArchiveSource作全部stat语义。
