import { describe, expect, it } from 'vitest'
import { DraftStore } from './drafts'
import { composerSchema, sessionSchema } from '@/api/runtime'

describe('标签页草稿与重试契约', () => {
  it('不同标签页和完整上下文键隔离，退出清除', () => {
    const a = new DraftStore(),
      b = new DraftStore()
    a.set('origin:principal:1:space-a:goal-a', '草稿甲')
    a.set('origin:principal:1:space-b:goal-b', '草稿乙')
    expect(a.get('origin:principal:1:space-a:goal-a')).toBe('草稿甲')
    expect(b.get('origin:principal:1:space-a:goal-a')).toBeUndefined()
    a.clear()
    expect(a.dirty).toBe(false)
  })
  it('相同载荷重试复用身份，修改载荷创建新身份', () => {
    const store = new DraftStore()
    const id = store.operation('goal-a', { expected_version: 2, text: '内容' })
    expect(store.operation('goal-a', { expected_version: 2, text: '内容' })).toBe(id)
    expect(store.operation('goal-a', { expected_version: 3, text: '内容' })).not.toBe(id)
  })
  it('输入运行时拒绝空正文；会话缺代次、能力或 CSRF 时拒绝', () => {
    expect(composerSchema.safeParse({ text: '  ', details: { name: '测试' } }).success).toBe(false)
    expect(sessionSchema.safeParse({ device: { id: '假 ID' } }).success).toBe(false)
  })
})
