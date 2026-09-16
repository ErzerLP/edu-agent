import { memo, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import katex from 'katex'
import 'katex/dist/katex.min.css'
import type { Block, Reference } from '@/api/teaching'
import { Button } from './ui/button'

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
}: {
  reference: Reference
  label?: string
}) {
  const dialog = useRef<HTMLDialogElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(false)
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
        className="source-dialog"
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
            <SafeMarkdown text={reference.slice || '此旧引用没有保留正文片段。'} />
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
          </>
        )}
      </dialog>
    </>
  )
}

export const ContentBlocks = memo(function ContentBlocks({
  blocks,
  references,
}: {
  blocks: Block[]
  references: Reference[]
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
            value = ref ? <SourceViewer reference={ref} /> : <p>{block.fallback}</p>
            break
          }
          case 'answer_input':
            value = <p className="hint">{block.fallback}</p>
            break
          case 'group':
            value = <ContentBlocks blocks={block.children ?? []} references={references} />
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
          >
            {value}
          </div>
        )
      })}
    </>
  )
})
