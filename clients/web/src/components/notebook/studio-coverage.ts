import type { StudioRevision } from '@/api/studio'

export function studioCoverageDescription(body: StudioRevision['body']) {
  if (body.coverage !== 'partial') return ''
  const limited = new Set(body.citations
    .filter(c => c.fragment.truncated || c.source.metadata?.status === 'partial')
    .map(c => `${c.source.collection_id}:${c.source.document_id}`))
  if (limited.size) return `有 ${limited.size} 个已引用来源存在解析缺口或片段截断，未覆盖部分未补写。`
  return `本次来源片段不足以覆盖全部内容，未覆盖部分未补写。${body.brief ? '具体范围见下方“覆盖缺口”。' : ''}`
}
