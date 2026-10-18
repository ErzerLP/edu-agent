import { z } from 'zod'
import { learningClient, unwrap } from './client'
import type { Session } from './runtime'
import { sourceIdentitySchema, type SourceSelection } from './source-selection'

const revision = z.number().int().nonnegative()
export const studioPlanSchema = z.object({
  revision,
  title: z.string(),
  coverage: z.enum(['complete', 'partial']),
  sections: z.array(
    z.object({
      id: z.string().min(1),
      topic: z.string(),
      objective: z.string(),
      chapters: z.array(
        sourceIdentitySchema.extend({
          node_id: z.string().min(1),
          node_revision_id: z.string().min(1),
          title: z.string(),
          range: z.object({
            start: revision,
            end: revision,
            start_line: revision,
            end_line: revision,
          }),
        }),
      ),
      unit_count: z.number().int().positive(),
    }),
  ),
  uncovered: z.array(z.string()),
  input_fingerprint: z.string(),
})
export const studioJobSchema = z.object({
  id: z.uuid(),
  kind: z.enum(['guide', 'brief', 'note']),
  version: revision,
  privacy_generation: revision,
  status: z.enum([
    'planning',
    'awaiting_confirmation',
    'queued',
    'failed',
    'invalidated',
    'deleted',
  ]),
  active_plan_revision: revision,
  confirmed_plan_revision: revision,
  plan: studioPlanSchema.optional(),
  messages: z.array(
    z.object({
      operation_id: z.uuid(),
      text: z.string(),
      explanation: z.string(),
      status: z.enum(['pending', 'succeeded', 'failed']),
      plan_revision: revision,
      created_at: z.string(),
    }),
  ),
  suggested_focus: z.array(z.string()),
  reason: z.string().optional(),
  run_id: z.uuid().optional(),
  created_at: z.string(),
  updated_at: z.string(),
})
export type StudioJob = z.infer<typeof studioJobSchema>
export type StudioPlan = z.infer<typeof studioPlanSchema>
export type StudioJobCreate = {
  operation_id: string
  privacy_generation: number
  kind: StudioJob['kind']
  source_selection: Extract<SourceSelection, { mode: 'items' }>
  direction_version: number
  focus?: string
  artifact_id?: string
  expected_artifact_version?: number
}
export type StudioPlanOperation = {
  operation_id: string
  expected_version: number
  plan_revision: number
}
export const studioJobStatus: Record<StudioJob['status'], string> = {
  planning: '正在整理大纲',
  awaiting_confirmation: '待确认大纲',
  queued: '待生成',
  failed: '大纲未完成',
  invalidated: '任务已不可用',
  deleted: '已删除',
}

export function studioJobsAPI(session: Session, spaceId: string) {
  const api = learningClient(session, spaceId)
  const header = { 'X-Learning-Space-ID': spaceId }
  const params = (jobID: string) => ({ header, path: { jobID } })
  return {
    focus: (signal?: AbortSignal) =>
      unwrap(
        api.GET('/v1/learning/studio/focus', { params: { header }, signal }),
        z.object({ suggested_focus: z.array(z.string()), coverage: z.string() }),
      ),
    list: (cursor?: string, signal?: AbortSignal) =>
      unwrap(
        api.GET('/v1/learning/studio/jobs', { params: { header, query: { cursor } }, signal }),
        z.object({ items: z.array(studioJobSchema), next_cursor: z.string().optional() }),
      ),
    get: (id: string, signal?: AbortSignal) =>
      unwrap(
        api.GET('/v1/learning/studio/jobs/{jobID}', { params: params(id), signal }),
        studioJobSchema,
      ),
    create: (body: StudioJobCreate) =>
      unwrap(api.POST('/v1/learning/studio/jobs', { params: { header }, body }), studioJobSchema),
    message: (id: string, body: StudioPlanOperation & { text: string }) =>
      unwrap(
        api.POST('/v1/learning/studio/jobs/{jobID}/plan-messages', { params: params(id), body }),
        studioJobSchema,
      ),
    patch: (
      id: string,
      body: StudioPlanOperation & { delete_ids?: string[]; ordered_ids?: string[] },
    ) =>
      unwrap(
        api.PATCH('/v1/learning/studio/jobs/{jobID}/plan', { params: params(id), body }),
        studioJobSchema,
      ),
    confirm: (id: string, body: StudioPlanOperation) =>
      unwrap(
        api.POST('/v1/learning/studio/jobs/{jobID}/confirm', { params: params(id), body }),
        studioJobSchema,
      ),
    command: (id: string, body: StudioPlanOperation & { kind: 'delete' | 'retry' }) =>
      unwrap(
        api.POST('/v1/learning/studio/jobs/{jobID}/commands', { params: params(id), body }),
        studioJobSchema,
      ),
  }
}
