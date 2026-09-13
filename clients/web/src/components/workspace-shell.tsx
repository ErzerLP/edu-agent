import { Link, Outlet, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { BookOpen, Moon, Sun } from 'lucide-react'
import { useIdentity } from '@/lib/session'
import { learningClient, unwrap } from '@/api/client'
import { pageOf, spaceSchema } from '@/api/runtime'
import { Button } from './ui/button'
import { ErrorState, Confirm } from './common'

export function SpaceSwitcher() {
  const { session, prefix } = useIdentity()
  const { spaceId } = useParams({ strict: false })
  const spaces = useQuery({
    queryKey: [...prefix, 'spaces', 'switcher'],
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces', {
          params: { query: { status: 'active', limit: 100 } },
          signal,
        }),
        pageOf(spaceSchema),
      ),
    enabled: session.capabilities.spaces,
  })
  return (
    <details className="space-switcher">
      <summary>切换学习区</summary>
      <div className="switcher-menu">
        {spaces.error && <ErrorState error={spaces.error} retry={() => void spaces.refetch()} />}
        {spaces.data?.items.map((space) => (
          <Link
            key={space.id}
            to="/spaces/$spaceId"
            params={{ spaceId: space.id }}
            aria-current={spaceId === space.id ? 'page' : undefined}
          >
            {space.name}
          </Link>
        ))}
        <Link to="/">查看与管理全部学习区</Link>
      </div>
    </details>
  )
}

export function WorkspaceShell() {
  const { logout, drafts } = useIdentity()
  const [error, setError] = useState<unknown>()
  const [theme, setTheme] = useState(() => {
    try {
      return localStorage.getItem('knowledge-mesh-theme') ?? 'light'
    } catch {
      return 'light'
    }
  })
  useEffect(() => {
    document.documentElement.dataset.theme = theme
    try {
      localStorage.setItem('knowledge-mesh-theme', theme)
    } catch {
      /* 无法保存偏好时仍可使用当前主题。 */
    }
  }, [theme])
  return (
    <>
      <a className="skip-link" href="#main">
        跳到主内容
      </a>
      <header className="site-header">
        <Link to="/" className="brand">
          <BookOpen aria-hidden="true" size={24} />
          <span>
            知络<small>Knowledge Mesh</small>
          </span>
        </Link>
        <nav aria-label="全局导航">
          <Link to="/">学习目标</Link>
          <SpaceSwitcher />
          <Link to="/settings">设置与能力</Link>
          <Button
            variant="ghost"
            onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
            aria-label={theme === 'dark' ? '切换浅色主题' : '切换深色主题'}
          >
            {theme === 'dark' ? <Sun aria-hidden="true" /> : <Moon aria-hidden="true" />}
          </Button>
          <Confirm
            label="退出"
            title="退出浏览器学习会话？"
            onConfirm={() => void logout().catch(setError)}
          >
            {drafts.dirty
              ? '未保存草稿会丢失。退出不会撤销设备。'
              : '已保存的目标会保留。退出不会撤销设备。'}
          </Confirm>
        </nav>
      </header>
      <main id="main" className="main" tabIndex={-1}>
        {!!error && <ErrorState error={error} />}
        <Outlet />
      </main>
      <footer>从目标出发 · 按你的节奏学习</footer>
    </>
  )
}
