import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'
import { webCitationSchema, webImportSchema, webProgressSchema } from './web-search'
import { sourceResearchLimitsSchema } from './settings'

const version = z.number().int().positive(), count = z.number().int().nonnegative()
const source = z.object({ collection_id: z.uuid(), revision_id: z.uuid(), document_id: z.uuid(), document_revision_id: z.uuid() })
export const researchJobSchema = z.object({
  destination_change: z.object({model:z.object({endpoint:z.string(),model:z.string()}),search:z.object({endpoint:z.string()}),confirmation:z.string()}).optional(),
  import_preview_id: z.string(),
  round_number: version, job_id: z.uuid(), round_id: z.uuid(), run_id: z.uuid(), version, selection_version: version, event_seq: count,
  status: z.enum(['queued', 'running', 'stopping', 'stopped', 'paused_tool_steps', 'completed', 'failed', 'outcome_unknown']),
  stage: z.string(), request: z.string(), limits: sourceResearchLimitsSchema,
  directions: z.array(z.object({ id: z.string(), title: z.string(), purpose: z.string(), query: z.string(), state: z.string() })),
  progress: webProgressSchema, candidate_count: count, result_snapshot_id: z.string(), report_revision_id: z.string(), selected_report_revision_id: z.string(),
  available_actions: z.array(z.enum(['stop', 'resume', 'more', 'delete', 'import'])), usage_task_id: z.string(), expires_at: z.string(),
})
export const researchCandidateSchema = z.object({
  round_id: z.uuid(), id: z.uuid(), document_id: z.uuid(), document_revision_id: z.uuid(), title: z.string(), url: z.url(), media_type: z.string(), reason: z.string(), quality_notes: z.string(), coverage: z.string(),
  selected: z.boolean(), explicitly_changed: z.boolean(), requires_partial_accept: z.boolean(), physical_pages: count, source: source.optional(),
})
export const researchReportSchema = z.object({
  round_id: z.uuid(), version, based_on_revision: z.string(), revision_id: z.uuid(), status: z.enum(['partial', 'complete']), gaps: z.array(z.string()), next_ordinal: count, model: z.string(), created_at: z.string(),
  sections: z.array(z.object({ id: z.string(), ordinal: version, title: z.string(), body: z.string(), citations: z.array(webCitationSchema), gaps: z.array(z.string()) })),
})
export const researchPreviewSchema = z.object({
  preview_id: z.uuid(), preview_version: version, expires_at: z.string(), result_snapshot_id: z.uuid(), report_revision_id: z.string(),
  items: z.array(z.object({ item_id: z.uuid(), kind: z.enum(['document', 'report']), operation_id: z.uuid(), web_preview_id: z.uuid().optional(), document_revision_id: z.uuid().optional(), title: z.string(), coverage: z.string(), requires_partial_accept: z.boolean(), already_imported: z.boolean(), source: source.optional() })),
})
export const researchEventSchema = z.object({ seq: version, type: z.string(), stage: z.string(), status: z.string(), progress: webProgressSchema, candidate_count: count, created_at: z.string() })
export type ResearchJob = z.infer<typeof researchJobSchema>
export type ResearchCandidate = z.infer<typeof researchCandidateSchema>
export type ResearchReport = z.infer<typeof researchReportSchema>
export type ResearchPreview = z.infer<typeof researchPreviewSchema>
export type ResearchEvent = z.infer<typeof researchEventSchema>
export function sourceResearchAPI(session: Session, spaceId: string) {
  const client = learningClient(session, spaceId), header = { 'X-Learning-Space-ID': spaceId }
  return {
    capabilities: () => unwrap(client.GET('/v1/learning/source-research/capabilities', { params: { header } }), z.object({ available: z.boolean(), can_import: z.boolean(), limits: sourceResearchLimitsSchema })),
    list: () => unwrap(client.GET('/v1/learning/source-research/jobs', { params: { header } }), z.object({ items: z.array(researchJobSchema), next_cursor: z.string() })),
    start: (body: components['schemas']['SourceResearchCreate']) => unwrap(client.POST('/v1/learning/source-research/jobs', { params: { header }, body }), researchJobSchema),
    job: (jobID: string) => unwrap(client.GET('/v1/learning/source-research/jobs/{jobID}', { params: { header, path: { jobID } } }), researchJobSchema),
    candidates: (jobID: string, cursor?:string) => unwrap(client.GET('/v1/learning/source-research/jobs/{jobID}/candidates', { params: { header, path: { jobID }, query:{cursor} } }), z.object({ items: z.array(researchCandidateSchema), next_cursor: z.string().optional() })),
    report: (jobID: string, revisionID: string, after = 0) => unwrap(client.GET('/v1/learning/source-research/jobs/{jobID}/reports/{revisionID}', { params: { header, path: { jobID, revisionID }, query: { after } } }), researchReportSchema),
    select: (jobID: string, body: components['schemas']['SourceResearchSelection']) => unwrap(client.PATCH('/v1/learning/source-research/jobs/{jobID}/selection', { params: { header, path: { jobID } }, body }), researchJobSchema),
    command: (jobID: string, body: components['schemas']['SourceResearchCommand']) => unwrap(client.POST('/v1/learning/source-research/jobs/{jobID}/commands', { params: { header, path: { jobID } }, body }), researchJobSchema),
    more: (jobID: string, body: components['schemas']['SourceResearchMore']) => unwrap(client.POST('/v1/learning/source-research/jobs/{jobID}/rounds', { params: { header, path: { jobID } }, body }), researchJobSchema),
    reports: (jobID: string, before = 0) => unwrap(client.GET('/v1/learning/source-research/jobs/{jobID}/reports', { params: { header, path: { jobID }, query: { before } } }), z.object({items: z.array(researchReportSchema), next_before: count})),
    preview: (jobID: string, body: components['schemas']['SourceResearchPreviewRequest']) => unwrap(client.POST('/v1/learning/source-research/jobs/{jobID}/import-previews', { params: { header, path: { jobID } }, body }), researchPreviewSchema),
    imports: (jobID: string, preview_id: string) => unwrap(client.GET('/v1/learning/source-research/jobs/{jobID}/imports', { params: { header, path: { jobID }, query: { preview_id } } }), z.object({preview:researchPreviewSchema,items:z.array(z.object({ item_id: z.uuid(), status: z.string(), source: source.optional() }))})),
    import: (jobID: string, preview: ResearchPreview, item: ResearchPreview['items'][number], accept_partial: boolean) => item.kind === 'report'
      ? unwrap(client.POST('/v1/learning/source-research/jobs/{jobID}/report-imports', { params: { header, path: { jobID } }, body: { operation_id: item.operation_id, preview_id: preview.preview_id, expected_preview_version: preview.preview_version, report_revision_id: item.item_id, storage_consent: true, accept_partial } }), webImportSchema)
      : unwrap(client.POST('/v1/learning/web-imports', { params: { header }, body: { operation_id: item.operation_id, preview_id: item.web_preview_id!, expected_preview_version: 1, document_revision_id: item.document_revision_id!, storage_consent: true, accept_partial } }), webImportSchema),
    async watch(jobID: string, after: number, signal: AbortSignal, receive: (events: ResearchEvent[]) => void) {
      const response = await client.GET('/v1/learning/source-research/jobs/{jobID}/events', { params: { header, path: { jobID }, query: { after } }, signal, parseAs: 'stream' })
      if (!response.response.ok || !response.data) throw new ApiError(response.response.status, 'stream_unavailable')
      const reader = response.data.getReader(), decoder = new TextDecoder()
      let buffer = ''
      try {
        while (!signal.aborted) {
          const chunk = await reader.read(); if (chunk.done) break
          buffer += decoder.decode(chunk.value, { stream: true }).replace(/\r/g, '')
          if (buffer.length > 131072) throw new ApiError(502, 'invalid_response')
          const events: ResearchEvent[] = []
          let end: number
          while ((end = buffer.indexOf('\n\n')) >= 0) {
            const frame = buffer.slice(0, end); buffer = buffer.slice(end + 2)
            const data = frame.split('\n').find(line => line.startsWith('data: '))
            if (data) events.push(researchEventSchema.parse(JSON.parse(data.slice(6))))
          }
          if (events.length) receive(events)
        }
      } finally { await reader.cancel().catch(() => {}); reader.releaseLock() }
    },
  }
}
