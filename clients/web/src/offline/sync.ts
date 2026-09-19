import { z } from 'zod'
import { readSession } from '@/api/client'
import { original, parseSigned } from './crypto'
import { verifyPack, prepareIntent, type Owner, type Selection, type SavedPack } from './protocol'
import { destroy, exclusive, type QueueEntry, type Vault } from './vault'

export class OfflineError extends Error {
  constructor(
    public status: number,
    public code: string,
  ) {
    super(`离线请求未完成：${code}（${status}）`)
  }
}
const purgeSchema = z.object({
  erasure_id: z.uuid(),
  device_id: z.uuid(),
  old_generation: z.number().int().positive(),
  current_generation: z.number().int().positive(),
  challenge_revision: z.number().int().positive(),
  challenge: z.string().length(43),
  status: z.enum(['pending', 'succeeded', 'unknown', 'failed']),
})
const sessionSchema = z.object({
  adapter_version: z.literal(1),
  device_id: z.uuid(),
  generation: z.string(),
  expires_at: z.string(),
  server_time: z.string(),
  csrf_token: z.string(),
  content_allowed: z.boolean(),
  purge: purgeSchema.nullable(),
  purge_receipt: z
    .object({
      erasure_id: z.uuid(),
      device_id: z.uuid(),
      source_generation: z.number().int().positive(),
      status: z.literal('succeeded'),
    })
    .nullable(),
})
export type OfflineSession = z.infer<typeof sessionSchema>
export async function call(
  path: string,
  options: {
    owner?: Pick<Owner, 'deviceId' | 'generation'>
    csrf?: string
    method?: string
    body?: string
    space?: string
  } = {},
): Promise<string> {
  const response = await fetch(`/v1/web/offline/${path}`, {
    method: options.method ?? 'GET',
    credentials: 'same-origin',
    cache: 'no-store',
    redirect: 'error',
    signal: AbortSignal.timeout(30_000),
    headers: {
      'X-Offline-Adapter': '1',
      ...(options.body ? { 'Content-Type': 'application/json' } : {}),
      ...(options.csrf ? { 'X-CSRF-Token': options.csrf } : {}),
      ...(options.owner
        ? {
            'X-Web-Principal-ID': options.owner.deviceId,
            'X-Web-Generation': options.owner.generation,
          }
        : {}),
      ...(options.space ? { 'X-Learning-Space-ID': options.space } : {}),
    },
    body: options.body,
  })
  let size = 0
  const reader = response.body?.getReader()
  const chunks: Uint8Array[] = []
  if (reader)
    while (true) {
      const next = await reader.read()
      if (next.done) break
      size += next.value.length
      if (size > 8 * 1024 * 1024) {
        await reader.cancel()
        throw new Error('服务器离线响应超过 8 MiB，停止保存')
      }
      chunks.push(next.value)
    }
  const buffer = new Uint8Array(size)
  let offset = 0
  for (const chunk of chunks) {
    buffer.set(chunk, offset)
    offset += chunk.length
  }
  const text = new TextDecoder('utf-8', { fatal: true }).decode(buffer)
  if (!response.ok) {
    const error = z
      .object({ error: z.object({ code: z.string().regex(/^[a-z0-9_]+$/) }) })
      .safeParse(JSON.parse(text || '{}'))
    throw new OfflineError(
      response.status,
      error.success ? error.data.error.code : 'invalid_response',
    )
  }
  return text
}
export async function enroll(): Promise<Owner> {
  const online = await readSession()
  const raw = await call('enable', {
    method: 'POST',
    csrf: online.csrf_token,
    body: JSON.stringify({ adapter_version: 1, save_consent: true }),
  })
  const value = parseSigned(raw)
  const schema = z.object({
    adapter_version: z.literal(1),
    device_id: z.uuid(),
    generation: z.string(),
    expires_at: z.string(),
    bootstrap: z.object({
      protocol_version: z.literal(1),
      learner_generation: z.string(),
      server_base_url: z.string(),
      signer_manifest: z.object({}).passthrough(),
    }),
  })
  if (!schema.safeParse(value).success) throw new Error('离线身份协商失败')
  const data = value as z.infer<typeof schema>
  if (
    data.device_id !== online.device.id ||
    data.generation !== String(online.generation) ||
    data.generation !== data.bootstrap.learner_generation ||
    data.bootstrap.server_base_url !== `${location.origin}/`
  )
    throw new Error('离线身份、代次或服务器不匹配')
  return {
    deviceId: data.device_id,
    generation: data.generation,
    expiresAt: data.expires_at,
    origin: data.bootstrap.server_base_url,
    root: original(data.bootstrap.signer_manifest),
  }
}
export async function readOfflineSession(
  owner?: Pick<Owner, 'deviceId' | 'generation'>,
): Promise<OfflineSession> {
  return sessionSchema.parse(JSON.parse(await call('session', { owner })))
}
const binding = (session: OfflineSession) => ({
  deviceId: session.device_id,
  generation: session.generation,
})
export async function purgeIfRequired(session: OfflineSession, vault?: Vault): Promise<boolean> {
  if (!session.purge && session.content_allowed) return false
  {
    vault?.lock()
    const purge = session.purge
    if (purge && purge.device_id !== session.device_id) throw new Error('清除任务设备不匹配')
    const ack = async (outcome: 'succeeded' | 'failed') => {
      if (!purge) return
      const text = await call(`purge/${purge.erasure_id}/ack`, {
        owner: binding(session),
        csrf: session.csrf_token,
        method: 'POST',
        body: JSON.stringify({
          challenge_revision: purge.challenge_revision,
          challenge: purge.challenge,
          outcome,
          ...(outcome === 'succeeded'
            ? { managed_objects_absent: true }
            : { failure_code: 'verification_failed' }),
        }),
      })
      const receipt = z
        .object({
          status: z.literal(outcome),
          device_id: z.literal(session.device_id),
          erasure_id: z.literal(purge.erasure_id),
        })
        .safeParse(JSON.parse(text))
      if (!receipt.success) throw new Error('清除回执尚未核对成功，请联网重试')
    }
    try {
      await destroy(vault)
    } catch (error) {
      await ack('failed').catch(() => {})
      throw error
    }
    if (!purge) {
      const receipt = session.purge_receipt
      if (
        receipt?.device_id === session.device_id &&
        String(receipt.source_generation) === session.generation
      )
        return true
      throw new Error('旧代次本地库已清除；服务器尚未提供可核对的清除任务或回执，请稍后重试')
    }
    await ack('succeeded')
  }
  return true
}
export async function download(vault: Vault, selection?: Selection): Promise<void> {
  const owner = (await vault.read()).owner
  const session = await readOfflineSession(owner)
  if (await purgeIfRequired(session, vault)) throw new Error('旧代次离线库已清除，请重新配对')
  await exclusive(async () => {
    let state = await vault.read()
    if (!state.pending) {
      if (!selection) throw new Error('请选择原目标的教学会话')
      const intent = await prepareIntent(owner, selection, state.trust)
      state = await vault.updateLocked((s) => {
        s.pending = intent
      })
    }
    const intent = state.pending!
    const response = await call('packs', {
      owner,
      csrf: session.csrf_token,
      method: 'POST',
      body: intent.request,
      space: intent.selection.spaceId,
    })
    const saved: SavedPack = { response, intent }
    const verified = await verifyPack(saved, owner)
    const renewed = await readOfflineSession(owner)
    if (!renewed.content_allowed || renewed.purge)
      throw new OfflineError(409, 'offline_purge_required')
    await vault.updateLocked((s) => {
      if (
        !s.packs.some(
          (p) =>
            JSON.parse(p.response).pack.payload.pack_id === verified.response.pack.payload.pack_id,
        )
      )
        s.packs.push(saved)
      s.trust = verified.trust
      s.pending = undefined
      s.serverTime = Math.max(s.serverTime, Date.parse(session.server_time))
      s.owner.expiresAt = renewed.expires_at
    })
  })
}
const receiptSchema = z.object({
  receipt_id: z.uuid(),
  archived_at: z.string(),
  aggregate_version: z.string(),
  first_event_seq: z.string(),
  last_event_seq: z.string(),
  projection_as_of_event_seq: z.string(),
  archive_status: z.enum(['archived_succeeded', 'archived_rejected']),
})
const resultSchema = z.object({
  operation_id: z.uuid(),
  submission_id: z.uuid(),
  archive_status: z.enum([
    'archived_succeeded',
    'archived_rejected',
    'not_archived_retryable',
    'not_archived_blocked',
    'idempotency_conflict',
    'device_sequence_conflict',
    'not_processed',
  ]),
  assessment_status: z
    .enum(['not_requested', 'queued', 'processing', 'pending_retry', 'completed', 'failed'])
    .optional(),
  evidence_status: z
    .enum([
      'accepted',
      'provisional',
      'pending_evaluation',
      'not_eligible',
      'not_applicable',
      'unchanged',
    ])
    .optional(),
  reason_codes: z.array(z.string()),
  ingest_receipt: receiptSchema.optional(),
  device_seq: z.string().optional(),
})
export type Result = z.infer<typeof resultSchema>
export function acceptResult(entry: QueueEntry, value: unknown): QueueEntry {
  const result = resultSchema.parse(value)
  const operation = JSON.parse(entry.operation)
  if (
    result.operation_id !== operation.operation_id ||
    result.submission_id !== operation.submission_id ||
    (result.device_seq !== undefined && result.device_seq !== operation.device_seq)
  )
    throw new Error('同步回执不属于原操作，保持结果未知')
  let state: QueueEntry['state']
  if (result.archive_status.startsWith('archived_')) {
    const allowed =
      result.archive_status === 'archived_rejected'
        ? result.assessment_status === 'not_requested' && result.evidence_status === 'unchanged'
        : (
            {
              not_requested: ['provisional', 'not_eligible', 'not_applicable'],
              queued: ['pending_evaluation'],
              processing: ['pending_evaluation'],
              pending_retry: ['pending_evaluation'],
              completed: ['accepted', 'provisional', 'not_eligible'],
              failed: ['unchanged'],
            }[result.assessment_status ?? 'not_requested'] ?? []
          ).includes(result.evidence_status ?? '')
    if (!allowed) throw new Error('存档、评估和证据状态组合不合法，保持结果未知')
    const receipt = result.ingest_receipt
    const positive = (v: string) => /^[1-9][0-9]*$/.test(v) && BigInt(v) <= 9223372036854775807n
    if (
      !receipt ||
      receipt.archive_status !== result.archive_status ||
      !result.assessment_status ||
      !result.evidence_status ||
      ![
        receipt.aggregate_version,
        receipt.first_event_seq,
        receipt.last_event_seq,
        receipt.projection_as_of_event_seq,
      ].every(positive) ||
      BigInt(receipt.last_event_seq) < BigInt(receipt.first_event_seq) ||
      BigInt(receipt.projection_as_of_event_seq) < BigInt(receipt.last_event_seq)
    )
      throw new Error('服务器未返回有效持久回执，保持结果未知')
    state = result.archive_status === 'archived_succeeded' ? 'confirmed' : 'rejected'
  } else {
    if (result.ingest_receipt || result.evidence_status || result.assessment_status)
      throw new Error('未存档结果错误地携带正式证据')
    state = result.archive_status.includes('conflict')
      ? 'conflict'
      : result.archive_status === 'not_archived_blocked'
        ? 'blocked'
        : 'queued'
  }
  return { operation: entry.operation, state, result: JSON.stringify(result) }
}
export async function synchronize(vault: Vault): Promise<void> {
  const owner = (await vault.read()).owner
  const session = await readOfflineSession(owner)
  if (await purgeIfRequired(session, vault)) throw new Error('旧代次离线库已清除，清除回执已提交')
  await exclusive(async () => {
    const lookup = async (entry: QueueEntry) => {
      const operation = JSON.parse(entry.operation)
      try {
        const result = JSON.parse(await call(`operations/${operation.operation_id}`, { owner }))
        await vault.updateLocked((s) => {
          const index = s.queue.findIndex((q) => q.operation === entry.operation)
          s.queue[index] = acceptResult(entry, result)
        })
      } catch (error) {
        if (error instanceof OfflineError && error.status === 404 && entry.state === 'unknown') {
          await vault.updateLocked((s) => {
            s.queue.find((q) => q.operation === entry.operation)!.state = 'queued'
          })
        } else throw error
      }
    }
    // 全部未知结果必须先核对；任何查询不确定时都不发送新记录。
    const initial = (await vault.read()).queue
    for (const entry of initial.filter((q) => q.state === 'unknown')) await lookup(entry)
    for (const entry of initial.filter((q) => q.state === 'confirmed')) await lookup(entry)
    const queued = (await vault.read()).queue
      .filter((q) => q.state === 'queued')
      .sort((a, b) =>
        BigInt(JSON.parse(a.operation).device_seq) < BigInt(JSON.parse(b.operation).device_seq)
          ? -1
          : 1,
      )
    for (const entry of queued) {
      await vault.updateLocked((s) => {
        s.queue.find((q) => q.operation === entry.operation)!.state = 'unknown'
      })
      const syncID = crypto.randomUUID()
      const raw = await call('sync', {
        owner,
        csrf: session.csrf_token,
        method: 'POST',
        body: `{"sync_request_id":"${syncID}","payload_schema_version":1,"operations":[${entry.operation}]}`,
      })
      const response = z
        .object({ sync_request_id: z.literal(syncID), results: z.array(z.unknown()).length(1) })
        .parse(JSON.parse(raw))
      const accepted = acceptResult(entry, response.results[0])
      await vault.updateLocked((s) => {
        s.queue[s.queue.findIndex((q) => q.operation === entry.operation)] = accepted
        s.serverTime = Math.max(s.serverTime, Date.parse(session.server_time))
      })
      if (accepted.state === 'queued') break
    }
  })
}
export async function discard(vault?: Vault) {
  const session = await readOfflineSession().catch(() => undefined)
  if (session && (await purgeIfRequired(session, vault))) return
  await destroy(vault)
  if (session)
    await call('session', { owner: binding(session), csrf: session.csrf_token, method: 'DELETE' })
}
