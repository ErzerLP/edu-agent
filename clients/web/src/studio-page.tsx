import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { learningClient, unwrap } from './api/client'
import { librarySchema } from './api/content'
import { contentHeader } from './api/teaching'
import { goalSchema, pageOf, spaceSchema } from './api/runtime'
import { useIdentity } from './lib/session'
import { ErrorState, Pagination } from './components/common'

export function StudioPage({ spaceId }: { spaceId: string }) {
  const { session, prefix } = useIdentity()
  const [goal, setGoal] = useState('')
  const [kind, setKind] = useState<'' | 'reading' | 'exercise'>('')
  const [node, setNode] = useState('')
  const [source, setSource] = useState<'' | 'available' | 'missing' | 'restricted'>('')
  const [favorite, setFavorite] = useState(false)
  const [after, setAfter] = useState('')
  const [before, setBefore] = useState('')
  const [cursors, setCursors] = useState([''])
  const reset = () => setCursors([''])
  const client = () => learningClient(session, spaceId)
  const goals = useQuery({
    queryKey: [...prefix, spaceId, 'studio-goals'],
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/goals', { params: { query: { limit: 100 } }, signal }),
        pageOf(goalSchema),
      ),
  })
  const space = useQuery({
    queryKey: [...prefix, spaceId, 'space'],
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning-spaces/{spaceID}', {
          params: { path: { spaceID: spaceId } },
          signal,
        }),
        spaceSchema,
      ),
  })
  const items = useQuery({
    queryKey: [
      ...prefix,
      spaceId,
      'studio',
      goal,
      kind,
      node,
      source,
      favorite,
      after,
      before,
      cursors.at(-1),
    ],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content', {
          params: {
            header: contentHeader(spaceId),
            query: {
              goal_id: goal || undefined,
              kind: kind || undefined,
              node_id: node || undefined,
              source_status: source || undefined,
              favorite,
              after: after ? new Date(after + 'T00:00:00Z').toISOString() : undefined,
              before: before ? new Date(before + 'T23:59:59Z').toISOString() : undefined,
              limit: 10,
              cursor: cursors.at(-1) || undefined,
            },
          },
          signal,
        }),
        librarySchema,
      ),
  })
  const knowledge = useQuery({
    queryKey: [...prefix, spaceId, 'studio-node-options'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/content', {
          params: { header: contentHeader(spaceId), query: { limit: 50 } },
          signal,
        }),
        librarySchema,
      ),
  })
  const nodes = [
    ...new Map(
      [...(knowledge.data?.items ?? []), ...(items.data?.items ?? [])]
        .flatMap((item) => item.knowledge_points)
        .map((point) => [point.id, point]),
    ).values(),
  ]
  return (
    <section className="studio-page">
      <Link to="/spaces/$spaceId" params={{ spaceId }}>
        ← {space.data?.name ?? '学习区'}
      </Link>
      <h1>Studio 内容库</h1>
      <p>教学内容按目标自动收录。独立阅读、加工和导出，收藏只表示偏好。</p>
      <div className="filters">
        <label>
          关联目标
          <select
            value={goal}
            onChange={(e) => {
              setGoal(e.target.value)
              reset()
            }}
          >
            <option value="">全部目标</option>
            {goals.data?.items.map((item) => (
              <option key={item.goal_id} value={item.goal_id}>
                {item.management.details.name || item.text}
              </option>
            ))}
          </select>
        </label>
        <label>
          内容类型
          <select
            value={kind}
            onChange={(e) => {
              setKind(e.target.value as typeof kind)
              reset()
            }}
          >
            <option value="">全部类型</option>
            <option value="reading">阅读讲解</option>
            <option value="exercise">练习与说明</option>
          </select>
        </label>
        <label>
          知识点
          <select
            value={node}
            onChange={(e) => {
              setNode(e.target.value)
              reset()
            }}
          >
            <option value="">全部知识点</option>
            {nodes.map((point) => (
              <option key={point.id} value={point.id}>
                {point.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          来源状态
          <select
            value={source}
            onChange={(e) => {
              setSource(e.target.value as typeof source)
              reset()
            }}
          >
            <option value="">全部来源</option>
            <option value="available">来源可核验</option>
            <option value="missing">片段缺失</option>
            <option value="restricted">来源已不可用</option>
          </select>
        </label>
        <label>
          更新起始日期
          <input
            type="date"
            value={after}
            onChange={(e) => {
              setAfter(e.target.value)
              reset()
            }}
          />
        </label>
        <label>
          更新结束日期
          <input
            type="date"
            value={before}
            onChange={(e) => {
              setBefore(e.target.value)
              reset()
            }}
          />
        </label>
        <label className="check-label">
          <input
            type="checkbox"
            checked={favorite}
            onChange={(e) => {
              setFavorite(e.target.checked)
              reset()
            }}
          />
          只看收藏
        </label>
      </div>
      {!!(items.error || goals.error || space.error || knowledge.error) && (
        <ErrorState
          error={items.error || goals.error || space.error || knowledge.error}
          retry={() => void items.refetch()}
        />
      )}
      {items.isPending && <p role="status">正在读取真实内容记录…</p>}
      {items.data?.items.length === 0 && (
        <p>当前筛选没有内容。开始教学后，正式内容会自动出现在这里。</p>
      )}
      {items.data?.items.map((item) => (
        <article className="panel studio-item" key={item.artifact_id}>
          <h2>
            {item.source_status === 'restricted' ? (
              item.title
            ) : (
              <Link
                to="/content/$artifactId"
                params={{ artifactId: item.artifact_id }}
                search={{ space: spaceId, version: undefined }}
              >
                {item.title || '学习内容'}
              </Link>
            )}
          </h2>
          <p>
            {item.kind === 'reading' ? '阅读讲解' : '练习与说明'} · 第 {item.version} 版
            {item.favorite ? ' · 已收藏' : ''}
            {item.pinned_version ? ` · 固定阅读第 ${item.pinned_version} 版` : ''}
          </p>
          <p className="hint">
            {goals.data?.items.find((g) => g.goal_id === item.goal_id)?.management.details.name ??
              '关联学习目标'}{' '}
            · {new Date(item.updated_at).toLocaleString('zh-CN')} ·{' '}
            {
              { available: '来源可核验', missing: '片段缺失', restricted: '来源已不可用' }[
                item.source_status
              ]
            }
          </p>
          <Link
            to="/spaces/$spaceId/learn/$sessionId"
            params={{ spaceId, sessionId: item.session_id }}
          >
            打开关联活动
          </Link>
        </article>
      ))}
      <Pagination
        page={cursors.length}
        previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
        next={
          items.data?.next_cursor
            ? () => setCursors([...cursors, items.data!.next_cursor!])
            : undefined
        }
      />
    </section>
  )
}
