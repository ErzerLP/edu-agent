import { z } from 'zod'
import { goalSchema } from './runtime'

const id = z.uuid()
const count = z.number().int().nonnegative()
export const progressSearch = z.object({
  space: id.optional(),
  goal: id.optional(),
  status: z.enum(['active', 'draft', 'paused', 'completed', 'archived', 'all']).catch('active'),
  order: z.enum(['priority', 'recent']).catch('priority'),
  due: z.iso.datetime({ offset: true }).optional(),
  view: z.enum(['goals', 'reviews']).catch('goals'),
})
export type ProgressSearch = z.infer<typeof progressSearch>
export const projectionSchema = z.object({
  generation: id,
  as_of_event_seq: count,
  rebuilding: z.boolean(),
  degraded: z.boolean(),
  incomplete: z.boolean(),
  reason_codes: z.array(z.string()),
})
export const reviewSchema = z.object({
  task_id: id,
  learning_space_id: id,
  goal_id: id,
  goal_revision_id: id,
  knowledge_revision_id: id.optional(),
  route_revision_id: id.optional(),
  session_id: id.optional(),
  carrier_session_id: id.optional(),
  evidence_id: id,
  attempt_id: id.optional(),
  node_revision_id: id,
  goal_name: z.string().optional(),
  space_name: z.string().optional(),
  due_at: z.string(),
  startable: z.boolean().optional(),
  unavailable_reason: z.string().optional(),
})
export type Review = z.infer<typeof reviewSchema>
export const goalProgressSchema = z.object({
  learning_space_id: id,
  space_name: z.string(),
  goal: goalSchema,
  routes: z.array(z.object({
    route: z.object({ route_revision_id: id, route_id: id, revision: count, created_at: z.string(), steps: z.array(z.object({ route_step_id: id, node_revision_id: id, teaching_intent: z.string(), completion_condition: z.string() })) }),
    numerator: count,
    denominator: count,
    percent: z.number().nullable(),
    completed_steps: z.array(id),
    previous_revision_id: id.optional(),
    added_steps: z.array(id),
    removed_steps: z.array(id),
  })),
  sessions: z.array(z.object({ session_id: id, learning_space_id: id, goal_id: id, goal_revision_id: id, name: z.string(), state: z.string(), position: z.string(), resumable: z.boolean(), activity_id: id.optional(), route_revision_id: id.optional(), route_step_id: id.optional() })),
  nodes: z.array(z.object({ mastery: z.object({ node_revision_id: id, state: z.string(), valid_evidence_count: count, pending_assessments: count, uncertainty_reasons: z.array(z.string()) }) })),
  reviews: z.array(reviewSchema),
  pending_assessments: z.array(z.object({ assessment_id: id, node_revision_id: id, reasons: z.array(z.string()) })),
  recent_activity: z.array(z.object({ event_id: id, event_type: z.string(), aggregate_id: id, parent_session_id: id.optional(), received_at: z.string() })),
  recent_has_more: z.boolean(),
  evidence_count: count,
  evidence_sources: z.array(z.object({ evidence_id: id, attempt_id: id.optional(), goal_revision_id: id, knowledge_revision_id: id, current_goal_revision: z.boolean() })),
  estimated_active_seconds: count,
  estimated: z.literal(true),
})
export type GoalProgress = z.infer<typeof goalProgressSchema>
export const progressPage = z.object({ data_cleared: z.boolean().default(false), metadata: projectionSchema, updated_at: z.string(), committed_event_high_water: count, items: z.array(goalProgressSchema), total: count, next_cursor: z.string().optional() })
export const reviewsPage = z.object({ data_cleared: z.boolean().default(false), metadata: projectionSchema, updated_at: z.string(), due_before: z.string(), items: z.array(reviewSchema), total: count, next_cursor: z.string().optional() })
export const reviewReasons: Record<string, string> = {
  original_session_not_at_review_node: '原会话已完成、正在作答或已离开原节点。可查看原记录，或明确创建复习承载。',
  goal_or_space_not_active: '目标已暂停、完成或归档，或学习区已归档。任务与到期日期仍保留。',
  not_due: '尚未到期，保留原调度日期。',
}
export function progressQuery(search: ProgressSearch, cursor: string, limit: number) {
  return { global: !search.space, goal_id: search.goal, status: search.status, cursor, limit }
}
