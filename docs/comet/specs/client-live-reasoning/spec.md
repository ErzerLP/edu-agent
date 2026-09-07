# 实时推理展示

状态：生产路径及 Linux 受影响包门禁完成；尚无 macOS 原生运行证据。发布与安装版本以 Git 提交及客户端 `version` 输出为准。

## 用户合同

- 前台 Agent 使用 Ctrl+O 同时展开/收起执行详情和模型接口实际返回的可读推理文本或摘要，默认折叠。不要求单独 Ctrl 键，不改变取消、退出或文件批准快捷键。
- 推理随 SSE 增量显示，并归属于同一次模型请求的思考卡；不能创建第二张悬空卡，也不能混入最终回答或工具参数。
- 只展示接口已经提供的文本；不推断、补写或请求模型生成虚假的内部思考。加密、签名、redacted 或未知结构不作为可读正文显示。
- 没有可读推理时明确提示未提供；展示请求阶段、耗时及最近收到响应正文数据的时间。心跳属于真实接收活动，不代表已经收到回答或推理正文。
- 推理正文仅在当前 TUI 运行内存中保留，不进入 checkpoint、持久 transcript、Source/Observation/Reflection、自动标题、日志、长期记忆或后续模型请求。会话切换、恢复与退出不恢复这部分内容；显示到终端后不承诺清除外部录屏或终端记录。
- 采用有界的内存展示缓存，回收旧内容或裁剪时明确披露，不因显示缓存耗尽而中断模型请求、修改工具执行或推进未返回的文件游标。
- 正常完成后可查看尚保留的推理；取消、超时或失败保留已收到的部分并明确标记请求终态。过期 turn/generation 的增量必须拒绝，不能串入新请求或会话。
- 接口的 SSE 完整性、无响应超时、工具组装、原始参数隔离、授权与不重放合同保持不变。非流式兼容回退不伪造推理增量。

## 实施与验证边界

优先支持 reasoning_content、reasoning、thinking 的纯文本，以及 reasoning_details 中明确标注 reasoning.text/text、reasoning.summary/summary 的可读字段；同帧别名择一，避免重复展示。仅通过专用易失事件投递，返回的模型 Message 不加入推理字段。UI 展示总正文预算复用现有 1 MiB 助手正文防护规模，优先回收旧请求正文，当前请求超限保留前缀并提示；这不是模型配置上限。

验证覆盖真实 SSE 到事件、模型消息/Source/checkpoint 隔离、Ctrl+O 默认折叠及滚动、正文安全呈现、缓存裁剪、取消/失败/切换和迟到事件。仅使用本地假模型，不调用付费服务。

## 本批验证证据

- modelclient、agentloop、agentui、agentcontroller、command 五包全量测试及 vet 通过；UI 增量合并后复用未变三包证据，并重跑 agentui/command 全包与 vet。
- 四包 TestLiveReasoning 及相关思考生命周期、真实 Esc、SSE 心跳/无响应/取消定向 race 通过。覆盖可读字段与不透明字段隔离、同请求增量合并及终态顺序、忙时 Ctrl+O 和滚动位置、UTF-8 前缀裁剪、真实加密 Store 重启恢复与 no-save、实际自动标题请求无推理正文。
- Linux 完整 CLI 构建、version 与帮助命令实际运行通过，Darwin/arm64 完整 CLI 交叉构建通过。产物和输入清单位于 `/tmp/edu-agent-live-reasoning.J4VBgz`；源码清单摘要 `8f464a23a01db26dabba5629d6efe0aed4befc188e0189d956a21026c5de683e`。
- 没有调用实际付费 provider，也没有 macOS 原生或 Runtime 验收；本批功能不修改用户配置。
