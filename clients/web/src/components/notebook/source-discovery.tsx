import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError, errorText, learningClient, unwrap } from '@/api/client'
import { knowledgeAPI } from '@/api/references'
import {
  discoverSources,
  discoveryCapabilitiesSchema,
  discoveryRequest,
  type DiscoveryCandidate,
  type DiscoveryPolicy,
} from '@/api/source-discovery'
import { fetchedItem, type ScanItem } from '@/lib/reference-import'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'

type DiscoveryDraft = {
  keywords: string
  domains: string
  items?: DiscoveryCandidate[]
  policy?: DiscoveryPolicy
  readings: Record<string, string>
}
const costNotice = '已发出的搜索或网页请求可能产生外部费用；取消不会撤销这些费用。'

export function SourceDiscovery({
  spaceId,
  collectionId,
  onFetched,
  onReading,
}: {
  spaceId: string
  collectionId: string
  onFetched: (items: ScanItem[]) => void
  onReading: (reading: boolean) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const key = JSON.stringify([...prefix, spaceId, collectionId, 'source-discovery'])
  const [draft, setDraft] = useState<DiscoveryDraft>(
    () => drafts.get(key) ?? { keywords: '', domains: '', readings: {} },
  )
  const draftRef = useRef(draft)
  const [consent, setConsent] = useState(false)
  const [selected, setSelected] = useState<string[]>([])
  const [busy, setBusy] = useState<'search' | 'read'>()
  const [message, setMessage] = useState('')
  const [error, setError] = useState<unknown>()
  const pending = useRef<AbortController | undefined>(undefined)
  const callbacks = useRef({ onFetched, onReading })
  callbacks.current = { onFetched, onReading }
  const capabilities = useQuery({
    queryKey: [...prefix, spaceId, 'source-discovery-capabilities'],
    gcTime: 0,
    retry: false,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/knowledge/source-capabilities', {
          params: { header: { 'X-Learning-Space-ID': spaceId } },
          signal,
        }),
        discoveryCapabilitiesSchema,
      ),
  })
  useEffect(
    () => () => {
      pending.current?.abort()
      pending.current = undefined
      callbacks.current.onReading(false)
    },
    [],
  )
  const save = (change: Partial<DiscoveryDraft>) => {
    const next = { ...draftRef.current, ...change }
    draftRef.current = next
    drafts.set(key, next)
    setDraft(next)
  }
  const edit = (change: Partial<DiscoveryDraft>) => {
    save({ ...change, items: undefined, policy: undefined, readings: {} })
    setConsent(false)
    setSelected([])
    setMessage('')
    setError(undefined)
  }
  const search = async () => {
    if (pending.current) return
    setError(undefined)
    setMessage('')
    let request
    try {
      request = discoveryRequest(draft.keywords, draft.domains, consent)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : '请核对输入')
      return
    }
    const controller = new AbortController()
    pending.current = controller
    setBusy('search')
    setSelected([])
    callbacks.current.onReading(true)
    save({ items: undefined, policy: undefined, readings: {} })
    try {
      const result = await discoverSources(session, spaceId, request, controller.signal)
      if (pending.current !== controller || controller.signal.aborted) return
      save({ items: result.items, policy: request.policy, readings: {} })
      setMessage(
        result.items.length
          ? `发现 ${result.items.length} 个候选，仅有摘要。`
          : '未找到符合域名与公开网络策略的候选。',
      )
    } catch (error) {
      if (pending.current === controller && !controller.signal.aborted) setError(error)
    } finally {
      if (pending.current === controller) {
        pending.current = undefined
        setBusy(undefined)
        setConsent(false)
        callbacks.current.onReading(false)
      }
    }
  }
  const read = async () => {
    if (pending.current || !selected.length || !draft.policy) return
    const controller = new AbortController()
    pending.current = controller
    setBusy('read')
    setMessage('')
    setError(undefined)
    callbacks.current.onReading(true)
    try {
      for (const candidate of draft.items ?? []) {
        if (!selected.includes(candidate.url) || controller.signal.aborted) continue
        try {
          const source = await knowledgeAPI(session, spaceId).source(
            candidate.url,
            controller.signal,
            draft.policy,
          )
          if (pending.current !== controller || controller.signal.aborted) break
          const item = fetchedItem(source)
          callbacks.current.onFetched([item])
          save({
            readings: {
              ...draftRef.current.readings,
              [candidate.url]:
                item.status === 'ready'
                  ? '已读取正文，请在下方审阅'
                  : `未取得可导入正文：${item.reason}`,
            },
          })
        } catch (error) {
          if (pending.current !== controller || controller.signal.aborted) break
          save({
            readings: {
              ...draftRef.current.readings,
              [candidate.url]: `读取失败：${errorText(error)}`,
            },
          })
          if (error instanceof ApiError && [401, 403].includes(error.status)) break
        }
      }
      if (pending.current === controller) {
        setSelected([])
        if (!controller.signal.aborted)
          setMessage('正文读取完成，请核对解析结果；确认后才会保存为来源。')
      }
    } finally {
      if (pending.current === controller) {
        pending.current = undefined
        setBusy(undefined)
        callbacks.current.onReading(false)
      }
    }
  }
  const cancel = () => {
    pending.current?.abort()
    setMessage('已取消本次请求；已读取的正文仍需审阅确认，未完成项未导入。')
  }
  const capability = capabilities.data?.source_discovery
  return (
    <section aria-label="搜索发现">
      <h3>搜索公开来源</h3>
      <p>
        只发送你在这里填写并确认的公开关键词和域名限制。不会自动加入学习目标或学习区资料。每次最多 1
        次搜索请求、4 个候选。
      </p>
      {capabilities.isPending && <p role="status">正在核对搜索能力…</p>}
      {capabilities.error && (
        <ErrorState error={capabilities.error} retry={() => void capabilities.refetch()} />
      )}
      {capabilities.data && !capability && (
        <p role="alert">当前服务器尚未支持搜索发现，请升级服务。</p>
      )}
      {capability && !capability.available && (
        <p role="alert">
          {capability.reason === 'missing_scope'
            ? '搜索和导入需要相应权限，请重新配对。'
            : '请先在设置中配置并启用 Brave 搜索。'}{' '}
          <a href="/app/settings">打开设置</a>
        </p>
      )}
      <fieldset disabled={!!busy}>
        <legend>确认外发内容</legend>
        <label>
          公开搜索关键词
          <input
            value={draft.keywords}
            maxLength={300}
            onChange={(e) => edit({ keywords: e.target.value })}
          />
        </label>
        <label>
          域名限制（可选）
          <input
            value={draft.domains}
            maxLength={1270}
            placeholder="example.org, docs.example.org"
            onChange={(e) => edit({ domains: e.target.value })}
          />
        </label>
        <p className="hint">
          最多 5 个精确公开域名，不含协议或路径，不自动包含子域；读取正文时每次重定向也遵守此限制。
        </p>
        <p>
          将外发关键词：<strong>{draft.keywords || '（尚未填写）'}</strong>；域名限制：
          {draft.domains || '不限公开域名'}。
        </p>
        <label>
          <input type="checkbox" checked={consent} onChange={(e) => setConsent(e.target.checked)} />
          我确认只外发上述公开关键词和域名限制，并同意可能产生的外部费用
        </label>
        <Button
          variant="outline"
          disabled={!consent || !draft.keywords.trim() || !capability?.available}
          onClick={() => void search()}
        >
          确认外发并搜索
        </Button>
      </fieldset>
      {!!error && (
        <>
          <ErrorState error={error} />
          <p role="alert">搜索失败，没有生成可导入的正文。请核对配置与请求限制后重新明确发起。</p>
        </>
      )}
      {busy && (
        <>
          <p role="status">{busy === 'search' ? '正在搜索候选摘要…' : '正在读取所选网页正文…'}</p>
          <Button variant="outline" onClick={cancel}>
            取消{busy === 'search' ? '搜索' : '读取'}
          </Button>
        </>
      )}
      {message && <p role="status">{message}</p>}
      <p className="hint">{costNotice}</p>
      {!!draft.items?.length && (
        <>
          <fieldset disabled={!!busy}>
            <legend>候选摘要（不能作为正文导入或引用）</legend>
            {draft.items.map((candidate) => (
              <article key={candidate.url} className="panel compact">
                <label>
                  <input
                    type="checkbox"
                    aria-label={`选择候选：${candidate.title}`}
                    checked={selected.includes(candidate.url)}
                    disabled={draft.readings[candidate.url] === '已读取正文，请在下方审阅'}
                    onChange={(e) =>
                      setSelected((values) =>
                        e.target.checked
                          ? [...values, candidate.url]
                          : values.filter((url) => url !== candidate.url),
                      )
                    }
                  />
                  {candidate.title}
                </label>
                <p className="reference-text">{candidate.url}</p>
                <p>{candidate.summary}</p>
                <p>{draft.readings[candidate.url] ?? '摘要，未读取正文'}</p>
              </article>
            ))}
            <p>
              勾选后，点击下方按钮会将所选公开地址发送到各网站，读取正文供你审阅。摘要不会进入导入内容。
            </p>
            <Button variant="outline" disabled={!selected.length} onClick={() => void read()}>
              授权读取所选 {selected.length} 个网页正文
            </Button>
          </fieldset>
        </>
      )}
    </section>
  )
}
