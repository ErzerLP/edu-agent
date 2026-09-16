# 目标内导师持久运行（Issue #21）

## 缺失确认与架构

工作树起点 `81a3031`，包含 #18、#19、#20，初始无修改。`server/internal/settings/service.go:226` 将 WebMentor 固定为 `not_implemented`；`server/internal/transport/httpapi/api.go:329–353` 只挂载原目标和教学会话接口，无 runs/events/commands/operations；迁移截至 `000020_web_identity.sql`。共享核心 `packages/agentcore/runner.go:95` 要求宿主提供历史、工具和上下文，不拥有持久 worker。原 `server/internal/integrations/tutormodel/adapter.go:33` 只同步生成教学提案，不能恢复后台运行。因此缺失确实存在，不能通过已有设置开启。

架构为模块化 Go 服务、PostgreSQL 权威状态、共享 Go Agent 核心和同源 React 客户端；CLI 独立。复用 identity 的设备与 scope、learning 的明确目标、privacy 的代次屏障、settings 的秘密与网络策略。新增运行归 learning 隐私 owner，避免引入第二套身份或目标。

## 开发方案（实施前）

拆分评估：队列、传输、隐私与面板共同构成一次可恢复导师交流，不独立交付空 worker。按以下有界批次串行完成公共 schema、应用组合与前端。

1. 运行闭环：追加迁移，保存会话绑定、运行元数据、操作回执、加密 checkpoint 和有界事件 outbox。创建显式绑定 space/goal/session、目标版本和 operation_id，固定规范载荷摘要。读取真实绑定目标，不使用 current。短事务检查权限、生命周期、代次、版本；模型调用在事务外。租约使用数据库竞争与 fencing；已发出但未确认的调用记录结果/费用不确定，恢复不自动重放。
2. 共享核心执行：只注册读取目标、问询与无业务写入的交流确认。checkpoint 保存已提交消息、待交互身份和未处理调用；旧交互不能批准新候选。每次模型调用前扣减用户请求/Token 额度，预算暂停保留结果，只有显式继续才能增加额度。支持取消、失败和部分结果。流式文本合并并限制总大小，不保存推理。
3. 传输与恢复：新增 OpenAPI 的目标 runs、运行快照/events/commands、operations 查询。快照和 watermark 一致，事件单调序号；过期游标要求重新取快照。SSE 仅观察，每批输出重新检查身份与隐私，配置心跳/空闲写超时并关闭代理缓冲。
4. 隐私与 UI：保存模式正文服务端 AES-GCM 加密，密钥文件与数据库分离；默认七日期限、存储限额、显式清除及全局隐私清除。临时模式正文仅驻留进程内，重启保留回执但明确无法恢复正文。目标详情恢复当前会话/运行，显示阶段、活动、增量、交互、预算、停止与错误；共享有界订阅、上滚停止追底、IME 不误提交，技术 ID 折叠。仅目标内交流，不自动开学、改目标或写掌握度。

## 验收标准（实施前）

- 浏览器 → 真实 Go API → 共享 Runner → HTTP 模型 fixture → PostgreSQL 结果，并可刷新、断线和重启恢复。
- 明确目标读取、跨区/跨设备隔离；只读工具白名单拒绝 Shell、SQL 和未知工具。
- 同 operation 重试返回同一回执，载荷不同冲突；命令版本和交互身份过期拒绝。
- PostgreSQL 双 worker 有效租约唯一、过期租约 fencing；调用已发出的崩溃不静默重放，独立标记未知结果与费用。
- 快照 watermark 后续读无缝，重复/乱序可恢复，过期游标明确 resync_required；订阅不调用模型。
- 预算暂停/拒绝后不启动新调用，停止可观察取消中并终止模型；部分结果和失败可区分。
- 暂停/归档/撤销/隐私清除与队列、模型在途、响应发送竞态受事务保护；无失效业务写入。
- 保存模式数据库正文不含明文，临时模式不写正文，重启语义一致；清除包含 checkpoint、事件与内存缓存。
- 前端切区/切页卸载订阅，迟到事件不写其他上下文；输入、可访问状态和小屏可操作。
- 受影响 Go 包、迁移/OpenAPI、前端类型/单测/构建、定向 race 与真实 PostgreSQL/浏览器验收；真实提供商 smoke 单列，未运行不计通过。

本次按专用工作树边界交付审核，不合并或推送基础分支。
