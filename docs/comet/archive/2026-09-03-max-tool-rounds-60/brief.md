# 目标

修复 Go 客户端 AI 助手设置中的工具轮数边界错误，使用户可以在中文主控制台或等价 `model set` 命令中把“最大工具轮数”设置为 60，并让新启动或恢复的 Agent Session 按确认后的同一预算语义运行，不再返回 `invalid_configuration`。

# 范围

- 调整 `clients/cli-go` 中本地 Agent 配置、命令入口和 Agent Loop 对最大工具轮数的边界定义，消除当前两处独立硬编码 `1..16` 带来的保存与启动拒绝。
- 让中文设置页明确显示合法范围，并保持设置页继续通过现有 `model set` 命令和严格配置保存路径生效。
- 补充配置校验、命令保存、Agent Loop 构造和必要的设置页映射回归测试。
- 只影响新提交的模型配置和后续 Agent turn；不改写现有会话正文、会话密钥、自动加密保存记录或自动标题摘要流程。

# 非目标

- 不修改服务端 API、PostgreSQL、OpenAPI、Nocturne、设备配对或模型 provider 协议。
- 不改变默认值 8，不自动把用户当前配置从 6 改成 60。
- 不解密、迁移或重写现有 Agent Session；用户提供的会话 `eb860910-b2a0-45d4-81f8-be85bb06bebf` 只用于确认问题发生场景。
- 不取消每次模型响应、工具参数、工具输出、上下文、超时、取消和文件授权等现有独立安全边界。

# 验收示例

- A1：在隔离配置目录中选择任一合法模型预设后，执行 `edu-agent model set --max-tool-rounds 60` 成功，配置文件持久化 `max_tool_rounds: 60`，不再产生 `error[invalid_configuration]`。
- A2：中文设置页提交最大工具轮数 60 时生成等价命令并成功保存；界面明确提示合法范围，避免用户只能从通用错误中猜测边界。
- A3：使用 `MaxToolRounds: 60` 创建 Agent Loop 成功；单次用户 turn 的模型轮次与工具调用预算按用户确认的方案保持一致且仍有确定上界。
- A4：0、负数和超过确认上界的值仍被配置层与运行时层拒绝；失败不会覆盖上一次有效配置。
- A5：默认最大工具轮数仍为 8，原有 1..16 配置继续有效；单响应最多 4 个工具调用、参数/输出/上下文限制、超时、取消和写工具授权保持不变。
- A6：修改不触及 Agent Session 加密存储、自动标题、provider 摘要发送或会话恢复格式；现有加密会话保持可读和可恢复。

# 约束与不变量

配置验证和 Agent Loop 运行时验证必须共享同一最大值来源，不能再次出现设置可保存但会话无法启动，或界面宣称可用但运行时提前截断的漂移。工具轮数与单响应工具调用数仍为不同边界；总预算必须有限、可测试并与用户可见设置语义一致。配置保存继续使用严格 JSON、原子替换和现有权限保护；模型与设备凭据不得进入配置、日志或测试输出。

# 决策

- 根因已确认：设置页把输入原样传给 `model set --max-tool-rounds`；`AgentConfig.Validate` 在保存前拒绝大于 16，`agentloop.New` 又独立拒绝大于 16，因此值 60 无法保存，即使只绕过第一处也无法启动 Session。
- 当前用户配置仍保持 `max_tool_rounds: 6`，失败发生在候选配置校验前，不是会话加密、自动标题或服务端故障。
- 本需求保持单一 Native change：配置、命令、设置页和 Agent Loop 是同一垂直用户结果，拆分会造成共享边界短暂不一致。
- 最大合法设置值固定为 60；用户确认单次用户 turn 的总工具调用预算随轮数扩展为 `MaxToolRounds × 4`，因此设置 60 时最多 240 次调用。每个模型响应仍最多 4 次，其他超时、上下文、取消、输出和授权边界保持不变。

# 待解决问题

无。

# 验证预期

先运行配置、命令和 Agent Loop 的精确具名测试，证明 60 从设置输入贯通到运行时且 61/0 被拒绝；精确测试通过后运行 `./internal/config`、`./internal/command`、`./internal/dashboard` 和 `./internal/agentloop` 受影响 package。稳定候选运行这些 package 的 vet、CLI build、error-level diagnostics 和 `git diff --check`。本 change 不需要 PostgreSQL、Compose、OpenAPI、数据库 migration、服务端黑盒或全仓 race；若实现意外触及对应边界，再按项目测试策略升级验证。
