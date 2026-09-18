import { z } from 'zod'

export const pdfPageSchema = z.object({
  number: z.number().int().min(1).max(100),
  status: z.enum(['text', 'partial', 'no_text', 'failed']),
  gaps: z.array(z.string()),
  text: z.string(),
  fingerprint: z.string(),
  duplicate_of: z.number().int().optional(),
  start: z.number().int().nonnegative(),
  end: z.number().int().nonnegative(),
})
export const pdfReportSchema = z.object({
  parser: z.string(),
  fingerprint: z.string(),
  page_count: z.number().int().min(1).max(100),
  coverage: z.enum(['complete_text', 'partial_pdf']),
  pages: z.array(pdfPageSchema),
})
export const pdfMetadataSchema = z.object({
  kind: z.enum(['uploaded_pdf', 'web_pdf']),
  locator: z.string(),
  report: pdfReportSchema,
  ranges: z.array(
    z.object({ number: z.number().int(), range: z.object({ start: z.number(), end: z.number() }) }),
  ),
})
export type PDFReport = z.infer<typeof pdfReportSchema>
export const pdfStatus = {
  text: '已提取文本',
  partial: '文本有缺口',
  no_text: '无可用文本层（扫描或空白页）',
  failed: '此页解析失败',
}
export const pdfGaps: Record<string, string> = {
  text_limit: '超过文本预算，余下内容未纳入',
  unicode_mapping: '部分字符无法可靠映射',
  images_tables_formulas_or_layout: '图片、图表、公式或版式未可靠解析',
  layout_not_verified: '版式未核验',
  font_not_embedded: '字体未嵌入，安全页图可能缺字或使用替代字形；须核对原件',
  no_text_layer_or_blank: '未运行 OCR，不将空文本视为有效解析',
  no_extractable_text: '无可提取文本',
  page_parse_failed: '页解析失败',
  text_decode_failed: '文本解码失败',
}
