import { afterAll, afterEach, expect, it, vi } from 'vitest'
const { fetchMock } = vi.hoisted(() => {
  const fetchMock = vi.fn()
  vi.stubGlobal('window', { location: { origin: 'http://localhost' } })
  vi.stubGlobal('fetch', fetchMock)
  return { fetchMock }
})
import { stopMentor, type MentorSnapshot } from './mentor'
import { sessionSchema } from './runtime'
import { DraftStore } from '../lib/drafts'

afterEach(() => vi.clearAllMocks())
afterAll(() => vi.unstubAllGlobals())
const id = '11111111-1111-4111-8111-111111111111'
const session = sessionSchema.parse({
  server_id: 'http://localhost', device: { id, display_name: 'test', scopes: ['learning:read', 'learning:write'] },
  generation: 1, csrf_token: 'x'.repeat(43), expires_at: '2026-12-01T00:00:00Z',
  capabilities: { spaces: true, goals: true, save_goal: true, start_learning: true, references: true, runs: true },
})
const snapshot: MentorSnapshot = {
  run_id: id, session_id: id, goal_id: id, space_id: id, goal_version: 1, privacy_generation: 1,
  version: 10, watermark: 10, status: 'running', stage: 'answering', reason: '',
  saved: true, body_available: true, requests_used: 1,
  result_unknown: false, cost_unknown: true, updated_at: '2026-09-15T00:00:00Z',
  expires_at: '2026-12-22T00:00:00Z', configuration: '', output: '已完成的增量',
}

function server(post: (body: Record<string, unknown>, attempt: number) => Response | Promise<Response>, status: MentorSnapshot['status'] = 'running') {
  const bodies: Record<string, unknown>[] = []
  let reads = 0
  fetchMock.mockImplementation(async (request: Request) => {
    const path = new URL(request.url).pathname
    if (path === '/v1/web/session') return Response.json(session)
    if (path === `/v1/learning/runs/${id}` && request.method === 'GET') {
      reads++
      return Response.json({ ...snapshot, version: 10 + reads, status })
    }
    if (path === `/v1/learning/runs/${id}/commands` && request.method === 'POST') {
      expect(request.headers.get('X-Learning-Space-ID')).toBe(id)
      const body = await request.json()
      bodies.push(body)
      return post(body, bodies.length)
    }
    throw new Error(`unexpected request: ${request.method} ${path}`)
  })
  const drafts = new DraftStore()
  const stop = () => stopMentor(session, id, id, body => drafts.operation('stop', body))
  return { stop, bodies, reads: () => reads }
}
const accepted = (body: Record<string, unknown>) => Response.json({ operation_id: body.operation_id, run_id: id, session_id: id, version: 20 }, { status: 202 })
const conflict = () => Response.json({ error: { code: 'version_conflict' } }, { status: 409 })

it('one stop intent, including overlapping handlers, sends one POST with the latest version', async () => {
  const api = server(accepted)
  await Promise.all([api.stop(), api.stop()])
  expect(api.bodies).toHaveLength(1)
  expect(api.bodies[0]).toMatchObject({ expected_version: 11, kind: 'stop' })
})

it('a definitive version conflict refreshes and retries once with a fresh operation payload', async () => {
  const api = server((body, attempt) => attempt === 1 ? conflict() : accepted(body))
  await api.stop()
  expect(api.bodies.map(body => body.expected_version)).toEqual([11, 12])
  expect(api.bodies[1].operation_id).not.toBe(api.bodies[0].operation_id)
  expect(api.reads()).toBe(2)
})

it('a second version conflict surfaces without a third attempt', async () => {
  const api = server(conflict)
  await expect(api.stop()).rejects.toMatchObject({ status: 409, code: 'version_conflict' })
  expect(api.bodies).toHaveLength(2)
})

it.each(['cancelling', 'cancelled', 'succeeded', 'partial', 'failed'] as const)('stopping %s is already satisfied and sends no command', async status => {
  const api = server(() => { throw new Error('must not POST') }, status)
  await api.stop()
  expect(api.reads()).toBe(1)
  expect(api.bodies).toHaveLength(0)
})

it.each([
  ['operation conflict', () => Response.json({ error: { code: 'operation_conflict' } }, { status: 409 })],
  ['lost response', () => { throw new TypeError('network disconnected') }],
] as const)('%s never automatically retries or creates a fresh operation', async (_name, post) => {
  const api = server(post)
  await expect(api.stop()).rejects.toThrow()
  expect(api.bodies).toHaveLength(1)
  expect(api.reads()).toBe(1)
})
