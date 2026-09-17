import { z } from 'zod'

const id = z.uuid()
const version = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
export const sessionSchema = z.object({
  device: z.object({ id, display_name: z.string(), scopes: z.array(z.string()).default([]) }),
  generation: version,
  expires_at: z.iso.datetime({ offset: true }),
  csrf_token: z.string().length(43),
  server_id: z.url(),
  capabilities: z.object({
    spaces: z.boolean(),
    goals: z.boolean(),
    save_goal: z.boolean(),
    start_learning: z.boolean(),
    references: z.boolean(),
    runs: z.boolean().default(false),
    import_jobs: z.boolean().default(false),
  }),
})
export type Session = z.infer<typeof sessionSchema>
export const spaceSchema = z.object({
  id,
  name: z.string(),
  description: z.string(),
  status: z.enum(['active', 'archived']),
  version,
  created_at: z.string(),
  updated_at: z.string(),
})
export type Space = z.infer<typeof spaceSchema>
export const detailsSchema = z.object({
  name: z.string().trim().min(1, '请填写目标名称').max(120),
  expected_outcome: z.string().max(4000).default(''),
  scope: z.string().max(4000).default(''),
  exclusions: z.string().max(4000).default(''),
  self_assessment: z.string().max(4000).default(''),
  purpose: z.string().max(4000).default(''),
  completion_criteria: z.string().max(4000).default(''),
  priority: z.enum(['', 'low', 'normal', 'high']).default('normal'),
  scope_snapshot_id: id.optional(),
  timezone: z.string().optional(),
  deadline: z.iso.datetime({ offset: true }).optional(),
  weekly_minutes: z.number().int().min(1).max(10080).optional(),
})
export const goalSchema = z.object({
  learning_space_id: id,
  goal_id: id,
  goal_revision_id: id,
  revision: version,
  text: z.string(),
  source: z.string(),
  created_at: z.string(),
  management: z.object({
    details: detailsSchema,
    status: z.enum(['draft', 'active', 'paused', 'completed', 'archived']),
    archived_from: z.string().optional(),
    completion: z
      .object({
        kind: z.literal('manual'),
        reason: z.string(),
        actor_device_id: id,
        at: z.string(),
      })
      .optional(),
    criteria_verification: z.string(),
    changed_fields: z
      .array(z.string())
      .nullable()
      .transform((v) => v ?? []),
    route_adjustment_needed: z.boolean(),
  }),
})
export type Goal = z.infer<typeof goalSchema>
export const pageOf = <T extends z.ZodType>(item: T) =>
  z.object({
    items: z
      .array(item)
      .nullable()
      .transform((v) => v ?? []),
    next_cursor: z.string().optional(),
  })
export const goalResult = z.object({ result: goalSchema, aggregate_version: version })
export const composerSchema = z.object({
  text: z.string().trim().min(1, '请写下想学会的内容').max(4000, '目标最多 4000 字'),
  details: detailsSchema,
})
export type GoalDraft = z.infer<typeof composerSchema>
export const spaceDraftSchema = z.object({
  name: z.string().trim().min(1, '请填写学习区名称').max(120),
  description: z.string().max(2000),
})
export const labels = {
  draft: '草稿',
  active: '进行中',
  paused: '已暂停',
  completed: '已完成',
  archived: '已归档',
}
