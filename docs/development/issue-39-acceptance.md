# Issue #39 浏览器加密离线学习验收

验收日期：2026-09-19。实现前先完成[开发方案与验收标准](../design/browser-offline-learning.md)。

## 缺失确认

基线 `a5bf595`，工作区原先无修改。此问题是缺失的浏览器能力，而非 CLI 离线故障。

- `clients/web/src/main.tsx:23`：所有页面在在线 SessionProvider 下，没有离线路由。
- `clients/web/src/lib/session.tsx:128、142、186`：启动必须请求在线身份，无法获得身份时只呈现配对。
- `clients/web/src/lib/drafts.ts:1`：明确规定草稿仅在标签页内存；刷新不能恢复答案。
- `clients/web/vite.config.ts:7`：只有资产构建/复制，没有离线壳或持久化机制。
- 在基线 Web 源码检索 `indexedDB|serviceWorker|offline`，没有加密离线学习实现，
  也没有可替代的已有设置。现有服务端 `learning/offline_service.go:83、202、248`
  已具备签发、同步和状态入口，原 CLI 也有加密离线库；不能据此宣称浏览器已经支持。

上述代码链足以确认缺失，未把正常的在线内存草稿改为隐式持久化。

## 实现范围

- 新增 `/app/offline`，在线课堂提供明确学习区/目标/会话下载入口。创建库与下载均需
  明确操作；冻结原响应、签名、授权和活动版本。支持 objective/open 无帮助文本答案，
  未知作答协议只读，绝不本地评分、写 Evidence 或推进在线会话。
- 使用既有 offline owner；仅新增浏览器适配 API、默认关闭的 `WEB_OFFLINE_ENABLED`
  及迁移 `000033_browser_offline.sql`。专用 HttpOnly Cookie 固定原设备、代次，
  只能访问离线合同；最多 37 天，仅明确下载续期。撤销、实时 scope、授权期限仍由服务器裁决。
- 口令包装随机 AES-256-GCM 密钥；正文、包清单、队列和回执为完整认证加密快照。
  strict IndexedDB 提交后回读才显示保存；Web Locks 防止跨页重复消费，密钥共享租约
  阻止其他页面尚未释放密钥时虚报清除。刷新/重启需解锁，15 分钟自动锁定。
- 发送前持久化“结果未知”；重连先查原 operation，再发送允许的新记录。原 submission、
  operation、序号和签名 payload 字节不改写；正式回执与 Evidence 状态分别显示。
- Service Worker 只缓存公开构建白名单及固定离线壳，安装不带凭据，逐文件核对摘要；
  不拦截 API、任意页面、外部来源或带额外查询参数的资产。
- 全局清除后，普通在线身份删除不妨碍原设备核对 purge/ack。先清除库、答案、索引、
  队列、缓存及密钥，再提交回执。ack 响应丢失后可查询原设备原代次的已完成回执；
  没有正式回执仍显示待完成。本地手动清除不删除服务端学习事实。

## 实际验证

环境：Linux amd64；Node.js 24.20.0；Go 1.26.6；PostgreSQL 17；
Playwright 1.62.1；Google Chrome for Testing 153.0.8010.12。
使用本任务独立 PostgreSQL 容器和独立数据库，不使用生产服务或外部模型。
浏览器数据和回执来自真实 Go HTTP 服务、真实签发器和 PostgreSQL；仅模型内容为本地固定夹具。

| 检查 | 结果 |
| --- | --- |
| `npm run check`、`npm test` | 16 个测试文件、40 项通过；涵盖 AES/AAD、错误口令、签名链轮换、大整数、原始授权字节、未知协议、回执状态组合及壳缓存边界 |
| `npm run build`、`go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 真实资产构建及嵌入成功，含离线 Service Worker |
| 真实浏览器 4 项离线验收 + 1 项原在线入口回归 | 全部通过，44.3 秒；在线回归含配对、IME、主题和四个视口 |
| 受影响 Go 包、迁移和 OpenAPI 测试 | 通过；未配置数据库的通用包运行不冒充 PostgreSQL 集成验证 |
| 受影响 Go 包 `go vet` | 通过 |
| 真实 PostgreSQL 离线、身份、隐私回归 | 通过；涵盖原 owner 幂等、序号冲突、双期限、撤销竞态、冻结内容、写入故障回滚、评估接纳及清除合同 |
| 原 CLI 离线回归 | `internal/command`、`internal/api`、`internal/offline` 三包通过，无 CLI 代码变更 |
| `git diff --check` | 通过 |

浏览器场景具体断言：

1. 真实签名包断网阅读、保存答案；关闭并重启持久配置 Chromium 后断网解锁恢复。
   改变页面学习区/目标参数不改变旧包归属，同步不附带当前学习区头。服务端提交后故意
   丢失同步响应，随后调用序列只有 `sync → status`，没有第二次提交；显示正式 Evidence，
   在线会话与原值完全一致。存储中不含测试正文或口令，缓存只含公开资产。
2. 在真实 IndexedDB 写入边界注入配额异常：不显示已保存且保留未保存输入；两标签页
   同时提交只保存一个原答案；锁定跨页生效。删除快照后明确报部分丢失，不能继续解锁。
   确认清除未知数量内容后数据库、存在标记及离线缓存均不存在。
3. 持久化许可拒绝、配额不足及缺少 BroadcastChannel：明确失败，不创建离线库、
   不明文降级。正向持久化场景使用真实 Chromium durableStorage 权限，不模拟成功。
4. 设备真正断网时，从独立身份提交真实隐私屏障；旧页面仍能读，不宣称远程即时擦除。
   重连两页清除全部受管数据，正式 ack 在服务端提交后故意丢失响应。刷新核对原回执
   成功；旧代次正文同步返回 409，不能复活已清除内容。

故障注入用于确定性验证 API 边界，不等价于宣称已经测试真实磁盘损坏或操作系统逐出。
安装混合版本的场景由运行实际生成 Service Worker 代码的测试验证：摘要不符时安装失败、
删除未完成缓存；敏感 API 与外部 URL 不被拦截。

## 复验命令

先设置独立 `TEST_DATABASE_URL`。浏览器清除测试会清除该库内学习数据，必须使用全新的
专用数据库；测试签发密钥每次启动随机生成，不用于复用历史测试包。依赖按锁文件安装。

```bash
cd clients/web
npm run generate
npm run check
npm test
npm run build
cd ../../server
go build -tags web_release -o edu-agentd ./cmd/edu-agentd
go test -p=2 ./internal/transport/httpapi ./internal/identity/... ./internal/platform/config ./internal/app ./migrations
go test ./api
go vet ./internal/transport/httpapi ./internal/identity/... ./internal/platform/config ./internal/app ./internal/privacy/postgresstore

# 以下命令需要已设置 TEST_DATABASE_URL，实际验收已带入本任务独立 PostgreSQL。
go test -p=1 -count=1 ./internal/identity/postgresstore ./internal/platform/config ./internal/learning/postgresstore ./internal/privacy/postgresstore -run 'WebOffline|Offline'
go test -p=1 -count=1 ./internal/transport/httpapi ./internal/identity/postgresstore ./internal/privacy/postgresstore -run 'TestPostgreSQLWeb|TestOfflineDevicePurgeReceipt'

cd ../clients/web
WEB_WORKSPACE_FIXTURE=1 WEB_OFFLINE_FIXTURE=1 WEB_CHROMIUM_PATH=/绝对路径/chrome \
  npm run test:browser -- offline.spec.ts learning.spec.ts \
  --grep '真实配对、IME|签名包|存储故障|没有持久|真实断网'
cd ../cli-go
go test ./internal/command ./internal/api ./internal/offline -run 'Offline|Canonical|Seal|Open|Purge' -count=1
```

`npm run generate` 保留既有 `ActionExposureRequest` discriminator 警告，本次未改相关合同；
类型生成和 OpenAPI 验证均成功。未运行与本功能无关的整仓浏览器套件或所有 Go 集成包。

## 发布与恢复边界

目前只声明上述桌面 Chromium 组合通过验收。Firefox、Safari、真实 Android/iOS 软键盘、
操作系统级断电/逐出尚未验证，不宣称对应浏览器已经具有可靠离线学习能力。
能力不足、隐私模式或存储权限拒绝时拒绝保存；细节见[使用说明](../../clients/web/README.md)。

用户清空整站数据和标记后无法凭空识别历史，界面明确提示潜在丢失；口令/密钥丢失无恢复，
本版本采用丢失提示而非导出恢复。不支持跨设备接管，不承诺浏览器永久保存、系统钥匙串、
端到端加密或取证擦除。身份丢失、到期、撤销依原合同拒绝同步，不能借新设备冒充旧设备；
可明确清除本地数据，但无正式回执不能宣称服务器清除流程已完成。

开发验收阶段仅提交工作分支供审查；用户接受任务后另行授权合入及推送主线。

## 用户验收后的主线整合

合入基线为 `f74ae70`，保留主线新增的导师历史、进度复习、数据管理及 companion 能力。

手动解决的冲突单独记录如下：

- `clients/web/README.md`：保留离线学习与 companion 两份使用说明。
- `clients/web/src/main.tsx`、`components/workspace-shell.tsx`：合并双方路由、导入和导航；仅离线页绕过在线身份入口。
- `clients/web/src/styles.css`：保留离线及主线新增页面的全部样式。
- `server/internal/platform/config/config.go`：同时保留离线和 companion 开关及校验。
- `server/internal/transport/httpapi/web.go`：静态页面白名单同时保留双方新增入口。
- `server/migrations/migrations_test.go`：保留主线 31、32 迁移检查，新增离线 33 检查。

另外解决迁移编号碰撞：本任务尚未落地主线的离线迁移从 31 顺延为 33；主线已有 SQL
内容和校验和均未修改。生成类型只叠加 396 行离线合同，保留主线原有知识集合请求头，
不夹带其他功能的历史生成物更新。

合入后重新验证：21 个 Web 测试文件、52 项单测通过；四项真实 Chromium 离线场景通过；
迁移、配置、HTTP 和应用包通过，离线 OpenAPI 精确测试通过；三个 PostgreSQL 包的
Web 离线身份与原设备清除回执测试通过。直接 Vite 打包及 Go 发行构建成功。

主线既有失败保留并明确区分：

- Web 类型检查：主线 `teaching-page.tsx:501` 的 `generate` 类型没有 `present_review`，
  但主线 871 行已传入该值；合入后对应错误位于 881 行，本任务仅新增下载入口。
  因此不把本轮 `npm run check` 或包含它的 `npm run build` 宣称为通过。
- `TestWebReleaseProxyCoversClientAPI`：companion 的 `/v1/companion/browser` 缺少部署代理
  白名单；已在原 `main` 检出执行同一精确测试复现。本次没有修改 companion 或代理规则。

原任务更早的通过记录仍对应当时基线；未因主线已有问题扩大本次合并的产品范围。
