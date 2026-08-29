# edu-agent

`edu-agent` 是单用户自托管的通用个人学习助理。Go 服务和 PostgreSQL 保存知识、教学状态、学习事件与投影；Go CLI 提供启动即用的全屏 TUI，并保留显式子命令用于脚本；本机管理 WebUI 用于服务状态、客户端配对和设备撤销；Nocturne sidecar 保存经过准入的长期个人记忆。

## 使用入口

服务端默认只监听本机回环地址。显式启用 `ADMIN_UI_ENABLED=true` 并配置独立的 `ADMIN_UI_TOKEN` 后，浏览器访问：

```text
http://127.0.0.1:8080/admin/
```

浏览器会进入独立的中文登录页，用户名固定为 `admin`，管理密码使用 `ADMIN_UI_TOKEN`。它必须是 32 个随机字节的规范无填充 base64url 编码，并且必须与模型、Nocturne API、Nocturne维护和NoteSync API凭据不同。登录表单关闭凭据自动填充，应用不会把原始密码写入cookie、页面脚本、页面标记或浏览器存储；登录成功后服务端只创建15分钟的短期会话。WebUI不展示或保存任何服务密钥，只提供运行状态、配对码、设备筛选与撤销和客户端连接指引。直接运行服务端时，管理面要求 `LISTEN_ADDR` 为loopback。Compose 仅因宿主端口明确绑定到 `127.0.0.1` 才设置 `ADMIN_UI_TRUSTED_LOOPBACK_PROXY=true`；不要在公开端口后复用这个设置。远程访问使用 SSH 端口转发。

构建客户端后，直接运行即可进入交互式仪表盘：

```bash
make cli-build
./clients/cli-go/bin/edu-agent
```

方向键或 `j/k` 移动，Enter选择；常用动作也有字母快捷键。中文设置页可在配对前调整客户端请求超时、输出颜色和本地AI模型，支持OpenAI、DeepSeek、OpenRouter、Ollama以及自定义OpenAI兼容端点。模型API Key按“提供商 + Base URL”绑定到平台凭据槽，不会进入 `config.json`，也不会跨端点复用；只有Ollama和无鉴权的自定义loopback端点可不配置Key，云端预设及远程自定义端点缺Key时失败关闭。旧服务器不可用时，可在重新配对页选择仅清除本地配对并保留模型设置。

“AI学习助手”是客户端Agent Loop，不会切换或重启服务端教学模型。它使用受限工具读取服务端知识、学习进度、路线、复习和已接纳偏好，并对输入、模型回答、工具调用、工具参数、工具结果和上下文设置硬上限。模型文本在渲染前会移除终端控制字符；保存长期偏好前，TUI会在可滚动区域展示内容、理由、类别、敏感性和稳定性并获得明确确认。`TERM=dumb`、stdin/stdout非TTY或传入显式子命令时不会启动全屏界面。

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
