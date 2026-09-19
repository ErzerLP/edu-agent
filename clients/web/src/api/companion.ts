import { z } from 'zod'
import { ApiError, loseIdentity, readSession, sameIdentity } from './client'
import type { Session } from './runtime'

export const pairSchema = z.object({ id: z.string(), code: z.string(), secret: z.string() })
export type LocalPair = z.infer<typeof pairSchema>
export const receiptSchema = z.object({ id: z.string(), device: z.string(), conversation: z.string(), run: z.string(), task: z.string().optional(), task_run: z.string().optional(), state: z.string(), value: z.unknown().optional(), error: z.string().optional() })
export const grantSchema = z.object({ generation: z.number(), space: z.string(), conversation: z.string(), files: z.boolean(), shell: z.boolean(), model: z.boolean(), destination: z.string() })
export const localViewSchema = z.object({
  id: z.string(), state: z.string(), device: z.object({ id: z.string(), host: z.string(), user: z.string(), os: z.string(), workspace: z.string() }),
  grant: grantSchema.optional(), receipts: z.array(receiptSchema),
})
export type LocalView = z.infer<typeof localViewSchema>
export type LocalReceipt = z.infer<typeof receiptSchema>

export async function localRequest<T>(session: Session, body: object, schema: z.ZodType<T>, signal?: AbortSignal): Promise<T> {
  const current = await readSession()
  if (!sameIdentity(current, session)) { loseIdentity(session); throw new ApiError(401, 'identity_changed') }
  const response = await fetch('/v1/companion/browser', {
    method: 'POST', credentials: 'same-origin', cache: 'no-store', signal,
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': session.csrf_token, 'X-Web-Principal-ID': session.device.id, 'X-Web-Generation': String(session.generation) },
    body: JSON.stringify(body),
  })
  if (!response.ok) throw new ApiError(response.status, 'companion_unavailable')
  return schema.parse(await response.json())
}

// 页面卸载的撤销是尽力通知；真正的退出边界是两端 45 秒租约。
export function revokeLocal(session: Session, pair: LocalPair) {
  return fetch('/v1/companion/browser', { method: 'POST', credentials: 'same-origin', cache: 'no-store', keepalive: true,
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': session.csrf_token, 'X-Web-Principal-ID': session.device.id, 'X-Web-Generation': String(session.generation) },
    body: JSON.stringify({ action: 'revoke', id: pair.id, secret: pair.secret }),
  })
}

export const localState: Record<string, string> = { pairing: '等待本机配对', awaiting_authorization: '等待浏览器授权', connected: '已连接', disconnected: '连接中断，任务结果可能未知', revoked: '已撤销，等待本机收尾', queued: '等待设备领取', dispatched: '已交给设备，等待回执', unknown: '结果未知，请查询原任务，勿重新执行', completed: '已收到设备回执（不代表命令成功）', not_executed: '尚未执行，授权已撤销' }

export function taskSnapshots(receipts: LocalReceipt[]) {
  const schema = z.object({ TaskID: z.string(), State: z.string(), Controllable: z.boolean(), PTY: z.boolean() }).passthrough()
  const tasks = new Map<string, z.infer<typeof schema>>()
  for (const receipt of receipts) {
    const list = z.object({ items: z.array(z.unknown()) }).safeParse(receipt.value)
    for (const value of list.success ? list.data.items : [receipt.value]) {
      const parsed = schema.safeParse(value)
      if (parsed.success) tasks.set(parsed.data.TaskID, parsed.data)
    }
  }
  return [...tasks.values()]
}
