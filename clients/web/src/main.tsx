import React from 'react'
import ReactDOM from 'react-dom/client'
import {
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
  useParams,
} from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { SessionProvider } from './lib/session'
import { WorkspaceShell } from './components/workspace-shell'
import { HomePage, SpacePage, GoalPage, SettingsPage } from './pages'
import './styles.css'
import { ResearchPage } from './research-page'
import { TeachingPage, ContentPage } from './teaching-page'

const root = createRootRoute({
  component: () => (
    <SessionProvider>
      <WorkspaceShell />
    </SessionProvider>
  ),
  notFoundComponent: () => (
    <section>
      <h1>页面不存在</h1>
      <a href="/app/">返回学习首页</a>
    </section>
  ),
})
const home = createRoute({ getParentRoute: () => root, path: '/', component: HomePage })
const space = createRoute({
  getParentRoute: () => root,
  path: '/spaces/$spaceId',
  component: () => {
    const { spaceId } = useParams({ from: '/spaces/$spaceId' })
    return <SpacePage key={spaceId} spaceId={spaceId} />
  },
})
const goal = createRoute({
  getParentRoute: () => root,
  path: '/spaces/$spaceId/goals/$goalId',
  component: () => {
    const { spaceId, goalId } = useParams({ from: '/spaces/$spaceId/goals/$goalId' })
    return <GoalPage key={`${spaceId}:${goalId}`} spaceId={spaceId} goalId={goalId} />
  },
})
const settings = createRoute({
  getParentRoute: () => root,
  path: '/settings',
  component: SettingsPage,
})
const research = createRoute({
  getParentRoute: () => root,
  path: '/spaces/$spaceId/goals/$goalId/research',
  component: () => {
    const { spaceId, goalId } = useParams({ from: '/spaces/$spaceId/goals/$goalId/research' })
    return <ResearchPage key={`${spaceId}:${goalId}`} spaceId={spaceId} goalId={goalId} />
  },
})
const teaching = createRoute({
  getParentRoute: () => root,
  path: '/spaces/$spaceId/learn/$sessionId',
  component: () => {
    const { spaceId, sessionId } = useParams({ from: '/spaces/$spaceId/learn/$sessionId' })
    return <TeachingPage key={`${spaceId}:${sessionId}`} spaceId={spaceId} sessionId={sessionId} />
  },
})
const content = createRoute({
  getParentRoute: () => root,
  path: '/content/$artifactId',
  validateSearch: (search: Record<string, unknown>) => ({
    space: typeof search.space === 'string' ? search.space : '00000000-0000-4000-8000-000000000001',
    version:
      Number.isSafeInteger(Number(search.version)) && Number(search.version) > 0
        ? Number(search.version)
        : undefined,
  }),
  component: () => {
    const { artifactId } = content.useParams()
    const { space, version } = content.useSearch()
    return (
      <ContentPage
        key={`${space}:${artifactId}:${version}`}
        artifactId={artifactId}
        spaceId={space}
        version={version}
      />
    )
  },
})
const router = createRouter({
  routeTree: root.addChildren([home, space, goal, research, teaching, content, settings]),
  basepath: '/app',
  defaultPreload: false,
})
declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: false, staleTime: 0, refetchOnWindowFocus: true },
    mutations: { retry: false },
  },
})
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </React.StrictMode>,
)
