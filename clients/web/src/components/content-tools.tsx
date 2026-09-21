import { useEffect, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { learningClient, unwrap } from '@/api/client'
import {
  contentDiff,
  exportSchema,
  flattenBlocks,
  librarySchema,
  preferenceSchema,
} from '@/api/content'
import { contentHeader, contentSchema, type Content } from '@/api/teaching'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { ErrorState } from './common'
import { SafeMarkdown } from './content-blocks'

export function ContentTools({
  content,
  preference,
  updatePreference,
}: {
  content: Content
  preference: { favorite: boolean; pinned_version: number | null }
  updatePreference: (next: typeof preference) => Promise<void>
}) {
  const { session, drafts, prefix } = useIdentity()
  const navigate = useNavigate()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [target, setTarget] = useState('')
  const [reuseOpen, setReuseOpen] = useState(false)
  const [sourceBlock, setSourceBlock] = useState('')
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const client = () => learningClient(session, content.learning_space_id)
  const params = {
    path: { artifactID: content.artifact_id },
    header: contentHeader(content.learning_space_id),
  }
  const key = JSON.stringify([
    ...prefix,
    content.learning_space_id,
    content.artifact_id,
    'content-command',
  ])
  const previous = content.body.change?.base_version
  const baseline = useQuery({
    queryKey: [...prefix, content.learning_space_id, content.artifact_id, previous, 'diff'],
    gcTime: 0,
    enabled: !!previous,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content/{artifactID}', {
          params: { ...params, query: { version: previous } },
          signal,
        }),
        contentSchema,
      ),
  })
  const targets = useQuery({
    queryKey: [...prefix, content.learning_space_id, 'reuse-targets'],
    enabled: reuseOpen,
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content', {
          params: { header: contentHeader(content.learning_space_id), query: { limit: 50 } },
          signal,
        }),
        librarySchema,
      ),
  })
  const run = async (fn: () => Promise<void>) => {
    if (busy) return
    setBusy(true)
    setError(undefined)
    try {
      await fn()
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setBusy(false)
    }
  }
  const preferenceChange = async (next: typeof preference) => {
    const saved = await unwrap(
      client().PUT('/v1/learning/content/{artifactID}/preferences', { params, body: next }),
      preferenceSchema,
    )
    if (mounted.current) await updatePreference(saved)
  }
  const download = async (format: 'json' | 'markdown') => {
    const result = await unwrap(
      client().GET('/v1/learning/content/{artifactID}/export', {
        params: { ...params, query: { version: content.version, format } },
      }),
      exportSchema,
    )
    if (!mounted.current) return
    const url = URL.createObjectURL(new Blob([result.text], { type: result.media_type }))
    const a = document.createElement('a')
    a.href = url
    a.download = result.filename
    a.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  const restore = async () => {
    const payload = {
      expected_version: content.committed_version,
      version: content.version,
      reason: `用户恢复第 ${content.version} 版`,
    }
    const result = await unwrap(
      client().POST('/v1/learning/content/{artifactID}/restore', {
        params,
        body: { ...payload, operation_id: drafts.operation(key + ':restore', payload) },
      }),
      contentSchema,
    )
    if (!mounted.current) return
    await navigate({
      to: '/content/$artifactId',
      params: { artifactId: result.artifact_id },
      search: { space: result.learning_space_id, version: result.version },
    })
  }
  const reuse = async () => {
    const item = targets.data?.items.find((item) => item.artifact_id === target)
    if (!item || !sourceBlock) return
    const payload = {
      expected_version: item.version,
      source_artifact_id: content.artifact_id,
      source_version: content.version,
      source_block_id: sourceBlock,
      reason: '用户明确选择内容并引用到任务',
    }
    const result = await unwrap(
      client().POST('/v1/learning/content/{artifactID}/reuse', {
        params: { ...params, path: { artifactID: target } },
        body: { ...payload, operation_id: drafts.operation(key + ':reuse', payload) },
      }),
      contentSchema,
    )
    if (!mounted.current) return
    await navigate({
      to: '/content/$artifactId',
      params: { artifactId: result.artifact_id },
      search: { space: result.learning_space_id, version: result.version },
    })
  }
  const writable = session.device.scopes.includes('learning:write')
  return (
    <section aria-label="内容管理与版本变化">
      <div className="actions">
        <Button
          variant="outline"
          disabled={busy || !writable}
          onClick={() =>
            void run(() => preferenceChange({ ...preference, favorite: !preference.favorite }))
          }
        >
          {preference.favorite ? '取消收藏' : '收藏内容'}
        </Button>
        <Button
          variant="outline"
          disabled={busy || !writable || content.status !== 'committed'}
          onClick={() =>
            void run(() =>
              preferenceChange({
                ...preference,
                pinned_version:
                  preference.pinned_version === content.version ? null : content.version,
              }),
            )
          }
        >
          {preference.pinned_version === content.version ? '取消固定阅读版本' : '固定当前阅读版本'}
        </Button>
        <Button
          variant="outline"
          disabled={busy}
          onClick={() => void run(() => download('markdown'))}
        >
          导出 Markdown
        </Button>
        <Button variant="outline" disabled={busy} onClick={() => void run(() => download('json'))}>
          导出结构化内容
        </Button>
        {content.version !== content.committed_version && content.status === 'committed' && (
          <Button disabled={busy || !writable} onClick={() => void run(restore)}>
            恢复为新的补偿版本
          </Button>
        )}
      </div>
      <p className="hint">固定版本只影响阅读；恢复只追加内容版本，不撤销学习行为或迁移评分。</p>
      {content.body.change && (
        <section aria-label="本版变化">
          <h2>本版变化</h2>
          <p>
            {content.body.change.reason} · 采用时间：
            {new Date(content.created_at).toLocaleString('zh-CN')}
          </p>
          {baseline.error && <ErrorState error={baseline.error} />}
          {baseline.data &&
            contentDiff(baseline.data.body.blocks, content.body.blocks).map((change) => (
              <details key={change.id} open>
                <summary>
                  {change.before ? (change.after ? '修改内容块' : '移除内容块') : '附加内容块'}
                </summary>
                <div className="content-diff">
                  <section>
                    <h3>旧版</h3>
                    {change.before ? (
                      <SafeMarkdown text={change.before.text ?? change.before.fallback} />
                    ) : (
                      <p>无</p>
                    )}
                  </section>
                  <section>
                    <h3>新版</h3>
                    {change.after ? (
                      <SafeMarkdown text={change.after.text ?? change.after.fallback} />
                    ) : (
                      <p>无</p>
                    )}
                  </section>
                </div>
              </details>
            ))}
        </section>
      )}
      <details onToggle={(e) => setReuseOpen(e.currentTarget.open)}>
        <summary>引用到另一个任务</summary>
        <p className="hint">
          明确选择当前学习区内的目标任务后，附加所选内容；保留原始来源链，不作为独立外部证据。
        </p>
        <label>
          要引用的内容块
          <select value={sourceBlock} onChange={(e) => setSourceBlock(e.target.value)}>
            <option value="">选择内容块</option>
            {flattenBlocks(content.body.blocks)
              .filter((b) => b.text)
              .map((b) => (
                <option key={b.block_id} value={b.block_id}>
                  {b.text?.slice(0, 70)}
                </option>
              ))}
          </select>
        </label>
        <label>
          目标任务
          <select value={target} onChange={(e) => setTarget(e.target.value)}>
            <option value="">选择目标任务</option>
            {targets.data?.items
              .filter(
                (item) =>
                  item.artifact_id !== content.artifact_id && item.source_status !== 'restricted',
              )
              .map((item) => (
                <option key={item.artifact_id} value={item.artifact_id}>
                  {item.title} · 第 {item.version} 版
                </option>
              ))}
          </select>
        </label>
        {targets.error && <ErrorState error={targets.error} />}
        <Button
          disabled={busy || !writable || !target || !sourceBlock || content.status !== 'committed'}
          onClick={() => void run(reuse)}
        >
          授权引用到所选任务
        </Button>
      </details>
      {!!error && <ErrorState error={error} />}
    </section>
  )
}
