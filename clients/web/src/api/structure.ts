import { z } from 'zod'
import { learningClient, unwrap } from './client'
import type { Session } from './runtime'

const id = z.uuid(), version = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
export const sourceStates = { candidate: '候选', included: '已纳入', unverified: '待核实', conflict: '冲突', superseded: '已替代' }
export const learningStates = { unseen: '未接触', needs_practice: '待练习', insufficient_evidence: '证据不足', evidenced: '有证据' }
export const relationNames = { prerequisite: '前置', related: '相关', contrast: '对比', part_of: '组成' }
export const proposalStates = { open: '待审阅', applied: '已应用', rejected: '已拒绝', stale: '已过期' }
export const conceptSource = z.object({ collection_id: id, revision_id: id, document_id: id, node_id: id, quote: z.string() })
export const conceptContent = z.object({ description: z.string(), source_status: z.enum(['candidate', 'included', 'unverified', 'conflict', 'superseded']), suggested: z.boolean(), sources: z.array(conceptSource), claims: z.array(z.object({ text: z.string(), conditions: z.string(), sources: z.array(version), gap: z.string() })), relations: z.array(z.object({ target_id: id, kind: z.enum(['prerequisite', 'related', 'contrast', 'part_of']), suggested: z.boolean(), sources: z.array(version) })), replaced_by: z.array(id) })
export const structureNode = z.object({ concept_id: id, revision_id: id, semantic_key: z.string(), name: z.string(), support: z.array(z.object({ source_id: id, revision_id: id, fragment_id: id, quote: z.string() })), goal_id: z.string(), content: conceptContent, learning_state: z.enum(['unseen', 'needs_practice', 'insufficient_evidence', 'evidenced']) })
export const structurePage = z.object({ version, generation: version, items: z.array(structureNode), edges: z.array(conceptContent.shape.relations.element.extend({ source_id: id })), next_cursor: z.string(), partial: z.boolean(), notice: z.string() })
export const structureCapabilities = z.object({ protocol_version: z.literal(1), available: z.boolean(), can_propose: z.boolean(), can_decide: z.boolean(), max_nodes: version, max_edits: version })
const impact = z.object({ goal_ids: z.array(id), contexts: version, activities: version, contents: version, evidence: version, fingerprint: z.string() })
export const structureProposal = z.object({ id, operation_id: id, status: z.enum(['open', 'applied', 'rejected', 'stale']), kind: z.enum(['edit', 'merge', 'split', 'compensate']), reason: z.string(), base_version: version, generation: version, hash: z.string(), before: z.array(structureNode), after: z.array(structureNode), impact, current_impact: impact, compensates: z.string(), applied_version: version, created_at: z.string(), decision_reason: z.string(), replayed: z.boolean() })
export const structureProposals = z.object({ items: z.array(structureProposal), next_cursor: z.string() })
export type StructureNode = z.infer<typeof structureNode>
export type ConceptContent = z.infer<typeof conceptContent>
export type StructureProposal = z.infer<typeof structureProposal>
export type StructureEdit = Pick<StructureNode, 'concept_id' | 'goal_id' | 'name' | 'content'>
export type StructureCommand = { operation_id: string; base_version: number; generation: number; kind: 'edit' | 'merge' | 'split' | 'compensate'; reason: string; edits: StructureEdit[]; compensates?: string }
export type StructureDecision = { operation_id: string; hash: string; decision: 'approve' | 'reject'; reason: string }
type Response<T> = { responses: { 200: { content: { 'application/json': T } } } }
type Post<B, T> = Response<T> & { requestBody: { content: { 'application/json': B } } }
type Path<P> = { parameters: { path: P } }
export interface StructurePaths {
  '/v1/knowledge/structure/capabilities': { get: Response<z.infer<typeof structureCapabilities>> }
  '/v1/knowledge/structure': { get: Response<z.infer<typeof structurePage>> & { parameters: { query?: { goal_id?: string; root_id?: string; search?: string; cursor?: string; limit?: number } } } }
  '/v1/knowledge/structure/concepts/{conceptID}': { get: Response<StructureNode> & { parameters: { path: { conceptID: string }; query?: { revision_id?: string } } } }
  '/v1/knowledge/structure/proposals': { get: Response<z.infer<typeof structureProposals>> & { parameters: { query?: { cursor?: string; limit?: number } } }; post: Post<StructureCommand, StructureProposal> }
  '/v1/knowledge/structure/proposals/{proposalID}': { get: Response<StructureProposal> & Path<{ proposalID: string }> }
  '/v1/knowledge/structure/proposals/{proposalID}/decisions': { post: Post<StructureDecision, StructureProposal> & Path<{ proposalID: string }> }
}
export function structureAPI(session: Session, space: string) {
  const client = () => learningClient(session, space)
  return {
    capabilities: () => unwrap(client().GET('/v1/knowledge/structure/capabilities'), structureCapabilities),
    list: (query: { goal_id?: string; root_id?: string; search?: string; cursor?: string; limit?: number }) => unwrap(client().GET('/v1/knowledge/structure', { params: { query } }), structurePage),
    node: (conceptID: string, revision_id?: string) => unwrap(client().GET('/v1/knowledge/structure/concepts/{conceptID}', { params: { path: { conceptID }, query: { revision_id } } }), structureNode),
    proposals: (cursor = '') => unwrap(client().GET('/v1/knowledge/structure/proposals', { params: { query: { cursor, limit: 20 } } }), structureProposals),
    proposal: (proposalID: string) => unwrap(client().GET('/v1/knowledge/structure/proposals/{proposalID}', { params: { path: { proposalID } } }), structureProposal),
    create: (body: StructureCommand) => unwrap(client().POST('/v1/knowledge/structure/proposals', { body }), structureProposal),
    decide: (proposalID: string, body: StructureDecision) => unwrap(client().POST('/v1/knowledge/structure/proposals/{proposalID}/decisions', { params: { path: { proposalID } }, body }), structureProposal),
  }
}

export const emptyConcept = (): ConceptContent => ({ description: '', source_status: 'candidate', suggested: false, sources: [], claims: [], relations: [], replaced_by: [] })
