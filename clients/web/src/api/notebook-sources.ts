import { z } from 'zod'
import { pdfMetadataSchema } from './pdf'

const id = z.uuid()
export const sourceMetadataSchema = z.object({
  kind: z.enum(['text', 'pdf', 'url']),
  title: z.string().optional(),
  locator: z.string().optional(),
  final_url: z.string().optional(),
  media_type: z.string().optional(),
  parser: z.string(),
  coverage: z.string(),
  fingerprint: z.string(),
  status: z.enum(['parsed', 'partial']),
  derived_from: z.object({ artifact_id: id, version: z.number().int().positive() }).optional(),
})
export const sourceItemSchema = z.object({
  collection_id: id,
  collection_name: z.string(),
  revision_id: id,
  document_id: id,
  document_revision_id: id,
  path: z.string(),
  metadata: sourceMetadataSchema.nullable(),
})
export const sourcePageSchema = z.object({
  items: z.array(sourceItemSchema),
  next_cursor: z.string().optional(),
})
export const sourceDetailSchema = sourceItemSchema.extend({
  pdf: pdfMetadataSchema.optional(),
  fragments: z.array(
    z.object({
      node_id: id,
      node_revision_id: id,
      title: z.string(),
      range: z.object({
        start: z.number().int().nonnegative(),
        end: z.number().int().nonnegative(),
        start_line: z.number().int().positive(),
        end_line: z.number().int().positive(),
      }),
      text: z.string(),
      truncated: z.boolean(),
      page: z.number().int().positive().optional(),
    }),
  ),
})
export type SourceItem = z.infer<typeof sourceItemSchema>
export type SourceMetadata = z.infer<typeof sourceMetadataSchema>
export type SourceDetail = z.infer<typeof sourceDetailSchema>
export function sourceTitle(source: Pick<SourceItem, 'path' | 'metadata'>) {
  const metadata = source.metadata
  if (metadata?.kind !== 'url') return source.path
  if (metadata.title?.trim()) return metadata.title.trim()
  for (const raw of [metadata.final_url, metadata.locator]) {
    if (!raw) continue
    try {
      const url = new URL(raw)
      if (url.protocol === 'https:' || url.protocol === 'http:') return url.host + url.pathname
    } catch {
      // Older metadata may not contain a usable URL; try the original locator.
    }
  }
  return source.path
}
export function sourceDescription(metadata: SourceMetadata | null) {
  if (!metadata) return '已保存 · 来源类型与解析覆盖未知'
  if (metadata.derived_from)
    return `已保存 · 笔记第 ${metadata.derived_from.version} 版快照 · 个人理解，非外部权威`
  const kind = { text: '文本', pdf: 'PDF', url: '网址' }[metadata.kind]
  if (metadata.coverage === 'partial_text_limit')
    return '已保存 · 网址 · 部分解析：正文已截断（16 KB 上限）'
  return `已保存 · ${kind} · ${metadata.status === 'partial' ? '部分解析，有覆盖缺口' : metadata.kind === 'text' ? '用户提供的文本' : '已提取文本，完整性未核验'}`
}

// Summaries stay plain strings: remove presentation markers before truncation,
// retaining code contents, link labels and literal arithmetic/underscores.
function plainSummary(text: string) {
  return text
    .replace(/^[ \t]*(?:`{3,}|~{3,})[^\n]*$/gm, '')
    .replace(/^[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)+\|?[ \t]*$/gm, '')
    .replace(/^[ \t]*(?:[-*_][ \t]*){3,}$|^[ \t]*=+[ \t]*$/gm, '')
    .replace(/^[ \t]*#{1,6}[ \t]+(.+?)(?:[ \t]+#+)?[ \t]*$/gm, '$1')
    .replace(/^[ \t]*(?:>[ \t]*)+/gm, '')
    .replace(/^[ \t]*(?:[-+*]|\d+[.)])[ \t]+(?:\[[ xX]\][ \t]*)?/gm, '')
    .replace(/!\[([^\]]*)\]\([^\n]*?\)|\[([^\]]+)\]\([^\n]*?\)/g, (_, image, link) => image ?? link)
    .replace(/!?\[([^\]]+)\]\[[^\]]*\]/g, '$1')
    .replace(/^[ \t]*\[[^\]]+\]:[ \t]+.*$/gm, '')
    .replace(/<((?:https?:\/\/|mailto:)[^>]+)>/g, '$1')
    .replace(/<\/?[a-zA-Z][^>]*>/g, '')
    .replace(/(`+)([\s\S]*?)\1/g, '$2')
    .replace(/(\*\*|__|~~)(.+?)\1/g, '$2')
    .replace(/(?<!\\)\*(\S(?:.*?\S)?)\*/g, '$1')
    .replace(/(^|[^\p{L}\p{N}_])_(\S(?:.*?\S)?)_(?![\p{L}\p{N}_])/gu, '$1$2')
    .replace(/^[ \t]*\|(.+)\|[ \t]*$/gm, (_, row: string) => row.replace(/(?<!\\)\|/g, ' '))
    .replace(/\\([\\`*_{}\[\]()#+\-.!>|~])/g, '$1')
    .replace(/\s+/g, ' ')
    .trim()
}

export function sourceOverview(source: SourceDetail) {
  const title =
    source.metadata?.title?.trim() ||
    (source.metadata?.kind === 'url' && sourceTitle(source) !== source.path
      ? sourceTitle(source)
      : source.metadata
        ? source.fragments.find((fragment) => fragment.title)?.title
        : '') ||
    source.collection_name
  const body = source.fragments
    .map((fragment) => {
      // URL imports wrap their body in a server-generated provenance JSON line.
      // Only unwrap that format; JSON supplied as an ordinary text source is content.
      const text =
        source.metadata?.kind === 'url'
          ? fragment.text.replace(/^\s*\{[^\n]*\}\s*(?:\n|$)/, '')
          : fragment.text
      return plainSummary(text)
    })
    .filter(Boolean)
    .join(' ')
  return { title, excerpt: body.slice(0, 160) || '没有可定位的正文片段。' }
}
