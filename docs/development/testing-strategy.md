# 分层测试与验证策略

## 目的

测试用于尽快证伪当前改动，并在稳定候选上建立足够证据。开发反馈、批次门禁和最终验收是三个不同阶段；不得用频繁运行全量检查代替完成生产路径。

## 核心规则

1. 每次修改后选择能够暴露该修改错误的最小检查。
2. 精确测试未通过时不得扩大测试范围。
3. 一个稳定批次只运行一次适用的数据库、race 或跨模块门禁。
4. 一个候选只运行一次完整门禁；相关输入不变时复用证据。
5. 实现明确不完整时，不运行全仓、Compose、OCI、三平台或独立宽审计。
6. 真实 PostgreSQL 语义不使用 SQLite、mock 或普通单测代替。
7. 未配置依赖而 skip 的测试只能记录为未运行，不能宣称通过。

## 测试阶梯

### L0：编辑反馈

目标是在数秒内发现格式、语法和局部编译错误。

```bash
gofmt -w <changed-go-files>
go test -run '^$' <affected-package-list>
```

SQL、OpenAPI 和配置修改只运行对应解析、checksum 或 schema 测试。L0 不启动 PostgreSQL、Compose 或外部服务。

### L1：精确回归

对当前行为运行一个或少量具名测试。修复缺陷时优先先形成能够稳定失败的回归。

```bash
go test -count=1 ./internal/learning -run '^TestExactBehavior$'
```

数据库缺陷使用单个 package、单个具名测试和独立测试数据库。L1 失败时禁止进入 L2。

### L2：受影响 package

同一局部改动的精确回归通过后，对受影响 package 运行一次完整测试和适用 vet。

```bash
go test -count=1 ./internal/learning/...
go vet ./internal/learning/...
```

不得顺带加入未受影响的 knowledge、memory、Nocturne、CLI 或部署检查。

### L3：垂直契约

当一个批次的领域、持久化和入口已经完整时，运行覆盖该垂直结果的最小跨模块集合。涉及真实 PostgreSQL 时必须串行，并使用隔离数据库或 schema。

```bash
TEST_DATABASE_URL="$TEST_DATABASE_URL" \
  go test -p=1 -count=1 ./internal/learning/postgresstore
```

只有修改了跨 owner 事务、generation gate、Outbox、projection 或公共协议时，才增加对应另一端的契约测试。

### L4：批次门禁

批次达到可提交状态后运行一次：

- 受影响 package 的完整测试。
- 受影响持久化边界的串行 PostgreSQL 测试。
- 只针对修改过的 goroutine、lease、gate 或共享状态运行定向 race。
- 受影响 package 的 vet、error-level diagnostics 和 `git diff --check`。
- 一个最小端到端场景，证明批次结果可运行。

L4 不默认包含全仓 race、全部黑盒、Compose、OCI 或三平台原生证据。

### L5：候选门禁

所有垂直批次完成、完整需求重新核对后才运行。候选门禁根据改动选择：

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
git diff --check
```

涉及 PostgreSQL、外部依赖、CLI 平台行为或部署时，补充真实数据库矩阵、黑盒、契约、Compose、供应链或原生平台证据。Runtime 即将对同一候选执行相同检查时，Builder 不连续重复运行。

## 检查选择矩阵

| 改动 | L1/L2 | L3/L4 | L5 才运行 |
| --- | --- | --- | --- |
| 纯领域规则 | 具名测试、package | 必要时 reducer/replay 契约 | 全仓 race |
| PostgreSQL store/SQL | 单 DB 测试 | 受影响 store 串行矩阵 | 全写点故障矩阵 |
| HTTP/OpenAPI | handler、schema 测试 | 实际 server 窄场景 | 完整黑盒/Compose |
| CLI 命令/DTO | command、fake HTTP | 单一真实 CLI 场景 | 多平台原生矩阵 |
| 加密/本地存储 | golden、单一 backend | 崩溃和路径攻击矩阵 | 全平台 key backend |
| privacy/generation | owner 精确回归 | 相关 owner 联合 DB 契约 | 多设备清除黑盒 |
| 外部 sidecar | fake contract | 受影响集成契约 | 固定镜像 Compose/OCI |

## 防止测试循环

同一个失败命令在没有新信息时最多连续执行两次：第一次稳定复现，第二次验证一个有依据的修复。第二次仍失败时，必须回到代码、SQL、fixture 或环境提出新假设；没有输入变化不得第三次重跑。

通过的命令在生产代码、测试、依赖、配置、schema 和环境均未变化时不得重复执行。宽检查失败后先定位到最小可复现测试，再继续修改。

## PostgreSQL 纪律

- 多个 package 共用 `TEST_DATABASE_URL` 时使用 `-p=1`，禁止并发重建同一 schema。
- 新测试优先使用随机隔离 schema、模板数据库 clone 或独立临时数据库。
- 没有 `TEST_DATABASE_URL` 时明确 skip，并在交接中记录数据库检查未运行。
- migration 只追加，不修改 checksum-protected 历史文件；兼容变化使用新 migration、版本化 fingerprint 或 upcaster。
- 全量故障注入只在稳定持久化批次或候选运行一次。

## 证据复用

昂贵检查使用以下 evidence key：

```text
检查名称 + 候选提交或相关文件摘要 + 工具版本 + 环境/平台标识
```

只有相关生产输入、测试输入、依赖、schema、工具或平台变化时证据才失效。Builder handoff 列出复用证据、key 和未运行检查；Verifier 判断复用是否充分。

## 审计与 Verifier

独立审计只在完整高风险批次或候选稳定后启动。局部修复不立即重复宽审计；先运行受影响检查，再由 Runtime 决定是否需要新的 Verifier attempt。

Verifier 不能用 Builder 的测试摘要代替独立判断。检查不足时请求具体补充命令，不应要求无差别重跑所有历史矩阵。

## 结果记录

每个批次记录：完成结果、实际运行命令、通过/失败/skip、复用证据、已知限制和下个批次。只有 Runtime 接受全部验收结论后才能宣称 change 完成。
