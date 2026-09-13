import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { readFile } from 'node:fs/promises'

async function pair(page: Page) {
  const code = execFileSync('../../server/edu-agentd', ['pairing-code', 'create'], {
    encoding: 'utf8',
    env: {
      PATH: process.env.PATH,
      DATABASE_URL: process.env.TEST_DATABASE_URL,
      MIGRATE_ON_START: 'true',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
}
async function api(
  page: Page,
  method: 'POST' | 'PUT',
  path: string,
  body: unknown,
  spaceId?: string,
) {
  const session = await (await page.request.get('/v1/web/session')).json()
  const response = await page.request.fetch(path, {
    method,
    data: body,
    headers: {
      Origin: 'http://127.0.0.1:32929',
      'X-CSRF-Token': session.csrf_token,
      ...(spaceId ? { 'X-Learning-Space-ID': spaceId } : {}),
    },
  })
  expect(response.ok(), await response.text()).toBeTruthy()
  return response.json()
}
async function newSpace(page: Page, name: string) {
  return api(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name,
    description: '',
    status: 'active',
  })
}
async function newGoal(page: Page, spaceId: string, text: string, details?: unknown) {
  const response = await api(
    page,
    'POST',
    '/v1/learning/goals',
    {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'goal',
      aggregate_id: randomUUID(),
      expected_version: 0,
      text,
      source: 'browser-test',
      ...(details ? { details } : {}),
    },
    spaceId,
  )
  return response.result
}

test('真实配对、IME 保存、生命周期及深浅主题四个视口', async ({ page }, testInfo) => {
  test.setTimeout(120_000)
  const errors: string[] = []
  const forbidden: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  page.on('request', (req) => {
    if (/\/v1\/(model|tutoring)/.test(req.url())) forbidden.push(req.url())
  })
  await pair(page)
  for (const theme of ['light', 'dark']) {
    if (theme === 'dark') await page.getByRole('button', { name: '切换深色主题' }).click()
    for (const width of [390, 768, 1280, 1440]) {
      await page.setViewportSize({ width, height: 1000 })
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
      ).toBeTruthy()
      const audit = await new AxeBuilder({ page })
        .withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
        .analyze()
      expect(audit.violations.map((v) => v.id)).toEqual([])
      await page.screenshot({
        path: testInfo.outputPath(`home-${theme}-${width}.png`),
        fullPage: true,
      })
    }
  }
  await page.getByRole('button', { name: '切换浅色主题' }).click()
  await expect(page.getByRole('button', { name: '开始学习', exact: true })).toBeDisabled()
  await page.getByText('新建学习区', { exact: true }).click()
  const spaceName = `浏览器真实学习 ${randomUUID().slice(0, 8)}`
  await page.getByLabel('学习区名称', { exact: true }).fill(spaceName)
  await page.getByLabel('学习区说明').fill('无模型、无资料的目标起步')
  await page.getByRole('button', { name: '创建学习区', exact: true }).click()
  await expect(page.getByText('学习区已创建，可从列表进入。')).toBeVisible()
  await page.getByLabel('搜索学习区', { exact: true }).fill(spaceName)
  await page.getByRole('link', { name: spaceName, exact: true }).click()
  const input = page.getByLabel('今天想学会什么？', { exact: true })
  await input.fill('理解并发调度\n能够独立解释竞争条件')
  await input.dispatchEvent('compositionstart')
  await input.dispatchEvent('keydown', { key: 'Enter', code: 'Enter', isComposing: true })
  await input.dispatchEvent('compositionend')
  await expect(page.getByText('目标已保存，尚未启动教学。')).toHaveCount(0)
  await page.getByRole('button', { name: '仅保存目标', exact: true }).click()
  await expect(page.getByText('目标已保存，尚未启动教学。')).toBeVisible()
  await page.getByRole('link', { name: '查看目标', exact: true }).click()
  await page.getByLabel('期望结果', { exact: true }).fill('能写出并解释一个无数据竞争的 Go 程序')
  await page.getByRole('button', { name: '保存修改', exact: true }).click()
  await expect(page.getByText('目标已保存，尚未启动教学。')).toBeVisible()
  await page.getByRole('button', { name: '标为进行中', exact: true }).click()
  await expect(page.getByRole('button', { name: '暂停', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '暂停', exact: true }).click()
  await page.getByRole('button', { name: '恢复', exact: true }).click()
  await expect(page.getByRole('button', { name: '手动完成', exact: true })).toBeDisabled()
  await page.getByLabel('手动完成依据').fill('已经独立完成并发练习并说明结果')
  await page.getByRole('button', { name: '手动完成', exact: true }).click()
  await expect(page.getByRole('button', { name: '取消', exact: true })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(page.getByRole('button', { name: '确认手动完成' })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByText('当前：已完成。', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: '归档目标', exact: true }).click()
  await page.getByRole('button', { name: '确认归档目标' }).click()
  await page.getByRole('button', { name: '恢复目标', exact: true }).click()
  await page.getByRole('button', { name: '确认恢复目标' }).click()
  await expect(page.getByText('当前：已完成。', { exact: false })).toBeVisible()
  for (const theme of ['light', 'dark']) {
    if (theme === 'dark') await page.getByRole('button', { name: '切换深色主题' }).click()
    for (const width of [390, 768, 1280, 1440]) {
      await page.setViewportSize({ width, height: 1000 })
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
      ).toBeTruthy()
      const audit = await new AxeBuilder({ page })
        .withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
        .analyze()
      expect(
        audit.violations.map((v) => ({ id: v.id, nodes: v.nodes.map((n) => n.target) })),
      ).toEqual([])
      await page.screenshot({ path: testInfo.outputPath(`${theme}-${width}.png`), fullPage: true })
    }
  }
  expect(errors).toEqual([])
  expect(forbidden).toEqual([])
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual(['knowledge-mesh-theme'])
  expect(await page.evaluate(() => document.cookie)).toBe('')
  const cookies = await page.context().cookies()
  expect(cookies.filter((c) => c.name === 'edu_web_dev').every((c) => c.httpOnly)).toBeTruthy()
})

test('未就绪能力显示独立提示且不请求目标 API', async ({ page }) => {
  await pair(page)
  await page.route('**/v1/web/session', async (route) => {
    const response = await route.fetch()
    const body = await response.json()
    body.capabilities.goals = false
    body.capabilities.save_goal = false
    await route.fulfill({ response, json: body })
  })
  const requested: string[] = []
  page.on('request', (request) => {
    if (request.url().includes('/v1/learning/goals')) requested.push(request.url())
  })
  await page.reload()
  await expect(page.getByText('当前服务尚不支持目标管理。')).toBeVisible()
  await page.getByRole('link', { name: '进入默认学习区 →' }).click()
  await expect(page.getByText('当前服务尚不支持目标管理。')).toBeVisible()
  await expect(page.getByText('正在读取目标…')).toHaveCount(0)
  expect(requested).toEqual([])
})

test('两标签页、双学习区与迟到响应不会覆盖草稿；失败保留输入', async ({ page, context }) => {
  await pair(page)
  const a = await newSpace(page, '隔离甲')
  const b = await newSpace(page, '隔离乙')
  const one = await newGoal(page, a.id, '目标一')
  const two = await newGoal(page, a.id, '目标二')
  const second = await context.newPage()
  await page.goto(`/app/spaces/${a.id}/goals/${one.goal_id}`)
  await second.goto(`/app/spaces/${a.id}/goals/${two.goal_id}`)
  await page.getByLabel('学习目标', { exact: true }).fill('甲页未提交内容')
  await second.getByLabel('学习目标', { exact: true }).fill('乙页未提交内容')
  await page.getByRole('link', { name: '← 返回隔离甲' }).click()
  await page.getByRole('link', { name: '目标一', exact: true }).click()
  await expect(page.getByLabel('学习目标', { exact: true })).toHaveValue('甲页未提交内容')
  await expect(second.getByLabel('学习目标', { exact: true })).toHaveValue('乙页未提交内容')
  await page.route(`**/v1/learning/goals/${one.goal_id}`, async (route) => {
    if (route.request().method() === 'PUT') await route.abort('failed')
    else await route.continue()
  })
  await page.getByRole('button', { name: '保存修改', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('网络连接失败')
  await expect(page.getByLabel('学习目标', { exact: true })).toHaveValue('甲页未提交内容')
  await page.unroute(`**/v1/learning/goals/${one.goal_id}`)
  let release!: () => void
  let started!: () => void
  const seen = new Promise<void>((resolve) => {
    started = resolve
  })
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route(`**/v1/learning/goals/${one.goal_id}`, async (route) => {
    if (route.request().method() !== 'PUT') {
      await route.continue()
      return
    }
    const response = await route.fetch()
    started()
    await gate
    await route.fulfill({ response })
  })
  await page.getByRole('button', { name: '保存修改', exact: true }).click()
  await seen
  await page.getByText('切换学习区', { exact: true }).click()
  await page.locator(`.switcher-menu a[href="/app/spaces/${b.id}"]`).click()
  await page.getByLabel('今天想学会什么？', { exact: true }).fill('另一个区的新草稿')
  release()
  await expect(page).toHaveURL(new RegExp(`/spaces/${b.id}$`))
  await expect(page.getByLabel('今天想学会什么？', { exact: true })).toHaveValue('另一个区的新草稿')
  await expect(second.getByLabel('学习目标', { exact: true })).toHaveValue('乙页未提交内容')
  await page.close({ runBeforeUnload: false })
  await second.close({ runBeforeUnload: false })
})

test('真实列表分页、筛选、修订历史和学习区归档恢复', async ({ page }) => {
  await pair(page)
  const space = await newSpace(page, '分页学习区')
  for (let i = 0; i < 12; i++) await newGoal(page, space.id, `分页目标 ${i}`)
  await page.goto(`/app/spaces/${space.id}`)
  await expect(page.locator('.goal-row')).toHaveCount(10)
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(page.locator('.goal-row')).toHaveCount(2)
  await page.getByLabel('搜索目标', { exact: true }).fill('分页目标 11')
  await expect(page.locator('.goal-row')).toHaveCount(1)
  await page.getByRole('combobox', { name: '目标状态', exact: true }).selectOption('completed')
  await expect(page.getByText('这里还没有匹配的目标')).toBeVisible()
  await page.getByRole('combobox', { name: '目标状态', exact: true }).selectOption('draft')
  await page.getByRole('link', { name: '分页目标 11', exact: true }).click()
  await expect(page.getByRole('heading', { name: '修订历史' })).toBeVisible()
  await page.getByRole('link', { name: '← 返回分页学习区' }).click()
  await page.getByText('编辑学习区', { exact: true }).click()
  await page.getByLabel('学习区名称').fill('分页学习区已编辑')
  await page.getByRole('button', { name: '保存学习区', exact: true }).click()
  await expect(page.getByRole('heading', { name: '分页学习区已编辑' })).toBeVisible()
  await page.getByRole('button', { name: '归档学习区', exact: true }).click()
  await page.getByRole('button', { name: '确认归档学习区' }).click()
  await expect(page.getByText('此学习区已归档，恢复后可继续保存目标。')).toBeVisible()
  await page.getByRole('button', { name: '恢复学习区', exact: true }).click()
  await page.getByRole('button', { name: '确认恢复学习区' }).click()
  await expect(page.getByRole('button', { name: '仅保存目标', exact: true })).toBeVisible()
})

test('版本冲突保留草稿，生产进程重启恢复目标与 Cookie，退出清理浏览器身份', async ({ page }) => {
  await pair(page)
  const space = await newSpace(page, '冲突与重启')
  const goal = await newGoal(page, space.id, '初始目标', {
    name: '初始目标',
    timezone: 'Asia/Shanghai',
    deadline: '2027-01-01T18:00:00+08:00',
  })
  await page.goto(`/app/spaces/${space.id}/goals/${goal.goal_id}`)
  await expect(page.getByLabel('截止时间（UTC，可选）')).toHaveValue('2027-01-01T10:00')
  await page.getByLabel('学习目标', { exact: true }).fill('本标签页尚未保存的修订')
  await api(
    page,
    'PUT',
    `/v1/learning/goals/${goal.goal_id}`,
    {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'goal',
      aggregate_id: goal.goal_id,
      expected_version: goal.revision,
      previous_revision_id: goal.goal_revision_id,
      text: '其他客户端已更新',
      source: 'browser-test',
    },
    space.id,
  )
  await page.getByRole('button', { name: '保存修改', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('内容版本已变化')
  await expect(page.getByLabel('学习目标', { exact: true })).toHaveValue('本标签页尚未保存的修订')
  await page.getByRole('button', { name: '读取最新版本并保留输入' }).click()
  await page.getByRole('button', { name: '保存修改', exact: true }).click()
  await expect(page.getByText('目标已保存，尚未启动教学。')).toBeVisible()
  const before = JSON.parse(await readFile('/tmp/edu-web-test-server-32929.json', 'utf8'))
  process.kill(before.supervisor, 'SIGUSR2')
  await expect
    .poll(
      async () => JSON.parse(await readFile('/tmp/edu-web-test-server-32929.json', 'utf8')).child,
      { timeout: 30_000 },
    )
    .not.toBe(before.child)
  await expect
    .poll(
      async () => {
        try {
          return (await page.request.get('/livez')).status()
        } catch {
          return 0
        }
      },
      { timeout: 30_000 },
    )
    .toBe(200)
  await page.reload()
  await expect(page.getByLabel('学习目标', { exact: true })).toHaveValue('本标签页尚未保存的修订')
  await page.getByRole('button', { name: '退出', exact: true }).click()
  await expect(page.getByRole('button', { name: '取消', exact: true })).toBeFocused()
  await page.getByRole('button', { name: '确认退出', exact: true }).click()
  await expect(page.getByLabel('配对码', { exact: true })).toBeVisible()
  expect((await page.request.get('/v1/web/session')).status()).toBe(401)
})
