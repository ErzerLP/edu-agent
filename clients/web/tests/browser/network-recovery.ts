import { expect, type BrowserContext, type Page, type Request } from '@playwright/test'

export async function reconnectAndWaitForReads(
  page: Page,
  context: BrowserContext,
  recoveredPath: string,
) {
  const origin = new URL(page.url()).origin
  const pending = new Set<Request>()
  const started = (request: Request) => {
    const url = new URL(request.url())
    // SSE 持续开放；恢复后刷新只等待会话校验、快照等有限 API 读取。
    if (
      request.method() === 'GET' &&
      url.origin === origin &&
      url.pathname.startsWith('/v1/') &&
      !/^\/v1\/learning\/runs\/[^/]+\/events$/.test(url.pathname)
    )
      pending.add(request)
  }
  const finished = (request: Request) => {
    pending.delete(request)
  }
  page.on('request', started)
  page.on('requestfinished', finished)
  page.on('requestfailed', finished)
  try {
    await context.setOffline(true)
    const [response] = await Promise.all([
      page.waitForResponse((response) => {
        const url = new URL(response.url())
        return (
          response.request().method() === 'GET' &&
          url.origin === origin &&
          url.pathname === recoveredPath &&
          response.ok()
        )
      }),
      context.setOffline(false),
    ])
    expect(await response.finished(), '重连读取的响应体应完整接收').toBeNull()
    await expect
      .poll(() => pending.size, {
        message: '重连的 API 读取应完成后再刷新，避免会话校验后续请求落入页面卸载阶段',
      })
      .toBe(0)
  } finally {
    page.off('request', started)
    page.off('requestfinished', finished)
    page.off('requestfailed', finished)
  }
}
