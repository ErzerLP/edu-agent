import { expect, it } from 'vitest'
import { sourceItemSchema, sourceDescription } from './notebook-sources'

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
