import { z } from 'zod'
import { ApiError } from './client'

const version = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
export const conversationSchema = z.object({
  id: z.uuid(), space_id: z.uuid(), goal_id: z.uuid().optional(), teaching_session_id: z.uuid().optional(),
  privacy_generation: version, version, saved: z.boolean(), title: z.string(),
  provider: z.string(), endpoint: z.string(), updated_at: z.iso.datetime({ offset: true }),
  current_run_id: z.uuid().optional(), goal_version: z.number().int().nonnegative(), writable: z.boolean(),
  storage_state: z.enum(['saved', 'temporary', 'temporary_unavailable', 'run_storage_unavailable', 'tutor_history_schema_unsupported']),
  destination: z.string(), destination_provider: z.string(), destination_endpoint: z.string(), confirmation_required: z.boolean(),
})
export type TutorConversation = z.infer<typeof conversationSchema>
export const conversationsSchema = z.object({ items: z.array(conversationSchema).max(50), next_cursor: z.string().optional(), save_available: z.boolean() })
export const conversationCreated = z.object({ id: z.uuid() })
export const conversationConfirmed = z.object({ confirmed: z.literal(true) })
export const tutorMessage = z.object({
  role: z.enum(['user', 'assistant', 'tool']), content: z.string().optional(), tool_call_id: z.string().optional(),
  tool_calls: z.array(z.object({ id: z.string(), type: z.string(), function: z.object({ name: z.string(), arguments: z.string() }) })).optional(),
})
export const turnsSchema = z.object({
  conversation: conversationSchema,
  items: z.array(z.object({ run_id: z.uuid(), ordinal: version, status: z.string(), body_available: z.boolean(), messages: z.array(tutorMessage), output: z.string() })).max(50),
  next_cursor: version.optional(),
})

export function checkConversation(c: TutorConversation, space: string, generation: number, id?: string) {
  if (c.space_id !== space || c.privacy_generation !== generation || id && c.id !== id) throw new ApiError(502, 'invalid_response')
  return c
}

export const conversationDraftKey = (prefix: readonly unknown[], id: string) => JSON.stringify([...prefix, 'tutor-conversation', id])
export const conversationSelectionKey = (prefix: readonly unknown[], space: string, goal?: string, teaching?: string) => JSON.stringify([...prefix, 'tutor-selection', space, goal, teaching])

export type TurnSubmission = { operation_id: string; expected_version: number; prompt: string; request_budget: number; token_budget: number; confirm_destination?: string }
type Result<T, Code extends number = 200> = { responses: { [K in Code]: { content: { 'application/json': T } } } }
type Body<T> = { requestBody: { content: { 'application/json': T } } }
type Scope = { header: { 'X-Learning-Space-ID': string } }
type Target = Scope & { path: { conversationID: string } }
export interface ConversationPaths {
  '/v1/learning/conversations': {
    get: Result<z.infer<typeof conversationsSchema>> & { parameters: Scope & { query?: { goal_id?: string; teaching_session_id?: string; all_contexts?: boolean; search?: string; cursor?: string; limit?: number } } }
    post: Result<{ id: string }, 201> & { parameters: Scope } & Body<{ id: string; goal_id?: string; teaching_session_id?: string; saved: boolean; title?: string }>
  }
  '/v1/learning/conversations/{conversationID}': {
    get: Result<z.infer<typeof turnsSchema>> & { parameters: Target & { query?: { after?: number; limit?: number } } }
    patch: Result<{ confirmed: true }> & { parameters: Target } & Body<{ expected_version: number; title: string }>
    delete: Result<{ confirmed: true }> & { parameters: Target } & Body<{ expected_version: number; confirmed: true }>
  }
  '/v1/learning/conversations/{conversationID}/turns': {
    post: Result<{ operation_id: string; run_id: string; session_id: string; version: number }, 202> & { parameters: Target } & Body<TurnSubmission>
  }
}
