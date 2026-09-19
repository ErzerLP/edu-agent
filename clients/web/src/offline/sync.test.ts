import { expect, it, vi } from 'vitest'
import type { QueueEntry } from './vault'

vi.stubGlobal('window', { location: { origin: 'https://learn.example' } })
const { acceptResult } = await import('./sync')
const operationId = '11111111-1111-4111-8111-111111111111'
const submissionId = '22222222-2222-4222-8222-222222222222'
const entry: QueueEntry = {
  operation: JSON.stringify({
    operation_id: operationId,
    submission_id: submissionId,
    device_seq: '9007199254740993',
  }),
  state: 'unknown',
}
const result = {
  operation_id: operationId,
  submission_id: submissionId,
  device_seq: '9007199254740993',
  archive_status: 'archived_succeeded',
  assessment_status: 'completed',
  evidence_status: 'accepted',
  reason_codes: [],
  ingest_receipt: {
    receipt_id: '33333333-3333-4333-8333-333333333333',
    archived_at: '2026-09-19T00:00:00Z',
    aggregate_version: '3',
    first_event_seq: '11',
    last_event_seq: '13',
    projection_as_of_event_seq: '13',
    archive_status: 'archived_succeeded',
  },
}
it('只有归属与范围完整的正式回执才标记服务端确认', () => {
  expect(acceptResult(entry, result).state).toBe('confirmed')
  expect(acceptResult(entry, result).operation).toBe(entry.operation)
  for (const bad of [
    { ...result, ingest_receipt: undefined },
    { ...result, operation_id: submissionId },
    { ...result, device_seq: '9007199254740992' },
    { ...result, ingest_receipt: { ...result.ingest_receipt, projection_as_of_event_seq: '12' } },
    { ...result, evidence_status: 'future-accepted' },
  ])
    expect(() => acceptResult(entry, bad)).toThrow()
})
it('服务端存档、拒绝、冲突、暂不可用与学习证据分别呈现', () => {
  expect(
    acceptResult(entry, {
      ...result,
      assessment_status: 'queued',
      evidence_status: 'pending_evaluation',
    }).state,
  ).toBe('confirmed')
  expect(
    acceptResult(entry, {
      ...result,
      archive_status: 'archived_rejected',
      assessment_status: 'not_requested',
      evidence_status: 'unchanged',
      ingest_receipt: { ...result.ingest_receipt, archive_status: 'archived_rejected' },
    }).state,
  ).toBe('rejected')
  for (const [archive_status, state] of [
    ['not_archived_retryable', 'queued'],
    ['not_archived_blocked', 'blocked'],
    ['idempotency_conflict', 'conflict'],
    ['not_processed', 'queued'],
  ]) {
    expect(
      acceptResult(entry, {
        operation_id: operationId,
        submission_id: submissionId,
        archive_status,
        reason_codes: [],
      }).state,
    ).toBe(state)
  }
})
