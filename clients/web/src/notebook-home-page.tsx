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

export function NotebookHomePage() {
  const { session, prefix } = useIdentity()
  const navigate = useNavigate()
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<'active' | 'archived' | 'all'>('active')
  const pages = usePage()
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
        <h1>你的学习区</h1>
        <p>把好奇心收进学习区，从资料出发，逐步学会想做的事。</p>
      </div>
      <div className="filters notebook-home-filters">
        <label>
          搜索学习区
          <input
            type="search"
            value={search}
            onChange={(event) => {
              setSearch(event.target.value)
              pages.reset()
            }}
          />
        </label>
        <label>
          学习区状态
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
          <div className="notebook-new-card">
            <SpaceDialog
              onCreated={(space, addSources) =>
                void navigate({ to: '/spaces/$spaceId', params: { spaceId: space.id }, search: addSources ? { panel: 'sources', add_sources: true } : {} })
              }
            />
          </div>
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
