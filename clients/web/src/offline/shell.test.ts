import { runInNewContext } from 'node:vm'
import { expect, it, vi } from 'vitest'
// @ts-expect-error 与 Vite 共用原生 ESM 构建插件。
import { offlineWorker } from '../../scripts/offline-worker.mjs'

function worker(changedShell = false) {
  let source = ''
  const files = {
    'index.html': { type: 'asset', source: '<script src="/app/assets/main.js"></script>' },
    'assets/main.js': { type: 'chunk', code: 'export {}' },
  }
  offlineWorker().generateBundle.call(
    {
      emitFile: (file: { source: string }) => {
        source = file.source
      },
    },
    {},
    files,
  )
  const listeners: Record<string, (event: unknown) => void> = {}
  const entries = new Map<string, Response>()
  const fetch = vi.fn(
    async (path: string, _options: RequestInit) =>
      new Response(
        path === '/app/offline'
          ? changedShell
            ? '来自另一构建的页面'
            : files['index.html'].source
          : files['assets/main.js'].code,
      ),
  )
  const remove = vi.fn(async () => {
    entries.clear()
    return true
  })
  runInNewContext(source, {
    self: {
      addEventListener: (name: string, fn: (event: unknown) => void) => {
        listeners[name] = fn
      },
      location: { origin: 'https://learn.example' },
    },
    caches: {
      open: async () => ({
        put: async (path: string, response: Response) => {
          entries.set(path, response)
        },
        match: async (path: string) => entries.get(path),
      }),
      delete: remove,
    },
    crypto,
    Uint8Array,
    URL,
    fetch,
  })
  const install = () =>
    new Promise<void>((resolve, reject) =>
      listeners.install({ waitUntil: (job: Promise<void>) => job.then(resolve, reject) }),
    )
  return { install, entries, fetch, remove, listeners }
}

it('只缓存同一构建白名单，安装不携带身份，API 和外部资源不被拦截', async () => {
  const w = worker()
  await w.install()
  expect([...w.entries.keys()].sort()).toEqual(['/app/assets/main.js', '/app/offline'])
  for (const call of w.fetch.mock.calls)
    expect(call[1]).toEqual({ credentials: 'omit', cache: 'no-store', redirect: 'error' })
  for (const url of [
    'https://learn.example/v1/web/session',
    'https://learn.example/v1/web/offline/sync',
    'https://learn.example/app/assets/main.js?private=1',
    'https://external.example/app/assets/main.js',
  ]) {
    const respondWith = vi.fn()
    w.listeners.fetch({ request: { url, method: 'GET', mode: 'cors' }, respondWith })
    expect(respondWith).not.toHaveBeenCalled()
  }
})
it('混合部署的页面摘要不符时安装失败并删除未完成缓存', async () => {
  const w = worker(true)
  await expect(w.install()).rejects.toThrow('不是同一构建版本')
  expect(w.remove).toHaveBeenCalledOnce()
  expect(w.entries.size).toBe(0)
})
