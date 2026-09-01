# Agent 侧栏 Token 指标完整规格

## 产品目标

交互式 Go CLI Agent 在宽终端右侧栏中提供可读、诚实且有界的模型上下文指标。用户可以看到当前模型请求占用了多少上下文 token、配置的上下文窗口上限，以及当前 Agent Session 的累计提示缓存命中率，从而判断上下文压力与缓存是否持续生效。

该 capability 只负责当前进程内的 usage 解析、状态投影和 TUI 展示，不成为服务端遥测、计费或长期历史系统。

## 数据来源与权威边界

上下文窗口上限来自当前 Agent Session 已验证的 `ContextWindow` 配置。

当前上下文 token 在模型请求发出前来自 Context Planner 的 `EstimatedInput`。如果 provider 对该请求返回合法 `usage.prompt_tokens`，该实际值覆盖当前请求的估算展示；usage 缺失时继续保留保守估算，不能把未知值显示为零。

缓存命中 token 只来自 provider usage：

- OpenAI/OpenRouter 风格端点读取 `usage.prompt_tokens_details.cached_tokens`；
- DeepSeek 风格端点读取 `usage.prompt_cache_hit_tokens`；
- 其他端点只有在返回上述兼容字段时才提供缓存指标。

缓存命中率按当前 Agent Session 中所有明确报告受支持缓存字段的已完成模型请求累计计算：

```text
cache_hit_rate = sum(cache_read_tokens) / sum(prompt_tokens) * 100
```

每个纳入累计的 `prompt_tokens` 都是该请求的完整输入 token，总数包含缓存命中与未命中部分。没有报告缓存字段的请求不作为零命中加入分母，避免把“未知/不支持”误算成 cache miss。缓存命中率不跨进程、不跨 Agent Session，也不从价格、响应时间、重复文本或本地估算反推。

## 状态与更新语义

现有进程内 `ContextStatus` 同时携带：

- 当前上下文 token；
- context window token；
- 当前 token 是否仍为估算；
- 既有上下文百分比、最近完整轮次、会话记忆计数和压缩状态；
- 当前 Agent Session 已报告缓存明细请求的累计 prompt token；
- 当前 Agent Session 的累计缓存命中 token；
- 根据上述累计值计算的缓存命中率及其是否可用。

每次 Context Planner 完成计划后，状态先发布估算 token、窗口上限与按估算值计算的窗口百分比。每次前台模型响应通过协议校验并返回 usage 后，状态再使用实际 prompt token 更新当前 token 数值和窗口百分比；如果 usage 同时包含合法缓存明细，则把该请求的 prompt token 与 cache read token 加入 Session 累计值并重新计算缓存命中率。

工具调用导致同一用户 turn 内存在多次模型请求时，每个已完成且报告缓存明细的响应都进入 Session 累计。失败、取消、协议无效或尚未完成的响应不得改变累计值；未报告缓存字段的完整响应只更新当前上下文 token，不改变缓存累计。

状态更新沿用现有 ContextRuntime 锁和有界 ContextEvent 通道。该 capability 不创建后台 worker、不增加无界队列，也不持久化 usage。

## Provider usage 兼容性与校验

usage 的基本 token 计数必须保持非负。缓存 token 只有满足以下条件时才可用于展示：

- provider 明确返回受支持的缓存字段；
- `prompt_tokens > 0`；
- 缓存 token 非负；
- 缓存 token 不大于 `prompt_tokens`。

只有满足全部条件的请求才进入 Session 缓存累计。provider 明确报告零缓存 token 的合法请求进入累计分母，因此可以形成真实的 `0.0%`；字段完全缺失的请求不进入缓存累计。

如果 OpenAI 风格和 DeepSeek 风格字段同时存在，优先使用标准嵌套的 `prompt_tokens_details.cached_tokens`；实现必须保持确定性，并对冲突或非法值失败关闭或忽略缓存明细，不能产生超过 `100%` 的命中率。

流式 Chat Completions 的最终 usage chunk 与非流式响应使用同一 Usage 类型和校验语义。缺失 usage 仍是兼容行为，不得因此使原本可用的 provider 请求失败。

## TUI 展示

### 宽终端侧栏

当现有响应式布局启用右侧栏时，`AGENT` 区显示：

```text
上下文  约12.3k/32.8k
缓存命中 75.0%
```

规则：

- Context Planner 的估算值带“约”；provider 返回实际 prompt usage 后去掉“约”。
- token 使用紧凑单位：小值可显示整数，千级显示 `k`，必要时显示 `M`；数值不得因为格式化而突破侧栏宽度。
- 当前 token 和窗口上限始终使用 `当前/上限` 顺序。
- provider 尚未对当前 Session 的任何请求返回可识别缓存明细时，缓存命中显示不可用标记 `—`，不得显示误导性的 `0%`。
- provider 明确报告零缓存 token 且 prompt token 大于零时，该请求进入累计分母，缓存命中率可以显示真实 `0.0%`。
- 命中率显示一位小数并限定在 `0.0%` 到 `100.0%`；数值由当前 Session 的累计 cache read/prompt token 计算。

侧栏仍可显示公开运行状态、模型名、最近轮次和会话记忆计数；为满足现有最小高度，次要行可以沿用现有 compact 策略，但上下文 token 对是 `AGENT` 区核心信息。

### 窄终端与底部状态

终端宽度不足现有侧栏合同或主 transcript 会低于最小宽度时，侧栏继续完全折叠，不显示半截指标，不通过横向滚动补偿。

本 capability 不要求把缓存命中率复制到底部 footer。既有底部上下文百分比可继续保留，用于窄终端降级；宽侧栏则提供具体 token 数值。

### 隐私与安全

指标只包含数字、单位、估算标记和不可用状态。不得显示或记录：

- prompt、assistant 或工具正文；
- 工具参数和原始 provider 响应；
- 隐藏 reasoning；
- API key、设备 token 或其他凭据；
- session、turn、revision、node 或其他 opaque ID。

## 非目标

本 capability 不提供：

- 服务端或数据库 usage 存储；
- 跨进程、跨设备或跨会话统计；
- 美元成本、TPS、TTFT、累计输入/输出 token；
- cache miss 成本估算或缓存 TTL 诊断；
- provider 缓存开关或 prompt cache 控制；
- context window 自动修改；
- 对 provider 未报告字段的缓存命中率猜测。

## 验收合同

- A1: context window 为 `32768` 且当前计划估算输入约 `12340` tokens 时，宽侧栏显示类似 `上下文 约12.3k/32.8k`，不再只显示百分比。
- A2: provider 返回 `prompt_tokens=12000` 后，侧栏更新为实际 `12k/32.8k`，且窗口百分比按实际值同步更新。
- A3: Session 内两个已完成请求分别报告 `cached/prompt=9000/12000` 与 `3000/8000` 时，OpenAI/OpenRouter 或 DeepSeek 字段均产生累计缓存命中率 `60.0%`。
- A4: provider 尚未对任何请求返回受支持缓存字段时显示 `—` 而不是 `0%`；明确返回零命中的请求进入累计分母并可显示真实 `0.0%`。
- A5: 同一 turn 的多次模型请求逐次更新当前上下文 token；所有合法且报告缓存明细的完整响应累计更新 Session 命中率，失败、取消、协议无效或字段缺失的响应不污染累计。
- A6: 窄终端继续折叠侧栏，宽侧栏和最小高度布局不溢出，不泄漏正文、隐藏推理、凭据或 opaque ID。

## 验证策略

modelclient 使用 fake OpenAI-compatible HTTP/SSE server 覆盖嵌套 cached token、DeepSeek cache hit token、usage 缺失、零命中和非法计数。Agent loop 使用 fake model 覆盖计划估算、实际 usage 覆盖、Session 累计命中率、字段缺失不污染累计与 ContextEvent 更新。TUI fake conversation 覆盖 token 格式、估算标记、累计缓存命中率、不可用状态、compact 侧栏和窄终端折叠。

实现期间运行受影响 package 的具名测试和 package 测试；稳定批次运行受影响 package 的 vet、error-level diagnostics、CLI build 和 `git diff --check`。不需要 PostgreSQL、Compose、OpenAPI、数据库 migration、race 全仓或服务端黑盒证据，除非实现意外扩大到对应边界。
