# 评估反馈、证据复核与继承审批（Issue #31）

## 缺失确认与架构

基线 `0667c7d`，工作树初始干净。下列行号均指实施前基线。
项目为 Go 模块化服务、PostgreSQL 权威记录、React Web 与独立 Go CLI。
learning 拥有答案、模型评估、处置事件和 Evidence；tutoring 拥有会话焦点；
learningcontent 拥有加密不可变正文；knowledge 拥有版本化 context、来源与继承映射；
identity 和 privacy 分别提供权限及跨 owner 的响应、清除屏障。

实施前确认：

- `clients/web/src/teaching-page.tsx:810–841` 仅显示当前反馈，明确要求去 CLI 复核。
- `server/internal/transport/httpapi/api.go:364` 只有在线处置 POST；详情与列表只存在于 offline API，不能读取在线答案。
- `server/internal/learning/command.go:641–659` 将处置限制在当前 Feedback，继续学习后旧评估无法纠错。
- `server/internal/learningcontent/store.go:364–380` 在答案事务校验内容版本，但未持久化答案与该版本的关联。
- `server/internal/learning/postgresstore/records.go:71` 已冻结活动 context，旧 DTO 未公开它。
- 已运行原有评估接纳、客观规则及处置单测，均通过。缺失的是在线读写闭环，不能通过已有设置启用。

随后先加入 `TestHistoricalAssessmentDecisionPreservesCurrentFocus`，在未修改处置实现时运行
`cd server && go test ./internal/learning -run TestHistoricalAssessmentDecisionPreservesCurrentFocus -count=1`，
实际失败为 `activity_state_conflict`：已接纳并离开 Feedback 的原评估无法追加作废。
该回归在修复后通过，真实数据库与浏览器再验证同一行为。

## 开发方案（实施前）

一个主要结果为“提交答案后能查证并处理正式学习记录”。按以下有界批次串行实施，
共享迁移、OpenAPI 和入口；不另建评分算法、异步 worker 或掌握度写入通道。

1. 冻结事实与查询：新增在线答案列表/详情，按区、原会话、原活动读取；包括接收回执、
   评估状态、原活动/答案/rubric/来源/context、答案所用内容版本、处置历史与有效 Evidence。
   追加迁移保存答案内容关联；无关联的历史答案明确未知，不以当前内容冒充原版本。
   查询沿用隐私屏障，不复制正文或引入导出缓存。
2. 正式处置：仍通过 learning owner 的原命令与双版本校验，允许处理原会话的历史评估，
   不切换当前焦点。补充 idempotency 目标绑定；复核、覆盖和作废均追加事件。
   不放宽来源、风险、帮助和证据资格政策；已接收答案在暂停/归档后继续结算。
3. Web：课堂摘要链接详情；学习区提供待评估/待复核/全部记录和已有继承提案。
   详情分开展示模型建议、字符串规则、人工处置及正式 Evidence；覆盖逐项选择原答案/来源证据。
   危险操作要求原因、具体确认；按 learning read/write/approve 隐藏无权操作。
   不确定响应核对原 operation/session/attempt 或 proposal，重试复用同一请求身份。
4. 同批更新 OpenAPI、Web DTO、使用文档；验证领域/HTTP/存储契约、真实 PostgreSQL 的
   幂等与重放、模型 fixture 和浏览器确认流程。明确记录真实模型质量未测及环境限制。

## 验收标准（实施前）

- 正式作答→接收→评估→反馈可操作；未评估、处理中、未知、已结算分别展示。
- 旧答案始终关联原题、rubric、目标/资料/context 和所提交的内容版本；新内容不重解释旧答案。
- 模型结论、规则核验、人工处置、有效 Evidence 分开，置信度不作为综合分数。
- 揭示答案不生成 Evidence；高帮助不自动产生掌握，自述及继续操作不提供能力证据。
- 不足来源、冲突和其他风险真实显示并按现有接纳政策处理。
- 列表和详情遵守学习区及隐私屏障；清除后旧正文、引用和新关联不可恢复。
- 复核/覆盖/作废要求正式权限、原会话和处置版本，历史原件及处置链保持不可变。
- 继承审批展示原证据、来源/目标版本与逐项映射；过期不可批准、重复操作不复制 Evidence。
- 响应丢失、重复提交、历史处置不影响其他活动，暂停/归档后的合法已接收答案仍可结算。
- 旧 CLI/API 响应保持兼容；新增未知能力明确受限；真实 PG、fixture、浏览器与构建检查如实记录。

交付当前工作分支供审查，遵守工作项禁止合并、rebase 或推送 base 分支的边界。

## 实施决策与边界

- 在原答案事务完成正文验证后写入 `learning_attempts.artifact_id/artifact_version`，
  不在新视图复制原文。原 context 读取活动已冻结的关联。历史未知关联保持空，不回填。
- 反馈以可重复读读取原事实和处置，按 knowledge→learning→tutoring 顺序持有读屏障，
  再由 HTTP 响应许可防止清除竞态下输出迟到正文。新增关联参与清除及残留检查。
- 模型仍只提供建议；原 learning owner 决定接纳、Evidence、掌握和复习投影。
  低确定性/冲突/歧义按现有政策允许明确人工复核；来源不足不能直接 confirm，
  必须逐项补齐原合同允许的引文才能覆盖。没有新增评分或复习算法。
- 继续学习后的处置改为校验原 assessment→attempt→activity 的不可变所有权链，
  同时保留原 session 和 disposition 双版本验证。当前会话焦点及状态不回退。
- 新 `assessment` 配对档案显式保留浏览器 learning 审批权限；普通档案仍只读，
  不扩展模型身份、知识审批或管理权限。
- 继承直接消费既有真实提案，批准/拒绝仍经原 learning 服务。该旧协议仅支持默认区，
  Web 明确限制，不借本项悄悄增加跨学习区迁移。审批只建立待验证映射，不复制 Evidence。
- 新 GET 契约与旧严格 DTO 分离；confirm 新增可选原因，旧 CLI 请求不变。
  Web 原因必填，完整生成 DTO 时同步之前未生成的已有路径，并明确以原手写细化协议
  覆盖同名生成路径，避免交叉类型收窄错误。
- 本项不新增导出或本地持久缓存。未提交处置及重试身份仅在标签页内存；正式结果可从
  原答案/会话/operation 或原 proposal 再查。刷新不是自动再次批准。

实际检查与限制见 [Issue #31 验收记录](../development/issue-31-acceptance.md)。
