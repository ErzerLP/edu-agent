import { afterAll, describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
afterAll(() => vi.unstubAllGlobals())
import {
  studioRevisionSchema,
  studioGuideSchema,
  studioBriefSchema,
  studioQuizSchema,
  noteSourceItem,
  studioRequest,
  studioExerciseSchema,
  checkStudioExercise,
  type NoteDraft,
} from './studio'
import { DraftStore } from '@/lib/drafts'

const id = '00000000-0000-4000-8000-000000000001'
describe('Studio saved revision and tab drafts', () => {
  it('Studio 卡片只按本区、原产物版本和真实内容映射显示，不能借用 study 身份', () => {
    const value = studioExerciseSchema.parse({
      id,
      space_id: id,
      privacy_generation: 1,
      session_id: id,
      activity_id: id,
      artifact_id: id,
      content_version: 1,
      session_version: 4,
      prompt: '解释偶数',
      allowed_help: ['hint'],
      help_version: 0,
      deferred: false,
      can_continue: false,
      studio_artifact_id: id,
      studio_version: 2,
      question: 1,
    })
    const expected = {
      id,
      space: id,
      generation: 1,
      artifact: id,
      version: 2,
      question: 1,
      session: id,
      activity: id,
      content: id,
    }
    expect(checkStudioExercise(value, expected)).toBe(value)
    for (const changed of [
      { generation: 2 },
      { space: crypto.randomUUID() },
      { artifact: crypto.randomUUID() },
      { version: 1 },
      { question: 2 },
      { session: crypto.randomUUID() },
      { activity: crypto.randomUUID() },
      { content: crypto.randomUUID() },
    ])
      expect(() => checkStudioExercise(value, { ...expected, ...changed })).toThrow()
    expect(
      studioExerciseSchema.safeParse({
        ...value,
        studio_artifact_id: undefined,
        conversation_id: id,
      }).success,
    ).toBe(false)
  })
  it('方向测验读回保留确切方向修订，旧 3c 无快照仍可读取', () => {
    const question = {
      prompt: '解释偶数',
      rubric: [{ id: 'd', criterion: '被 2 整除' }],
      allowed_help: ['hint'],
      citations: ['1'],
    }
    const frozen = {
      goal_version: 1,
      questions: [question],
      direction: { id, version: 1, goal_revision_id: id },
      assessment_policy: { configuration: 'fixture', requests: 2, tokens: 50000 },
    }
    expect(studioQuizSchema.safeParse(frozen).success).toBe(true)
    expect(studioQuizSchema.safeParse({ goal_version: 3, questions: [question] }).success).toBe(
      true,
    )
    expect(
      studioQuizSchema.safeParse({ ...frozen, direction: { ...frozen.direction, version: 0 } })
        .success,
    ).toBe(false)
  })
  it('重开测验验证题目引用，目标版本不是题目组', () => {
    const source = { collection_id: id, revision_id: id, document_id: id, document_revision_id: id }
    const hash = 'a'.repeat(64)
    const raw = {
      artifact_id: id,
      learning_space_id: id,
      goal_id: id,
      kind: 'quiz',
      version: 1,
      privacy_generation: 1,
      actor_device_id: id,
      created_at: '2026-10-12T00:00:00Z',
      body: {
        schema: 1,
        title: '测验',
        markdown: '',
        model: 'fixture',
        input_fingerprint: hash,
        coverage: 'complete',
        source_manifest: {
          schema: 1,
          space_id: id,
          generation: 1,
          sha256: hash,
          entries: [{ ...source, sha256: hash }],
        },
        citations: [
          {
            id: '1',
            source: { ...source, collection_name: '资料', path: 'even.md', metadata: null },
            fragment: {
              node_id: id,
              node_revision_id: id,
              title: '偶数',
              range: { start: 0, end: 6, start_line: 1, end_line: 1 },
              text: '偶数',
              truncated: false,
            },
            document_sha256: hash,
            slice_sha256: hash,
          },
        ],
        quiz: {
          goal_version: 2,
          questions: [
            {
              prompt: '解释偶数',
              rubric: [{ id: 'd', criterion: '能被 2 整除' }],
              allowed_help: ['hint'],
              citations: ['1'],
            },
          ],
        },
      },
    }
    expect(studioRevisionSchema.parse(raw).body.quiz?.goal_version).toBe(2)
    raw.body.quiz.questions[0].citations = ['2']
    expect(studioRevisionSchema.safeParse(raw).success).toBe(false)
  })
  it('测验保留不含 none 的真实帮助规则，拒绝空标准与重复帮助', () => {
    const question = {
      prompt: '解释偶数',
      rubric: [{ id: 'definition', criterion: '被 2 整除' }],
      allowed_help: ['hint', 'scaffold'],
      citations: ['1'],
    }
    expect(
      studioQuizSchema.parse({ goal_version: 1, questions: [question] }).questions[0].allowed_help,
    ).toEqual(['hint', 'scaffold'])
    for (const invalid of [
      { ...question, rubric: [] },
      { ...question, allowed_help: ['hint', 'hint'] },
      { ...question, citations: [] },
    ]) {
      expect(() => studioQuizSchema.parse({ goal_version: 1, questions: [invalid] })).toThrow()
    }
  })
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
