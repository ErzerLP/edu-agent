import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ApiError } from '../api/client'
import { structureAPI, emptyConcept, sourceStates, learningStates, relationNames, proposalStates, type StructureNode, type StructureEdit, type StructureCommand, type StructureProposal, type ConceptContent } from '../api/structure'
import { knowledgeAPI } from '../api/references'
import { useIdentity } from '../lib/session'
import { Button } from './ui/button'
import { Confirm, ErrorState } from './common'

export function KnowledgeStructure({ spaceId, goalId, sessionId, compact = false }: { spaceId: string; goalId?: string; sessionId?: string; compact?: boolean }) {
  const { session, prefix, drafts } = useIdentity()
  const queryClient = useQueryClient(), api = structureAPI(session, spaceId)
  const key = [...prefix, spaceId, 'knowledge-structure']
  const draftKey = JSON.stringify([...key, goalId, sessionId, 'pending'])
  const [unknown, setUnknown] = useState<StructureCommand | undefined>(() => drafts.get(draftKey))
  const [search, setSearch] = useState(''), [root, setRoot] = useState(''), [cursor, setCursor] = useState(''), [graph, setGraph] = useState(false)
  const [selected, setSelected] = useState<string[]>([]), [detail, setDetail] = useState<StructureNode>(), [edits, setEdits] = useState<StructureEdit[]>([])
  const [kind, setKind] = useState<StructureCommand['kind']>('edit'), [reason, setReason] = useState(''), [busy, setBusy] = useState(false), [error, setError] = useState<unknown>(), [notice, setNotice] = useState('')
  const live = useRef(true)
  useEffect(() => { live.current = true; return () => { live.current = false } }, [])
  const caps = useQuery({ queryKey: [...prefix, 'structure-capabilities'], queryFn: () => api.capabilities(), retry: false })
  const page = useQuery({ queryKey: [...key, goalId, root, search, cursor], queryFn: () => api.list({ goal_id: goalId, root_id: root || undefined, search, cursor, limit: compact ? 8 : 30 }), enabled: caps.data?.available === true, gcTime: 0 })
  const [proposalCursor, setProposalCursor] = useState('')
  const proposals = useQuery({ queryKey: [...key, 'proposals', proposalCursor], queryFn: () => api.proposals(proposalCursor), enabled: caps.data?.available === true && !compact, gcTime: 0 })
  const refresh = async () => { if (live.current) { setCursor(''); await queryClient.invalidateQueries({ queryKey: key }) } }
  const run = async (fn: () => Promise<void>) => { if (busy) return; setBusy(true); setError(undefined); try { await fn() } catch (e) { if (live.current) setError(e) } finally { if (live.current) setBusy(false) } }
  const pending = (value?: StructureCommand) => { if (live.current) { setUnknown(value); if (value) drafts.set(draftKey, value); else drafts.delete(draftKey) } }
  const create = async (command: StructureCommand) => {
    pending(command)
    try { const result = await api.create(command); pending(); if (live.current) { setEdits([]); setSelected([]); setNotice(`提案${proposalStates[result.status]}，编号 ${result.id}`) }; await refresh() }
    catch (e) { if (e instanceof ApiError && e.status >= 400 && e.status < 500) pending(); throw e }
  }
  const submit = () => { if (!page.data) return; void run(() => create({ operation_id: crypto.randomUUID(), base_version: page.data.version, generation: page.data.generation, kind, reason, edits })) }
  const open = (id: string) => void run(async () => { const n = await api.node(id); if (live.current) setDetail(n) })
  const prepare = (action: 'edit' | 'merge' | 'split') => void run(async () => {
    const nodes = await Promise.all(selected.map(id => api.node(id)))
    if (!live.current || !nodes.length) return
    setKind(action); setReason('')
    if (action === 'edit') { setEdits(nodes.map(n => ({ concept_id: n.concept_id, goal_id: n.goal_id, name: n.name, content: structuredClone(n.content) }))); return }
    const ids = Array.from({ length: action === 'split' ? 2 : 1 }, () => crypto.randomUUID())
    const content = { ...emptyConcept(), sources: nodes.flatMap(n => n.content.sources).slice(0, 16), description: '请填写此身份的含义与适用范围；原评估不会自动继承。' }
    setEdits([...nodes.map(n => ({ concept_id: n.concept_id, goal_id: n.goal_id, name: n.name, content: { ...n.content, source_status: 'superseded' as const, replaced_by: ids } })), ...ids.map((id, i) => ({ concept_id: id, goal_id: nodes[0].goal_id, name: action === 'split' ? `${nodes[0].name}（${i + 1}）` : nodes[0].name, content: structuredClone(content) }))])
  })
  if (caps.error) return <ErrorState error={caps.error} />
  if (!caps.data?.available) return null
  return <section className={compact ? 'knowledge-structure' : 'panel knowledge-structure'} aria-label={compact ? '局部知识视图' : '动态知识结构'}>
    <h2>{compact ? '局部知识视图' : '动态知识结构'}</h2>
    <p className="hint">来源状态与学习状态分别记录。AI 建议、已纳入和关系颜色均不表示掌握。</p>
    <div className="actions"><label>搜索概念<input value={search} maxLength={200} onChange={e => { setSearch(e.target.value); setCursor('') }} /></label><Button variant="outline" onClick={() => setGraph(!graph)}>{graph ? '显示节点列表' : '显示局部关系图'}</Button>{root && <Button variant="outline" onClick={() => { setRoot(''); setCursor('') }}>退出局部节点范围</Button>}<Button variant="outline" onClick={() => void refresh()}>读取最新结构</Button></div>
    {!!(error || page.error || proposals.error) && <ErrorState error={error || page.error || proposals.error} retry={() => void refresh()} />}
    {page.data && <p role="status">结构版本 {page.data.version} · {page.data.notice}</p>}
    {notice && <p role="status">{notice}</p>}
    {unknown && <div role="alert"><p>提案提交结果未知，已保留原操作。请核对后再继续。</p><Button disabled={busy} onClick={() => void run(() => create(unknown))}>核对并重试原提案</Button></div>}
    {graph && page.data && <LocalGraph nodes={page.data.items} edges={page.data.edges} onSelect={open} />}
    <ul className="structure-list">{page.data?.items.map(n => <li key={n.concept_id}>
      {!compact && caps.data.can_propose && <label><input type="checkbox" checked={selected.includes(n.concept_id)} disabled={busy || !!unknown} onChange={e => setSelected(e.target.checked ? [...selected, n.concept_id] : selected.filter(id => id !== n.concept_id))} />选择「{n.name}」{n.concept_id.slice(0, 8)}</label>}
      <Button variant="outline" onClick={() => open(n.concept_id)}>{n.name}</Button><p>来源：{sourceStates[n.content.source_status]}{n.content.suggested ? ' · AI 建议' : ''}；学习：{learningStates[n.learning_state]}</p>
      {n.content.relations.map((e, i) => <p key={i}>{relationNames[e.kind]} → {page.data?.items.find(v => v.concept_id === e.target_id)?.name} {e.suggested ? '（建议，待核对）' : ''}</p>)}
      <Button variant="ghost" onClick={() => { setRoot(n.concept_id); setCursor('') }}>查看一跳关系</Button>
    </li>)}</ul>
    {page.data?.items.length === 0 && <p>当前授权范围没有匹配的概念。资料章节不会自动按标题合并成概念。</p>}
    {page.data?.next_cursor && <Button variant="outline" onClick={() => setCursor(page.data!.next_cursor)}>下一页节点</Button>}
    {compact && <Link to="/spaces/$spaceId/knowledge" params={{ spaceId }} search={{ goal: goalId, session: sessionId }}>查看知识、冲突与维护</Link>}
    {!compact && caps.data.can_propose && <div className="actions"><Button disabled={busy || !!unknown} onClick={() => { setKind('edit'); setReason(''); setEdits([{ concept_id: crypto.randomUUID(), goal_id: goalId ?? '', name: '', content: emptyConcept() }]) }}>新增独立概念</Button><Button disabled={busy || !!unknown || selected.length !== 1} onClick={() => prepare('edit')}>修订所选概念</Button><Button disabled={busy || !!unknown || selected.length < 2 || selected.length > 19} onClick={() => prepare('merge')}>提议合并所选概念</Button><Button disabled={busy || !!unknown || selected.length !== 1} onClick={() => prepare('split')}>提议拆分所选概念</Button></div>}
    {detail && <article className="panel compact" aria-label="概念详情"><h3>{detail.name}</h3><p className="hint">稳定身份 {detail.concept_id} · 修订 {detail.revision_id}</p><p>{detail.content.description}</p><p>来源：{sourceStates[detail.content.source_status]}；学习：{learningStates[detail.learning_state]}</p>
      {detail.support.map((s, i) => <blockquote key={i}>{s.quote}<p>研究来源 {s.source_id} · 修订 {s.revision_id} · 片段 {s.fragment_id}</p></blockquote>)}
      {detail.content.sources.map((s, i) => <blockquote key={i}>出处 {i + 1}：{s.quote}<p>集合 {s.collection_id} · 版本 {s.revision_id} · 章节 {s.node_id}</p></blockquote>)}
      {detail.content.claims.map((c, i) => <section key={i}><h4>主张 {i + 1}：{c.text}</h4><p>适用条件：{c.conditions || '尚未说明'}</p><p>具体出处：{c.sources.map(j => j + 1).join('、') || '缺少出处'}</p><p>缺口：{c.gap || '未记录'}</p></section>)}
      {detail.content.replaced_by.map(id => <Button key={id} variant="outline" onClick={() => open(id)}>查看替代身份 {id.slice(0, 8)}</Button>)}
      {detail.goal_id && <div className="actions"><Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId: detail.goal_id }}>补充这个知识点 / 请求研究</Link><Link to="/spaces/$spaceId/knowledge" params={{ spaceId }} search={{ goal: detail.goal_id, session: sessionId }}>排除本次参考 / 调整参考角色</Link></div>}
      <Button variant="ghost" onClick={() => setDetail(undefined)}>关闭详情</Button>
    </article>}
    {!!edits.length && <form className="panel" onSubmit={e => { e.preventDefault(); submit() }}><h3>维护候选：{kind === 'merge' ? '合并' : kind === 'split' ? '拆分' : '修订'}</h3><p>提交后等待审批；新身份不继承旧评估。名称相同不代表语义等价。</p><fieldset disabled={busy || !!unknown}>
      {edits.map((e, i) => <ConceptEditor key={e.concept_id} spaceId={spaceId} value={e} nodes={page.data?.items ?? []} onChange={v => setEdits(edits.map((x, j) => i === j ? v : x))} />)}
      <label>维护理由与语义依据<textarea required maxLength={2000} value={reason} onChange={e => setReason(e.target.value)} /></label><Button type="submit">提交维护提案</Button><Button type="button" variant="ghost" onClick={() => setEdits([])}>取消编辑</Button>
    </fieldset></form>}
    {!compact && <section aria-label="知识维护记录"><h3>知识维护记录</h3>{proposals.data?.items.map(p => <ProposalReview key={p.id + p.status} proposal={p} canDecide={caps.data!.can_decide} disabled={busy || !!unknown} onDecision={(decision, text) => void run(async () => { await api.decide(p.id, { operation_id: drafts.operation(`${draftKey}:${p.id}:${decision}`, { hash: p.hash, reason: text }), hash: p.hash, decision, reason: text }); await refresh() })} onRead={() => void run(async () => { const latest = await api.proposal(p.id); setNotice(`核对结果：${proposalStates[latest.status]}`); await refresh() })} onCompensate={text => { if (!page.data) return; void run(() => create({ operation_id: crypto.randomUUID(), base_version: page.data!.version, generation: page.data!.generation, kind: 'compensate', compensates: p.id, reason: text, edits: [] })) }} />)}{proposals.data?.next_cursor && <Button variant="outline" onClick={() => setProposalCursor(proposals.data!.next_cursor)}>下一页维护记录</Button>}{proposalCursor && <Button variant="ghost" onClick={() => setProposalCursor('')}>返回首批维护记录</Button>}</section>}
  </section>
}

function LocalGraph({ nodes, edges, onSelect }: { nodes: StructureNode[]; edges: { source_id: string; target_id: string; kind: keyof typeof relationNames }[]; onSelect: (id: string) => void }) {
  const point = (id: string) => { const i = nodes.findIndex(n => n.concept_id === id); return { x: 80 + i % 3 * 190, y: 45 + Math.floor(i / 3) * 100 } }
  return <div className="reference-scroll" tabIndex={0} aria-label="局部关系图；下方列表提供相同操作"><svg role="img" aria-label="当前页概念关系，无掌握颜色" viewBox={`0 0 560 ${Math.max(100, Math.ceil(nodes.length / 3) * 100)}`}>
    {edges.map((e, i) => { const a = point(e.source_id), b = point(e.target_id); return <g key={i}><line x1={a.x} y1={a.y} x2={b.x} y2={b.y} stroke="currentColor" /><text x={(a.x + b.x) / 2} y={(a.y + b.y) / 2 - 8} textAnchor="middle" fontSize="11">{relationNames[e.kind]}</text></g> })}
    {nodes.map(n => { const p = point(n.concept_id); return <g key={n.concept_id}><circle cx={p.x} cy={p.y} r="24" fill="var(--background, white)" stroke="currentColor" /><text x={p.x} y={p.y + 40} textAnchor="middle" fontSize="12">{n.name.slice(0, 12)}</text><title>{n.name}</title></g> })}
  </svg><p className="hint">方向与出处见下方关系列表。</p>{nodes.map(n => <Button key={n.concept_id} variant="ghost" onClick={() => onSelect(n.concept_id)}>图节点：{n.name}</Button>)}</div>
}

function ConceptEditor({ spaceId, value, nodes, onChange }: { spaceId: string; value: StructureEdit; nodes: StructureNode[]; onChange: (v: StructureEdit) => void }) {
  const content = value.content, set = (v: Partial<ConceptContent>) => onChange({ ...value, content: { ...content, ...v } })
  return <section className="panel compact"><p className="hint">身份 {value.concept_id}</p><label>概念名称<input required maxLength={300} value={value.name} onChange={e => onChange({ ...value, name: e.target.value })} /></label><label>含义、纠正说明与适用范围<textarea maxLength={8000} value={content.description} onChange={e => set({ description: e.target.value })} /></label>
    <label>来源状态<select value={content.source_status} disabled={content.replaced_by.length > 0} onChange={e => set({ source_status: e.target.value as ConceptContent['source_status'] })}>{Object.entries(sourceStates).filter(([key]) => key !== 'superseded' || content.replaced_by.length).map(([key, text]) => <option key={key} value={key}>{text}</option>)}</select></label>
    <p>{content.replaced_by.length > 0 ? `替代身份：${content.replaced_by.join('、')}` : '此操作不声明学习掌握度。'}</p>
    <SourcePicker spaceId={spaceId} onAdd={source => set({ sources: [...content.sources, source] })} disabled={content.sources.length >= 16} />
    {content.sources.map((s, i) => <p key={i}>出处 {i + 1}：{s.quote}<Button variant="ghost" type="button" onClick={() => set({ sources: content.sources.filter((_, j) => i !== j), claims: content.claims.map(c => ({ ...c, sources: c.sources.filter(j => j !== i).map(j => j > i ? j - 1 : j) })), relations: content.relations.map(r => ({ ...r, sources: r.sources.filter(j => j !== i).map(j => j > i ? j - 1 : j) })) })}>移除此出处</Button></p>)}
    {content.claims.map((c, i) => <fieldset key={i}><legend>具体主张 {i + 1}</legend>{(['text', 'conditions', 'gap'] as const).map(field => <label key={field}>{({ text: '主张正文', conditions: '适用条件', gap: '待补充缺口' })[field]}<textarea required={field === 'text'} maxLength={2000} value={c[field]} onChange={e => set({ claims: content.claims.map((v, j) => i === j ? { ...v, [field]: e.target.value } : v) })} /></label>)}{content.sources.map((_, j) => <label key={j}><input type="checkbox" checked={c.sources.includes(j)} onChange={e => set({ claims: content.claims.map((v, k) => i === k ? { ...v, sources: e.target.checked ? [...v.sources, j] : v.sources.filter(n => n !== j) } : v) })} />支持出处 {j + 1}</label>)}</fieldset>)}
    <Button type="button" variant="outline" disabled={content.claims.length >= 16} onClick={() => set({ claims: [...content.claims, { text: '', conditions: '', sources: [], gap: '需要补充出处与适用条件' }] })}>补充具体主张 / 冲突</Button>
    {content.relations.map((r, i) => <div className="actions" key={i}><label>关系类型<select value={r.kind} onChange={e => set({ relations: content.relations.map((v, j) => i === j ? { ...v, kind: e.target.value as typeof r.kind } : v) })}>{Object.entries(relationNames).map(([key, text]) => <option key={key} value={key}>{text}</option>)}</select></label><label>目标概念<select required value={r.target_id} onChange={e => set({ relations: content.relations.map((v, j) => i === j ? { ...v, target_id: e.target.value } : v) })}><option value="">选择明确身份</option>{!nodes.some(n => n.concept_id === r.target_id) && r.target_id && <option value={r.target_id}>{r.target_id}</option>}{nodes.filter(n => n.concept_id !== value.concept_id).map(n => <option key={n.concept_id} value={n.concept_id}>{n.name} · {n.concept_id.slice(0, 8)}</option>)}</select></label><Button type="button" variant="ghost" onClick={() => set({ relations: content.relations.filter((_, j) => i !== j) })}>移除此关系</Button></div>)}
    <Button type="button" variant="outline" disabled={content.relations.length >= 40} onClick={() => set({ relations: [...content.relations, { target_id: '', kind: 'related', suggested: false, sources: [] }] })}>添加关系修正</Button>
  </section>
}

function SourcePicker({ spaceId, onAdd, disabled }: { spaceId: string; onAdd: (v: ConceptContent['sources'][number]) => void; disabled: boolean }) {
  const { session, prefix } = useIdentity(), api = knowledgeAPI(session, spaceId)
  const [collection, setCollection] = useState(''), [node, setNode] = useState(''), [quote, setQuote] = useState('')
  const collections = useQuery({ queryKey: [...prefix, spaceId, 'collections'], queryFn: () => api.collections() })
  const selected = collections.data?.items.find(c => c.id === collection), revision = selected?.head_revision_id ?? ''
  const tree = useQuery({ queryKey: [...prefix, spaceId, collection, revision, 'structure-source-tree'], queryFn: () => api.tree(collection, revision), enabled: !!revision, gcTime: 0 })
  const options = tree.data?.revision.documents.flatMap(d => d.document.nodes.map(n => ({ ...n, document_id: d.document.document_id, path: d.path }))) ?? []
  const current = options.find(n => n.node_id === node)
  return <fieldset disabled={disabled}><legend>添加真实章节出处</legend><label>出处集合<select value={collection} onChange={e => { setCollection(e.target.value); setNode('') }}><option value="">选择当前区已关联集合</option>{collections.data?.items.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}</select></label><label>出处章节<select value={node} onChange={e => setNode(e.target.value)}><option value="">选择章节身份</option>{options.map(n => <option key={n.node_id} value={n.node_id}>{n.path} · {n.title || '全文'}</option>)}</select></label><label>原文片段<textarea maxLength={2000} value={quote} onChange={e => setQuote(e.target.value)} /></label><Button type="button" variant="outline" disabled={!current || !quote.trim()} onClick={() => { if (current) { onAdd({ collection_id: collection, revision_id: revision, document_id: current.document_id, node_id: node, quote }); setQuote('') } }}>添加出处（服务端核验原文）</Button>{(collections.error || tree.error) && <ErrorState error={collections.error || tree.error} />}</fieldset>
}

function ProposalReview({ proposal: p, canDecide, disabled, onDecision, onCompensate, onRead }: { proposal: StructureProposal; canDecide: boolean; disabled: boolean; onDecision: (d: 'approve' | 'reject', reason: string) => void; onCompensate: (reason: string) => void; onRead: () => void }) {
  const [reason, setReason] = useState('')
  return <article className="panel compact"><h4>{p.reason}</h4><p>{proposalStates[p.status]} · 基础版本 {p.base_version} · {p.id}</p><p>影响：{p.current_impact.goal_ids.length} 个目标、{p.current_impact.contexts} 个上下文、{p.current_impact.activities} 项活动、{p.current_impact.contents} 份内容、{p.current_impact.evidence} 条历史证据。</p><p className="hint">旧题、答案和冻结依据保留；不会复制 Evidence。后续课堂通过“路径与教学变更”接入。</p>{p.impact.fingerprint !== p.current_impact.fingerprint && <p role="alert">依赖已变化，原提案不能直接批准；请按当前影响重新提出。</p>}
    <details><summary>版本、差异与身份映射</summary>{p.after.map(n => { const old = p.before.find(v => v.concept_id === n.concept_id); return <section key={n.concept_id}><h5>{old?.name ?? '新身份'} → {n.name}</h5><p>身份 {n.concept_id} · {old?.revision_id ?? '无旧修订'} → {n.revision_id}</p><p>{old?.content.description} → {n.content.description}</p><p>来源：{old ? sourceStates[old.content.source_status] : '无'} → {sourceStates[n.content.source_status]}</p><pre className="reference-scroll" tabIndex={0}>{JSON.stringify({ 前: old?.content, 后: n.content }, null, 2)}</pre></section> })}</details>
    {canDecide && <><label>审阅 / 补偿理由<input value={reason} maxLength={2000} onChange={e => setReason(e.target.value)} /></label>{p.status === 'open' && <div className="actions"><Confirm label="批准知识维护" title="批准此版本的知识维护？" disabled={disabled || !reason.trim()} onConfirm={() => onDecision('approve', reason)}>将追加正式概念版本；不会认定新概念已掌握，也不会改写当前课堂。</Confirm><Button variant="outline" disabled={disabled || !reason.trim()} onClick={() => onDecision('reject', reason)}>拒绝知识维护</Button></div>}{p.status === 'applied' && <Button variant="outline" disabled={disabled || !reason.trim()} onClick={() => onCompensate(reason)}>创建补偿提案</Button>}</>}
    <Button variant="ghost" disabled={disabled} onClick={onRead}>核对原提案结果</Button>
  </article>
}
