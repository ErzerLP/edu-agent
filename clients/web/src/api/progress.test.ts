import { afterAll, expect, it, vi } from 'vitest'
vi.hoisted(() => { vi.stubGlobal('window', { location: { origin: 'http://localhost' } }) })
afterAll(() => vi.unstubAllGlobals())
import { progressQuery, progressSearch, reviewSchema, reviewsPage } from './progress'

it('深链接仅保留已知范围和过滤，私人正文不进入查询', () => {
  const search = progressSearch.parse({ space: '11111111-1111-4111-8111-111111111111', status: 'paused', answer: '私人答案', cursor: '旧范围游标' })
  expect(search).not.toHaveProperty('answer')
  expect(search).not.toHaveProperty('cursor')
  expect(progressQuery(search, 'current', 10)).toEqual({ global: false, goal_id: undefined, status: 'paused', cursor: 'current', limit: 10 })
  expect(() => progressSearch.parse({ space: 'invalid' })).toThrow()
})

it('复习保留稳定任务与原会话，不按 node 合并不同目标', () => {
  const id = '11111111-1111-4111-8111-111111111111'
  const second = '22222222-2222-4222-8222-222222222222'
  const source = { task_id: id, learning_space_id: id, goal_id: id, goal_revision_id: id, evidence_id: id, attempt_id: id, node_revision_id: id, session_id: id, carrier_session_id: second, due_at: '2026-09-01T00:00:00Z' }
  expect(reviewSchema.parse(source).session_id).toBe(id)
  const value = { metadata: { generation: id, as_of_event_seq: 10, rebuilding: false, degraded: false, incomplete: false, reason_codes: [] }, updated_at: source.due_at, due_before: source.due_at, total: 2, items: [source, { ...source, task_id: second, goal_id: second }] }
  expect(reviewsPage.parse(value).items).toHaveLength(2)
  expect(reviewsPage.safeParse({ error: 'projection_unavailable' }).success).toBe(false)
})
