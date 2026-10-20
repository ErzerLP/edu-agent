import { z } from 'zod'
import { sourceItemSchema, sourceDetailSchema } from './notebook-sources'
import { webCitationSchema } from './web-search'
import type { components } from './schema'

export const sourceIdentitySchema = z.object({
  collection_id: z.uuid(),
  revision_id: z.uuid(),
  document_id: z.uuid(),
  document_revision_id: z.uuid(),
})
export type SourceIdentity = z.infer<typeof sourceIdentitySchema>
export const sourceSelectionSchema = z.discriminatedUnion('mode', [
  z.object({ mode: z.literal('none'), items: z.array(sourceIdentitySchema).length(0) }),
  z.object({ mode: z.literal('items'), items: z.array(sourceIdentitySchema).min(1).max(100) }),
])
export type SourceSelection = components['schemas']['SourceSelection']
const hash = z.string().regex(/^[a-f0-9]{64}$/)
export const sourceManifestSchema = z.object({
  schema: z.literal(1),
  space_id: z.uuid(),
  generation: z.number().int().positive(),
  sha256: hash,
  entries: z.array(sourceIdentitySchema.extend({ sha256: hash })).max(100),
})
export const learningSourceCitationSchema = z.object({
  id: z.string().regex(/^[1-9][0-9]{0,2}$/),
  source: sourceItemSchema,
  fragment: sourceDetailSchema.shape.fragments.element,
  document_sha256: hash,
  slice_sha256: hash,
})
export const mentorCitationSchema = z.discriminatedUnion('origin', [learningSourceCitationSchema.extend({ origin: z.literal('learning_source') }), webCitationSchema])
export type MentorCitation = z.infer<typeof mentorCitationSchema>
// Staged Studio artifacts cite sections as `<unit>:<n>`; other fields stay identical.
export const studioCitationSchema = learningSourceCitationSchema.extend({ id: z.string().regex(/^[A-Za-z0-9._-]{1,128}(:[1-9][0-9]{0,3})?$/) })
export const sourceKey = (s: SourceIdentity) =>
  `${s.collection_id}:${s.revision_id}:${s.document_id}:${s.document_revision_id}`
export function selectedSources(
  selected: SourceIdentity[] | undefined,
  available: SourceIdentity[],
): SourceSelection | undefined {
  if (!selected) return undefined
  const keys = new Set(available.map(sourceKey))
  const items = selected
    .filter((s) => keys.has(sourceKey(s)))
    .map((s) => sourceIdentitySchema.parse(s))
  return items.length ? { mode: 'items', items } : { mode: 'none', items: [] }
}
export function citationParts(text: string, ids: string[]): { text: string; id?: string }[] {
  return text
    .split(/(\[[1-9][0-9]{0,2}\])/g)
    .filter(Boolean)
    .map((part) => {
      const id = part.slice(1, -1)
      return part.startsWith('[') && part.endsWith(']') && ids.includes(id)
        ? { text: part, id }
        : { text: part }
    })
}
