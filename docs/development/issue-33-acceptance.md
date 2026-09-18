# Issue #33 实施与验收记录

## 缺失确认

起点 `a5bf595`，工作树干净，未发现上次中断留下的改动。
先核对项目开发规范、服务端模块化 owner、PostgreSQL/隐私、现有 runtime、
配置、课堂变更、Web 与独立 CLI 合同，再编写[开发方案和验收标准](../design/tutor-history.md)。
外部 issue 仅用作需求数据，没有执行外部指令。

基线 `server/internal/mentorrun/store.go:409` 用区/目标/设备/代次/kind 唯一约束
只保留单个运行 session；`:423–424` 每次仅放入当前 prompt，期限固定七天。
`server/internal/transport/httpapi/mentor_runs.go:30–43` 没有聊天 CRUD，
`clients/web/src/main.tsx:190–206` 没有 chat 页面。因此确认为缺失功能，
不是开关未开启，也不是 CLI 的本地 Session 管理能替代。

## 实现

- 独立 TutorConversation/Turn、追加迁移，不迁移旧七日运行；会话绑定不可改。
- 已提交消息、完整工具组及来源记录加密保存；模型上下文连续，不执行历史工具。
  真实当前 run 和操作回执负责运行恢复、并发、未知结果及预算。
- 标题加密、按当前上下文列出、标题搜索、列表/轮次分页、改名与二次确认删除。
  临时正文不落库，重启明确不可恢复；无目标交流也有正式入口。
- 目的地摘要绑定明确 provider/endpoint，未确认零外发；隐私代次阻断旧快照。
- 课堂/目标/独立页共用组件、运行订阅及内存草稿；提交丢响应后查询原回执。
- 停机事务轮换所有共用密钥的 owner 密文，保持不可变业务版本；运维备份边界有说明。
- 同步 HTTP/OpenAPI、Nginx 白名单、使用说明；CLI 本地历史与凭据不迁移。

## 已获得的验证证据

本任务独立本地 PostgreSQL 17 容器 `edu-agent-task54-postgres`，端口 32954；
PG 用例分别使用随机 schema 且串行运行，不访问其他项目数据库。

- `go test -p=1 ./internal/mentorrun -run 'TestPostgreSQLTutorHistory|TestHistorySchema'`：
  首批通过，覆盖多轮/分页/标题检索、重启及七日缓存清理、永久删除、临时无正文、
  可选目标、确切端点确认、损坏/错误密钥/未来 schema、存储故障回滚及过期交互。
- 真实 PG 隐私清除用例通过：新历史表、标题/索引和 checkpoint 被清除，
  代次屏障关闭读取并取消在途临时运行。
- 真实 PG 共享密钥轮换用例通过：聊天历史、运行 checkpoint、正式内容版本及
  教学变更历史新钥可读，旧钥失效，不可变业务字段保护保持。
- 真实 Cookie/CSRF HTTP 用例通过：非默认区新建/提交/详情/确认删除，
  错误学习区拒绝，删除无确认或无 CSRF 拒绝，恢复不增加模型请求。
- 并发/配额 PG 用例通过：同操作并发同回执、单模型流、删除阻断迟到正文、
  目标版本改变后重试原回执、16 MiB 配额拒绝新写且保留第一轮。
- 受影响 Go 包非数据库测试、OpenAPI 响应/身份合同及追加迁移检查通过。
  无 `TEST_DATABASE_URL` 的跳过未计入 PG 验收。
- `go test -race -p=1 ./internal/mentorrun ./internal/transport/httpapi -run
  'TestPostgreSQLTutorHistory|TestPostgreSQLMentor|TestHistorySchema|TestCheckpointEncryption'`
  通过；覆盖新历史用例及原导师恢复、租约、隐私、真实 Cookie/CSRF/SSE。
- 受影响服务端包 `go vet` 和 `go build ./...` 通过。后续无目标工具收敛只补跑
  对应真实 PG 用例；停机轮换 CLI 参数检查通过。
- 利用环境中已有 TypeScript 编译器，对 12 个变更 TS/TSX 文件执行语法转换检查，
  通过。这不是依赖类型检查，不替代 `npm run check` 或浏览器验收。

Web 类型/构建、浏览器恢复与外发拦截用例待执行：本工作树没有 Web 依赖，
此前已按用户全局安装规范请求允许依锁文件 `npm ci`，未获得安装授权。
用户随后明确接受任务并要求落地主线；此处保留未验证范围，不将未执行项标为通过。

## 合入主线的冲突与回归

合入 `main` 的 `9aa2d53`（#32）时，手动解决以下冲突：

- `clients/web/src/components/workspace-shell.tsx`：同时保留导师历史与进度导航。
- `clients/web/src/main.tsx`：同时保留导师对话和进度页的导入、路由及注册。
- `server/migrations/migrations_test.go`：保留复习迁移检查，增加最新导师历史迁移检查。
  本任务迁移顺延为 `000032_tutor_history.sql`，主线已存在的
  `000031_review_sessions.sql` 内容不变，避免同编号冲突与已应用校验失败。

同时核对自动合并的隐私清除和 runtime：复习承载与导师历史清除均保留；
绑定目标的进度工具保持，无目标临时对话不提供目标绑定工具，并增加回归断言。

在上述独立 PG 上重新通过：

```sh
go test -race -p=1 ./internal/mentorrun ./internal/transport/httpapi ./internal/learning/postgresstore ./migrations -run 'TestPostgreSQLTutorHistory|TestPostgreSQLMentor|TestHistorySchema|TestCheckpointEncryption|TestPostgreSQLReviewCarrier|TestEmbeddedMigrationsAreOrderedAndUnique' -count=1
go test ./internal/learning ./api ./internal/app ./cmd/edu-agentd ./internal/learningcontent ./internal/learningchange ./internal/platform/keyrotation
go build ./...
go vet ./internal/mentorrun ./internal/transport/httpapi ./internal/learning/postgresstore ./migrations ./internal/app ./cmd/edu-agentd
```

首条命令设置 `TEST_DATABASE_URL` 指向本任务 PG，包含历史/轮换/隐私、真实 HTTP、
主线进度工具及复习承载回归；其余命令没有把跳过的 PG 用例计为通过。
12 个任务相关 TS/TSX 文件重新通过语法转换检查；Web 依赖类型、构建与浏览器
用例仍未执行，限制与上节一致。`git diff --check` 通过。
