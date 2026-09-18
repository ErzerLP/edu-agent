import { afterAll, expect, it, vi } from 'vitest'
vi.hoisted(() => {
  vi.stubGlobal('window', { location: { origin: 'http://localhost' } })
})
afterAll(() => vi.unstubAllGlobals())
import { quoteEvidence, carryoverSchema } from './feedback'

it('中文和补充字符的引文位置使用 UTF-8 字节而非 JS 字符下标', async () => {
  const value = await quoteEvidence('原文🌱：可以整除。', '可以整除')
  expect(value.range).toEqual({ start: 13, end: 25 })
  expect(value.hash).toHaveLength(64)
  await expect(quoteEvidence('相同 相同', '相同')).rejects.toThrow('唯一')
  await expect(quoteEvidence('原答案', '虚构答案')).rejects.toThrow('唯一')
})

it('清除后的继承提案允许省略敏感映射，未知状态不能提供审批能力', () => {
  const id = '11111111-1111-4111-8111-111111111111'
  const value = {
    proposal_id: id,
    knowledge_proposal_id: id,
    status: 'redacted',
    redacted: true,
    policy_version: 'evidence-carryover-v1',
  }
  expect(carryoverSchema.safeParse(value).success).toBe(true)
  expect(carryoverSchema.safeParse({ ...value, status: 'auto_mastered' }).success).toBe(false)
})
