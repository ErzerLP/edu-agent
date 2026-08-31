# Agent 会话上下文压缩优化设计

> 状态：已实现并通过本地候选门禁；用户可见合同已同步到 `docs/design/operator-interfaces.md` 与 `docs/comet/specs/learning-agent/spec.md`。
>
> 参考：[`pi-observational-memory` V3.0.4](https://github.com/elpapi42/pi-observational-memory/tree/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21)，基准提交 `ce9fc982b3a219a7839f07c9f4a3e054e81a2b21`。

## 1. 结论

当前 Go CLI Agent 的上下文机制应从“超过字节上限后静默保留最近完整轮次”升级为：

1. **请求预算器**：按模型上下文窗口、工具 Schema、系统提示、输出预留和安全余量计算真实可用消息预算。
2. **会话观察账本**：在每轮完成后异步把较早原始对话提炼为带来源的 Observation。
3. **稳定反思层**：只把真正需要跨多轮保留的会话决定、约束、纠正、完成结果和未解决事项提炼为 Reflection。
4. **确定性上下文投影**：发起模型请求时不重新总结整段历史，而是确定性组合系统提示、会话记忆、最近完整轮次和当前轮次。
5. **精确来源回查**：模型只能通过明确的记忆 ID 回查被压缩事实的会话来源，不提供模糊搜索或任意聊天浏览。
6. **显式降级**：后台整理失败时继续使用最近完整轮次，但必须在 TUI 中说明发生了上下文降级，不能静默遗忘。

第一版保持**仅当前进程内存有效**，退出 `edu-agent agent` 后清除；不新增聊天记录落盘。跨进程恢复需要单独 Shape，因为现有 Go CLI 合同禁止把知识正文、学习状态、答案和普通交互内容演变成第二份本地业务真值。

## 2. 为什么不直接复制参考实现

参考项目最值得复用的思想是：

- 在上下文真正溢出前，后台持续记录 Observation；
- 将事件型 Observation 与耐久型 Reflection 分层；
- 使用 append-only ledger、coverage watermark 和 tombstone；
- 压缩时只做确定性 fold/render，不在临界路径重新调用模型；
- 给每条压缩记忆来源 ID，并提供精确 `recall`；
- 删除只影响活跃记忆，原来源仍可回查；
- 区分当前可见投影和完整账本，并暴露 drift。

但是本项目不能直接复制以下行为：

1. **不能默认落盘保存完整聊天账本**：当前 CLI 正式合同明确限制本地持久化学习正文、状态和答案。
2. **不能把客户端摘要变成服务端权威状态**：Knowledge、Session、Route、Review 和长期偏好仍必须通过公开 HTTP 工具读取。
3. **不能自动把普通聊天升级为长期记忆**：只有用户明确要求、经过本地确认并由服务端 Memory admission 接纳的偏好才是长期记忆。
4. **不能把隐藏推理纳入 Observation 来源**：本项目只记录用户文本、最终助手文本和经过脱敏/投影的工具结果，不记录模型 chain-of-thought 或内部思考文本。
5. **不能以“被两个 Reflection 引用”作为强覆盖**：引用数量不等于语义完整。本设计使用明确的 `exact/partial` 覆盖边。
6. **不能继续使用 `字符数 / 4` 或 `ContextWindow * 3 bytes` 作为唯一预算**：中文、代码、JSON 和不同 provider 的误差过大，而且会漏算工具 Schema 与回答空间。
7. **不使用内容哈希截断成 12 个十六进制字符作为唯一 ID**：相同文本在不同时间可能是不同事件，短内容哈希也存在碰撞和错误去重风险。

## 3. 实现前基线

当前 `clients/cli-go/internal/agentloop/session.go`：

- `Session.messages` 在进程内保存完整消息；
- `contextMessages()` 使用 `ContextWindow * 3` 作为字节预算；
- 超限时以 `user` 开始的完整轮次为组，从最新轮次向前保留；
- 系统提示始终保留；
- 当前完整轮次仍无法放入时返回硬错误；
- 单个工具结果经过 `boundedValue()` 限制为约 8 KiB；
- 被裁掉的旧轮次没有摘要、来源引用或 TUI 提示；
- `Session.messages` 本身不会缩减，长会话内存和扫描成本持续增长；
- 模型请求没有解析 provider usage，也没有显式输出 token 上限；
- 工具定义和 JSON Schema 不计入上下文预算。

现有“按完整用户轮次裁剪”必须保留为最后降级路径，因为它能保证 assistant tool call 与 tool result 不被拆散。

## 4. 目标与非目标

### 4.1 目标

- 长对话多次压缩后仍保留用户目标、关键决定、纠正、完成结果和未解决事项。
- 压缩不阻塞正常聊天主路径；大多数模型整理工作在轮次完成后进行。
- 每条重要会话记忆都能追溯到当前会话中的来源条目。
- Knowledge、Learning、Review 和长期偏好仍以服务端为权威。
- 当前模型请求始终为回答预留空间，并计入工具 Schema。
- 工具调用协议始终完整，不产生孤立 tool result。
- 后台整理失败、来源不可用或预算降级对用户可见。
- 会话退出后清除所有会话记忆，不新增普通聊天持久化。

### 4.2 非目标

- 不实现跨进程聊天恢复。
- 不把会话 Observation 自动写入 Nocturne Memory。
- 不把会话摘要用作评分、Evidence、Mastery、Review 或 tutoring 状态输入。
- 不实现对普通聊天的语义搜索。
- 不保存或展示隐藏 chain-of-thought。
- 不引入第二套服务端 Knowledge、Learning 或 Memory 状态。
- 不在第一版增加独立“记忆模型”及第二份模型密钥配置。

## 5. 总体架构

```text
                    ┌─────────────────────────────┐
User / Assistant ──▶│ Session source ledger       │
Tool projection ───▶│ user/assistant/tool evidence│
                    └──────────────┬──────────────┘
                                   │ turn completed
                                   ▼
                    ┌─────────────────────────────┐
                    │ Background consolidator     │
                    │ 1. Observer                  │
                    │ 2. Reflector when due        │
                    │ 3. Deterministic pruning     │
                    └──────────────┬──────────────┘
                                   ▼
                    ┌─────────────────────────────┐
                    │ Session memory ledger        │
                    │ observations / reflections  │
                    │ supersession / tombstones   │
                    └──────────────┬──────────────┘
                                   │ before each model request
                                   ▼
                    ┌─────────────────────────────┐
                    │ Context planner              │
                    │ system + memory + recent raw│
                    │ + current turn + tools       │
                    └──────────────┬──────────────┘
                                   ▼
                              Model request
```

实现分为四条相互独立的逻辑路径：

- **Capture path**：同步记录本轮可用来源，不调用额外模型。
- **Consolidation path**：轮次结束后异步生成 Observation/Reflection。
- **Projection path**：请求前确定性生成上下文，不调用额外模型。
- **Recall path**：按明确 ID 读取被压缩记忆的会话来源。

## 6. 数据模型

### 6.1 SourceEntry

```go
type SourceEntry struct {
    ID              string
    TurnID          string
    Kind            SourceKind // user | assistant | tool
    CreatedAt       time.Time
    ModelMessage    modelclient.Message
    RecallText      string
    TokenEstimate   int
    Retention       RetentionClass
    ServerReference *ServerReference
}
```

规则：

- ID 使用会话内随机 96-bit opaque ID，并执行碰撞检查；不从正文直接派生。
- `ModelMessage` 只用于最近原文上下文。
- `RecallText` 是经过工具专属投影和秘密脱敏后的来源文本。
- 思考生命周期 Activity、工具参数和隐藏推理不进入 SourceEntry。
- 服务端工具结果记录 revision/generation/reference，避免只保存无版本正文。

### 6.2 Observation

```go
type Observation struct {
    ID             string
    Content        string
    CreatedAt      time.Time
    Relevance      Relevance // low | medium | high | critical
    Kind           ObservationKind
    SourceEntryIDs []string
    Authority      AuthorityClass
    Freshness      FreshnessClass
    TokenEstimate  int
}
```

建议 `ObservationKind`：

- `user_intent`
- `user_constraint`
- `correction`
- `decision`
- `completion`
- `open_question`
- `tool_snapshot`
- `failure`
- `preference_flow`

`AuthorityClass`：

- `session_statement`：用户或助手在当前会话中表达的事实。
- `server_snapshot`：来自服务端工具的历史快照，必须带 revision/generation。
- `server_reference`：只保存权威实体引用，不复制其当前状态声明。

### 6.3 Reflection

```go
type Reflection struct {
    ID          string
    Content     string
    Kind        ReflectionKind
    Support     []CoverageEdge
    Authority   AuthorityClass
    CreatedAt   time.Time
    TokenEstimate int
}

type CoverageEdge struct {
    ObservationID string
    Fidelity      CoverageFidelity // partial | exact
}
```

关键区别：

- 一个 `exact` 覆盖可以比多个 `partial` 覆盖更可信。
- 多个 `partial` 不能自动升级为 `exact`。
- Reflection 必须明确保留 Observation 的关键决定、约束、理由或完成语义，才能使用 `exact`。
- 服务端状态 Reflection 默认只能是 `server_reference` 或带版本的历史快照，不能声称仍是当前状态。

### 6.4 Supersession 与 Tombstone

```go
type Supersession struct {
    OlderObservationID string
    NewerObservationID string
    Reason             string
}

type ObservationTombstone struct {
    ObservationID string
    Reason        DropReason
    DroppedAt     time.Time
}
```

Tombstone 只移出活跃上下文；只要来源仍在内存账本中，就可以精确回查。

### 6.5 内存保留层级

第一版不能用“仅不落盘”代替内存上限，否则超长进程仍会无限增长。SourceEntry 分三层保留：

1. **Hot raw**：最近完整轮次，保留可直接发送的 `ModelMessage`。
2. **Warm evidence**：离开最近窗口后清除 `ModelMessage`，只保留有界、脱敏的 `RecallText`、来源类型、时间、turn ID、server reference 和摘要 hash。
3. **Metadata-only**：Warm evidence 达到总内存上限后，按最老且已被安全覆盖的来源开始回收正文，只保留 ID/hash/状态；Recall 返回 `source_unavailable`。

建议默认：

- 单个 `RecallText` 最大 16 KiB；
- 全 Session Warm evidence 最大 32 MiB；
- Hot raw 由上下文预算器按完整轮次控制；
- Observation/Reflection 活跃投影按 token pool 控制；
- 回收顺序不能越过当前轮次、结果未知的 preference 写入、未解决 blocker 或没有 `exact` 覆盖的关键 Observation 来源。

这样可以保证进程内存有界，同时对被回收的精确来源诚实报告不可用，而不是伪造“无限可回查”。

## 7. 请求预算器

### 7.1 预算必须包含

每次请求预算包含：

- system prompt；
- 会话记忆使用说明；
- Observation/Reflection 投影；
- 最近原始消息；
- 当前用户轮次及其工具调用链；
- 完整工具定义和 JSON Schema；
- Chat Completions 结构开销；
- 回答输出预留；
- provider/tokenizer 误差安全余量。

### 7.2 分配优先级

预算按以下优先级分配，不使用固定字节乘数：

1. system prompt 和安全规则；
2. 工具 Schema；
3. 当前完整用户轮次；
4. 输出预留；
5. 安全余量；
6. 最近完整原始轮次；
7. 会话 Reflection；
8. 活跃 Observation。

如果固定部分加当前轮次已经超过窗口，直接返回可诊断错误；不能通过删除系统规则或拆散当前工具链来继续。

### 7.3 默认比例

对默认 `32768` 上下文窗口：

- 输出预留：约 15%，最低 1024、最高 8192 tokens；
- 安全余量：约 5%；
- 会话记忆最大：约 20%，但不能挤掉当前轮次和最近两个完整轮次；
- 其余用于最近原始对话。

公式示意：

```text
usableInput = contextWindow
              - reservedOutput
              - safetyMargin
              - systemAndToolTokens

memoryCap = min(contextWindow * 0.20,
                usableInput - currentTurn - minimumRecentTurns)
```

### 7.4 Token 估算与校准

新增 `TokenEstimator` 接口：

```go
type TokenEstimator interface {
    EstimateText(string) int
    EstimateRequest(modelclient.Request) int
    ObserveActual(estimatedInput int, usage modelclient.Usage)
}
```

策略：

1. provider 返回 `usage.prompt_tokens` 时记录真实值；
2. 使用 EWMA 校准当前 provider/model 的估算比例；
3. 未获得 usage 前，对 ASCII、CJK、代码/JSON 使用保守混合估算；
4. provider/model 切换后清空校准基线；
5. usage 缺失或异常时继续使用保守估算，不把未知视为零。

`modelclient.Response` 应增加可选 `Usage`，并严格接受缺失 usage 的兼容 provider。

## 8. 工具结果的双投影

当前统一 8 KiB 截断应改为：

- **Live projection**：当前工具调用链中提供给模型的较完整结果。
- **History projection**：本轮结束后替换进最近历史的紧凑结构。
- **Recall projection**：按 ID 回查时提供的有界、脱敏来源。

示例：

### `search_knowledge`

历史只保留：

- `knowledge_revision_id`
- hit 的 path、node revision、slice hash
- 每个 hit 的短关键摘录
- degraded/truncated

完整 canonical slice 不应长期重复占用上下文。

### `get_learning_progress`

历史只保留：

- session ID/state/version
- goal/route/activity 等当前引用 ID
- allowed actions 摘要
- generation/revision

在后续需要当前状态时重新调用工具。

### `get_learning_route`

历史只保留 route/goal revision、offset/total、涉及的 step ordinal 与 node revision；教学意图只保留当前讨论需要的部分。

### `get_due_reviews`

历史只保留 review ID、node revision、due time、分页状态和数量。

### `list_long_term_preferences`

历史优先保存服务端 memory ID、revision、category 和 valid-until。正文只在最近上下文中使用；后续需要时重新查询。

### 总轮次预算

除单工具上限外，新增当前轮次累计工具结果预算。超过预算后，工具仍可返回紧凑的结构化结果，但不能继续把多个 8 KiB 原始结果累积到同一轮次。

## 9. Observer

Observer 在完整轮次结束后、Agent 空闲时异步运行。第一版复用当前 Agent 模型和现有模型凭据，不增加第二套模型配置；Observer/Reflector 使用独立的短请求预算，不继承正常 Agent 的服务端工具面。

### 9.1 触发

建议按上下文窗口比例派生：

```text
observeAfterTokens = clamp(contextWindow * 0.12, 2000, 8000)
```

触发条件：

- 自最新 observation coverage watermark 后的来源 token 达到阈值；
- 没有另一个 consolidation 在运行；
- Session 未关闭；
- 当前没有等待用户确认的 preference 写入结果未知状态。

### 9.2 输入

- 当前 Reflection；
- 当前活跃 Observation；
- 从 coverage watermark 后开始、最老优先的完整 SourceEntry；
- source ID 标签；
- 明确说明工具结果是历史快照、服务端状态可能已变化；
- 不含隐藏推理、模型密钥、设备令牌、工具原始参数。

单次 chunk 最大使用记忆模型上下文窗口的约 20%，且保留输出空间。第一条来源本身过大时使用明确标记的 head/tail 摘录，但原 RecallText 不改写。

### 9.3 输出与验证

Observer 只能调用内部 `record_session_observations` 工具，不能访问服务端写工具。单次 run 最多接受 128 条 Observation、最多 4 次内部 record tool call，并为结果设置不超过 2048 tokens 的输出预算；达到任一边界立即结束，不允许形成无界后台 agent loop。

代码端验证：

- source ID 必须来自本次允许集合；
- content 单行、长度有界、无终端/双向控制字符；
- relevance 和 kind 为封闭 enum；
- 不接受空来源；
- 不接受模型提供的 ID，由代码分配 opaque ID；
- 不接受把 tool snapshot 描述为当前权威状态；
- 不接受秘密样式内容。

Observer 返回空结果时不推进 coverage，并对相同区间启用 backoff；API/协议失败与“没有值得记录的内容”必须区分。

## 10. Reflector 与确定性裁剪

### 10.1 Reflector 触发

Reflector 不需要每轮运行。建议在以下任一条件满足时运行：

- 已完成至少两次新的 Observer pass；
- 活跃 Observation 超过上下文窗口约 10%；
- 预测下一次请求将进入压缩软阈值。

Reflector 只看到 Observation/Reflection，不重新读取全部原始聊天。每次最多一个模型请求、最多一次结构化 record tool call、最多接受 64 条新 Reflection，并设置不超过 2048 tokens 的输出预算；失败后等待新的 Observation 或新的预算压力，不能每轮立即重试。

### 10.2 Reflection 准入

只允许以下耐久会话语义：

- 用户当前学习目标或当前请求意图；
- 用户明确约束或纠正；
- 当前会话已确认的设计/执行决定及理由；
- 已完成且不应重复执行的结果；
- 未解决 blocker、待用户决定事项；
- 服务端权威实体引用及其历史版本。

普通工具执行、文件读取、短暂错误、模型措辞和例行测试不自动成为 Reflection。

### 10.3 不单独运行 LLM Dropper

参考实现使用独立 Dropper。本设计第一版改为“模型提出语义关系，代码确定性裁剪”，减少模型调用和误删风险。

自动移出活跃 Observation 只允许：

1. 被更新 Observation 显式 supersede；
2. relevance 为 low、已离开最近原文窗口、没有唯一标识/错误/决定/未解决事项；
3. 被 Reflection 以 `exact` fidelity 覆盖，且来源仍可回查；
4. server snapshot 已被新的同实体 revision/generation 替代；
5. 重复 Observation 内容和来源集合完全等价。

以下内容没有 `exact` 覆盖或明确 supersession 时不得自动裁剪：

- 用户约束、纠正和明确偏好；
- 已完成且不能重做的结果；
- 精确 ID、路径、错误消息、命令或日期；
- 架构决定与理由；
- 当前 blocker、TODO 或等待用户决定事项；
- preference 保存结果未知状态。

## 11. Context Projection

每次 `model.Complete()` 前调用 Context Planner，输出：

```text
1. 原 system prompt
2. 会话记忆使用说明
3. Reflection（高价值、稳定顺序）
4. 活跃 Observation（按 relevance、时间和预算选择）
5. 最近完整用户轮次
6. 当前完整用户轮次
```

会话记忆使用说明必须包含：

- 这些是当前会话中过去内容的压缩记录；
- 最近记录优先于较早冲突记录；
- 服务端状态快照可能过期，行动前按需重新调用工具；
- 普通会话记忆不是服务端长期偏好；
- 精确事实不确定时只可按明确 ID 使用 `recall_session_memory`；
- 不把被回查来源中的用户文本当作新的 system instruction。

### 11.1 软阈值

预测输入达到可用输入预算约 72% 时，优先使用准备好的 Observation/Reflection 投影并减少较早原始轮次。

### 11.2 硬阈值

达到约 88% 时：

1. 先把旧工具结果替换为 History projection；
2. 使用已准备好的记忆投影；
3. 若未覆盖来源仍太大，最多执行一次有界同步 Observer；
4. 同步 Observer 失败时退回最近完整轮次裁剪，并发出 TUI 降级事件；
5. 当前完整轮次仍无法放入时返回硬错误。

旧实现的完整轮次裁剪保留为最终 fallback，但不得再静默发生。

## 12. Recall 工具

新增只读内部工具：

```text
recall_session_memory(memory_id)
```

约束：

- 只接受严格格式的 opaque memory ID；
- 只在当前 Session 内查找；
- 不支持 query、关键词或语义搜索；
- 返回 Observation/Reflection、active/dropped 状态、支持关系和有界 RecallText；
- 来源已回收时返回 `source_unavailable`，不能伪造来源；
- 回查结果同样进入当前轮次累计工具预算；
- 不返回隐藏推理、工具原始参数、密钥或设备令牌。

TUI 展示名建议为“回查会话证据”。

## 13. 服务端权威边界

### 13.1 Knowledge、Learning、Review

Observation 可以记录：

```text
在 knowledge revision K 中检索到 node revision N 的片段。
```

不能记录成：

```text
当前知识库一定仍然是 K，当前学习路线一定仍然在第 3 步。
```

当用户问题依赖当前状态或要执行下一步动作时，模型应重新调用对应工具。

### 13.2 长期偏好

- 用户普通聊天中表达的偏好只能是 session observation。
- 用户明确要求长期保存时，继续使用现有 `remember_preference` 确认和服务端 admission 路径。
- 只有服务端成功接纳后，session reflection 才能记录 memory ID/revision 引用。
- Reflection 不能替代后续 `list_long_term_preferences` 的权威读取。
- 用户拒绝、保存失败或结果未知必须保留为明确状态，不能被自动解释为已保存。

### 13.3 Privacy / redaction

服务端返回 `content_redacted`、`privacy_clear_in_progress`、长期偏好的 learner/memory generation pair 变化或相关 degraded 状态时：

- 立即使对应 server snapshot Observation 失效；
- 后续上下文不再注入旧正文；
- 只保留最小本地说明“历史服务端内容已失效，需要重新读取”；
- 不从会话记忆恢复服务端已清除正文。

## 14. 并发与生命周期

`Session` 增加独立 context runtime：

```go
type ContextRuntime struct {
    mu               sync.Mutex
    ledger           SessionLedger
    consolidationRun *ConsolidationRun
    closed           bool
}
```

规则：

- 同时最多一个 consolidation；
- Worker 使用不可变 source snapshot 和 `coversUpToID`；
- append 前重新验证 source ID、Session 未关闭和 watermark 单调性；
- 用户下一轮可以在后台整理未完成时继续；Context Planner 使用最后一个已提交的安全投影；
- Agent 退出取消 worker，取消后不得 append；
- preference outcome unknown 状态保留现有幂等 ID，不被压缩流程清除；
- Context Planner 对 tool-call/result 组做原子选择。

## 15. TUI 设计

### 15.1 顶部与整体层级

顶部只保留一行产品标识，不再堆叠运行状态、模型和上下文指标。稳定信息向下靠近输入区，使阅读视线优先停留在 transcript：

```text
◇ edu-agent · AI 学习助手
────────────────────────────────────────
<conversation transcript>
```

### 15.2 消息编辑器

编辑器使用有边界的多行 composer，而不是裸 textarea：

```text
╭─ 消息 ─────────────────────── 18/8000 ─╮
│ › 请结合我的学习进度解释拓扑排序        │
│ › 并给一个小练习                        │
╰────────────────────────────────────────╯
```

要求：

- 空输入至少显示两行，按内容和软换行动态增长，第一版最多六行；
- `Enter` 发送，`Ctrl+J`/`Alt+Enter` 换行；
- busy、长期偏好确认和失败状态通过边框、标题和底部状态显示，不用弹出新的输入模式；
- 字符计数只在有输入时显示，保持 `8000` 字符硬上限可见；
- 宽度计算必须包含边框、padding、双宽字符和 ANSI 样式，窄终端不得横向溢出。

### 15.3 底部状态与帮助

原 header 中的状态、模型和上下文信息移动到编辑器下方。第一行是动态状态，第二行是键位帮助：

```text
● 就绪 · 上下文约 54% · 最近 6 轮 · 会话记忆 18 条 · 模型 deepseek-chat
Enter 发送 · Ctrl+J 换行 · PgUp/PgDn 滚动 · Ctrl+G 到底部 · Esc 退出
```

token 为估算时显示“约”，避免制造精确错觉。窄终端按“最近轮次 → 记忆条数 → 次要键位”的顺序降级，但必须保留当前状态、上下文百分比、模型和发送/退出提示。新消息提示属于动态状态行，不插入 transcript。

视觉原则参考 Crush 在固定 commit `7944b8e52225d8805e31eacbf7ef24856b0dfb7a` 的实现，但不复制其品牌或完整架构：采用最小 header、动态高度 textarea、语义颜色、底部 status/help 分层、响应式裁剪和有界右侧栏；本项目继续使用现有 Bubble Tea/viewport 架构，不引入 Ultraviolet 或 Crush 的多会话/文件侧栏状态机。

### 15.4 右侧学习状态栏

在内容宽度至少 `86` 列时，主区域拆为 transcript/composer 与右侧状态栏；更窄时状态栏完全折叠，底部 status/help 继续保留 Agent 状态、上下文、模型和关键操作，因此 `46×18` 的最低终端合同不变：

```text
<conversation transcript>                 ╭─ 学习概览 ─────────╮
                                          │ AGENT              │
╭─ 消息 ───────────────────────────────╮  │ ● 就绪             │
│ › 帮我继续当前学习任务               │  │                    │
╰──────────────────────────────────────╯  │ 当前学习           │
● 就绪 · 上下文约 54% · 模型 ...          │ 目标  图论基础       │
Enter 发送 · Ctrl+J 换行 · Esc 退出        │ 路线  ████░░ 2/3    │
                                          │ 活动  练习 · 难度 2 │
                                          ╰────────────────────╯
```

状态栏分为两个权威边界：

- `AGENT` 区只显示当前 TUI 运行状态、正在执行的可公开 activity 摘要、上下文估算和模型名，不显示隐藏推理、工具参数或凭据；
- `当前学习` 区由 `Session` 通过既有认证客户端直接读取服务端 `CurrentSession`，显示有界目标文本、会话状态、路线位置、当前 Activity 类型/难度和估算活跃时间；它不是模型回答、会话 Observation/Reflection 或工具结果缓存的派生副本；
- 启动、完整 Agent turn 结束和用户按 `Ctrl+R` 时刷新；读取失败只把侧栏标记为暂不可用，不阻断对话，不把旧数据冒充当前事实；404 是“尚无进行中的学习会话”的合法状态；
- 状态栏不显示 session、revision、node、device 等 opaque ID，不持久化快照，也不把展示快照重新注入模型上下文；所有服务端文本在布局前清理终端及双向控制字符；
- 侧栏宽度、高度、换行、进度条和文本行数都有硬边界；主 transcript 最少保留 `56` 列。宽度不足时不显示半截侧栏或用横向滚动代替折叠。

### 15.5 Transcript Activity

实际发生压缩时显示：

```text
◇ 上下文已整理
  已整理较早 9 轮 · 保留最近 6 轮 · 14 条观察 · 3 条反思
```

后台失败并进入 fallback 时显示：

```text
! 上下文整理降级
  暂时只保留最近完整轮次；本次不会写入长期偏好
```

routine Observer 成功不必每次生成聊天卡片；可以只更新底部状态行。失败、硬压缩和来源回查才进入 transcript。

### 15.5 隐私

TUI 只显示计数、预算、阶段和稳定错误码，不显示：

- Observer/Reflector prompt；
- 原始工具参数；
- 被压缩的聊天正文；
- 隐藏推理；
- 密钥和 bearer token。

## 16. 错误与降级语义

建议稳定错误/状态：

| Code | 含义 |
| --- | --- |
| `context_budget_invalid` | 固定 system/tools 或禁用压缩后的完整历史超过模型窗口 |
| `context_turn_too_large` | 当前完整轮次即使语义压缩后仍无法放入 |
| `context_recent_turns_too_large` | 当前轮次与最近两个完整轮次无法同时放入，拒绝静默丢弃最近上下文 |
| `context_observer_failed` | Observer 模型或协议失败 |
| `context_reflector_failed` | Reflector 失败，Observation 仍可继续使用 |
| `context_source_unavailable` | 指定记忆存在但来源已不可回查 |
| `context_compaction_degraded` | 退回最近完整轮次裁剪 |
| `context_usage_unknown` | provider 未返回 usage，使用保守估算 |

降级原则：

- Observer 失败不删除已有消息或记忆；
- Reflector 失败不回滚已提交 Observation；
- Recall 失败不修改上下文；
- 后台整理永远不能阻塞退出；
- 没有安全记忆投影时使用最近完整轮次，而不是空摘要。

## 17. 配置建议

第一版不暴露参考项目的大量高级阈值，避免用户需要理解多个池。

建议新增：

```json
{
  "agent": {
    "context_compaction": "auto"
  }
}
```

枚举：

- `auto`：默认，启用预算器、Observer/Reflector 和确定性压缩；
- `recent-only`：禁用模型整理，只使用语义工具投影和最近完整轮次；
- `off`：仅用于诊断，超过安全预算直接失败，不发送超限请求。

高级阈值先由 ContextWindow 比例派生，不进入用户配置。经过真实使用证明需要后再开放。

## 18. 建议代码边界

新增：

```text
clients/cli-go/internal/agentloop/context_types.go
clients/cli-go/internal/agentloop/context_budget.go
clients/cli-go/internal/agentloop/context_ledger.go
clients/cli-go/internal/agentloop/context_observer.go
clients/cli-go/internal/agentloop/context_reflector.go
clients/cli-go/internal/agentloop/context_projection.go
clients/cli-go/internal/agentloop/context_recall.go
clients/cli-go/internal/agentloop/tool_projection.go
```

修改：

```text
clients/cli-go/internal/agentloop/session.go
clients/cli-go/internal/agentloop/types.go
clients/cli-go/internal/agentloop/tools.go
clients/cli-go/internal/modelclient/types.go
clients/cli-go/internal/modelclient/client.go
clients/cli-go/internal/agentui/agentui.go
clients/cli-go/internal/agentui/transcript.go
clients/cli-go/internal/config/config.go
clients/cli-go/internal/command/agent.go
```

第一版不修改 server、数据库、OpenAPI 或 migration。

新增 recall 工具、上下文状态和错误语义属于用户可见行为；对应 Agent/operator interface 合同已在进入实现前同步，后续实现发现范围或可观察语义变化时必须返回 Shape 更新，不能只修改代码。

## 19. 垂直实施批次

### 批次 1：预算与语义工具投影

**结果**：模型请求会计入 system、tools、输出预留和安全余量；旧工具结果使用紧凑历史投影，超限不再依赖粗略字节乘数。

**退出标准**：

- provider usage 可选解析；
- 请求预算包含工具 Schema；
- 当前工具调用组保持原子；
- 当前轮次累计工具结果有上限；
- 预算不足返回稳定错误；
- 不新增持久化。

### 批次 2：会话 Observation / Reflection

**结果**：较早轮次在后台提炼为来源可追踪的会话记忆，请求前确定性注入记忆与最近原文。

**退出标准**：

- coverage watermark 单调；
- Observer/Reflector 输出严格验证；
- 无输出与失败区分；
- 没有独立 LLM Dropper；
- server snapshot 标记历史性；
- 普通聊天不会调用长期记忆写入。

### 批次 3：Recall、TUI 与降级闭环

**结果**：模型可按 ID 回查会话证据，用户可以看到上下文整理或降级状态。

**退出标准**：

- exact-ID recall；
- active/dropped/source-unavailable 状态；
- TUI header 和压缩事件；
- 退出取消后台 worker；
- 硬压力 fallback 不静默；
- 完整端到端长会话通过。

跨进程持久化不属于这三个批次，应作为单独 capability。

## 20. 核心验收场景

1. 20+ 轮对话超过原始上下文预算后，模型请求包含压缩记忆和最近完整轮次，并能正确引用较早的用户决定。
2. 多次压缩不会只对旧摘要再次摘要；新 Reflection 引用 Observation，Observation 引用 SourceEntry。
3. assistant tool call 和对应 tool result 永不被拆开。
4. 工具 Schema、系统提示和输出预留进入预算，发送请求不超过安全窗口。
5. 中文、英文、代码和 JSON 混合会话不会因 `bytes * constant` 明显低估。
6. Observer 模型失败时不推进 coverage、不删除原始消息，并在硬压力时显示降级。
7. Observer 返回伪造 source ID、非法 enum、多行内容或秘密样式内容时被拒绝。
8. Reflection 只有 `partial` 支持时，关键 Observation 不会因支持数量而自动删除。
9. 被 supersede 或 exact-covered 的旧 Observation 可移出活跃投影，但仍能按 ID 回查。
10. 来源已回收时 recall 明确返回 `source_unavailable`，不伪造证据。
11. Knowledge/Session/Route 历史快照不会被当成当前权威状态；需要行动时重新调用服务端工具。
12. 用户普通聊天中表达偏好不会自动持久化；`remember_preference` 仍必须显式确认。
13. preference 写入结果未知时，压缩不会丢失 operation ID 或允许取消。
14. `content_redacted` 或 generation 变化后，旧服务端正文不再从会话记忆回流。
15. TUI 不显示隐藏推理、Observer prompt、工具参数或秘密。
16. 退出 Agent 后，内存账本和后台 worker 被清理，不创建新的聊天历史文件。
17. SourceEntry 离开 Hot raw 后清除完整模型消息；Warm evidence 达到上限后只按安全顺序回收正文，进程内存不会随轮次无限增长。
18. 下一轮在后台 consolidation 运行时仍可继续，且只读取最后一个完整提交的投影。
19. 当前单轮本身过大时，先应用工具语义投影，仍过大才返回稳定硬错误。
20. 当前轮次与最近两个完整轮次在安全预算内无法同时容纳时，返回 `context_recent_turns_too_large`，不得静默发送更少的最近轮次。

## 21. 参考实现映射

| 参考项目机制 | 本设计处理 |
| --- | --- |
| Observation | 采用，并增加 kind、authority、freshness |
| Reflection | 采用，并增加 exact/partial coverage edge |
| Append-only ledger | 当前进程内采用，不落盘 |
| `coversUpToId` | 采用为单调 coverage watermark |
| Dropper model | 不采用；改为确定性硬规则裁剪 |
| Tombstone | 采用 |
| Model-free compaction render | 采用 |
| Exact-ID recall | 采用，限制在当前 Session |
| Visible/full drift | 简化为 header 中 prepared/current drift |
| Content-hash 12-char ID | 不采用；使用随机 opaque ID |
| char/4 token estimate | 不采用；使用 usage 校准估算器 |
| Raw thinking as source | 不采用 |
| Persistent session ledger | 第一版不采用，避免违反 CLI 持久化边界 |

## 22. 推荐决策

推荐按以下版本落地：

- **保留参考项目的“提前观察、分层记忆、来源回查、确定性投影”核心架构。**
- **不复制持久化聊天账本、隐藏推理采集、支持计数式覆盖和模型 Dropper。**
- **先交付进程内、服务端权威感知、预算正确的版本。**
- **把跨进程会话恢复作为单独 capability，届时必须同时设计加密、保留期限、显式清除、privacy generation 和跨平台原生证据。**

## 23. 参考资料

- [`pi-observational-memory` README](https://github.com/elpapi42/pi-observational-memory/blob/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21/README.md)
- [V3 技术流程与 ledger/projection/recall](https://github.com/elpapi42/pi-observational-memory/blob/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21/docs/how-it-works.md)
- [Observation、Reflection、Drop 与 folded details 类型](https://github.com/elpapi42/pi-observational-memory/blob/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21/src/session-ledger/types.ts)
- [后台 Observer/Reflector/Dropper 调度实现](https://github.com/elpapi42/pi-observational-memory/blob/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21/src/hooks/consolidation-trigger.ts)
- [确定性 summary renderer](https://github.com/elpapi42/pi-observational-memory/blob/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21/src/session-ledger/render-summary.ts)
- [Exact-ID recall 工具](https://github.com/elpapi42/pi-observational-memory/blob/ce9fc982b3a219a7839f07c9f4a3e054e81a2b21/src/tools/recall-observation.ts)
- [Crush UI architecture](https://github.com/charmbracelet/crush/blob/7944b8e52225d8805e31eacbf7ef24856b0dfb7a/internal/ui/AGENTS.md)
- [Crush dynamic editor and layout](https://github.com/charmbracelet/crush/blob/7944b8e52225d8805e31eacbf7ef24856b0dfb7a/internal/ui/model/ui.go)
- [Crush status/help bar](https://github.com/charmbracelet/crush/blob/7944b8e52225d8805e31eacbf7ef24856b0dfb7a/internal/ui/model/status.go)

## 24. 实现与候选证据

第一版已经按三个垂直批次落地：

- 请求预算器计入 system、工具 Schema、回答预留和安全余量，按语义投影旧工具结果，并把当前轮次与最近最多两个完整轮次视为不可静默丢弃的最小上下文；
- 进程内 Source/Observation/Reflection ledger、coverage watermark、确定性 pruning、Hot/Warm/metadata-only 回收和有界后台 consolidation 已接入 Session；
- `recall_session_memory` 只接受当前 Session 的 exact opaque ID，TUI header 和结构化卡片显示估算上下文、真实压缩、降级和来源不可用状态；
- `auto`、`recent-only`、`off` 可通过命令和交互式设置页配置；
- 长期偏好工具保留 learner/memory generation pair，redacted item、`content_redacted` 与 `privacy_clear_in_progress` 会使旧 preference snapshot 及其派生 assistant、Observation、Reflection 失效；
- 退出会取消并有界等待后台 worker，清空进程内 ledger，不创建聊天持久化文件。

稳定候选在 `clients/cli-go` 运行并通过：

```text
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./cmd/edu-agent
git diff --check
```

此外，20+ 轮场景、exact-ID recall、最近轮次强制预算、双 generation privacy fence、typed context error、TUI context cards 和 bounded Close 均有具名回归。第一版的已知边界仍是：记忆只在当前进程有效；provider usage 缺失时百分比为保守估算；Observer/Reflector 复用当前 Agent 模型但其输出必须通过本地严格验证，失败只触发可见 recent-turn fallback。
