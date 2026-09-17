import { useState } from 'react'
import type { ImportCommand, ImportPreview } from '@/api/import-jobs'
import type { ScanItem } from '@/lib/import-files'
import { Button } from './ui/button'
import { Pagination } from './common'

export function ImportManifest({
  rows,
  change,
  disabled,
}: {
  rows: ScanItem[]
  change?: (rows: ScanItem[]) => void
  disabled?: boolean
}) {
  const [page, setPage] = useState(0)
  const selected = rows.filter((row) => row.selected && !row.error)
  return (
    <section aria-label="来源清单" className="panel">
      <p>
        已选择 {selected.length} / {rows.length} 项 ·{' '}
        {selected.reduce((n, row) => n + (row.source?.item.bytes ?? 0), 0).toLocaleString()} 字节
      </p>
      {rows.slice(page * 20, page * 20 + 20).map((row, i) => (
        <div key={`${row.name}:${i}`} className="import-row">
          <label>
            <input
              type="checkbox"
              checked={row.selected}
              disabled={disabled || !!row.error || !change}
              onChange={(e) =>
                change?.(
                  rows.map((other, index) =>
                    index === page * 20 + i ? { ...other, selected: e.target.checked } : other,
                  ),
                )
              }
            />
            {row.name}
          </label>
          <span>
            {row.bytes.toLocaleString()} 字节 · {row.error ?? '可导入'}
          </span>
          {row.source && (
            <small>
              目标：{row.source.item.path} · SHA-256：{row.source.item.sha256}
            </small>
          )}
        </div>
      ))}
      <Pagination
        page={page + 1}
        previous={page ? () => setPage(page - 1) : undefined}
        next={(page + 1) * 20 < rows.length ? () => setPage(page + 1) : undefined}
      />
    </section>
  )
}

export function ImportDiff({ preview }: { preview: ImportPreview }) {
  return (
    <section aria-label="批次差异">
      <p>
        新增 {preview.summary.added} · 更新 {preview.summary.updated} · 未变化{' '}
        {preview.summary.unchanged}
      </p>
      <p>
        {preview.impact_known
          ? `关联学习证据 ${preview.affected_evidence} 项；导入不自动改写这些证据。`
          : '关联影响尚未完全确认。'}
      </p>
      {preview.diff.map((diff) => (
        <details key={diff.document_id} open>
          <summary>
            {diff.before_path ?? ''} → {diff.after_path ?? ''}（{diff.kind}）
          </summary>
          <p>
            新增章节 {diff.added_node_ids.length} · 删除章节 {diff.removed_node_ids.length} ·
            改写章节 {diff.edited_node_ids.length} · 标题变化 {diff.title_node_ids.length} ·
            结构变化 {diff.structure_node_ids.length}
          </p>
          {diff.truncated && <p role="status">差异达到展示上限，以下不是完整正文。</p>}
          <pre>{diff.unified_diff || '没有正文差异。'}</pre>
        </details>
      ))}
      {preview.before.map((item) => (
        <details key={item.path}>
          <summary>原正文：{item.path}</summary>
          <pre>{item.markdown}</pre>
        </details>
      ))}
    </section>
  )
}

type Decisions = Pick<ImportCommand, 'document_resolutions' | 'node_resolutions'>
export function ImportIdentityReview({
  preview,
  disabled,
  submit,
}: {
  preview: ImportPreview
  disabled: boolean
  submit: (decisions: Decisions) => void
}) {
  const review = preview.identity_review
  const [decisions, setDecisions] = useState<
    Record<string, { action: string; sources: string[]; reason: string }>
  >({})
  const [page, setPage] = useState(0)
  if (!review) return null
  const entries = [
    ...review.document_reviews.map((item) => ({ ...item, node: false })),
    ...review.node_reviews.map((item) => ({ ...item, node: true })),
  ]
  const key = (entry: (typeof entries)[number]) => `${entry.node}:${entry.locator}`
  const complete = entries.every((entry) => {
    const d = decisions[key(entry)]
    return d && d.reason.trim() && (d.action === 'new' || d.sources.length > 0)
  })
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (!complete) return
        submit({
          document_resolutions: review.document_reviews.map((entry) => {
            const d = decisions[`false:${entry.locator}`]
            return {
              locator: entry.locator,
              action: d.action as 'new' | 'preserve',
              reason: d.reason,
              ...(d.action === 'preserve' ? { document_id: d.sources[0] } : {}),
            }
          }),
          node_resolutions: review.node_reviews.map((entry) => {
            const d = decisions[`true:${entry.locator}`]
            return {
              locator: entry.locator,
              action: d.action as 'new' | 'preserve' | 'rewrite' | 'split' | 'merge',
              reason: d.reason,
              ...(d.action !== 'new' ? { source_node_revision_ids: d.sources } : {}),
            }
          }),
        })
      }}
    >
      <fieldset disabled={disabled}>
        <legend>身份审阅（依据当前服务端候选）</legend>
        {entries.slice(page * 10, page * 10 + 10).map((entry) => {
          const id = key(entry)
          const d = decisions[id] ?? { action: '', sources: [], reason: '' }
          const update = (value: Partial<typeof d>) =>
            setDecisions((old) => ({ ...old, [id]: { ...d, ...value } }))
          return (
            <section key={id} className="panel">
              <h4>
                {entry.node ? '章节' : '文档'}：{entry.path}
              </h4>
              <p>需审阅原因：{entry.reason_code}</p>
              <label>
                身份决定
                <select
                  value={d.action}
                  onChange={(e) => update({ action: e.target.value, sources: [] })}
                  required
                >
                  <option value="">请选择</option>
                  <option value="new">创建新身份</option>
                  <option value="preserve">保留原身份</option>
                  {entry.node && (
                    <>
                      <option value="rewrite">明确改写</option>
                      <option value="split">拆分</option>
                      <option value="merge">合并</option>
                    </>
                  )}
                </select>
              </label>
              {d.action &&
                d.action !== 'new' &&
                entry.candidates.map((candidate) => {
                  const value = entry.node ? candidate.revision_id : candidate.stable_id
                  const multiple = entry.node && ['merge', 'split'].includes(d.action)
                  return (
                    <label key={value}>
                      <input
                        type={multiple ? 'checkbox' : 'radio'}
                        name={id}
                        checked={d.sources.includes(value)}
                        onChange={(e) =>
                          update({
                            sources: multiple
                              ? e.target.checked
                                ? [...d.sources, value]
                                : d.sources.filter((s) => s !== value)
                              : [value],
                          })
                        }
                      />
                      {candidate.stable_id} · {candidate.reason_code}
                      <small>
                        修订：{candidate.revision_id}{' '}
                        {candidate.evidence ? JSON.stringify(candidate.evidence) : ''}
                      </small>
                    </label>
                  )
                })}
              <label>
                决定理由
                <input
                  value={d.reason}
                  onChange={(e) => update({ reason: e.target.value })}
                  maxLength={2000}
                  required
                />
              </label>
            </section>
          )
        })}
        <Pagination
          page={page + 1}
          previous={page ? () => setPage(page - 1) : undefined}
          next={(page + 1) * 10 < entries.length ? () => setPage(page + 1) : undefined}
        />
        <Button disabled={!complete}>提交身份决定并重新预览</Button>
      </fieldset>
    </form>
  )
}
