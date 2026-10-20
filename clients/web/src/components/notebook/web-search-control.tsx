import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { learningClient, unwrap } from '@/api/client'
import type { TutorConversation } from '@/api/conversations'
import { webPreferenceSchema } from '@/api/web-search'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'

export function WebSearchControl({ spaceId, conversation, enabled = true, onNewChange }: {
  spaceId: string; conversation?: TutorConversation; enabled?: boolean; onNewChange?: (enabled: boolean) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const [pending, setPending] = useState<boolean>()
  const [confirmed, setConfirmed] = useState<{ enabled: boolean; web_search_version: number }>()
  const [error, setError] = useState<unknown>()
  const current = conversation && confirmed && confirmed.web_search_version >= conversation.web_search_version ? confirmed.enabled : conversation?.web_search_enabled ?? enabled
  const change = async (next: boolean) => {
    if (!conversation) { onNewChange?.(next); return }
    setPending(next); setError(undefined)
    const body = { enabled: next, expected_web_search_version: Math.max(conversation.web_search_version, confirmed?.web_search_version ?? 0) }
    try {
      const result = await unwrap(learningClient(session, spaceId).PATCH('/v1/learning/conversations/{conversationID}/web-search', {
        params: { header: { 'X-Learning-Space-ID': spaceId }, path: { conversationID: conversation.id } },
        body: { ...body, operation_id: drafts.operation(JSON.stringify([...prefix, conversation.id, 'web-search']), body) },
      }), webPreferenceSchema)
      setConfirmed(result)
      await query.invalidateQueries({ predicate: item => item.queryKey.includes(spaceId) || item.queryKey.includes(conversation.id) })
    } catch (e) { setError(e); await query.invalidateQueries({ predicate: item => item.queryKey.includes(spaceId) || item.queryKey.includes(conversation.id) }) } finally { setPending(undefined) }
  }
  return <div className="web-search-control">
    <label><input type="checkbox" checked={current} disabled={pending !== undefined} onChange={e => void change(e.target.checked)} />联网搜索</label>
    <span role="status">{pending === false ? '正在关闭…' : pending === true ? '正在开启…' : error ? '设置尚未确认，请核对后重试。' : current ? '' : '已关闭联网搜索'}</span>
    {conversation && !conversation.web_search_available && <span>{conversation.web_search_reason || '搜索服务不可用'} · <Link to="/settings">前往设置</Link></span>}
    <p className="hint">导师可能搜索网页。问题中的检索词会发送给搜索服务，网页由服务器读取。关闭后不再联网，正常回答仍会使用你设置的模型服务。</p>
    <details><summary>联网与保存说明</summary><p>检索内容和读到的网页会随本次任务暂存；加入来源需你确认。已经发出的请求无法撤回。删除对话不会删除已确认加入的来源。</p></details>
    {!!error && <ErrorState error={error} />}
  </div>
}
