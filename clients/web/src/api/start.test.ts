import { describe, expect, it } from 'vitest'
import { knowledgeContextSchema, publicTopic, startStateSchema } from './start'

describe('开学来源与兼容边界', () => {
  it('默认公开主题移除换行，并按 UTF-8 边界限制外发长度', () => {
    expect(publicTopic('  Go\n并发\r练习  ')).toBe('Go 并发 练习')
    expect(new TextEncoder().encode(publicTopic('😀'.repeat(100))).length).toBe(300)
    expect(publicTopic('语'.repeat(101))).toHaveLength(100)
  })
  it('开学必须明确新建现场和模型授权，不能把准备结果当成已发布课堂', () => {
    expect(
      startStateSchema.safeParse({ request: { new_session: false, model_consent: true } }).success,
    ).toBe(false)
    const state = startStateSchema.parse({ request: { new_session: true, model_consent: true } })
    expect(state.result).toBeUndefined()
    expect(knowledgeContextSchema.safeParse({ mastery: 'retained' }).success).toBe(false)
  })
})
