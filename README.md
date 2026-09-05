# edu-agent

`edu-agent` 是单用户自托管的通用个人学习助理。Go 服务和 PostgreSQL 保存知识、教学状态、学习事件与投影；Go CLI 提供启动即用的全屏 TUI，并保留显式子命令用于脚本；本机管理 WebUI 用于服务状态、客户端配对、设备撤销、Nocturne Memory 树浏览、知识库查看与导出，以及 NoteSync 配置和预览；Nocturne sidecar 保存经过准入的长期个人记忆。

## 使用入口

服务端默认只监听本机回环地址。显式启用 `ADMIN_UI_ENABLED=true` 并配置独立的 `ADMIN_UI_TOKEN` 后，浏览器访问：

```text
http://127.0.0.1:8080/admin/
```

浏览器会进入独立的中文登录页，用户名固定为 `admin`，管理密码使用 `ADMIN_UI_TOKEN`。它必须是 32 个随机字节的规范无填充 base64url 编码，并且必须与模型、Nocturne API、Nocturne 维护和 NoteSync API 凭据不同。登录表单关闭凭据自动填充，应用不会把原始密码写入 cookie、页面脚本、页面标记或浏览器存储；登录成功后服务端只创建 15 分钟的短期会话。WebUI 提供运行状态、配对码、设备筛选与撤销、客户端连接指引、只读 Memory 树、知识库树与 Markdown 导出，以及 NoteSync 连接配置和同步预览。NoteSync API Token 只写入权限为 `0600` 的服务器设置文件，从不由 API 回显；设置在服务器重启后原子生效。知识导入、知识提案审批和 NoteSync 冲突解决继续使用既有用户凭据与审计路径，管理会话不会冒充设备身份。直接运行服务端时，管理面要求 `LISTEN_ADDR` 为 loopback；若需要在 WebUI 保存 NoteSync 配置，还需为 `ADMIN_UI_SETTINGS_FILE` 指定绝对路径。Compose 仅因宿主端口明确绑定到 `127.0.0.1` 才设置 `ADMIN_UI_TRUSTED_LOOPBACK_PROXY=true`；不要在公开端口后复用这个设置。远程访问使用 SSH 端口转发。

构建客户端后，直接运行即可进入交互式仪表盘：

```bash
make cli-build
./clients/cli-go/bin/edu-agent
```

方向键或 `j/k` 移动，Enter选择；常用动作也有字母快捷键。中文设置页可在配对前调整客户端请求超时、输出颜色和本地AI模型，支持OpenAI、DeepSeek、OpenRouter、Ollama以及自定义OpenAI兼容端点。模型API Key按“提供商 + Base URL”绑定到平台凭据槽，不会进入 `config.json`，也不会跨端点复用；只有Ollama和无鉴权的自定义loopback端点可不配置Key，云端预设及远程自定义端点缺Key时失败关闭。旧服务器不可用时，可在重新配对页选择仅清除本地配对并保留模型设置。

“AI学习助手”是客户端Agent Loop，不会切换或重启服务端教学模型。它使用受限工具读取服务端知识、学习进度、路线、复习和已接纳偏好，不再对单次用户 turn 的模型工具轮数、单响应工具调用数量或总工具调用数设置固定上限；循环由模型最终回答、用户交互、取消、超时或上下文管理结束。输入、单个协议载荷、工具结果投影和上下文仍保留独立安全边界。模型文本在渲染前会移除终端控制字符；保存长期偏好前，TUI会在可滚动区域展示内容、理由、类别、敏感性和稳定性并获得明确确认。新 Agent Session 默认使用系统钥匙串保护的密钥自动加密保存；可在工作台“AI助手与模型”设置中切换，或用 `edu-agent model set --session-history auto|off` 控制后续新会话，`off` 不影响恢复和删除已有历史。TUI 空闲时按 `F2` 可在共享 Session picker 中搜索、恢复、重命名、二次确认删除或新建会话；切换后 YOLO、旧文件授权和未完成交互一律重置。Session 不按时间自动删除，达到硬上限也不自动淘汰，需用 `agent sessions delete/clear` 手动清理。`edu-agent agent --no-save` 仅关闭当前新会话的持久化且不改配置；自动标题会向当前模型端点发送有界、清理后的已提交用户文本和安全最终回答，恢复后的下一模型请求会向当前 provider 发送历史上下文，端点身份变化时必须先确认。恢复绑定旧工作区；旧 root 不可用时只恢复本地对话并禁用文件工具，绝不回退到当前目录，且历史文件正文不代表磁盘当前内容。平台密钥服务不可用时只降级为明确的未保存状态，不写明文。`clear` 只清除本地 Agent Session store，不清除服务端事件、Nocturne、终端 scrollback、Shell history、provider retention 或 OS backup。`TERM=dumb`、stdin/stdout非TTY或传入显式子命令时不会启动全屏界面。

## 开发入口

开始需求设计、实现、测试或验收前，先阅读：

1. [`docs/development/README.md`](docs/development/README.md)
2. 当前 Comet change 的 brief/spec
3. 当前 capability 的 `docs/design/` 文档

项目采用垂直交付、分层测试和证据复用。用户可见需求变化通过 Comet Native 返回 Shape，不能只在实现或测试中隐式修改。

## 常用命令

```bash
make test
make vet
make build
make check
```

真实 PostgreSQL 测试需要显式配置 `TEST_DATABASE_URL`，未配置时的 skip 不代表数据库行为通过。CLI 和服务端分别位于 `clients/cli-go` 与 `server`。
