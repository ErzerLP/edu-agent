import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import {
  sourceDescription,
  sourceDetailSchema,
  type SourceItem,
  type SourceMetadata,
} from '@/api/notebook-sources'
import { useIdentity } from '@/lib/session'
import { sourceSchema as researchSourceSchema, sourceStatus } from '@/api/research'
import { Button } from '../ui/button'
import { mentorCitationSchema } from '@/api/source-selection'
import { ErrorState } from '../common'
import { PDFCoverage, PDFPageViewer } from '../pdf-source'

// Every entry point keeps its own authoritative resolver. This shell never widens a citation scope.
export function SourceDialog({
  label,
  title = '来源查看器',
  children,
}: {
  label: string
  title?: string
  children: ReactNode
}) {
  const dialog = useRef<HTMLDialogElement>(null),
    trigger = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(false),
    [expanded, setExpanded] = useState(false)
  return (
    <>
      <Button
        ref={trigger}
        variant="outline"
        onClick={() => {
          setOpen(true)
          dialog.current?.showModal()
        }}
      >
        {label}
      </Button>
      <dialog
        ref={dialog}
        className={`source-dialog${expanded ? ' expanded' : ''}`}
        aria-label={title}
        onClose={() => {
          setOpen(false)
          trigger.current?.focus()
        }}
      >
        {open && (
          <>
            <h2>{title}</h2>
            {children}
            <div className="actions">
              <Button autoFocus onClick={() => dialog.current?.close()}>
                关闭来源
              </Button>
              <Button variant="outline" onClick={() => setExpanded(!expanded)}>
                {expanded ? '收起来源宽度' : '展开来源'}
              </Button>
            </div>
          </>
        )}
      </dialog>
    </>
  )
}

export function SourceProvenance({
  metadata,
  statusLabel,
}: {
  metadata: SourceMetadata | null
  statusLabel?: string
}) {
  return (
    <section aria-label="来源解析状态">
      <p>{statusLabel ?? sourceDescription(metadata)}</p>
      {metadata && (
        <>
          {metadata.locator && <p>请求地址：{metadata.locator}</p>}
          {metadata.final_url && <p>实际读取：{metadata.final_url}</p>}
          <p>
            解析器：{metadata.parser || '未知'} · 覆盖：{metadata.coverage || '未知'}
          </p>
          <p className="hint">指纹：{metadata.fingerprint || '未知'}</p>
          {metadata.derived_from && (
            <p>
              派生自已保存产物 {metadata.derived_from.artifact_id} 的版本{' '}
              {metadata.derived_from.version}
            </p>
          )}
        </>
      )}
      <p className="hint">
        解析状态描述实际保存的历史文本，不表示结论已验证，也不代表外部页面当前内容。
      </p>
    </section>
  )
}

export function CatalogSourceViewer({ spaceId, source }: { spaceId: string; source: SourceItem }) {
  return (
    <SourceDialog label={`查看来源：${source.path}`}>
      <CatalogSourceContent spaceId={spaceId} source={source} />
    </SourceDialog>
  )
}

export function ResearchSourceContent({
  spaceId,
  runId,
  sourceId,
  revisionId,
  fragmentId,
}: {
  spaceId: string
  runId: string
  sourceId: string
  revisionId: string
  fragmentId?: string
}) {
  const { session, prefix } = useIdentity()
  const detail = useQuery({
    queryKey: [
      ...prefix,
      spaceId,
      runId,
      sourceId,
      revisionId,
      fragmentId,
      'research-source-detail',
    ],
    gcTime: 0,
    retry: false,
    queryFn: async ({ signal }) => {
      const source = await unwrap(
        learningClient(session, spaceId).GET('/v1/learning/runs/{runID}/sources/{sourceID}', {
          params: {
            path: { runID: runId, sourceID: sourceId },
            header: { 'X-Learning-Space-ID': spaceId },
          },
          signal,
        }),
        researchSourceSchema,
      )
      if (
        source.space_id !== spaceId ||
        source.id !== sourceId ||
        source.revision_id !== revisionId ||
        (fragmentId && !source.fragments.some((f) => f.id === fragmentId))
      )
        throw new ApiError(404, 'not_found')
      return source
    },
  })
  if (detail.error) return <ErrorState error={detail.error} />
  if (!detail.data) return <p role="status">正在核对研究来源版本与访问权限…</p>
  const source = detail.data
  return (
    <>
      <h3>{source.title}</h3>
      <SourceProvenance
        statusLabel={sourceStatus[source.status]}
        metadata={{
          kind: source.pdf ? 'pdf' : 'url',
          locator: source.locator,
          final_url: source.final_url,
          parser: source.parser,
          coverage: source.coverage,
          fingerprint: source.fingerprint,
          status: source.coverage === 'complete_text' ? 'parsed' : 'partial',
        }}
      />
      {source.pdf && <PDFCoverage report={source.pdf} />}
      {source.fragments.map((fragment, index) => (
        <details key={fragment.id} open={fragmentId === fragment.id || undefined}>
          <summary>
            原文片段 {index + 1}
            {fragment.page ? ` · 第 ${fragment.page} 页` : ''}（字节 {fragment.start}–{fragment.end}
            ）
          </summary>
          <pre className="reference-text">{fragment.text}</pre>
        </details>
      ))}
      <p className="hint">研究来源版本：{source.revision_id}</p>
      {source.pdf && source.status === 'adopted' && source.document_revision_id && (
        <PDFPageViewer
          spaceId={source.space_id}
          revisionId={source.knowledge_revision_id}
          documentId={source.document_revision_id}
          collectionId={source.collection_id}
          pages={source.pdf.pages.map((p) => p.number)}
        />
      )}
    </>
  )
}

function CatalogSourceContent({ spaceId, source }: { spaceId: string; source: SourceItem }) {
  const { session, prefix } = useIdentity()
  const detail = useQuery({
    queryKey: [
      ...prefix,
      spaceId,
      source.collection_id,
      source.revision_id,
      source.document_revision_id,
      'source-detail',
    ],
    gcTime: 0,
    retry: false,
    queryFn: async ({ signal }) => {
      const value = await unwrap(
        learningClient(session, spaceId).GET(
          '/v1/knowledge/sources/{revisionID}/documents/{documentRevisionID}',
          {
            params: {
              path: {
                revisionID: source.revision_id,
                documentRevisionID: source.document_revision_id,
              },
              header: {
                'X-Learning-Space-ID': spaceId,
                'X-Knowledge-Collection-ID': source.collection_id,
              },
            },
            signal,
          },
        ),
        sourceDetailSchema,
      )
      if (
        value.collection_id !== source.collection_id ||
        value.revision_id !== source.revision_id ||
        value.document_revision_id !== source.document_revision_id ||
        value.document_id !== source.document_id
      )
        throw new ApiError(502, 'invalid_response')
      return value
    },
  })
  if (detail.error) return <ErrorState error={detail.error} />
  if (!detail.data) return <p role="status">正在核对来源版本与访问权限…</p>
  const value = detail.data
  return (
    <>
      <h3>{value.path}</h3>
      <p>{value.collection_name}</p>
      <SourceProvenance metadata={value.metadata} />
      {value.pdf && (
        <>
          <PDFCoverage report={value.pdf.report} />
          <PDFPageViewer
            spaceId={spaceId}
            revisionId={value.revision_id}
            documentId={value.document_revision_id}
            collectionId={value.collection_id}
            pages={value.pdf.report.pages.map((p) => p.number)}
          />
        </>
      )}
      <section aria-label="来源原文片段">
        {value.fragments.length === 0 && <p>没有可定位的正文片段。</p>}
        {value.fragments.map((fragment, index) => (
          <details key={fragment.node_revision_id}>
            <summary>
              原文片段 {index + 1} · {fragment.title || '正文'}
              {fragment.page ? ` · 第 ${fragment.page} 页` : ''}（字节 {fragment.range.start}–
              {fragment.range.end}）
            </summary>
            <p>
              规范文本第 {fragment.range.start_line}–{fragment.range.end_line} 行
            </p>
            <pre className="reference-text">{fragment.text}</pre>
            {fragment.truncated && (
              <p>此片段达到展示预算；可从资料管理页导出该历史版本核对完整正文。这不是解析缺口。</p>
            )}
          </details>
        ))}
      </section>
      <details>
        <summary>来源与版本标识</summary>
        <p>
          集合：{value.collection_id}
          <br />
          文档：{value.document_id}
          <br />
          文档版本：{value.document_revision_id}
          <br />
          集合版本：{value.revision_id}
        </p>
      </details>
    </>
  )
}

export function MentorCitationContent({
  spaceId,
  runId,
  citationId,
}: {
  spaceId: string
  runId: string
  citationId: string
}) {
  const { session, prefix } = useIdentity()
  const fragmentView = useRef<HTMLElement>(null)
  const detail = useQuery({
    queryKey: [...prefix, spaceId, runId, citationId, 'mentor-citation'],
    gcTime: 0,
    retry: false,
    refetchInterval: 5000,
    queryFn: async ({ signal }) => {
      const value = await unwrap(
        learningClient(session, spaceId).GET('/v1/learning/runs/{runID}/citations/{citationID}', {
          params: {
            path: { runID: runId, citationID: citationId },
            header: { 'X-Learning-Space-ID': spaceId },
          },
          signal,
        }),
        mentorCitationSchema,
      )
      if (value.id !== citationId) throw new ApiError(502, 'invalid_response')
      return value
    },
  })
  useEffect(() => {
    if (detail.data && !detail.error) {
      fragmentView.current?.scrollIntoView({ block: 'nearest' })
      fragmentView.current?.focus({ preventScroll: true })
    }
  }, [detail.data, detail.error])
  if (detail.error) return <ErrorState error={detail.error} />
  if (!detail.data) return <p role="status">正在核对本轮来源版本、片段与访问权限…</p>
  const { source, fragment } = detail.data
  return (
    <section ref={fragmentView} tabIndex={-1} aria-label={`引用 ${citationId} 核验片段`}>
      <h3>
        [{citationId}] {source.path}
      </h3>
      <p>
        {source.collection_name} · {source.collection_id.slice(0, 8)}
      </p>
      <SourceProvenance metadata={source.metadata} />
      <p>
        {fragment.title} {fragment.page ? `· 第 ${fragment.page} 页` : ''} · 字节{' '}
        {fragment.range.start}–{fragment.range.end}
      </p>
      <pre className="reference-text">
        <mark>{fragment.text}</mark>
      </pre>
      {fragment.truncated && <p>此片段达到展示预算，不表示资料已完整读取。</p>}
      <details>
        <summary>核验版本与指纹</summary>
        <p>
          集合版本：{source.revision_id}
          <br />
          文档版本：{source.document_revision_id}
          <br />
          片段 SHA256：{detail.data.slice_sha256}
        </p>
      </details>
    </section>
  )
}
