# PDF 验收夹具

`bilingual-mixed.pdf` 是本项目生成的真实三页 PDF，使用标准 STSong 字体、显式
ToUnicode 映射及规范交叉引用表。第 1、2 页各有中文和英文原文，第 3 页只有红色
图片。用途是核对真实上传、物理页码、扫描页缺口和安全栅格页查看；不是 OCR 结果。

其他解析/资源攻击夹具由 `internal/pdffixture` 在测试中构造，避免依赖外部服务。
原件来源许可与项目相同，不含私人材料。可使用独立 PDF 阅读器核对文字和页序。

`bilingual-mixed.pdf` 保留非嵌入字体，另用于复现沙箱缺字风险。
`bilingual-embedded.pdf.base64` 由本项目构造 Identity-H/CIDToGIDMap 的三页文件，再通过
Ghostscript 10.02.1 的 pdfwrite（EmbedAllFonts、SubsetFonts）生成，嵌入
DroidSansFallbackFull 与 DejaVuSans 的字体子集；
浏览器测试解码后真实上传，用于核对中文原页。字体由 Google 提供，采用 Apache-2.0，
许可见 `LICENSE.droid`，英文字体许可见 `LICENSE.dejavu`。
生成工具只用于准备测试资产，不是生产解析依赖。
# 加密夹具许可

`encrypted.pdf.base64` 是 go-pdfium v1.20.0 的
`shared_tests/testdata/encrypted_hello_world_r3.pdf` 的 Base64 编码，
用于验证不接受密码文件；保留原项目 MIT 许可于 `LICENSE.klippa`。
