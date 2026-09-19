# 可选本地 companion（Issue #38）

## 缺失确认与架构

初始工作树干净。`clients/cli-go` 执行 `go run ./cmd/edu-agent companion --help`
返回 `unknown command companion`（程序退出码 2）。全库无 companion 实现或开关。
`server/internal/mentorrun/core.go:51–78` 只注册在线领域工具；
`clients/cli-go/internal/workspace/workspace.go:122`、`mutation.go:46/289`
和 `localexec/manager.go:187` 已提供原生读取、冻结修改和任务执行。
根因是这些 CLI 内部 provider 没有独立适配、认证通道、浏览器授权及展示。

架构是 Go 服务和 PostgreSQL 负责学习身份/会话/运行，独立共享 Go 模块负责模型循环，
React 同源 Web 负责交互，CLI internal 负责本机工作区、进程组和 PTY。
本项不将原生 provider 编译进服务端，也不读取 CLI 配置、钥匙串或历史。

## 开发方案与批次

这是一个贯穿三端的单一结果。按通道和本地适配、Web 操作面板、导师接线串行实施；
共同的协议先确定，不并行修改公共接口。

1. 默认关闭 `COMPANION_ENABLED`。独立 `edu-companion` 程序主动轮询服务端 HTTPS，
   不监听本地控制端口。Origin/Host 精确匹配；仅显式 loopback 开发允许 HTTP。
2. 浏览器创建短期一次性配对码，用户在本机输入；本机显示站点、设备、OS 用户、
   工作区和原生 Shell 风险并明确确认。浏览器再检查设备信息，独立授权文件、Shell 和模型外发。
   通道凭据、浏览器能力凭据与学习 Cookie 分离；凭据只在进程/标签页内存保留。
3. 授权绑定学习设备、隐私代次、学习区和单个真实对话。通道每次请求有递增序号，
   丢失响应后只查询回执，不重新投递命令或输入。操作保留 device/conversation/run/task 身份。
4. 复用 workspace 的 list/read/stat 与 write/edit 冻结预览、版本复核和提交；
   模型不能批准文件发布。复用 localexec 的 Shell、任务列表/状态/输出分页、停止、输入及 PTY 控制。
   Shell 没有新增命令白名单或路径沙箱，文件权限不限制 Shell。
5. 页面定时续期；切换对话、撤销或退出停止原授权任务。断网允许短暂查询恢复，
   租约到期本机收尾。服务重启使旧授权失效，必须重新配对，不恢复旧 PID 或重放操作。
6. 原始命令/env/input 不持久化。输出仅内存有界保存；模型外发需独立授权，
   文件/终端内容会经学习服务器传输。退出码、输出缺口、输入确认字节与保存状态分别展示。

## 验收标准

- 关闭开关、未配对、未授权、跨设备/对话、错误 Origin/Host/凭据及重放均不能执行。
- 双端配对后执行身份与 UI 一致；撤销、过期和重启失效，服务器没有原生执行 fallback。
- 真实工作区读取、冻结修改、拒绝越界/链接/过期版本；审批只消费一次。
- 真实长任务、分页、管道输入、部分输入和 Linux PTY 控制通过原 provider。
- 相同操作/丢失响应不会重复命令或 stdin；结果未知可查询原身份，不能伪称成功。
- 页面切换及退出遵守有界清理合同，无法证明清理完整时如实告知。
- 模型工具仅在明确授权时注册；参数不被持久历史自动保存。
- 受影响 Go 测试、定向 race、vet、Web 类型/单测/构建与最小端到端通过。
- Linux/macOS 原生证据分开记录；交叉编译不代表 macOS 运行验收。

安装、撤销及实际验收结果随实现补齐；没有完整证据前不宣称全部平台验收通过。
