import { afterAll, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import {
  availableHelp,
  feedbackHeading,
  checkExercise,
  exerciseSchema,
  citationExcerpt,
} from './exercises'
afterAll(() => vi.unstubAllGlobals())
it('反馈引用不显示系统节点标记，保留原文', () => {
  expect(
    citationExcerpt('<!-- edu-agent-node:v1 {"id":"private-node"} -->\n# 偶数\n\n能被 2 整除。'),
  ).toBe('# 偶数\n\n能被 2 整除。')
})
const id = '11111111-1111-4111-8111-111111111111'
const card = {
  id,
  conversation_id: id,
  run_id: id,
  space_id: id,
  privacy_generation: 1,
  session_id: id,
  activity_id: id,
  artifact_id: id,
  content_version: 1,
  session_version: 4,
  prompt: '解释偶数',
  allowed_help: ['none', 'hint', 'scaffold'],
  help_version: 1,
  help_floor: 'hint',
  deferred: false,
  can_continue: false,
}
it('已披露帮助不能降级，且不能添加题目未允许的等级', () => {
  expect(availableHelp(['none', 'hint', 'scaffold'], 'hint')).toEqual(['hint', 'scaffold'])
  expect(availableHelp(['hint', 'answer_revealed'])).toEqual(['hint', 'answer_revealed'])
})
it('待复核与通过分别呈现，不把模型通过建议当成已通过', () => {
  expect(feedbackHeading('provisional', ['pass'])).toBe('待复核')
  expect(feedbackHeading('accepted', ['pass'])).toBe('你找到了关键点')
  expect(feedbackHeading('accepted', ['partial'])).toBe('再补充一点理由')
  expect(feedbackHeading('voided', ['pass'])).toBe('本次反馈已撤回')
})
it('卡片身份、学习区和代次不匹配时拒绝显示', () => {
  const value = exerciseSchema.parse(card)
  expect(checkExercise(value, id, id, 1, id)).toBe(value)
  expect(() => checkExercise(value, id, id, 2, id)).toThrow()
  expect(() => checkExercise(value, crypto.randomUUID(), id, 1, id)).toThrow()
  expect(() => checkExercise(value, id, crypto.randomUUID(), 1, id)).toThrow()
})
