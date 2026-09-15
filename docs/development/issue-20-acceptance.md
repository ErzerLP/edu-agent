# Issue #20 验收记录

## 确认结果

确认提交 `44a5121` 已有 #18，但没有模型/搜索配置流程。`clients/web/src/pages.tsx:647` 原设置页仅展示身份及静态能力；`server/internal/transport/httpapi/api.go:303` 原仅提供旧模型探测；`server/internal/platform/config/config.go:64` 原只有一份环境变量教学模型。根因是服务端配置 owner、独立 scope、搜索适配器和能力合同尚未实现。先完成上述代码核对及整体架构调查，再按 [开发方案](../design/model-search-settings.md) 实施。

## 已完成的验证

| 范围 | 实际检查 | 结果 |
| --- | --- | --- |
| 配置、秘密、预算、探测 | `go test ./internal/settings ./internal/integrations/websearch ./internal/identity -run 'TestConfiguration\|TestEndpoint\|TestProbe\|TestLimits\|TestEnvironment\|TestBrave\|TestSettings' -count=1` | 通过：真实文件保存/恢复、秘密权限、替换/清除、端点绑定、导师复用、范围、探测类别及 HTTP 适配器 |
| 应用启动与操作者配置 | `go test ./internal/settings ./internal/platform/config ./internal/app ./cmd/edu-agentd -run 'TestSavedTeaching\|TestLearningSettings\|TestPairingCodeProfile\|TestConfiguration\|TestEndpoint\|TestProbe\|TestLimits\|TestEnvironment' -count=1` | 通过：启动消费已保存模型；保存不切换当前连接；路径和允许列表解析；新档案 |
| 真实 API 与身份 | 独立 PostgreSQL：`go test -p=1 ./internal/transport/httpapi ./api -run 'TestPostgreSQLSettings\|TestSettings' -count=1` | 通过：普通设备只读、专用权限、Origin/CSRF、撤销/收回 scope、admin Cookie 隔离、配置恢复、OpenAPI DTO 验证、只读无外部请求 |
| 定向并发与旧 Web 回归 | 独立 PostgreSQL：`go test -p=1 -race ./internal/settings ./internal/transport/httpapi ./internal/identity/postgresstore -run 'TestConfiguration\|TestEndpoint\|TestProbe\|TestLimits\|TestEnvironment\|TestPostgreSQLSettings\|TestPostgreSQLWeb' -count=1` | 通过：并发探测与更新，真实新/旧 Web 身份及无模型目标保存 |
| OpenAPI 解析 | `go test ./api -run '^TestKnowledgeReviewAndRetrievalSchemasValidateDomainPayloads$' -count=1` | 初次新增错误 schema 名称引用失败；修正为既有 ErrorEnvelope 后通过 |
| 服务端候选 | `go test ./...` | 所有 package 除旧帮助文本断言通过；该断言需增加新 settings 档案，修正后 `go test ./cmd/edu-agentd -count=1` 全包通过。未设置数据库环境的其他 DB 测试跳过，不作为数据库证据 |
| 静态检查与构建 | 对 settings、llm、websearch、identity、config、httpapi、app、命令入口执行 `go vet`；服务端 `go build ./...`；`git diff --check` | 通过 |
| CLI 合同 | `cd clients/cli-go && go test ./internal/api` | 通过；CLI 代码与本地配置未修改 |
| 前端生成与单测 | `npm ci --no-fund && npm run generate && npm run check && npm test` | 锁文件未改变，类型生成与检查通过，2 个文件的 5 项测试通过 |
| 发行构建 | `npm run build`；`go build -tags web_release -o edu-agentd ./cmd/edu-agentd` | 通过，发行服务包含真实 Web 资源 |
| 设置页浏览器验收 | 使用本机已有 `mcr.microsoft.com/playwright:v1.62.1-noble` 容器、真实服务和独立 PostgreSQL，执行 `npm run test:browser -- settings.spec.ts` | 1 项端到端测试通过（2.5 秒），覆盖保存/恢复、Key 不回显、显式探测、预算、重启、390/768/1280px 布局及普通设备权限隔离 |

数据库使用本任务单独的 `edu-agent-task26-settings-test` 容器，既有数据库与部署未变更；镜像为本机已有 `pgvector/pgvector:pg17`，没有拉取或升级。

## 验证范围与限制

- 宿主浏览器首次启动因缺少 `libasound.so.2` 失败，随后使用本机已有的同版本 Playwright 容器完成验证，没有安装宿主系统库。
- OpenAPI 生成器仍提示既有 `ActionExposureRequest` discriminator 警告；Vite 提示依赖注释和主 bundle 超过 500 kB。这些提示未阻碍类型检查、构建或本次浏览器行为验证；未为消除无关提示扩展改动。
- 真实模型/Brave 提供商 smoke 未运行；没有为本任务配置可使用的提供商凭据和费用授权。确定性本机 HTTP 测试不冒充真实 smoke 或长期稳定性证明。
- 数据库证据只覆盖上述新设置及相关 Web 身份/目标场景，不代表其他模块的完整 PostgreSQL 矩阵已运行。教学配置保存后仍按设计采用明确的重启生效边界。
