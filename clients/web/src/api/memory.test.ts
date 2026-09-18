import { afterAll, describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { ApiError, errorText, unwrap } from './client'
import { candidateSchema, erasureSchema, memoryExportSchema, memoryOperationSchema, memoryAccessLost } from './memory'
afterAll(() => vi.unstubAllGlobals())

const id = '00000000-0000-4000-8000-000000000001'
const now = '2026-09-18T00:00:00Z'
const candidate = { candidate_id: id, candidate_uri: `candidate://${id}`, content_sha256: 'a'.repeat(64), source_kind: 'model_inference', source_reference: { model_id: '夹具' }, proposer_id: id, reason: '长期交互偏好', category: 'interaction_preference', sensitivity: 'non_sensitive', stability: 'stable', valid_until: now, admission_policy_version: 'memory-admission-v1', status: 'pending_review', revision: 1, created_at: now }

describe('记忆、清除和导出的真实回执边界', () => {
  it('待审批必须有具体正文，普通成功标志不构成回执', () => {
    expect(candidateSchema.safeParse({ candidate, content_status: 'available' }).success).toBe(false)
    expect(candidateSchema.parse({ candidate, content_status: 'available', proposed_content: '偏好图示' }).candidate.status).toBe('pending_review')
    expect(memoryOperationSchema.safeParse({ success: true }).success).toBe(false)
    expect(erasureSchema.safeParse({ success: true, cache_cleared: true }).success).toBe(false)
  })
  it('保留清除失败步骤，导出剔除合同以外的凭据', () => {
    const receipt = erasureSchema.parse({ erasure_id: id, status: 'partial', summary_version: 1, learner_generation: 2, requested_at: now, updated_at: now, steps: [{ receipt_id: id, store: 'knowledge_index', version: 1, status: 'failed', stable_reason: '验证失败', verification_method: '正式服务', started_at: now }] })
    expect(receipt.steps[0].status).toBe('failed')
    const result = memoryExportSchema.parse({ items: [], read_generation: { learner_generation: 1, memory_generation: 1 }, degraded: true, reason_codes: ['nocturne_not_configured'], model_api_key: '不可导出', search_key: '不可导出' })
    expect(JSON.stringify(result)).not.toContain('不可导出')
    expect(result.degraded).toBe(true)
  })
  it('错误不回显服务端正文，只保留安全请求 ID', async () => {
    const failure = unwrap(Promise.resolve({ response: new Response(null, { status: 500 }), error: { error: { code: 'internal_error', message: '私人正文和 Key', request_id: 'req/123' } } }), erasureSchema)
    await expect(failure).rejects.toMatchObject({ requestId: 'req/123' })
    expect(errorText(new Error('私人正文和 Key'))).not.toContain('私人正文')
    expect(memoryAccessLost(new ApiError(403, 'forbidden'))).toBe(true)
    expect(memoryAccessLost(new ApiError(503, 'content_redacted'))).toBe(true)
    expect(memoryAccessLost(new ApiError(409, 'revision_conflict'))).toBe(false)
  })
})
