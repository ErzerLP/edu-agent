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


## 远端记录浏览器补验（2026-10-11）

从干净的 `main@1f883ef` 新建 `issue-34-memory-remote-browser`。上面的“尚未验收”
保留原交付时的历史状态；本节结合后续证据给出最终结论。已读取 GitHub #34 正文、
11 项验收清单及评论；本次只补剩余浏览器验收，不修改 GitHub 状态。

变更限于 `clients/web/scripts/nocturne-fixture.mjs`、`test-server.mjs`、
`clients/web/tests/browser/memory.spec.ts` 和本记录，没有修改前端产品源码、Go、
公共 API/OpenAPI、migration 或用户可见语义。

### 夹具与正式服务边界

`WEB_NOCTURNE_FIXTURE=1` 启动 loopback `32932` 的确定性 HTTP 远端，测试服务的
Nocturne 配置指向它。协议参考 `server/internal/integrations/nocturne/client.go`
和 `bridge_test.go` 的 `strictHTTPFake`：健康、固定 capability/boot epoch、API 与
维护令牌隔离、namespace/domain、节点读写、完整 references、搜索、orphan 历史与
永久删除响应均按真实客户端字段提供。每次修改产生历史版本，删除先移除路径，再
逐版本永久删除；测试用夹具统计观察远端状态，不直接修改 PostgreSQL。

测试仍经过真实 Go 服务、独立 PG schema、Cookie/CSRF、正式审批、outbox consumer、
内容哈希回读、删除对账和回执。`/fixture` 只控制远端可用性、一次能力探测 401 及
读取统计。一次维护鉴权故障发生在业务写入发送前，使原 outbox 成为可正式重放的
`dead`；重放恢复原意图，无 SQL 改状态、审批 API 模拟或第二套交付逻辑。

夹具的公开测试 API/维护令牌与包装 Key 是 canary；503/401 原始错误正文故意包含
它们，浏览器下载、正式错误/回执、页面和真实服务日志均检查不含这些值或凭据字段。
服务日志在权限 `0600` 的临时文件中保存。备份接口仅提供空 inventory，启动所需
备份目录为临时 `0700` 目录；`pg_dump` 配置指向不可用的本地地址，本次没有备份
生产/恢复验收。日志中的 `backup_producer` / `backup_prune` 的脱敏 `unavailable`/`deferred`
告警属于该夹具限制，真实备份及物理清除证据采用下述 #67 已通过结果。

### 按 Issue 验收项的最终结论

| #34 验收项 | 最终结论与证据 |
| --- | --- |
| 1. 候选创建、批准/拒绝与真实记录的 Web/Agent 链路 | **通过。** 保留原浏览器的逐条审阅、取消、拒绝、移动焦点及导师内嵌审批；本次还核对批准后的远端正文、正式记录与哈希验证回执。三引擎完整文件通过。 |
| 2. 临时要求不默认保存，聊天不构成广泛授权 | **通过（复用已有证据）。** 本记录原 `TestPostgreSQLMentorMemory` 集成已覆盖临时要求、普通“好”、取消及撤权不准入；本次原导师浏览器场景仍要求内嵌候选明确批准。未扩大授权语义。 |
| 3. 纠正、删除、导出与交付重放走原服务/权限 | **通过，本次补齐。** 新场景“已交付记忆的纠正审批、正文导出与完整远端删除”及“远端一次维护故障后浏览器重放沿用原交付且不重复写入”，细节见下一节；使用正式 Cookie/CSRF 与原服务门禁。 |
| 4. 实际全局范围，不虚构分区隔离 | **通过。** 原场景保留“未按学习区隔离”断言；成功导出仍携带实际全局范围。沿用 [设计边界](../design/web-memory-privacy.md)，没有新增多域或跨区许可。 |
| 5. 个性化有来源，不以 Nocturne 作为掌握度真值 | **通过（复用已有证据）。** 原 PG/consumer/exporter 及导师集成已验证来源可见、远端正文不复制 checkpoint、删除/撤权阻止发送；本次记录详情仍展示准入来源链接。没有修改学习事实或掌握度权威。 |
| 6. 各类删除/归档/移除/清除行为准确 | **通过（已有与本次合并）。** 原 [设计](../design/web-memory-privacy.md) 和数据页明确动作范围；本次单条记忆删除验证路径及两个远端版本全无、本地 `deleted`、回执成功，刷新后正文不恢复；全局清除仍走独立一次性 grant。 |
| 7. 隐私清除真实回执与失败 | **通过。** [#47 验收结果](issue-47-acceptance.md#验收结果) 已补齐等待原清除响应、本地逐 owner 结果、重新配对与同一回执；本次保留该浏览器场景，三引擎通过。远端全链路采用 [#67 验收结果](issue-67-acceptance.md#验收结果)。 |
| 8. 清除后旧运行/历史/checkpoint/引用不能恢复或重发 | **通过（复用已有证据）。** 原真实 PG owner/导师集成已覆盖；[#67](issue-67-acceptance.md#验收结果) 的原始 Compose `full` 成功，包含密钥销毁与销毁后恢复拒绝。本次不把空备份 fixture 当作真实备份清除证据。 |
| 9. 设备撤销即时失效，普通会话无 admin 权限 | **通过。** 原 Go 身份/权限及活动运行回归提供服务端证据；原浏览器设备场景继续核对被撤销设备的新会话请求 401、新身份及清除代次，三引擎通过。未开放 admin。 |
| 10. 导出/错误/日志无 Key，危险确认与移动焦点 | **通过，本次补齐成功导出。** 下载含纠正后正文、`degraded=false` 和原成功回执，旧正文不进入导出；含 canary 的上游错误没有泄漏到正式错误、回执、页面或服务日志。原危险操作取消焦点与 390/768/1280 宽度断言保留，新增批准、导出、删除、重放也经过二次确认。模型/搜索 Key 沿原 DTO 排除合同，本次不使用真实 Key。 |
| 11. 真实 PG、确定性远端与浏览器通过，真实 Nocturne 单列 | **通过（合并证据）。** 本次三引擎完整文件及 WebKit 额外三轮均使用真实 PG/正式服务/确定性远端。真实 Nocturne Compose `full` 已由 [#67](issue-67-acceptance.md#验收结果) 通过，锁定平台 manifest 为 `sha256:8e427437d35e603cb603608a124ff14344031fcc352435098b4275fa16119d45`；本次未启动 Compose。 |

### 四个剩余缺口的浏览器证据

1. **纠正闭环通过。** 先批准并等待原记录 `applied`，正文可读且“提出纠正”可用。
   表单预填真实原文，创建纠正候选后远端写入数、本地正文和远端正文均不变；明确
   批准后同一逻辑记录版本加一，同一远端 node UUID 正文更新，产生第二个历史版本，
   新回执 `succeeded / nocturne_uri_content_sha256` 与原回执不同。
2. **已交付删除通过。** 浏览器确认删除后刷新正式回执，记录为 `deleted`、正文
   `unavailable`，回执为 `succeeded / remote_logical_delete_verified /
   nocturne_complete_reference_absence`；远端路径及该 node 的两个历史版本均为空。
   刷新页面仍显示已验证删除、纠正与删除按钮禁用，正文不恢复。按原合同，删除的
   `delivery.public_status` 是 `rejected`（退出可用记忆集合），删除成功以记录状态
   和完整引用缺失回执判断，没有把该状态错当保存的 `applied`。
3. **正文导出通过。** 真实浏览器下载包含纠正后已交付正文及对应成功回执，
   `degraded=false`，没有旧正文或 canary/凭据字段。原降级场景保留：启用远端时
   先交付一条独立对照，再让远端返回 503；待交付记录仍不可纠正、导出不含其正文，
   已交付对照读取失败使整页 `degraded=true`。未启用夹具时仍验证原
   `nocturne_not_configured` 场景；没有改变 exporter 的既有降级定义。
4. **重放与不重复写入通过。** 一次写入前维护能力探测 401 后，真实记录仍为
   `queued`、回执 `pending`、尝试一次且远端写入数不增加。浏览器确认“重放交付”
   得到正式 202，原逻辑记录 ID、交付 ID 和版本保持；最终 `applied` 且正文可读。
   使用该浏览器请求的原 Cookie/CSRF/operation ID 重发得到 200、`replayed=true`；
   已完成交付用新 operation ID 重放得到 409。远端只增加一次写入、一个版本；
   完成后按钮禁用，成功导出与错误/回执/日志均无 canary。

Playwright JSON 内附带“已交付记忆正文导出”“纠正与删除正式回执”和
“故障与重放原身份核对”三个附件，内容包含实际正式回执与远端状态快照。

### WebKit 首轮不稳定的根因与处理

原基线的五条 `due to access control checks` 均指向首页的后台请求：学习区切换、
进度、复习、开始学习能力和首页学习区列表。标题已出现不代表这些请求已结束；
配对后立即硬导航会取消它们。

临时诊断对首页请求加入 700ms 延迟，并记录 `requestfailed` 与 `pageerror`。
未修复时复现同类 pageerror，其中学习区 `limit=100` 请求同时记录
`Load request cancelled`，原 `expect(errors).toEqual([])` 失败；没有得到真实 CORS
拒绝证据。配对帮助函数增加 `waitForLoadState('networkidle')`，只在没有活动订阅的
首页等待有限请求稳定后离开；同样的延迟诊断随后 1/1 通过。未过滤 pageerror、
未修改 CORS/产品请求、未延长原断言超时；临时 route/诊断代码已移除。

原始失败/新远端缺失基线在 `red.log` / `red.json`，同延迟修复验证在
`navigation-fixed.log`。远端缺失的新增断言按预期失败于纠正按钮禁用，接入夹具后
通过。新增测试调试期间修正了备份目录配置、审批响应码、预填文本控件的角色定位、
尝试计数和删除回执断言；这些是夹具/测试问题，没有产品修复。

[#47 的历史导师快照超时](issue-47-acceptance.md#待复核的-webkit-导师更新问题)
没有被推断为同一原因。本次原导师场景在当前基线的完整文件三引擎与后续 WebKit
复验通过，提供现状证据，不改写 #47 当时的失败记录。

### 命令与结果

复用已准备的 `edu-agent-acc-postgres`、Web 构建与 `server/edu-agentd`、既有 schema
包装工具和 Playwright 三引擎。固定测试端口 `32929`、模型 `32930`、远端夹具
`32932`；所有组串行，每组独立随机 schema，结束后只删除该组 schema。
未操作其他项目容器。以下是实际逐组执行的等价命令：

```bash
cd clients/web
export TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:32995/edu_agent_test?sslmode=disable'
export WEB_RELEASE_MATRIX=1 WEB_MENTOR_FIXTURE=1 WEB_NOCTURNE_FIXTURE=1
export WEB_CHROMIUM_PATH='/home/yunyue/.cache/ms-playwright/chromium-1247/chrome-linux64/chrome'
issue34_database=/tmp/edu-web-baseline-6fe9a7a.5ZH8lk/webdatabase
issue34_evidence=/tmp/edu-issue34-browser

for issue34_browser in chromium firefox webkit; do
  PLAYWRIGHT_JSON_OUTPUT_FILE="$issue34_evidence/$issue34_browser.json" "$issue34_database" npm run test:browser -- memory.spec.ts --project="$issue34_browser" --reporter=line,json --output="$issue34_evidence/artifacts/$issue34_browser" > "$issue34_evidence/$issue34_browser.log" 2>&1 || exit "$?"
done
for issue34_repeat in 1 2 3; do
  PLAYWRIGHT_JSON_OUTPUT_FILE="$issue34_evidence/webkit-repeat$issue34_repeat.json" "$issue34_database" npm run test:browser -- memory.spec.ts --project=webkit --reporter=line,json --output="$issue34_evidence/artifacts/webkit-repeat$issue34_repeat" > "$issue34_evidence/webkit-repeat$issue34_repeat.log" 2>&1 || exit "$?"
done
```

| 完整文件运行 | 结果 | 耗时 | 退出码 |
| --- | --- | --- | --- |
| Chromium | 5 passed，0 failed/skipped/retries | 27.8 秒 | 0 |
| Firefox | 5 passed，0 failed/skipped/retries | 32.6 秒 | 0 |
| WebKit | 5 passed，0 failed/skipped/retries | 27.9 秒 | 0 |
| WebKit 连续复验 1 | 5 passed，0 failed/skipped/retries | 29.3 秒 | 0 |
| WebKit 连续复验 2 | 5 passed，0 failed/skipped/retries | 30.1 秒 | 0 |
| WebKit 连续复验 3 | 5 passed，0 failed/skipped/retries | 29.2 秒 | 0 |

核对六份最终 JSON：每份 5 个场景均单次通过，0 收集错误、0 跳过、0 重试；最终
共 30 次场景执行通过。每引擎通过一次即停止，WebKit 只额外执行用户要求的三轮。
中间失败日志保留为 `chromium-initial.*`、`chromium-setup.*`、`chromium-flow.*`；
最终有效结果为表中六份 JSON，临时调试结果不计通过。

以下最小检查通过：

```bash
# 仓库根目录
node --check clients/web/scripts/nocturne-fixture.mjs
node --check clients/web/scripts/test-server.mjs
node --check clients/web/tests/browser/memory.spec.ts
git diff --check
```

没有修改前端产品源码或 Go 代码，因此按本次验证范围未执行 `npm run check`、
`npm test` 或 Go 包测试；#47 的类型/单测/构建结果只作已执行的历史证据引用。
未运行全量浏览器矩阵、Go 全量、race 或 Compose。已使用的现有构建没有因本次
测试/夹具修改而变化。运行日志另有既有 `NO_COLOR` / `FORCE_COLOR` 提示，未为此
修改环境或配置。

日志、JSON 内嵌附件、浏览器产物与归档服务日志均在本机
`/tmp/edu-issue34-browser`，服务日志分别为 `chromium-server.log`、
`firefox-server.log`、`webkit-server.log`、`webkit-repeat{1,2,3}-server.log`。
这些本地证据不纳入提交。schema 已由隔离工具清理，测试服务与夹具已退出。

本次没有发现需要产品修复的问题；Issue #34 原浏览器缺口 1–4 已补齐，结合本记录、
#47 和 #67 的已有证据，11 项验收均有对应结论。本次范围内没有未解决验收项；
夹具只覆盖单 node、单 managed path、两个历史版本和写入前一次故障，不替代真实
Nocturne 的备份或多别名/并发故障验收。真实远端 `full` 采用 #67 证据，不重复运行。
没有推送、建 PR、关闭或修改 GitHub issue。
