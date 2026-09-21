# Issue #62：Studio 关联目标与收藏筛选

## 确认与开发方案

基线 `dc03439`（包含报告中的 `364fbe1`），初始工作树干净。
已读取开发规范、分层测试策略、目标管理及内容协作设计，并核对 Go 服务、
PostgreSQL 权威记录、同源 Web 与 CLI 的职责。此次修复只涉及目标列表查询和
Web 内容偏好保存，不改变内容版本、学习证据、设备归属或隐私清除流程。

修改前的代码证据：

- `server/internal/learning/postgresstore/privacy.go:202` 将清除后的目标保留为
  `source='privacy_erasure'`、`management=NULL` 的记录；`goals.go:95` 列举最新目标时
  未排除这些记录。`scanGoal` 还会省略默认学习区的旧格式字段。
  `clients/web/src/api/runtime.ts:51–83` 要求目标具有学习区与管理信息，
  因此 Studio 的整页目标解析失败，内容查询却仍能成功。
- `clients/web/src/components/content-tools.tsx:91–97` 在保存偏好后发起异步刷新，
  调用方 `teaching-page.tsx:1237` 丢弃刷新 Promise，按钮提前解除禁用。
  固定版本使用旧偏好构造完整 PUT；`learningcontent/library.go:75` 按合同保存全部字段，
  从而可能把刚设为 true 的收藏覆盖回 false。该问题依赖时序，符合不同浏览器结果不同的现象。

实施方案：

1. 在目标列表选择每个目标的最新修订后、分页前排除隐私清除记录；保留历史记录和
   未被清除的旧格式目标，不以缺少 management 判定清除。
2. 用偏好 PUT 返回的已确认数据更新当前内容查询缓存，取消尚未完成的旧偏好查询，
   在更新完成前保持操作禁用，避免下一次操作复用旧收藏或固定版本。
3. 添加 PostgreSQL 分页回归及浏览器受控延迟回归，再运行相关包检查和原 Studio 场景。

## 验收标准

- 同区存在已清除目标时，新目标仍可列举、搜索和分页；清除记录不占用列表页。
- 历史引用仍可读取清除标记，旧格式但未清除的目标不被误删。
- 收藏后立即固定第 2 版，即使旧偏好读取未返回，第二次保存仍带有 `favorite: true`。
- 恢复第 1 版形成第 3 版后，Studio 按目标及收藏筛选可见第 3 版，独立阅读仍打开固定的第 2 版。
- 相关 Go 测试与 vet、Web 类型检查、单测、构建及 Chromium 场景通过；未运行范围如实记录。

## 验证记录

- PostgreSQL 17.11 只读最小案例：从 `HEAD` 与工作树分别提取实际列表 SQL，
  在 `BEGIN READ ONLY` 中用 CTE 提供一条清除记录和一条新目标记录。
  原 SQL 返回两条（包含 `management: null` 的清除记录）；修复后仅返回新目标。
  使用现有本地测试容器，未修改其中的数据库或表。
- Node.js 24.20.0 最小竞态案例：提取修复前后的实际 `preferenceChange` 函数体，
  让旧刷新 Promise 始终处于等待状态，再连续保存收藏和固定版本。
  原实现的请求为 `{favorite:true,pinned_version:null}`、
  `{favorite:false,pinned_version:2}`；修复后第二次请求保持 `favorite:true`。
  此检查验证保存函数的数据流，不等同于 React 浏览器端到端验收。
- `cd server && go test ./internal/learning/postgresstore ./internal/transport/httpapi -count=1`
  通过；未配置 `TEST_DATABASE_URL`，数据库集成用例跳过，不计为集成通过。
- `cd server && go vet ./internal/learning/postgresstore ./internal/transport/httpapi` 通过。
- `cd server && go build ./cmd/edu-agentd` 通过；这是普通服务构建，不包含生产 Web 资产。
- `git diff --check` 通过。

新增回归包括 `TestPostgreSQLGoalListExcludesErasedGoalsBeforePagination`、
`memory.spec.ts` 的清除后 Studio 目标选择，以及 `workspace.spec.ts` 中暂停偏好读取的
连续收藏/固定断言。当前工作树没有 Web 依赖，也未配置独立测试数据库；已请求按锁文件
安装依赖并启动本任务临时数据库的授权，尚未收到确认，因此新增数据库回归、Web 类型
检查、单测、生产构建和 Chromium 端到端仍待执行。未推送、创建 PR 或合并基线分支。
