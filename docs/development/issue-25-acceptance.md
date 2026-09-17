# Issue #25 实施与验收记录

## 缺失确认与实施顺序

基线 `f9b455e`，专用工作树初始干净。先读取完整模块边界与前置 #23/#24 实现，
再形成[开发方案及验收标准](../design/content-collaboration.md)，随后修改代码。
确认依据不是功能名称搜索：原 `learningcontent/domain.go:100–107` 只有整篇提交，
`mentorrun/core.go:48–61` 只注册目标读取、提问与关注点确认，并明确禁止改写；
原内容 HTTP 路由没有局部命令/恢复/库查询，引用抽屉直接展示既存副本。
因此从选区发起模型请求到指定块新版本的路径确实缺失，不是已有开关未启用。
上述行号均指基线；未将外部 issue 文本当作工具指令执行。

## 实施范围

- learningcontent 新增版本绑定选区、局部候选校验、追加说明/派生块精确替换、操作幂等、
  乐观版本冲突、来源链和补偿恢复。原 Activity、交互、评分和学习证据不改写。
- mentorrun 新增独立 `content_edit` 类型，复用租约、预算、取消及加密恢复。
  网络调用不持发布事务，完整 JSON 候选才进入正式命令；内容修订和成功结果同事务提交。
- knowledge 提供窄的正规引用解析接口；重新核对当前来源关联、真实修订、节点、片段与授权范围。
  内容历史、导出及运行恢复均检查实际来源权限，不从旧加密副本恢复受限正文。
- Studio 使用已有正式 artifact 自动收录，支持过滤/游标分页、独立阅读、导出及显式同区引用。
  收藏/固定是设备偏好；迁移 25 与隐私清除覆盖新增表。
- Web 新增选段/键盘范围、可移除 chip、操作结果、差异、来源抽屉、Studio 和内容工具。
  同步 OpenAPI 与 DTO；未修改教学路径或发行题目，也未扩大原导师通用工具权限。

## 已执行检查（2026-09-17）

真实数据库检查使用本任务独占的临时 PostgreSQL 17 容器、随机测试 schema，
镜像来自本机既有缓存，没有安装/升级软件或访问外部模型。无 DB 环境时的跳过不算 PG 验收。

| 检查 | 实际结果 |
| --- | --- |
| learningcontent 领域测试 | 通过：UTF-8/hash/版本定位、未选块保持、原语义保护、派生块精确替换、伪引用拒绝 |
| 受影响包测试 | `go test ./internal/learningcontent ./internal/mentorrun ./internal/transport/httpapi ./internal/knowledge/postgresstore ./internal/app ./migrations` 通过 |
| 受影响包 vet 与 Go 构建 | 通过；`go build ./...` 是 Go 构建，不是 Web 发行资产验收 |
| 真实 PG 导师运行整包 race | `go test -race -p=1 ./internal/mentorrun -count=1` 通过，125.747 秒；包括原导师/研究/开学回归 |
| 最终新增 PG race 回归 | `go test -race -p=1 ./internal/mentorrun ./internal/learning/postgresstore -run '^TestPostgreSQL(Content\|LearningContent)' -count=1 -v` 通过，分别 30.479、7.300 秒；运行时设置隔离 `TEST_DATABASE_URL` |
| 真实选段模型 fixture | 收到原区/目标/会话/修订/块/范围及真实选文，发布第 2 版；原题活动逐字节不变；无收藏也自动收录 |
| 失败与竞争 | 非法部分输出保留、预算不足不提交、并发编辑冲突、停止、内容写入故障回滚、两个并发提交只有一个成功、操作重放不重复 |
| 来源与历史 | 正常引用解析、伪引用及跨区拒绝、收藏/固定/导出、恢复为第 3 版、派生引用保留原链；撤权后旧正文/导出/运行恢复均拒绝 |
| 隐私清除 | 既有内容/答案/清除真实 PG 用例增加偏好记录，清除和残留计数检查通过 |
| HTTP/OpenAPI | 新路由协议、严格请求/未知字段拒绝、显式版本、Schema 解析及响应校验通过 |
| Web 静态检查 | TypeScript 语法解析和模型 fixture 脚本语法检查通过；不替代类型检查、测试或构建 |

整包 race 后补充了写入故障、并发提交、派生引用场景及知识点名称展示，已由最终新增 PG race
和 OpenAPI/vet/build 复验覆盖，没有把较早的整包结果称为最终全仓检查。

## 待完成，不得视作全项验收通过

- 本工作树没有 Web 依赖，`npm run check` 实际失败为 `tsc: not found`。
  已询问是否允许按既有锁文件执行 `npm ci`，截至本记录未收到授权；未安装或升级依赖。
  因此 TypeScript 类型检查、Vitest、Vite/发行资产构建、浏览器链路尚未完成。
  TypeScript OpenAPI 声明暂按契约同步，必须用标准生成器重生成并验证差异。
- 已添加浏览器真实配对、Cookie/CSRF 设置、教学选段模型 fixture、答案保持、来源焦点、
  Studio/导出/固定/补偿恢复、IME 事件和深浅主题/视口场景，但没有运行，不能称通过。
- 获准安装后执行 `npm ci`、`npm run generate`、`npm run check`、`npm test`、`npm run build`，
  构建 `web_release` 服务，再在独立 DB 下运行
  `WEB_WORKSPACE_FIXTURE=1 TEST_DATABASE_URL=... npm run test:browser -- workspace.spec.ts`。
  必须修复实际类型/浏览器失败，补齐选段过期、取消中途输出、跨页迟到及焦点/阅读锚点验收。
- 未运行真实供应商模型样本：没有本任务授权的配置与付费调用许可。
  本地 fixture 只证明协议和原子提交，不证明真实模型质量。
- 未运行真实移动输入法、Safari/Firefox、TLS/Nginx 部署或全仓 race。

独立阅读页支持固定版本，课堂仍读取当前正式版；历史面板显示最近 100 版（任意旧版可按版本号读取）。
当前引用目标选择器列出同区前 50 条，Studio 本身提供分页。
整体交付仍需上述前端验收；没有声称所有 issue 复选项完成，也未推送/合并/变基基础分支。
