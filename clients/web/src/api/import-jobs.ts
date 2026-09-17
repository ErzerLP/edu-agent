import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'

const count = z.number().int().nonnegative()
const digest = z.string().regex(/^[0-9a-f]{64}$/)
const array = <T extends z.ZodType>(schema: T) =>
  z
    .array(schema)
    .nullish()
    .transform((v) => v ?? [])
export const importItemSchema = z.object({
  path: z.string(),
  sha256: digest,
  bytes: count.max(4 << 20),
})
const candidate = z.object({
  stable_id: z.uuid(),
  revision_id: z.uuid(),
  reason_code: z.string(),
  evidence: z.record(z.string(), z.unknown()).optional(),
})
const review = z.object({
  path: z.string(),
  locator: digest,
  reason_code: z.string(),
  candidates: array(candidate),
})
export const importPreviewSchema = z.object({
  status: z.enum(['ready', 'review']),
  summary: z.object({ added: count, updated: count, unchanged: count }),
  affected_evidence: count,
  impact_known: z.boolean(),
  before: array(z.object({ path: z.string(), markdown: z.string() })),
  diff: array(
    z.object({
      document_id: z.uuid(),
      before_path: z.string().optional(),
      after_path: z.string().optional(),
      kind: z.enum(['add', 'delete', 'edit', 'unchanged']),
      unified_diff: z.string().optional(),
      truncated: z.boolean(),
      added_node_ids: array(z.uuid()),
      removed_node_ids: array(z.uuid()),
      edited_node_ids: array(z.uuid()),
      title_node_ids: array(z.uuid()),
      structure_node_ids: array(z.uuid()),
    }),
  ),
  identity_review: z
    .object({
      identity_review_basis_hash: digest,
      identity_review_operation_id: z.uuid(),
      identity_review_receipt: z.string(),
      document_reviews: array(review),
      node_reviews: array(review.extend({ preorder: count })),
    })
    .optional(),
})
export const importBatchSchema = z.object({
  item: importItemSchema,
  operation_id: z.uuid(),
  request_hash: digest.optional(),
  status: z.enum([
    'pending',
    'uploaded',
    'ready',
    'review',
    'unknown',
    'completed',
    'failed',
    'missing',
    'stale',
  ]),
  error: z.string().optional(),
  preview: importPreviewSchema.optional(),
  result: z
    .object({
      revision: z.object({ revision_id: z.uuid(), manifest_hash: digest }),
      summary: z.object({ added: count, updated: count, unchanged: count }).optional(),
      unchanged: z.boolean(),
      replayed: z.boolean().optional(),
    })
    .optional(),
  planned_revision: z.uuid().optional(),
  planned_manifest: z.string().optional(),
})
export const importJobSchema = z.object({
  id: z.uuid(),
  actor_device_id: z.uuid(),
  space_id: z.uuid(),
  collection_id: z.uuid(),
  generation: count,
  version: count,
  plan_version: count,
  approved: z.boolean(),
  status: z.enum([
    'uploading',
    'ready',
    'review',
    'committing',
    'partial',
    'completed',
    'failed',
    'cancelled',
    'expired',
  ]),
  created_at: z.string(),
  expires_at: z.string(),
  batch_count: count,
  batches: array(importBatchSchema),
  cleanup_pending: z.boolean(),
})
export type ImportJob = z.infer<typeof importJobSchema>
export type ImportBatch = z.infer<typeof importBatchSchema>
export type ImportPreview = z.infer<typeof importPreviewSchema>
export type ImportItem = z.infer<typeof importItemSchema>
export type ImportCommand = components['schemas']['ImportJobCommand']
export const collectionSchema = z.object({
  id: z.uuid(),
  name: z.string(),
  source: z.string(),
  shared: z.boolean(),
})

export function importAPI(
  session: Session,
  space: string,
  collection: string,
  signal?: AbortSignal,
) {
  const client = learningClient(session, space)
  const header = { 'X-Learning-Space-ID': space, 'X-Knowledge-Collection-ID': collection }
  const check = (job: ImportJob, id?: string) => {
    if (
      job.space_id !== space ||
      job.collection_id !== collection ||
      job.actor_device_id !== session.device.id ||
      (id && (job.id !== id || job.batches.length !== job.batch_count))
    )
      throw new ApiError(502, 'invalid_response')
    return job
  }
  return {
    list: async (cursor?: string) => {
      const page = await unwrap(
        client.GET('/v1/knowledge/import-jobs', { params: { header, query: { cursor } }, signal }),
        z.object({ items: z.array(importJobSchema), next_cursor: z.string().optional() }),
      )
      page.items.forEach((job) => check(job))
      return page
    },
    get: async (id: string) =>
      check(
        await unwrap(
          client.GET('/v1/knowledge/import-jobs/{jobID}', {
            params: { header, path: { jobID: id } },
            signal,
          }),
          importJobSchema,
        ),
        id,
      ),
    batch: (id: string, batch: number) =>
      unwrap(
        client.GET('/v1/knowledge/import-jobs/{jobID}', {
          params: { header, path: { jobID: id }, query: { batch } },
          signal,
        }),
        importBatchSchema,
      ),
    command: async (body: ImportCommand) =>
      check(
        await unwrap(
          client.POST('/v1/knowledge/import-jobs', { params: { header }, body, signal }),
          importJobSchema,
        ),
        body.id,
      ),
    cancel: async (id: string) =>
      check(
        await unwrap(
          client.DELETE('/v1/knowledge/import-jobs/{jobID}', {
            params: { header, path: { jobID: id } },
            signal,
          }),
          importJobSchema,
        ),
        id,
      ),
  }
}

export const importStatus: Record<string, string> = {
  uploading: '上传中',
  pending: '未上传，需重新选择来源',
  uploaded: '已上传并解析',
  ready: '预览就绪，待确认',
  review: '需要审阅',
  committing: '已确认，等待逐批发布',
  partial: '部分已发布',
  unknown: '结果未知，先核对原操作',
  completed: '已发布',
  failed: '失败',
  missing: '暂存丢失，需核对原来源补传',
  stale: '集合基线变化，预览已失效',
  cancelled: '已取消后续批次',
  expired: '已过期',
}
export const importErrors: Record<string, string> = {
  import_job_source_changed: '本地来源缺失或摘要变化。恢复原文件后重新核对，或创建新任务重新预览。',
  import_job_staging_missing:
    '服务器暂存已丢失。重新选择原来源并核对摘要后补传，随后重新预览确认。',
  import_preview_stale: '集合基线已变化，旧批准失效。请重新预览并确认完整剩余计划。',
  import_job_quota: '任务或服务器暂存配额已满。请检查资料预算或清理已结束任务。',
  import_job_busy: '原任务仍有请求执行中。请稍后核对原任务，勿创建重复任务。',
  import_job_closed: '原任务已结束或过期，不能继续。已发布结果仍保留。',
  import_job_storage_unavailable: '暂存服务不可用；请核对原任务后重试未上传部分。',
}
