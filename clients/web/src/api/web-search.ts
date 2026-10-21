import { z } from 'zod'
import { pdfReportSchema } from './pdf'
const sourceIdentitySchema = z.object({ collection_id: z.uuid(), revision_id: z.uuid(), document_id: z.uuid(), document_revision_id: z.uuid() })

const counter = z.number().int().nonnegative()
const hash = z.string().regex(/^[a-f0-9]{64}$/)
export const webCitationSchema = z.object({
  id: z.string(), origin: z.literal('web'), expired: z.boolean().optional(), document_id: z.uuid(), revision_id: z.uuid(),
  fragment_id: z.uuid(), document_sha256: hash, slice_sha256: hash, quote: z.string(),
  page: z.number().int().min(1).max(100).optional(),
})
export type WebCitation = z.infer<typeof webCitationSchema>
export const webProgressSchema = z.object({
  failures: z.array(z.object({ resource_id: z.uuid(), title: z.string(), category: z.string(), reason: z.string() })).optional(),
  queries: z.array(z.string()).optional(),
  stage: z.string(), query: z.string().optional(), title: z.string().optional(), notice: z.string().optional(),
  read_attempted: counter, read_succeeded: counter, read_failed: counter, fetch_http_requests: counter,
})
export type WebProgress = z.infer<typeof webProgressSchema>
export const webDocumentSchema = z.object({
  document_id: z.uuid(), resource_id: z.uuid(), revision_id: z.uuid(),
  request_url: z.url(), url: z.url().refine(value => /^https?:\/\//.test(value)), title: z.string(),
  fetched_at: z.string(), media_type: z.string(), parser: z.string(), coverage: z.string(),
  fingerprint: z.string(), document_sha256: hash, storage_allowed: z.boolean(), text: z.string(),
  fragments: z.array(z.object({ id: z.uuid(), text: z.string(), start: counter, end: counter, page: counter.optional() })),
  pdf: pdfReportSchema.optional(), next_cursor: z.string().optional(),
})
export const webPreviewSchema = z.object({
  preview_id: z.uuid(), preview_version: z.number().int().positive(), expires_at: z.string(),
  items: z.array(z.object({ document_revision_id: z.uuid(), hash, coverage: z.string(), requires_partial_accept: z.boolean(), document: webDocumentSchema })).length(1),
})
export const webImportSchema = z.object({ operation_id: z.uuid(), run_id: z.uuid(), document_id: z.uuid(), document_revision_id: z.uuid(), source: sourceIdentitySchema })
export const webPreferenceSchema = z.object({ enabled: z.boolean(), web_search_version: z.number().int().positive() })
export const webCoverage: Record<string, string> = {
  complete_text: '已读取可用文本；引用存在不代表网页事实可靠。',
  partial_html_text_only: '仅读取静态正文，未读取动态内容、图片及视频。',
  partial_html_parse: '部分 HTML 无法解析，仅引用可读正文。',
  partial_text_limit: '正文达到读取上限，后续内容未读取。',
  partial_depth_limit: '页面结构过深，部分正文未读取。',
  partial_pdf: 'PDF 文本存在缺口，请核对逐页报告。',
}
