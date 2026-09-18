import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { z } from 'zod'
import { learningClient, unwrap } from '@/api/client'
import { pdfGaps, pdfStatus, type PDFReport } from '@/api/pdf'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { ErrorState } from './common'

export function PDFCoverage({ report }: { report: PDFReport }) {
  return (
    <details className="pdf-coverage">
      <summary>PDF 逐页覆盖报告（共 {report.page_count} 页）</summary>
      <p>
        页码按原文件顺序从 1
        开始，不使用印刷页标签。表格、公式、图片和复杂阅读顺序需对照页图核对；文本提取不代表已经理解或验证。
      </p>
      <ol>
        {report.pages.map((p) => (
          <li key={p.number}>
            第 {p.number} 页：{pdfStatus[p.status]}
            {p.duplicate_of ? `；提取文本与第 ${p.duplicate_of} 页相同，保留原页序` : ''}
            {p.gaps.length > 0 && <p>{p.gaps.map((g) => pdfGaps[g] ?? g).join('；')}</p>}
            {p.text && (
              <details>
                <summary>查看第 {p.number} 页提取文本</summary>
                <pre className="reference-text">{p.text}</pre>
              </details>
            )}
          </li>
        ))}
      </ol>
      <p className="hint">
        原文件 SHA-256：{report.fingerprint}
        <br />
        解析器：{report.parser}
      </p>
    </details>
  )
}

export function PDFPageViewer({
  spaceId,
  revisionId,
  documentId,
  collectionId,
  pages,
  selectedText,
}: {
  spaceId: string
  revisionId: string
  documentId: string
  collectionId?: string
  pages: number[]
  selectedText?: string
}) {
  const { session, prefix } = useIdentity()
  const [open, setOpen] = useState(false),
    [number, setNumber] = useState(pages[0] ?? 1),
    [url, setURL] = useState('')
  const result = useQuery({
    queryKey: [...prefix, spaceId, revisionId, documentId, collectionId, number, 'pdf-page'],
    enabled: open,
    gcTime: 0,
    retry: false,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET(
          '/v1/knowledge/revisions/{revisionID}/documents/{documentID}/pages/{page}',
          {
            params: {
              path: { revisionID: revisionId, documentID: documentId, page: number },
              header: {
                'X-Learning-Space-ID': spaceId,
                ...(collectionId ? { 'X-Knowledge-Collection-ID': collectionId } : {}),
              },
            },
            parseAs: 'blob',
            signal,
          },
        ),
        z.instanceof(Blob),
      ),
  })
  useEffect(() => {
    setURL('')
    if (!open || !result.data) return
    const next = URL.createObjectURL(result.data)
    setURL(next)
    return () => URL.revokeObjectURL(next)
  }, [open, result.data])
  if (!pages.length) return null
  return (
    <section aria-label="PDF 安全页视图" className="pdf-viewer">
      <Button variant="outline" onClick={() => setOpen(!open)}>
        {open ? '关闭 PDF 页视图' : `查看 PDF 原页（${pages.length} 页可定位）`}
      </Button>
      {open && (
        <>
          <label>
            定位 PDF 物理页
            <select value={number} onChange={(e) => setNumber(Number(e.target.value))}>
              {pages.map((p) => (
                <option key={p} value={p}>
                  第 {p} 页
                </option>
              ))}
            </select>
          </label>
          <div className="actions">
            <Button
              variant="outline"
              disabled={pages.indexOf(number) <= 0}
              onClick={() => setNumber(pages[pages.indexOf(number) - 1])}
            >
              上一页
            </Button>
            <Button
              variant="outline"
              disabled={pages.indexOf(number) >= pages.length - 1}
              onClick={() => setNumber(pages[pages.indexOf(number) + 1])}
            >
              下一页
            </Button>
          </div>
          {selectedText && (
            <blockquote aria-label="被引用的原文片段" className="reference-text">
              {selectedText}
            </blockquote>
          )}
          {result.error ? (
            <ErrorState error={result.error} />
          ) : result.isFetching ? (
            <p role="status">正在核对历史版本并生成第 {number} 页…</p>
          ) : (
            url && (
              <img
                src={url}
                alt={`被引用 PDF 历史版本的第 ${number} 页`}
                style={{ maxWidth: '100%', height: 'auto' }}
              />
            )
          )}
          <p className="hint">
            当前为保存的历史原件页图。未嵌入字体时可能缺字，请核对覆盖报告。脚本、表单和链接均不可执行；图片未加入持久缓存。
          </p>
        </>
      )}
    </section>
  )
}
