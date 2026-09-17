import { memo, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import katex from 'katex'
import 'katex/dist/katex.min.css'
import type { Block, Reference } from '@/api/teaching'
import { Button } from './ui/button'
import type { Content } from '@/api/teaching'
import { useQuery } from '@tanstack/react-query'
import { useIdentity } from '@/lib/session'
import { learningClient, unwrap } from '@/api/client'
import { citationSchema } from '@/api/content'
import { contentHeader } from '@/api/teaching'
import { ErrorState } from './common'

export function safeLink(value: string) {
  try {
    const url = new URL(value)
    return url.protocol === 'https:' && !url.username && !url.password ? url.href : ''
  } catch {
    return ''
  }
}

export const SafeMarkdown = memo(function SafeMarkdown({ text }: { text: string }) {
  return (
    <div className="safe-markdown">
      <Markdown
        skipHtml
        urlTransform={safeLink}
        components={{
          a: ({ href, children }) =>
            href ? (
              <a href={href} target="_blank" rel="noopener noreferrer">
                {children}（外部链接）
              </a>
            ) : (
              <span>{children}（链接已禁用）</span>
            ),
          img: ({ alt }) => <span>图片未自动加载：{alt || '无文字说明'}</span>,
          pre: ({ children }) => (
            <pre tabIndex={0} aria-label="代码，可横向滚动">
              {children}
            </pre>
          ),
        }}
      >
        {text}
      </Markdown>
    </div>
  )
})

export function safeMath(text: string): string | null {
  if (
    text.length > 4096 ||
    /\\(?:def|gdef|edef|xdef|newcommand|renewcommand|includegraphics|href|url|html|require|input)/i.test(
      text,
    )
  )
    return null
  try {
    return katex.renderToString(text, {
      displayMode: true,
      throwOnError: true,
      trust: false,
      strict: 'error',
      maxExpand: 100,
      maxSize: 10,
      output: 'mathml',
      macros: {},
    })
  } catch {
    return null
  }
}
function MathBlock({ text }: { text: string }) {
  const rendered = useMemo(() => safeMath(text), [text])
  return rendered ? (
    <div
      className="math-block"
      aria-label="数学公式"
      dangerouslySetInnerHTML={{ __html: rendered }}
    />
  ) : (
    <pre className="math-block">公式文本（不支持或超过安全限制）：{text}</pre>
  )
}

export function SourceViewer({
  reference,
  label = '查看原资料依据',
  content,
}: {
  reference: Reference
  label?: string
  content?: Content
}) {
  const dialog = useRef<HTMLDialogElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(false)
  const [expanded, setExpanded] = useState(false)
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
        aria-label="原资料依据"
        onClose={() => {
          setOpen(false)
          trigger.current?.focus()
        }}
      >
        {open && (
          <>
            <h2>原资料依据</h2>
            <p className="hint">
              活动冻结时的正规资料片段；字节范围 {reference.range.start}–{reference.range.end}。
            </p>
            {content ? (
              <ResolvedCitation content={content} reference={reference} />
            ) : (
              <p>此旧活动尚未关联可校验的内容版本，请从正式内容页查看来源。</p>
            )}
            <details>
              <summary>来源与版本标识</summary>
              <p>
                节点版本：{reference.node_revision_id}
                <br />
                资料版本：{reference.document_revision_id ?? reference.knowledge_revision_id}
                <br />
                校验摘要：{reference.slice_sha256}
              </p>
            </details>
            <Button autoFocus onClick={() => dialog.current?.close()}>
              关闭来源
            </Button>
            <Button variant="outline" onClick={() => setExpanded(!expanded)}>
              {expanded ? '收起来源宽度' : '展开来源'}
            </Button>
          </>
        )}
      </dialog>
    </>
  )
}

function ResolvedCitation({ content, reference }: { content: Content; reference: Reference }) {
  const { session, prefix } = useIdentity()
  const source = useQuery({
    queryKey: [
      ...prefix,
      content.learning_space_id,
      content.artifact_id,
      content.version,
      reference.node_revision_id,
      'citation',
    ],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, content.learning_space_id).GET(
          '/v1/learning/content/{artifactID}/sources/{referenceID}',
          {
            params: {
              path: { artifactID: content.artifact_id, referenceID: reference.node_revision_id },
              header: contentHeader(content.learning_space_id),
              query: { version: content.version },
            },
            signal,
          },
        ),
        citationSchema,
      ),
  })
  if (source.error) return <ErrorState error={source.error} />
  if (!source.data) return <p role="status">正在核对来源版本与访问权限…</p>
  const value = source.data
  return (
    <>
      <h3>{value.title || '原始来源片段'}</h3>
      {value.status === 'missing_fragment' ? (
        <p role="alert">解析缺口：此旧引用没有保存可定位片段，不能据此推断原文。</p>
      ) : (
        <SafeMarkdown text={value.reference.slice} />
      )}
      <p className="hint">
        出处：{value.locator} · 解析覆盖范围：{value.coverage || '未记录，不推断完整性'}
      </p>
      <details>
        <summary>片段前后文 · 历史版本</summary>
        <SafeMarkdown text={value.context} />
        <p className="hint">
          解析器：{value.parser || '旧版本未记录'}。展示的是被引用版本，不代表外部页面当前内容。
        </p>
      </details>
    </>
  )
}

export const ContentBlocks = memo(function ContentBlocks({
  blocks,
  references,
  content,
  onSelect,
}: {
  blocks: Block[]
  references: Reference[]
  content?: Content
  onSelect?: (block: Block, start: number, end: number) => void
}) {
  return (
    <>
      {blocks.map((block) => {
        let value
        switch (block.kind) {
          case 'markdown':
          case 'question':
            value = <SafeMarkdown text={block.text ?? block.fallback} />
            break
          case 'code':
            value = (
              <pre tabIndex={0} aria-label="代码，可横向滚动">
                <code>{block.text ?? block.fallback}</code>
              </pre>
            )
            break
          case 'math':
            value = <MathBlock text={block.text ?? block.fallback} />
            break
          case 'callout':
            value = (
              <aside className="content-callout">
                <SafeMarkdown text={block.text ?? block.fallback} />
              </aside>
            )
            break
          case 'table':
            value = block.rows?.length ? (
              <div className="content-table" tabIndex={0} aria-label="表格，可横向滚动">
                <table>
                  <tbody>
                    {block.rows.map((row, i) => (
                      <tr key={i}>
                        {row.map((cell, j) => (
                          <td key={j}>{cell}</td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <p>{block.fallback}</p>
            )
            break
          case 'citation': {
            const ref = references.find((r) => r.node_revision_id === block.reference_id)
            value = ref ? (
              <SourceViewer reference={ref} content={content} />
            ) : (
              <p>引用未找到，无法核验来源。</p>
            )
            break
          }
          case 'answer_input':
            value = <p className="hint">{block.fallback}</p>
            break
          case 'group':
            value = (
              <ContentBlocks
                blocks={block.children ?? []}
                references={references}
                content={content}
                onSelect={onSelect}
              />
            )
            break
          default:
            value = (
              <aside className="content-callout">
                <p>{block.fallback}</p>
                <small>当前版本以文字展示此内容块。</small>
              </aside>
            )
        }
        return (
          <div
            className={`content-block content-${block.kind.replace(/[^a-z_]/g, '')}`}
            id={`block-${block.block_id}`}
            key={block.block_id}
            onMouseUp={(event) => {
              if (!onSelect || !block.text || block.kind === 'group') return
              const selection = window.getSelection()
              if (
                !selection ||
                selection.isCollapsed ||
                !event.currentTarget.contains(selection.anchorNode) ||
                !event.currentTarget.contains(selection.focusNode)
              )
                return
              const text = selection.toString(),
                start = block.text.indexOf(text)
              if (text.trim() && start >= 0 && block.text.indexOf(text, start + 1) < 0)
                onSelect(block, start, start + text.length)
            }}
          >
            {value}
            {onSelect &&
              block.text &&
              !['citation', 'answer_input', 'group'].includes(block.kind) && (
                <Button variant="ghost" onClick={() => onSelect(block, 0, block.text!.length)}>
                  选择此段
                </Button>
              )}
          </div>
        )
      })}
    </>
  )
})
