import { z } from 'zod'

export const researchRequestSchema = z.object({
  topic: z.string().min(1).max(300), external_consent: z.literal(true), auto_adopt: z.boolean(),
  policy: z.object({ mode: z.enum(['supplement', 'prefer', 'restrict']), domains: z.array(z.string()).max(5) }),
})
export const sourceSchema = z.object({
  space_id: z.uuid(), goal_id: z.uuid(), purpose: z.literal('goal_reference'),
  id: z.uuid(), revision_id: z.string(), locator: z.string(), final_url: z.string(), title: z.string(),
  kind: z.string(), status: z.enum(['candidate', 'failed', 'parsed', 'partial', 'adopted', 'rejected']),
  failure: z.string(), fetched_at: z.string().optional(), fingerprint: z.string(), parser: z.string(),
  coverage: z.string(), storage_allowed: z.boolean(), text: z.string().max(16000),
  fragments: z.array(z.object({ id: z.uuid(), start: z.number().int().nonnegative(), end: z.number().int().nonnegative(), text: z.string() })),
  knowledge_revision_id: z.string(), collection_id: z.string(),
})
export const researchStateSchema = z.object({
  request: researchRequestSchema, discovered: z.boolean(), sources: z.array(sourceSchema).max(4), search_configuration: z.string(),
  synthesis: z.object({
    points: z.array(z.object({ text: z.string(), citations: z.array(z.object({ source_id: z.uuid(), revision_id: z.uuid(), fragment_id: z.uuid(), quote: z.string() })) })),
    gaps: z.array(z.string()), examples: z.array(z.string()),
  }).optional(),
})
export type ResearchSource = z.infer<typeof sourceSchema>
export const sourceStatus: Record<ResearchSource['status'], string> = {
  candidate: '候选，尚未读取', failed: '获取失败', parsed: '已读取文本', partial: '部分解析', adopted: '已纳入本目标参考', rejected: '已拒绝',
}
export const researchStage: Record<string, string> = {
  references_loaded: '已读取获准的固定参考片段',
  preparing_activity: '准备当前活动', activity_prepared: '当前活动已准备，正在发布',
  learning_started: '课堂已就绪', sources_insufficient: '来源不足，目标和已有结果已保留',
  queued: '等待执行', query_planned: '查询已拟定', running: '正在研究', discovering: '搜索候选', discovered: '候选已发现',
  fetching: '读取正文', parsed: '解析正文', checking_fragments: '核对相关片段', waiting_model: '整理有来源的要点',
  synthesized: '综合已保存', completed: '研究结束', paused_budget: '预算用尽', cancelled: '已停止', partial: '仅有部分结果', failed: '研究失败',
}
