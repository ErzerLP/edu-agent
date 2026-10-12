import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { FileText, Folder } from 'lucide-react'
import type { Collection } from '@/api/references'
import { useNotebookCollectionSources, useNotebookSources } from '@/lib/use-notebook-sources'
import { ErrorState } from '../common'

function CollectionSources({ spaceId, collection }: { spaceId: string; collection: Collection }) {
  const [expanded, setExpanded] = useState(false)
  const tree = useNotebookCollectionSources(spaceId, collection, expanded)
  return (
    <details
      className="notebook-source-collection"
      onToggle={(event) => setExpanded(event.currentTarget.open)}
    >
      <summary>
        <Folder size={18} aria-hidden="true" />
        {collection.name}
      </summary>
      <p className="hint">
        {collection.shared ? '已关联共享集合' : '区内私有集合'} · {collection.source}
      </p>
      {!collection.head_revision_id && <p>尚无正式资料。</p>}
      {expanded && collection.head_revision_id && tree.isPending && (
        <p role="status">正在读取文档目录…</p>
      )}
      {tree.error && <ErrorState error={tree.error} retry={() => void tree.refetch()} />}
      {!tree.error && tree.data && (
        <>
          <p className="hint">
            已保存版本 {tree.data.revision_no} · {tree.data.documents.length} 份文档
          </p>
          {tree.data.documents.length === 0 && <p>此版本没有文档。</p>}
          {tree.data.documents.map(({ path, document }) => (
            <details
              className="notebook-source"
              key={`${collection.id}:${tree.data.revision_id}:${document.document_id}`}
            >
              <summary>
                <FileText size={18} aria-hidden="true" />
                {path}
              </summary>
              <p className="hint">
                {document.pdf
                  ? `PDF · ${document.pdf.report.coverage === 'complete_text' ? '文本已提取，版式未核验' : '部分正文，解析有缺口'}`
                  : '来源类型与覆盖情况未知'}
              </p>
              <ul>
                {document.nodes
                  .filter((node) => node.heading_level > 0)
                  .map((node) => (
                    <li key={node.node_id}>{node.title}</li>
                  ))}
              </ul>
            </details>
          ))}
        </>
      )}
    </details>
  )
}

export function SourcesPanel({ spaceId }: { spaceId: string }) {
  const { available, collections } = useNotebookSources(spaceId)
  if (!available) return <p role="status">当前配对没有来源读取权限，可从设置与能力查看配对要求。</p>
  return (
    <>
      <p className="hint">当前仅浏览已保存资料。课堂使用其已冻结资料；浏览不会改变课堂范围。</p>
      <Link
        className="button button-outline notebook-source-manage"
        to="/spaces/$spaceId/knowledge"
        params={{ spaceId }}
        search={{ goal: undefined, session: undefined }}
      >
        浏览与管理资料 →
      </Link>
      {collections.isPending && <p role="status">正在读取来源集合…</p>}
      {collections.error && (
        <ErrorState error={collections.error} retry={() => void collections.refetch()} />
      )}
      {!collections.error && collections.data?.items.length === 0 && (
        <div className="notebook-empty">
          <Folder size={32} aria-hidden="true" />
          <h3>这里还没有已保存的资料</h3>
          <p>可到知识与参考页管理资料，再为课堂正式设置参考。</p>
        </div>
      )}
      {!collections.error &&
        collections.data?.items.map((collection) => (
          <CollectionSources
            key={`${spaceId}:${collection.id}`}
            spaceId={spaceId}
            collection={collection}
          />
        ))}
    </>
  )
}
