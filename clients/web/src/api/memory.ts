import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'

const version = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
const date = z.iso.datetime({ offset: true })
const hash = z.string().regex(/^[0-9a-f]{64}$/)
export const memoryScope = '全局长期偏好，未按学习区隔离；不授予其他学习区正文访问权。'
export const memorySearch = z.object({ candidate: z.uuid().optional(), record: z.uuid().optional() })
export const candidateNames = { pending_review: '待明确批准', admitted: '已批准', rejected: '已拒绝', expired: '已过期' }
export const recordNames = { queued: '等待交付', applied: '已保存到 Nocturne', permanently_rejected: '交付被拒绝', superseded: '已被新版替代', delete_pending: '删除待验证', deleted: '已验证删除' }
export const categoryNames: Record<string, string> = { interaction_preference: '交互偏好', time_constraint: '长期时间安排', personal_context: '长期背景', generated_summary: '生成摘要' }
export const sourceNames = { user_statement: '用户明确陈述', model_inference: '模型提议', long_term_background: '长期背景', generated_summary: '生成摘要' }
export const stepNames: Record<string, string> = { pending: '等待处理', succeeded: '已验证完成', partial: '部分完成', failed: '失败', unknown: '结果未知', not_applicable: '不适用', unsupported: '不在服务保证范围' }
const generation = z.object({ learner_generation: version, memory_generation: version })
export const candidateSchema = z.object({
  candidate: z.object({ candidate_id: z.uuid(), candidate_uri: z.string(), logical_memory_id: z.uuid().optional(),
    content_sha256: hash, source_kind: z.enum(['user_statement', 'model_inference', 'long_term_background', 'generated_summary']),
    source_reference: z.object({ event_id: z.uuid().optional(), operation_id: z.uuid().optional(), model_id: z.string().optional(), prompt_revision: z.string().optional() }),
    proposer_id: z.uuid(), reason: z.string(), category: z.string(), sensitivity: z.enum(['non_sensitive', 'sensitive']),
    stability: z.enum(['stable', 'transient']), valid_until: date, admission_policy_version: z.literal('memory-admission-v1'),
    status: z.enum(['pending_review', 'admitted', 'rejected', 'expired']), revision: version, created_at: date }),
  content_status: z.enum(['available', 'scrubbed']), proposed_content: z.string().max(32000).optional(), read_generation: generation.optional(),
}).refine(v => v.content_status !== 'available' || !!v.proposed_content, '可审阅候选缺少正文')
export type CandidateView = z.infer<typeof candidateSchema>
export const recordSchema = z.object({ logical_memory_id: z.uuid(), record_revision_id: z.uuid(), revision: version, record_generation: version,
  learner_generation: version, candidate_id: z.uuid(), content_sha256: hash,
  status: z.enum(['queued', 'applied', 'permanently_rejected', 'superseded', 'delete_pending', 'deleted']),
  delivery_id: z.uuid(), receipt_id: z.uuid(), created_at: date, applied_at: date.optional(), deleted_at: date.optional() })
const receipt = z.object({ receipt_id: z.uuid(), delivery_id: z.uuid(), version, status: z.enum(['pending', 'succeeded', 'partial', 'failed', 'unknown', 'not_applicable', 'unsupported']), reason: z.string(), verification_method: z.string(), created_at: date })
const delivery = z.object({ delivery_id: z.uuid(), kind: z.enum(['admit', 'correction', 'delete', 'erasure']), status: z.string(), public_status: z.enum(['queued', 'applied', 'rejected']), attempt_state: z.string(), valid_until: date, attempt_count: z.number().int().nonnegative(), last_category: z.string().optional() })
export const recordDetailSchema = z.object({ record: recordSchema, delivery, receipt, read_generation: generation, content_status: z.enum(['available', 'degraded', 'unavailable', 'redacted']), content: z.string().max(32000).optional() })
export type MemoryRecord = z.infer<typeof recordDetailSchema>
export const memoryOperationSchema = z.object({ candidate: candidateSchema.optional(), record: recordSchema.optional(), delivery: delivery.optional(), replayed: z.boolean() })
export type MemoryOperation = z.infer<typeof memoryOperationSchema>
export const memoryExportSchema = z.object({ items: z.array(z.object({ record: recordSchema, delivery_status: z.enum(['queued', 'applied', 'rejected']), receipt, content_status: z.enum(['available', 'degraded', 'unavailable', 'redacted']), content: z.string().optional() })), next_cursor: z.string().optional(), read_generation: generation, degraded: z.boolean(), reason_codes: z.array(z.string()) })
export const erasureSchema = z.object({ erasure_id: z.uuid(), status: z.enum(['barrier_committed', 'local_scrubbed', 'remote_draining', 'remote_purged', 'verified', 'partial', 'blocked']), summary_version: version, learner_generation: version, requested_at: date, updated_at: date,
  steps: z.array(z.object({ receipt_id: z.uuid(), store: z.string(), version, status: receipt.shape.status, stable_reason: z.string(), verification_method: z.string(), started_at: date, completed_at: date.optional() })) })
export type ErasureReceipt = z.infer<typeof erasureSchema>
export const devicesSchema = z.object({ devices: z.array(z.object({ id: z.uuid(), display_name: z.string(), scopes: z.array(z.string()).optional(), created_at: date, last_used_at: date.optional(), revoked_at: date.optional() })) })

export interface MemoryPaths {
  '/v1/privacy/operations/{operationID}': { get: { parameters: { path: { operationID: string }; query: { device_id: string } }; responses: { 200: { content: { 'application/json': ErasureReceipt } } } } }
}
export type CandidateInput = components['schemas']['MemoryCandidateRequest']
export function memoryAPI(session: Session, signal?: AbortSignal) {
  const client = () => learningClient(session)
  return {
    candidates: (cursor?: string) => unwrap(client().GET('/v1/memory/candidates', { params: { query: { cursor, limit: 25 } }, signal }), z.object({ items: z.array(candidateSchema), next_cursor: z.string().optional(), read_generation: generation })),
    candidate: (candidateID: string) => unwrap(client().GET('/v1/memory/candidates/{candidateID}', { params: { path: { candidateID } }, signal }), candidateSchema),
    records: (cursor?: string) => unwrap(client().GET('/v1/memory/records', { params: { query: { cursor, limit: 25 } }, signal }), z.object({ items: z.array(recordSchema), next_cursor: z.string().optional(), read_generation: generation })),
    record: (memoryID: string) => unwrap(client().GET('/v1/memory/records/{memoryID}', { params: { path: { memoryID } }, signal }), recordDetailSchema),
    create: (body: CandidateInput, base?: MemoryRecord) => base
      ? unwrap(client().POST('/v1/memory/records/{memoryID}/candidates', { params: { path: { memoryID: base.record.logical_memory_id } }, body: { ...body, expected_record_revision: base.record.revision, expected_record_generation: base.record.record_generation }, signal }), memoryOperationSchema)
      : unwrap(client().POST('/v1/memory/candidates', { body, signal }), memoryOperationSchema),
    decide: (candidateID: string, body: components['schemas']['MemoryCandidateDecisionRequest']) => unwrap(client().POST('/v1/memory/candidates/{candidateID}/decisions', { params: { path: { candidateID } }, body, signal }), memoryOperationSchema),
    remove: (memoryID: string, body: components['schemas']['MemoryDeleteRequest']) => unwrap(client().DELETE('/v1/memory/records/{memoryID}', { params: { path: { memoryID } }, body, signal }), memoryOperationSchema),
    replay: (deliveryID: string, operation_id: string) => unwrap(client().POST('/v1/memory/deliveries/{deliveryID}/replays', { params: { path: { deliveryID } }, body: { operation_id, payload_schema_version: 1 }, signal }), memoryOperationSchema),
    export: (cursor?: string) => unwrap(client().GET('/v1/memory/export', { params: { query: { cursor, limit: 25 } }, signal }), memoryExportSchema),
    erasure: (erasureID: string) => unwrap(client().GET('/v1/privacy/erasures/{erasureID}', { params: { path: { erasureID } }, signal }), erasureSchema),
    operation: (operationID: string, device_id: string) => unwrap(client().GET('/v1/privacy/operations/{operationID}', { params: { path: { operationID }, query: { device_id } }, signal }), erasureSchema),
    erase: (body: components['schemas']['PrivacyErasureRequest'], grant: string) => unwrap(client().POST('/v1/privacy/erasures', { headers: { 'X-Privacy-Erasure-Grant': grant }, body, signal }), erasureSchema),
    devices: () => unwrap(client().GET('/v1/devices', { signal }), devicesSchema),
    revoke: async (deviceID: string) => {
      const result = await client().DELETE('/v1/devices/{deviceID}', { params: { path: { deviceID } }, signal })
      if (!result.response.ok) throw new ApiError(result.response.status, 'device_revoke_failed')
    },
  }
}

export function memoryAccessLost(error: unknown) {
  return error instanceof ApiError && ([401, 403, 404].includes(error.status) || ['content_redacted', 'privacy_clear_in_progress'].includes(error.code))
}
