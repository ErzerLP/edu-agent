import { afterAll, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { answerable, contentSchema } from './teaching'
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
