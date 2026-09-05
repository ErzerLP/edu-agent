# 工作区 find 设计（C4）

正式合同：[client-file-find](../comet/specs/client-file-find/spec.md)。已实现Go原生路径扫描，不调用fd/rg/git，不下载程序。

## 参数和匹配

`find({path:".", pattern:"src/**/*.go", type:"file", limit:200})`；type为file/directory/any，any只返回普通文件与目录，链接/特殊入口单独计入跳过统计。无斜杠模式匹配basename，有斜杠匹配工作区相对完整路径。独立组件`**`匹配零或多层，`*`/`?`/字符类沿用单组件Go path.Match。模式最长256字节，不接受绝对/父目录/空组件、控制字符或反斜杠；动态规划匹配避免重复`**`的指数回溯。

扫描不读取正文，包括无正文读取权限的大文件。结果按工作区相对路径排序，返回entries(path/type)、returned、visited_entries、scanned_directories、skipped及complete。普通范围排除归档，显式归档可只读发现；默认包括隐藏文件，不应用.gitignore。后者仅由C5的明确参数增加。

## 边界与集成

默认200结果、10000入口、64层、6KiB输出，复用2000条单目录扫描上限；工具取消/超时保持有效。被限制或不能检查的目录不能形成全量结论，提供truncation_reason及收窄提示，不制造续页游标。模型只能看到安全相对路径，查询不创建目录或触发授权。

引用为`find_result`和有界结果digest，属于workspace观察而非服务端事实。归档立即使受影响发现范围失效；后续mkdir/copy/move必须通过统一副作用失效机制接续这一约束。Loop已有注册、活动、live/history/预算投影和checkpoint支持，TUI显示“查找文件或目录”。

## 验证

Find具名测试覆盖递归/零层glob、隐藏/归档、普通大文件和无正文权限文件、链接、类型/预算/非法参数、取消、截断、引用失效以及真实workspace→fake model→checkpoint。workspace/agentloop/agentui整包与vet通过，原4096四工具回归通过；没有原生macOS/Windows运行、真实provider或宽平台矩阵。

小窗口曾因工具schema固定开销回归。最终修正不是删除工具/约束或修改预算断言，而是请求级非约束说明压缩：见[大窗口设计](client-agent-large-context.md)。
