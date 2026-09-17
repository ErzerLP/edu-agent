import { z } from 'zod'
import { researchRequestSchema } from './research'

const support = z.object({
  source_id: z.uuid(),
  revision_id: z.uuid(),
  fragment_id: z.uuid(),
  quote: z.string(),
})
export const knowledgeContextSchema = z.object({
  id: z.uuid(),
  scope_snapshot_id: z.uuid(),
  previous_revision_id: z.uuid().optional(),
  policy: z.object({ id: z.uuid(), goal_revision_id: z.uuid(), request: researchRequestSchema }),
  concepts: z.array(
    z.object({
      concept_id: z.uuid(),
      revision_id: z.uuid(),
      semantic_key: z.string(),
      name: z.string(),
      support: z.array(support).min(1),
    }),
  ),
})
export const startStateSchema = z.object({
  model_id: z.string().optional(),
  request: z.object({ new_session: z.literal(true), model_consent: z.literal(true) }),
  prepared: z
    .object({
      concept_key: z.string(),
      name: z.string(),
      prompt: z.string(),
      criterion: z.string(),
      citations: z.array(support),
    })
    .optional(),
  result: z
    .object({
      session_id: z.uuid(),
      activity_id: z.uuid(),
      artifact_id: z.uuid(),
      artifact_version: z.number().int().positive(),
      knowledge_context: knowledgeContextSchema,
    })
    .optional(),
})
export const startCapabilitiesSchema = z.object({
  protocol_version: z.literal(1),
  available: z.boolean(),
  reason: z.string(),
  request_budget: z.number().int().positive(),
  token_budget: z.number().int().positive(),
  legacy_projection: z.literal('open_activity'),
})
export const startUnavailable: Record<string, string> = {
  start_storage_unavailable: '服务器尚未配置开学运行与正文保存，请联系操作者。',
  research_configuration_required: '请在设置页配置并启用搜索和 Web 导师。',
  research_pairing_required: '自动获取资料需要研究权限，请使用研究配对码重新配对。',
}

export function publicTopic(text: string): string {
  let value = ''
  for (const char of [...text.replace(/[\r\n]+/g, ' ').trim()].slice(0, 100)) {
    if (new TextEncoder().encode(value + char).length > 300) break
    value += char
  }
  return value
}
