import { describe, expect, it } from 'vitest'
import { bytesToBase64, editImport, emptyImport, fetchedItem, importDocuments, pastedItem, reviewedImport, scanFiles, textDocument } from './reference-import'
import type { ImportPreview, ImportRequest } from '@/api/references'

type Review = NonNullable<ImportPreview['identity_review']>
const review = (operation: string): Review => ({ identity_review_basis_hash: 'basis', identity_review_operation_id: operation, identity_review_receipt: `receipt-${operation}`, document_reviews: [], node_reviews: [] })
const request: ImportRequest = { operation_id: 'initial', expected_parent_revision_id: 'parent', source: 'test', documents: [{ path: '教材.pdf.md', markdown: '' }] }
const documentReview = { path: '教材.pdf.md', locator: 'document-locator', reason_code: 'ambiguous', candidates: [] }
const nodeReview = { ...documentReview, locator: 'node-locator', preorder: 5 }

describe('分阶段身份审阅请求', () => {
  it('文档决定 → 章节决定 → 再预览携带完整决定和最新审阅回执', () => {
    const document = reviewedImport(request, { ...review('document-review'), document_reviews: [documentReview] }, { 'document-locator': 'original-document' })
    const node = reviewedImport(document, { ...review('node-review'), node_reviews: [nodeReview] }, { 'node-locator': 'new' })
    const again = reviewedImport(node, review('ready-preview'), {})
    expect(again.document_resolutions).toEqual([{ locator: 'document-locator', action: 'preserve', document_id: 'original-document', reason: '用户在浏览器逐项审阅身份' }])
    expect(again.node_resolutions).toEqual([{ locator: 'node-locator', action: 'new', source_node_revision_ids: [], reason: '用户在浏览器审阅章节承接关系' }])
    expect(again).toMatchObject({ documents: request.documents, expected_parent_revision_id: 'parent', identity_review_operation_id: 'ready-preview', identity_review_receipt: 'receipt-ready-preview' })
    expect(new Set([request.operation_id, document.operation_id, node.operation_id, again.operation_id]).size).toBe(4)
    expect(again).not.toHaveProperty('document_reviews')
    expect(again).not.toHaveProperty('node_reviews')
    expect(request).not.toHaveProperty('document_resolutions')
  })

  it('同 locator 的新决定覆盖旧决定，同时保留其他文档与章节决定', () => {
    const original = { ...request,
      document_resolutions: [{ locator: 'document-locator', action: 'preserve' as const, document_id: 'old-document', reason: 'old' }, { locator: 'other-document', action: 'preserve' as const, document_id: 'other', reason: 'keep' }],
      node_resolutions: [{ locator: 'node-locator', action: 'new' as const, source_node_revision_ids: [], reason: 'old' }, { locator: 'other-node', action: 'new' as const, source_node_revision_ids: [], reason: 'keep' }],
    }
    const next = reviewedImport(original, { ...review('review'), document_reviews: [documentReview], node_reviews: [nodeReview] }, { 'document-locator': 'new-document', 'node-locator': 'old-node-revision' })
    expect(next.document_resolutions).toHaveLength(2)
    expect(next.document_resolutions).toEqual(expect.arrayContaining([{ locator: 'document-locator', action: 'preserve', document_id: 'new-document', reason: '用户在浏览器逐项审阅身份' }, original.document_resolutions[1]]))
    expect(next.node_resolutions).toHaveLength(2)
    expect(next.node_resolutions).toEqual(expect.arrayContaining([{ locator: 'node-locator', action: 'rewrite', source_node_revision_ids: ['old-node-revision'], reason: '用户在浏览器审阅章节承接关系' }, original.node_resolutions[1]]))
    expect(original.document_resolutions[0].document_id).toBe('old-document')
    const edited = editImport({ ...emptyImport(), request: next }, { paste: '变更原文' })
    expect(edited.request).toBeUndefined()
  })
})

describe('浏览器真实来源与原操作草稿', () => {
  it('搜索摘要和禁止保存的正文均不能进入正式导入，只有实际正文与回执可预览', () => {
    const source = { locator: 'https://example.org/', final_url: 'https://example.org/', title: 'Public', kind: 'text/html', status: 'candidate', failure: '', fingerprint: '', parser: '', coverage: '', storage_allowed: false, text: 'SUMMARY' }
    expect(() => importDocuments([fetchedItem(source)])).toThrow()
    expect(fetchedItem(source).markdown).toBe('')
    expect(() => fetchedItem({ ...source, status: 'parsed', storage_allowed: true })).toThrow('回执')
    const item = fetchedItem({ ...source, status: 'parsed', storage_allowed: true, text: 'BODY', import_markdown: '# 原文\nBODY', source_receipt: 'signed' })
    expect(importDocuments([item])[0]).toMatchObject({ markdown: '# 原文\nBODY', source_receipt: 'signed' })
    expect(importDocuments([item])[0].markdown).not.toContain('SUMMARY')
  })
  it('网址导入逐字传递服务端正文和绑定回执，不重排成客户端前缀', () => {
    const item = { ...pastedItem('网页', 'placeholder'), markdown: '# 网页原文\n\n    服务器正文\n', sourceReceipt: 'signed-receipt' }
    expect(importDocuments([item])[0]).toEqual({ path: '网页.md', markdown: '# 网页原文\n\n    服务器正文\n', source_receipt: 'signed-receipt' })
  })
  it('PDF 上传真实字节，部分成功必须单独确认，提交时不信任预览文本', async () => {
    const raw = new Uint8Array([37, 80, 68, 70, 0, 255])
    const items = await scanFiles([new File([raw], '资料.pdf')], async data => {
      expect(data).toBe(bytesToBase64(raw))
      return { text: '真实文本', pdf: { parser: 'fixture', fingerprint: 'a'.repeat(64), page_count: 2, coverage: 'partial_pdf', pages: [{ number: 1, status: 'text', gaps: [], text: '真实文本', fingerprint: '', start: 0, end: 12 }, { number: 2, status: 'no_text', gaps: ['no_text_layer_or_blank'], text: '', fingerprint: '', start: 0, end: 0 }] } }
    })
    expect(items[0].selected).toBe(false)
    items[0].selected = true
    expect(() => importDocuments(items)).toThrow('明确确认')
    items[0].pdf!.acceptPartial = true
    items[0].markdown = '伪造的第九页'
    expect(importDocuments(items)[0]).toEqual({ path: '资料.pdf.md', markdown: '', pdf: { data: bytesToBase64(raw), accept_partial: true } })
  })
  it('逐项报告编码、不支持和成功，只提交明确选择的实际内容', async () => {
    const items = await scanFiles([new File(['# 来源\n正文'], 'a.md'), new File(['纯文本'], 'a.txt'), new File([new Uint8Array([0xff, 0xfe])], 'bad.txt'), new File(['不得上传'], 'private.pdf')])
    expect(items.map(i => i.status)).toEqual(['ready', 'ready', 'error', 'unsupported'])
    items[1].selected = false
    expect(importDocuments(items)).toEqual([{ path: 'a.md', markdown: '# 来源\n正文' }])
    expect(items[3].markdown).toBe('')
  })
  it('同名不静默覆盖，纯文本保留代码围栏', async () => {
    const items = await scanFiles([new File(['第一来源'], 'README.md'), new File(['第二来源'], 'README.md')])
    expect(() => importDocuments(items)).toThrow('路径重复')
    expect(textDocument('文字', '```\n原文')).toContain('````text\n```\n原文\n````')
    expect(pastedItem('原文', '内容').markdown).toContain('内容')
  })
  it('未知提交禁止编辑，结果核对后修改使批准失效', () => {
    const draft = { ...emptyImport(), unknown: true }
    expect(() => editImport(draft, { paste: '新内容' })).toThrow('原操作')
    const edited = editImport({ ...draft, unknown: false }, { paste: '新内容' })
    expect(edited.preview).toBeUndefined(); expect(edited.request).toBeUndefined(); expect(edited.paste).toBe('新内容')
  })
})
