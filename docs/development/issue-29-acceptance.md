# Issue #29 验收记录

日期：2026-09-18；分支：`task/50`；变更前基线：`1d0861d`。
开发方案、原因定位、限制及保存合同见 [文本层 PDF 设计](../design/import-pdf.md)。

## 先确认再实施

变更前执行：

```sh
cd server
go test ./internal/research -run '^TestFetchParsingLimitsAndRestrictions/PDF$' -count=1 -v
```

测试通过，但其断言是 PDF 返回 `unsupported_format`。结合基线的服务端 MIME 白名单、
Markdown-only 导入结构和 Web 文件类型过滤，确认能力确实缺失，而非另有设置。
改动后，损坏的 `%PDF` 仍拒绝（现在是格式错误），真实中英文多页 PDF 进入独立解析器。

## 已执行并通过

| 范围 | 证据与结果 |
| --- | --- |
| 解析器 | `go test ./internal/pdfsource -count=1`：真实中英文、重复页、混合图片页、字符/字节预算、真实密码文件、损坏/体积/页数限制、嵌入字体与非嵌入字体报告 |
| 资源与隔离 | 压缩输入约 164 KiB、解压 160 MiB，不能导入可用正文；取消/5 ms 执行期限可终止；包含 JavaScript/OpenAction 的 PDF 不执行脚本；新实例空文件系统、无网络挂载 |
| 领域/网络 | `go test ./internal/research ./internal/knowledge -count=1`：既有 SSRF/DNS/重定向边界、禁止留存、PDF 原件与页片段、部分采用确认、来源签名、客户端伪造正文/替换原件拒绝 |
| 真实 PostgreSQL | 完整 `migrations`、`knowledge/postgresstore`、`mentorrun`、`transport/httpapi` 包测试通过；迁移 28、预览不发布、确认发布、原件不可变、同名身份审阅/新版本、旧引用页图、冻结章节授权、公开协议与 cookie/CSRF |
| 教学链路 | `TestPostgreSQLPDFReferenceTeachingKeepsPhysicalPage`：仅选择第二页开课，模型仅收到该页原文，正式教学内容引用仍准确返回第二物理页和版本；`TestPostgreSQLPDFResearchExplicitPartialAdoption`：混合页不自动采纳、重启恢复加密附件、显式选择后发布 |
| 清除 | `TestKnowledgeRedactedRevisionTombstoneAllowsFreshImport`：真实 erasure barrier/scrub 清空原件与提取报告，旧页接口不可恢复；沿用已有索引/运行正文清除合同 |
| 附件限额 | `TestPDFAttachmentsDoNotExpandModelPayloadBudget`：1 MiB 原件可加密恢复；模型正文仍受原 256 KiB 预算；未知来源和超过抓取预算的附件拒绝 |
| 并发 | `go test -race ./internal/pdfsource -run '^TestPDFConcurrentCancellation$' -count=1` 通过，取消释放并发槽 |
| API/CLI | `go test ./api ./internal/learningstart`；CLI `go test ./internal/api ./internal/command`；CLI 严格 DTO 接受 PDF 来源扩展，规范文本回退可用 |
| Web | 类型检查、28 个单元测试（10 个文件）、OpenAPI 类型生成、生产构建通过 |
| 浏览器 | Playwright `pdf.spec.ts` 与原 `references.spec.ts` 共 2 项通过：真实上传、覆盖选择、预览确认、PNG 页图、方向键/前后页、390 px 移动视口无横向溢出、axe WCAG 2A/2AA/2.1AA 无违规 |
| 构建/静态检查 | Go 服务端 `web_release` 构建、CLI `CGO_ENABLED=0` 构建、受影响 Go 包 vet、`git diff --check` 通过 |

PostgreSQL 使用当前任务独占容器、测试 schema；没有使用生产或用户数据库。
最终 PG 专项补跑命令（`TEST_DATABASE_URL` 指向任务临时实例）：

```sh
go test -p=1 -count=1 ./internal/transport/httpapi
go test -p=1 -count=1 ./internal/knowledge/postgresstore ./internal/mentorrun \
  ./internal/privacy/postgresstore \
  -run 'TestPostgreSQLPDF|TestKnowledgeRedactedRevisionTombstone' -v
```

## 页图核对与发现的问题

使用仓库内真实三页 `bilingual-embedded.pdf.base64`（浏览器解码为原 PDF 后上传）。
代理检查浏览器截图：选中物理第 2 页，PNG 显示“第二物理页：版本引用保持准确”
和 “Second page: immutable source citation.”，与提取片段一致；第 3 页报告无文本层。
截图保存在本地 Playwright 输出 `pdf-page-two.png`，不提交测试输出或私人数据。

这一步实际发现并修复了两个问题：既有 CSP 拦截 PNG Blob，现仅允许图片 Blob；
非嵌入中文字体可能提取正确但页图缺字，现明确标注字体缺口并要求接受部分结果。
沙箱仍不读取宿主字体；嵌入字体的真实页图已再次核对。

## 中间失败与未运行范围

- 第一轮完整 PG 测试后段临时数据库退出，HTTP 包报连接 EOF/recovery；不能把该轮
  整体写成通过。重新建立磁盘型临时实例后，完整 HTTP 包和 PDF/清除专项补跑通过。
- 尝试整个解析器包 `-race` 时，插桩后的首次 WASM 编译超过生产 10 秒上限；未放宽
  生产限制来掩盖结果。普通解析/资源测试及定向并发取消 race 通过，完整解析器 race
  不列为通过。
- 生产 Web 构建有现有大 chunk 和第三方 PURE 注释警告；类型检查和构建均成功，未为
  清理警告改动无关分包。生成 API 类型有原 TutoringAction 判别字段警告。
- 未运行全仓 race、全仓全部 PG 分片、Compose/OCI、跨 OS 原生测试、真实外部搜索
  提供商付费调用、物理手机或辅助设备实测。外部网页通过真实 HTTP PDF fixture 核对。
- 上述页图为代理视觉核对，不冒充真人使用独立 PDF 阅读器的人工签收。提交审阅时应
  请审阅者对所用真实教材另做一次独立阅读器/页码/字体对照；不承诺任意 PDF 均可靠。
- 不实现 OCR、Office、大批任务原 PDF 上传、原件导出和复杂图表语义；单批 PDF 链路
  与 CLI 文本回退已覆盖，本项不扩大参考审批权限。
