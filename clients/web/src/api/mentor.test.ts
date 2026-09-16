import { afterAll, describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { eventAction, mentorSnapshotSchema } from './mentor'
afterAll(() => vi.unstubAllGlobals())

describe('导师流恢复', () => {
  const id = '11111111-1111-4111-8111-111111111111'
  const snapshot = mentorSnapshotSchema.parse({
    run_id: id, session_id: id, goal_id: id, space_id: id, goal_version: 1, privacy_generation: 1,
    version: 10, watermark: 10, status: 'running', stage: 'answering', reason: '',
    saved: true, body_available: true, requests_left: 1, tokens_left: 5000, requests_used: 1,
    result_unknown: false, cost_unknown: true, updated_at: '2026-09-15T00:00:00Z',
    expires_at: '2026-09-22T00:00:00Z', configuration: '', output: '已完成的增量',
  })
  const event = { run_id: id, session_id: id, space_id: id, goal_id: id, privacy_generation: 1, version: 11, seq: 11, type: 'output' }
  it('连续通知读取新快照，重复和迟到通知不回退正文', () => {
    expect(eventAction(snapshot, event)).toBe('refresh')
    expect(eventAction(snapshot, { ...event, seq: 10 })).toBe('ignore')
    expect(eventAction(snapshot, { ...event, seq: 8 })).toBe('ignore')
  })
  it('乱序或缺口要求恢复，跨上下文不能进入当前面板', () => {
    expect(eventAction(snapshot, { ...event, seq: 12 })).toBe('resync')
    expect(eventAction(snapshot, { ...event, version: 9 })).toBe('resync')
    expect(() => eventAction(snapshot, { ...event, privacy_generation: 2 })).toThrow()
    expect(() => eventAction(snapshot, { ...event, goal_id: '22222222-2222-4222-8222-222222222222' })).toThrow()
  })
})
