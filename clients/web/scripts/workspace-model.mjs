import { createHash } from 'node:crypto'

// 仅供真实浏览器验收的本地 HTTP 模型；资料和教学状态来自真实服务端请求。
export function workspaceModel(payload) {
  if (payload.response_format?.json_schema?.name === 'capability_probe')
    return { capability_probe: true }
  const input = JSON.parse(payload.messages.findLast((message) => message.role === 'user').content)
  if (input.candidates)
    return {
      knowledge_revision_id: input.knowledge_revision_id,
      candidate_set_hash: input.candidate_set_hash,
      decisions: input.candidates.map((candidate) => ({
        node_revision_id: candidate.node_revision_id,
        action: candidate.has_children ? 'expand' : 'select',
      })),
    }
  const hits = input.input.retrieval.hits
  const references = hits.map((hit) => ({
    node_revision_id: hit.node_revision_id,
    range: { start: hit.range.start, end: hit.range.end },
    slice_sha256: hit.slice_sha256,
  }))
  const item = input.input.work_item
  switch (input.proposal_type) {
    case 'route':
      return {
        route: hits.map((hit) => ({
          node_revision_id: hit.node_revision_id,
          teaching_intent: '理解偶数并解释依据',
          completion_condition: '完成活动后继续',
        })),
      }
    case 'activity':
      return {
        activity: {
          prompt:
            '根据原资料，哪个是偶数？\n\nA. 2\n\nB. 3\n\n```text\n' +
            '长代码'.repeat(180) +
            '\n```',
          type: 'objective',
          rubric: {
            rubric_revision: 'browser-r1',
            items: [
              {
                rubric_item_id: 'even',
                criterion: '识别偶数',
                required_reference_ids: [references[0].node_revision_id],
              },
            ],
            objective_rule: { accepted_answers: ['A'], case_sensitive: false, trim_space: true },
          },
          difficulty: 1,
          allowed_help: ['none', 'hint', 'scaffold', 'answer_revealed'],
          knowledge_references: references,
        },
      }
    case 'free_answer':
    case 'explanation':
      return {
        text: {
          text: '原资料说明：偶数可以被 2 整除。先检查选项的含义。',
          knowledge_references: references,
        },
      }
    case 'assessment': {
      const answer = item.attempt.answer
      const quote = hits[0].slice
      const hash = (text) => createHash('sha256').update(text).digest('hex')
      return {
        assessment: {
          items: [
            {
              rubric_item_id: item.activity.rubric.items[0].rubric_item_id,
              conclusion: 'pass',
              answer_quote: answer,
              answer_range: { start: 0, end: Buffer.byteLength(answer) },
              answer_quote_sha256: hash(answer),
              knowledge_reference_id: hits[0].node_revision_id,
              knowledge_quote: quote,
              knowledge_range: hits[0].range,
              knowledge_quote_sha256: hash(quote),
            },
          ],
          rubric_complete: true,
          confidence: 1000,
          risk_flags: [],
        },
      }
    }
    default:
      throw new Error('验收模型收到未知请求')
  }
}
