# Issue #55：生产模型对照黑盒无法就绪

## 架构与问题确认

基线 `dc03439`，初始工作树干净。项目由 Go 服务端、PostgreSQL、Go CLI/TUI、
React Web 与共享 `agentcore` 组成。黑盒夹具构建真实服务端、CLI 和 fake LLM，
通过 HTTP、生产模型适配器与 learning/tutoring 服务验证数据库中的学习事实。
本问题位于测试夹具与服务启动配置的衔接处。

使用 Go 1.26.6、Linux amd64 和仓库固定的 PostgreSQL 17 镜像执行原版：

```bash
bash scripts/test-postgres-candidate.sh --shard model-vertical
```

baseline 与 candidate 分别在 31.01 秒、31.09 秒后报
`process readiness failed: status endpoint`，进程退出码为 1。
等待 candidate 就绪时读取其临时 `edu-agentd.log`，内容为“配置格式或范围无效”。
测试未进入配对。宿主无 `psql`，使用已有固定镜像中的客户端包装命令；
候选脚本持有共享主机锁，并负责创建、清理本次独立数据库容器。

根因（修复前行号）：

- `contracttests/cli-m1/blackbox/production_fake_model_vertical_test.go:108`
  给两组模型设置 `modelTimeout: 2 * time.Second`。
- `contracttests/cli-m1/blackbox/harness_test.go:237` 将该值写入 `MODEL_TIMEOUT`。
- `server/internal/app/app.go:566` 将模型超时转换为学习设置的 `IdleTimeoutSeconds`。
- `server/internal/settings/types.go:48` 限定该值为 5–600 秒；
  `server/internal/settings/service.go:71` 校验失败返回 `ErrInvalid`，
  `server/internal/app/app.go:88` 的启动流程随即退出。
- 同一夹具 `production_fake_model_vertical_test.go:129` 原先仅延迟 3000ms；
  若只增大服务超时，该场景将不再触发所需的真实超时。

## 开发方案与验收标准（实施前）

本次保持一个局部修复：将模型对照夹具的超时设为合法的 5 秒，
fake LLM 超时场景的延迟由该值加 1 秒推导，避免两者独立漂移。
使用既有完整黑盒作为回归，保留生产配置边界、模型协议及业务断言。

验收标准：

1. baseline、candidate 服务均完成启动并进入配对和既定评估语料。
2. schema 错误、低置信 provisional、真实超时、429、重试耗尽及后续成功
   的原有持久化与权威状态断言全部通过；超时必须仍记录 `timeout,success`。
3. 运行受影响模块的必要测试、静态检查、构建及 `git diff --check`；
   跳过与未运行项单独说明。

## 验证记录

2026-09-21 完成以下检查：

| 检查 | 结果 |
| --- | --- |
| 原版 `model-vertical` 分片 | 失败：baseline/candidate 均无法就绪，与报告一致 |
| 修改后相同分片 | 启动问题已消失，两组均完成服务启动、CLI 配对、资料导入与目标/会话创建；完整分片仍失败，见下述独立问题 |
| `contracttests/cli-m1`：`env -u TEST_DATABASE_URL go test -count=1 -v ./...` | 无数据库测试通过；15 个数据库场景明确跳过，不计为数据库验收通过 |
| `contracttests/cli-m1`：`go vet ./...`、`go build ./...` | 通过 |
| `contracttests/fakellm`：`go test -count=1 ./...` | 通过 |
| 真实黑盒构建 | 两次分片均成功构建服务端、CLI、fake LLM、响应丢失代理和投影重建夹具 |
| `git diff --check` | 通过 |

修改后 baseline/candidate 分别运行 6.54 秒、6.53 秒后失败于评估准备，
报 `schema setup state=GoalReady want=AwaitingResponse exit=6 error_code=internal_error`。
这已经越过 `production_fake_model_vertical_test.go:107` 的服务就绪等待、
`:112` 的两个 CLI 配对和 `:113` 的资料导入。

后续失败与已有 [Issue #56](https://github.com/ErzerLP/edu-agent/issues/56)
记录的目标选择现象一致，代码确认如下：

- `production_fake_model_vertical_test.go:313` 创建会话后，`:314` 仍执行没有
  `--session` 的新 CLI `learn` 进程，标准输入以 `y` 开始。
- `clients/cli-go/internal/command/learn.go:60` 只从当前进程读取已选会话；
  新进程没有选择，`:71` 因而进入目标/会话选择。
- `clients/cli-go/internal/command/tutoring_sessions.go:92` 等待目标编号，
  `:103` 起忽略 `y` 与空行，最终输入耗尽，既有教学会话仍为 `GoalReady`。

因此本次仅证明 #55 的启动缺陷已修复，尚不能宣称完整模型对照通过；
评估语料的超时/重试及持久化断言未执行。没有通过放宽配置限制、修改教学语义、
绕过目标选择或削弱断言来处理此独立问题。待 #56 修复后应重跑同一分片，
特别核对延长至 6000ms 的故障仍得到 `timeout,success`。

本地证据分别位于 `/tmp/edu-agent-issue55.FBq5KM/before/` 和
`/tmp/edu-agent-issue55.FBq5KM/after/`。两次使用 `POSTGRES_TEST_TIMEOUT=3m`，
均保留失败标记，没有生成通过标记。候选脚本已清理本次数据库容器；
临时日志及本机 `psql` 包装命令不纳入提交。
