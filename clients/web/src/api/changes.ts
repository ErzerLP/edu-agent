import { z } from 'zod'
import { detailsSchema } from './runtime'

const version = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
export const changeBase = z.object({ goal_version: version, goal_revision_id: z.uuid(), session_version: version, route_revision_id: z.string(), context_id: z.string(), activity_id: z.string(), artifact_id: z.string(), artifact_version: version })
export const changeStep = z.object({ node_revision_id: z.uuid(), name: z.string(), criterion: z.string(), prompt: z.string(), difficulty: z.number().int().min(1).max(5), prerequisites: z.array(z.number().int().nonnegative()) })
const block = z.object({ block_id: z.uuid(), kind: z.string(), fallback: z.string(), text: z.string().optional() }).passthrough()
export const changeSchema = z.object({
  id: z.uuid(), learning_space_id: z.uuid(), goal_id: z.uuid(), session_id: z.uuid(), revision: version,
  hash: z.string().regex(/^[0-9a-f]{64}$/), interaction_id: z.uuid(),
  status: z.enum(['proposed', 'waiting_approval', 'approved', 'queued_for_boundary', 'applied', 'stale', 'rejected', 'cancelled', 'needs_sources']),
  risk: z.enum(['presentation', 'route', 'goal_scope']), policy: z.enum(['direct', 'boundary', 'approval']), status_reason: z.string(),
  base: changeBase,
  candidate: z.object({ kind: z.enum(['explanation', 'route', 'goal']), trigger: z.string(), reason: z.string(), evidence_ids: z.array(z.uuid()), context_id: z.string(), steps: z.array(changeStep), explanation: z.string(), goal: detailsSchema.optional() }),
  diff: z.object({ before_steps: z.array(changeStep), after_steps: z.array(changeStep), before_goal: detailsSchema, after_goal: detailsSchema, before_content: z.array(block), after_content: z.array(block) }),
  applied: changeBase.optional(), compensates: z.uuid().optional(), impact: z.string(), frame_id: z.string(), restored: z.boolean(), created_at: z.string(), privacy_generation: version,
})
export type LearningChange = z.infer<typeof changeSchema>
export const changesSchema = z.object({ items: z.array(changeSchema) })
export const adaptiveMode = z.object({ mode: z.enum(['adaptive', 'cautious']), version })
export const changeContext = z.object({ base: changeBase, steps: z.array(changeStep) })
export const changeCapabilities = z.object({ protocol_version: z.literal(1), available: z.boolean(), modes: z.array(z.string()) })
export const changeHeader = (space: string) => ({ 'X-Learning-Space-ID': space, 'X-Learning-Change-Version': '1' as const })
export const changeStatus: Record<LearningChange['status'], string> = { proposed: '已提出', waiting_approval: '等待审阅', approved: '已批准，尚未生效', queued_for_boundary: '已排队，本题处理后应用', applied: '已生效', stale: '版本已失效，差异只读', rejected: '已拒绝', cancelled: '已取消', needs_sources: '需要补充来源' }

// 独立变更协议的客户端合同；旧教学 DTO 不增加字段或状态枚举。
type Response<T> = { responses: { 200: { content: { 'application/json': T } } } }
type Parameters = { header: ReturnType<typeof changeHeader>; path: { goalID: string } }
type Command = { session_id: string; operation_id: string; action: 'approve' | 'apply_now' | 'reject' | 'cancel' | 'compensate' | 'restore_focus' | 'propose'; expected_revision: number; hash: string; interaction_id: string; immediate: false; base?: z.infer<typeof changeBase>; candidate?: LearningChange['candidate'] }
export interface ChangePaths {
  '/v1/learning/changes/capabilities': { get: Response<z.infer<typeof changeCapabilities>> }
  '/v1/learning/goals/{goalID}/changes': { get: Response<z.infer<typeof changesSchema>> & { parameters: Parameters } }
  '/v1/learning/goals/{goalID}/changes/{changeID}': { post: Response<LearningChange> & { parameters: { header: ReturnType<typeof changeHeader>; path: { goalID: string; changeID: string } }; requestBody: { content: { 'application/json': Command } } } }
  '/v1/learning/goals/{goalID}/change-context': { get: Response<z.infer<typeof changeContext>> & { parameters: Parameters & { query: { session_id: string } } } }
  '/v1/learning/goals/{goalID}/adaptive-mode': {
    get: Response<z.infer<typeof adaptiveMode>> & { parameters: Parameters }
    post: Response<z.infer<typeof adaptiveMode>> & { parameters: Parameters; requestBody: { content: { 'application/json': z.infer<typeof adaptiveMode> } } }
  }
}
