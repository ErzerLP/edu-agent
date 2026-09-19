# Issue #38 验收记录

## 确认与范围

基线 `5855c8a` 的工作树干净。`go run ./cmd/edu-agent companion --help`
返回 `unknown command companion`；Web 导师生产工具目录和分支没有本机能力，
全库无 companion 配对、授权或通道。不是已有能力的改名或配置问题。
代码证据和实施前方案见 [本地 companion 设计](../design/local-companion.md)。

交付独立 `edu-companion`、默认关闭的 `COMPANION_ENABLED`、认证主动轮询、
Web 独立授权/撤销/文件与任务面板，以及仅授权后注册的导师转发工具。
结构化文件工具复用 `workspace`，Shell/任务/PTY 复用 `localexec`；服务端没有 OS provider。
CLI 原入口、配置、Keychain 和加密历史没有迁移。

## 已取得的证据

- 缺失命令的初始检查确实失败；新增独立程序通过真实 HTTP 端到端构建并运行，
  在指定临时工作区创建真实文件，回执包含设备、对话、运行、任务和原任务运行身份。
- 共享通道测试：未授权、错误设备/浏览器/对话/Origin/MAC、重复配对码/序号拒绝；
  相同操作返回原回执，变更载荷冲突；丢失投递不重发；部分输入回执可重复结算；
  过期和服务重启不能恢复旧权限，模型不能批准文件提交。
- Linux 原生 provider 测试：先冻结预览再创建文件，工作区越界和符号链接不能读取；
  真实 Shell/stdin、重复输入仅写一次、长任务退出清理、旧任务不能再中断、PTY 输入和 resize；
  输出截断、原字节偏移和仅内存保存状态分别保留。
- 客户端主动 HTTP 通道实测：真实执行、丢失响应后不重放；拒绝远程 HTTP、含凭据/路径的 Origin，
  不跟随重定向发送配对秘密。
- 独立 PostgreSQL：真实对话与隐私代次、模型目的地、设备身份及撤销校验；
  真实共享 Runner → companion 通道 → 设备回执 → 模型续答，命令/env 参数没有进入持久历史。
- 真实学习 Cookie/CSRF → Go HTTP → 独立本机程序：Origin/Host/CSRF 伪造拒绝；
  未授权不能发 Shell；执行用户和工作区来自真实程序；浏览器不能伪造模型运行身份；
  学习设备撤销后原生程序退出，旧 Cookie 不能再读取回执；关闭开关不挂载通道。

PostgreSQL 使用本任务独立的本地临时容器和数据库，测试按 package 串行，
不使用开发/生产数据库。测试中的模型是本地确定性夹具，不是真实付费供应商。

## 验证命令与待补项

已通过精确检查：

```text
packages/agentcore: go test -count=1 -v ./companion
clients/cli-go: go test -count=1 -v ./internal/companion
server（独立 TEST_DATABASE_URL）:
  go test -count=1 -v ./internal/mentorrun -run 'TestCompanion|TestPostgreSQLCompanion'
  go test -count=1 -v ./internal/transport/httpapi -run '^TestPostgreSQLCompanionHTTPNativeEndToEnd$'
```

候选检查结果：

- `packages/agentcore`：`go test ./...`、companion 定向 race、`go vet ./...`、`go build ./...` 通过。
- CLI：`go test ./internal/companion ./internal/agentloop ./internal/localexec ./internal/workspace`
  通过；companion 定向 race、受影响包/入口 vet、`go build ./...` 通过。
  后补的文件过期版本、Shell 工作区外原生执行和实际部分输入回执测试也在 race 下通过。
  `make companion-build` 和独立程序 `--help` 通过，入口不加载旧 CLI 的本地配置。
- server：独立 `TEST_DATABASE_URL` 下串行运行 `go test -p=1 ./internal/mentorrun
  ./internal/transport/httpapi ./internal/platform/config ./internal/app` 全部通过；
  相应 vet 和 `go build ./...` 通过。最后收紧浏览器 token 绑定后，重跑 companion
  的 PostgreSQL/HTTP 定向 race、vet 与构建通过，未重复无关宽矩阵。
- `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/edu-companion` 交叉构建通过，
  仅证明可编译，不代表 macOS 原生运行通过。
- `docker compose -f deploy/compose.yaml config --no-interpolate --quiet`、`git diff --check` 通过。

以上 Go 构建是开发构建；Web 发行资产和 `web_release` 构建不在已通过范围。
未配置的其他外部集成、真实供应商与平台矩阵不在本次检查结论内。

本机没有 `clients/web/node_modules`，`npm run check` 实际失败为 `tsc: not found`。
已申请按锁文件安装依赖，但未获回复前不执行安装。Web 类型检查、单测、生成类型、
生产资产构建及真实浏览器交互尚未验收，不把 Go HTTP 检查替代浏览器证据。
macOS 没有原生执行环境，尚未取得原生安全/交互证据；交叉编译不能替代该项。
因此本记录不声明全部平台或整个 Issue 已完成发布验收。

## 生命周期与限制

配对码五分钟内使用一次，双端凭据仅内存保留。单授权绑定一台设备和一个对话，
不能升级或重新绑定，需撤销后重新配对。浏览器每三秒观察并续期；原生程序约每秒主动轮询。
两端租约均为 45 秒；网络请求最多十秒，失联的本机可能到下一次检查才开始收尾，
再等待至多十秒清理。已领取的在途操作可能完成，撤销不是副作用回滚。
页面关闭通知是尽力发送，租约才是最终边界；进程被强杀或 OS 崩溃时不能承诺所有子进程已结束。

文件预览和操作回执仅在内存。重启授权失效，不读取旧 PID 或重放命令、stdin、文件提交。
同一原生进程重连可以继续查询原任务；服务重启或程序退出后必须重新配对，旧任务/输出不可恢复。
受管进程清理由原 provider 执行，不承诺终止已经脱离进程组的进程。
每连接至多 1024 个操作、8 MiB 回执、一个待投递操作；总连接上限 16。
原生任务沿用 provider 的默认 256 项/16 并发和输出预算；文件预览上限 48 KiB。
容量不足明确拒绝，不自动删旧身份再执行。最近回执显示 20 项，原操作可凭身份查询。
本版本提供 list/read/stat 和 write/edit 的冻结审批；不暴露归档清理等额外文件操作。

本机数据经学习服务器传输；未授权模型外发时导师没有本地工具。授权后，工具结果和其中可能出现的
终端回显按当前对话的保存模式处理；独立参数字段的原始 command/env/input 不持久化。
已进入模型、对话历史或 OS 自身日志的内容不能通过撤销 companion 撤回。
