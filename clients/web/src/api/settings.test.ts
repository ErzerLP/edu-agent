import { describe, expect, it } from 'vitest'
import { limitsSchema, settingsReason, settingsSchema } from './settings'

const defaults = {
  research_requests: 20,
  research_tokens: 100000,
  output_tokens: 2048,
  context_tokens: 32768,
  idle_timeout_seconds: 60,
  concurrency: 2,
  storage_mib: 512,
}
describe('设置限额与状态', () => {
  it('保留教学接口类型，兼容旧响应并拒绝未知接口', () => {
    const connection = {
      enabled: true,
      provider: 'openai_compatible',
      endpoint: 'https://model.example/v1',
      model: 'test',
      auth_mode: 'bearer',
      has_key: true,
      configured: true,
      source: 'server',
      status: 'unavailable',
      reason: 'not_probed',
      probe: null,
    }
    const schema = settingsSchema.shape.teaching
    expect(schema.parse({ ...connection, api_style: 'responses' }).api_style).toBe('responses')
    expect(schema.parse(connection).api_style).toBe('chat_completions')
    expect(schema.safeParse({ ...connection, api_style: 'unknown' }).success).toBe(false)
    expect(settingsReason('output_budget_exceeded')).toContain('输出 Token')
    expect(settingsReason('response_not_completed')).toContain('未完成')
  })
  it('校验默认值、类型、边界以及上下文关系', () => {
    expect(limitsSchema.safeParse(defaults).success).toBe(true)
    for (const field of Object.keys(defaults)) {
      for (const value of [0, -1, 1e10, '2', null, 2.5]) {
        expect(limitsSchema.safeParse({ ...defaults, [field]: value }).success).toBe(false)
      }
    }
    expect(limitsSchema.safeParse({ ...defaults, output_tokens: 32768 }).success).toBe(false)
    expect(limitsSchema.safeParse({ ...defaults, research_tokens: 1024 }).success).toBe(false)
  })
  it('区分不可用类别，不将未知类别误报就绪', () => {
    const codes = [
      'not_implemented',
      'not_enabled',
      'not_configured',
      'not_probed',
      'probe_expired',
      'unauthorized',
      'timeout',
      'unavailable',
      'invalid_response',
    ]
    expect(new Set(codes.map(settingsReason)).size).toBe(codes.length)
    expect(settingsReason('probe_failed:unauthorized')).toContain('鉴权失败')
    expect(settingsReason('future_unknown_reason')).toBe('当前能力不可用')
  })
})
