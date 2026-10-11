# Issue #37：旧库升级、功能关闭回滚与备份恢复演练

2026-10-11，候选 `048ec1102dfa7f199fce89f5cdaa9517d889ca47`，独立分支
`issue-37-upgrade`。依据 [issue #37](https://github.com/ErzerLP/edu-agent/issues/37)、
[issue #40](https://github.com/ErzerLP/edu-agent/issues/40) 和
[验收记录的发布附加项及升级/回退要求](issue-37-acceptance.md#安装升级保存与回退)。
本次没有修改原验收记录、migration、公共 API/OpenAPI 或产品代码。

演练流程完整执行并清理了临时数据库；**发布验收仍有两项失败**：旧 CLI 的
`progress` 无法解码新服务响应；撤销/清除之前的主库原始快照恢复后，旧身份与正文
重新可用。其余已执行检查通过，不据此宣称完整旧 CLI 矩阵或 Nocturne 恢复通过。

## 基线与环境

旧版本使用 `bc55267e023bb3b5cb77f9da61d81b44d5c6257d`，2026-09-10，
`fix(tui): 修复工作台概览协议解码与失败态`。它是 #18 实现提交 `82573c7`
的父提交，也正是 issue #40 声明的 Web 工作台之前基线。仓库没有发布 tag。
没有追加 #29 前的中间版本。

```sh
gh issue view 37 --repo ErzerLP/edu-agent --json title,body
gh issue view 40 --repo ErzerLP/edu-agent --json title,body
git log --oneline --all --grep='#18'
git rev-parse 82573c7^
# bc55267e023bb3b5cb77f9da61d81b44d5c6257d
```

工作目录 `/home/yunyue/Projects/myworks/edu-agent-wt/issue-37-upgrade`。使用既有
`edu-agent-acc-postgres`，PostgreSQL **17.11**（x86_64 Alpine），宿主端口
`127.0.0.1:32995`，仅创建自己的新库。未重建容器，也未操作其他项目容器。
Go **1.27.1 linux/amd64**、Node **26.10.0**、Python **3.12.3**。
服务端使用 `127.0.0.1:33137`，模型 fixture 使用 `127.0.0.1:33138`。

当前迁移目录有 **33 个 SQL 迁移、39 个总文件**，而非任务背景中的 39 个 SQL。
旧版本应用 1–19，升级后应用 1–33，最新为 `000033_browser_offline.sql`。
脚本逐项将数据库版本/checksum 与当前 SQL 文件 SHA-256 比较，并比较原 19 条记录。

旧服务不启用 Web。当前服务启用 Web 与正文密钥；`ADMIN_UI_ENABLED=false`、
`COMPANION_ENABLED=false`、`WEB_OFFLINE_ENABLED=false`、`NOCTURNE_ENABLED=false`。
模型仅使用本地合成回答，教学响应复用
[workspace-model.mjs](../../clients/web/scripts/workspace-model.mjs)，导师响应使用合成 SSE。
没有接入真实模型或搜索。演练把 `NOCTURNE_RECONCILIATION_INTERVAL` 设为 `1s`，
让关闭 Nocturne 时的清除续跑 worker 有界完成；生产默认值是 `30s`。

## 可重复构建与执行

新增 [test-issue37-upgrade-backup.py](../../scripts/test-issue37-upgrade-backup.py)。
脚本接受预先构建的旧服务、旧 CLI、当前服务；Python 使用标准库，不安装依赖。
每次随机创建 `issue37_upgrade_<随机值>` 和同名 `_restore` 库，不接受任意数据库名。
拒绝 33100–33199 以外的服务/fixture 端口和已占用监听端口；拒绝复用证据目录。
正常结束、异常、Ctrl-C/SIGTERM 均停止自身进程并清理已创建的库。

构建步骤（从本 worktree 根目录执行；目标旧 worktree 必须不存在）：

```sh
OLD_TREE=/home/yunyue/Projects/myworks/edu-agent-wt/issue37_upgrade_baseline_bc55267
git worktree add --detach "$OLD_TREE" bc55267e023bb3b5cb77f9da61d81b44d5c6257d
(cd "$OLD_TREE/server" && GOCACHE=/tmp/issue37-upgrade-gocache \
  go build -trimpath -o /tmp/issue37-upgrade-old-server ./cmd/edu-agentd)
(cd "$OLD_TREE/clients/cli-go" && GOCACHE=/tmp/issue37-upgrade-gocache \
  go build -trimpath -ldflags '-X main.commit=bc55267e023bb3b5cb77f9da61d81b44d5c6257d' \
  -o /tmp/issue37-upgrade-old-cli ./cmd/edu-agent)
(cd clients/web && npm run check && \
  node node_modules/vite/bin/vite.js build --configLoader runner)
(cd server && GOCACHE=/tmp/issue37-upgrade-gocache \
  go build -trimpath -tags web_release -o /tmp/issue37-upgrade-current-server ./cmd/edu-agentd)
```

旧服务首次构建下载了该提交锁定的 `golang.org/x/text v0.39.0`、
`golang.org/x/sync v0.21.0`、`golang.org/x/sys v0.41.0`；没有改依赖文件。
Web 使用已有 node_modules 符号链接。普通 `npm run build` 因 Vite 想写符号链接
依赖目录的 `.vite-temp` 被沙箱拒绝；改用 `--configLoader runner` 在本 worktree
完成生产资产构建，不写主仓库。既有 Zod 注释和 bundle 大小警告未阻止构建。

最终完整运行命令：

```sh
python3 scripts/test-issue37-upgrade-backup.py \
  --old-server /tmp/issue37-upgrade-old-server \
  --old-cli /tmp/issue37-upgrade-old-cli \
  --server /tmp/issue37-upgrade-current-server \
  --evidence-dir /tmp/issue37-upgrade-run-10
```

退出码 **0** 表示演练所有阶段完成，包括已知失败行为的记录；**不是发布门禁通过**。
必须读取 `results.json` 的各项 `status` 和末尾 `acceptance_failures`。
意外错误退出非零，并仍执行清理。最终关键输出：

```json
{"check":"execution","status":"complete","acceptance_failures":["old_cli_current_server","stale_snapshot_barrier"]}
{"check":"cleanup","status":"pass","databases":["issue37_upgrade_9032bdfd20eb","issue37_upgrade_9032bdfd20eb_restore"],"errors":[]}
```

原始证据留在 `/tmp/issue37-upgrade-run-10`，目录 `0700`、文件 `0600`：
`results.json`、`old-cli.log`、服务日志、`progress-response.json`、
`old-migrations.txt`、`current-migrations.txt`、`privacy-receipt.json` 和备份文件。
这里包含本次合成数据的原始 dump、临时凭据和密钥，不纳入 Git。
最终运行前的调试轮次没有当作完整通过，均删除了其临时库。

| 二进制 | SHA-256 |
| --- | --- |
| 旧服务 | `f078ff3712fb28eada79090f7439fdb8071dcf34305d03763bffbfa467eff014` |
| 旧 CLI | `9c2b5d785636c5c3bdb08134d402d4739013b94de73612fdab2e4156f33bc5b8` |
| 当前服务（含 Web 资产） | `81d5b6ca1510162fa052b36c98a1abf0afd6438507883189d7be706ac26cc6ab` |

## 旧库升级与 CLI 读取

所有业务数据均由旧二进制提供的正式 CLI/HTTP 建立，没有直接插入业务表。
旧 CLI 配对使用独立 `XDG_CONFIG_HOME`，一次性配对码通过 stdin 输入；脚本从
该私有目录读取同一旧设备的凭据用于正式 HTTP，不打印 token。

| 操作 | 实际命令/入口 | 结果 |
| --- | --- | --- |
| 配对 | 旧服务 `pairing-code create --profile user`；旧 CLI `pair --server http://127.0.0.1:33137 --name issue37-old-cli` | 通过，签发并保存旧设备身份 |
| 学习区 | 旧 CLI `space create --name 旧版学习区` | 通过，升级/恢复后名字与 ID 仍可读 |
| 两份文本资料 | `POST /v1/knowledge/imports`，`even.md`、`examples.md` | 通过，真实 canonical Markdown、节点、版本和检索片段 |
| 目标/课堂 | `POST /v1/learning/goals`、`POST /v1/tutoring/sessions` | 通过，默认学习区建立原教学闭环 |
| 出题 | actions：`start_diagnostic` → `apply_route` → `issue_activity` → `present_activity`；`POST /v1/tutoring/proposals` | 通过，由本地模型生成，引用正式资料片段 |
| 作答/评估 | actions：`submit_attempt(answer=A, help=none)` → `record_assessment` | 通过，原答案、原活动及一条正式 Evidence 持久化 |
| 记忆候选 | `POST /v1/memory/candidates`，`interaction_preference / transient / non_sensitive` | 通过，保存“旧版记忆候选：先解释再练习”，不依赖外部记忆 |
| 升级 | 当前二进制 `serve`，`MIGRATE_ON_START=true` | 通过，19 → 33，旧 checksum 未变，旧表未删 |
| 原数据读取 | 目标、课堂、Evidence、原资料导出、记忆候选、学习区详情 | 通过，活动/答案/Evidence 与升级前精确一致 |

最终样本：旧目标 `89b4653e-b0c0-49af-8ff3-d5e80c348037`，原课堂
`3c0b1961-0302-4b60-b0d1-2ceab1a15cc5`，资料版本
`887afd76-21a9-421b-9be1-1cc74ef54889`，记忆候选
`b1b14186-519a-4347-8992-532995673348`。这些 ID 对应现已删除的独立演练库。

升级初期 `/readyz` 已为 200，但 `/v1/learning/progress` 暂为
`503 projection_unavailable`；已有异步升级 worker 重建后变为 200。
脚本等待这个实际状态变化，最长 20 秒，没有写入测试专用重建状态。

同一旧 CLI 二进制连接当前服务，`goal show --id <旧目标>`、`space list`、
`route`、`evidence`、`device status` 均退出 **0**。`progress` 在重建后仍退出 **6**：

```text
error[protocol_error]: 服务端响应不符合公开协议（malformed_success_response）
method=GET path=/v1/learning/progress status=200 content_type=application/json
request_id=kvm17331/BRewCenyr7-000018 decode=unknown_field
```

旧 DTO 严格拒绝未知字段；当前 `ProgressPage` 增加 `data_cleared`，
`RouteProgress` 增加 `added_steps`/`removed_steps`，Evidence 来源增加 `attempt_id`。
现有响应没有给旧客户端提供相应兼容表示。**旧 CLI 部分读取通过，整体兼容失败**。
本任务停止这项修复：它涉及公共响应合同，不能通过更换被测旧 CLI 或改 OpenAPI
绕过失败；需要维护者决定兼容协商或明确版本限制。

## 原离线 golden / 签名回归

下面的现有测试均实际通过，没有数据库 skip，没有运行全量 Go 或浏览器矩阵。

```sh
(cd clients/cli-go && GOCACHE=/tmp/issue37-upgrade-gocache go test \
  ./internal/offline ./internal/command \
  -run 'Test(HeaderGoldenAndStrictValidation|VerifyPreparedPackRejectsTamperedSignature|OfflineTrustChainRejectsRollbackForkGapAndUnknownSigner|OfflinePrepareLearnSyncAndSafeLogoutLoop|OfflinePrepareReplaysRotationFromLegacyIntentAfterLocalAdvance)$' \
  -count=1 -v)
(cd server && GOCACHE=/tmp/issue37-upgrade-gocache go test ./internal/learning \
  -run 'Test(OfflineSignerLegacyConstructorRemainsRevisionOneByteCompatible|OfflineSignerUsesCanonicalDomainSeparatedPayloads|OfflineSignerManifestChainAdvancesOneTwoThree|Uint63DecimalAndJCSBoundaries|OfflineServicePrepareAndSyncFreezeWireSemantics)$' \
  -count=1 -v)
(cd clients/web && node node_modules/vitest/vitest.mjs run \
  src/offline/protocol.test.ts src/offline/crypto.test.ts --configLoader runner)
```

- `TestHeaderGoldenAndStrictValidation`：原 key/object header golden 及严格字段校验。
- `TestVerifyPreparedPackRejectsTamperedSignature`：篡改 pack 签名被拒绝。
- `TestOfflineTrustChainRejectsRollbackForkGapAndUnknownSigner`：回退、分叉、断链和未知 signer 被拒绝。
- `TestOfflinePrepareLearnSyncAndSafeLogoutLoop`、`TestOfflinePrepareReplaysRotationFromLegacyIntentAfterLocalAdvance`：离线闭环和原 intent 轮换重放。
- `TestOfflineSignerLegacyConstructorRemainsRevisionOneByteCompatible`：旧 revision-one 字节兼容。
- `TestOfflineSignerUsesCanonicalDomainSeparatedPayloads`、`TestOfflineSignerManifestChainAdvancesOneTwoThree`：规范化域分离签名和 1→2→3 manifest 链。
- `TestUint63DecimalAndJCSBoundaries`、`TestOfflineServicePrepareAndSyncFreezeWireSemantics`：大整数/JCS 与冻结 wire 语义。
- Web 两文件 **4 tests passed**，包括“完整验证签名链、冻结题目与原身份，保留授权字节和大整数”、
  “真实签名的未知授权协议仍只能只读，不能生成提交”。

## 关闭功能回滚

升级后用正式 Web Cookie/Origin/CSRF 建立新目标
`44f36b79-8cbc-45af-a262-a339b792fc9b`，通过显式 `settings` 配对档案保存导师复用
教学的设置。使用旧设备的 Bearer 身份建立保存的导师对话
`c06bd47c-8bf6-4dda-bcde-d2b5d9f45ea4`，提交真实运行并保存一轮 SSE 合成回复。
独立 `MENTOR_KEY_FILE` 保护标题和轮次；运行
`95c24537-ac38-4728-9b51-f99a533e5537` 成功保存正文。

```text
WEB_UI_ENABLED=true → 新目标与加密导师历史保存成功
WEB_UI_ENABLED=false → 重启 readyz=200
GET /v1/web/session（原 Cookie）=401；GET /app/=404
GET 新目标/导师历史（原 Bearer）=200，正文与标题逐项不变
旧 CLI goal show --id <新目标>：exit=0
DELETE 另一份临时保存对话（原 Bearer）=200；再 GET=404
WEB_UI_ENABLED=true → 重启原 Web Cookie=200，保留的新目标/历史逐项不变
```

关闭期间 schema_migrations 全部版本/checksum 及 public 表名集合不变；未删表、
未修改历史 migration、未让旧服务强写新 schema。**本开关回滚通过**。
`WEB_UI_ENABLED` 关闭浏览器入口，Bearer API 保留读取和清除，也仍允许其原有写入；
它不表示所有 HTTP API 只读。其他开关不能据此一并判通过。

## 备份、独立库恢复与权限

按 [安装/升级要求](issue-37-acceptance.md#安装升级保存与回退)，停止本次服务写入后
分别保存主库、环境配置、受保护学习设置和独立正文密钥。脚本实际调用：

```sh
docker exec edu-agent-acc-postgres pg_dump -U postgres \
  -d issue37_upgrade_9032bdfd20eb -Fc --no-owner --no-acl > database.dump
# server-env.json、settings.json、mentor.key 分别复制；目录 0700、各文件 0600
docker exec edu-agent-acc-postgres psql -X -U postgres -d postgres \
  -v ON_ERROR_STOP=1 -Atc 'CREATE DATABASE "issue37_upgrade_9032bdfd20eb_restore"'
docker exec -i edu-agent-acc-postgres pg_restore -U postgres \
  -d issue37_upgrade_9032bdfd20eb_restore --exit-on-error --no-owner --no-acl < database.dump
# 当前服务改 DATABASE_URL，并使用复制的配置、settings.json 与 mentor.key 启动
```

以上重定向必须在私有目录内执行；完整脚本已设置 `umask 0077`。
数据库原始 dump 含合成正文，**没有将它当作 Nocturne 受管加密备份**。
本次只验文件读取权限与独立密钥恢复；生产备份的独立加密/密钥托管部署没有运行。

最终 dump SHA-256：
`2f9c17ebdb1e4d0eec7a7077df01424ad3a2ee4c0375e87c92671d4a63ba2e03`。
备份目录 `0700`；`database.dump`、`server-env.json`、`settings.json`、`mentor.key`
均 `0600`，各自 SHA-256 记录在私有 `results.json`。

| 检查 | 结论与证据 |
| --- | --- |
| 恢复版本/迁移 | 通过，同一当前二进制 SHA-256；恢复的 33 条版本/checksum 精确一致，启动成功 |
| 配置恢复 | 通过，从复制的环境 JSON 和学习设置文件启动，保存的导师复用配置有效 |
| 原课堂及数据 | 通过，目标、活动、答案 `A`、正式 Evidence、资料和记忆候选与备份前一致 |
| 正文解密 | 通过，复制的正确密钥读回加密标题和永久轮次，内容与备份前精确一致 |
| 原身份登录 | 通过，备份前 CLI Bearer 与 Web Cookie 均返回 200 |
| 错误密钥 | 通过，替换为另一合法长度密钥后历史读取为 503；恢复正确密钥后原正文不变 |
| 密钥权限 | 通过，将恢复用 `mentor.key` 改为 0644，当前服务启动退出 1；恢复 0600 后可启动 |
| 设置文件权限 | 通过，将恢复用 `settings.json` 改为 0644，当前服务启动退出 1；恢复 0600 后可启动 |

未测试生产服务账户、PostgreSQL 独立最小权限角色、ACL/属主迁移；本环境使用
postgres 超级用户，dump 明确使用 `--no-owner --no-acl`。

## 撤销、隐私清除与旧快照恢复

在恢复库上使用原 CLI Bearer 执行 `DELETE /v1/devices/<Web 设备>`，随后原 Web
Cookie 登录从 200 变为 **401**。清除使用当前服务正式 `privacy-grant create --device
<原 CLI 设备>`，通过 `X-Privacy-Erasure-Grant` 调用 `POST /v1/privacy/erasures`，
读取正式候选响应的当前 generation，不通过 SQL 写入 barrier。

最终清除 `e6597736-21e7-4d4b-a91d-120344419174` 达到 **verified**：11 个本地
受管位置全部 `succeeded`；Nocturne 三个位置和受管备份为 `not_applicable`，
外部提供商为 `unsupported`，不是外部数据清除通过。完整回执另存
`privacy-receipt.json`。原课堂、资料导出、导师历史均拒绝旧正文读取。
另用只读 SQL 检查未清除目标、永久导师轮次、未清除标题的计数，结果 **`0:0:0`**。
**当前库的旧/新数据隐私清除通过**。

随后停止服务，删除并重新创建**自己的恢复库**，从同一份撤销/清除之前的 dump
再次恢复，使用相同配置和旧密钥启动。观察到：

```json
{"check":"stale_snapshot_barrier","status":"fail","revoked_web_cookie":200,"erased_old_and_new_content":"returns","snapshot_type":"raw main PostgreSQL pg_dump"}
```

旧目标、原答案/Evidence、记忆候选、新 Web 目标和加密导师正文均重新可读。
这不是当前库清除未生效：旧快照同时恢复了清除前的身份、吊销状态、generation、
barrier 和正文，且恢复者持有原正文密钥。现有主库启动流程没有独立于快照的单调
吊销/清除凭据，不能阻止这种回到旧状态的恢复。

## 文档差异、缺陷边界与未覆盖项

- **公共协议缺陷未修复**：旧 CLI `progress` 严格 DTO 与新增响应字段不兼容。
  issue #40 要求增加协议时保证原 CLI 兼容或明确受限；本次发现该读取入口实际失败。
  修复需要公共合同决策，按任务要求停止实施，未修改 API/OpenAPI。
- **恢复验收失败**：[原验收表](issue-37-acceptance.md) 写“撤销/清除后不重新上线旧快照”，
  原始主库 dump 的实测不满足。该文档升级节同时将外部备份排除在应用清除保证外；
  [记忆桥接规格](../comet/specs/memory-bridge/spec.md#erasure-receipt-与备份) 更明确将
  Go 数据库外部备份标为 unsupported/unknown。本次据实区分验收表要求与保证边界。
  没有自行加入外部单调状态、强制重配对或改变恢复/清除语义。
- [Nocturne README](../../deploy/nocturne/README.md) 和
  [ROLLBACK.md](../../deploy/nocturne/ROLLBACK.md) 约束的是 sidecar 独立数据库、
  带存活 generation key 的受管加密备份及新 volume 恢复。本次主库 dump 不属于
  该协议，不能用本次旧快照结果推断 Nocturne 销毁密钥后的恢复行为。
- SQL 数量以仓库实际为准：旧 19、当前 33；39 是目录总文件数。
- 升级后短暂进度 503 为已有异步重建行为；本次明确等待并记录，没有把它与重建后
  `HTTP 200 / old CLI unknown_field` 混为同一个问题。

未运行：中间版本升级、旧服务读取/强写升级后的新 schema、全部旧 CLI 命令、旧
离线包的真实跨版本签发/同步（本项使用既有 golden/签名回归）、其他功能开关、
真正二进制降级、密钥轮换恢复、数据库/WAL 物理擦除、Nocturne 容器及受管备份恢复、
真实提供商、外部搜索、浏览器渲染矩阵、TLS/Nginx、全量 Go、race、Compose。
日志中既有 `import-job-cleanup / unavailable` 警告未阻碍本次读取与演练，不在本任务
扩展修复。没有产品代码最小修复提交，因为已发现的需修复行为触及明确停止边界。

## 清理与提交范围

最终脚本删除 `issue37_upgrade_9032bdfd20eb` 与 `_restore`；前面各调试轮次也已清理。
旧版本临时 worktree 在构建和演练结束后通过以下命令删除：

```sh
git worktree remove /home/yunyue/Projects/myworks/edu-agent-wt/issue37_upgrade_baseline_bc55267
docker exec edu-agent-acc-postgres psql -X -U postgres -d postgres -Atc \
  "SELECT datname FROM pg_database WHERE datname LIKE 'issue37_upgrade_%' ORDER BY datname"
# 无输出
```

仅提交本记录及演练脚本；不提交二进制、Web 构建产物、凭据、dump、密钥或私有日志。
不推送、不建 PR、不修改 GitHub issue。
