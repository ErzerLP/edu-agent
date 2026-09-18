import { z } from 'zod'
import { referenceSchema } from './teaching'
import type { components } from './schema'

const id = z.uuid()
const version = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
const range = z.object({
  start: z.number().int().nonnegative(),
  end: z.number().int().nonnegative(),
})
export const assessmentItem = z.object({
  rubric_item_id: z.string(),
  conclusion: z.enum(['pass', 'partial', 'fail', 'unassessed']),
  answer_quote: z.string(),
  answer_range: range,
  answer_quote_sha256: z.string(),
  knowledge_reference_id: z.string(),
  knowledge_quote: z.string(),
  knowledge_range: range,
  knowledge_quote_sha256: z.string(),
  misconception_candidate: z.string().optional(),
})
const disposition = z.enum(['accepted', 'provisional', 'overridden', 'voided'])
export const feedbackSchema = z
  .object({
    learning_space_id: id,
    session_version: version,
    status: z.enum(['received', 'pending', 'processing', 'ready', 'failed', 'unknown', 'settled']),
    goal_revision: z.object({
      goal_id: id,
      goal_revision_id: id,
      revision: version,
      text: z.string(),
    }),
    activity: z.object({
      activity_id: id,
      revision: version,
      session_id: id,
      prompt: z.string(),
      type: z.string(),
      goal_revision_id: id,
      route_revision_id: id,
      route_step_id: id,
      knowledge_revision_id: id,
      rubric: z.object({
        rubric_revision: z.string(),
        items: z.array(
          z.object({
            rubric_item_id: z.string(),
            criterion: z.string(),
            required_reference_ids: z.array(id).optional(),
          }),
        ),
        objective_rule: z
          .object({
            accepted_answers: z.array(z.string()),
            case_sensitive: z.boolean(),
            trim_space: z.boolean(),
          })
          .optional(),
      }),
      knowledge_references: z.array(referenceSchema),
      assessment_policy_version: z.string(),
    }),
    attempt: z.object({
      attempt_id: id,
      session_id: id,
      activity_id: id,
      activity_revision: version,
      answer: z.string(),
      help: z.enum(['none', 'hint', 'scaffold', 'answer_revealed']),
      evidence_eligibility: z.boolean(),
      evidence_ineligible_reason: z.string().optional(),
      received_at: z.string(),
    }),
    receipt: z.object({ operation_id: id, event_seq: version, received_at: z.string() }),
    content: z.object({ artifact_id: id, version }).optional(),
    knowledge_context_revision_id: id.optional(),
    assessment: z
      .object({
        assessment_id: id,
        items: z.array(assessmentItem),
        confidence: z.number(),
        risk_flags: z.array(z.string()),
        model_id: z.string(),
        rubric_complete: z.boolean(),
        prompt_revision: z.string(),
        proposal_input_hash: z.string(),
      })
      .optional(),
    decisions: z.array(
      z.object({
        decision_id: id,
        assessment_id: id,
        version,
        disposition,
        items: z.array(assessmentItem),
        reason: z.string().optional(),
        actor_device_id: id,
        created_at: z.string(),
        produced_evidence_id: id.optional(),
        replaces_decision_id: id.optional(),
      }),
    ),
    evidence: z.array(
      z.object({
        evidence_id: id,
        assessment_id: id,
        outcome: z.string(),
        help: z.string(),
        rubric_revision: z.string(),
        accepted_event_seq: version,
        acceptance_policy_version: z.string(),
      }),
    ),
    reasons: z.array(z.string()),
    allowed_decisions: z.array(z.enum(['confirm', 'override', 'void'])),
  })
  .superRefine((value, ctx) => {
    if (
      value.attempt.session_id !== value.activity.session_id ||
      value.attempt.activity_id !== value.activity.activity_id ||
      value.attempt.activity_revision !== value.activity.revision ||
      value.goal_revision.goal_revision_id !== value.activity.goal_revision_id ||
      value.decisions.some((d) => d.assessment_id !== value.assessment?.assessment_id) ||
      value.evidence.some((e) => e.assessment_id !== value.assessment?.assessment_id)
    )
      ctx.addIssue({ code: 'custom', message: '反馈身份不一致' })
  })
export type Feedback = z.infer<typeof feedbackSchema>
export type DecisionRequest = components['schemas']['AssessmentDecisionRequest']
export const feedbackPage = z.object({
  items: z.array(
    z.object({
      attempt_id: id,
      activity_id: id,
      session_id: id,
      assessment_id: id.optional(),
      received_at: z.string(),
      status: z.enum([
        'received',
        'pending',
        'processing',
        'ready',
        'failed',
        'unknown',
        'settled',
      ]),
      disposition: disposition.optional(),
    }),
  ),
  next_cursor: z.string().optional(),
})
export const carryoverSchema = z.object({
  proposal_id: id,
  knowledge_proposal_id: id,
  status: z.enum(['open', 'approved', 'rejected', 'stale', 'redacted']),
  redacted: z.boolean(),
  source_evidence_id: id.optional(),
  source_knowledge_revision_id: id.optional(),
  source_node_revision_id: id.optional(),
  target_knowledge_revision_id: id.optional(),
  candidates: z
    .array(
      z.object({
        knowledge_revision_id: id,
        node_id: id,
        node_revision_id: id,
        document_revision_id: id,
      }),
    )
    .optional(),
  basis_fingerprint: z.string().optional(),
  policy_version: z.string(),
  decision: z
    .object({
      operation_id: id,
      requested_decision: z.string(),
      outcome: z.string(),
      reason: z.string().optional(),
    })
    .optional(),
  links: z
    .array(z.object({ link_id: id, target_node_revision_id: id.optional() }).passthrough())
    .optional(),
})
export type Carryover = z.infer<typeof carryoverSchema>
export const carryoverPage = z.object({
  items: z.array(carryoverSchema),
  next_cursor: z.string().optional(),
})
export const feedbackStatus: Record<Feedback['status'], string> = {
  received: '答案已接收，尚未评估',
  pending: '评估排队中',
  processing: '评估处理中',
  ready: '模型建议已生成，等待正式提交',
  failed: '评估失败，可返回原会话重试',
  unknown: '评估状态未知，请核对原会话',
  settled: '评估已结算',
}
export const dispositionLabels = {
  accepted: '已接纳',
  provisional: '待复核',
  overridden: '已人工覆盖',
  voided: '已作废',
}
export const conclusionLabels = {
  pass: '达到要求',
  partial: '部分达到',
  fail: '尚未达到',
  unassessed: '未评估',
}
export const riskLabels: Record<string, string> = {
  low_confidence: '模型不确定，未达到自动接纳门槛',
  incomplete_rubric: '评分标准不完整',
  insufficient_answer_evidence: '答案证据不足',
  insufficient_knowledge_support: '来源支持不足',
  conflicting_evidence: '证据冲突',
  ambiguous_rubric: '标准存在歧义',
  unsafe_content: '内容风险',
  schema_repaired: '模型输出经修复',
  stale_context: '上下文过期',
  retry_exhausted: '评估重试已耗尽',
  answer_revealed: '已揭示答案',
  unassessed: '含未评估项',
  no_scorable_items: '没有可评分项',
}

// 范围和摘要使用服务端的 UTF-8 字节语义；重复引文需补充上下文以免指向错误位置。
export async function quoteEvidence(source: string, quote: string) {
  const start = source.indexOf(quote)
  if (!quote || start < 0 || source.lastIndexOf(quote) !== start)
    throw new Error('请从原文填写唯一的完整引文；重复片段请补充前后文。')
  const encoder = new TextEncoder()
  const bytes = encoder.encode(quote)
  const hash = await crypto.subtle.digest('SHA-256', bytes)
  return {
    range: {
      start: encoder.encode(source.slice(0, start)).length,
      end: encoder.encode(source.slice(0, start)).length + bytes.length,
    },
    hash: Array.from(new Uint8Array(hash), (byte) => byte.toString(16).padStart(2, '0')).join(''),
  }
}
