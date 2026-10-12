import { z } from 'zod'
import { learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'
import type { DraftStore } from '@/lib/drafts'
import type { ScanItem } from '@/lib/reference-import'
import {
  sourceManifestSchema,
  mentorCitationSchema,
  sourceSelectionSchema,
} from './source-selection'

const version = z.number().int().positive()
export const studioClaimSchema = z
  .object({ text: z.string().min(1).max(1500), citations: z.array(z.string()).min(1).max(10) })
  .strict()
const claims = z.array(studioClaimSchema).min(1).max(8)
export const studioGuideSchema = z
  .object({ key_points: claims, terms: claims, learning_order: claims, misconceptions: claims })
  .strict()
export const studioBriefSchema = z
  .object({ overview: claims, differences: claims, gaps: claims })
  .strict()
export const studioQuizSchema = z
  .object({
    goal_version: version,
    questions: z
      .array(
        z
          .object({
            prompt: z.string().trim().min(1).max(1500),
            rubric: z
              .array(
                z
                  .object({
                    id: z.string().trim().min(1).max(100),
                    criterion: z.string().trim().min(1).max(1000),
                  })
                  .strict(),
              )
              .min(1)
              .max(10)
              .refine((items) => new Set(items.map((i) => i.id)).size === items.length),
            allowed_help: z
              .array(z.enum(['none', 'hint', 'scaffold', 'answer_revealed']))
              .min(1)
              .max(4)
              .refine((items) => new Set(items).size === items.length),
            citations: z.array(z.string()).min(1).max(10),
          })
          .strict(),
      )
      .min(1)
      .max(8),
  })
  .strict()
export const studioQuizStateSchema = z.object({
  artifact_id: z.uuid(),
  version,
  goal_version: version,
  questions: z.array(
    z.object({
      question: z.number().int().min(1).max(8),
      session_id: z.uuid(),
      activity_id: z.uuid(),
      content_artifact_id: z.uuid(),
      state: z.string(),
      completed: z.boolean(),
      attempt_id: z.uuid().optional(),
    }),
  ),
})
export type StudioQuizStart = components['schemas']['StudioQuizStart']
export const studioGenerateSchema = z.object({
  operation_id: z.uuid(),
  privacy_generation: version,
  kind: z.enum(['guide', 'brief', 'quiz']),
  goal_id: z.uuid().nullable(),
  goal_version: z.number().int().nonnegative(),
  artifact_id: z.uuid().nullable(),
  expected_version: z.number().int().nonnegative(),
  source_selection: sourceSelectionSchema,
  request_budget: version,
  token_budget: version,
})
export const studioStateSchema = z.object({
  request: studioGenerateSchema,
  result: z.object({ artifact_id: z.uuid(), version }).optional(),
})
export type StudioGenerate = components['schemas']['StudioGenerate']
export type StudioClaim = z.infer<typeof studioClaimSchema>
const itemSchema = z.object({
  artifact_id: z.uuid(),
  goal_id: z.uuid().nullable(),
  kind: z.enum(['note', 'guide', 'brief', 'quiz']),
  version,
  title: z.string(),
  updated_at: z.string(),
})
export const studioPageSchema = z.object({
  items: z.array(itemSchema),
  next_cursor: z.string().optional(),
})
export const studioRevisionSchema = z
  .object({
    artifact_id: z.uuid(),
    learning_space_id: z.uuid(),
    goal_id: z.uuid().nullable(),
    kind: z.enum(['note', 'guide', 'brief', 'quiz']),
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
      coverage: z.enum(['complete', 'partial']).optional(),
      guide: studioGuideSchema.optional(),
      brief: studioBriefSchema.optional(),
      quiz: studioQuizSchema.optional(),
    }),
  })
  .superRefine((r, ctx) => {
    if (r.kind === 'note') {
      if (
        r.body.guide ||
        r.body.brief ||
        r.body.quiz ||
        r.body.source_manifest ||
        r.body.citations.length
      )
        ctx.addIssue({ code: 'custom', message: 'Invalid note provenance' })
    } else {
      if (
        !r.body.coverage ||
        !r.body.source_manifest?.entries.length ||
        !r.body.model ||
        !r.body.input_fingerprint ||
        (r.kind === 'guide') !== !!r.body.guide ||
        (r.kind === 'brief') !== !!r.body.brief ||
        (r.kind === 'quiz') !== !!r.body.quiz ||
        (r.kind === 'quiz' && !r.goal_id)
      )
        ctx.addIssue({ code: 'custom', message: 'Missing generated document' })
      const ids = new Set(r.body.citations.map((c) => c.id))
      const groups = r.body.guide ?? r.body.brief
      const claims = r.body.quiz?.questions ?? (groups ? Object.values(groups).flat() : [])
      if (claims.some((claim) => claim.citations.some((id: string) => !ids.has(id))))
        ctx.addIssue({ code: 'custom', message: 'Unverified citation' })
    }
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
    quiz: (id: string, version: number, signal?: AbortSignal) =>
      unwrap(
        api.GET('/v1/learning/studio/artifacts/{artifactID}/quiz', {
          params: { header, path: { artifactID: id }, query: { version } },
          signal,
        }),
        studioQuizStateSchema,
      ),
    startQuiz: (id: string, body: StudioQuizStart) =>
      unwrap(
        api.POST('/v1/learning/studio/artifacts/{artifactID}/quiz/start', {
          params: { header, path: { artifactID: id } },
          body,
        }),
        studioQuizStateSchema,
      ),
    quizOperation: (operationID: string) =>
      unwrap(
        api.GET('/v1/learning/studio/quiz-operations/{operationID}', {
          params: { header, path: { operationID } },
        }),
        studioQuizStateSchema,
      ),
    list: async (cursor?: string, signal?: AbortSignal) => {
      return unwrap(
        api.GET('/v1/learning/studio/artifacts', { params: { header, query: { cursor } }, signal }),
        studioPageSchema,
      )
    },
    get: async (id: string, signal?: AbortSignal, version?: number) => {
      return unwrap(
        api.GET('/v1/learning/studio/artifacts/{artifactID}', {
          params: { header, path: { artifactID: id }, query: { version } },
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
    generate: (body: StudioGenerate) =>
      unwrap(
        api.POST('/v1/learning/studio/generations', { params: { header }, body }),
        z.object({ operation_id: z.uuid(), run_id: z.uuid(), session_id: z.uuid(), version }),
      ),
  }
}
