import { useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { z } from 'zod'
import { useIdentity } from './lib/session'
import { ApiError, loseIdentity } from './api/client'
import { memoryAPI, memoryAccessLost, stepNames, type ErasureReceipt } from './api/memory'
import { Confirm, ErrorState } from './components/common'
import { Button } from './components/ui/button'

const stores: Record<string, string> = {
  identity_metadata: '设备可识别标签与会话', knowledge_content: '资料、引用与来源正文', knowledge_index: '资料索引', knowledge_artifacts: '资料加工与导入产物',
  learning_event_payload: '学习事件正文', learning_typed_payload: '学习记录、导师运行/聊天/checkpoint、内容版本', tutoring_payload: '教学正文',
  inbox_outbox: '收发队列正文', projection_generations: '全部学习投影', memory_candidate_delivery: '记忆候选与交付正文',
  offline_device_cache: '离线设备缓存', process_cache: '服务进程缓存', nocturne_paths: 'Nocturne 活动路径', nocturne_orphan_history: 'Nocturne 历史版本',
  nocturne_snapshot_changeset: 'Nocturne 快照与变更引用', managed_backup: '受管理备份', external_model_provider: '外部模型提供商', operator_backup: '操作者备份',
}
const erasureNames: Record<ErasureReceipt['status'], string> = { barrier_committed: '隐私屏障已提交，清除待完成', local_scrubbed: '本地清理完成，其他步骤待验证', remote_draining: '等待在途远端操作结束', remote_purged: '远端已清理，等待最终验证', verified: '受管理活动存储已验证', partial: '仅部分完成，存在未完成步骤', blocked: '清除受阻，请联系操作者' }
export const dataSearch = z.object({ erasure: z.uuid().optional(), operation: z.uuid().optional(), device: z.uuid().optional() })

export function ErasureStatus({ receipt }: { receipt: ErasureReceipt }) {
  return <section className="panel" aria-label="隐私清除回执"><h2>隐私清除回执</h2><p role="status">{erasureNames[receipt.status]}</p>
    <p>回执 ID：{receipt.erasure_id} · 隐私代次 {receipt.learner_generation}</p>
    <p>更新于 {new Date(receipt.updated_at).toLocaleString('zh-CN')}。只有正式回执代表服务端进展，浏览器清缓存不代表服务端已清除。</p>
    <ul>{receipt.steps.map(step => <li key={step.store}><strong>{stores[step.store] ?? step.store}：{stepNames[step.status]}</strong><p>{step.stable_reason} · {step.verification_method}</p>{step.completed_at && <p>{new Date(step.completed_at).toLocaleString('zh-CN')}</p>}</li>)}</ul>
    <p>外部提供商留存、自行导出、终端日志、WAL、宿主快照和非受管理备份不在物理擦除保证内。“已验证”不表示这些位置全部清空。模型/搜索 Key 需在配置页另行清除和撤销。</p>
  </section>
}

export function DataPage({ search }: { search: z.infer<typeof dataSearch> }) {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const grant = useRef<HTMLInputElement>(null)
  const [hasGrant, setHasGrant] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [receipt, setReceipt] = useState<ErasureReceipt>()
  const [operation, setOperation] = useState(() => search.operation && search.device ? { id: search.operation, device: search.device } : undefined)
  const read = session.device.scopes.includes('privacy:read')
  const query = useQuery({ queryKey: [...prefix, 'privacy-receipt', search.erasure, operation], enabled: read && !pending && (!!search.erasure || !!operation), gcTime: 0,
    queryFn: ({ signal }) => search.erasure ? memoryAPI(session, signal).erasure(search.erasure) : memoryAPI(session, signal).operation(operation!.id, operation!.device),
    refetchInterval: q => q.state.data && q.state.data.status !== 'verified' ? 5000 : false })
  const erase = async () => {
    if (pending || !grant.current?.value || operation) return
    const token = grant.current.value
    grant.current.value = ''; setHasGrant(false)
    const op = { id: crypto.randomUUID(), device: session.device.id }
    // 地址只保留原操作身份，用于清除失效会话后的重新配对与丢响应核对。
    setOperation(op); setPending(true); setError(undefined)
    try {
      await navigate({ to: '/settings/data', search: { operation: op.id, device: op.device }, replace: true })
      const value = await memoryAPI(session).erase({ operation_id: op.id, payload_schema_version: 1, expected_current_learner_generation: session.generation, reason_code: 'learner_request', explicit_confirmation: true }, token)
      setReceipt(value)
    } catch (e) {
      setError(e)
      if (e instanceof ApiError && [400, 403, 429].includes(e.status)) {
        setOperation(undefined); await navigate({ to: '/settings/data', search: {}, replace: true })
      }
    } finally { setPending(false) }
  }
  const value = memoryAccessLost(query.error) ? undefined : query.data ?? receipt
  return <><section className="intro"><h1>数据与隐私</h1><p>每个动作有独立范围和回执。清理本机缓存不会删除服务端数据。</p><a href="/app/settings/devices">设备与当前会话</a></section>
    <section className="panel"><h2>保存与删除的边界</h2><dl className="memory-metadata">
      <dt>临时聊天</dt><dd>不持久保存聊天正文；正式作答、学习事实和另行批准的记忆仍由各自服务保存。</dd>
      <dt>删除聊天</dt><dd>删除选定导师对话的正文与运行缓存；不删除目标、正式学习事实、已采用资料或长期记忆。<a href="/app/">从目标打开导师历史</a></dd>
      <dt>归档目标</dt><dd>改变生命周期和导航可见性，可恢复；不清除聊天、学习事实或记忆。</dd>
      <dt>移除资料引用</dt><dd>解除选定目标或学习区的引用，不等于删除共享原件、历史冻结来源或外部 NoteSync 笔记。</dd>
      <dt>清除长期记忆</dt><dd>在记忆页逐条确认删除，复用远端删除和验证回执；不删除聊天、学习记录或自己下载的副本。<a href="/app/memory">管理长期记忆</a></dd>
      <dt>全局隐私清除</dt><dd>不可逆地关闭旧代次，清除各 owner 的活动正文、运行、聊天、checkpoint、引用及索引；保留最小无正文审计和回执。旧浏览器会话失效，远端或 owner 失败保持可见，不能宣称全清。</dd>
    </dl></section>
    <section className="panel"><h2>发起服务端隐私清除</h2><p>影响全部学习区及全局记忆。需要操作者在本机为当前设备签发短时一次性 privacy-grant；Web 无权自行签发，不提供 admin 配对管理。</p>
      <p>当前设备 ID：{session.device.id}</p>
      <p>发起前保留本页地址。清除后需要重新配对，再通过该地址查询原回执；丢失响应时先核对原操作，不重复发起。</p>
      {!read && <p>当前身份没有 privacy:read，无法在清除后核对回执；请先使用 memory 档案重新配对。</p>}
      {!operation && !search.erasure && <><label>一次性清除授权<input ref={grant} type="password" autoComplete="off" maxLength={256} disabled={!read || pending} onChange={e => setHasGrant(!!e.target.value)} /></label>
        <Confirm label="发起全局隐私清除" title="不可逆地清除全部学习区的服务端私人数据？" disabled={!read || !hasGrant || pending} onConfirm={() => void erase()}>
          所有旧学习正文、聊天、运行和长期记忆将进入不可逆清除。当前会话会失效。失败不回滚屏障，远端/备份状态以逐项回执为准。取消不会发送请求。
        </Confirm></>}
      {pending && <p role="status">正在提交清除；请等待或用原操作地址核对。</p>}
      {operation && <p>原操作 ID：{operation.id}。{!value && '尚未取得完成回执，结果未知。重新配对后继续核对。'}</p>}
      {!!error && <ErrorState error={error} />}
      {query.error && <ErrorState error={query.error} />}
      {(operation || search.erasure) && <Button variant="outline" disabled={!read || pending} onClick={() => void query.refetch()}>核对原清除回执</Button>}
    </section>{value && <ErasureStatus receipt={value} />}</>
}

export function DevicesPage() {
  const { session, prefix } = useIdentity()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [message, setMessage] = useState('')
  const read = session.device.scopes.includes('devices:read')
  const manage = session.device.scopes.includes('devices:manage') && session.device.scopes.includes('memory:web')
  const list = useQuery({ queryKey: [...prefix, 'devices'], queryFn: ({ signal }) => memoryAPI(session, signal).devices(), enabled: read, gcTime: 0 })
  const revoke = async (id: string) => {
    if (pending) return
    setPending(true); setError(undefined); setMessage('')
    try {
      await memoryAPI(session).revoke(id)
      if (id === session.device.id) { loseIdentity(session); return }
      setMessage('设备已撤销，后续写入和活动订阅的实时权限检查立即失效。')
      await list.refetch()
    } catch (e) { setError(e) }
    finally { setPending(false) }
  }
  return <><section className="intro"><h1>设备与当前 Web 会话</h1><a href="/app/settings/data">数据与隐私</a></section>
    <section className="panel"><h2>当前会话</h2><p>{session.device.display_name} · {session.device.id}</p><p>有效至 {new Date(session.expires_at).toLocaleString('zh-CN')}；隐私代次 {session.generation}。</p>
      <p>退出只结束当前浏览器会话；撤销设备使该设备凭据与活动订阅失效，不删除学习数据。不显示 Cookie 或 Token。</p>
      <p>普通学习设备无本机 admin、配对码签发或通用管理权限。撤销需要操作者明确授予的 memory 档案和 devices:manage。</p>
    </section><section className="panel"><h2>已配对设备</h2>
      {!read && <p>当前身份没有设备列表权限。</p>}
      {list.error && <ErrorState error={list.error} retry={() => void list.refetch()} />}{!!error && <ErrorState error={error} />}
      {!memoryAccessLost(list.error) && list.data?.devices.map(device => <article key={device.id}><h3>{device.display_name || '已清除设备标签'}{device.id === session.device.id ? '（当前）' : ''}</h3><p>{device.id}</p><p>{device.revoked_at ? `已撤销：${new Date(device.revoked_at).toLocaleString('zh-CN')}` : '有效设备'}</p>
        <Confirm label="撤销设备" title={`撤销${device.id === session.device.id ? '当前浏览器设备' : device.display_name}？`} disabled={!manage || pending || !!device.revoked_at} onConfirm={() => void revoke(device.id)}>
          目标设备：{device.id}。其新写入、读取和活动订阅将失效。需要新配对才能再次访问；不会删除学习内容。取消不撤销。
        </Confirm></article>)}
      {message && <p role="status">{message}</p>}
      <Button variant="outline" disabled={!read || pending} onClick={() => void list.refetch()}>刷新设备列表</Button>
    </section></>
}
