import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useIdentity } from './lib/session'
import { learningClient, unwrap } from './api/client'
import { pageOf, spaceSchema } from './api/runtime'
import { usePage } from './pages'
import { SpaceDialog } from './components/notebook/space-dialog'
import { EmptyState, ErrorState, Pagination } from './components/common'
import { NotebookCard } from './components/notebook/notebook-card'
import { Sparkles } from 'lucide-react'
import type { Space } from './api/runtime'

export function NotebookHomePage() {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const [search, setSearch] = useState('')
  const [cardContainer, setCardContainer] = useState<HTMLDivElement | null>(null)
  const [status, setStatus] = useState<'active' | 'archived' | 'all'>('active')
  const pages = usePage()
  const onCreated = (space: Space, addSources: boolean) =>
    void navigate({
      to: '/spaces/$spaceId',
      params: { spaceId: space.id },
      search: addSources ? { panel: 'sources', add_sources: true } : {},
    })
  const spaces = useQuery({
    queryKey: [...prefix, 'spaces', search, status, pages.cursor],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces', {
          params: {
            query: {
              search,
              status: status === 'all' ? undefined : status,
              cursor: pages.cursor,
              limit: 10,
            },
          },
          signal,
        }),
        pageOf(spaceSchema),
      ),
    enabled: session.capabilities.spaces,
    gcTime: 0,
  })
  return (
    <section className="notebook-home">
      <div className="notebook-home-intro">
        <div className="notebook-home-kicker">
          <Sparkles size={16} aria-hidden="true" />
          你的个人学习空间
        </div>
        <h1>你的学习区</h1>
        <p>把资料放在一起，和 AI 聊一聊，让每一次好奇都变成新的理解。</p>
      </div>
      <div className="filters notebook-home-filters">
        {session.capabilities.spaces && (
          <SpaceDialog onCreated={onCreated} cardContainer={cardContainer} />
        )}
        <label>
          <span className="sr-only">搜索学习区</span>
          <input
            type="search"
            placeholder="搜索学习区"
            value={search}
            onChange={(event) => {
              setSearch(event.target.value)
              pages.reset()
            }}
          />
        </label>
        <label>
          <span className="sr-only">学习区状态</span>
          <select
            value={status}
            onChange={(event) => {
              setStatus(event.target.value as typeof status)
              pages.reset()
            }}
          >
            <option value="active">进行中</option>
            <option value="archived">已归档</option>
            <option value="all">全部</option>
          </select>
        </label>
      </div>
      {!session.capabilities.spaces && <EmptyState>当前服务尚不支持学习区。</EmptyState>}
      {spaces.isPending && session.capabilities.spaces && <p role="status">正在加载学习区…</p>}
      {spaces.error && <ErrorState error={spaces.error} retry={() => void spaces.refetch()} />}
      {spaces.data?.items.length === 0 && (
        <EmptyState>没有匹配的学习区，试试其他搜索条件或新建一个。</EmptyState>
      )}
      <div className="notebook-card-wall">
        {session.capabilities.spaces && (
          <div className="notebook-new-card" ref={setCardContainer} />
        )}
        {spaces.data?.items.map((space) => (
          <NotebookCard key={space.id} space={space} />
        ))}
      </div>
      {spaces.data && (
        <Pagination
          page={pages.page}
          previous={pages.previous}
          next={spaces.data.next_cursor ? () => pages.next(spaces.data!.next_cursor!) : undefined}
        />
      )}
    </section>
  )
}
