# Issue #41：PDF Race 超时修复与验收

## 确认与原因

基线为 `d591bcc`，相对报告中的 `4913be9` 仅增加验收文档。环境为 Linux amd64、
Go `go1.26.6`、8 个逻辑 CPU；初次复现前一分钟负载为 `0.15`，工作树干净。

在 `server` 执行原报告的精确命令：

```bash
go test -race -p=1 -count=1 ./internal/pdfsource ./internal/knowledge ./internal/research -run '^(TestStaticAndEncryptedPDF|TestPDFPreviewRequiresRealBytesAndPartialConsent|TestFetchPDFOriginalPagesAndRestrictions)$'
```

三项分别在 18.69、18.52、27.55 秒失败，均为 `pdf_timeout`。移除 `-race` 的同一
命令通过，三个包分别耗时 2.249、2.247、2.181 秒。非 Race 对照在第三项 Race
用例期间启动，因此第三项的耗时不能作为完全独占机器的测量；前两项已独立复现。

原因是初始化阶段与文件执行阶段共用了超时预算：

- 基线 `server/internal/pdfsource/pdf.go:91` 先设置 10 秒期限，`:107` 才进入
  `webassembly.Init`，首次调用须编译内嵌可信 WASM。
- go-pdfium `v1.20.0` 的 `webassembly/webassembly.go:149` 调用
  `runtime.CompileModule`；wazero `v1.12.0` 的
  `internal/engine/wazevo/engine.go:264` 默认走单线程编译，函数循环没有检查取消。
  因此冷编译可以超过期限，随后由本地 `pdf.go:103` 映射为 `pdf_timeout`。
- 独立探针使用同一沙箱配置和共享编译缓存，仅初始化并关闭池、不提供 PDF：
  Race 冷编译为 18.704 秒，缓存命中后的初始化为 0.314 秒。证明失败发生在
  PDF 正文处理之前，不是夹具内容无效，也不是已检测到数据竞争。

## 开发方案与验收标准

项目为 Go 模块化服务端、PostgreSQL、Go CLI、React Web 和共享 agentcore。
PDF 沙箱由 knowledge 的上传/导入与 research 的网页抓取共用；文档身份和版本
仍由 knowledge 管理，网页网络与留存边界仍由 research 管理。

本次单批次修复解析器初始化顺序，不改变接口、数据库、依赖版本或客户端。

1. 抽取相同的沙箱池配置，提供 `pdfsource.Prepare`，在正式服务启动和三个相关
   测试包的 `TestMain` 中预编译可信 WASM；只保留已有机器码缓存，关闭临时池。
2. 每个 PDF 仍创建独立实例；保留 10 秒期限、等待并发槽计时、调用方取消、
   128 MiB 线性内存、输入/页数/文本/像素上限和空文件系统。
3. 验收原三个 Race 用例，以及取消、真实执行超时、资源攻击和正常解析/渲染；
   补充准备阶段上下文取消不影响后续文件实例的回归。
4. 精确回归通过后执行相关包测试、`make test-race`、相关 vet 和服务端 Go 构建。

## 验收结果

修复后执行精确回归（在原三项基础上加入取消、执行超时和新增的上下文隔离测试）：

```bash
cd server
go test -race -p=1 -count=1 ./internal/pdfsource ./internal/knowledge ./internal/research -run '^(TestStaticAndEncryptedPDF|TestPDFPreviewRequiresRealBytesAndPartialConsent|TestFetchPDFOriginalPagesAndRestrictions|TestPDFPreparationContextDoesNotOwnFileInstances|TestPDFExecutionDeadline|TestPDFConcurrentCancellation)$'
```

三个包全部通过，总耗时分别为 23.292、21.051、20.261 秒，包含测试开始前的可信
编译。原测试中的文本页、部分导入确认、物理页序断言均保持原样。

| 检查 | 结果 |
| --- | --- |
| 上述精确 Race 回归 | 通过 |
| `go test -count=1 ./internal/pdfsource ./internal/knowledge ./internal/research ./internal/app` | 通过，包含正常解析/渲染、加密、输入/页数/文本/解压限制等已有回归 |
| `go vet ./internal/pdfsource ./internal/knowledge ./internal/research ./internal/app` | 通过 |
| `go build -o /tmp/edu-agent-issue41.CHMWoQ/edu-agentd ./cmd/edu-agentd` | 通过；仅 Go 服务端构建，不含 Web 生产资产 |
| `make test-race` | 共享模块、服务端、CLI 全部通过，退出码 0 |
| `git diff --check` | 通过 |

全量 Race 中 pdfsource、knowledge、research 三个包分别为 38.338、33.361、
22.223 秒，包含各自准备阶段和完整包测试；未放大文件超时，也未为 Race 跳过 PDF
断言。新增回归确认准备上下文取消后，新文件仍能提取原文，且已取消的准备请求
返回明确错误。

## 范围与限制

服务启动会承担一次可信编译耗时；本次测量普通构建约 2 秒、Race 约 19 秒，
实际时间取决于机器和负载。编译不读取用户文件，也不因请求数量重复创建缓存。
底层单线程编译仍不能及时响应取消；准备阶段返回前检查调用方取消，文件阶段继续
使用原有可中止执行的沙箱。未升级或修改上游依赖。

本轮未配置 `TEST_DATABASE_URL` 和 `RESEARCH_LIVE_SMOKE`；数据库、真实网页以及
真实外部服务检查的跳过不算通过。没有修改 Web，本轮不运行 Web 发布构建或浏览器
矩阵。分支交付供审查，不推送或合并基础分支。
