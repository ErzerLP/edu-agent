import { useEffect, useRef, useState } from 'react'
import { Folder, X } from 'lucide-react'
import { sourceDescription, sourceTitle, type SourceItem } from '@/api/notebook-sources'
import { useSourceSelection } from './source-selection'
import { sourceKey } from '@/api/source-selection'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { Button } from '../ui/button'
import { AddSourceDialog } from './add-source-dialog'
import { CatalogSourceContent, MentorCitationContent } from './source-viewer'

export function SourcesPanel({
  spaceId,
  archived = false,
  addSources = false,
  onAddSourcesClose,
}: {
  spaceId: string
  archived?: boolean
  addSources?: boolean
  onAddSourcesClose?: () => void
}) {
  const { session } = useIdentity()
  const { available, capabilities, sources, selection, toggle, selectAll, citation, setCitation } =
    useSourceSelection()
  const [opened, setOpened] = useState<SourceItem>()
  const [expanded, setExpanded] = useState(false)
  const trigger = useRef<HTMLButtonElement | null>(null)
  useEffect(() => {
    if (citation) setOpened(undefined)
  }, [citation])
  if (!available) return <p role="status">当前配对没有来源读取权限，可从设置查看。</p>
  if (capabilities.error)
    return <ErrorState error={capabilities.error} retry={() => void capabilities.refetch()} />
  if (capabilities.isPending) return <p role="status">正在读取来源…</p>
  if (!capabilities.data?.source_catalog) return <p role="status">当前服务尚不支持来源列表。</p>
  const items = sources.data?.pages.flatMap((p) => p.items) ?? []
  const closeReader = () => {
    setOpened(undefined)
    setCitation(undefined)
    setExpanded(false)
    trigger.current?.focus()
  }
  return (
    <div className={`notebook-sources-content${expanded ? ' reader-expanded' : ''}`}>
      <div className="notebook-sources-list">
        {archived ? (
          <p className="hint">已归档，来源只读。</p>
        ) : session.capabilities.references ? (
          <AddSourceDialog
            spaceId={spaceId}
            initiallyOpen={addSources}
            onClose={addSources ? onAddSourcesClose : undefined}
          />
        ) : (
          <p className="hint">添加来源需要资料管理权限。</p>
        )}
        {selection && (
          <label className="notebook-source-all">
            <span>选择全部来源</span>
            <input
              type="checkbox"
              aria-label="选择全部来源"
              checked={items.length > 0 && selection.items.length === items.length}
              onChange={(e) => selectAll(e.target.checked)}
            />
          </label>
        )}
        {sources.isPending && <p role="status">正在读取来源…</p>}
        {sources.error && <ErrorState error={sources.error} retry={() => void sources.refetch()} />}
        {!sources.error && !sources.isPending && items.length === 0 && (
          <div className="notebook-empty">
            <Folder size={32} aria-hidden="true" />
            <h3>暂无来源</h3>
            <p>添加资料，开始探索。</p>
          </div>
        )}
        {!sources.error &&
          items.map((item, index) => {
            const title = sourceTitle(item)
            const kind = item.metadata?.kind ?? 'text'
            const partial = item.metadata?.status === 'partial'
            const status = item.metadata?.derived_from
              ? `笔记第 ${item.metadata.derived_from.version} 版快照`
              : partial
                ? `${kind === 'url' ? '网址' : kind === 'pdf' ? 'PDF' : '文本'} · 部分解析`
                : kind === 'text'
                  ? '用户提供的文本'
                  : kind === 'url'
                    ? '网址 · 已提取文本'
                    : item.metadata
                      ? '已提取文本'
                      : '覆盖未知'
            return (
              <article
                className={`notebook-source${opened && sourceKey(opened) === sourceKey(item) ? ' selected-source' : ''}`}
                key={sourceKey(item)}
              >
                <button
                  type="button"
                  className="notebook-source-open"
                  aria-label={`打开来源：${title}`}
                  title={`${title} · ${item.collection_name}\n${sourceDescription(item.metadata)}`}
                  onClick={(event) => {
                    trigger.current = event.currentTarget
                    setCitation(undefined)
                    setOpened(item)
                  }}
                >
                  <span className={`notebook-file-type ${kind}`} aria-hidden="true">
                    {{ pdf: 'PDF', url: 'WEB', text: 'TXT' }[kind]}
                  </span>
                  <span className="notebook-source-info">
                    <h3>
                      <span className="notebook-source-index" aria-hidden="true">
                        {index + 1}.
                      </span>
                      <span>{title}</span>
                    </h3>
                    <span
                      className={`notebook-source-status${partial ? ' partial' : ''}${item.metadata?.derived_from ? ' note' : ''}`}
                    >
                      {status}
                    </span>
                    <span className="sr-only">
                      {item.collection_name} · {item.collection_id.slice(0, 8)}
                    </span>
                  </span>
                </button>
                <input
                  type="checkbox"
                  aria-label={`选择来源：${title} · ${item.collection_name} · ${item.collection_id.slice(0, 8)}`}
                  disabled={!selection}
                  checked={selection?.items.some((s) => sourceKey(s) === sourceKey(item)) ?? false}
                  onChange={(e) => toggle(item, e.target.checked)}
                />
              </article>
            )
          })}
        {!sources.error && sources.hasNextPage && (
          <Button
            variant="outline"
            disabled={sources.isFetchingNextPage}
            onClick={() => void sources.fetchNextPage()}
          >
            更多来源
          </Button>
        )}
      </div>
      {(opened || citation) && (
        <section
          className="notebook-source-reader"
          aria-label="来源查看器"
          key={
            opened
              ? sourceKey(opened)
              : `${citation?.runId ?? citation?.artifactId}:${citation?.version}:${citation?.id}`
          }
        >
          <div className="notebook-reader-top">
            <span>原文</span>
            <Button
              variant="ghost"
              aria-label={citation ? '关闭引用片段' : '关闭来源'}
              onClick={closeReader}
            >
              <X size={16} aria-hidden="true" />
            </Button>
          </div>
          {opened ? (
            <CatalogSourceContent spaceId={spaceId} source={opened} />
          ) : citation?.source ? (
            <CatalogSourceContent spaceId={spaceId} source={citation.source} highlight />
          ) : (
            citation && (
              <MentorCitationContent
                spaceId={spaceId}
                runId={citation.runId}
                artifactId={citation.artifactId}
                version={citation.version}
                citationId={citation.id}
              />
            )
          )}
          <Button variant="ghost" onClick={() => setExpanded(!expanded)}>
            {expanded ? '收起来源宽度' : '展开来源'}
          </Button>
        </section>
      )}
    </div>
  )
}
