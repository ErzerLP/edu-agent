import { z } from 'zod'
import { learningClient, unwrap } from './client'
import type { Session } from './runtime'
import type { components } from './schema'
import { pdfMetadataSchema, pdfReportSchema } from './pdf'

const id = z.uuid()
const count = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
export const collectionSchema = z.object({ id, name: z.string(), source: z.string(), shared: z.boolean(), head_revision_id: id.nullable(), version: count })
export type Collection = z.infer<typeof collectionSchema>
export const collectionsSchema = z.object({ items: z.array(collectionSchema).nullable().transform(v => v ?? []) })
export const entrySchema = z.object({ collection_id: id, revision_id: id, document_id: id.optional(), node_id: id.optional() })
export const roleSchema = z.enum(['supplement', 'prefer', 'restrict'])
export const referenceEntry = entrySchema.extend({ role: roleSchema })
export type ReferenceEntry = z.infer<typeof referenceEntry>
export const selectionSchema = z.object({ session_id: z.string(), entries: z.array(referenceEntry) })
export const referenceState = z.object({ version: count, context_id: z.string(), scope_snapshot_id: z.string(), selection: selectionSchema })
export const referencePreview = z.object({ receipt: z.string().min(1), before: referenceState, after: selectionSchema, requires_scope_confirmation: z.boolean(), impact: z.string() })
export type ReferencePreview = z.infer<typeof referencePreview>
export type ReferenceRequest = { operation_id: string; expected_goal_version: number; selection: z.infer<typeof selectionSchema> }
export const roleNames = { supplement: '补充参考', prefer: '优先依据', restrict: '限制范围' }
const node = z.object({ node_id: id, node_revision_id: id, title: z.string(), heading_level: count, section_range: z.object({ start: count, end: count }) })
export const treeSchema = z.object({ revision: z.object({ revision_id: id, parent_revision_id: id.nullable(), revision_no: count, documents: z.array(z.object({ path: z.string(), collection_id: id.optional(), document: z.object({ document_id: id, document_revision_id: id, nodes: z.array(node), pdf: pdfMetadataSchema.optional() }) })).default([]) }) })
export const exportSchema = z.object({ revision_id: id, documents: z.array(z.object({ path: z.string(), markdown: z.string(), collection_id: id.optional() })) })
export const scopeSchema = z.object({ id, space_id: id, entries: z.array(entrySchema), updates: z.array(entrySchema).optional() })
export const summarySchema = z.object({ operation_id: id, space_id: id, collection_id: id, actor_device_id: id, document_ids: z.array(id).nullable().transform(v => v ?? []), added: count, updated: count, unchanged: count })
const candidate = z.object({ stable_id: id, revision_id: id, reason_code: z.string() })
const reviewItem = z.object({ path: z.string(), locator: z.string(), reason_code: z.string(), candidates: z.array(candidate).nullable().transform(v => v ?? []) })
export const importPreviewSchema = z.object({
  status: z.enum(['ready', 'review']), receipt: z.string().optional(), summary: summarySchema,
  diff: z.array(z.object({ document_id: id, before_path: z.string().optional(), after_path: z.string().optional(), kind: z.string(), unified_diff: z.string().optional(), truncated: z.boolean() })),
  before: z.array(z.object({ path: z.string(), markdown: z.string() })).optional(),
  identity_review: z.object({ identity_review_basis_hash: z.string(), identity_review_operation_id: id, identity_review_receipt: z.string(), document_reviews: z.array(reviewItem).nullable().transform(v => v ?? []), node_reviews: z.array(reviewItem.extend({ preorder: count })).nullable().transform(v => v ?? []) }).optional(),
  affected_evidence: count, impact_known: z.boolean(),
}).refine(v => v.status === 'ready' ? !!v.receipt : !!v.identity_review, '缺少确认或身份审阅回执')
export const importResultSchema = z.object({ summary: summarySchema, revision: z.object({ revision_id: id, revision_no: count }), unchanged: z.boolean(), replayed: z.boolean().optional() })
export type ImportRequest = components['schemas']['KnowledgeImportRequest']
export type ImportPreview = z.infer<typeof importPreviewSchema>
export type ImportResult = z.infer<typeof importResultSchema>
export const sourceSchema = z.object({ locator: z.string(), final_url: z.string(), title: z.string(), kind: z.string(), status: z.string(), failure: z.string(), fingerprint: z.string(), parser: z.string(), coverage: z.string(), storage_allowed: z.boolean(), text: z.string(), pdf: pdfReportSchema.optional(), pdf_data: z.string().optional(), source_receipt: z.string().optional() })

type Response<T> = { responses: { 200: { content: { 'application/json': T } } } }
type Post<B, R> = Response<R> & { requestBody: { content: { 'application/json': B } } }
type GoalParams = { parameters: { path: { goalID: string }; query?: { session_id?: string } } }
export interface ReferencePaths {
  '/v1/knowledge/reference-sources': { post: Post<{ url?: string; external_consent?: boolean; pdf_data?: string; storage_consent?: boolean }, z.infer<typeof sourceSchema>> }
  '/v1/learning/goals/{goalID}/references': { get: GoalParams & Response<z.infer<typeof referenceState>> }
  '/v1/learning/goals/{goalID}/references/previews': { post: GoalParams & Post<ReferenceRequest, ReferencePreview> }
  '/v1/learning/goals/{goalID}/references/confirm': { post: GoalParams & Post<{ request: ReferenceRequest; receipt: string; confirm_scope: boolean }, z.infer<typeof referenceState>> }
  '/v1/learning/goals/{goalID}/references/operations/{operationID}': { get: { parameters: { path: { goalID: string; operationID: string } } } & Response<z.infer<typeof referenceState>> }
}

export function knowledgeAPI(session: Session, space: string) {
  const client = () => learningClient(session, space)
  const header = (collection: string) => ({ 'X-Learning-Space-ID': space, 'X-Knowledge-Collection-ID': collection })
  return {
    collections: (shared = false) => unwrap(client().GET('/v1/knowledge/collections', { params: { query: { shared }, header: { 'X-Learning-Space-ID': space } } }), collectionsSchema),
    collection: (body: components['schemas']['KnowledgeCollectionCommand']) => unwrap(client().POST('/v1/knowledge/collections', { params: { header: { 'X-Learning-Space-ID': space } }, body }), collectionSchema),
    tree: (collection: string, revision: string) => unwrap(client().GET('/v1/knowledge/revisions/{revisionID}/tree', { params: { path: { revisionID: revision }, header: header(collection) } }), treeSchema),
    export: (collection: string, revision: string) => unwrap(client().GET('/v1/knowledge/revisions/{revisionID}/export', { params: { path: { revisionID: revision }, header: header(collection) } }), exportSchema),
    scope: (scopeID: string) => unwrap(client().GET('/v1/knowledge/scopes/{scopeID}', { params: { path: { scopeID }, header: { 'X-Learning-Space-ID': space } } }), scopeSchema),
    scopeExport: (scopeID: string) => unwrap(client().GET('/v1/knowledge/scopes/{scopeID}/export', { params: { path: { scopeID }, header: { 'X-Learning-Space-ID': space } } }), exportSchema),
    preview: (collection: string, body: ImportRequest) => unwrap(client().POST('/v1/knowledge/imports/previews', { params: { header: header(collection) }, body }), importPreviewSchema),
    confirm: (collection: string, request: ImportRequest, receipt: string) => unwrap(client().POST('/v1/knowledge/imports/confirm', { params: { header: header(collection) }, body: { request, receipt } }), importResultSchema),
    operation: (collection: string, operationID: string) => unwrap(client().GET('/v1/knowledge/imports/operations/{operationID}', { params: { path: { operationID }, header: header(collection) } }), importResultSchema),
    source: (url: string) => unwrap(client().POST('/v1/knowledge/reference-sources', { body: { url, external_consent: true } }), sourceSchema),
    pdf: (data: string) => unwrap(client().POST('/v1/knowledge/reference-sources', { body: { pdf_data: data, storage_consent: true } }), sourceSchema),
  }
}
