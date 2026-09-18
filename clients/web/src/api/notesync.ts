import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'
import { importPreviewSchema } from './import-jobs'

export const notesyncSpace = '00000000-0000-4000-8000-000000000001'
export const notesyncCollection = '00000000-0000-4000-8000-000000000002'
const count = z.number().int().nonnegative()
const hash = z.string().regex(/^[a-f0-9]{64}$/)
export const syncStatusNames = { open: '待审阅', resolved: '已解决', closed: '已关闭' }
export const syncCategories = {
  in_sync: '内容一致', remote_unchanged: '远端未变化', local_changed: '本地已变化',
  remote_changed: '远端已变化', both_changed: '两端均有变化', remote_missing: '远端缺失',
  remote_moved: '远端身份移动', unbased_remote: '没有发布基线', path_occupied: '路径被其他身份占用',
  invalid_remote_markdown: '远端正文无效',
}
export const syncStatusSchema = z.object({
  configured: z.boolean(), compatible: z.boolean(), reason: z.string(), version: z.string().optional(),
  vault: z.string().optional(), path_prefix: z.string().optional(), external_cleanup_required: z.boolean(),
  learning_space_id: z.uuid().optional(), collection_id: z.uuid().optional(),
  configuration_source: z.enum(['environment', 'admin_settings']).optional(),
}).refine(value => !value.configured || value.learning_space_id === notesyncSpace && value.collection_id === notesyncCollection, '活动同步缺少明确的默认映射')
const snapshot = z.object({
  missing: z.boolean(), knowledge_revision_id: z.uuid().optional(), knowledge_revision_no: count.optional(),
  document_revision_id: z.uuid().optional(), source_revision_id: z.uuid().optional(), path: z.string().optional(),
  sha256: hash.optional(), remote_version: count, remote_last_time: count,
})
const diff = z.object({ base_to_local: z.string(), base_to_remote: z.string(), local_truncated: z.boolean(), remote_truncated: z.boolean() })
const category = z.enum(Object.keys(syncCategories) as [keyof typeof syncCategories, ...(keyof typeof syncCategories)[]])
export const syncReviewSummary = z.object({
  review_id: z.uuid(), category, reason_code: z.string(), status: z.enum(['open', 'resolved', 'closed']),
  basis_hash: hash, generation: count, head_revision_id: z.union([z.uuid(), z.literal('')]), head_revision_no: count,
  document_id: z.uuid().optional(), remote_document_id: z.uuid().optional(),
  canonical_path: z.string(), remote_vault: z.string(), remote_path: z.string(),
  base: snapshot, local: snapshot, remote: snapshot,
  resolution_kind: z.enum(['accept_remote', 'keep_canonical', 'merged', 'superseded', 'privacy_redaction']).optional(),
  resolution_operation_id: z.uuid().optional(), resolved_by_device_id: z.uuid().optional(),
  resolved_knowledge_revision_id: z.uuid().optional(), created_at: z.string(), updated_at: z.string(),
})
export const syncReviewSchema = syncReviewSummary.extend({
  base: snapshot.extend({ markdown: z.string() }), local: snapshot.extend({ markdown: z.string() }),
  remote: snapshot.extend({ markdown: z.string() }), diff,
})
export const syncPageSchema = z.object({ items: z.array(syncReviewSummary).max(25), next_cursor: z.string().optional() })
export const syncPreviewSchema = z.object({
  items: z.array(z.object({ category, reason_code: z.string(), review_id: z.uuid().optional(), basis_hash: hash,
    document_id: z.uuid().optional(), remote_path: z.string(), base: snapshot, local: snapshot, remote: snapshot, diff })).max(25),
  page: count.min(1), page_size: count.min(1).max(25), next_page: count.min(1).optional(), total_rows: count,
})
export const syncResultSchema = z.object({
  review_id: z.uuid(), resolution_kind: z.enum(['accept_remote', 'keep_canonical', 'merged']),
  knowledge_revision_id: z.uuid().optional(), document_id: z.uuid().optional(), document_revision_id: z.uuid().optional(),
  unchanged: z.boolean(), replayed: z.boolean().optional(),
})
export type SyncReview = z.infer<typeof syncReviewSchema>
export type SyncPreview = z.infer<typeof syncPreviewSchema>
export type SyncResult = z.infer<typeof syncResultSchema>
export type SyncResolution = components['schemas']['NotesyncResolutionRequest']

type SyncResponse<T> = { responses: { 200: { content: { 'application/json': T } } } }
type SyncHeaders = { 'X-Learning-Space-ID': string; 'X-Knowledge-Collection-ID': string }
export interface NotesyncPaths {
  '/v1/knowledge/notesync/operations/{operationID}': {
    get: { parameters: { path: { operationID: string }; header: SyncHeaders } } & SyncResponse<SyncResult>
  }
  '/v1/knowledge/notesync/reviews/{reviewID}/resolution-previews': {
    post: { parameters: { path: { reviewID: string }; header: SyncHeaders }; requestBody: { content: { 'application/json': SyncResolution } } } & SyncResponse<z.infer<typeof importPreviewSchema>>
  }
}

export function notesyncAPI(session: Session, space: string, collection: string, signal?: AbortSignal) {
  const client = () => learningClient(session, space)
  const header = { 'X-Learning-Space-ID': space, 'X-Knowledge-Collection-ID': collection }
  return {
    status: () => unwrap(client().GET('/v1/knowledge/notesync/status', { params: { header }, signal }), syncStatusSchema),
    reviews: (status = 'all' as 'all' | 'open' | 'resolved' | 'closed', cursor?: string) => unwrap(client().GET('/v1/knowledge/notesync/reviews', { params: { header, query: { status, cursor, limit: 25 } }, signal }), syncPageSchema),
    review: (reviewID: string) => unwrap(client().GET('/v1/knowledge/notesync/reviews/{reviewID}', { params: { header, path: { reviewID } }, signal }), syncReviewSchema),
    preview: (path = '', page = 1) => unwrap(client().POST('/v1/knowledge/notesync/previews', { params: { header }, body: { path, page }, signal }), syncPreviewSchema),
    plan: (reviewID: string, body: SyncResolution) => unwrap(client().POST('/v1/knowledge/notesync/reviews/{reviewID}/resolution-previews', { params: { header, path: { reviewID } }, body, signal }), importPreviewSchema),
    resolve: (reviewID: string, body: SyncResolution) => unwrap(client().POST('/v1/knowledge/notesync/reviews/{reviewID}/resolutions', { params: { header, path: { reviewID } }, body, signal }), syncResultSchema),
    operation: (operationID: string) => unwrap(client().GET('/v1/knowledge/notesync/operations/{operationID}', { params: { header, path: { operationID } }, signal }), syncResultSchema),
  }
}

export function syncAccessLost(error: unknown) {
  return error instanceof ApiError && ([401, 403, 404].includes(error.status) || ['content_redacted', 'privacy_clear_in_progress'].includes(error.code))
}
export function syncError(error: unknown): string {
  if (!(error instanceof ApiError)) return '连接中断，结果尚未核对。不会自动重放解决动作。'
  const messages: Record<string, string> = {
    stale_notesync_review: '审阅已过期：本地或远端版本改变。旧差异保留供对照，请重新预览。',
    revision_conflict: '本地版本已变化，请重新预览并核对实际影响。',
    stale_identity_review: '身份审阅已过期，请重新预览。',
    identity_review_required: '需要原知识身份审阅，请先预览实际影响并逐项决定。',
    notesync_unavailable: '远端不可用或版本/能力不兼容；这不是空列表。预览可能已保存部分审阅，可刷新列表核对。',
    notesync_not_configured: 'NoteSync 尚未配置或未启用。请联系操作者使用本机管理入口。',
    content_redacted: '内容已清除，旧正文和差异不再提供。',
    privacy_clear_in_progress: '正在执行隐私清除，已停止显示旧正文。',
    learning_space_archived: '学习区已归档，不能预览或解决；归档没有删除远端笔记。',
    learning_space_module_unavailable: '当前区没有 NoteSync 映射，不能继承默认区同步权限。',
    idempotency_conflict: '原操作内容不一致，未执行新解决；请核对原操作。',
    invalid_response: '响应格式不受支持，不能据此认定成功。',
  }
  if (messages[error.code]) return messages[error.code]
  if (error.status === 404) return '映射、引用或审阅不可用。原操作尚无收据也不能视为失败或自动重放。'
  if (error.status === 403) return '当前学习身份没有此权限，需要已有 import 或 references 授权。'
  if (error.status === 401) return '学习身份已失效，请重新配对。'
  return '请求未完成，请核对原审阅及操作。'
}
