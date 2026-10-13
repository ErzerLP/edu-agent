import { Link, Outlet, useParams, useSearch } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { BookOpen, Menu } from 'lucide-react'
import { Button } from './ui/button'
import { NavigationDrawer } from './notebook/navigation-drawer'
import { NotebookChrome } from './notebook/notebook-chrome'
import { z } from 'zod'

export function WorkspaceShell() {
  const { spaceId } = useParams({ strict: false })
  const search = useSearch({ strict: false })
  const [menuOpen, setMenuOpen] = useState(false)
  const [header, setHeader] = useState<HTMLElement | null>(null)
  const [menu, setMenu] = useState<HTMLElement | null>(null)
  const selectedSpace = z.uuid().safeParse(spaceId ?? search.space)
  const currentSpace = selectedSpace.success ? selectedSpace.data : undefined
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
    <NotebookChrome.Provider value={{ header, menu }}>
      <a className="skip-link" href="#main">
        跳到主内容
      </a>
      <header className="site-header notebook-site-header">
        <Button
          variant="ghost"
          aria-label="打开侧边菜单"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen(true)}
        >
          <Menu aria-hidden="true" size={24} />
        </Button>
        <Link to="/" className="brand" aria-label="所有学习区">
          <span className="notebook-brand-mark">
            <BookOpen aria-hidden="true" size={23} />
          </span>
          <span className="notebook-brand-text">edu-agent</span>
        </Link>
        <div className="notebook-header-slot" ref={setHeader} />
        <span className="notebook-home-top-link">我的学习空间</span>
        <Link className="notebook-avatar" to="/settings/devices" aria-label="我的账户">
          我
        </Link>
      </header>
      {menuOpen && (
        <NavigationDrawer
          spaceId={currentSpace}
          theme={theme}
          onThemeChange={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
          onClose={() => setMenuOpen(false)}
          onOptionsMount={setMenu}
        />
      )}
      <main id="main" className="main" tabIndex={-1}>
        <Outlet />
      </main>
      <footer>从目标出发 · 按你的节奏学习</footer>
    </NotebookChrome.Provider>
  )
}
