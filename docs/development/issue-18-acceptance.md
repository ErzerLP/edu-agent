# Issue #18 验收记录

日期：2026-09-13。基线：`bc55267e023bb3b5cb77f9da61d81b44d5c6257d`。交付分支：`task/24`。

## 先确认，再实施

基线 `server/internal/transport/httpapi/api.go:266–365` 没有 `/app/`；`:282` 只注册返回长期设备凭据的配对路径；`:527–532` 在所有普通 API 上强制解析 Bearer。`clients/` 只有 Go CLI，现有 `adminui` 仅服务独立本机管理身份。因此浏览器学习入口确实缺失，并非隐藏设置或更名功能。

已存在的 `learning/goals.go`、`learning/postgresstore/goals.go` 与 `learningspace` 提供真实目标、版本、历史、生命周期和范围校验。本次没有复制业务状态机或将资料、模型设为保存前置。实施前已记录 [开发方案和验收标准](../design/web-learning-entry.md)。

## 环境

- Linux，Go 1.26.6，Node 24.20.0；React 19.3.0、Vite 7.3.6、TypeScript 5.9.3，完整版本由 `clients/web/package-lock.json` 固定。
- 本任务独立 PostgreSQL 17.11 容器，镜像 `pgvector/pgvector@sha256:cf134a767f474095eeba57e0117be8e568e011a63f33fbf252f14c9b760f8e6f`；连接回环端口 32928。Go 集成使用随机 schema 并清理，浏览器测试使用独立测试库。没有操作既有部署数据库。
- Playwright 1.62.1 与 axe 4.13.0。宿主 Chromium 因缺少 `libasound.so.2` 无法启动，随后使用已存在的 `mcr.microsoft.com/playwright:v1.62.1-noble` 容器运行真实浏览器；没有安装系统库。

## 实际通过的检查

| 范围 | 命令或测试 | 结果 |
| --- | --- | --- |
| Go 候选 | server 内 `go test ./... && go vet ./... && go build ./...` | 通过；此轮未配置数据库的 skip 不计数据库证据 |
| 最后 HTTP 修正 | `go test ./internal/transport/httpapi && go vet ./internal/transport/httpapi` | 通过；覆盖失效 Cookie 清除，复用未改变包的候选结果 |
| 真实数据库包 | `TEST_DATABASE_URL=… go test -p=1 -count=1 ./migrations ./internal/identity/postgresstore ./internal/transport/httpapi` | 三包通过 |
| 新身份/HTTP race | `TEST_DATABASE_URL=… go test -p=1 -race -count=1 ./internal/identity/postgresstore ./internal/transport/httpapi -run 'TestPostgreSQLWeb\|TestWebProduction'` | 通过 |
| 隐私清除 | `TEST_DATABASE_URL=… go test -p=1 -count=1 ./internal/privacy/postgresstore -run '^TestPostgreSQLCompletePrivacyMarkerFixtureAndReplay$'` | 新会话随既有完整 marker 夹具清除与重放验证通过 |
| 既有 CLI 契约 | cli-go 内 `go test ./internal/api` | 通过 |
| 前端 | `npm run generate`、`npm run check`、`npm test`、`npm run build` | 类型/运行时草稿测试/构建通过；3 项单测通过 |
| 可重复发行入口 | `make server-build` | npm ci、类型检查、Vite、复制 assets、Go `web_release` 构建全部通过 |
| 缺资产 | 暂时移走生成的 index.html，再执行 `go build -tags web_release ./internal/webassets` | 预期失败：`pattern dist/index.html: no matching files found`；文件通过 trap 恢复 |
| 引用资产缺失 | `TestWebProductionCookieAndConfiguration` 删除虚拟 FS 中入口引用的 JS | 拒绝启用 Web |
| 浏览器最终候选 | 容器内 `TEST_DATABASE_URL=… npm run test:browser` | 5 项通过，25.6 秒 |
| 最后时区修正 | 重新前端类型检查/构建、Go 发行构建，再以 `npm run test:browser -- --grep 版本冲突 --output=test-results/timezone` 定向回归 | 通过；旧目标 `+08:00` 截止时间正确换算为 UTC 显示，冲突/重启/退出仍通过；复用未改变的其余四项浏览器结果 |
| 依赖与改动 | npm ci 的 audit、`git diff --check` | 零已报告漏洞；无空白错误 |

## 用户结果与证据

1. 浏览器通过一次性配对码登录，JSON 无长期 Bearer；Cookie 不能当 Bearer 使用，属性符合开发/生产配置。`TestPostgreSQLWebHTTPIdentityAndRealGoals` 使用真实 HTTP 和数据库，校验会话响应符合 OpenAPI；日志不含配对码、Cookie 值或 CSRF 秘密。
2. `TestPostgreSQLWebPairingRollsBackWholeExchange` 注入会话表写入失败，证明设备、token、会话和配对码一起回滚；同一码可重试成功，成功后不能重放。
3. Cookie 防护覆盖新旧写 API，缺 Origin/CSRF、错误 Origin/CSRF、重复 Cookie、Authorization 歧义、旧 principal/代次均拒绝。admin/internal/MCP 不接受学习身份。过期、退出、设备撤销和撤回写 scope 后禁止新写入；退出不撤销设备。过期 Cookie 清除后可重新配对。
4. 真实浏览器创建学习区、以四行输入保存无模型/无资料目标、结构化修订、开始目标状态、暂停、恢复、手动完成、归档、恢复；完成依据必填。浏览器没有请求模型或教学路径，HTTP 用例断言教学会话和学习证据均为零。
5. 浏览器生产二进制真正停止并重启，原 Cookie 和目标仍能恢复。目标写入版本冲突保留正文；读取新版本、核对后保存成功。旧目标的非 UTC 截止时间正确换算为编辑框标注的 UTC 时间。服务重建后的跨区目标读取返回 404，不回落默认区。
6. 同区双目标、两个标签页和两个学习区保留独立输入。人为中止保存请求后保留正文；延迟旧区保存响应，在新区输入后释放旧响应，新区 URL 和输入保持不变。
7. 浏览器操作 12 个真实目标的分页、搜索、状态筛选与空状态；打开修订历史，编辑/归档/恢复学习区。未就绪能力用独立提示展示，页面不发送目标 API 请求。能力展示用例只替换 capability 响应，不计业务持久化证据。
8. 首页与详情页在浅色/深色、390/768/1280/1440 CSS px，共 16 个场景中无整页横向溢出。axe 使用 WCAG 2/2.1/2.2 AA 标签检查真实渲染（含对比度），没有违规报告；确认弹层默认聚焦取消，Tab/Enter 可操作，IME composition Enter 不触发保存。不是仅以颜色表判断合格。
9. localStorage 只有主题偏好，document.cookie 读不到 HttpOnly 会话。草稿/重试身份仅内存，失效与退出清理。隐私 marker 夹具新增会话行，identity owner 删除会话并将其数量加入残留校验。
10. 真实 Go 响应 `/app/` 与页面路由、JS；未知 API、缺失资源返回真实 404，不回 SPA。发行携带真实 Vite 产物；原本机管理配置测试仍通过。提供只转发学习路径的 HTTPS 代理示例，默认不开启新的公网监听。

浏览器截图位于被 Git 忽略的 `clients/web/test-results/learning-真实配对、IME-保存、生命周期及深浅主题四个视口/`，包含 `home-light-390.png` 等首页和详情页证据。截图仅含本任务测试数据；不将截图、二进制、node_modules 或临时会话记录提交到仓库。

## 限制与未运行项

- 未运行完整 Compose/OCI 镜像构建、实际公网 TLS/Nginx 部署、真实外部模型/搜索/Nocturne 端到端、Firefox/WebKit、原生操作系统输入法或全项目 race。当前交付不宣称这些环境已验证；浏览器 IME 证据为 Chromium composition 事件回归。
- 自动可访问性检查与定向键盘验收不等于完成全部 WCAG 人工审计。
- OpenAPI 生成器对已有、此 Web 不使用的 `ActionExposureRequest` discriminator 报告警告；Vite 对 Zod 内部注释及约 565 kB 的 JS chunk 发出非阻断提示（gzip 约 180 kB）。没有隐藏警告或放宽相关契约。
- 自动研究、教学启动、参考绑定按工单非目标保留明确禁用，不创建虚假进度。正式 Go API 可在后续能力接入时扩展。
- 按本任务独立 worktree 的落地约束，仅提交当前分支供审查，不推送或合并基线分支。
