# CLI Agent 大窗口与长输出设计（C1）

正式合同：[client-agent-large-context](../comet/specs/client-agent-large-context/spec.md)。仅覆盖 C1，不增加文件工具或工具参数容量。

## 配置和请求预算

- `agentlimits` 是 272000 总窗口、128000 最大输出、1 MiB 助手正文的常量来源。
- `AgentConfig.max_tokens` 默认 128000，合法显式值为 1..128000。旧 JSON 只有缺少该字段时在加载结果中补默认，不写回文件；显式0、null、负数和超过上限均拒绝，已有 `context_window`、模型和其他设置保留。内部Go零值构造仍允许缺省归一化，不把它当作配置文件显式0的授权。
- `edu-agent model set --max-tokens 64000` 可调低输出；显式 CLI 0、负数和大于 128000 拒绝。`model show` 与 Dashboard 参数表单显示该字段；新建和恢复都把当前配置传入 Loop。
- 请求不是能力协商：provider/model 名称不改，不测试真实 provider 是否支持容量；provider 的不支持/长度错误仍明确返回。

每次主请求满足：

```text
estimated_input + effective_max_tokens + ceil(context_window * 5%) <= context_window
```

默认短主请求（普通和 SSE）实际发送 128000，安全余量 13600，预留完整输出时输入预算 130400。标题维持 96；Observer/Reflector 维持最多 2048。

当配置输出加安全余量本身就占满窗口时，为小窗口采用 `min(configured_max_tokens, max(1024, ceil(window * 15%)))` 的初始输出目标，不再套用旧 8192 封顶；必要时继续收缩到 `min(512, configured_max_tokens)`，保留安全余量和已提交记忆的空间。4096 工作区继续使用最多 512 的输出覆盖值；用户配置小于 512 时不反向增大。本次预算调整不改持久配置。

## 小窗口工具说明投影（后续C4集成）

配置窗口不超过8192时，主请求只省略非约束工具description及schema说明；所有工具、字段、required、enum、数值/长度/pattern等约束保持不变，`ask_user_question`全部说明及额外终端显示宽度规则原样保留。`const`、`enum`、默认值等数据中的`description`键不是说明，不得被删除。系统规则仍明确权威、秘密和授权边界。默认272k保留完整说明；本机制不改配置、不隐藏工具、不削减安全余量或输出断言。

## 有界历史投影

1. 当前完整轮次不可裁剪；仅复用已有“完成工具调用参数归一化”为 `{}` 的内部规则。
2. 最近两轮原文优先，不再强制超过预算也保留原文。先收缩本次输出，仍无法放入时才投影较早助手正文。
3. 用户陈述、受保护/结果未知轮次以及工具 call/result 不因正文投影丢失。请求保留全部在场工具组；原始历史回收保留 write/edit/archive/remember_preference 等副作用轮次。
4. 投影只替换请求中的助手正文，不覆盖 Session messages、ledger 原文或 Transcript。投影含显式 `degraded`、`context_history_projected`、轮次序号、原文 SHA-256、原文字节数和最多 1024 字节节选；auto 模式关联同一轮次的 `src_` ID。recent-only 无 ledger 时不伪造可回查 ID。
5. 节选声明不是完整原文或用户授权，来源回查仍遵循已有 16 KiB 上限。剩余不可压缩内容仍超限则明确拒绝。
6. `context_compaction=off` 完整保留历史，只允许调整输出额度，不静默裁剪。
7. Runtime 发布可见降级状态，Controller 保存对应 context 事件，恢复后仍可显示。此路径修复了旧 Controller 把 context 事件错误标成 `presentation_only`、与严格 Transcript 校验冲突的问题；不放宽校验。

## 正文与传输配额

| 边界 | C1 配额/行为 |
| --- | --- |
| 普通/流式助手正文 | 1 MiB UTF-8 文本字节；不是 token 数承诺 |
| 普通 JSON 响应包 | 7 MiB，容纳正文 JSON 最坏 6 倍转义与有界元数据 |
| SSE 总响应 | 256 MiB，容纳细粒度 framing，仍有总量上限 |
| SSE 事件数 | `1 MiB + 4096`，允许正文逐字节事件 |
| SSE 行数 | 事件上限的 4 倍 |
| SSE 单行/单事件 | 原有 512 KiB |
| 单正文 delta | 原有 64 KiB |
| 连续空 delta | 最多 4096，避免无正文事件洪泛 |
| 空闲超时、协议验证、取消 | 沿用且有回归覆盖 |
| 隐藏推理 | 不展示、不写入 checkpoint/Transcript |

一般用户输入、工具参数、文件容量不扩大。非助手消息旧校验限额保留。

## Session 配额与兼容

- 助手源正文单独限制为 1 MiB；助手 Transcript 单条序列化限制为 `6 MiB + 16 KiB`。
- 助手行数上限为源字节上限 + 1，单行列数上限为源字节上限，避免合法字节范围内的一行或多行文本被旧 1024 行/4096 列误拒。显示层自行换行，不截断保存正文。
- Transcript 总序列化配额 16 MiB；checkpoint 总配额 24 MiB，覆盖原文、source 消息副本与 JSON 转义。
- 用户 Transcript、工具/notice 事件、一般输入与工具参数配额不变；Session 48 MiB 明文/64 MiB 密文、profile 1 GiB 配额不变。配额不足返回明确错误，不把截断内容当完整保存。
- 正文首尾空白保留；纯空白响应仍拒绝。终端安全和路径/控制字符校验不取消。
- 本批没有改变任何持久化 DTO 字段、加密容器格式或工具协议，因此不做无意义的 schema/upcaster 扩张：record v3、dirty v2、Transcript v1、checkpoint v1 均保持。调整的是运行时资源配额和既有字段的正确取值；旧 DTO 仍严格解码，未来版本 fail closed，已有 archive 兼容测试保留。配置新增可缺省字段，不给旧配置强制改版本。
- 恢复不重放文件操作，旧授权/YOLO 重置规则不变。

## 验证映射

新增测试均使用 `TestLargeContext` 或 `TestLargeOutput` 前缀：

| 正式验收 | 直接证据 |
| --- | --- |
| 1–2 默认/显式/旧配置 | config legacy 真文件不回写、command set/show/launch、Dashboard 表单与显示 |
| 3 实际 max_tokens 和预算 | Agent Loop→fake HTTP 普通/SSE、预算不变量、恢复当前配置 64000 |
| 4 最小窗口 | 原具名 `TestWorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls` |
| 5–6 正文和细粒度 SSE | >64 KiB 普通/SSE、8193 个正文事件、1 MiB 及越界、6 倍转义、空增量和 transport 边界 |
| 7 取消与副作用 | 长 SSE 中途取消、Esc/迟到事件、已完成文件发布及取消 checkpoint 既有回归纳入 C1 精确门禁 |
| 8 保存与恢复 | 1 MiB 多行加密 Session、>8 MiB checkpoint、首尾空白、单条/集合/Session 配额失败 |
| 9 带来源续聊 | 1 MiB 长答复恢复后再提问、来源 ID/哈希、显式降级、请求局部投影不修改保存原文 |
| 10 原子工具与内部小请求 | tool call/result、用户授权事实、protected/off、标题96、Observer/Reflector最多2048 |

仅 fake provider、临时工作区和现有依赖。未运行真实 provider、数据库、Compose、全平台、race 或性能压力矩阵；父代理负责最终集成复核/vet。已知 AIX `unix.Linkat` 诊断来自基线，不在 C1 改动范围。
