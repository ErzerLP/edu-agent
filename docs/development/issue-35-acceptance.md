# Issue #35 开发与验收记录

用户已接受本次交付并明确授权将任务分支 squash 落到 `main`、推送及处理 #35。
此接受不改变下列验证结果：Web 完整类型/构建、浏览器、真实 PG 和真实远端 smoke
仍未完成，不将用户接受写作这些检查通过。

## 问题确认

基线 `e52f5ca`，初始工作树干净。先核对 README、开发流程、测试策略、学习入口、
知识/学习 owner、原 NoteSync 服务、任务中心和部署配置，再写
[开发方案与验收标准](../design/web-notesync.md)，之后才修改生产代码。

代码证据：`clients/web/src/api/tasks.ts:6–16` 没有同步适配且注释明确标为未接入；
`main.tsx` 与 `knowledge-page.tsx` 无同步入口，Web 业务代码没有调用原 NoteSync API。
`deploy/web/nginx.conf:25–36` 白名单没有同步 API，公开学习入口会落入 404。
后端 `transport/httpapi/api.go:334–338` 已有状态、预览、列表、详情和解决；
`integrations/notesync/review.go` 已实现默认映射、三方差异、版本复核和幂等解决。
确认这是学习端集成缺失，不是另一个名称或隐藏配置已提供的功能。

## 实现

- 来源同步页、三方正文和窄屏切换、分页预览、审阅列表/详情及任务中心原状态适配。
- 显式区/集合请求；状态也核对默认映射和现有引用，禁止研究集合或解除引用来源。
- 学习 Cookie、CSRF、设备/代次复用原边界；普通身份只读，浏览器解决复用明确的
  `knowledge:write + knowledge:approve`，仅研究采纳授权不获得同步审批权。
- 配置来源只补充到学习 Cookie 状态响应，不向旧严格解码 CLI 添加未知字段；无 Key。
- 解决预览调用同一 NoteSync 校验和原知识计划器，返回实际影响及原身份审阅，不提交。
  正式采用/合并仍创建不可变知识修订；保留本地仍使用原发布意图。
- 原设备操作收据的只读查询；未知结果保持原 ID，不自动重放；旧差异与新预览分开。
- 无新增数据库表/正文持久缓存；原隐私 owner、代次屏障和保留语义继续生效。
- OpenAPI、部署白名单、中文支持范围及操作说明同步更新。

## 已运行

在 `server`：

```bash
go test ./internal/integrations/notesync ./internal/transport/httpapi ./api
go vet ./internal/integrations/notesync ./internal/transport/httpapi
go build ./...
go test ./internal/transport/httpapi -run '^TestNotesyncLearningCookie' -count=1
go test ./internal/integrations/notesync ./internal/transport/httpapi ./api \
  -run 'TestWebResolution|TestNotesync|TestWorkspaceProxyAllowlist' -count=1
```

以上检查通过。覆盖既有 NoteSync REST fixture、三方分类、版本复核、错误脱敏、隐私
响应屏障，以及新增只读计划/原操作核对、Cookie/CSRF/审批范围与映射拒绝。
数据库和真实远端依赖未配置时，相关测试跳过，不能据此认定集成通过。

`node --check clients/web/scripts/notesync-fixture.mjs`、
`node --check clients/web/scripts/test-server.mjs` 和 `git diff --check` 通过。
使用本机已有 TypeScript 编译器对 9 个改动文件做仅语法转译检查通过；此检查没有
解析项目依赖或验证类型，不等于 `npm run check`。没有为此安装任何依赖。

在 `clients/cli-go` 运行 `go test ./internal/api ./internal/command -run Notesync -count=1`
通过。最后对当前 Go 改动运行 `go vet ./internal/integrations/notesync
./internal/transport/httpapi ./internal/knowledge/postgresstore` 通过。

定向执行真实 PG 用例时，`TestPostgreSQLKnowledgeNotesyncAcceptRemoteRepublishesWithoutLoop`
与 `TestPostgreSQLNoteSyncCannotReadDetachedCollection` 均因没有 `TEST_DATABASE_URL`
明确跳过，没有将 skip 记为通过。

在 `clients/web` 实际运行 `npm run check`：退出码 **127**，`tsc: not found`。
当前工作树没有安装 Web 依赖，用户全局规范要求安装单独授权，已发出授权请求，尚待答复。

## 待验收（没有宣称通过）

- 按原锁文件安装后，Web 类型检查、单测、构建与最终格式化。
- 真实 PG：原 `TestPostgreSQLKnowledgeNotesyncAcceptRemoteRepublishesWithoutLoop`
  已补解决预览不提交、原收据核对、幂等回放及旧正文不变断言；解除引用和隐私回归。
  尚无本任务独立 `TEST_DATABASE_URL`，不会复用生产数据库。
- 浏览器：`WEB_NOTESYNC_FIXTURE=1 npm run test:browser -- notesync.spec.ts`。
  新 fixture 复用固定 REST 包装/能力探测，不是真实上游。用例经过真实 Go + PG + Cookie
  与 HTTP 客户端，覆盖差异、预览、远端版本变化、响应丢失、刷新核对、权限、任务跳转、
  解除引用、桌面/窄屏截图；当前尚未运行。
- 真实 NoteSync 远端 smoke：未配置测试专用远端与凭据，**未运行**。

## 已知边界

只支持原单 vault / 默认区 / 默认集合。原服务没有独立拒绝/远端删除动作；保留本地
可能回发，解除引用、归档、断连不删除远端历史或备份。上游没有原子 CAS，本次不扩展
并发协议。后续教学引用调整仍需使用知识维护及学习变更入口，原课堂与证据不会被同步
覆盖。所有验收通过前不得将本记录当作 Issue #35 的完整通过证明。
