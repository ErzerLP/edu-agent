import { describe, expect, it } from 'vitest'
import { selectedSources, citationParts, sourceSelectionSchema } from './source-selection'

const a = {
  collection_id: '10000000-0000-4000-8000-000000000001',
  revision_id: '10000000-0000-4000-8000-000000000002',
  document_id: '10000000-0000-4000-8000-000000000003',
  document_revision_id: '10000000-0000-4000-8000-000000000004',
}
const b = { ...a, collection_id: '20000000-0000-4000-8000-000000000001' }
describe('source selection', () => {
  it('keeps loading distinct from none and never selects later imports', () => {
    expect(selectedSources(undefined, [a, b])).toBeUndefined()
    expect(selectedSources([], [a, b])).toEqual({ mode: 'none', items: [] })
    expect(selectedSources([a], [a, b])).toEqual({ mode: 'items', items: [a] })
    expect(selectedSources([a], [b])).toEqual({ mode: 'none', items: [] })
  })
  it('rejects ambiguous or incomplete public selection payloads', () => {
    for (const value of [
      null,
      {},
      { mode: 'items', items: [] },
      { mode: 'none', items: [a] },
      { mode: 'items', items: [{ document_id: a.document_id }] },
    ])
      expect(sourceSelectionSchema.safeParse(value).success).toBe(false)
  })
  it('only links numbers verified by the server', () => {
    expect(citationParts('原文 [1]，模型猜测 [8]。', ['1'])).toEqual([
      { text: '原文 ' },
      { text: '[1]', id: '1' },
      { text: '，模型猜测 ' },
      { text: '[8]' },
      { text: '。' },
    ])
  })
})
