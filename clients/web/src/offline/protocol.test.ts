import { expect, it } from 'vitest'
import { base64, bytes, canonical, digest, unbase64 } from './crypto'
import {
  answerOperation,
  prepareIntent,
  supportsAnswer,
  verifyPack,
  verifyRoot,
  type Owner,
} from './protocol'

async function signedFixture(unknownProtocol = false, rotate = false) {
  const pair = (await crypto.subtle.generateKey('Ed25519', true, [
    'sign',
    'verify',
  ])) as CryptoKeyPair
  const nextPair = (await crypto.subtle.generateKey('Ed25519', true, [
    'sign',
    'verify',
  ])) as CryptoKeyPair
  const publicKey = base64(new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey)))
  const newPublicKey = base64(
    new Uint8Array(await crypto.subtle.exportKey('raw', nextPair.publicKey)),
  )
  const fingerprint = async (value: string) =>
    base64(new Uint8Array(await crypto.subtle.digest('SHA-256', unbase64(value))))
  const sign = async <T extends object>(
    domain: string,
    payload: T,
    key = pair.privateKey,
    id = '原签发器',
  ) => ({
    payload,
    signer_key_id: id,
    signature: base64(
      new Uint8Array(
        await crypto.subtle.sign(
          'Ed25519',
          key,
          new Uint8Array([
            ...bytes(`edu-agent-${domain}-v1\n`),
            ...unbase64(await digest(canonical(payload))),
          ]),
        ),
      ),
    ),
  })
  const rootPayload = {
    protocol_version: 1,
    manifest_revision: '1',
    issuer: 'edu-agent',
    server_base_url: 'https://learn.example/',
    previous_manifest_digest: base64(new Uint8Array(32)),
    issued_at: '2026-01-01T00:00:00Z',
    keys: [
      {
        key_id: '原签发器',
        public_key: publicKey,
        fingerprint: await fingerprint(publicKey),
        not_before: '2026-01-01T00:00:00Z',
        not_after: '2027-01-01T00:00:00Z',
        status_effective_at: '2026-01-01T00:00:00Z',
        status: 'active',
      },
    ],
  }
  const root = await sign('signer-manifest', rootPayload)
  const owner: Owner = {
    deviceId: crypto.randomUUID(),
    generation: '1',
    origin: rootPayload.server_base_url,
    root: canonical(root),
    expiresAt: '2026-10-20T00:00:00Z',
  }
  const session = crypto.randomUUID()
  const intent = await prepareIntent(
    owner,
    {
      spaceId: crypto.randomUUID(),
      goalId: crypto.randomUUID(),
      sessionId: session,
      sessionVersion: '7',
      name: '原会话',
    },
    owner.root,
  )
  const rotated = await sign('signer-manifest', {
    ...rootPayload,
    manifest_revision: '2',
    previous_manifest_digest: await digest(canonical(rootPayload)),
    issued_at: '2026-06-01T00:00:00Z',
    keys: [
      {
        ...rootPayload.keys[0],
        status: 'verify_only',
        status_effective_at: '2026-06-01T00:00:00Z',
      },
      {
        ...rootPayload.keys[0],
        key_id: '新签发器',
        public_key: newPublicKey,
        fingerprint: await fingerprint(newPublicKey),
        status_effective_at: '2026-06-01T00:00:00Z',
      },
    ],
  })
  const active = rotate ? rotated : root
  const signArtifact = <T extends object>(domain: string, payload: T) =>
    sign(
      domain,
      payload,
      rotate ? nextPair.privateKey : pair.privateKey,
      rotate ? '新签发器' : '原签发器',
    )
  const packID = crypto.randomUUID()
  const activity = {
    activity_id: crypto.randomUUID(),
    revision: 1,
    session_id: session,
    goal_revision_id: crypto.randomUUID(),
    route_revision_id: crypto.randomUUID(),
    knowledge_revision_id: crypto.randomUUID(),
    prompt: '原签名题目',
    type: 'objective',
    allowed_help: ['none'],
    activity_policy_version: 'activity-policy-v1',
    assessment_policy_version: 'assessment-acceptance-v1',
    rubric: { items: [] },
    knowledge_references: [],
  }
  const activityDigest = await digest(canonical(activity))
  const authorization = await signArtifact('offline-authorization', {
    protocol_version: unknownProtocol ? 2 : 1,
    format: unknownProtocol ? 'future-answer-v2' : 'offline-authorization-v1',
    issuer: 'edu-agent',
    signer_key_id: rotate ? '新签发器' : '原签发器',
    pack_id: packID,
    device_id: owner.deviceId,
    credential_epoch: '1',
    learner_generation: owner.generation,
    server_origin_digest: await digest(owner.origin),
    offline_activity_id: activity.activity_id,
    activity_revision: '1',
    submission_id: crypto.randomUUID(),
    operation_id: crypto.randomUUID(),
    device_seq: '9223372036854775807',
    expected_version: '0',
    activity_payload_digest: activityDigest,
    eligible_until: '2026-09-22T00:00:00Z',
    archive_until: '2026-10-22T00:00:00Z',
  })
  const pack = await signArtifact('offline-pack', {
    protocol_version: 1,
    pack_id: packID,
    revision: '1',
    device_id: owner.deviceId,
    learner_generation: owner.generation,
    parent_session_id: session,
    issued_at: '2026-09-19T00:00:00Z',
    eligible_until: authorization.payload.eligible_until,
    archive_until: authorization.payload.archive_until,
    truncated: false,
    items: [{ activity, activity_payload_digest: activityDigest, authorization }],
  })
  const operationID = JSON.parse(intent.request).operation_id
  const response = {
    operation_id: operationID,
    replayed: false,
    pack,
    manifest_chain: rotate ? [rotated] : [],
    response_signature: await signArtifact('offline-prepare-response', {
      protocol_version: 1,
      operation_id: operationID,
      request_hash: await digest(intent.request),
      replayed: false,
      pack_digest: await digest(canonical(pack)),
      manifest_revision: active.payload.manifest_revision,
      manifest_digest: await digest(canonical(active.payload)),
      response_at: '2026-09-19T00:00:00Z',
    }),
  }
  return { owner, saved: { intent, response: JSON.stringify(response, null, 2) }, response }
}
it('完整验证签名链、冻结题目与原身份，保留授权字节和大整数', async () => {
  const fixture = await signedFixture(false, true)
  await verifyRoot(fixture.owner)
  const checked = await verifyPack(fixture.saved, fixture.owner)
  expect(JSON.parse(checked.trust).payload.manifest_revision).toBe('2')
  const item = checked.response.pack.payload.items[0]
  const operation = await answerOperation(item, '原答案')
  expect(JSON.parse(operation).device_seq).toBe('9223372036854775807')
  expect(JSON.parse(operation).authorization).toEqual(item.authorization.payload)
  expect(operation).toContain('\n')
  for (const owner of [
    { ...fixture.owner, deviceId: crypto.randomUUID() },
    { ...fixture.owner, generation: '2' },
    { ...fixture.owner, origin: 'https://another.example/' },
  ])
    await expect(verifyPack(fixture.saved, owner)).rejects.toThrow()
  const tampered = structuredClone(fixture.response)
  tampered.pack.payload.items[0].activity.prompt = '替换的新题目'
  await expect(
    verifyPack({ ...fixture.saved, response: JSON.stringify(tampered) }, fixture.owner),
  ).rejects.toThrow()
  const broken = structuredClone(fixture.response)
  broken.manifest_chain[0].payload.manifest_revision = '3'
  await expect(
    verifyPack({ ...fixture.saved, response: JSON.stringify(broken) }, fixture.owner),
  ).rejects.toThrow()
})
it('真实签名的未知授权协议仍只能只读，不能生成提交', async () => {
  const fixture = await signedFixture(true)
  const verified = await verifyPack(fixture.saved, fixture.owner)
  const item = verified.response.pack.payload.items[0]
  expect(item.activity.prompt).toBe('原签名题目')
  expect(supportsAnswer(item)).toBe(false)
  await expect(answerOperation(item, '答案')).rejects.toThrow('未知作答协议')
})
