import { z } from 'zod'

export const limitsSchema = z
  .object({
    max_tool_steps: z.number().int().positive().nullable().default(null),
    output_tokens: z.number().int().min(64).max(32768),
    context_tokens: z.number().int().min(4096).max(2000000),
    idle_timeout_seconds: z.number().int().min(5).max(600),
    concurrency: z.number().int().min(1).max(16),
    storage_mib: z.number().int().min(16).max(1048576),
  })
  .refine(
    (v) => v.output_tokens < v.context_tokens,
    '单次输出须小于上下文',
  )
const probeSchema = z.object({
  status: z.enum(['ready', 'failed']),
  reason: z.string(),
  checked_at: z.iso.datetime({ offset: true }),
  valid_until: z.iso.datetime({ offset: true }),
  structured_json: z.boolean(),
  native_schema: z.boolean(),
  requests: z.number().int().min(0).max(2),
  cost_estimate: z.literal('unknown'),
})
const connectionSchema = z.object({
  enabled: z.boolean(),
  provider: z.enum(['openai_compatible', 'brave']),
  endpoint: z.string(),
  model: z.string(),
  auth_mode: z.enum(['bearer', 'none']),
  api_style: z.enum(['chat_completions', 'responses']).default('chat_completions'),
  has_key: z.boolean(),
  configured: z.boolean(),
  source: z.enum(['server', 'environment']),
  status: z.enum(['ready', 'unavailable', 'probe_failed']),
  reason: z.string(),
  probe: probeSchema.nullable(),
})
export const settingsSchema = z.object({
  revision: z.number().int().nonnegative(),
  writable: z.boolean(),
  teaching: connectionSchema,
  mentor: connectionSchema,
  effective_mentor: connectionSchema,
  search: connectionSchema,
  mentor_uses_teaching: z.boolean(),
  limits: limitsSchema,
  policy: z.object({ private_queries: z.boolean() }),
  teaching_restart_required: z.boolean(),
  model_endpoints: z.array(z.string()),
})
const capability = z.object({ available: z.boolean(), reason: z.string() })
export const capabilitiesSchema = z.object({
  schema_version: z.literal(1),
  checked_at: z.iso.datetime({ offset: true }),
  schema: capability,
  rendering: capability,
  answering: capability,
  search: capability,
  persistence: capability,
  research: capability,
  web_mentor: capability,
})
export type Settings = z.infer<typeof settingsSchema>
export type ConnectionView = z.infer<typeof connectionSchema>
export type Limits = z.infer<typeof limitsSchema>

export function settingsReason(reason: string) {
  const descriptions: Record<string, string> = {
    not_implemented: '此功能尚未实现',
    not_enabled: '已配置但未启用',
    not_configured: '缺少端点、模型名称或凭据',
    not_probed: '尚未测试连接',
    probe_expired: '上次测试已过期，请显式重新测试',
    unauthorized: '提供商鉴权失败，请替换凭据或检查权限',
    unavailable: '端点不可达或被网络策略拒绝',
    timeout: '连接测试超时',
    rate_limited: '提供商限流',
    upstream_error: '提供商服务错误',
    invalid_response: '提供商返回了无效响应',
    output_budget_exceeded: '达到单次输出 Token 上限，请调大单次输出上限后重新测试',
    response_not_completed: '提供商响应未完成，请检查提供商状态后重新测试',
    schema_mismatch: '提供商响应不符合所需 Schema',
    incompatible: '端点不兼容或拒绝重定向',
    invalid_request: '连接配置无效',
    redirect_rejected: '端点重定向已被拒绝',
  }
  const code = reason.replace(/^probe_failed:/, '')
  return descriptions[code] ?? (code ? '当前能力不可用' : '最近测试通过')
}
