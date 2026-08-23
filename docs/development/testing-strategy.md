# 开发测试策略

## 目的

本文定义 `memory-bridge` 剩余 Build 阶段及后续变更的开发期测试流程。目标是在不降低最终 Comet 验收强度的前提下，缩短反馈周期，避免开发陷入重复测试循环。

本文只是开发流程约束，不修改已经冻结的 `memory-bridge` Shape、A1-A89 或 Builder handoff 前的最终检查要求。

## 本轮反思

当前开发进度过慢，测试流程确实是主要原因之一，具体问题如下：

1. 把“每次编辑后的快速反馈”和“里程碑验收”混在了一起。小改动之后也频繁运行 package 组、全量 PostgreSQL、全量 race、Compose、供应链检查和独立审计。
2. 单个失败测试尚未修好时就重跑宽范围测试，导致每次诊断都重复支付无关模块的执行成本。
3. 没有按输入变化管理昂贵证据。例如 privacy Go 代码变化不会使已锁定的 Nocturne OCI 可复现构建结果失效，纯领域变化也不会使 Compose 网络验证失效。
4. 实现批次尚未稳定就启动独立审计，后续修改又使部分审计结论失效，形成重复审计。
5. PostgreSQL 测试曾被当作一个可随时全量运行的命令。当前多个 package 都会重建共享的 `public` schema，同一数据库上的并发 package 测试不安全，也容易制造与实现无关的失败。
6. 把“测试通过”当成进度单位，而不是把“完整实现一个可交付批次”当成进度单位。结果是反复验证未完成的局部代码，而 HTTP、app wiring、备份和最终清除编排仍未完成。

后续必须使用“能够证伪当前改动的最短测试”，只在稳定批次边界扩大范围，并把全量证据保留到真正可提交的候选版本。

## 核心规则

1. **先测刚改动的行为。** 一次编辑后只跑一个具名测试或一个受影响 package，不从 `go test ./...` 开始。
2. **窄测试通过后才扩大。** 固定顺序为：具名测试、受影响 package、受影响的跨模块契约、稳定批次、Builder candidate。
3. **复用未失效证据。** 已通过的检查只有在其生产输入、测试输入、依赖、环境或 schema 改变后才需要重跑。
4. **每个稳定批次只做一次宽检查。** 一个批次内部不因每次修复重复运行 PostgreSQL 矩阵或 race。
5. **每个候选版本只做一次全仓门禁。** 完整测试、完整 race、vet、漏洞检查、Compose 和部署检查只在完整候选准备好时运行一次。若 Comet Runtime 会在同一提交和同一环境重跑，不在 handoff 前无意义地连续重复。
6. **共享数据库测试必须串行。** 当前集成测试会重建 `public` schema，多个数据库 package 使用同一个 `TEST_DATABASE_URL` 时必须加 `-p=1`。
7. **实现已知不完整时不做宽验收。** 可以跑聚焦回归，但不能把全仓测试和审计当作替代实现的活动。
8. **稳定后再审计。** 每个高风险完整批次做一次独立只读审计，最终候选再做一次；局部修复后不立即重复派发宽审计。

## 测试阶梯

### L0：编辑反馈

每次生产代码编辑后执行，目标是数秒到几十秒。

```bash
cd server
gofmt -w <changed-go-files>
go test -run '^$' <affected-package-list>
```

这一级只检查格式和编译。SQL 或 OpenAPI 改动只运行能读取该文件的最小解析或 migration 单测。

### L1：精确回归

修复已知缺陷时，只运行能够复现该缺陷的具名测试。

```bash
cd server
go test -count=1 ./internal/privacy -run '^TestExactName$'

TEST_DATABASE_URL="$TEST_DATABASE_URL" \
  go test -p=1 -count=1 ./internal/memory/postgresstore \
  -run '^TestPostgreSQLExactName$'
```

L1 没通过时禁止扩大测试范围。新缺陷原则上先有聚焦回归再修生产代码，除非只能在更宽的真实边界稳定复现。

### L2：受影响 package

同一局部改动的所有精确回归通过后执行一次。

```bash
cd server
go test -count=1 ./internal/privacy/...
go test -count=1 ./internal/integrations/nocturne/...
```

PostgreSQL package 只跑受影响的单个 package，并使用 `-p=1`。不能为了“更放心”而顺带添加无关的 learning、knowledge、memory 或 Outbox package。

### L3：跨模块契约

只有当改动跨越 ownership、事务或状态契约时执行一次，选择能够覆盖契约两端的最小集合。

```bash
cd server

# Privacy barrier 与 memory maintenance authorization。
TEST_DATABASE_URL="$TEST_DATABASE_URL" \
  go test -p=1 -count=1 \
  ./internal/privacy/postgresstore \
  ./internal/memory/postgresstore

# Redacted event loading 与 projection rebuild。
TEST_DATABASE_URL="$TEST_DATABASE_URL" \
  go test -p=1 -count=1 \
  ./internal/privacy/postgresstore \
  ./internal/learning/postgresstore

# Delivery 与通用 Outbox 状态转换。
TEST_DATABASE_URL="$TEST_DATABASE_URL" \
  go test -p=1 -count=1 \
  ./internal/memory/postgresstore \
  ./internal/platform/outbox/postgresstore
```

L3 不是每次编辑后的固定步骤，只在本地 package 已通过且跨模块契约已经实现完整时运行。

### L4：稳定批次门禁

一个完整实现批次结束后只执行一次。当前批次包括：本地 privacy erasure、远端 Nocturne purge、managed backup、HTTP/app composition。

批次门禁仅包括：

1. 受影响 package 的单测。
2. 受影响持久化边界的 PostgreSQL 测试，串行执行。
3. 只有批次修改 goroutine、lease、gate、cancel 或共享进程状态时，才跑相关 package 的定向 `-race`。
4. 受影响 package 的 `go vet`。
5. 修改文件的 error-level diagnostics。
6. 上述检查通过后的一次独立只读审计。

除非该批次直接修改对应输入，否则 L4 不包含全仓 race、Compose 或 OCI 重建。

### L5：Builder candidate 门禁

只有 A1-A89 的生产路径全部存在、工作树准备提交时才运行。

```bash
cd server
TEST_DATABASE_URL="$TEST_DATABASE_URL" go test -p=1 -count=1 ./...
TEST_DATABASE_URL="$TEST_DATABASE_URL" go test -p=1 -race -count=1 ./...
go vet ./...
govulncheck ./...
go build ./cmd/edu-agentd

cd ..
git diff --check
```

同时只运行输入发生过变化的最终检查：OpenAPI/YAML 解析、Nocturne contract、Compose E2E、backup restore/key destruction、OCI verification。之后由 Comet Runtime 和新的独立 Verifier 给出正式验收结果。

## 改动与检查映射

| 改动区域 | 立即检查 | 稳定批次检查 | 不会因此失效的证据 |
| --- | --- | --- | --- |
| `internal/memory` domain/service | 精确 domain 测试、memory package | memory package 和相关 PostgreSQL 契约 | OCI 可复现性、Compose 网络 |
| `internal/memory/postgresstore` | 精确 PostgreSQL 测试 | memory PostgreSQL；只有事务或状态契约改变时才加 Outbox | Nocturne overlay build |
| `internal/privacy` domain/permit | 精确 privacy 测试、privacy package | permit manager 定向 race | OCI、无关 knowledge 测试 |
| `internal/privacy/postgresstore` 或 privacy SQL | 精确 privacy PostgreSQL 测试 | privacy 加受影响 owner store；DDL 改变时加 migrations | 未修改部署时的 Compose 和 OCI |
| redacted learning replay/projection | 精确 learning event/rebuild 测试 | learning 与 privacy PostgreSQL 跨模块契约 | memory delivery、Nocturne overlay |
| Outbox 状态或 lease | 精确 Outbox 测试 | Outbox 与 memory delivery PostgreSQL 契约 | knowledge、model 测试 |
| Go Nocturne client/worker | 精确 fake-server 测试 | `internal/integrations/nocturne`，再加受影响 memory PostgreSQL 测试 | overlay 输入未变时无需 OCI 重建 |
| `deploy/nocturne` overlay、lock、Dockerfile、tool | contract 与 lock verification | 输入稳定后做一次双构建和 OCI verification | Go 未变时无需全仓 race |
| backup encryption/controller | 精确 crypto/inventory 测试 | privacy/backup PostgreSQL 与 restore/key-destroy fixture | 无关 learning、knowledge 测试 |
| HTTP/OpenAPI/config/app | 精确 route/config/app 测试 | 对应 package，最终 wiring 稳定后做一次 Compose | 镜像输入未变时无需 OCI 重建 |
| `deploy/compose.yaml` 或 runtime wiring | 配置解析、相关 app 测试 | wiring 稳定后做一次 Compose E2E | 无需重跑所有领域单测 |

## 防测试循环规则

### 两次运行规则

针对同一个失败，完全相同的测试命令最多连续执行两次：

1. 第一次用于稳定复现并记录失败。
2. 第二次用于验证一个有明确依据的修复。

第二次仍失败时，不得立刻第三次重跑。必须回到代码、SQL、状态和日志，提出新的根因假设，并产生对应的代码、fixture 或诊断改动后才能再次执行。

### 禁止无变化重跑

一个通过的命令，如果相关生产代码、测试代码、依赖、环境和数据库 schema 都未变化，不得重复运行。证据记录到当前批次并复用。

### 先窄后宽

任一精确回归仍然失败时：

- 不跑 package 组；
- 不跑全量 PostgreSQL；
- 不跑全量 race；
- 不跑 Compose 或 OCI build；
- 不派发新的宽范围独立审计。

### 时间预算

以下是触发流程调整的预算，不是牺牲正确性的硬超时：

| 反馈级别 | 目标预算 | 超出后的动作 |
| --- | --- | --- |
| L0 编译/格式 | 30 秒内 | 缩小 package 或直接处理编译输出 |
| L1 精确 unit/DB 测试 | 实际可行时 60 秒内 | 移除无必要 setup/sleep，隔离 fixture |
| L2 受影响 package | 2 分钟内 | 拆成具名测试，识别慢 fixture |
| L3 跨模块 DB 契约 | 5 分钟内 | 串行 package，移除无关模块 |
| L4/L5 | 明确排期 | 禁止放进编辑-修复循环 |

真实并发和 TTL 语义有时需要较慢的精确测试。预算要求测试是有意选择的，而不是通过削弱断言换速度。

## 昂贵证据复用

每项昂贵结果使用以下 evidence key：

```text
检查名称 + 相关文件摘要或提交 + 工具版本 + 环境标识
```

失效规则：

- OCI：Dockerfile、overlay、supply-chain lock、image lock、构建工具、平台或 source archive 改变时失效。
- Compose：Compose、配置、image digest、app startup、health/readiness、网络、volume 或 credential wiring 改变时失效。
- PostgreSQL：相关 store Go 代码、migration SQL、fixture、PostgreSQL major version 或事务契约改变时失效。
- Race：相关并发生产代码或并发测试改变时失效；文档或 OpenAPI 改动不会使其失效。

Builder handoff 要列出复用的证据及 key。无法判断是否失效时，在 L4 或 L5 重跑一次相关检查，不能在 L1 阶段重复运行。

## PostgreSQL 测试纪律

当前集成 package 会重置共享的 `public` schema，因此：

1. 修复期间只运行一个 package 的一个具名测试。
2. 多个数据库 package 共用 `TEST_DATABASE_URL` 时必须使用 `-p=1`。
3. 禁止两个数据库测试命令并发使用同一个测试数据库。
4. 后续可以建设“已迁移模板数据库 + 每 package 独立 clone”的测试 harness。在此之前，串行执行是确定性选择。
5. 只有在 test clock 或等价数据库不变量能够保持并发语义时，才替换较宽的 fixture sleep。不能单纯缩短 TTL 制造时间敏感测试。

## Memory-Bridge 剩余执行顺序

1. **本地 privacy core：** 完成 owner read/write generation fence、空 active projection rebuild、本地 scrub verification 和精确并发回归。修复期间只运行具体 privacy、memory 或 learning 测试；批次结束时做一次 privacy 跨模块 PostgreSQL 门禁和一次定向 race。
2. **远端 erasure：** 把 durable maintenance reconciliation 接到 Nocturne purge、orphan/history verification 和 remote receipt。只跑 fake Nocturne 和受影响 memory PostgreSQL 测试；overlay 输入未变时不重建 OCI。
3. **Managed backup：** 实现 envelope encryption、wrapped-key destruction、inventory、restore verification 和 retention。先跑聚焦 backup fixture，再做一次 backup/Nocturne 集成门禁。
4. **HTTP、grant、app、config、OpenAPI：** 使用精确 route 和 composition 测试，之后跑受影响 package；所有 wiring 稳定后只做一次 Compose。
5. **Candidate：** L5 只跑一次，A1-A89 只完整核对一次，然后提交并执行 Builder handoff，由 Runtime 和新 Verifier 做正式验证。

## 立即应用

本文写入时，一个 memory maintenance 精确回归仍在修复。下一条测试命令只能运行这个具名测试，不能再次运行三个 package 的 PostgreSQL 组合。该测试通过后：

1. 运行 privacy gate-retention 的精确回归。
2. memory 和 privacy 受影响 package 各运行一次。
3. 继续完成尚未实现的 privacy owner port、remote purge 和 backup 路径。
4. 本地 privacy 批次完成前，不运行全仓测试、全仓 race、Compose、OCI build 或新的宽范围审计。

正确性检查必须服务于实现进度，不能取代完成模块本身。
