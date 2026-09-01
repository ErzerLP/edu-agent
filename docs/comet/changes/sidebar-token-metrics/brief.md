# Outcome

在宽终端的 Go CLI Agent 右侧栏中，用户可以直接看到当前模型上下文的具体 token 使用量与窗口上限，并看到当前 Agent Session 的累计缓存命中率，从而不再只能依赖百分比判断上下文压力，也能识别提示缓存是否持续生效。

# Scope

- 扩展 OpenAI-compatible Chat Completions usage 解析，读取 OpenAI/OpenRouter 常见的 `prompt_tokens_details.cached_tokens`，并兼容 DeepSeek 的 `prompt_cache_hit_tokens`。
- 将当前请求的估算输入 token、provider 返回的实际 prompt token、配置的 context window 和 Session 累计缓存命中率纳入现有进程内 `ContextStatus`/TUI 更新流。
- 在右侧栏 `AGENT` 区显示紧凑的 `当前上下文/窗口上限` token 数值，例如 `12.3k/32.8k`；估算值保留“约”语义，provider 返回实际 usage 后显示实际值。
- 在右侧栏显示当前 Agent Session 的累计缓存命中率；只累计明确报告缓存明细的已完成请求，provider 尚未报告缓存明细时显示 `—`，不伪造 `0%`。
- 补充 modelclient、agent loop 和 TUI 的精确回归测试，并保持现有窄终端折叠与宽度边界。

# Non-goals

- 不新增服务端 API、数据库、OpenAPI、migration 或持久化遥测。
- 不统计美元成本、TPS、累计输入/输出 token 或跨进程历史。
- 不改变上下文压缩、预算分配、工具调用或长期记忆语义。
- 不为未返回缓存 usage 的 provider 推测缓存命中率。
- 不把侧栏指标注入模型上下文或写入服务端。

# Acceptance examples

- A1: 当 context window 为 `32768`、当前请求估算输入为约 `12340` tokens 时，宽终端侧栏显示类似 `上下文 约12.3k/32.8k`，而不是只显示 `约38%`。
- A2: 当 provider 随已完成响应返回 `prompt_tokens=12000` 时，侧栏更新为实际 `12k/32.8k`，并同步更新既有上下文百分比计算。
- A3: 当 Session 内两个已完成请求分别报告 `cached/prompt=9000/12000` 与 `3000/8000` 时，侧栏显示累计缓存命中率 `60.0%`；OpenAI-compatible 的 `prompt_tokens_details.cached_tokens` 与 DeepSeek 的 `prompt_cache_hit_tokens` 使用同一累计口径。
- A4: 当 provider 尚未对任何请求返回可识别的缓存 token 明细时，侧栏显示 `缓存命中 —`，不显示误导性的 `0%`；明确报告零命中的请求则进入累计分母并可显示真实 `0.0%`。
- A5: 多轮工具调用中的每个已完成模型响应都可以更新指标；上下文 token 反映最近一次请求，缓存命中率反映当前 Session 内所有已报告缓存明细请求的累计结果。
- A6: 侧栏仍只在满足现有宽度合同的终端显示；窄终端继续完全折叠侧栏，所有渲染行不得超过终端宽度，也不得暴露 prompt 正文、工具参数、隐藏推理、凭据或 opaque ID。

# Constraints and invariants

- 缓存命中率固定按 `累计 cache_read_tokens / 累计 prompt_tokens * 100` 计算；只纳入 provider 明确报告受支持缓存字段的已完成请求，每个请求的 prompt token 已包含命中与未命中输入。
- 当前模型请求开始后先显示 Context Planner 的保守估算；provider 返回合法 usage 后，以实际 prompt token 覆盖该请求的估算展示。
- usage 缺失、缓存字段缺失、负数、缓存 token 大于 prompt token 或 prompt token 为零时，不生成缓存命中率。
- token 数值采用与当前安装 Pi footer 相同方向的紧凑单位（整数、`k`、必要时 `M`），保证侧栏宽度有界。
- 指标只存在于当前 Agent 进程内，沿用 ContextStatus 的并发锁与有界事件通道，不引入新的无界缓存或后台 worker。

# Decisions

- 缓存命中率采用当前 Agent Session 累计口径：对所有明确报告缓存明细的已完成请求累计 cache read 与 prompt token，避免最近单次请求造成大幅波动。
- 对 OpenAI/OpenRouter 优先读取 `prompt_tokens_details.cached_tokens`；对 DeepSeek 读取 `prompt_cache_hit_tokens`；两者都以 `prompt_tokens` 为分母。
- provider 未报告缓存明细时显示不可用状态，避免把“不支持或未知”误报为 `0%`。
- 本 change 保持单一 Native change：token 数量与缓存命中率共享同一 usage 解析、ContextStatus 和侧栏渲染路径，不能形成独立可发布结果，拆分会增加重复修改与验证成本。

# Open questions

- 无。

# Verification expectations

- modelclient 非流式与 SSE 流式测试覆盖 OpenAI `prompt_tokens_details.cached_tokens` 和 DeepSeek `prompt_cache_hit_tokens` 的解析及非法缓存计数拒绝。
- agent loop 具名测试覆盖估算 token 状态、实际 usage 覆盖、Session 累计缓存命中率计算和 ContextEvent 发布。
- agentui 具名测试覆盖宽侧栏 token 对、缓存命中率、不可用状态、最小高度与窄终端折叠。
- 受影响 package 通过 `go test -count=1 ./internal/modelclient ./internal/agentloop ./internal/agentui`、`go vet`、error-level diagnostics、`git diff --check` 与 CLI build；稳定候选再按 Runtime 要求执行更宽门禁。
