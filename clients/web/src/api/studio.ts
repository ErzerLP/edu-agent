import { z } from 'zod'
import { learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'
import type { DraftStore } from '@/lib/drafts'
import type { ScanItem } from '@/lib/reference-import'
import { sourceManifestSchema, mentorCitationSchema } from './source-selection'

const version = z.number().int().positive()
const itemSchema = z.object({
  artifact_id: z.uuid(),
  goal_id: z.uuid().nullable(),
  kind: z.literal('note'),
  version,
  title: z.string(),
  updated_at: z.string(),
})
export const studioPageSchema = z.object({
  items: z.array(itemSchema),
  next_cursor: z.string().optional(),
})
export const studioRevisionSchema = z.object({
  artifact_id: z.uuid(),
  learning_space_id: z.uuid(),
  goal_id: z.uuid().nullable(),
  kind: z.literal('note'),
  version,
  privacy_generation: version,
  actor_device_id: z.uuid(),
  created_at: z.string(),
  body: z.object({
    schema: z.literal(1),
    title: z.string(),
    markdown: z.string(),
    source_manifest: sourceManifestSchema.optional(),
    citations: z.array(mentorCitationSchema),
    model: z.string().optional(),
    input_fingerprint: z.string().optional(),
  }),
})
const historySchema = z.object({
  items: z.array(z.object({ version, status: z.literal('committed'), created_at: z.string() })),
})
const candidateSchema = z.object({
  title: z.string(),
  derived_from: z.object({ artifact_id: z.uuid(), version }),
  sha256: z.string().regex(/^[a-f0-9]{64}$/),
  document: z.object({ path: z.string(), markdown: z.string(), source_receipt: z.string().min(1) }),
})
export type StudioRevision = z.infer<typeof studioRevisionSchema>
export type StudioSave = components['schemas']['StudioSave']
export type NoteSourceCandidate = z.infer<typeof candidateSchema>
export type NoteDraft = {
  title: string
  markdown: string
  baseVersion: number
  goalId: string | null
  pending?: StudioSave
  unknown?: boolean
  conflict?: boolean
}
export function studioRequest(
  drafts: DraftStore,
  key: string,
  draft: NoteDraft,
  generation: number,
): StudioSave {
  const body = {
    kind: 'note' as const,
    title: draft.title,
    markdown: draft.markdown,
    goal_id: draft.goalId,
    expected_version: draft.baseVersion,
    privacy_generation: generation,
  }
  return { ...body, operation_id: drafts.operation(key, body) }
}
export function noteSourceItem(candidate: NoteSourceCandidate): ScanItem {
  return {
    id: crypto.randomUUID(),
    name: candidate.title,
    path: candidate.document.path,
    bytes: new TextEncoder().encode(candidate.document.markdown).length,
    status: 'ready',
    reason: '已保存笔记快照；个人理解，不是外部权威',
    selected: true,
    markdown: candidate.document.markdown,
    sourceReceipt: candidate.document.source_receipt,
    coverage: `笔记第 ${candidate.derived_from.version} 版 · 独立副本`,
  }
}
export function studioAPI(session: Session, spaceId: string) {
  const header = { 'X-Learning-Space-ID': spaceId }
  const api = learningClient(session, spaceId)
  return {
    list: async (cursor?: string, signal?: AbortSignal) => {
      return unwrap(
        api.GET('/v1/learning/studio/artifacts', { params: { header, query: { cursor } }, signal }),
        studioPageSchema,
      )
    },
    get: async (id: string, signal?: AbortSignal) => {
      return unwrap(
        api.GET('/v1/learning/studio/artifacts/{artifactID}', {
          params: { header, path: { artifactID: id } },
          signal,
        }),
        studioRevisionSchema,
      )
    },
    history: async (id: string, signal?: AbortSignal) => {
      return unwrap(
        api.GET('/v1/learning/studio/artifacts/{artifactID}/revisions', {
          params: { header, path: { artifactID: id } },
          signal,
        }),
        historySchema,
      )
    },
    save: async (id: string | undefined, body: StudioSave) => {
      return unwrap(
        id
          ? api.PUT('/v1/learning/studio/artifacts/{artifactID}', {
              params: { header, path: { artifactID: id } },
              body,
            })
          : api.POST('/v1/learning/studio/artifacts', { params: { header }, body }),
        studioRevisionSchema,
      )
    },
    operation: async (id: string) => {
      return unwrap(
        api.GET('/v1/learning/studio/operations/{operationID}', {
          params: { header, path: { operationID: id } },
        }),
        studioRevisionSchema,
      )
    },
    candidate: async (id: string, version: number) => {
      return unwrap(
        api.POST('/v1/learning/studio/artifacts/{artifactID}/import-candidate', {
          params: { header, path: { artifactID: id } },
          body: { version },
        }),
        candidateSchema,
      )
    },
  }
}
