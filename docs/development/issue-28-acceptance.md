# Issue #28 验收记录（2026-09-17）

## 问题确认

基线 `fd037ca` 的 Web 路由未注册 `/app/runs`；全文检索 Web 源码的 `import-jobs`
只命中生成类型，没有实际调用、页面或替代开关。服务端和 CLI 已有完整导入任务合同。
根因是 Web 接入与原运行历史列表缺失，具体架构、方案和标准见
[任务中心设计](../design/web-task-center.md)。开发基线也没有 #27 的 Web 共享组件，
因此新增本流程所需的共享清单、身份审阅和差异视图，正式规则仍由原导入服务处理。

## 当前实现

- `/app/runs`、原运行详情及原导入任务详情；学习区/集合/类型筛选、各 owner 分页。
- 1000 文件/128 MiB 清单、4 MiB 单文件、base64 分段、原任务来源摘要核对及缺失补传。
- 完整计划审阅/批准、逐批继续、原操作对账、取消保留已发布结果，关闭页面停止观察。
- mentorrun 原状态列表查询，设备/学习区/代次门禁；更新能力、OpenAPI、代理白名单和说明。
- 显式 `import` 配对档案，只有携带新 `imports:web` 标记的授权设备在浏览器保留资料写入/
  审批；旧设备不增权。此缺口由 `identity/web.go` 的原权限过滤规则确认。
- 新增单测及真实浏览器断线/服务重启/超单请求容量/取消/归档/跨设备验收。
- Go 页面白名单支持任务详情直接打开和刷新；浏览器实测发现遗漏后补齐回归断言。
- 上传/解析计数表示历史完成事实，后续发布失败、暂存丢失和预览失效独立计数。

## 已执行证据

环境：Linux、Go 1.26.6、Node 24.20.0。独立临时 PostgreSQL 使用本机已有镜像
`pgvector/pgvector@sha256:cf134a767f474095eeba57e0117be8e568e011a63f33fbf252f14c9b760f8e6f`，
独立容器及随机测试 schema，没有操作应用数据库。

- `TestPostgreSQLTaskListUsesOriginalOwnerAndDevice`：通过，覆盖真实 owner 状态、分页、
  类型/状态筛选、跨区、伪造设备、撤销、非法筛选及列表不解密正文。
- `go test -p=1 ./internal/knowledge/postgresstore -run '^TestPostgreSQLImportJob' -count=1 -v`：
  四项全部通过，包含超 16 MiB、暂存加密/重启、丢失回执、幂等、外部版本变化、配额、
  暂存缺失、取消、过期和清理失败。
- `TestPostgreSQLMentorCookieHTTPAndSSERecovery`：通过，新增非默认区列表和缺省区拒绝检查。
- `TestPostgreSQLWebImportJobLifecycle`：通过，真实 Web Cookie、HTTP、非默认学习区与 PG
  完成导入、预览零发布、批准、单批发布、查询恢复、归档拒绝继续、取消保留结果和跨设备拒绝。
- `TestPostgreSQLWebImportProfileAndRevocation`、档案权限矩阵和命令解析测试：通过，
  收回 `imports:web` 即收回浏览器写入/审批，没有放宽原 user/agent/research 权限。
- 权限补齐后，identity、identity/postgresstore、cmd/edu-agentd 和 HTTP 四个受影响包的
  完整测试（配置真实 PG）、对应 vet 及 Go build 全部通过。
- `TestPostgreSQLMentorPrivacyBarrierClearsSavedAndInFlightTemporary`、
  `TestBarrierPersistsAcrossStepFailureAndLocalScrubResumes`：通过，列表及旧导入不能越过隐私屏障。
- 新列表和导师隐私测试 `-race`：通过。
- `go vet ./internal/mentorrun ./internal/transport/httpapi`、`go build ./...`：通过。
- `go test -p=1 ./internal/mentorrun ./internal/transport/httpapi ./api -count=1`：
  HTTP 和 API 包通过；mentorrun 完整包有一次取消时序断言失败（`runtime_test.go:470`，
  快照必须恰为 `cancelling`）。随后该 `stop` 子测试通过。最后再次执行配置真实 PG 的
  `go test -p=1 ./internal/mentorrun -count=1`，完整包通过（166.592 秒）；取消实现及其
  断言均未修改，保留首次时序失败记录供复查。
- `TestBlackBoxImportJobsProcessRestartAndLostResponse`：通过，真实 CLI/服务进程及 PG，
  原任务重启恢复、丢失 HTTP 提交响应、逐批继续与身份审阅合同无回归。首次启动因宿主机
  无 `psql` 失败；之后用临时包装命令复用已有镜像中的 psql，未安装宿主机软件。
- 直接用 Node 原生断言执行实际 `lib/import-files.ts`：17.5 MB 分段、每请求低于 16 MiB、
  跳过已发布项、先读后传、完整来源变化拒绝、结果未知停止、空文件及 UTF-8/base64 通过。
- 锁定版本 Prettier 检查新增 TS/TSX：通过。
- `git diff --check`：通过。

## 安装授权后的 Web 验收

用户授权安装后执行 `npm ci --no-fund`：安装 271 包、审计无漏洞，锁文件未变。
Chromium 启动缺少 `libasound.so.2`，按授权安装 `libasound2t64` 及其数据包后运行，
不将此前启动失败算作浏览器验收。

- `npm run generate`：完成。生成文件还同步了基线中已经存在但未生成的内容/变更 API，
  没有手改生成器输出。原有 TutoringActionRequest discriminator 警告仍存在。
- `npm run check`：通过。显式标注既有内容编辑器的 session ID 为 `string`，避免随机 UUID
  推断为模板字面量后不能接收服务端字符串；不改变运行行为。
- `npm test`：9 个文件、24 项测试全部通过。原有两项 Node 环境测试补齐与其他测试相同的
  window origin 夹具，不修改生产浏览器客户端以迁就测试。
- `npm run build`、`go build -tags web_release -o edu-agentd ./cmd/edu-agentd`：通过。
  构建保留 Zod 注释与大于 500 kB bundle 的非阻断警告，本项不做无关打包重构。
- 新页面白名单补齐后，真实 PG 的 `go test -p=1 ./internal/transport/httpapi ./api -count=1`
  两包完整通过。
- 最终在新的空测试库执行以下命令，**4 项全部通过，0 跳过（51 秒）**：

  ```sh
  WEB_WORKSPACE_FIXTURE=1 WEB_MENTOR_FIXTURE=1 TEST_DATABASE_URL=独立空测试库 \
    npm run test:browser -- tasks.spec.ts mentor.spec.ts workspace.spec.ts \
    -g '大清单分段|取消保留已发布|真实浏览器导师|选段模型加工'
  ```

  覆盖约 18 MB、5 文件一次任务，每次请求小于 16 MiB；上传与发布分别丢失响应；原任务
  刷新及服务进程重启；未上传来源重选、摘要变化拒绝；最终 5 个 operation、5 个 revision，
  无重复发布；取消保留结果、归档拒绝继续、跨设备拒绝；390/768/1280 宽度与浏览器存储边界。
  同时验证原运行类型/状态筛选、关闭详情不取消、重进继续回应，以及内容运行跳回正式版本。
  研究等类型的原 owner 状态与分页、设备和隐私门禁由上述真实 PG 列表测试覆盖。

浏览器夹具每次启动会生成新密钥，复用旧库曾导致旧正文解密失败；最终使用新库，未降低
加密边界。旧选段用例的来源顺序假设改为按真实出处核对正文，配对后等待真实登录完成；
导师重启断言允许现有 10 秒优雅退出。没有跳过失败断言或冒充真实外部模型测试。

本项没有运行全部 Web 浏览器用例、真实外部模型或线上 Nginx 部署；这里的通过仅指明确列出的
本地真实 HTTP/PG/Chromium 与回归范围。初次交付仅提交到 `task/49`，没有推送、变基或合并 `main`。

## 用户验收后的落地整合

用户接受后授权将 `main`（`57640be`，#27）合入任务分支，再在基础工作树 squash 提交和推送。
手动处理 9 个文件的冲突：

- `main.tsx` 与 `styles.css`：并存知识页、参考资料样式及全部任务页，不丢失任一入口。
- `cmd/edu-agentd/main.go`、`main_test.go` 与 identity 的 `domain.go`、`service.go`、`web.go`：
  并存 `references`、`import` 两个显式档案，保留各自 scope，不给普通设备增权。
- HTTP `web.go`：同时保留原参考能力及新运行/导入能力。
- `api/schema.d.ts`：从合并后的 OpenAPI 重新生成，保留两项功能的合同。

合并后类型检查、27 项 Web 单测、identity/命令/API 测试、Go 构建和 Web 生产构建通过。
独立 PG/Chromium 上的两项任务导入用例和 #27 单批参考资料用例均通过；旧参考用例原先
在实际请求发出前解除响应拦截，已只修正该时序并精确复测通过，不改生产提交规则。
沿用此前未受冲突影响的验收证据，不重跑完整历史矩阵。
