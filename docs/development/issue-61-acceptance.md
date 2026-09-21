# Issue #61：共享候选数据库下的浏览器场景准备

## 架构与问题确认

基线 `dc03439`，初始工作树干净。Go 模块化单体及 PostgreSQL 保存知识、身份、
学习证据和隐私代次；React 学习端使用同源 API，Playwright 启动生产发行资产及
本地模型/NoteSync fixture。知识正文清除、身份匹配和提案审批分别由原 owner 负责。
本任务没有单独的 Comet change，按 issue 给出的测试范围实施，不修改历史规格。

`scripts/check-web-release.mjs:92–110` 按 Chromium、Firefox、WebKit 和文件名串行
执行，每个文件重启服务但复用 `TEST_DATABASE_URL`。三个问题都能在当前代码定位：

1. `workspace.spec.ts:55–62` 读取默认 head 后无条件导出正文；
   `memory.spec.ts:89–102` 已执行全局清除。服务端
   `knowledge/postgresstore/store.go:487–511` 保留已清除修订墓碑，
   `knowledge/service.go:847–854` 明确返回 `content_redacted`。head 存在不表示
   正文可读，重新启动服务也不会恢复正文。
2. `knowledge-structure.spec.ts:39–41,51` 用固定理由定位并统计提案；
   `components/knowledge-structure.tsx:114–119` 给每个历史提案独立的理由输入，
   待审阅记录只有自身理由非空时才能批准。标题不是提案身份；历史记录出现后，
   旧输入可能先匹配，或定位违反唯一性，固定理由数量也不再等于一。
3. `notesync.spec.ts:36–43` 只随机化路径，正文仍与历史同步资料相似；
   `knowledge/service.go:512–517,535–558` 对相似正文返回
   `document_match_ambiguous` / `document_similarity`。既有 `as_new` 合同
   （`openapi.yaml:9407`、`knowledge/service.go:465–466`）可以明确创建新身份，
   测试准备没有使用它。默认集合是现有 NoteSync 的固定映射，不能换随机集合规避。

以上行号为修改前位置。失败属于候选测试的状态假设；没有证据支持修改生产保护。

## 开发方案

保持一个交付结果：共享候选数据库存在前序状态时，三个场景仍能到达并验证原有操作。

1. 继承准备只对明确的 `content_redacted` 拒绝按空正文处理，保留真实 head 作为
   新导入的父版本；其他错误继续失败，旧正文不会恢复。
2. 知识维护捕获实际创建的提案和操作 ID，核对响应丢失后的幂等重放，并定位同一提案
   的输入与按钮；去重按本次操作核对，不依赖全库理由唯一。
3. NoteSync 使用现有“作为新资料”合同准备独立文档；保留相似文档未明确新建时
   必须审阅的回归，继续使用原默认集合和正式预览/解决/解除引用流程。
4. 补充共享状态回归与本记录，不改 API、迁移、生产业务或候选数据库复用规则。

## 验收标准

- 前序全局清除后继承准备成功，新资料可用；旧修订导出仍返回 `content_redacted`。
- 同名已应用提案存在时，新提案的响应丢失、原操作核对、审批、图视图和键盘操作通过；
  重放不新增提案，旧提案不会被误操作。
- 存在相似历史同步正文时，普通导入仍要求身份审阅，明确新建成功且保留独立身份；
  同步操作不把拒绝当作成功。
- 使用同一独立 PostgreSQL 数据库和生产资产运行相关浏览器序列；Web 类型检查、
  单测、构建及必要的既有隐私/身份契约通过。完整矩阵或无关已知问题单独记录。

## 验证记录

已运行：

- `cd server && go test ./internal/knowledge -count=1`：通过，覆盖现有知识领域与身份合同。
- `cd server && go vet ./internal/knowledge`：通过。
- `node --test scripts/web-release-results.test.mjs`：2 项通过，失败/跳过不能冒充发布证据。
- `git diff --check`：通过。

当前工作树缺少 npm 依赖及 `TEST_DATABASE_URL`；本机已缓存 PostgreSQL 17 和
Playwright 1.62.1 Noble 镜像。已请求安装锁定依赖和创建本任务专用临时环境的授权。
Web 类型检查、单测、生产构建、真实 PostgreSQL 及三引擎共享状态浏览器验收尚未运行，
以上领域测试不能替代这些证据。

## 主线集成

用户已接受上述实现和验证边界，要求将任务 squash 到 `main`。任务分支合入
`0b2b009` 时自动合并成功，无需手工解决冲突。该主线已经包含 #63 的候选数据库
隔离；保留此隔离方式，本任务仍补齐单场景面对历史数据时的准备和身份定位。
主线 #62 的收藏/固定回归与本任务的继承准备均保留；集成不扩大已记录的验证结论。
