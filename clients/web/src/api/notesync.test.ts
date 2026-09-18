import { afterAll, describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { ApiError } from './client'
import { syncPreviewSchema, syncResultSchema, syncReviewSchema, syncAccessLost, syncError } from './notesync'
afterAll(() => vi.unstubAllGlobals())

describe('同步结果与隐私边界', () => {
  it('不把任意成功标志或缺正文的摘要当作正式结果和差异', () => {
    expect(syncResultSchema.safeParse({ status: 'success' }).success).toBe(false)
    expect(syncReviewSchema.safeParse({ status: 'open', diff: {} }).success).toBe(false)
    expect(syncPreviewSchema.safeParse({ items: [], page: 1, page_size: 25, total_rows: 0 }).success).toBe(true)
    expect(syncPreviewSchema.safeParse({ items: [], page: 1 }).success).toBe(false)
  })
  it('区分过期与撤权，并且只展示安全错误说明', () => {
    expect(syncAccessLost(new ApiError(409, 'stale_notesync_review'))).toBe(false)
    for (const error of [new ApiError(503, 'content_redacted'), new ApiError(503, 'privacy_clear_in_progress'), new ApiError(404, 'not_found'), new ApiError(403, 'forbidden')]) expect(syncAccessLost(error)).toBe(true)
    expect(syncError(new ApiError(503, 'notesync_unavailable'))).toContain('不是空列表')
    expect(syncError(new ApiError(500, '上游 Token 与正文'))).not.toContain('Token')
    expect(syncError(new Error('上游敏感正文'))).not.toContain('敏感正文')
  })
})
