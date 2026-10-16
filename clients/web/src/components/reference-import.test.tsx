import { expect, it, vi } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { ReferenceImport } from './reference-import'
import { importPreviewSchema } from '@/api/references'
import { emptyImport, type ImportDraft } from '@/lib/reference-import'

const state = vi.hoisted(() => {
  vi.stubGlobal('window', { location: { origin: 'http://localhost' } })
  return { draft: undefined as ImportDraft | undefined }
})
vi.mock('@/lib/session', () => ({ useIdentity: () => ({ session: {}, prefix: ['review-test'], drafts: { get: () => state.draft } }) }))

it('章节候选显示具体页标题与前后正文，帮助判断是否承接旧章节', () => {
  const id = '00000000-0000-4000-8000-000000000001'
  state.draft = { ...emptyImport(), preview: importPreviewSchema.parse({
    status: 'review', summary: { operation_id: id, space_id: id, collection_id: id, actor_device_id: id, document_ids: [], added: 0, updated: 0, unchanged: 0 },
    diff: [], affected_evidence: 0, impact_known: false,
    identity_review: { identity_review_basis_hash: 'basis', identity_review_operation_id: id, identity_review_receipt: 'receipt', document_reviews: [], node_reviews: [{
      path: '教材.pdf.md', locator: 'last-page', preorder: 5, reason_code: 'node_match_ambiguous',
      candidates: [{ stable_id: id, revision_id: id, reason_code: 'node_similarity', evidence: { title: '第 5 页', excerpt: '原书30页：移码通过偏置值表示阶码。', incoming_title: '第 6 页', incoming_excerpt: '原书31页：校验码用于检测数据错误。' } }],
    }] },
  }) }
  const html = renderToStaticMarkup(<ReferenceImport spaceId={id} collection={{ id, name: '教材', source: '', shared: false, head_revision_id: id, version: 1 }} target="notebook" onImported={() => {}} />)
  expect(html).toContain('本次章节：第 6 页')
  expect(html).toContain('原书31页：校验码用于检测数据错误。')
  expect(html).toContain('旧章节：第 5 页')
  expect(html).toContain('原书30页：移码通过偏置值表示阶码。')
  expect(html).toContain('更新原章节：第 5 页')
})
