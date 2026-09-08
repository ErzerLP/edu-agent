# Issue #1：最终验收入口

当前实现已覆盖 C1–C10；本次核查未确认新的产品功能缺口。此前交接见
[候选交接](../design/client-local-tools-handoff.md)。本文补充可重复执行的验收入口，不替代对应提交的运行结果。

## 确认并修复的失败

`TestWorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls` 在自动上下文模式下，
完成前台请求后会触发 observer。旧测试的 `fakeModel.Complete` 追加 `requests`，
与预算断言读取同一切片竞争，且后台请求可能破坏“恰好两次前台请求”的断言。
用以下命令已复现 DATA RACE：

```sh
cd clients/cli-go
go test -race ./internal/agentloop -run '^TestWorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls$' -count=30 -timeout=90s
```

修复只隔离此测试的后台 observer 请求，并注册 Session 清理；保留自动上下文模式和原预算断言。
同一测试修复后 `-count=100` 通过。生产工具行为没有变化。

## 双平台自动验收

`.github/workflows/cli-platform.yml` 在 Linux 和 macOS 原生 runner 上运行：

- 全量 `go test -count=1 -timeout=180s -json ./...`。
- 全量 `go test -race -count=1 -timeout=180s -json ./...`。
- `go vet ./...`、CLI 构建和实际执行 `version`。
- 现有真实系统凭据、Session、终端与文件安全证据检查。
- 以下本地工具命名测试；必须实际执行并通过，缺失或跳过均判失败。

| 验收范围 | 原生证据组 | 可观察结果 |
| --- | --- | --- |
| C1 Shell 与进程 | local-tools-process / model | 管道、重定向、cwd/env、真实退出、停止和子进程清理；模型调用生产工具 |
| C2 输出 | local-tools-process / model | 超内存加密输出、分页、跨边界检索和历史正文隔离 |
| C3 PTY | local-tools-process / pty-recovery / ui | 真实终端、输入、resize、同任务 Shell 状态、客户端操作及恢复只读 |
| C4/C5 大文件 | local-tools-large-edit / files / model | 大文件范围读取、精确编辑、授权、76,000 字节累计输入及逐字节读回 |
| C6 patch/diff | local-tools-model / files / controller / ui | 多文件授权及逐项结算、完整 diff、加密恢复和独立分页 |
| C7 检索 | local-tools-files / model | 2,501 项目录完整续页、变化失效、模型游标和界面 Activity |
| C8 复制 | local-tools-files / model | 33 MiB 二进制逐字节一致、递归目录、冲突和部分完成 |
| C9/C10 归档 | local-tools-model / controller / ui | 恢复目标、明确批准清理、真实结算、加密恢复及不可重放 |
| 任务生命周期 | local-tools-controller / ui | Session 切换、退出收尾、未知历史不重跑、模型忙时可操作面板 |

具体测试名和方法在 `clients/cli-go/scripts/cli-platform-evidence.ps1`，完整模块测试补充其它边界。
`cli-check-<OS>-<arch>-<SHA>` 保存完整测试 JSON、平台身份、vet 和 version 日志；
`cli-native-<OS>-<arch>-<SHA>` 保存严格命名测试及原生安全证据。CI 产物保留 14 天。
验收应核对运行的提交与待合入提交一致，不复用其它 SHA 的绿色状态。

模型验收使用脚本化模型响应和生产执行器，客户端界面验收使用实际 Update/View 和受控交互；
Controller 的 PTY 测试运行真实 shell、输入和加密恢复。它们不等价于云端模型质量测评、
人工桌面点击或所有跨挂载攻击的运行证据。不扩大 Windows 支持，不承诺重启后重附着旧进程。

## 落地顺序

任务分支推送后等待上述双平台检查；失败先定位、修复并更新绑定提交的证据。
全部通过后提交审查，由维护者合入基础分支，再关闭 Issue #1。
不得因仅 Linux 成功或 macOS 交叉编译成功而提前宣布双平台验收完成。
