# 知络浏览器学习入口

浏览器学习入口为 `/app/`，与 Go API 同域。此批提供目标和学习区管理；保存不会调用模型或创建教学会话。自动研究/教学与参考绑定入口根据服务端 capability 禁用。

## 本机开发

需要 Node.js 22.12+、Go（版本以 `server/go.mod` 为准）和独立 PostgreSQL。

```bash
make server-build
DATABASE_URL='postgres://用户:密码@127.0.0.1:5432/数据库?sslmode=disable' \
MIGRATE_ON_START=true WEB_UI_ENABLED=true WEB_UI_ALLOW_LOOPBACK_HTTP=true \
LISTEN_ADDR=127.0.0.1:8080 PUBLIC_BASE_URL=http://127.0.0.1:8080 \
./server/edu-agentd serve
```

通过同一 `DATABASE_URL` 运行 `./server/edu-agentd pairing-code create`，或使用既有本机管理页获取一次性配对码。在 `http://127.0.0.1:8080/app/` 输入配对码，设置浏览器名称后即可使用。模型、搜索和资料都不是前置。

`make web-build` 安装锁定依赖、检查类型、生成 Vite dist，并复制到 `server/internal/webassets/dist`。`cd clients/web && npm run dev` 监听前端源码并在每次构建后复制资源；重启 `go run ./cmd/edu-agentd` 后 Go 会嵌入最新资源。开发流量始终经过 Go 的同域 Cookie/CSRF 路径。没有跨域 Vite 开发服务器或代理身份。

若只检查 Go 代码且不启用 Web，可直接运行 Go 命令；启用 Web 但没有真实资产时服务启动失败。发行必须用 `make server-build` 或 `go build -tags web_release`，缺少入口或 assets 会在编译时失败。Dockerfile 也先构建真实前端再嵌入 Go。

## HTTPS 部署

设置 `WEB_UI_ENABLED=true`、`PUBLIC_BASE_URL=https://你的域名`，关闭 `WEB_UI_ALLOW_LOOPBACK_HTTP`。保持 Go 监听 loopback，使用 [Nginx 白名单示例](../../deploy/web/nginx.conf) 终结 TLS，只暴露 `/app` 与本次学习 API；`/admin`、`/internal`、`/mcp` 均不公开。不要把完整服务端口绑定到公网，也不要将旧管理代理信任配置用于公开入口。现有管理网络配置和独立管理凭据要求不变。

学习 Cookie 为 `__Host-edu_web`，Secure、HttpOnly、SameSite=Strict、Path=/、无 Domain。loopback HTTP 开发例外采用 `edu_web_dev`，必须单独启用。配置必须使用访问页面的精确 scheme/host/port，HTTP 层不根据转发头推断 Origin。学习 Cookie 与 Authorization 同时存在时拒绝；所有使用学习 Cookie 的非安全请求都校验 Origin 和 `X-CSRF-Token`。

浏览器会话固定有效 12 小时，存储于 PostgreSQL，可跨服务重启恢复。学习设备只获得配对权限范围中的 `learning:read/write`；每次请求读取实时权限、设备撤销、过期与隐私代次。退出只删除当前会话。管理页与学习页同源时，携带学习 Cookie 的管理请求会被拒绝；使用独立浏览器上下文访问管理面，或先退出学习会话。

## 状态与隐私

已保存目标是服务端事实；选择属于每个标签页 URL；未提交正文和幂等重试身份只在标签页内存。刷新前提示会丢失草稿，退出或身份失效清除查询缓存和草稿。localStorage 仅保存主题；没有明文答案、Token 或 Key。

新迁移 `000020_web_identity.sql` 只追加会话表，保存 Cookie 摘要、关联设备 token、代次和到期时间。长期设备 Token 不返回脚本，也不保存在 Web 会话表中。identity 隐私 owner 删除全部新增会话，并将残留会话数纳入校验。

查询与回调固定绑定 origin、设备 principal、隐私代次、空间和目标；历史查询带目标修订版本。业务请求先确认当前浏览器身份，并发送 principal/代次头防止检查与发送之间的身份切换。旧身份失败回调不会清除新身份。版本冲突保留输入，用户可读取最新版本后核对再保存；相同载荷重试复用 operation_id，修改载荷生成新操作身份。

## 契约与检查

`npm run generate` 从服务端 OpenAPI 生成类型，`openapi-fetch` 将其用于实际请求；Zod 对表单、会话、目标和分页响应进行运行时校验。生成器会提示既有教学 `ActionExposureRequest` discriminator 警告，此 Web 不使用该路径；没有放宽相关契约。

```bash
cd clients/web
npm ci
npm run generate
npm run check
npm test
npm run build
TEST_DATABASE_URL='独立测试数据库 URL' npm run test:browser
```

浏览器检查需要与锁文件一致的 Playwright Chromium；可以设置 `WEB_CHROMIUM_PATH` 使用已有兼容浏览器。测试启动生产 Go 二进制、真实配对和使用 PostgreSQL，保留各主题/视口截图到被 Git 忽略的 `test-results/`。数据库应独占，HTTP/隐私集成测试和浏览器检查串行运行；不要连接生产数据库。宿主缺浏览器依赖时可使用版本匹配的 Playwright 容器。

设计与验收见 [Web 入口设计](../../docs/design/web-learning-entry.md) 和 [Issue #18 验收记录](../../docs/development/issue-18-acceptance.md)。
