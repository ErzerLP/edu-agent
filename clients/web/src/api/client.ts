import createClient from 'openapi-fetch'
import type { paths } from './schema'
import { z } from 'zod'
import { sessionSchema, type Session } from './runtime'

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
  ) {
    super(code)
  }
}
export function identityKey(s: Session) {
  return [s.server_id, s.device.id, s.generation] as const
}
export function sameIdentity(a: Session, b: Session) {
  return JSON.stringify(identityKey(a)) === JSON.stringify(identityKey(b))
}
export function loseIdentity(session: Session) {
  window.dispatchEvent(
    new CustomEvent('web-identity-lost', { detail: JSON.stringify(identityKey(session)) }),
  )
}

// OpenAPI 生成路径约束真实请求；输入与响应仍由 Zod 独立校验。
export const publicClient = createClient<paths>({
  baseUrl: window.location.origin,
  credentials: 'same-origin',
  cache: 'no-store',
})
export async function unwrap<T>(
  promise: Promise<{ data?: unknown; error?: unknown; response: Response }>,
  schema: z.ZodType<T>,
): Promise<T> {
  const { data, response, error } = await promise
  if (!response.ok) {
    const parsed = z.object({ error: z.object({ code: z.string() }) }).safeParse(error)
    throw new ApiError(response.status, parsed.success ? parsed.data.error.code : 'request_failed')
  }
  const parsed = schema.safeParse(data)
  if (!parsed.success) throw new ApiError(502, 'invalid_response')
  return parsed.data
}
export const readSession = () => unwrap(publicClient.GET('/v1/web/session'), sessionSchema)

export function learningClient(session: Session, spaceId?: string) {
  if (session.server_id !== window.location.origin) throw new ApiError(502, 'invalid_response')
  const client = createClient<paths>({
    baseUrl: session.server_id,
    credentials: 'same-origin',
    cache: 'no-store',
    headers: {
      'X-CSRF-Token': session.csrf_token,
      'X-Web-Principal-ID': session.device.id,
      'X-Web-Generation': String(session.generation),
      ...(spaceId ? { 'X-Learning-Space-ID': spaceId } : {}),
    },
  })
  client.use({
    async onRequest({ request }) {
      let current: Session
      try {
        current = await readSession()
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) loseIdentity(session)
        throw error
      }
      if (!sameIdentity(current, session)) {
        loseIdentity(session)
        throw new ApiError(401, 'identity_changed')
      }
      return request
    },
    onResponse({ response }) {
      if (response.status === 401) loseIdentity(session)
      return response
    },
  })
  return client
}

export function errorText(error: unknown) {
  if (!(error instanceof ApiError)) return '网络连接失败，输入已保留。请检查连接后重试。'
  if (error.code === 'learning_content_key_unavailable')
    return '版本化内容需要服务器配置独立正文加密密钥。原会话仍然保留。'
  if (error.code === 'learning_content_upgrade_required')
    return '当前内容或作答协议不受支持，请升级客户端；未提交答案。'
  if (error.code === 'retrieval_incomplete')
    return '本次资料检索不完整，未据此生成活动。请检查资料范围后重试。'
  if (error.code === 'knowledge_scope_required')
    return '此旧会话尚未绑定教学资料。可返回目标，授权“新开学习现场”自动获取资料，或在 CLI 中选择已有资料。'
  if (error.status === 409)
    return '内容版本已变化或当前状态不允许操作。输入已保留；请读取最新版本后检查并重试。'
  if (error.status === 401) return '浏览器会话已失效，请重新配对。'
  if (error.status === 403) return '当前设备没有此权限，或请求来源校验失败。'
  if (error.status === 404) return '该内容不存在或不属于当前学习区。'
  if (error.status === 501) return '当前服务器尚未支持此能力。'
  if (error.status === 429) return '请求过于频繁，请稍后重试。'
  if (error.code === 'invalid_response') return '服务器响应格式不受支持，未使用不完整数据。'
  return '请求未完成，输入已保留。请稍后重试。'
}
