import { Link } from '@tanstack/react-router'
import { Folder } from 'lucide-react'
import { sourceDescription } from '@/api/notebook-sources'
import { useSourceSelection } from './source-selection'
import { sourceKey } from '@/api/source-selection'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { Button } from '../ui/button'
import { AddSourceDialog } from './add-source-dialog'
import { CatalogSourceViewer, MentorCitationContent } from './source-viewer'

export function SourcesPanel({
  spaceId,
  archived = false,
}: {
  spaceId: string
  archived?: boolean
}) {
  const { session } = useIdentity()
  const { available, capabilities, sources, selection, toggle, selectAll, citation, setCitation } =
    useSourceSelection()
  if (!available) return <p role="status">当前配对没有来源读取权限，可从设置与能力查看配对要求。</p>
  if (capabilities.error)
    return <ErrorState error={capabilities.error} retry={() => void capabilities.refetch()} />
  if (capabilities.isPending) return <p role="status">正在读取来源能力…</p>
  if (!capabilities.data?.source_catalog)
    return <p role="status">当前服务尚不支持来源目录，请升级服务后使用。</p>
  const items = sources.data?.pages.flatMap((p) => p.items) ?? []
  return (
    <>
      <p className="hint">勾选用于之后发起的导师追问与 Studio 生成。课堂使用其已冻结资料。</p>
      {citation && (
        <div className="notebook-citation" key={`${citation.runId ?? citation.artifactId}:${citation.version}:${citation.id}`}>
          <MentorCitationContent
            spaceId={spaceId}
            runId={citation.runId}
            artifactId={citation.artifactId}
            version={citation.version}
            citationId={citation.id}
          />
          <Button variant="outline" onClick={() => setCitation(undefined)}>
            关闭引用片段
          </Button>
        </div>
      )}
      {selection && (
        <label className="check-label">
          <input
            type="checkbox"
            aria-label="全选已加载来源"
            checked={items.length > 0 && selection.items.length === items.length}
            onChange={(e) => selectAll(e.target.checked)}
          />
          全选已加载来源（已选 {selection.items.length} / {items.length}）
        </label>
      )}
      {selection?.mode === 'none' && <p role="status">未选来源，不引用资料</p>}
      {archived ? (
        <p>此学习区已归档，只能查看已有来源。</p>
      ) : session.capabilities.references ? (
        <AddSourceDialog spaceId={spaceId} />
      ) : (
        <p>添加来源需要参考管理配对权限。</p>
      )}
      <Link
        className="button button-outline notebook-source-manage"
        to="/spaces/$spaceId/knowledge"
        params={{ spaceId }}
        search={{ goal: undefined, session: undefined }}
      >
        浏览与管理资料 →
      </Link>
      {sources.isPending && <p role="status">正在读取正式来源…</p>}
      {sources.error && <ErrorState error={sources.error} retry={() => void sources.refetch()} />}
      {!sources.error && !sources.isPending && items.length === 0 && (
        <div className="notebook-empty">
          <Folder size={32} aria-hidden="true" />
          <h3>这里还没有已保存的资料</h3>
          <p>添加来源后可在此查看真实解析状态和原文片段。</p>
        </div>
      )}
      {!sources.error &&
        items.map((item) => (
          <article
            className="notebook-source"
            key={`${item.collection_id}:${item.revision_id}:${item.document_id}`}
          >
            <label className="check-label">
              <input
                type="checkbox"
                aria-label={`选择来源：${item.path} · ${item.collection_name} · ${item.collection_id.slice(0, 8)}`}
                disabled={!selection}
                checked={selection?.items.some((s) => sourceKey(s) === sourceKey(item)) ?? false}
                onChange={(e) => toggle(item, e.target.checked)}
              />
              <h3>{item.path}</h3>
            </label>
            <p className="hint">
              {item.collection_name} · {item.collection_id.slice(0, 8)}
            </p>
            <p>{sourceDescription(item.metadata)}</p>
            <CatalogSourceViewer spaceId={spaceId} source={item} />
          </article>
        ))}
      {!sources.error && sources.hasNextPage && (
        <Button
          variant="outline"
          disabled={sources.isFetchingNextPage}
          onClick={() => void sources.fetchNextPage()}
        >
          加载更多来源
        </Button>
      )}
    </>
  )
}
