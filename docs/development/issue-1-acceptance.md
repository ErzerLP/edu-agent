# Issue #1：最终验收入口

当前实现已覆盖 C1–C10；新增原生验收确认了 macOS PTY 收尾问题并修复。此前交接见
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

修复在前台假模型中隔离后台 observer/reflector 请求，并注册 Session 清理；
保留自动上下文模式和原预算断言。完整 macOS race 也在不限工具轮数测试中复现同一夹具问题，
因此隔离放在共用前台夹具中；后台整理由独立同步模型测试覆盖。
同一测试修复后 `-count=100` 通过。

首次新增 macOS 原生验收在提交 `4b38151` 复现：正常 PTY 退出仍返回
`cleanup_incomplete`，影响任务等待和 Session 关闭。`sessionHasMembers` 原先依赖
`kinfo_proc.e_sess`，但现代 Darwin 不再导出该内核指针；零值被误当作无法定位会话。
修复改用 `getsid` 的数值会话 ID，并继续要求未回收 leader 固定身份；查询失败或
残留成员仍保守报告，不在 leader 回收后追发信号。新增原生测试覆盖已退出但未回收的空会话。
依据：[Apple XNU fill_user64_eproc](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_sysctl.c)
与 [getsid](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_prot.c)。

全量原生检查另复现归档中符号链接清理失败：Darwin 的 `O_NOFOLLOW` 会拒绝链接，
即便同时设置 `O_SYMLINK`。修复对已确认的链接入口仅用 `O_SYMLINK` 打开链接本身，
父路径仍逐级拒绝链接，发布前仍校验身份。外部目标、相对及悬空链接由现有真实清理测试覆盖，
并新增为严格原生验收项。依据：[Apple open(2)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/open.2.html)。

Darwin 对僵尸进程的 getsid 查询可能返回 ESRCH；根进程组内僵尸仍计入残留，
其它组里已确认死亡的入口不再使所有无关 PTY 会话误报清理失败。无法确认的活进程仍保守报告。
此处不宣称能归属 Darwin 已丢失会话身份的其它组僵尸。单进程模型等待/取消测试显式使用
`exec sleep` / `exec cat`，不再依赖 Linux Shell 的末命令替换优化；真实子进程收尾测试保留。

并发原生运行进一步确认：收尾已经观察到退出、回收成功且进程组消失，仍因历史发信号错误
标记误报 `cleanup_incomplete`。最终结算改为依据当前实际证据；未退出、未回收、
查询失败或残留组/会话仍报告不完整。`TestStopSettlementOverridesSignalAttemptError`
在 Linux 稳定复现修复前失败，修复后通过并进入双平台指定验收。模型合同测试继续严格检查
最终清理错误；临时诊断已移除，不增加生产日志或环境开关。

完整模块检查还修正两项测试假设：恢复后的复制/清理测试沿用原夹具 30 秒工具预算，
避免 42 项持久结算被默认一秒截断；导入器大小写冲突测试在不能创建两个不同大小写文件的
文件系统上明确跳过（Linux 继续验证），不把 APFS 已合并为同一文件误称为接受了重复路径。
该平台条件测试不属于要求零跳过的本地工具指定验收组。

## 双平台自动验收

`.github/workflows/cli-platform.yml` 在 Linux 和 macOS 原生 runner 上运行：

- 全量 `go test -count=1 -timeout=10m -json ./...`。
- 全量 `go test -race -count=1 -timeout=10m -json ./...`。
- `go vet ./...`、CLI 构建和实际执行 `version`。
- 现有真实系统凭据、Session、终端与文件安全证据检查。
- 以下本地工具命名测试；必须实际执行并通过，缺失或跳过均判失败。

测试使用私有、规范化的 TMPDIR，避免 macOS 默认 `/var` 链接触发加密存储的路径保护。
CI 单包预算为 10 分钟：首次 Linux race 在 3 分钟上限时仍处理大输出加密/JSON，
并非数据竞争报告；本地该包约 99 秒。该预算只影响测试，不限制用户 Shell 执行。

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
