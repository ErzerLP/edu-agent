import { test, expect, chromium } from '@playwright/test'
import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'
import {
  createVault,
  offlineFixture,
  offlineOrigin,
  offlinePassword,
  pairOffline,
  unlockVault,
  serverCommand,
} from './offline-fixture'

test('真实签名包断网作答、浏览器重启和响应丢失后核对原操作', async ({ request }) => {
  test.setTimeout(150_000)
  const fixture = await offlineFixture(request)
  const profile = await mkdtemp(join(tmpdir(), 'edu-offline-browser-'))
  const launch = () =>
    chromium.launchPersistentContext(profile, {
      headless: true,
      executablePath: process.env.WEB_CHROMIUM_PATH,
    })
  let context = await launch()
  try {
    let page = context.pages()[0]
    const cdp = await context.newCDPSession(page)
    await cdp.send('Browser.grantPermissions', {
      permissions: ['durableStorage'],
      origin: offlineOrigin,
    })
    await pairOffline(page)
    await createVault(page, fixture.path)
    await page.getByRole('button', { name: '下载并加密保存此会话的离线包' }).click()
    await expect(page.getByText('签名包已加密保存并回读核对，可以断网阅读。')).toBeVisible()
    await context.setOffline(true)
    await page.getByLabel('离线答案').fill('A')
    await page.getByRole('button', { name: '加密保存原答案' }).click()
    await expect(page.getByText('已保存未同步', { exact: true })).toBeVisible()
    const stored = await page.evaluate(async () => {
      const db = await new Promise<IDBDatabase>((resolve, reject) => {
        const r = indexedDB.open('edu-browser-offline-v1')
        r.onsuccess = () => resolve(r.result)
        r.onerror = () => reject(r.error)
      })
      const result = await new Promise<unknown>((resolve, reject) => {
        const r = db.transaction('vault').objectStore('vault').getAll()
        r.onsuccess = () => resolve(r.result)
        r.onerror = () => reject(r.error)
      })
      db.close()
      const cacheKeys: string[] = []
      for (const name of await caches.keys())
        for (const req of await (await caches.open(name)).keys()) cacheKeys.push(req.url)
      return { text: JSON.stringify(result), local: JSON.stringify(localStorage), cacheKeys }
    })
    expect(stored.text).not.toContain('偶数')
    expect(stored.text).not.toContain(offlinePassword)
    expect(stored.local).not.toContain('偶数')
    expect(
      stored.cacheKeys.every(
        (url) =>
          url.startsWith(`${offlineOrigin}/app/assets/`) || url === `${offlineOrigin}/app/offline`,
      ),
    ).toBe(true)
    await context.close()
    context = await launch()
    await context.setOffline(true)
    page = context.pages()[0]
    await page.goto(`${offlineOrigin}/app/offline?space=${randomUUID()}&goal=${randomUUID()}`)
    await unlockVault(page)
    await expect(page.getByText('已保存未同步', { exact: true })).toBeVisible()
    await expect(
      page.getByText(`原学习区 ${fixture.path.split('space=')[1].split('&')[0]}`, { exact: false }),
    ).toBeVisible()
    const calls: string[] = []
    let lost = false
    await page.route('**/v1/web/offline/sync', async (route) => {
      calls.push('sync')
      expect(route.request().headers()['x-learning-space-id']).toBeUndefined()
      if (!lost) {
        lost = true
        await route.fetch()
        await route.abort('failed')
      } else await route.continue()
    })
    page.on('request', (req) => {
      if (req.url().includes('/offline/operations/')) calls.push('status')
    })
    await context.setOffline(false)
    await expect(page.getByText('同步结果未知', { exact: true })).toBeVisible()
    await page.getByRole('button', { name: '核对原操作并同步' }).click()
    await expect(page.getByText('服务端已确认', { exact: true })).toBeVisible()
    expect(calls).toEqual(['sync', 'status'])
    await expect(page.getByText('服务端已接纳学习证据', { exact: true })).toBeVisible()
    expect((await fixture.get()).session).toEqual(fixture.view.session)
    await page.getByRole('button', { name: '锁定离线库', exact: true }).click()
    await expect(page.getByText('根据原资料，哪个是偶数？', { exact: false })).not.toBeVisible()
    await page.getByLabel('解锁口令', { exact: true }).fill('错误口令-123456789')
    await page.getByRole('button', { name: '解锁', exact: true }).click()
    await expect(page.getByRole('alert')).toContainText('无法解锁')
  } finally {
    await context.close()
  }
})

test('存储故障不假保存、两个标签页只保存一次、损坏与本地清除可核对', async ({ request }) => {
  test.setTimeout(150_000)
  const fixture = await offlineFixture(request)
  const profile = await mkdtemp(join(tmpdir(), 'edu-offline-fault-'))
  const context = await chromium.launchPersistentContext(profile, {
    headless: true,
    executablePath: process.env.WEB_CHROMIUM_PATH,
  })
  try {
    const page = context.pages()[0]
    await (
      await context.newCDPSession(page)
    ).send('Browser.grantPermissions', { permissions: ['durableStorage'], origin: offlineOrigin })
    await pairOffline(page)
    await createVault(page, fixture.path)
    await page.getByRole('button', { name: '下载并加密保存此会话的离线包' }).click()
    await expect(page.getByLabel('离线答案')).toBeVisible()
    await context.setOffline(true)
    await page.evaluate(() => {
      const original = IDBObjectStore.prototype.put
      ;(window as unknown as { restorePut: () => void }).restorePut = () => {
        IDBObjectStore.prototype.put = original
      }
      IDBObjectStore.prototype.put = function (value, key) {
        if (key === 'snapshot') throw new DOMException('验收配额不足', 'QuotaExceededError')
        return original.call(this, value, key)
      }
    })
    await page.getByLabel('离线答案').fill('尚未保存的敏感答案')
    await page.getByRole('button', { name: '加密保存原答案' }).click()
    await expect(page.getByRole('alert')).toContainText('未保存')
    await expect(page.getByText('已保存未同步', { exact: true })).not.toBeVisible()
    await expect(page.getByLabel('离线答案')).toHaveValue('尚未保存的敏感答案')
    await page.evaluate(() => (window as unknown as { restorePut: () => void }).restorePut())
    const other = await context.newPage()
    await other.goto(`${offlineOrigin}/app/offline`)
    await unlockVault(other)
    await page.getByLabel('离线答案').fill('A')
    await other.getByLabel('离线答案').fill('B')
    await Promise.all([
      page.getByRole('button', { name: '加密保存原答案' }).click(),
      other.getByRole('button', { name: '加密保存原答案' }).click(),
    ])
    await expect(page.getByText('已保存未同步', { exact: true })).toBeVisible()
    await expect(other.getByText('已保存未同步', { exact: true })).toBeVisible()
    await expect(page.getByText('存储状态：已解锁', { exact: false })).toContainText('1 条')
    await page.getByRole('button', { name: '锁定离线库', exact: true }).click()
    await expect(other.getByRole('heading', { name: '解锁本地离线库' })).toBeVisible()
    await page.evaluate(async () => {
      const db = await new Promise<IDBDatabase>((resolve) => {
        const r = indexedDB.open('edu-browser-offline-v1')
        r.onsuccess = () => resolve(r.result)
      })
      await new Promise<void>((resolve) => {
        const tx = db.transaction('vault', 'readwrite')
        tx.objectStore('vault').delete('snapshot')
        tx.oncomplete = () => resolve()
      })
      db.close()
    })
    await page.getByLabel('解锁口令', { exact: true }).fill(offlinePassword)
    await page.getByRole('button', { name: '解锁', exact: true }).click()
    await expect(page.getByRole('alert')).toContainText('部分丢失')
    page.once('dialog', (dialog) => {
      expect(dialog.message()).toContain('未知数量')
      void dialog.accept()
    })
    await page.getByRole('button', { name: '清除全部本地离线数据' }).click()
    await expect(page.getByText('本地库和缓存已清除。', { exact: false })).toBeVisible()
    expect(
      await page.evaluate(async () => ({
        db: (await indexedDB.databases()).some((db) => db.name === 'edu-browser-offline-v1'),
        marker: localStorage.getItem('edu-browser-offline-presence-v1'),
        cache: (await caches.keys()).filter((name) => name.startsWith('edu-offline-shell-')),
      })),
    ).toEqual({ db: false, marker: null, cache: [] })
  } finally {
    await context.close()
  }
})

test('没有持久化许可或配额时不创建离线库，也不写明文', async ({ page }) => {
  await pairOffline(page)
  await page.goto(`${offlineOrigin}/app/offline`)
  expect(await page.evaluate(() => indexedDB.databases())).toEqual([])
  await page.evaluate(() => {
    navigator.storage.persist = async () => false
  })
  await page.getByLabel('解锁口令', { exact: true }).fill(offlinePassword)
  await page.getByLabel('再次输入口令').fill(offlinePassword)
  await page.getByRole('checkbox').check()
  await page.getByRole('button', { name: '授权并创建加密离线库' }).click()
  await expect(page.getByRole('alert')).toContainText('持久存储权限未获批准')
  expect(await page.evaluate(() => indexedDB.databases())).toEqual([])
  await page.evaluate(() => {
    navigator.storage.persist = async () => true
    navigator.storage.estimate = async () => ({ quota: 1024, usage: 1000 })
  })
  await page.getByLabel('解锁口令', { exact: true }).fill(offlinePassword)
  await page.getByLabel('再次输入口令').fill(offlinePassword)
  await page.getByRole('button', { name: '授权并创建加密离线库' }).click()
  await expect(page.getByRole('alert')).toContainText('配额不足')
  expect(await page.evaluate(() => indexedDB.databases())).toEqual([])
  await page.addInitScript(() => {
    Object.defineProperty(window, 'BroadcastChannel', { value: undefined })
  })
  await page.reload()
  await expect(page.getByRole('alert')).toContainText('浏览器缺少')
  await expect(page.getByRole('button', { name: '授权并创建加密离线库' })).not.toBeVisible()
})

test('真实断网设备重连清除，丢失 purge 响应后刷新核对原回执', async ({ request }) => {
  test.setTimeout(150_000)
  const fixture = await offlineFixture(request)
  const profile = await mkdtemp(join(tmpdir(), 'edu-offline-purge-'))
  const context = await chromium.launchPersistentContext(profile, {
    headless: true,
    executablePath: process.env.WEB_CHROMIUM_PATH,
  })
  try {
    const page = context.pages()[0]
    await (
      await context.newCDPSession(page)
    ).send('Browser.grantPermissions', { permissions: ['durableStorage'], origin: offlineOrigin })
    await pairOffline(page)
    await createVault(page, fixture.path)
    await page.getByRole('button', { name: '下载并加密保存此会话的离线包' }).click()
    await expect(page.getByLabel('离线答案')).toBeVisible()
    const session = await (
      await page.request.get(`${offlineOrigin}/v1/web/offline/session`, {
        headers: { 'X-Offline-Adapter': '1' },
      })
    ).json()
    const other = await context.newPage()
    await other.goto(`${offlineOrigin}/app/offline`)
    await unlockVault(other)
    await context.setOffline(true)
    const grant = serverCommand(['privacy-grant', 'create', '--device', fixture.identity.device.id])
    const clear = await request.post(`${offlineOrigin}/v1/privacy/erasures`, {
      headers: {
        Authorization: `Bearer ${fixture.identity.token}`,
        'X-Privacy-Erasure-Grant': grant,
      },
      data: {
        operation_id: randomUUID(),
        payload_schema_version: 1,
        expected_current_learner_generation: Number(session.generation),
        reason_code: 'learner_request',
        explicit_confirmation: true,
      },
    })
    expect(clear.ok(), await clear.text()).toBe(true)
    const receipt = await clear.json()
    expect(receipt.status).not.toBe('verified')
    // 断网时不能声称服务器已经擦除了浏览器；旧页面仍显示实际本地内容。
    await expect(page.getByLabel('离线答案')).toBeVisible()
    let ack: { status: string; device_id: string } | undefined
    await context.route('**/v1/web/offline/purge/*/ack', async (route) => {
      const response = await route.fetch()
      if (response.ok()) ack = await response.json()
      // 服务端已提交清除回执，但两个页面都收不到响应。
      await route.abort('failed')
    })
    await context.setOffline(false)
    await expect.poll(() => ack?.status).toBe('succeeded')
    expect(ack?.device_id).toBe(session.device_id)
    await expect(page.getByLabel('离线答案')).not.toBeVisible()
    await expect(other.getByLabel('离线答案')).not.toBeVisible()
    expect(
      await page.evaluate(async () => ({
        db: (await indexedDB.databases()).some((db) => db.name === 'edu-browser-offline-v1'),
        cache: (await caches.keys()).filter((name) => name.startsWith('edu-offline-shell-')),
      })),
    ).toEqual({ db: false, cache: [] })
    await expect(page.getByText('清除或正式回执尚未完成，请联网核对后重试。')).toBeVisible()
    await other.close()
    await context.unroute('**/v1/web/offline/purge/*/ack')
    await page.reload()
    await expect(page.getByText('本地受管数据已清除，正式清除回执已核对。')).toBeVisible()
    const recovered = await (
      await page.request.get(`${offlineOrigin}/v1/web/offline/session`, {
        headers: { 'X-Offline-Adapter': '1' },
      })
    ).json()
    expect(recovered.purge).toBeNull()
    expect(recovered.purge_receipt).toMatchObject({
      device_id: session.device_id,
      status: 'succeeded',
      source_generation: Number(session.generation),
    })
    const oldSync = await page.request.post(`${offlineOrigin}/v1/web/offline/sync`, {
      headers: {
        Origin: offlineOrigin,
        'X-Offline-Adapter': '1',
        'X-CSRF-Token': session.csrf_token,
        'X-Web-Principal-ID': session.device_id,
        'X-Web-Generation': session.generation,
      },
      data: {},
    })
    expect(oldSync.status()).toBe(409)
    expect((await oldSync.json()).error.code).toBe('offline_purge_required')
  } finally {
    await context.close()
  }
})
