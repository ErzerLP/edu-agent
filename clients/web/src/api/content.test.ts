import { describe, it, expect } from 'vitest'
import { contentDiff, contentSelection } from './content'
import type { Content, Block } from './teaching'

describe('选区定位和版本变化', () => {
  it('中文和 emoji 使用原文字节范围及摘要', async () => {
    const block: Block = {
      block_id: '10000000-0000-4000-8000-000000000005',
      kind: 'markdown',
      text: '前面中文🙂结尾',
      fallback: '前面中文🙂结尾',
    }
    const content = {
      learning_space_id: '10000000-0000-4000-8000-000000000001',
      goal_id: '10000000-0000-4000-8000-000000000002',
      session_id: '10000000-0000-4000-8000-000000000003',
      artifact_id: '10000000-0000-4000-8000-000000000004',
      version: 7,
    } as Content
    const selection = await contentSelection(content, block, 2, 6)
    expect(selection).toMatchObject({
      start: 6,
      end: 16,
      version: 7,
      artifact_id: content.artifact_id,
      session_id: content.session_id,
    })
    const raw = await crypto.subtle.digest('SHA-256', new TextEncoder().encode('中文🙂'))
    expect(selection.sha256).toBe(
      Array.from(new Uint8Array(raw), (b) => b.toString(16).padStart(2, '0')).join(''),
    )
  })
  it('只列出真实变化，保留删除与新增块', () => {
    const first = { block_id: 'first', kind: 'markdown', text: '原文', fallback: '原文' }
    const second = { ...first, block_id: 'second', text: '补充' }
    expect(contentDiff([first], [first, second])).toEqual([
      { id: 'second', before: undefined, after: second },
    ])
    expect(contentDiff([first, second], [first])).toEqual([
      { id: 'second', before: second, after: undefined },
    ])
  })
})
