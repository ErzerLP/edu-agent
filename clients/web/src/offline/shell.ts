export async function saveShell(): Promise<void> {
  const registration = await navigator.serviceWorker.register('/app/offline-sw.js', {
    scope: '/app/',
    updateViaCache: 'none',
  })
  if (!registration.active)
    await new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(
        () => reject(new Error('离线页面安装未完成，不能承诺离线可用')),
        30_000,
      )
      const worker = registration.installing ?? registration.waiting
      if (!worker) {
        clearTimeout(timeout)
        reject(new Error('离线页面安装失败'))
        return
      }
      const check = () => {
        if (worker.state === 'activated') {
          clearTimeout(timeout)
          resolve()
        } else if (worker.state === 'redundant') {
          clearTimeout(timeout)
          reject(new Error('离线资产安装失败'))
        }
      }
      worker.addEventListener('statechange', check)
      check()
    })
  const worker = registration.active
  if (!worker) throw new Error('离线页面尚未可用')
  const version = await new Promise<{ adapter: number; cache: string }>((resolve, reject) => {
    const channel = new MessageChannel()
    const timeout = setTimeout(() => {
      channel.port1.close()
      reject(new Error('离线页面协议核对超时'))
    }, 5000)
    channel.port1.onmessage = (event) => {
      clearTimeout(timeout)
      channel.port1.close()
      resolve(event.data)
    }
    worker.postMessage('version', [channel.port2])
  })
  if (
    version.adapter !== 1 ||
    !version.cache.startsWith('edu-offline-shell-v1-') ||
    !(await (await caches.open(version.cache)).match('/app/offline'))
  )
    throw new Error('离线页面与存储协议不匹配')
}
