# Issue #30 验收记录

## 先确认，再实施

基线 `9a80ee4`，初始工作树干净。先读模块边界和现有合同，再记录
[开发方案与验收标准](../design/dynamic-knowledge.md)，之后才修改生产实现。

既有 ConceptRevision 仅含身份、名称、语义键和研究引用（基线
`server/internal/knowledge/context.go:14`）；既有 Web 知识页只有集合、导入和参考
（基线 `clients/web/src/knowledge-page.tsx:26`）。文档 maintenance 服务已经存在，
但不能提供概念关系、冲突主张、概念合并/拆分及其审阅。

先加入 HTTP 回归用例，再运行：

```bash
cd server
go test ./internal/transport/httpapi -run '^TestIssue30KnowledgeStructureCapabilities$' -count=1
```

修改前：能力入口返回 **404**，用例失败。修改后：同一用例通过。
原因是结构领域数据和正式服务/页面入口均缺失，不是已有设置未开启。

## 已实现范围

- 在 knowledge owner 内追加概念结构、独立身份、具体章节出处与主张/条件/缺口。
- 先授权过滤再分页，图和列表使用同一页；一跳关系及有限结果，不递归遍历图。
- 正式提案、审阅、合并/拆分映射、拒绝和补偿；幂等回放、基础版本与依赖核验。
- 学习事实由 learning owner 的只读端口提供；不复制 Evidence、不按来源采纳判定掌握。
- 后续上下文通过 learningchange 的正式路线候选、安全边界和版本检查接入。
- 新增数据纳入既有隐私屏障、清除与残留核验；旧版本不能恢复已清除正文。
- Web 节点/局部图/详情/维护记录与教学侧栏；旧文档维护复用原服务及默认映射。
- OpenAPI、能力入口、增量迁移、部署白名单和中文使用说明。

## 已通过检查

Go 检查在 `server` 运行。真实 PG 使用独立本机 PostgreSQL 17/pgvector 容器，
各集成测试创建独立 schema；没有连接生产库。设置独立 `TEST_DATABASE_URL`，
数据库测试按 `-p=1` 串行执行。

1. `go test ./internal/knowledge/... ./internal/transport/httpapi ./api ./migrations ./internal/learningchange`
   的非数据库检查。OpenAPI 包最初发现部署白名单仍将旧维护入口列为禁止；本次正式开放
   这些接口后，更新对应正向断言并补内部/强制写入路径的负向断言，`go test ./api` 通过。
   **未设置数据库的用例跳过，不计为真实 PG 验收。**
2. `TEST_DATABASE_URL=… go test -p=1 ./internal/knowledge/postgresstore -count=1` 全包通过。
   包括多来源、同名独立身份、关系环与分页、跨区隔离、原文核验、解除引用、并发审批、
   过期游标、旧引用、合并/拆分、拒绝、补偿和未知结果幂等回放。
3. `TEST_DATABASE_URL=… go test -p=1 ./internal/knowledge/postgresstore ./internal/mentorrun ./internal/privacy/postgresstore -run 'TestPostgreSQLIssue30|TestPostgreSQLAdaptiveApprovalVersionsAndExplanation|TestPostgreSQLAdaptiveGoalRevisionAndQueueFences|TestKnowledgeRedactedRevisionTombstoneAllowsFreshImport' -count=1` 通过。
   覆盖真实课堂安全边界、当前题不变、旧 context 不变、新内容依赖使提案过期和隐私清除。
4. `TEST_DATABASE_URL=… go test -p=1 ./internal/transport/httpapi ./migrations -run 'TestIssue30|TestKnowledgeMaintenanceHTTP' -count=1` 通过。
   包括实际从 28 升级到 29、旧版本头正确回填、追加修订推进版本、身份注入和独立审批权限。
5. `go vet ./internal/knowledge/... ./internal/learningchange ./internal/learning/postgresstore ./internal/transport/httpapi ./internal/app ./internal/mentorrun ./internal/privacy/postgresstore` 与 `go build ./...` 通过。
6. 在 `clients/cli-go` 运行 `go test ./internal/api ./internal/command -run 'Knowledge|Maintenance|OpenAPI' -count=1` 通过，未改旧 CLI DTO。
7. `git diff --check` 通过。

完整 mentorrun 持久化包曾发现三处本次引入的兼容性回归：无结构变化时错误填入旧 context，
导致已修订目标或暂停路线被拒绝。已限定为确有结构变化时才创建候选 context，并重跑第 3 项
中的全部失败场景通过；**修复后未重新运行完整 mentorrun 包**，不将局部结果写成全包通过。

## 尚未完成的验收

当前工作树没有 `clients/web/node_modules`。用户工作规范要求安装须单独授权，已请求允许
`npm ci` 按现有锁文件安装，尚未获得确认，因此没有安装或升级依赖。
实际运行 `npm run check` 返回退出码 127：`tsc: not found`。

下列项目仍待完成，不能据现有后端结果认定 Issue #30 全部验收通过：

- Web OpenAPI 类型生成、TypeScript 检查、格式化、单元测试及生产构建。
- 新增 `knowledge-structure.spec.ts` 的真实浏览器列表/图、键盘、审阅、响应丢失恢复、
  移动视口及无障碍检查，以及受影响的既有浏览器用例。
- 根据实际检查结果修复前端问题并进行最终差异审查。

以上是开发阶段的验收结果。用户随后明确接受现有结果，授权提交任务分支并以 squash
落地到 `main`、推送远端；本次按该后续指令落地，不将 Web 的待验收项改写为通过。
专用测试容器 `edu-agent-issue30-test` 已停止，未删除；可在继续验收时重新启动。
