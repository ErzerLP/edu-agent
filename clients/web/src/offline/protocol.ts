import { z } from 'zod'
import {
  base64,
  bytes,
  canonical,
  digest,
  hexDigest,
  original,
  parseSigned,
  unbase64,
} from './crypto'

const id = z.uuid()
const decimal = z
  .string()
  .regex(/^(0|[1-9][0-9]*)$/)
  .refine((v) => BigInt(v) <= 9223372036854775807n)
const time = z
  .string()
  .regex(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/)
  .refine((v) => Number.isFinite(Date.parse(v)))
const hash = z.string().regex(/^[A-Za-z0-9_-]{43}$/)
const signature = z.string().regex(/^[A-Za-z0-9_-]{86}$/)
const keySchema = z.strictObject({
  key_id: z.string().min(1).max(128),
  public_key: hash,
  fingerprint: hash,
  not_before: time,
  not_after: time,
  status_effective_at: time,
  status: z.enum(['active', 'verify_only', 'retired']),
})
const manifestPayload = z.strictObject({
  protocol_version: z.literal(1),
  manifest_revision: decimal,
  issuer: z.literal('edu-agent'),
  server_base_url: z.string(),
  previous_manifest_digest: hash,
  issued_at: time,
  keys: z.array(keySchema).min(1).max(16),
})
const envelope = <T extends z.ZodType>(payload: T) =>
  z.strictObject({ payload, signer_key_id: z.string().min(1), signature })
const manifestSchema = envelope(manifestPayload)
export type Manifest = z.infer<typeof manifestSchema>
type Key = z.infer<typeof keySchema>
const authorization = z.strictObject({
  protocol_version: z.number().int(),
  format: z.string(),
  issuer: z.literal('edu-agent'),
  signer_key_id: z.string(),
  pack_id: id,
  device_id: id,
  credential_epoch: decimal,
  learner_generation: decimal,
  server_origin_digest: hash,
  offline_activity_id: id,
  activity_revision: decimal,
  submission_id: id,
  operation_id: id,
  device_seq: decimal,
  expected_version: decimal,
  activity_payload_digest: hash,
  eligible_until: time,
  archive_until: time,
})
const activity = z
  .object({
    activity_id: id,
    revision: z.number().int().positive().max(Number.MAX_SAFE_INTEGER),
    session_id: id,
    goal_revision_id: id,
    route_revision_id: id,
    knowledge_revision_id: id,
    prompt: z.string().max(1_048_576),
    type: z.string(),
    allowed_help: z.array(z.string()),
    activity_policy_version: z.string(),
    assessment_policy_version: z.string(),
    rubric: z
      .object({
        items: z.array(
          z.object({ rubric_item_id: z.string(), criterion: z.string() }).passthrough(),
        ),
      })
      .passthrough(),
    knowledge_references: z.array(
      z
        .object({
          slice: z.string(),
          slice_sha256: z.string(),
          knowledge_revision_id: id,
          node_revision_id: id,
        })
        .passthrough(),
    ),
  })
  .passthrough()
const itemSchema = z.strictObject({
  activity,
  activity_payload_digest: hash,
  authorization: envelope(authorization),
})
export type Item = z.infer<typeof itemSchema>
const packPayload = z.strictObject({
  protocol_version: z.literal(1),
  pack_id: id,
  revision: decimal,
  device_id: id,
  learner_generation: decimal,
  parent_session_id: id,
  issued_at: time,
  eligible_until: time,
  archive_until: time,
  truncated: z.boolean(),
  truncated_reason: z.string().optional(),
  items: z.array(itemSchema).min(1).max(20),
})
const responseSchema = z.strictObject({
  operation_id: id,
  replayed: z.boolean(),
  pack: envelope(packPayload),
  manifest_chain: z.array(manifestSchema).max(16),
  response_signature: envelope(
    z.strictObject({
      protocol_version: z.literal(1),
      operation_id: id,
      request_hash: hash,
      replayed: z.boolean(),
      pack_digest: hash,
      manifest_revision: decimal,
      manifest_digest: hash,
      response_at: time,
    }),
  ),
})
export type PreparedResponse = z.infer<typeof responseSchema>
export type Owner = {
  deviceId: string
  generation: string
  origin: string
  root: string
  expiresAt: string
}
export type Selection = {
  spaceId: string
  goalId: string
  sessionId: string
  sessionVersion: string
  name: string
}
export type PrepareIntent = { selection: Selection; request: string; trust: string }
export type SavedPack = { response: string; intent: PrepareIntent }

function validate<T>(schema: z.ZodType<T>, value: unknown): T {
  if (!schema.safeParse(value).success) throw new Error('离线协议字段不兼容或已损坏')
  return value as T
}
export const readManifest = (raw: string) => validate(manifestSchema, parseSigned(raw))
export const readPack = (raw: string) => validate(responseSchema, parseSigned(raw))
const require = (condition: unknown, message = '离线签名、摘要或原始归属不匹配') => {
  if (!condition) throw new Error(message)
}
async function verify(
  envelope: { payload: object; signer_key_id: string; signature: string },
  domain: string,
  key: Key,
) {
  require(envelope.signer_key_id === key.key_id)
  const publicKey = unbase64(key.public_key)
  require((await digestBytes(publicKey)) === key.fingerprint)
  const imported = await crypto.subtle.importKey('raw', publicKey, 'Ed25519', false, ['verify'])
  const data = new Uint8Array([
    ...bytes(`edu-agent-${domain}-v1\n`),
    ...unbase64(await digest(canonical(envelope.payload))),
  ])
  require(await crypto.subtle.verify('Ed25519', imported, unbase64(envelope.signature), data))
}
const digestBytes = async (value: Uint8Array<ArrayBuffer>) =>
  base64(new Uint8Array(await crypto.subtle.digest('SHA-256', value)))
function signingKey(manifest: Manifest, keyId: string, at: string, historical = false): Key {
  const key = manifest.payload.keys.find((k) => k.key_id === keyId)
  require(key)
  require(
    Date.parse(at) >= Date.parse(key!.not_before) && Date.parse(at) < Date.parse(key!.not_after),
  )
  require(
    key!.status === 'active'
      ? Date.parse(at) >= Date.parse(key!.status_effective_at)
      : historical && Date.parse(at) < Date.parse(key!.status_effective_at),
  )
  return key!
}
async function manifestMetadata(manifest: Manifest, owner: Owner) {
  require(manifest.payload.server_base_url === owner.origin)
  require(new Set(manifest.payload.keys.map((k) => k.key_id)).size === manifest.payload.keys.length)
  require(manifest.payload.keys.filter((k) => k.status === 'active').length === 1)
  for (const key of manifest.payload.keys) {
    require(Date.parse(key.not_before) < Date.parse(key.not_after))
    require(
      Date.parse(key.status_effective_at) >= Date.parse(key.not_before) &&
        Date.parse(key.status_effective_at) <= Date.parse(key.not_after),
    )
    require((await digestBytes(unbase64(key.public_key))) === key.fingerprint)
  }
}
export async function verifyRoot(owner: Owner) {
  const root = readManifest(owner.root)
  await manifestMetadata(root, owner)
  require(
    root.payload.manifest_revision === '1' &&
      root.payload.previous_manifest_digest === base64(new Uint8Array(32)),
  )
  await verify(
    root,
    'signer-manifest',
    signingKey(root, root.signer_key_id, root.payload.issued_at),
  )
  return root
}
export async function prepareIntent(
  owner: Owner,
  selection: Selection,
  trust: string,
): Promise<PrepareIntent> {
  const manifest = readManifest(trust)
  return {
    selection,
    trust,
    request: canonical({
      operation_id: crypto.randomUUID(),
      payload_schema_version: 1,
      session_id: selection.sessionId,
      expected_session_version: selection.sessionVersion,
      trusted_manifest_revision: manifest.payload.manifest_revision,
      trusted_manifest_digest: await digest(canonical(manifest.payload)),
      requested_count: 1,
    }),
  }
}
export async function verifyPack(saved: SavedPack, owner: Owner) {
  const response = readPack(saved.response)
  let trust = readManifest(saved.intent.trust)
  await manifestMetadata(trust, owner)
  const request = parseSigned(saved.intent.request) as {
    operation_id: string
    trusted_manifest_revision: string
    trusted_manifest_digest: string
  }
  require(
    request.trusted_manifest_revision === trust.payload.manifest_revision &&
      request.trusted_manifest_digest === (await digest(canonical(trust.payload))),
  )
  for (const next of response.manifest_chain) {
    await manifestMetadata(next, owner)
    require(
      BigInt(next.payload.manifest_revision) === BigInt(trust.payload.manifest_revision) + 1n &&
        next.payload.previous_manifest_digest === (await digest(canonical(trust.payload))),
    )
    require(Date.parse(next.payload.issued_at) >= Date.parse(trust.payload.issued_at))
    await verify(
      next,
      'signer-manifest',
      signingKey(trust, next.signer_key_id, next.payload.issued_at),
    )
    trust = next
  }
  const signed = response.response_signature.payload
  require(
    response.operation_id === request.operation_id &&
      signed.operation_id === request.operation_id &&
      signed.replayed === response.replayed,
  )
  require(
    signed.request_hash === (await digest(canonical(parseSigned(saved.intent.request)))) &&
      signed.pack_digest === (await digest(canonical(response.pack))),
  )
  require(
    signed.manifest_revision === trust.payload.manifest_revision &&
      signed.manifest_digest === (await digest(canonical(trust.payload))),
  )
  await verify(
    response.response_signature,
    'offline-prepare-response',
    signingKey(trust, response.response_signature.signer_key_id, signed.response_at),
  )
  const pack = response.pack.payload
  await verify(
    response.pack,
    'offline-pack',
    signingKey(trust, response.pack.signer_key_id, pack.issued_at, true),
  )
  require(
    pack.device_id === owner.deviceId &&
      pack.learner_generation === owner.generation &&
      pack.parent_session_id === saved.intent.selection.sessionId,
  )
  require(
    Date.parse(pack.issued_at) < Date.parse(pack.eligible_until) &&
      Date.parse(pack.eligible_until) < Date.parse(pack.archive_until),
  )
  const operations = new Set<string>()
  const submissions = new Set<string>()
  const sequences = new Set<string>()
  for (const item of pack.items) {
    const auth = item.authorization.payload
    await verify(
      item.authorization,
      'offline-authorization',
      signingKey(trust, item.authorization.signer_key_id, pack.issued_at, true),
    )
    require(
      item.activity_payload_digest === (await digest(canonical(item.activity))) &&
        auth.activity_payload_digest === item.activity_payload_digest,
    )
    require(
      auth.signer_key_id === item.authorization.signer_key_id &&
        auth.signer_key_id === response.pack.signer_key_id,
    )
    require(
      auth.device_id === owner.deviceId &&
        auth.learner_generation === owner.generation &&
        auth.server_origin_digest === (await digest(owner.origin)) &&
        auth.pack_id === pack.pack_id,
    )
    require(
      auth.offline_activity_id === item.activity.activity_id &&
        auth.activity_revision === String(item.activity.revision) &&
        item.activity.session_id === pack.parent_session_id,
    )
    require(
      auth.eligible_until === pack.eligible_until &&
        auth.archive_until === pack.archive_until &&
        auth.expected_version === '0' &&
        BigInt(auth.device_seq) > 0n &&
        BigInt(auth.credential_epoch) > 0n,
    )
    require(
      !operations.has(auth.operation_id) &&
        !submissions.has(auth.submission_id) &&
        !sequences.has(auth.device_seq),
    )
    operations.add(auth.operation_id)
    submissions.add(auth.submission_id)
    sequences.add(auth.device_seq)
    for (const ref of item.activity.knowledge_references)
      require(ref.slice_sha256 === (await hexDigest(ref.slice)))
  }
  return { response, trust: original(trust) }
}
export function supportsAnswer(item: Item): boolean {
  const auth = item.authorization.payload
  return (
    auth.protocol_version === 1 &&
    auth.format === 'offline-authorization-v1' &&
    ['objective', 'open'].includes(item.activity.type) &&
    item.activity.allowed_help.includes('none') &&
    item.activity.activity_policy_version === 'activity-policy-v1' &&
    item.activity.assessment_policy_version === 'assessment-acceptance-v1'
  )
}
export async function answerOperation(item: Item, answer: string): Promise<string> {
  if (!supportsAnswer(item)) throw new Error('未知作答协议：只读，不允许提交')
  if (!answer.trim() || bytes(answer).length > 64 * 1024)
    throw new Error('答案必须非空且不超过 64 KiB')
  const auth = item.authorization.payload
  const fields = {
    operation_id: auth.operation_id,
    device_id: auth.device_id,
    device_seq: auth.device_seq,
    submission_id: auth.submission_id,
    payload_schema_version: 1,
    aggregate_type: 'offline_attempt',
    aggregate_id: auth.submission_id,
    expected_version: auth.expected_version,
    offline_activity_id: auth.offline_activity_id,
    activity_revision: auth.activity_revision,
    signature: item.authorization.signature,
    occurred_at: null,
    operation_type: 'offline_attempt_completed',
    payload: { answer, answer_sha256: await hexDigest(answer), help: 'none', observations: [] },
  }
  // 签名授权按原传输字节嵌入；队列以后重试使用这段完全相同的 operation。
  return `${canonical(fields).slice(0, -1)},"authorization":${original(auth)}}`
}
