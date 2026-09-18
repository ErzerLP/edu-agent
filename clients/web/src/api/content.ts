import { z } from 'zod'
import { pdfPageSchema } from './pdf'
import {
  referenceSchema,
  selectionSchema,
  type Block,
  type Content,
  type ContentSelection,
} from './teaching'

const version = z.number().int().positive()
export const preferenceSchema = z.object({
  favorite: z.boolean(),
  pinned_version: version.nullable(),
})
export const librarySchema = z.object({
  items: z.array(
    z.object({
      artifact_id: z.uuid(),
      goal_id: z.uuid(),
      session_id: z.uuid(),
      version,
      title: z.string(),
      kind: z.enum(['reading', 'exercise']),
      source_status: z.enum(['available', 'missing', 'restricted']),
      nodes: z.array(z.uuid()),
      knowledge_points: z.array(z.object({ id: z.uuid(), name: z.string() })),
      updated_at: z.string(),
      favorite: z.boolean(),
      pinned_version: version.nullable(),
    }),
  ),
  next_cursor: z.uuid().optional(),
})
export const citationSchema = z.object({
  pdf: z.object({ fingerprint: z.string(), page_count: z.number().int(), pages: z.array(pdfPageSchema), collection_id: z.uuid().optional() }).optional(),
  reference: referenceSchema,
  title: z.string(),
  context: z.string(),
  status: z.enum(['available', 'missing_fragment']),
  parser: z.string(),
  historical: z.boolean(),
  coverage: z.string(),
  locator: z.string(),
})
export const exportSchema = z.object({
  filename: z.string(),
  media_type: z.string(),
  text: z.string(),
})
export const editStateSchema = z.object({
  request: z.object({
    selection: selectionSchema,
    action: z.enum(['explain', 'example', 'expand', 'critique', 'rewrite']),
  }),
  reason: z.string(),
  result: z
    .object({ artifact_id: z.uuid(), version, changed_blocks: z.array(z.uuid()) })
    .optional(),
})
export type EditAction = z.infer<typeof editStateSchema>['request']['action']

export async function contentSelection(
  content: Content,
  block: Block,
  start = 0,
  end = (block.text ?? '').length,
): Promise<ContentSelection> {
  const text = block.text ?? ''
  if (start < 0 || end <= start || end > text.length) throw new Error('无效选区')
  const encode = new TextEncoder()
  const hash = await crypto.subtle.digest('SHA-256', encode.encode(text.slice(start, end)))
  return {
    space_id: content.learning_space_id,
    goal_id: content.goal_id,
    session_id: content.session_id,
    artifact_id: content.artifact_id,
    version: content.version,
    block_id: block.block_id,
    start: encode.encode(text.slice(0, start)).length,
    end: encode.encode(text.slice(0, end)).length,
    sha256: Array.from(new Uint8Array(hash), (b) => b.toString(16).padStart(2, '0')).join(''),
  }
}

export function flattenBlocks(blocks: Block[]): Block[] {
  return blocks.flatMap((block) => [block, ...flattenBlocks(block.children ?? [])])
}
export function contentDiff(before: Block[], after: Block[]) {
  const old = new Map(flattenBlocks(before).map((b) => [b.block_id, b]))
  const next = new Map(flattenBlocks(after).map((b) => [b.block_id, b]))
  return [...new Set([...old.keys(), ...next.keys()])]
    .filter((id) => JSON.stringify(old.get(id)) !== JSON.stringify(next.get(id)))
    .map((id) => ({ id, before: old.get(id), after: next.get(id) }))
}
