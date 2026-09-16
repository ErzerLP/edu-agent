# 版本化学习内容与教学工作区（Issue #23）

## 缺失确认与架构

基线 `e0dd3bd`，工作树初始干净，已包含 #18、#21、#22。
`clients/web/src/main.tsx:69` 的路由树只有首页、学习区、目标、研究和设置，没有教学或内容页面；
`server/internal/transport/httpapi/web.go:236` 的 SPA 白名单也不接受内容地址。
`server/internal/learning/domain.go:179` 的 Activity 在 `:190` 只保存 prompt；
迁移截至 `000022_goal_research.sql`，没有学习内容 Artifact/Revision。
`server/internal/learning/store.go:220` 已提供完整 SessionWorkItem，
`server/internal/tutoring/state.go` 与 `learning/command.go` 已拥有正式答案、反馈、自由问答和返回焦点。
因此缺失的是独立内容协议及 Web 教学入口，不能通过已有设置启用，也不需要重建教学状态机。

项目是模块化 Go 服务、PostgreSQL 权威状态、同源 React/TypeScript Web、独立 Go CLI。
learning 管事件、提案、评分与投影，tutoring 管会话与焦点，knowledge 管正规资料，
identity 管设备和浏览器身份，privacy 管代次、响应屏障与清除。
learningcontent 是新增正文 owner，复用 learning 的隐私代次屏障和清除回执，正文 SQL 留在自身模块。

## 开发方案（实施前）

拆分评估：此项只有“在真实教学会话中阅读并继续学习”一个主要结果，内容与作答必须一起交付。
共享迁移、OpenAPI、应用组合串行实施，按三个有界批次完成。

1. 内容闭环：新增 Artifact、不可变 Revision、递归 block 和独立 interaction；以正规 Activity 确定性适配旧内容。
   提供协议能力、按会话读取、授权提交、指定版本与历史。内容绑定区、目标、会话、活动、资料、模型与输入指纹。
   提交使用操作身份、版本比较、设备权限及隐私代次；草稿/失败版本不能替换正式活动。旧 DTO、活动 ID、事件及 rubric 不变。
   正文和引用 AES-GCM 加密，复用独立挂载的 MENTOR_KEY_FILE，缺密钥明确受限，不落明文。
2. Web 闭环：目标页选择既有教学会话；学习页按服务端 allowed_actions 展示正式答案、帮助、反馈、讨论和续学。
   按固定来源会话查询，响应丢失核对原操作/原会话，不自动重发答案。安全块渲染、来源弹层、内容历史与版本深链接。
   草稿只在标签页内存按身份/区/会话/活动隔离；布局适配 390/768/1280/1440，主题、IME、焦点恢复和长内容可操作。
3. 验收闭环：同步 OpenAPI、配置及使用文档；运行领域、存储、HTTP、旧 DTO、前端类型与单测，
   再用隔离 PostgreSQL 和真实浏览器完成教学、隐私、响应丢失与布局回归。记录未运行范围。

## 验收标准（实施前）

- 既有教学会话能选择、阅读原题与资料、正式作答、查看反馈并续学；自由问答能返回原焦点。
- 内容有独立 ID、不可变版本、稳定 block_id、来源及生成依据；刷新和指定版本返回同一正文，历史可达。
- Markdown/code/math/table/citation/callout/question/answer_input/group 可自由排序组合；不依赖固定课程模板。
- 未提交、失败、不合法或不完整内容不能正式作答；未知展示块有完整文本回退，未知交互明确受限。
- 展示修改不改变题意、原 activity/rubric/历史身份；旧严格 CLI DTO 仍可解码。
- 正式答案与聊天分别提交；Ctrl+Enter 与 Enter/Shift+Enter 有独立语义，IME 不发送。
- 操作丢响应后只核对原身份/原会话，冲突不盲目补发；两标签页、切区及迟到响应不混入其他草稿。
- 所有版本正文与引用加密；隐私清除包含内容、引用、运行增量和缓存，旧版本及迟到提交不能复活。
- 危险 HTML/链接/资源请求与公式扩展被阻断；来源内容不执行。
- 390/768/1280/1440 与深浅主题无整页横溢出；弹层焦点恢复，分栏可键盘调整，上滚暂停追底。
- 追加迁移、OpenAPI、真实 PostgreSQL/浏览器和旧数据回归通过；不把 skip 当作通过。

不实现代码执行、数学等价判定、音视频、本机工具、完整评估复核或进度总览。
