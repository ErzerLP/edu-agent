# 文件检索增强设计（C5）

正式合同：[client-file-search-enhancement](../comet/specs/client-file-search-enhancement/spec.md)。已按结果呈现、模型glob、共享gitignore三个内部单元串行实现并验收。

## search 结果呈现

- `output=content`（缺省）保持既有逐匹配项`matches(path,line,column,preview)`；`context=0`不附加邻近行。`context=1..3`仅用于content，增加按path/line去重和排序的`context_lines`，UTF-8安全裁剪，所有窗口计入同一6KiB结果预算。
- `output=files`返回唯一、排序的匹配路径；不返回匹配正文，文件内找到第一处即可停止收集。`matched_files`是已发现数，截断时可大于实际列出的`returned`。
- `output=count`返回匹配行数`matched_lines`和文件数`matched_files`，同一行多个occurrence只计一行，不返回正文/路径列表。
- files/count的`counts_partial`明确表示局部结果，不是无限全仓统计。原100条收集上限分别按occurrence/文件/匹配行作用；原文件数、扫描字节、入口、深度及输出约束保留。内容模式的regexp lookahead按剩余容量+1收集，不生成无界匹配数组。
- 严格拒绝非法mode、显式null、context范围错误及files/count非零context。schema的条件约束与parser一致。

## 模型 glob

`glob`缺省匹配全部文件；使用[find](client-file-find.md)的256-byte路径模式和组件`**`语义。无斜杠匹配basename，有斜杠匹配工作区相对全路径，不因搜索scope变化而改基准。新glob与原include共同收窄，exclude优先；旧include/exclude不改成递归语法。显式文件仍遵守调用方指定的这些筛选。

## opt-in Git-ignore

find/search的`respect_gitignore`缺省false，与旧结果等价且不读取规则；true才在调用内加载工作区根到实际进入目录的`.gitignore`。没有外部祖先、全局git配置、git命令、索引或跨调用缓存。

专用Git规则层支持注释/空行、BOM/CRLF、未转义尾部空格处理、常见转义、basename、根锚定、目录规则、字符类、`*`/`?`/完整组件`**`、同层最后匹配和更深规则优先。完全排除的目录不入队，不能从不可达子树读取否定规则重新包含。显式目录不能绕过被排除祖先；显式普通文件绕过发现ignore，但不绕过调用方glob/include/exclude或任何安全权限。

隐藏文件没有额外排除。归档规则独立：普通scope跳过归档；显式归档目录只读且仍受ignore筛选，显式归档文件按显式文件规则只读。ignore不是权限/敏感文件denylist。

| 忽略读取边界 | 上限 |
| --- | --- |
| 单文件 | 64KiB，且不高于现有FileBytes |
| 调用内配置字节 | 256KiB |
| 存在并尝试读取的规则文件 | 256 |
| 接受的规则 | 4096 |
| 单规则模式 | 1024字节、64组件 |

配置与search正文共享原SearchFiles/SearchBytes预算，包含增长探针和失败读取的保守计费。find只增加明确配置读取，不读取候选正文。true时可见`ignore_files/ignore_bytes/ignored_entries`，不虚构未遍历子树数量。

链接/特殊规则入口、无法读取、无效文本、不支持语法或限额使受影响子树闭合并标为不完整；不会当空规则继续扩大搜索。已知可访问兄弟结果可保留。规则正文/原始OS错误/绝对根不进入结果。POSIX命名类/collating/equivalence类、悬空转义、无效路径组件等明确不支持，给出有界原因。

## 安全、投影与验证

Root.Stat先检查规则入口，ReadSnapshot执行root相对no-follow读取。Unix `openReadWithinRoot`仅在leaf增加O_NONBLOCK，避免Stat后替换为FIFO卡住open；不是所有OS I/O的硬实时取消保证，也未更改绝对路径配置读取等无关入口。

Loop的live/history/预算投影保留模式、必要计数和不完整性；再次裁剪不能冒充完整，files/count不能泄露正文。来源仍是workspace snapshot，checkpoint不新增字段/格式，不产生WAL或写授权。

C5三个单元的具名测试及securefile/workspace/agentloop整包通过；父代理vet与diff检查通过，4096原四call门禁保持。真实workspace+fake model覆盖注册、双工具ignore、无mutation WAL、history/Recall/checkpoint。FIFO测试覆盖Stat后替换与坏规则子树闭合。未运行真实provider、数据库/Compose、全仓race或原生多平台矩阵。
