import createClient from 'openapi-fetch'
import type { paths } from './schema'
import type { ChangePaths } from './changes'
import type { ReferencePaths } from './references'
import type { StructurePaths } from './structure'
import type { ConversationPaths } from './conversations'
import type { NotesyncPaths } from './notesync'
import type { MemoryPaths } from './memory'
import { z } from 'zod'
import { sessionSchema, type Session } from './runtime'

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    public requestId?: string,
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
    const parsed = z.object({ error: z.object({ code: z.string(), request_id: z.string().regex(/^[A-Za-z0-9_./:-]{1,200}$/).optional() }) }).safeParse(error)
    throw new ApiError(response.status, parsed.success ? parsed.data.error.code : 'request_failed', parsed.success ? parsed.data.error.request_id : undefined)
  }
  const parsed = schema.safeParse(data)
  if (!parsed.success) throw new ApiError(502, 'invalid_response')
  return parsed.data
}
export const readSession = () => unwrap(publicClient.GET('/v1/web/session'), sessionSchema)

export function learningClient(session: Session, spaceId?: string) {
  if (session.server_id !== window.location.origin) throw new ApiError(502, 'invalid_response')
  const client = createClient<Omit<paths, keyof ChangePaths | keyof ReferencePaths | keyof StructurePaths | keyof ConversationPaths | keyof NotesyncPaths | keyof MemoryPaths> & ChangePaths & ReferencePaths & StructurePaths & ConversationPaths & NotesyncPaths & MemoryPaths>({
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
  const tutorErrors: Record<string, string> = {
    run_storage_limit: '导师历史或上下文已达上限，未删除旧历史。可清理不需要的对话或新建对话。',
    run_storage_unavailable: '加密历史不可读取或保存，请检查服务器密钥及存储。未回退明文或空历史。',
    tutor_history_schema_unsupported: '历史格式来自更高版本，请升级服务。未重写或清空历史。',
    tutor_destination_confirmation_required: '模型目的地已变化，请核对目的地和历史范围后明确确认；尚未发送。',
    tutor_temporary_unavailable: '临时正文已不可恢复，请新建对话；正式学习事实仍保留。',
  }
  if (tutorErrors[error.code]) return tutorErrors[error.code]
  if (error.code.startsWith('pdf_')) {
    const messages: Record<string, string> = { pdf_encrypted: 'PDF 已加密，不接受密码或绕过访问限制。', pdf_file_limit: 'PDF 超过 4 MiB。', pdf_page_limit: 'PDF 超过 100 页。', pdf_timeout: 'PDF 解析达到时间限制，已中止。', pdf_resource_limit: 'PDF 解析达到内存或资源限制。', pdf_partial_confirmation_required: '请核对逐页报告并明确选择仅采用可用部分。', pdf_no_usable_text: 'PDF 没有可用文本层，未运行 OCR。' }
    return messages[error.code] ?? 'PDF 损坏、格式不兼容或超过解析资源限制；未将失败内容算作有效资料。'
  }
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
