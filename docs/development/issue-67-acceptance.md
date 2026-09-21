# Issue #67：隐私清除远端核对的并发推进

## 问题确认与架构

Go/PostgreSQL 的 memory owner 保存交付、核对租约和删除计划，privacy owner
编排逐存储清除；Nocturne 适配器在事务外执行远端删除。托管备份密钥在首个
barrier 事务中销毁，备份销毁验证在远端核对完成后执行。

基线 `dc03439` 的相关代码与报告的 `364fbe1` 相同。使用原服务端镜像
`edu-agent-task76-server:candidate` 和通过当前锁文件校验的 OCI，在独立 Compose
项目运行原始 `full` 场景，退出 1，复现 `privacy erasure did not verify managed
backup destruction`。回执为 `partial`，备份为 `pending/awaiting_erasure_step`，
一个核对停在 `delete_pending` 且仍持有租约。

根因链条（以下行号对应修复前基线）：

- `server/internal/transport/httpapi/memory_privacy.go:451` 与
  `server/internal/app/workers.go:190` 分别从 HTTP 和后台推进同一清除。
- `server/internal/privacy/postgresstore/remote.go:30` 没有覆盖远端操作的互斥；
  一个执行者持租约删除时，另一个无法领取该任务，却可提交 partial 回执，
  在同文件 `:197` 替换当前授权回执。
- `server/internal/memory/postgresstore/maintenance.go:27` 要求维护授权绑定当前
  回执，旧执行者因此失效；`:199` 又要求原租约到期才可重领。
  Compose 的租约是 120 秒，验收窗口是 90 秒。

本次现场中，未完成任务的授权回执与 current receipt 不同，删除计划尚未保存；
HTTP 日志记录 `privacy remote erase queued / receipt_not_current`。这证明了
回执竞争，不意味着备份密钥仍然存在或发生数据泄漏。

## 开发方案与验收标准

单批次修复 privacy 的远端编排入口：按 erasure ID 使用 PostgreSQL 会话级
非阻塞 advisory lock，在读取回执前取得锁，并持有至远端操作及回执提交结束。
竞争者只返回当前回执，后续恢复任务继续推进。远端 I/O 不持数据库事务；
取消、错误和成功均释放锁，解锁失败的连接不得归还池。

不修改租约长度、90 秒验收窗口、维护授权、generation fence、密钥销毁逻辑
或 Nocturne 镜像；不新增 schema 或 API。

验收标准：

1. 真实 PostgreSQL、独立连接池上的确定性并发回归：第一个执行者已取得维护
   租约并进入删除阶段时，第二个执行者不能替换授权回执；首个执行者无需等待
   租约过期即可完成，远端范围回执收敛，memory gate 按原规则打开。
2. 取消或远端失败后可以继续；完成后的重复调用不重复执行远端删除。
3. 受影响 privacy 持久化测试、相关 Nocturne/app 契约、定向 race、vet 和构建通过。
4. 从当前源码 Dockerfile 构建修复镜像，运行相同锁定 OCI 的原始 `full` 场景，
   包括备份销毁验证、销毁后恢复拒绝、保留期清理与最终 SIGTERM。

## 验收结果

- 原始 `full`：修复前退出 1，复现相同的 90 秒超时和状态组合。
- 确定性并发回归：修复前失败于 `maintenance_authorization_not_current`；修复后
  同一测试通过。错误与取消恢复回归也通过，无数据库测试跳过。
- `bash scripts/test-postgres-candidate.sh --shard privacy-core`：29 项执行、29 项
  通过、0 项跳过；按项目分片约定不包含无关的全写点故障矩阵。
- `cd server && go test -count=1 ./internal/privacy/... ./internal/integrations/nocturne ./internal/app`、
  对相同包的 `go vet`、`go build ./...`：通过。普通 Go 测试中的数据库 skip
  不作为数据库证据；真实数据库结论来自上述独立分片。
- 当前源码 `server/Dockerfile` 构建通过，候选为
  `edu-agent-issue67-server:candidate`，平台 manifest 为
  `sha256:8834e61f1529f38e6378ca40f83bafb9b7c5ed227b7040b9ad9d98a60ad4b39d`。
- 独立 PostgreSQL 上执行
  `go test -race -p=1 -count=1 -v -run '^TestRunNocturneErase' ./internal/privacy/postgresstore`：
  3 项测试通过，包含并发授权保护、取消/错误恢复及既有成功后开门契约。
- 修复后原始 `full`：退出 0，输出
  `Nocturne Compose candidate: PASS scenario=full`。现场查询确认所有交付为
  `deleted`、清除回执为 `verified`；原脚本的 90 秒回执轮询、密钥销毁验证、
  销毁后恢复拒绝、保留期清理与 SIGTERM 检查全部通过。

完整场景使用同一锁定 Nocturne platform manifest
`sha256:8e427437d35e603cb603608a124ff14344031fcc352435098b4275fa16119d45`，
命令如下（OCI 路径替换为本机已经校验的目录）：

```sh
docker build --provenance=false --sbom=false -f server/Dockerfile \
  -t edu-agent-issue67-server:candidate .
NOCTURNE_E2E_SERVER_IMAGE=edu-agent-issue67-server:candidate \
  sh contracttests/nocturne/run-compose-e2e.sh /absolute/path/to/verified-oci-layout
```

原始失败日志、回归日志和构建日志保留在本机私有证据目录；不提交临时日志、
凭据或本地 agent 状态。脚本已清理各自的临时容器、卷和导入标签，临时测试
数据不可恢复，未清理已有部署。工作仅提交当前任务分支，由用户审查落地。
