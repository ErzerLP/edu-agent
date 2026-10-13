import { expect, it } from 'vitest'
import {
  sourceItemSchema,
  sourceMetadataSchema,
  sourceDescription,
  sourceTitle,
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
