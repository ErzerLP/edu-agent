# Issue #54：候选测试脚本执行权限

## 问题确认与开发方案

基线 `dc03439`，分支 `task/77`，初始工作树干净。项目由 Go 服务端与 PostgreSQL、
Go CLI/TUI、React Web、共享 `agentcore` 和外部集成组成；根目录 Makefile 提供开发
入口，候选 Shell 脚本负责数据库及 NoteSync 检查，独立 operations Go 模块协调证据。
本项仅涉及候选脚本的启动权限，无业务代码、依赖或数据结构变更。

修复前实际运行：

| 命令 | 结果 |
| --- | --- |
| `make postgres-candidate` | `Makefile:99` 报 `Permission denied`、`Error 127`，Make 退出码 2 |
| `make notesync-candidate` | `Makefile:109` 报 `Permission denied`、`Error 127`，Make 退出码 2 |
| `scripts/test-operations-candidate.sh --list` | Shell 报 `Permission denied`，退出码 126 |

`git ls-files -s scripts/*.sh` 显示三个脚本均为 `100644`，磁盘权限也均为 `0644`。
`Makefile:99、102、106、109` 直接执行脚本，
`scripts/test-notesync-candidate.sh:193` 也直接调用 PostgreSQL 脚本；
三个脚本第 1 行已有 Bash shebang，但缺少可执行位会在解释器启动前被操作系统拒绝。
因此仅修改 Make 的调用方式仍会遗漏嵌套调用和文档中的直接执行入口。

开发方案：将三个已确认受影响的脚本记录为 Git 模式 `100755`，保留脚本正文。
当前仓库 `core.filemode=false`，需显式更新这三个路径的索引模式，确保修复随检出交付。
这是一个独立的小批次，无需拆分；本项不运行数据库、NoteSync 或 Nocturne 完整候选矩阵。

## 验收标准

- 三个脚本的 Git 模式及新检出的文件均具有可执行位，正文摘要与基线一致。
- PostgreSQL 完整、恢复、分片及 NoteSync Make 入口均能启动脚本；受控缺少 Docker
  时报告脚本自身的依赖错误，不再报权限错误。
- PostgreSQL `--list` 与 operations `--list` 直接执行成功。
- 三个脚本的 Bash 语法检查、现有 operations 模块测试、vet、构建及差异检查通过。
- 明确区分入口验收与完整数据库／外部服务集成验收。

## 验收结果

2026-09-21，Linux amd64，Go 1.26.6。三个脚本仅发生 `100644 → 100755` 模式变化；
逐一用 `git hash-object` 对照 `git rev-parse HEAD:<路径>`，正文对象摘要与基线一致。

| 检查 | 结果 |
| --- | --- |
| 对三个脚本分别运行 `bash -n` | 通过 |
| `git ls-files -s` | 三个脚本均为 `100755` |
| 使用 `git checkout-index` 将 Makefile、三个脚本及 operations 模块检出到新临时目录 | 三个脚本磁盘模式均为 `0755`，`test -x` 全部通过 |
| 在新检出目录直接运行 PostgreSQL `--list` | 退出码 0，列出九个分片 |
| 在新检出目录直接运行 operations `--list` | 退出码 0，列出十一个候选通道 |
| 新检出目录的 `make postgres-candidate`、`make postgres-candidate-resume`、`POSTGRES_SHARD=db-core make postgres-candidate-shard` | 在受控 PATH 下均进入脚本并报告 `required command is unavailable: docker`，退出码 2，无权限错误 |
| 新检出目录的 `make notesync-candidate` | 同一受控 PATH 下报告 `NoteSync candidate: required tool not found: docker`，退出码 2，无权限错误 |
| operations 模块 `go test -count=1 ./...` | 通过 |
| operations 模块 `go vet ./...`、`go build ./...` | 通过 |
| `git diff --check`、`git diff --cached --check` | 通过 |

Go 检查使用 `GOPROXY=off GOTOOLCHAIN=local`，没有安装或升级依赖。
入口验收的 PATH 目录仅放置指向现有 `/usr/bin/bash`、`/usr/bin/dirname` 的符号链接，
通过 `env -i PATH=<受控目录> /usr/bin/make ...` 启动，稳定验证缺少 Docker 的诊断分支。
这是刻意隔离的启动检查，不表示宿主没有 Docker，也不表示数据库或 NoteSync 集成通过。

本次未启动 PostgreSQL、NoteSync、Nocturne 容器，未运行完整候选矩阵、全仓 Go 检查或
Web 发布检查；未修改这些业务实现。NoteSync 的数据库子调用也引用同一已修复为
可执行的 PostgreSQL 脚本，但没有将静态调用链核对表述为完整 NoteSync 流程已执行。
