import { z } from 'zod'
import { ApiError, learningClient } from './client'
import type { Session } from './runtime'
import { researchStateSchema } from './research'
import { startStateSchema } from './start'
import { editStateSchema } from './content'

const positive = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
export const mentorSnapshotSchema = z.object({
  teaching_session_id: z.uuid().optional(),
  kind: z.enum(['mentor', 'research', 'start_learning', 'content_edit']).optional(), research: researchStateSchema.optional(),
  content_edit: editStateSchema.optional(),
  start_learning: startStateSchema.optional(),
  run_id: z.uuid(), session_id: z.uuid(), space_id: z.uuid(), goal_id: z.uuid(),
  goal_version: positive, privacy_generation: positive, version: positive, watermark: positive,
  status: z.enum(['queued', 'running', 'waiting_input', 'waiting_approval', 'paused_budget', 'succeeded', 'partial', 'failed', 'cancelling', 'cancelled']),
  stage: z.string(), reason: z.string(), saved: z.boolean(), body_available: z.boolean(),
  requests_left: z.number().int().nonnegative(), tokens_left: z.number().int().nonnegative(),
  requests_used: z.number().int().nonnegative(), result_unknown: z.boolean(), cost_unknown: z.boolean(),
  updated_at: z.iso.datetime({ offset: true }), expires_at: z.iso.datetime({ offset: true }),
  configuration: z.string(), output: z.string().max(65536),
  interaction: z.object({ id: z.uuid(), question: z.string(), choices: z.array(z.string()).max(8), approval: z.boolean(), call_id: z.string(), reference_selection: z.boolean().optional() }).optional(),
})
export type MentorSnapshot = z.infer<typeof mentorSnapshotSchema>
export const mentorCurrentSchema = z.object({ run: mentorSnapshotSchema.nullable(), save_available: z.boolean() })
export const mentorReceiptSchema = z.object({ operation_id: z.uuid(), run_id: z.uuid(), session_id: z.uuid(), version: positive })
export const mentorEventSchema = z.object({ run_id: z.uuid(), session_id: z.uuid(), space_id: z.uuid(), goal_id: z.uuid(), privacy_generation: positive, version: positive, seq: positive, type: z.string() })
type MentorEvent = z.infer<typeof mentorEventSchema>

export function eventAction(snapshot: MentorSnapshot, event: MentorEvent): 'ignore' | 'refresh' | 'resync' {
  if (snapshot.run_id !== event.run_id || snapshot.space_id !== event.space_id || snapshot.goal_id !== event.goal_id || snapshot.session_id !== event.session_id || snapshot.privacy_generation !== event.privacy_generation) throw new ApiError(502, 'invalid_response')
  if (event.seq <= snapshot.watermark) return 'ignore'
  return event.seq === snapshot.watermark + 1 && event.version > snapshot.version ? 'refresh' : 'resync'
}

type Observer = (snapshot: MentorSnapshot, error?: unknown) => void
type Subscription = { snapshot: MentorSnapshot; observers: Set<Observer>; controller: AbortController }
const subscriptions = new Map<string, Subscription>()

// 同一身份和运行共享一个连接；最后一个面板卸载立即释放连接和正文缓存。
export function observeMentor(session: Session, initial: MentorSnapshot, observer: Observer) {
  const key = JSON.stringify([session.server_id, session.device.id, session.generation, initial.space_id, initial.goal_id, initial.run_id])
  let subscription = subscriptions.get(key)
  if (!subscription) {
    subscription = { snapshot: initial, observers: new Set(), controller: new AbortController() }
    subscriptions.set(key, subscription)
  }
  subscription.observers.add(observer)
  observer(subscription.snapshot)
  if (subscription.observers.size === 1) void watch(session, subscription)
  return () => {
    subscription.observers.delete(observer)
    if (subscription.observers.size === 0) {
      subscription.controller.abort()
      subscriptions.delete(key)
    }
  }
}

async function watch(session: Session, sub: Subscription) {
  const signal = sub.controller.signal
  const header = { 'X-Learning-Space-ID': sub.snapshot.space_id }
  const path = { runID: sub.snapshot.run_id }
  const client = learningClient(session, sub.snapshot.space_id)
  let backoff = 500
  const report = (error?: unknown) => { if (!signal.aborted) sub.observers.forEach((observer) => observer(sub.snapshot, error)) }
  const refresh = async () => {
    const result = await client.GET('/v1/learning/runs/{runID}', { params: { path, header }, signal })
    if (!result.response.ok) throw new ApiError(result.response.status, 'run_unavailable')
    const snapshot = mentorSnapshotSchema.parse(result.data)
    if (snapshot.run_id !== sub.snapshot.run_id || snapshot.goal_id !== sub.snapshot.goal_id || snapshot.space_id !== sub.snapshot.space_id || snapshot.privacy_generation !== session.generation) throw new ApiError(401, 'identity_changed')
    if (snapshot.watermark >= sub.snapshot.watermark) sub.snapshot = snapshot
    report()
  }
  while (!signal.aborted) {
    try {
      await refresh()
      const result = await client.GET('/v1/learning/runs/{runID}/events', { params: { path, header, query: { after: sub.snapshot.watermark } }, parseAs: 'stream', signal })
      if (!result.response.ok || !result.data) throw new ApiError(result.response.status, 'stream_unavailable')
      const reader = result.data.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      try {
        while (!signal.aborted) {
          const chunk = await reader.read()
          if (chunk.done) break
          buffer += decoder.decode(chunk.value, { stream: true }).replace(/\r/g, '')
          if (buffer.length > 131072) throw new ApiError(502, 'invalid_response')
          let end: number
          while ((end = buffer.indexOf('\n\n')) >= 0) {
            const frame = buffer.slice(0, end)
            buffer = buffer.slice(end + 2)
            const data = frame.split('\n').find((line) => line.startsWith('data: '))
            if (!data) continue
            const event = mentorEventSchema.parse(JSON.parse(data.slice(6)))
            if (eventAction(sub.snapshot, event) !== 'ignore') await refresh()
          }
          backoff = 500
        }
      } finally { await reader.cancel().catch(() => {}); reader.releaseLock() }
    } catch (error) {
      if (signal.aborted) return
      if (error instanceof ApiError && [401, 403, 404, 503].includes(error.status)) {
        // 身份失效或隐私门禁关闭后立即隐藏旧正文，不能依赖下一次成功查询。
        sub.snapshot = { ...sub.snapshot, output: '', interaction: undefined, research: undefined, start_learning: undefined, content_edit: undefined, body_available: false }
        report(error)
        return
      }
      report(error)
    }
    await new Promise<void>((resolve) => {
      const end = () => { clearTimeout(timer); signal.removeEventListener('abort', end); resolve() }
      const timer = setTimeout(end, backoff)
      signal.addEventListener('abort', end, { once: true })
    })
    backoff = Math.min(backoff * 2, 10000)
  }
}

export const mentorStatus: Record<MentorSnapshot['status'], string> = {
  queued: '排队中', running: '导师正在回答', waiting_input: '等待你的回答', waiting_approval: '等待你确认',
  paused_budget: '预算已用完，等待决定', succeeded: '已完成', partial: '仅保留部分结果', failed: '运行失败', cancelling: '正在停止', cancelled: '已停止',
}
export const isMentorTerminal = (status: MentorSnapshot['status']) => ['succeeded', 'partial', 'failed', 'cancelled'].includes(status)
