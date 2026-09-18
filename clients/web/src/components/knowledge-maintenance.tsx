import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { z } from 'zod'
import { ApiError, learningClient, unwrap } from '../api/client'
import type { components } from '../api/schema'
import { knowledgeAPI } from '../api/references'
import { structureAPI } from '../api/structure'
import { useIdentity } from '../lib/session'
import { Button } from './ui/button'
import { Confirm, ErrorState } from './common'

const proposal = z.object({ proposal_id: z.uuid(), status: z.enum(['open', 'applied', 'rejected', 'stale', 'redacted']), base_revision_id: z.uuid(), applied_revision_id: z.uuid().optional(), candidate_snapshot: z.array(z.object({ path: z.string(), markdown: z.string() })).optional(), diff: z.array(z.object({ document_id: z.uuid(), kind: z.string(), unified_diff: z.string().optional(), truncated: z.boolean() })).optional(), risk: z.object({ level: z.string(), reasons: z.array(z.string()), auto_apply: z.boolean() }), accepted_learning_evidence_impact: z.object({ count: z.number() }) })
const pageSchema = z.object({ items: z.array(proposal), next_cursor: z.string().optional() })
const documents = z.array(z.object({ path: z.string().min(1), markdown: z.string() })).max(1000)
const defaultSpace = '00000000-0000-4000-8000-000000000001', defaultCollection = '00000000-0000-4000-8000-000000000002'
const statusNames = { open: '待审阅', applied: '已应用', rejected: '已拒绝', stale: '已过期', redacted: '已清除' }

// 文档和章节维护继续走已有服务，不通过概念接口覆盖原文或推断身份。
export function KnowledgeMaintenance({ spaceId }: { spaceId: string }) {
  const { session, prefix, drafts } = useIdentity()
  const client = () => learningClient(session, spaceId)
  const [cursor, setCursor] = useState(''), [base, setBase] = useState(''), [candidate, setCandidate] = useState(''), [target, setTarget] = useState(''), [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false), [error, setError] = useState<unknown>(), [notice, setNotice] = useState('')
  const key = JSON.stringify([...prefix, spaceId, 'document-maintenance'])
  const [pending, setPending] = useState<components['schemas']['KnowledgeMaintenanceProposalRequest'] | undefined>(() => drafts.get(key))
  const allowed = spaceId === defaultSpace
  const caps = useQuery({ queryKey: [...prefix, 'structure-capabilities'], queryFn: () => structureAPI(session, spaceId).capabilities(), retry: false })
  const proposals = useQuery({ queryKey: [...prefix, spaceId, 'document-maintenance', cursor], enabled: allowed, gcTime: 0, queryFn: () => unwrap(client().GET('/v1/knowledge/maintenance/proposals', { params: { query: { cursor: cursor || undefined, limit: 20 } } }), pageSchema) })
  const run = async (fn: () => Promise<void>) => { if (busy) return; setBusy(true); setError(undefined); try { await fn(); await proposals.refetch() } catch (e) { setError(e) } finally { setBusy(false) } }
  const source = async () => ({ kind: 'note' as const, locator: '浏览器知识维护审阅', excerpt: reason, sha256: Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(reason)))).map(v => v.toString(16).padStart(2, '0')).join('') })
  const create = async (request: components['schemas']['KnowledgeMaintenanceProposalRequest']) => {
    setPending(request); drafts.set(key, request)
    try {
      const p = await unwrap(client().POST('/v1/knowledge/maintenance/proposals', { body: request }), proposal)
      setPending(undefined); drafts.delete(key); setNotice(`文档提案 ${p.proposal_id}：${statusNames[p.status]}`)
    } catch (e) { if (e instanceof ApiError && e.status >= 400 && e.status < 500) { setPending(undefined); drafts.delete(key) }; throw e }
  }
  if (!allowed) return <p className="hint">当前区可独立维护概念；既有文档/章节维护与 NoteSync 仍使用默认区的默认集合映射。此处不借用其他区身份操作原文。</p>
  return <section className="panel" aria-label="既有文档与章节维护"><h2>文档与章节维护</h2><p>保留原身份审阅、共享引用和证据继承规则。候选是完整快照，遗漏文档表示移除。既有低风险政策可能在创建时直接应用，请先核对。</p>
    {caps.data?.can_decide && <label>文档维护 / 审阅理由<input value={reason} maxLength={2000} onChange={e => setReason(e.target.value)} /></label>}
    {!!(error || proposals.error) && <ErrorState error={error || proposals.error} />}{notice && <p role="status">{notice}</p>}
    {caps.data?.can_decide && <><Button disabled={busy || !!pending} variant="outline" onClick={() => void run(async () => { const collections = await knowledgeAPI(session, spaceId).collections(); const head = collections.items.find(c => c.id === defaultCollection)?.head_revision_id; if (!head) { setNotice('默认集合尚无版本。'); return }; const exported = await knowledgeAPI(session, spaceId).export(defaultCollection, head); setBase(head); setCandidate(JSON.stringify(exported.documents.map(d => ({ path: d.path, markdown: d.markdown })), null, 2)) })}>载入默认集合完整候选</Button>
      {base && <><p>基础版本：{base}</p><label>完整文档快照（保留身份标记）<textarea rows={12} value={candidate} disabled={busy || !!pending} onChange={e => setCandidate(e.target.value)} /></label><Confirm label="创建文档维护提案" title="已核对完整快照及既有自动应用政策？" disabled={busy || !!pending || !reason.trim()} onConfirm={() => void run(async () => create({ request_id: crypto.randomUUID(), base_revision_id: base, sources: [await source()], candidate_snapshot: documents.parse(JSON.parse(candidate)) }))}>删除、移动、合并和拆分沿用原身份审阅；不确定身份会保留在待审阅提案中。</Confirm><label>回滚目标（历史祖先版本）<input value={target} onChange={e => setTarget(e.target.value)} /></label><Button variant="outline" disabled={busy || !!pending || !reason.trim() || !z.uuid().safeParse(target).success} onClick={() => void run(async () => { const body = { base_revision_id: base, target_revision_id: target, sources: [await source()] }; const p = await unwrap(client().POST('/v1/knowledge/maintenance/rollbacks', { body: { ...body, request_id: drafts.operation(`${key}:rollback`, body) } }), proposal); setNotice(`回滚提案 ${p.proposal_id}：${statusNames[p.status]}`) })}>创建正式回滚提案</Button></>}
      {pending && <div role="alert"><p>提交结果尚未核对，原候选和操作身份已保留。</p><Button disabled={busy} onClick={() => void run(() => create(pending))}>核对原文档提案</Button></div>}</>}
    {proposals.data?.items.map(p => <article className="panel compact" key={p.proposal_id}><h3>文档提案 {p.proposal_id}</h3><p>状态 {statusNames[p.status]} · 风险 {p.risk.level} · 影响 {p.accepted_learning_evidence_impact.count} 条历史证据</p><details><summary>文档差异与风险依据</summary><p>{p.risk.reasons.join('；')}</p>{p.diff?.map((d, i) => <section key={i}><p>{d.kind}{d.truncated && ' · 差异已裁剪，请核对完整候选'}</p><pre className="reference-scroll" tabIndex={0}>{d.unified_diff}</pre></section>)}<pre className="reference-scroll" tabIndex={0}>{JSON.stringify(p.candidate_snapshot, null, 2)}</pre></details>
      <Button variant="ghost" disabled={busy} onClick={() => void run(async () => { const fresh = await unwrap(client().GET('/v1/knowledge/maintenance/proposals/{proposalID}', { params: { path: { proposalID: p.proposal_id } } }), proposal); setNotice(`核对文档提案：${statusNames[fresh.status]}`) })}>读取文档提案详情 / 核对结果</Button>
      {p.status === 'open' && caps.data?.can_decide && <>{(['approve', 'reject'] as const).map(decision => <Confirm key={decision} label={decision === 'approve' ? '批准文档提案' : '拒绝文档提案'} title="确认按原服务审阅此提案？" disabled={busy || !reason.trim()} onConfirm={() => void run(async () => { const path = decision === 'approve' ? '/v1/knowledge/maintenance/proposals/{proposalID}/approve' : '/v1/knowledge/maintenance/proposals/{proposalID}/reject'; await unwrap(client().POST(path, { params: { path: { proposalID: p.proposal_id } }, body: { operation_id: drafts.operation(`${key}:${p.proposal_id}:${decision}`, reason), reason } }), proposal) })}>版本变化会使旧提案过期；旧答案和评估依据保留。</Confirm>)}</>}
    </article>)}{proposals.data?.next_cursor && <Button onClick={() => setCursor(proposals.data!.next_cursor!)}>下一页文档提案</Button>}
  </section>
}
