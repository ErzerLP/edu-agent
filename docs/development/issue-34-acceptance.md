# Issue #34 开发与验收记录

用户已接受本次交付并授权将 `task/56` squash 落到本地 `main`。
此接受不改变下列验证结果；Web 与真实 Nocturne 未验收项仍不计通过。

## 问题确认

基线 `ce02ffb`，任务分支 `task/56`，开始时工作树干净。先阅读项目架构、开发流程、
测试策略，以及身份、导师运行、历史、memory/privacy owner 和 Nocturne 的既有合同，
再完成[方案与验收标准](../design/web-memory-privacy.md)，之后实施。

基线 `clients/web/src/main.tsx:221` 未注册记忆、数据或设备页面；
`server/internal/identity/web.go:29` 过滤对应 Web scope；
`server/internal/mentorrun/core.go:48` 无保存偏好的正式受限工具。
已有管理面只读树和 CLI 入口不是浏览器审批闭环，确认不是已实现但名称不同。
新增 `TestMemoryWebRequiresExplicitProfile` 在修改前失败：`ParsePairingProfile("memory")`
返回无效身份输入；实现后通过。

## 实现边界

- `/app/memory`：原候选/记录查询、具体审阅、明确批准/拒绝、纠正、删除、按页导出、
  交付与可用重放；纠正必须有可核对的已交付原正文。Web 强制审阅，不改变原 CLI
  自动准入政策或 Nocturne 存储合同。
- `request_memory` 只创建模型候选并等待；继续查原服务真实回执，不靠普通回复批准。
  临时要求不产生候选；权限在执行、事务和模型发送前复核。候选/取消使用原服务
  的外层事务保存点，失败回滚；未引入第二套审计或交付状态。
- 个性化实时读取已批准、已交付的全局信息，显示记录版本与来源。只将来源元数据
  存入运行，远端正文不复制到 checkpoint。正式学习事实继续由原目标/区工具提供。
- 数据页区分每项删除的范围，使用原一次性 grant 和逐 owner 清除回执；新增只读
  原操作查询，解决清除导致旧会话失效/丢响应后无法定位回执的问题。
- 设备页复用实时撤销；新 `memory` 配对档案显式授予管理权限，不给旧 token 增权，
  不开放本机 admin。同步 OpenAPI、反向代理白名单和中文使用说明。

## 已执行与结果

真实 PostgreSQL 使用任务专属本地容器 `edu-agent-task56-postgres`（本机已有
`postgres:17` 镜像、loopback 32956、独立测试库），没有连接用户业务数据库。
各 Go 集成测试创建随机 schema 并自行清理。

在 `server`、配置该独立 `TEST_DATABASE_URL` 后：

```bash
go test -p=1 -count=1 ./internal/mentorrun -run '^TestPostgreSQLMentorMemory' -timeout=2m
go test -p=1 -count=1 ./internal/app ./internal/identity/postgresstore -run 'TestPostgreSQLWeb(Memory|PairingRolls)' -timeout=2m
go test -p=1 ./... -timeout=5m
go vet ./internal/identity/... ./internal/memory/... ./internal/mentorrun ./internal/privacy/... ./internal/transport/httpapi ./internal/app ./cmd/edu-agentd
go build ./cmd/edu-agentd
```

上述检查最终全部通过。全量首轮发现旧权限断言需更新、新测试夹具表名写错，已修正，
最终全量退出码为 0；导师包真实 PG 全量复测耗时约 250 秒。其余同状态已通过的包
复用 Go 测试缓存。未配置真实远端的条件验收仍不计通过。覆盖：

- 真实 Cookie/CSRF 新建待审阅、陈旧版本拒绝、批准产生正式排队回执、拒绝清正文；
  远端未配置时导出显示 degraded，不冒充已保存正文。
- 模型申请→等待→用户正式审批→真实记录回执；“好”不能批准；取消/临时要求/撤权
  均不产生长期记录；同决定操作重放不重复创建记录；外层事务回滚不遗留候选或操作收据。
- 原 memory/outbox/Nocturne consumer 与确定性远端哈希回读夹具推进真实 PG 交付，
  原 exporter 提供模型个性化；来源可见、checkpoint 不复制正文；读取后删除或撤权
  阻止模型发送。
- 撤销设备后旧 Cookie 写入失败；没有 grant 拒绝隐私清除；有效 grant 提交原屏障，
  旧会话失效；重新配对可查询原设备/操作的同一回执，旧候选仅保留已脱敏审计。
- 原 memory/privacy、导师历史与在途运行、来源和引用的全量 PG 回归，以及原远端
  确定性 HTTP/consumer 夹具通过；不重写其他 owner 的清除实现。

`node --check clients/web/scripts/test-server.mjs` 和 `git diff --check` 通过。

## 尚未验收

实际执行 `cd clients/web && npm run check` 返回 **127 / `tsc: not found`**。
此工作树没有 Web 依赖，已请求按锁文件执行 `npm ci` 的安装授权，尚未收到答复；
没有绕过安装授权，也没有把静态审阅当作类型检查。

- Web 依赖安装、OpenAPI 类型重新生成、类型检查、单测、格式化与生产构建未运行。
- 新增 `memory.spec.ts` 的真实 Go/PG/Cookie 浏览器流程尚未运行：移动审批/取消焦点、
  拒绝、降级导出、设备撤销、清除后重新配对查回执、导师内嵌候选审批。运行需最新
  Web 构建和 Go 二进制、`WEB_MENTOR_FIXTURE=1` 及独占测试库。
- 完整远端读写/纠正/删除/重放的浏览器验收尚未执行；已有原服务的确定性 fixture
  与 PG 生命周期测试不能替代浏览器端到端验收。
- 真实 Nocturne 远端验收 **未运行**，没有使用已有业务远端或把夹具冒充真实服务。

当前不能宣称 Issue #34 全部验收通过；按用户后续授权仅执行本地 squash 落地。
本次推送指令存在矛盾，因此不推送，也不把本地落地视为远端 Issue 已完成。
