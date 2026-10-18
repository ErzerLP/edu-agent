import { z } from 'zod'

const counter = z.number().int().nonnegative()
const metric = z.object({ known_sum: counter, unknown_calls: counter })
export const usageTotals = z.object({
  calls: counter,
  pending_calls: counter,
  outcome_unknown_calls: counter,
  input: metric,
  output: metric,
  reasoning: metric,
  total: metric,
})
const model = z.object({
  provider: z.string(),
  model: z.string(),
  api_style: z.string(),
  destination: z.string(),
})
export const usageSummary = z.object({
  privacy_generation: counter,
  coverage_since: z.string(),
  full_coverage_since: z.string().nullable(),
  coverage: z.enum(['mentor', 'server']),
  from: z.string(),
  to: z.string(),
  timezone: z.string(),
  grain: z.enum(['day', 'week', 'month']),
  totals: usageTotals,
  buckets: z.array(usageTotals.extend({ start: z.string() })),
  models: z.array(model),
})
export const usageTask = z.object({
  id: z.string().uuid(),
  space_id: z.string().uuid().nullable(),
  kind: z.string(),
  source: z.enum(['web', 'cli', 'server']),
  started_at: z.string(),
  run_id: z.string().uuid().nullable(),
  conversation_id: z.string().uuid().nullable().optional(),
  totals: usageTotals,
})
export const usageTasks = z.object({
  items: z.array(usageTask),
  next_cursor: z.string().optional(),
  privacy_generation: counter,
})
export const usageCall = model.extend({
  id: z.string().uuid(),
  stage: z.string(),
  attempt_no: counter,
  state: z.enum(['dispatching', 'received', 'failed', 'cancelled', 'outcome_unknown']),
  started_at: z.string(),
  finished_at: z.string().nullable(),
  input_tokens: counter.nullable(),
  output_tokens: counter.nullable(),
  reasoning_tokens: counter.nullable(),
  total_tokens: counter.nullable(),
  provider_total_tokens: counter.nullable(),
  usage_inconsistent: z.boolean(),
})
export const usageCalls = z.object({
  items: z.array(usageCall),
  next_cursor: z.string().optional(),
  privacy_generation: counter,
})
export type UsageTotals = z.infer<typeof usageTotals>
export type UsageTask = z.infer<typeof usageTask>
export type UsageFilter = {
  from: string
  to: string
  timezone: string
  grain: 'day' | 'week' | 'month'
  model?: string
  space_id?: string
}
