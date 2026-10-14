import { expect, it } from 'vitest'
import {
  sourceItemSchema,
  sourceMetadataSchema,
  sourceDescription,
  sourceTitle,
  sourceOverview,
  sourceDetailSchema,
} from './notebook-sources'

it('旧来源的未知出处保持未知，不从路径推断 PDF 或网页', () => {
  const item = sourceItemSchema.parse({
    collection_id: '00000000-0000-4000-8000-000000000001',
    collection_name: '集合',
    revision_id: '00000000-0000-4000-8000-000000000002',
    document_id: '00000000-0000-4000-8000-000000000003',
    document_revision_id: '00000000-0000-4000-8000-000000000004',
    path: 'web.pdf.md',
    metadata: null,
  })
  expect(sourceDescription(item.metadata)).toContain('未知')
  expect(
    sourceDescription({
      kind: 'pdf',
      parser: 'pdf-v1',
      coverage: 'partial_pdf',
      fingerprint: 'a',
      status: 'partial',
    }),
  ).toContain('部分解析')
  expect(() => sourceItemSchema.parse({ ...item, document_revision_id: undefined })).toThrow()
})

it('概要呈现网页标题和正文，不泄露生成文件名或封装元数据', () => {
  const source = sourceDetailSchema.parse({
    collection_id: '00000000-0000-4000-8000-000000000001',
    collection_name: '线性代数资料',
    revision_id: '00000000-0000-4000-8000-000000000002',
    document_id: '00000000-0000-4000-8000-000000000003',
    document_revision_id: '00000000-0000-4000-8000-000000000004',
    path: 'web-00000000-0000-4000-8000-000000000003.md',
    metadata: { kind: 'url', title: '线性无关', locator: 'https://example.org/linear', parser: 'safe-main-text-v2', coverage: 'partial_text_limit', fingerprint: 'PRIVATE_HASH', status: 'partial' },
    fragments: [{ node_id: '00000000-0000-4000-8000-000000000005', node_revision_id: '00000000-0000-4000-8000-000000000006', title: '网页原文', range: { start: 1, end: 200, start_line: 1, end_line: 7 }, text: '    {"kind":"url","title":"线性无关","fingerprint":"PRIVATE_HASH"}\n\n    线性无关表示向量间不存在非平凡的线性组合。\n    可以用方程判断。\n', truncated: false }],
  })
  expect(sourceOverview(source)).toEqual({ title: '线性无关', excerpt: '线性无关表示向量间不存在非平凡的线性组合。 可以用方程判断。' })
  expect(sourceDescription(source.metadata)).toContain('正文已截断')
  expect(sourceOverview({ ...source, metadata: null, fragments: [] })).toEqual({ title: '线性代数资料', excerpt: '没有可定位的正文片段。' })
  expect(sourceOverview({ ...source, metadata: { ...source.metadata!, kind: 'text' }, fragments: [{ ...source.fragments[0], title: '用户说明', text: '{"我写的 JSON":"保留"}' }] })).toEqual({ title: '线性无关', excerpt: '{"我写的 JSON":"保留"}' })
})

it('网页使用真实标题，旧网页按最终 URL、原 URL 回退，其他来源保留路径', () => {
  const metadata = sourceMetadataSchema.parse({
    kind: 'url',
    title: '  线性无关 — 教材  ',
    final_url: 'https://example.org/algebra/linear?tracking=1#definition',
    locator: 'https://original.example/start',
    parser: 'safe-main-text-v2',
    coverage: 'partial_html_text_only',
    fingerprint: 'a',
    status: 'partial',
  })
  expect(metadata).toHaveProperty('title', '  线性无关 — 教材  ')
  const source = { path: 'web-generated.md', metadata }
  expect(sourceTitle(source)).toBe('线性无关 — 教材')
  expect(sourceTitle({ ...source, metadata: { ...metadata, title: '  ' } })).toBe(
    'example.org/algebra/linear',
  )
  expect(
    sourceTitle({ ...source, metadata: { ...metadata, title: undefined, final_url: undefined } }),
  ).toBe('original.example/start')
  expect(
    sourceTitle({ ...source, metadata: { ...metadata, title: undefined, final_url: 'invalid' } }),
  ).toBe('original.example/start')
  expect(
    sourceTitle({
      ...source,
      metadata: { ...metadata, title: undefined, final_url: '', locator: '' },
    }),
  ).toBe('web-generated.md')
  expect(sourceTitle({ path: 'text.md', metadata: { ...metadata, kind: 'text' } })).toBe('text.md')
  expect(sourceTitle({ path: 'legacy.md', metadata: null })).toBe('legacy.md')
})
