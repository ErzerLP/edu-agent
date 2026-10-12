# Notebook Studio 3a 实施计划

依据：[设计](notebook-workspace.md) 批次 3、G4/G6 与 2c 合同；[spec](../comet/specs/notebook-workspace/spec.md)。本计划在当前 worktree 内串行执行，不提交、不安装、不实现生成或正式测验。

结果：无目标区也能明确保存、重开与编辑笔记，指定已保存版本经过原 knowledge preview/confirm 成为独立来源。

- [x] 存储与 owner：新增 `learningcontent/studio.go`、`studio_store.go`、`000035_studio.sql`；`StudioSave(ctx, actor, id, StudioSave)` 创建或追加，`StudioGet/StudioList/StudioHistory/StudioOperation` 读取。先以 HTTP 缺路由及 DTO 测试证伪，再实现。写入核验操作身份、期望版本、隐私代次、区和 goal；加密标题／正文／来源清单／引用／模型与输入指纹。新表接入 `RedactTx/Remaining/RotateKeyTx`，升级测试验证旧约束不变。
- [x] G6 与 HTTP：新增 `knowledge/note_source.go` 正式读取端口，服务端取得指定 note 版本后签发绑定正文 hash、knowledge 代次与派生身份的来源回执。`httpapi/studio.go` 注册列表／创建／读取／保存／历史／操作核对／import-candidate；更新区路由分类、OpenAPI、代理白名单和合同测试。原导入请求不新增客户端可写 provenance。
- [x] Web：新增 `api/studio.ts`、`studio.test.ts`、`studio-note-editor.tsx`，改 Studio 面板和原导入控件的候选输入。草稿使用现有标签页内存 DraftStore；保存未知先查原操作，冲突保留本地内容并显式读取新版比较。来源导入复用同一 ReferenceImport 控制器。
- [x] 验证：先新增真实 PostgreSQL 用例，覆盖版本并发、重放、隐私清除不可复活、轮换和派生导入；新增 Chromium 场景覆盖保存／刷新／冲突／快照不漂移。按用户指定局部包、前端 check/build/vitest 和 notebook 回归各通过一次即停。
- [x] 文档：设计文档记录 3a DTO、权限、错误、版本与清除规则、文件和证据；勾选 3a，保留 3b/3c 未完成。

边界：手动 note 无模型、来源清单为空且不产生引用；后续生成必须用 G3 SourceManifest 与重新核验引用，不能把当前选择直接写为可信依据。保存请求只开放 note，旧 content／mentor／CLI enum 不变。读取笔记快照和签发回执在 HTTP 隐私 read permit 下进行；全局清除使旧回执失效，已导入副本随后由 knowledge owner 管理。
