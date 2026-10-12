import { Link } from '@tanstack/react-router'
import { Folder } from 'lucide-react'
import { sourceDescription } from '@/api/notebook-sources'
import { useNotebookSources } from '@/lib/use-notebook-sources'
import { useIdentity } from '@/lib/session'
import { ErrorState } from '../common'
import { Button } from '../ui/button'
import { AddSourceDialog } from './add-source-dialog'
import { CatalogSourceViewer } from './source-viewer'

export function SourcesPanel({
  spaceId,
  archived = false,
}: {
  spaceId: string
  archived?: boolean
}) {
  const { session } = useIdentity()
  const { available, capabilities, sources } = useNotebookSources(spaceId)
  if (!available) return <p role="status">当前配对没有来源读取权限，可从设置与能力查看配对要求。</p>
  if (capabilities.error)
    return <ErrorState error={capabilities.error} retry={() => void capabilities.refetch()} />
  if (capabilities.isPending) return <p role="status">正在读取来源能力…</p>
  if (!capabilities.data?.source_catalog)
    return <p role="status">当前服务尚不支持来源目录，请升级服务后使用。</p>
  const items = sources.data?.pages.flatMap((p) => p.items) ?? []
  return (
    <>
      <p className="hint">当前仅浏览已保存资料。课堂使用其已冻结资料；浏览不会改变课堂范围。</p>
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
            <h3>{item.path}</h3>
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
