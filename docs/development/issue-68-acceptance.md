# Issue #68：目标分页数据库测试的修订链夹具

## 问题确认与架构

基线为 `169f8f7`，与报告中的提交一致。项目由 Go 服务、PostgreSQL、Go CLI、
Web 客户端和共享 agentcore 模块组成；服务端通过 HTTP/MCP 暴露领域服务，
各领域的 postgresstore 负责持久化，迁移维护数据库约束。
本次涉及 learning owner 的目标列表与修订历史查询。

修复前使用 Go 1.26.6、Linux amd64，以及仓库固定的 PostgreSQL 17 镜像
`postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73`，
运行以下精确回归。仓库脚本创建独立临时数据库容器、设置 `TEST_DATABASE_URL`，
测试通过 `learningIntegrationPool` 在隔离 schema 中执行全部迁移。

```sh
bash scripts/test-postgres-candidate.sh --shard learning-core \
  --run '^TestPostgreSQLGoalListExcludesErasedGoalsBeforePagination$'
```

命令退出 1，复现 `goals_test.go:52` 的 `learning_goal_previous_shape`
约束错误（SQLSTATE 23514），分页与历史断言尚未执行。以下行号对应修复前基线：

- `server/internal/learning/postgresstore/goals_test.go:44–52`：夹具直接插入
  第二版修订，却没有提供 `previous_revision_id`、`previous_goal_id` 和
  `previous_revision`，三个字段均为 NULL。
- `server/migrations/000003_learning_core.sql:88–94`：第一版要求前序字段为
  NULL，后续版本要求三者齐全、同属一个目标、版本号连续，并引用实际存在的修订。
- `server/internal/learning/postgresstore/records.go:19–26`：生产写入已遵守上述契约。
- `server/internal/learning/postgresstore/goals.go:93–96`：列表在分页前排除
  `privacy_erasure`，历史查询保留修订；本次失败不能证明分页实现有误。
- `server/internal/learning/postgresstore/privacy.go:202`：实际清除只替换文本、
  来源和管理快照，保留版本链，故夹具也必须保留合法的前序引用。

## 开发方案与验收标准

一个批次完成测试夹具修复：插入辅助函数接收前序修订 ID，按生产契约填写三个
前序字段并返回新修订 ID；第一版保持 NULL，第二版引用实际插入的第一版。
增强既有历史断言，核对两版身份、版本号、清除来源和前序引用。
范围限于该测试和本验收记录，沿用现有数据库约束及生产分页逻辑。

验收标准：

1. 原精确回归在真实 PostgreSQL 上通过，并执行首页、下一页、搜索/状态筛选和
   历史断言；旧格式未清除目标仍可见，两版已清除修订及其引用仍保留。
2. 真实数据库 `learning-core` 分片通过，数据库用例没有因缺少配置而跳过。
   按项目约定，该分片排除 offline 和全写点故障矩阵。
3. 受影响 postgresstore 包的 `go vet`、`go build`、Go 格式检查及
   `git diff --check` 通过。

## 验收结果

- 原精确回归修复前失败，修复后通过：1 项执行、1 项通过、0 项跳过。
  首页返回新目标，下一页保留未清除旧格式目标；搜索和状态筛选不会重新显示
  已清除目标；两版历史身份、版本号、清除来源及前序引用均符合预期。
- `bash scripts/test-postgres-candidate.sh --shard learning-core` 通过：
  39 项执行、39 项通过、0 项失败、0 项跳过，耗时 143 秒。
- 在 `server` 目录执行 `go vet ./internal/learning/postgresstore` 和
  `go build ./internal/learning/postgresstore`，均退出 0。
- 修改过的 Go 文件通过 `gofmt` 检查，`git diff --check` 通过。

数据库证据绑定相同源码指纹
`9383acdaaeb87dd8e13d292d2d5bbbc82b780d5f0fbfc15aa1f5a444bd24ca78`；
精确回归 evidence key 为
`3607ec27dfff297c2cf0941870e927e26d94249f3d6d80438d448052f2bf4691`，
learning-core 分片 evidence key 为
`ea87f1ff349121ac310ec5e370034a55a74d3aae904b1e03607bcf0321a67115`。
日志和测试清单由仓库脚本保存在本机临时证据目录，不提交临时记录。

本次只修改测试夹具和历史断言，生产代码、迁移及数据库约束未改。
未运行无关的 offline/故障矩阵、CLI/Web、外部模型及全仓发布检查。
脚本已清理各次验证专用的临时 PostgreSQL 容器及测试数据；这些临时数据不可恢复，
已有部署未被清理。结果提交当前任务分支，按任务工作树约定交由用户审查落地。
