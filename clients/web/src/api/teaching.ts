import { z } from 'zod'
import { ApiError, learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'

const id = z.uuid()
const version = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
export const referenceSchema = z.object({
  knowledge_revision_id: id,
  node_id: id,
  node_revision_id: id,
  document_revision_id: id.optional(),
  range: z.object({ start: z.number().int(), end: z.number().int() }),
  slice: z.string().default(''),
  slice_sha256: z.string(),
})
export type Reference = z.infer<typeof referenceSchema>
const goalRevision = z
  .object({
    goal_id: id,
    goal_revision_id: id,
    text: z.string(),
    learning_space_id: id.optional(),
    management: z
      .object({
        details: z.object({ name: z.string(), scope_snapshot_id: id.optional() }).passthrough(),
        status: z.string(),
      })
      .passthrough()
      .optional(),
  })
  .passthrough()
const activity = z
  .object({
    activity_id: id,
    revision: version,
    session_id: id,
    type: z.string(),
    prompt: z.string(),
    knowledge_revision_id: id,
    goal_revision_id: id,
    route_revision_id: id,
    route_step_id: id,
    target_node_revision_id: id,
    knowledge_references: z.array(referenceSchema),
    allowed_help: z.array(z.enum(['none', 'hint', 'scaffold', 'answer_revealed'])),
  })
  .passthrough()
export const teachingSchema = z
  .object({
    session: z
      .object({
        session_id: id,
        aggregate_version: version,
        state: z.string(),
        focus: z
          .object({
            goal_revision_id: id.optional(),
            route_revision_id: id.optional(),
            route_step_id: id.optional(),
            knowledge_revision_id: id.optional(),
            focus_node_revision_id: id.optional(),
            activity_id: id.optional(),
          })
          .passthrough(),
        active_focus_frame: z.object({ focus_frame_id: id }).passthrough().optional(),
      })
      .passthrough(),
    work_item: z
      .object({
        allowed_actions: z.array(z.string()),
        allowed_assessment_decisions: z.array(z.string()),
        goal_revision: goalRevision.optional(),
        activity: activity.optional(),
        route_revision: z
          .object({
            route_revision_id: id,
            knowledge_revision_id: id,
            steps: z.array(
              z
                .object({
                  route_step_id: id,
                  node_revision_id: id,
                  teaching_intent: z.string(),
                  completion_condition: z.string(),
                })
                .passthrough(),
            ),
          })
          .passthrough()
          .optional(),
        attempt: z
          .object({ attempt_id: id, answer: z.string(), help: z.string() })
          .passthrough()
          .optional(),
        assessment: z
          .object({
            assessment_id: id,
            confidence: z.number(),
            risk_flags: z.array(z.string()),
            items: z.array(
              z
                .object({
                  rubric_item_id: z.string(),
                  conclusion: z.string(),
                  answer_quote: z.string(),
                })
                .passthrough(),
            ),
          })
          .passthrough()
          .optional(),
        assessment_decision: z
          .object({ disposition: z.string(), version: version })
          .passthrough()
          .optional(),
        free_question: z
          .object({ free_question_id: id, focus_frame_id: id, text: z.string() })
          .passthrough()
          .optional(),
        free_answer: z.object({ free_answer_id: id, text: z.string() }).passthrough().optional(),
      })
      .passthrough()
      .nullable(),
  })
  .passthrough()
export type Teaching = z.infer<typeof teachingSchema>
export type Help = components['schemas']['HelpLevel']
export type Action = components['schemas']['TutoringActionRequest']

export type Block = {
  block_id: string
  kind: string
  fallback: string
  text?: string
  language?: string
  reference_id?: string
  rows?: string[][]
  children?: Block[]
}
const blockSchema: z.ZodType<Block> = z.lazy(() =>
  z.object({
    block_id: id,
    kind: z.string().max(64),
    fallback: z.string().min(1),
    text: z.string().optional(),
    language: z.string().optional(),
    reference_id: id.optional(),
    rows: z.array(z.array(z.string()).max(20)).max(100).optional(),
    children: z.array(blockSchema).max(256).optional(),
  }),
)
export const selectionSchema = z.object({
  space_id: id,
  goal_id: id,
  session_id: id,
  artifact_id: id,
  version,
  block_id: id,
  start: z.number().int().nonnegative(),
  end: z.number().int().positive(),
  sha256: z.string().regex(/^[a-f0-9]{64}$/),
})
export type ContentSelection = z.infer<typeof selectionSchema>
export const contentSchema = z.object({
  protocol_version: z.literal(1),
  artifact_id: id,
  version,
  committed_version: version,
  learning_space_id: id,
  goal_id: id,
  goal_revision_id: id,
  session_id: id,
  activity_id: id,
  activity_revision: version,
  privacy_generation: version,
  actor_device_id: id,
  status: z.enum(['draft', 'failed', 'committed']),
  created_at: z.string(),
  body: z.object({
    change: z
      .object({
        base_version: version,
        restored_version: version.optional(),
        action: z.string(),
        reason: z.string(),
        selection: selectionSchema.optional(),
        changed_blocks: z.array(id),
      })
      .optional(),
    lineage: z
      .array(z.object({ artifact_id: id, version, block_id: id, source_block_id: id.optional() }))
      .optional(),
    blocks: z.array(blockSchema).min(1).max(256),
    interaction: z.object({
      kind: z.string(),
      choices: z
        .array(z.object({ value: z.string(), label: z.string() }))
        .max(20)
        .optional(),
    }),
    references: z.array(referenceSchema),
    model_id: z.string(),
    input_fingerprint: z.string(),
    semantic_fingerprint: z.string(),
  }),
})
export type Content = z.infer<typeof contentSchema>
export const contentCapabilities = z.object({
  protocol_version: z.number(),
  available: z.boolean(),
  blocks: z.array(z.string()),
  interactions: z.array(z.string()),
})
export const contentHistory = z.object({
  items: z.array(z.object({ version, status: z.string(), created_at: z.string() })),
})
export const sessionPage = z.object({
  items: z.array(
    z.object({
      session_id: id,
      learning_space_id: id,
      goal_id: id,
      name: z.string(),
      state: z.string(),
      position: z.string(),
      resumable: z.boolean(),
    }),
  ),
  next_cursor: z.string().optional(),
})
export const operationResult = z
  .object({ status: z.literal('succeeded'), aggregate_id: id, aggregate_version: version })
  .passthrough()
export const operationReceipt = z.object({
  operation_id: id,
  session_id: id,
  status: z.enum(['succeeded', 'rejected']),
  aggregate_version: z.number().int(),
  code: z.string().optional(),
})
export const stateLabels: Record<string, string> = {
  GoalReady: '准备开始',
  Diagnostic: '准备学习路线',
  RouteActive: '阅读与探索',
  ActivityIssued: '活动已就绪',
  AwaitingResponse: '等待正式答案',
  Evaluating: '答案已保存，等待反馈',
  Feedback: '查看反馈',
  FreeQuestion: '讨论中，原焦点已保存',
  FreeAnswer: '导师已回复',
  Completed: '本次学习已完成',
}
export const helpLabels: Record<Help, string> = {
  none: '独立作答',
  hint: '已获得提示',
  scaffold: '已获得分步引导',
  answer_revealed: '已查看答案',
}

export function answerable(content?: Content) {
  return (
    !!content &&
    content.status === 'committed' &&
    content.version === content.committed_version &&
    ['text', 'single_choice'].includes(content.body.interaction.kind)
  )
}
export function operation(view: Teaching, operationID: string) {
  return {
    operation_id: operationID,
    payload_schema_version: 1 as const,
    aggregate_type: 'session' as const,
    aggregate_id: view.session.session_id,
    expected_version: view.session.aggregate_version,
  }
}
export const contentHeader = (spaceId: string) => ({
  'X-Learning-Space-ID': spaceId,
  'X-Learning-Content-Version': '1' as const,
})

// 模型只产出提案；核对原会话版本后才能由用户动作应用，半成品不会进入内容版本。
export async function propose(
  identity: Session,
  spaceId: string,
  view: Teaching,
  kind: 'route' | 'activity' | 'assessment' | 'free_answer' | 'explanation',
  requestID: string,
) {
  const client = learningClient(identity, spaceId)
  const item = view.work_item
  if (!item?.goal_revision) throw new ApiError(409, 'missing_work_item')
  const focus = view.session.focus
  const refs = item.activity?.knowledge_references
  let knowledgeRevision = item.activity?.knowledge_revision_id ?? focus.knowledge_revision_id
  let hits: Reference[] = refs ?? []
  if (!hits.length) {
    // 已开课现场以实际知识版本为准，不能被目标创建时的旧冻结范围覆盖。
    const scope = knowledgeRevision
      ? undefined
      : item.goal_revision.management?.details.scope_snapshot_id
    if (!knowledgeRevision && !scope) {
      const head = await unwrap(
        client.GET('/v1/knowledge/revisions/head'),
        z.object({ revision: z.object({ revision_id: id }).passthrough() }),
      )
      knowledgeRevision = head.revision.revision_id
    }
    const retrieval = await unwrap(
      client.POST('/v1/knowledge/retrievals', {
        body: {
          query: item.free_question?.text ?? item.goal_revision.text,
          ...(scope ? { scope_snapshot_id: scope } : { knowledge_revision_id: knowledgeRevision! }),
          query_context_schema_version: 'query-context-v1',
          context: {
            session_id: view.session.session_id,
            aggregate_version: view.session.aggregate_version,
            tutoring_state: view.session.state,
            goal_revision_id: item.goal_revision.goal_revision_id,
          },
          limits: { max_depth: 4, candidates_per_layer: 12, max_hits: 10, total_candidates: 100 },
        },
      }),
      z.object({
        knowledge_revision_id: id,
        degraded: z.boolean(),
        truncated: z.boolean(),
        hits: z.array(
          z.object({
            node_revision_id: id,
            node_id: id,
            document_revision_id: id,
            canonical_slice: z.string(),
            slice_sha256: z.string(),
            section_range: z.object({ start: z.number(), end: z.number() }),
          }),
        ),
      }),
    )
    if (retrieval.degraded || retrieval.truncated) throw new ApiError(409, 'retrieval_incomplete')
    knowledgeRevision = retrieval.knowledge_revision_id
    hits = retrieval.hits.map((h) => ({
      knowledge_revision_id: retrieval.knowledge_revision_id,
      node_id: h.node_id,
      node_revision_id: h.node_revision_id,
      document_revision_id: h.document_revision_id,
      range: h.section_range,
      slice: h.canonical_slice,
      slice_sha256: h.slice_sha256,
    }))
  }
  if (!knowledgeRevision || !hits.length) throw new ApiError(409, 'knowledge_scope_required')
  const result = await unwrap(
    client.POST('/v1/tutoring/proposals', {
      body: {
        request_id: requestID,
        proposal_type: kind,
        aggregate_type: 'session',
        aggregate_id: view.session.session_id,
        aggregate_version: view.session.aggregate_version,
        goal_revision_id: item.goal_revision.goal_revision_id,
        route_revision_id: focus.route_revision_id,
        route_step_id: focus.route_step_id,
        focus_node_revision_id: focus.focus_node_revision_id,
        activity_id: item.activity?.activity_id,
        attempt_id: item.attempt?.attempt_id,
        free_question_id: item.free_question?.free_question_id,
        free_answer_id: item.free_answer?.free_answer_id,
        focus_frame_id: item.free_question?.focus_frame_id,
        tutoring_state: view.session.state as components['schemas']['TutoringState'],
        knowledge_revision_id: knowledgeRevision,
        node_revision_ids: [...new Set(hits.map((h) => h.node_revision_id))],
        input: {
          schema_version: 'go-cli-context-v1',
          work_item: item,
          retrieval: { knowledge_revision_id: knowledgeRevision, hits },
        },
      },
    }),
    z.object({ proposal_id: id }).passthrough(),
  )
  const current = await unwrap(
    client.GET('/v1/tutoring/sessions/{sessionID}', {
      params: { path: { sessionID: view.session.session_id } },
    }),
    teachingSchema,
  )
  if (
    current.session.session_id !== view.session.session_id ||
    current.session.aggregate_version !== view.session.aggregate_version
  )
    throw new ApiError(409, 'stale_proposal')
  return result.proposal_id
}
