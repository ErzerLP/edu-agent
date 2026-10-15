import { expect, it } from 'vitest'
import { studioCoverageDescription } from './studio-coverage'
import type { StudioRevision } from '@/api/studio'

it('完整产物不显示缺口，partial 根据保存的引用解释截断而不声称引用被剔除', () => {
  const body = { coverage: 'complete', citations: [] } as unknown as StudioRevision['body']
  expect(studioCoverageDescription(body)).toBe('')
  expect(studioCoverageDescription({ ...body, coverage: 'partial' })).toContain('来源片段不足以覆盖全部内容')
  const partial = { ...body, coverage: 'partial', citations: [
    { source: { collection_id: 'c', document_id: 'd', metadata: null }, fragment: { truncated: true } },
    { source: { collection_id: 'c', document_id: 'd', metadata: null }, fragment: { truncated: true } },
  ] } as unknown as StudioRevision['body']
  expect(studioCoverageDescription(partial)).toContain('1 个已引用来源存在解析缺口或片段截断')
  expect(studioCoverageDescription(partial)).not.toContain('省略')
})
