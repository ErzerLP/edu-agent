import { z } from 'zod'

const ordinal = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
export const messageAnchorSchema = z.object({
  message_id: z.uuid(), run_id: z.uuid(), message_slot: z.number().int().min(-1), ordinal,
})
export type MessageAnchor = z.infer<typeof messageAnchorSchema>
const point = z.object({ text: z.string(), anchor_ids: z.array(z.uuid()) })
export const conversationTopicSchema = z.object({
  id: z.uuid(), title: z.string(), revision: ordinal,
  anchor_count: ordinal, exercise_related: z.boolean(), points: z.array(point),
  observations: z.array(point.extend({
    state: z.enum(['understood_in_chat', 'question_open', 'exercise_error_observed', 'unassessed']),
    evidence: z.enum(['user_report', 'tutor_explanation']),
  })),
})
export type ConversationTopic = z.infer<typeof conversationTopicSchema>
export const topicPageSchema = z.object({
  conversation_id: z.uuid(), privacy_generation: z.number().int().positive(),
  revision: ordinal, covered_through: ordinal, covered_tail: ordinal,
  covered_messages: ordinal, total_messages: ordinal, unmapped_turns: ordinal,
  status: z.enum(['not_started', 'queued', 'running', 'ready', 'failed']),
  items: z.array(conversationTopicSchema).max(50), next_cursor: z.uuid().optional(),
})
export const topicAnchorPageSchema = z.object({
  conversation_id: z.uuid(), privacy_generation: z.number().int().positive(),
  items: z.array(messageAnchorSchema).max(50), next_cursor: ordinal.optional(),
})
