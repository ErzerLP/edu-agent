# Outcome

交互式 Go Agent TUI 在模型分析、工具调用和工具结果后的继续生成阶段始终可观察、可停止、可恢复：用户可以停止当前普通 Agent turn、调整后续请求的推理强度、展开安全的执行详情，并能区分“仍在等待模型/网络”与真实失败，而不是把长耗时误判为卡死。

# Scope

- 为每个普通 Agent turn 建立独立取消生命周期；忙碌时按 `Esc` 停止当前模型请求、只读工具链和后续模型轮次，但不退出 TUI。
- 安全清理被停止 turn 的不完整模型消息、工具协议组和会话来源，隔离迟到事件，并在 worker 确认退出后恢复输入。
- 保留长期偏好写入的既有幂等与结果未知合同：一旦写入可能已提交，`Esc` 不伪装成取消，而是明确显示正在核对。
- 在 Agent TUI 中提供推理强度选择对话框，并把选定强度在明确请求边界传递到 OpenAI-compatible Chat Completions 请求；`auto` 不发送强度字段，不支持的显式档位返回稳定、可恢复的错误。
- 将现有工具详情扩展为安全的执行详情，显示客户端可证明的分析阶段、工具名称/状态、耗时、慢响应提示和稳定错误码，不显示隐藏 chain-of-thought、工具参数或供应商原始响应。
- 为模型请求、工具读取及工具结果后的继续分析增加有界阶段状态、经过时间和慢响应提示；长期偏好元数据读取使用可取消的有界执行并提供进度，降低串行 N+1 延迟造成的无反馈等待。
- 引入真正的 OpenAI-compatible SSE token/tool-call 流式响应；增量文本即时进入当前可见回答，工具调用 delta 在完整组装和严格校验后才执行，取消或协议失败不能把半截 assistant/tool-call 消息写入正式会话历史。
- 流式兼容按明确能力失败分级处理：端点只拒绝 `stream_options` 时最多重试一次不带该字段的 SSE，随后若明确拒绝 `stream` 才最多执行一次非流式降级；`length` 与 `content_filter` 终止必须保留可见草稿但返回稳定未完成错误，不得提交为完整回答。
- 推理强度采用“设置页保存新会话默认值 + 对话框仅覆盖当前会话”，并提供 `auto`、`none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max` 完整档位。
- 将现有可靠的 `Ctrl+O` 工具详情升级为统一执行详情入口；不使用传统终端无法可靠识别的 `Ctrl+0`。
- 新增模型可调用的只读本地工具 `ask_user_question`：每次提供一个问题、2–4 个带稳定 ID 的选项，并声明单选或多选；工具执行期间暂停 Agent Loop，等待用户回答后把结构化结果作为原 tool call 的 tool result 回传模型，再从原工具序列继续。
- 待回答时用 Codex 风格的独占底部选择面板替换普通 composer/footer；选项按 `1.`–`4.` 编号，`↑/↓` 与数字键移动焦点，单选 `Enter` 确认，多选 `Space` 勾选/取消且 `Enter` 提交，长文本在 `46×18` 最小终端内换行并保持当前项可见。
- 长期偏好确认复用同一选择器视觉与键盘状态机，但继续调用专用 `ResolvePreference`、稳定 operation ID 和 outcome-unknown/retry-only 路径；普通问询答案不能成为写授权。
- 更新本地模型设置、Agent Session、模型协议、TUI 状态/帮助、正式规格和局部测试。

# Non-goals

- 不展示、保存或注入模型隐藏推理原文、reasoning trace、Observer/Reflector prompt 或内部 chain-of-thought。
- 不新增跨进程聊天记录或第二份 Knowledge、Learning、Memory 权威状态。
- 不修改服务端数据库、OpenAPI、migration、长期记忆准入或教学状态机。
- 不承诺消除模型供应商、SSH、代理或网络本身的延迟；目标是让等待可见、可停止并受既有超时约束。
- 不把普通 Agent turn 的取消语义错误应用到可能已经提交的长期偏好写入。
- 不把通用问询答案解释为任何外部写入、删除、发布或长期记忆的持久授权。
- 首版不实现 Codex 的多问题切页、每题备注、全局备注或复杂 preview pane；一次工具调用只显示一个问题。
- 不把传统终端无法区分的 `Ctrl+0` 作为未经确认的唯一操作入口。

# Acceptance examples

- A1：普通 Agent turn 正在等待第一次或工具后的模型响应时，按 `Esc` 立即进入“正在停止”，取消请求但不退出 TUI；worker 退出后输入框恢复，用户可以继续下一轮。
- A2：停止 turn 后不显示普通“请求失败”，而是保留用户问题和有界“本轮已停止”记录；未完成的 assistant/tool-call 协议和会话来源不会进入下一次模型请求。
- A3：取消后旧 turn 的迟到 activity、结果或错误不能污染新 turn，满 activity channel 或慢 provider 也不能令 worker 永久阻塞。
- A4：取消会传播到模型 HTTP、正在执行的只读服务端工具和剩余工具列表；取消后不再发起后续模型轮次。
- A5：长期偏好确认仍可用 `Esc` 拒绝；长期偏好写入开始后若结果可能未知，`Esc` 不声称取消，幂等 operation ID 和核对路径保持有效。若 create 已产生 `pending_review` 而 admit 确定性失败，必须用独立稳定 operation ID 补偿 reject；只有确认 rejected 后才恢复本地撤销项，补偿未知时保持 retry-only。
- A6：TUI 可用 `F3` 打开推理强度对话框，查看当前值、选择合法档位并取消或应用；切换不清空当前会话、工具结果或上下文记忆。
- A7：`auto` 保持旧配置兼容且不序列化推理强度字段；显式档位在下一次适用模型请求中发送，不支持时返回稳定错误并提示切回自动，不静默降级。
- A8：设置页保存以后新 Agent Session 的默认推理强度；对话框只覆盖当前会话，状态栏显示当前值及待生效值，临时切换不改写持久化默认值。
- A9：执行详情展开后只显示客户端可证明的阶段、状态、耗时、工具显示名和稳定错误码；收起后保留简洁摘要，任何状态都不显示隐藏推理、工具参数、凭据或原始供应商正文。
- A10：`Ctrl+O` 在 Bubble Tea 支持的普通本地终端和 SSH 字节流中统一展开/收起思考阶段与工具详情；帮助栏和 transcript 提示与实际按键一致。
- A11：工具成功后进入下一次模型请求时，TUI 显示“准备上下文/等待模型/校验响应”等真实阶段、已等待时间和慢响应提示；静态画面不再是唯一反馈。
- A12：长期偏好读取的元数据步骤有有界进度和取消传播，并避免最多二十次无反馈串行请求；输出顺序、隐私过滤和 generation/revision 语义不变。
- A13：用户报告的“长期记忆工具成功后继续思考”场景由 fake model 精确回归覆盖：第二次模型请求可持续显示进度、可被 `Esc` 停止且不退出 TUI。
- A14：真正的 SSE 增量文本与跨 chunk 工具调用被严格组装、边界校验并可取消；只拒绝 `stream_options` 时最多重试一次无该字段的 SSE，随后明确拒绝 `stream` 才最多一次非流式降级；`length`/`content_filter` 和停止后的可见半截回答标记为未完成或已停止，但不会作为完整 assistant 消息进入会话历史。
- A15：窄终端、暂停滚动、偏好确认、上下文压缩卡片、右侧状态栏和现有显式 CLI 子命令行为不回归。
- A16：模型调用 `ask_user_question` 后，Agent 停止后续工具和模型请求，TUI 状态明确变成“等待你的选择”；回答绑定原 tool call ID 作为结构化 tool result，随后只继续未执行的工具序列。
- A17：单选面板显示 2–4 个编号选项；默认高亮不等于提交，`↑/↓` 或数字键移动焦点，`Enter` 只提交当前项，重复 Enter 不能完成两次。
- A18：多选面板使用复选状态；`Space` 勾选/取消，`Enter` 在满足至少一项后提交稳定顺序的所选 option IDs，不能把 label 文案当作业务主键。
- A19：选择面板独占 composer/footer 的输入焦点，问题、标签和说明按显示宽度换行；`PgUp/PgDn` 仍可查看 transcript，resize 后焦点和选择不丢失，最小终端不溢出。
- A20：通用问询固定增加“自定义输入”；单选 custom 替代普通选项，多选 custom 可与普通项并存。`Esc` 返回 `cancelled` tool result 让 Agent 继续；工具结果必须区分 answered、cancelled 和 unavailable，不能把 UI 加载失败解释为用户拒绝。
- A21：长期偏好确认改用同一选择器 UI并提供“保存为长期记忆 / 仅本次会话 / 不采用”三项；保存进入既有专用写 resolver，另两项不产生服务端写入。写入开始后的 outcome-unknown 只显示原 ID 重试/核对，不显示可撤销写入的假选项。
- A22：问询参数在显示前强制校验问题、2–4 个唯一 option ID、单/多选模式、UTF-8、长度和控制字符；同一用户 turn 同时最多一个 pending interaction，累计问询次数有界，问题不得索取密码、API key、token、私钥或恢复码。

# Constraints and invariants

- 当前实现固定 `stream=false`；本 change 将新增真正 SSE 流式协议，同时保持端点不支持、流中断和解析失败时明确报错，不静默把未完成流当作完整回答。
- `Session` 目前不是并发 turn 安全的；按 `Esc` 后必须等旧 worker 确认退出才重新开放发送，或先完成等价的严格 turn 隔离。
- 当前完整工具调用组保持原子；停止或失败不能留下孤立 assistant tool call 或 tool result。
- 模型和工具取消必须保留 `context.Canceled`，不得被改写成普通工具失败后继续运行。
- 推理强度支持与具体模型相关；客户端不能根据 provider 名称假装当前模型一定支持，也不能把普通 HTTP 400 全部映射成不支持。
- `Ctrl+0` 在传统 C0 终端编码和当前 Bubble Tea 版本中没有可靠独立事件，SSH/tmux 可能进一步丢失或转换该组合键。
- 所有阶段文本、模型错误和工具摘要继续经过终端/双向控制字符清理和长度限制。
- `ask_user_question` 是只读本地交互工具；回答属于当前 Session 的用户决定，不是服务端快照，不自动写入 Nocturne，也不构成其他工具的授权。
- 用户交互工具采用独占执行；问卷激活后不能并行弹出第二个面板，也不能让同一 assistant 响应中的后续工具越过等待点执行。
- 选择器与长期偏好确认可以共享纯 UI reducer、布局和键盘处理，但通用 `ResolveQuestion` 与写安全相关的 `ResolvePreference`/retry resolver 必须保持类型分离。

# Decisions

- 使用 Comet Native change `tui-interaction-controls`，在用户选择的当前 `main` 工作区推进。
- 创建 change 前发现的两处 Admin UI 未提交修改只是回退上一提交的严格模式和占位符清理；经用户确认已恢复，不纳入本 change。
- 代码调查和聚焦 race 测试未发现长期记忆工具返回后必然发生 channel/锁死锁；高置信根因是工具后的第二次非流式模型请求没有心跳、耗时或取消入口，远程 SSH/网络延迟会放大这种“卡住”感知。
- 本 change 保持单一 change：四项需求共享 `agentui`、`agentloop`、`modelclient`、配置和同一 turn 状态机，拆分会重复修改相同公共接口和测试夹具，协调风险高于并行收益。
- 展开的“思考情况”只指高层执行生命周期，不包括隐藏推理原文。
- 普通 turn 与可能产生远端写入的长期偏好提交采用不同取消语义。
- 用户确认本 change 实现真正的 OpenAI-compatible SSE token/tool-call 流式响应，而不只为现有非流式请求增加心跳。
- 用户确认推理强度采用“当前会话覆盖 + 设置页持久化新会话默认值”；对话框切换不直接改写默认配置。对话框使用不与 textarea 编辑键冲突、普通终端可识别的 `F3` 打开。
- 用户确认首版提供 `auto`、`none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max` 完整档位；`auto` 省略协议字段，模型不支持显式值时明确失败。
- 用户确认把现有 `Ctrl+O` 升级为统一执行详情开关，同时展开高层思考阶段和工具状态；不以 `Ctrl+0` 建立合同。
- Pi 和 Codex 的一手实现均采用“模型工具调用 → 阻塞式本地选择 UI → 结构化 tool result → 原 Agent Loop 续跑”；本 change 采用同一协议骨架，而不是把选择伪装成新的普通 user message。
- Codex `0.150.1` 只有单选；本 change 按用户明确要求扩展真正多选，使用 `Space` 勾选、`Enter` 提交，并以稳定 option ID 返回结构化数组。
- 首版一次 `ask_user_question` 只承载一个问题和 2–4 个选项；不复制 Pi 的多问题 Tab、备注和 preview 全套，以保持当前交互 change 有界。
- 长期偏好只复用选择器展示；普通问询的 option ID 即使名为 `save`/`yes` 也不得进入长期记忆写 resolver。
- 用户确认通用问询固定追加“自定义输入”；自定义文本有界、可多行，作为结构化 custom answer 返回，不自动成为长期记忆。
- 用户确认问询面板中的 `Esc` 只取消当前问题并返回 `cancelled` tool result，让 Agent 继续；`Ctrl+C` 才中止整个 turn。
- 用户确认长期偏好首版使用“保存为长期记忆 / 仅本次会话 / 不采用”三项；后两项不调用服务端写接口，写入开始后的结果未知状态不再提供撤销项。

# Open questions

- 无。用户已确认完整目标、范围、关键决定、验收项和非目标。

# Verification expectations

- L1 精确覆盖：模型请求中取消、工具执行中取消、工具后第二次模型请求取消、turn rollback、最终回答提交与取消的线性化边界、迟到事件隔离、activity channel 关闭、长期偏好结果未知与 pending candidate 补偿拒绝边界、推理字段序列化/不支持错误、详情折叠、阶段心跳、长期偏好读取进度，以及通用问询的 schema、单选、多选、取消、续跑和 resolver 隔离。
- L2 覆盖 `internal/modelclient`、`internal/agentloop`、`internal/agentui`、`internal/config`、`internal/command`、`internal/dashboard` 的受影响测试与 vet。
- L4 对修改过的 turn goroutine、activity channel、取消传播、pending interaction、Session 状态和有界元数据并发运行定向 race；构建真实 `edu-agent` 并在宽/窄伪终端场景检查按键提示、选择器焦点和行宽。
- 若引入 SSE，增加 fake provider 契约矩阵：UTF-8 与 JSON 跨 chunk、`[DONE]`、工具参数 delta、错误帧、EOF、慢 body、取消、响应上限、`stream_options` 单次移除重试、`stream` 单次非流式降级、`length`/`content_filter` 未完成状态和非流式兼容策略。
- 候选阶段运行完整 CLI Go 测试、race、vet、build 和 `git diff --check`；真实外部 provider 只在不暴露凭据且不会把未确认兼容性表述为通过的边界内做窄验证。
