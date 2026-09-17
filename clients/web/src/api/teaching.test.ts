import { afterAll, expect, it, vi } from 'vitest'
const { fetchMock } = vi.hoisted(() => {
  vi.stubGlobal('window', { location: { origin: 'http://localhost' } })
  const fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  return { fetchMock }
})
import { answerable, contentSchema, propose, teachingSchema } from './teaching'
import { sessionSchema } from './runtime'
afterAll(() => vi.unstubAllGlobals())
const id = '11111111-1111-4111-8111-111111111111'
const content = contentSchema.parse({
  protocol_version: 1,
  artifact_id: id,
  version: 1,
  committed_version: 1,
  learning_space_id: id,
  goal_id: id,
  goal_revision_id: id,
  session_id: id,
  activity_id: id,
  activity_revision: 1,
  privacy_generation: 1,
  actor_device_id: id,
  status: 'committed',
  created_at: '2026-09-16',
  body: {
    blocks: [{ block_id: id, kind: 'future_display', fallback: '完整语义回退' }],
    interaction: { kind: 'text' },
    references: [],
    model_id: 'fixture',
    input_fingerprint: 'a'.repeat(64),
    semantic_fingerprint: 'b'.repeat(64),
  },
})
it('草稿、失败、旧版本与未知交互均不开放作答', () => {
  expect(answerable(content)).toBe(true)
  expect(answerable({ ...content, status: 'draft' })).toBe(false)
  expect(answerable({ ...content, status: 'failed' })).toBe(false)
  expect(answerable({ ...content, committed_version: 2 })).toBe(false)
  expect(
    answerable({ ...content, body: { ...content.body, interaction: { kind: 'future_input' } } }),
  ).toBe(false)
})

it('已有现场的新知识版本优先于目标的旧范围，后续提案不回落全库', async () => {
  const current = '22222222-2222-4222-8222-222222222222'
  const identity = sessionSchema.parse({
    device: {
      id,
      display_name: '知识上下文验收',
      scopes: ['learning:read', 'learning:write', 'knowledge:read'],
    },
    generation: 1,
    expires_at: '2026-09-17T00:00:00Z',
    csrf_token: 'a'.repeat(43),
    server_id: 'http://localhost',
    capabilities: {
      spaces: true,
      goals: true,
      save_goal: true,
      start_learning: true,
      references: true,
    },
  })
  const view = teachingSchema.parse({
    session: {
      session_id: id,
      aggregate_version: 3,
      state: 'route_active',
      focus: { knowledge_revision_id: current, goal_revision_id: id },
    },
    work_item: {
      allowed_actions: ['issue_activity'],
      allowed_assessment_decisions: [],
      goal_revision: {
        goal_id: id,
        goal_revision_id: id,
        text: '学习并发',
        management: { details: { name: '当前目标', scope_snapshot_id: id }, status: 'active' },
      },
    },
  })
  const paths: string[] = []
  fetchMock.mockImplementation(async (request: Request) => {
    const path = new URL(request.url).pathname
    paths.push(path)
    if (path === '/v1/web/session') return Response.json(identity)
    if (path === '/v1/knowledge/retrievals') {
      const body = await request.json()
      expect(body.knowledge_revision_id).toBe(current)
      expect(body.scope_snapshot_id).toBeUndefined()
      return Response.json({
        knowledge_revision_id: current,
        degraded: false,
        truncated: false,
        hits: [
          {
            node_revision_id: id,
            node_id: id,
            document_revision_id: id,
            canonical_slice: '真实资料',
            slice_sha256: 'a'.repeat(64),
            section_range: { start: 0, end: 12 },
          },
        ],
      })
    }
    if (path === '/v1/tutoring/proposals') {
      expect((await request.json()).knowledge_revision_id).toBe(current)
      return Response.json({ proposal_id: id })
    }
    if (path === `/v1/tutoring/sessions/${id}`) return Response.json(view)
    throw new Error(`未授权的请求：${path}`)
  })
  expect(await propose(identity, id, view, 'activity', id)).toBe(id)
  expect(paths).not.toContain('/v1/knowledge/revisions/head')
})
