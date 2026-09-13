# 浏览器学习入口（Issue #18）

## 确认与方案

核查基线为 `bc55267e023bb3b5cb77f9da61d81b44d5c6257d`，与工单一致，工作区初始无修改。
`server/internal/transport/httpapi/api.go:282` 仅提供设备 Bearer 配对；`:527–532` 强制从 Authorization 读取 Bearer。
`:266–365` 的路由没有 `/app/`，`clients/` 只有 cli-go；现有 adminui 是独立本机管理面。
因此缺失的是浏览器学习身份与可运行前端。目标管理、空资料保存、学习区生命周期已存在，复用其业务状态机、版本、幂等、权限和隐私边界。

架构：Go app 组合 identity、learningspace、learning/tutoring、knowledge、memory、privacy 和 HTTP/MCP；PostgreSQL 是权威状态源，CLI/TUI 是既有学习入口。新 Web 仅消费正式 HTTP，静态资源由同一个 Go 进程提供。没有 Node 业务服务。

拆分评估：保留一个用户结果，按三个有界批次串行实施，共享 OpenAPI、迁移和应用组合不并行修改。

1. 浏览器配对与学习身份：identity 内创建独立短期会话，同一事务消费配对码和登记设备，Cookie 仅含随机不透明值；每次认证查实时 token scope、设备撤销、过期与隐私代次。全局 Cookie 中间件保护所有非安全方法，拒绝 Cookie/Authorization 歧义。管理面和 MCP 不接受学习 Cookie。退出仅删除会话。
2. 真实学习页面：React/TypeScript/Vite、Tailwind/shadcn、TanStack Router/Query、React Hook Form/Zod。生成 OpenAPI 类型并使用其请求客户端，响应和输入另做运行时验证。实现首页、学习区管理、目标详情/历史/结构化编辑、设置与能力入口。尚无 Web 自动研究/教学、参考绑定流程的能力明确禁用，保存始终只调用目标 API。
3. 交付与验收：开发构建也复制 dist 到 Go embed 包；发行使用强制资产的构建标记，缺失 index/资源时失败。SPA 仅匹配学习页面，资源和 API 404 保留真实错误。追加迁移、identity 隐私清除和配置/部署说明。

## 数据与交互约定

服务端事实使用按 origin、设备 principal、隐私 generation、空间、目标及版本划分的查询缓存；请求上下文固定，不变更共享客户端的空间头。未提交正文仅标签页内存，刷新提示会丢失草稿；只有主题偏好可写 localStorage。退出/失效清理缓存和草稿；两个标签页的选区由各自 URL 决定。

生产配置固定 HTTPS Origin，Cookie 使用 `__Host-edu_web`、Path=/、Secure、HttpOnly、SameSite=Strict、无 Domain；HTTP 例外仅显式允许 loopback。CSRF 与会话绑定，非安全方法同时校验精确 Origin 和自定义头。不会信任转发头推断外部 Origin。

主题采用工单基线，正文 17px/1.8，系统字体；界面提供可见焦点、原生语义和有名称的字段。危险确认默认聚焦取消，文本输入支持 IME。能力关闭解释其原因，不提供假进度。

## 验收标准（实施前确定）

- 真实 HTTP 配对返回 HttpOnly Cookie，JSON/HTML 不返回长期 Bearer；Cookie 不能当 Bearer 使用。
- Cookie 非安全请求在所有 API 上必须有正确 Origin/CSRF；混用身份、错误 Origin、重复 Cookie、过期、撤销和收回 scope 均拒绝。
- 退出不撤销设备；重新启动服务可继续有效会话；隐私清除删除新增会话并阻止旧 Cookie 重用。
- 学习 Cookie 不获得 admin/internal/MCP 权限；原管理网络配置边界不变。
- 无模型、搜索、资料仍可创建真实目标；模型请求和教学会话创建为零；重启后可读取和修订。
- 学习区创建/编辑/归档/恢复，以及目标搜索/过滤/分页/历史/编辑/暂停/恢复/手动完成/归档可操作；完成必须输入依据且不新增 mastery。
- 跨区实体组合拒绝且不回落默认区；同区双目标、两个区、两个标签页的选择和草稿互不覆盖，迟到响应不导航到旧区。
- 空结果、断网、版本冲突和不支持能力分别显示；失败保留输入，重试保留操作身份。
- 浏览器验证深浅主题、390/768/1280/1440px 无整页横溢出、IME、键盘和确认焦点，计算真实渲染颜色对比度。
- 前端类型检查、测试、构建及 Go 受影响包/候选检查通过；Go 发行包含真实资产，缺资产失败，API 与资源 404 不返回 SPA。
- 实际执行的数据库/浏览器/构建证据记录在验收文档，未运行项明确列出。

不实现自动研究、模型运行、内容生成、完整课堂、本机工具或后续资料增强。
