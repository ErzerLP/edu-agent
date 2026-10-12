import { afterAll, describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
afterAll(() => vi.unstubAllGlobals())
import {
  studioRevisionSchema,
  studioGuideSchema,
  studioBriefSchema,
  noteSourceItem,
  studioRequest,
  type NoteDraft,
} from './studio'
import { DraftStore } from '@/lib/drafts'

const id = '00000000-0000-4000-8000-000000000001'
describe('Studio saved revision and tab drafts', () => {
  it('指南与简报使用不同结构，任何论断都必须带引用', () => {
    const claim = { text: '来自所选资料的论断', citations: ['1'] }
    const guide = {
      key_points: [claim],
      terms: [claim],
      learning_order: [claim],
      misconceptions: [claim],
    }
    const brief = { overview: [claim], differences: [claim], gaps: [claim] }
    expect(studioGuideSchema.parse(guide).terms[0].citations).toEqual(['1'])
    expect(studioBriefSchema.parse(brief).gaps[0].text).toBe(claim.text)
    expect(() => studioGuideSchema.parse(brief)).toThrow()
    expect(() =>
      studioBriefSchema.parse({ ...brief, overview: [{ text: '未核验', citations: [] }] }),
    ).toThrow()
  })
  it('拒绝缺版本的正文和未交付产物类型', () => {
    const raw = {
      artifact_id: id,
      learning_space_id: id,
      goal_id: null,
      kind: 'note',
      version: 1,
      privacy_generation: 1,
      actor_device_id: id,
      created_at: '2026-10-12T00:00:00Z',
      body: { schema: 1, title: '标题', markdown: '正文', citations: [] },
    }
    expect(studioRevisionSchema.parse(raw).body.markdown).toBe('正文')
    expect(() => studioRevisionSchema.parse({ ...raw, version: 0 })).toThrow()
    expect(() => studioRevisionSchema.parse({ ...raw, kind: 'guide' })).toThrow()
  })
  it('保存请求冻结基础版本，操作待核对时不随本地输入漂移', () => {
    const drafts = new DraftStore()
    const draft: NoteDraft = { title: '草稿', markdown: '本地内容', baseVersion: 4, goalId: null }
    const request = studioRequest(drafts, 'one', draft, 2)
    expect(request.expected_version).toBe(4)
    expect(request.privacy_generation).toBe(2)
    expect(studioRequest(drafts, 'one', draft, 2).operation_id).toBe(request.operation_id)
    expect(studioRequest(drafts, 'one', { ...draft, markdown: '另一版' }, 2).operation_id).not.toBe(
      request.operation_id,
    )
    expect(draft.markdown).toBe('本地内容')
  })
  it('导入条目只取服务端快照和回执，不取尚未保存的草稿', () => {
    const item = noteSourceItem({
      title: '笔记',
      derived_from: { artifact_id: id, version: 3 },
      sha256: 'a'.repeat(64),
      document: { path: 'note-v3.md', markdown: '已保存版本', source_receipt: 'signed-v3' },
    })
    expect(item.markdown).toBe('已保存版本')
    expect(item.sourceReceipt).toBe('signed-v3')
    expect(item.coverage).toContain('第 3 版')
  })
})
