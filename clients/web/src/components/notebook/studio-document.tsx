import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { studioAPI, type StudioClaim, type StudioRevision } from '@/api/studio'
import { useIdentity } from '@/lib/session'
import { Button } from '../ui/button'
import { ErrorState } from '../common'
import { studioNames } from './studio-generation'
import { StudioQuiz } from './studio-quiz'

export function StudioDocument({
  spaceId,
  artifactId,
  canWrite,
  onBack,
  onRegenerate,
  onCitation,
}: {
  spaceId: string
  artifactId: string
  canWrite: boolean
  onBack: () => void
  onRegenerate: (base: StudioRevision) => void
  onCitation: (artifact: string, version: number, id: string) => void
}) {
  const { session, prefix } = useIdentity()
  const [version, setVersion] = useState<number>()
  const api = studioAPI(session, spaceId)
  const saved = useQuery({
    queryKey: [...prefix, spaceId, 'studio-document', artifactId, version],
    queryFn: ({ signal }) => api.get(artifactId, signal, version),
    gcTime: 0,
    retry: false,
    refetchInterval: 5000,
  })
  const history = useQuery({
    queryKey: [...prefix, spaceId, 'studio-history', artifactId],
    queryFn: ({ signal }) => api.history(artifactId, signal),
    gcTime: 0,
    retry: false,
  })
  const r = saved.data
  const sections = r?.body.guide
    ? [
        ['核心要点', r.body.guide.key_points],
        ['术语', r.body.guide.terms],
        ['学习顺序（生成建议）', r.body.guide.learning_order],
        ['常见误区与辨析', r.body.guide.misconceptions],
      ]
    : r?.body.brief
      ? [
          ['一页综述', r.body.brief.overview],
          ['来源差异', r.body.brief.differences],
          ['覆盖缺口', r.body.brief.gaps],
        ]
      : []
  return (
    <article className="studio-document">
      <Button variant="outline" onClick={onBack}>
        返回 Studio
      </Button>
      {saved.error || history.error ? (
        <ErrorState error={saved.error ?? history.error} />
      ) : !r ? (
        <p role="status">正在核验产物和来源…</p>
      ) : (
        <>
          <h3>{r.body.title}</h3>
          <p>
            {studioNames[r.kind]} · 已保存第 {r.version} 版 ·{' '}
            {r.body.coverage === 'partial' ? '部分完成' : '已完成'}
          </p>
          <label>
            产物版本
            <select
              value={version ?? ''}
              onChange={(e) => setVersion(e.target.value ? Number(e.target.value) : undefined)}
            >
              <option value="">最新版本</option>
              {history.data?.items.map((item) => (
                <option key={item.version} value={item.version}>
                  第 {item.version} 版
                </option>
              ))}
            </select>
          </label>
          <p className="hint">
            基于生成时选定的 {r.body.source_manifest?.entries.length}{' '}
            个来源；当前勾选不会改变已保存版本。引用核验确认出处与片段，不代表结论已被人工审核。
          </p>
          {(sections as [string, StudioClaim[]][]).map(([title, claims]) => (
            <section key={title}>
              <h4>{title}</h4>
              {claims.map((claim, i) => (
                <p key={i}>
                  {claim.text}
                  {claim.citations.map((id) => (
                    <button
                      type="button"
                      className="citation-link"
                      key={id}
                      aria-label={`查看引用 ${id}`}
                      onClick={() => onCitation(r.artifact_id, r.version, id)}
                    >
                      [{id}]
                    </button>
                  ))}
                </p>
              ))}
            </section>
          ))}
          {r.body.quiz && (
            <StudioQuiz
              key={`${r.artifact_id}:${r.version}`}
              revision={r}
              canWrite={canWrite}
              onCitation={onCitation}
            />
          )}
          <Button disabled={!canWrite} onClick={() => onRegenerate(r)}>
            重新生成新版本
          </Button>
        </>
      )}
    </article>
  )
}
