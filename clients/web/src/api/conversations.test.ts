import { afterAll, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { checkConversation, conversationSchema, conversationDraftKey, turnsSchema } from './conversations'
afterAll(() => vi.unstubAllGlobals())

const id = '11111111-1111-4111-8111-111111111111'
const c = conversationSchema.parse({ id, space_id: id, privacy_generation: 1, version: 1, saved: true, title: '历史', provider: 'openai_compatible', endpoint: 'https://old.example/v1', updated_at: '2026-09-18T00:00:00Z', goal_version: 1, writable: true, storage_state: 'saved', destination: 'new', destination_provider: 'openai_compatible', destination_endpoint: 'https://new.example/v1', confirmation_required: true })
it('恢复校验原会话、学习区与隐私代次，不能重标记', () => {
  expect(checkConversation(c, id, 1, id)).toBe(c)
  expect(() => checkConversation(c, id, 2)).toThrow()
  expect(() => checkConversation(c, crypto.randomUUID(), 1)).toThrow()
  expect(() => checkConversation(c, id, 1, crypto.randomUUID())).toThrow()
})
it('分页消息保留来源工具组；未来保存状态明确拒绝', () => {
  const page = turnsSchema.parse({ conversation: c, items: [{ run_id: id, ordinal: 1, status: 'waiting_input', body_available: true, output: '', messages: [{ role: 'user', content: '问题' }, { role: 'assistant', tool_calls: [{ id: 'source', type: 'function', function: { name: 'read_goal', arguments: '{}' } }] }, { role: 'tool', tool_call_id: 'source', content: '有来源的正文' }] }] })
  expect(page.items[0].messages[2].tool_call_id).toBe('source')
  expect(conversationSchema.safeParse({ ...c, storage_state: 'future' }).success).toBe(false)
})
it('同一身份的侧栏和独立页按会话共享草稿，其他身份不能复用', () => {
  expect(conversationDraftKey(['server', 'device', 1], id)).toBe(conversationDraftKey(['server', 'device', 1], id))
  expect(conversationDraftKey(['server', 'device', 2], id)).not.toBe(conversationDraftKey(['server', 'device', 1], id))
})
