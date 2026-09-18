import { describe, expect, it } from 'vitest'
import { emptyConcept, structureNode, structurePage } from './structure'

const id = '00000000-0000-4000-8000-000000000011'
const node = { concept_id: id, revision_id: id, semantic_key: '显式身份', name: '同名概念', goal_id: '', content: emptyConcept(), support: [], learning_state: 'unseen' }
describe('动态知识协议', () => {
  it('来源采纳和 AI 建议不能充当学习状态', () => {
    expect(structureNode.safeParse({ ...node, content: { ...node.content, source_status: 'included', suggested: true } }).success).toBe(true)
    expect(structureNode.safeParse({ ...node, learning_state: 'mastered' }).success).toBe(false)
    expect(structureNode.safeParse({ ...node, content: { ...node.content, source_status: 'mastered' } }).success).toBe(false)
  })
  it('拒绝缺失裁剪提示或版本的局部图', () => {
    expect(structurePage.safeParse({ items: [node], edges: [] }).success).toBe(false)
    expect(structurePage.safeParse({ version: 3, generation: 1, items: [node], edges: [], next_cursor: '', partial: true, notice: '只显示当前授权页' }).success).toBe(true)
  })
})
