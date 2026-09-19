import { createHash } from 'node:crypto'

// 只把构建产物加入白名单；不使用运行时 URL、API 响应或浏览器身份生成缓存。
export function offlineWorker() {
  return {
    name: 'offline-shell',
    enforce: 'post',
    generateBundle(_, bundle) {
      const files = Object.keys(bundle)
        .filter((name) => name === 'index.html' || name.startsWith('assets/'))
        .sort()
      const hash = createHash('sha256')
      for (const file of files)
        hash
          .update(file)
          .update(bundle[file].type === 'chunk' ? bundle[file].code : bundle[file].source)
      const version = hash.digest('hex').slice(0, 20)
      const assets = files.filter((file) => file !== 'index.html').map((file) => `/app/${file}`)
      if (!files.includes('index.html')) throw new Error('离线构建缺少应用壳，拒绝生成缓存')
      const digests = Object.fromEntries(
        files.map((file) => [
          file === 'index.html' ? '/app/offline' : `/app/${file}`,
          createHash('sha256')
            .update(bundle[file].type === 'chunk' ? bundle[file].code : bundle[file].source)
            .digest('hex'),
        ]),
      )
      const source = `
const CACHE = 'edu-offline-shell-v1-${version}';
const ASSETS = ${JSON.stringify(assets)};
const SHELL = '/app/offline';
const ALLOWED = new Set([...ASSETS, SHELL]);
const DIGESTS = ${JSON.stringify(digests)};
self.addEventListener('install', event => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    try {
      for (const path of ALLOWED) {
        const response = await fetch(path, {credentials: 'omit', cache: 'no-store', redirect: 'error'});
        if (!response.ok || response.headers.has('Set-Cookie')) throw new Error('离线资产不可用');
        const digest = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', await response.clone().arrayBuffer())), b => b.toString(16).padStart(2, '0')).join('');
        if (digest !== DIGESTS[path]) throw new Error('应用壳或资产不是同一构建版本，拒绝保存');
        await cache.put(path, response);
      }
    } catch (error) { await caches.delete(CACHE); throw error; }
  })());
});
self.addEventListener('activate', event => event.waitUntil((async () => {
  for (const name of await caches.keys()) if (name.startsWith('edu-offline-shell-') && name !== CACHE) await caches.delete(name);
  await self.clients.claim();
})()));
self.addEventListener('message', event => {
  if (event.data === 'version' && event.ports[0]) event.ports[0].postMessage({adapter: 1, cache: CACHE});
});
self.addEventListener('fetch', event => {
  const url = new URL(event.request.url);
  if (event.request.method !== 'GET' || url.origin !== self.location.origin) return;
  if (event.request.mode === 'navigate' && url.pathname === SHELL) {
    event.respondWith((async () => {
      const cached = await (await caches.open(CACHE)).match(SHELL);
      if (cached) return cached;
      return fetch(event.request);
    })());
  } else if (!url.search && ASSETS.includes(url.pathname)) {
    event.respondWith((async () => {
      const cached = await (await caches.open(CACHE)).match(url.pathname);
      return cached || fetch(event.request);
    })());
  }
});`
      this.emitFile({ type: 'asset', fileName: 'offline-sw.js', source })
    },
  }
}
