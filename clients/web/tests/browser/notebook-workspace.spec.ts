import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import {
  createSpace,
  defaultSpace,
  openNavigation,
  pairNotebook,
  seedSources,
  webAPI,
} from './notebook-fixture'

test('配对、新建学习区、卡片进入、建立并恢复真实课堂、正式答案与反馈', async ({
  page,
  context,
}) => {
  await pairNotebook(page)
  const name = `偶数笔记本 ${randomUUID().slice(0, 8)}`
  await page.getByText('新建学习区', { exact: true }).click()
  await page.getByLabel('学习区名称', { exact: true }).fill(name)
  await page.getByRole('button', { name: '创建学习区', exact: true }).click()
  await expect(page.getByRole('heading', { name, exact: true, level: 1 })).toBeVisible()
  const spaceId = new URL(page.url()).pathname.split('/')[3]
  const sources = await seedSources(page, spaceId)

  await page.getByRole('link', { name: '全部学习区', exact: true }).click()
  await page.getByLabel('搜索学习区', { exact: true }).fill(name)
  await page.getByRole('link', { name, exact: true }).click()
  await expect(page.getByRole('region', { name: '来源', exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: '课堂', exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Studio', exact: true })).toBeVisible()
  await page.getByText('偶数资料', { exact: true }).click()
  await expect(page.getByRole('region', { name: '来源', exact: true })).toContainText('even.md')
  await expect(
    page.getByRole('region', { name: '来源', exact: true }).getByRole('checkbox'),
  ).toHaveCount(0)
  await expect(
    page
      .getByRole('region', { name: 'Studio', exact: true })
      .getByText('尚未交付', { exact: true }),
  ).toHaveCount(4)

  const text = '理解偶数并使用原资料说明依据'
  await page.getByLabel('今天想学会什么？', { exact: true }).fill(text)
  await page.getByRole('button', { name: '仅保存目标', exact: true }).click()
  await expect(page.getByText('目标已保存，尚未启动教学。')).toBeVisible()
  const listed = await webAPI(page, 'GET', '/v1/learning/goals', undefined, spaceId)
  const goal = listed.items.find((item: { text: string }) => item.text === text)
  // 正式教学必须用已采用的来源范围；只在夹具准备阶段采用，不以左栏浏览替代审批。
  await webAPI(
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
      text: goal.text,
      source: 'Notebook 来源夹具',
      details: { ...goal.management.details, scope_snapshot_id: sources.scopeId },
    },
    spaceId,
  )
  await page.reload()
  await page.getByRole('button', { name: text, exact: true }).click()
  await page.getByRole('button', { name: '新建教学会话', exact: true }).click()
  await expect(page).toHaveURL(/\/learn\/[0-9a-f-]+$/)
  const classroomURL = page.url()
  const sessionId = new URL(classroomURL).pathname.split('/').at(-1)!
  await page.getByRole('button', { name: '开始学习', exact: true }).click()
  await page.getByRole('button', { name: '生成学习路线', exact: true }).click()
  await page.getByRole('button', { name: '生成下一活动', exact: true }).click()
  await page.getByRole('button', { name: '开始当前活动', exact: true }).click()
  const answer = page.getByRole('textbox', { name: '我的正式答案', exact: true })
  await expect(answer).toBeEnabled()
  await answer.fill('A')
  const second = await context.newPage()
  await second.goto(classroomURL)
  await expect(second.getByRole('textbox', { name: '我的正式答案', exact: true })).toHaveValue('')
  await second.getByRole('textbox', { name: '我的正式答案', exact: true }).fill('B')
  const other = await createSpace(page, 'Notebook 课堂草稿另一学习区')
  await openNavigation(page)
  await page.getByText('切换学习区', { exact: true }).click()
  await page
    .getByRole('dialog', { name: '侧边菜单' })
    .getByRole('link', { name: other.name, exact: true })
    .click()
  await openNavigation(page)
  await page.getByText('切换学习区', { exact: true }).click()
  await page
    .getByRole('dialog', { name: '侧边菜单' })
    .getByRole('link', { name, exact: true })
    .click()
  await page.getByRole('button', { name: text, exact: true }).click()
  await page.getByRole('region', { name: '教学会话', exact: true }).getByRole('link').click()
  await expect(answer).toHaveValue('A')
  await expect(second.getByRole('textbox', { name: '我的正式答案', exact: true })).toHaveValue('B')
  await second.close()
  await page.getByRole('button', { name: '提交正式答案', exact: true }).click()
  await page.getByRole('button', { name: '获取教学反馈', exact: true }).click()
  await expect(page.getByRole('region', { name: '教学反馈', exact: true })).toContainText('已接纳')
  const saved = await webAPI(page, 'GET', `/v1/tutoring/sessions/${sessionId}`, undefined, spaceId)
  expect(saved.work_item.attempt).toMatchObject({ answer: 'A', help: 'none' })
  expect(saved.work_item.assessment_decision.disposition).toBe('accepted')
  await page.reload()
  await expect(page.getByRole('region', { name: '教学反馈', exact: true })).toContainText('已接纳')
  await page.getByRole('link', { name: '全部学习区', exact: true }).click()
  await page.getByLabel('搜索学习区', { exact: true }).fill(name)
  await page.getByRole('link', { name, exact: true }).click()
  await page.getByRole('button', { name: text, exact: true }).click()
  await page.getByRole('region', { name: '教学会话', exact: true }).getByRole('link').click()
  await expect(page).toHaveURL(classroomURL)
  await expect(page.getByRole('region', { name: '教学反馈', exact: true })).toContainText('已接纳')
})

test('390 与 1280 宽度无整页溢出，底部标签切换保留目标草稿', async ({ page }, testInfo) => {
  await pairNotebook(page)
  await page.getByRole('link', { name: '默认学习区', exact: true }).click()
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 })
    const header = await page.getByRole('banner').boundingBox()
    const brand = await page.locator('.brand').boundingBox()
    expect(brand!.y + brand!.height, '品牌必须在顶栏内，不能压住学习区标题').toBeLessThanOrEqual(
      header!.y + header!.height,
    )
    await page.getByLabel('今天想学会什么？', { exact: true }).fill(`目标草稿 ${width}`)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: testInfo.outputPath(`notebook-${width}.png`), fullPage: true })
    if (width === 390) {
      const tabs = page.getByRole('tablist', { name: '学习区面板', exact: true })
      await tabs.getByRole('tab', { name: '来源', exact: true }).click()
      await expect(page.getByRole('region', { name: '课堂', exact: true })).not.toBeVisible()
      await tabs.getByRole('tab', { name: 'Studio', exact: true }).click()
      await page.reload()
      await expect(tabs.getByRole('tab', { name: 'Studio', exact: true })).toHaveAttribute(
        'aria-selected',
        'true',
      )
      await tabs.getByRole('tab', { name: '课堂', exact: true }).click()
      await page.getByLabel('今天想学会什么？', { exact: true }).fill('切标签保留草稿')
      await tabs.getByRole('tab', { name: '来源', exact: true }).click()
      await tabs.getByRole('tab', { name: '课堂', exact: true }).click()
      await expect(page.getByLabel('今天想学会什么？', { exact: true })).toHaveValue(
        '切标签保留草稿',
      )
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }
})

test('同标签页切区恢复草稿，双标签页互不覆盖，来源不跨区', async ({ page, context }) => {
  await pairNotebook(page)
  const a = await createSpace(page, 'Notebook 隔离甲')
  const b = await createSpace(page, 'Notebook 隔离乙')
  await seedSources(page, a.id, '只属于甲的资料')
  await page.goto(`/app/spaces/${a.id}`)
  const second = await context.newPage()
  await second.goto(`/app/spaces/${a.id}`)
  await page.getByLabel('今天想学会什么？', { exact: true }).fill('甲标签页草稿')
  await second.getByLabel('今天想学会什么？', { exact: true }).fill('乙标签页草稿')
  await openNavigation(page)
  await page.getByText('切换学习区', { exact: true }).click()
  await page
    .getByRole('dialog', { name: '侧边菜单' })
    .getByRole('link', { name: b.name, exact: true })
    .click()
  await page.getByLabel('今天想学会什么？', { exact: true }).fill('另一区的草稿')
  await expect(page.getByRole('region', { name: '来源', exact: true })).not.toContainText(
    '只属于甲的资料',
  )
  await openNavigation(page)
  await page.getByText('切换学习区', { exact: true }).click()
  await page
    .getByRole('dialog', { name: '侧边菜单' })
    .getByRole('link', { name: a.name, exact: true })
    .click()
  await expect(page.getByLabel('今天想学会什么？', { exact: true })).toHaveValue('甲标签页草稿')
  await expect(second.getByLabel('今天想学会什么？', { exact: true })).toHaveValue('乙标签页草稿')
  await second.close()
})

test('菜单完整可达、键盘关闭返回焦点、旧深链接与无效 query 不扩大范围', async ({ page }) => {
  await pairNotebook(page)
  await openNavigation(page)
  const drawer = page.getByRole('dialog', { name: '侧边菜单' })
  await drawer.getByRole('button', { name: '评估与证据', exact: true }).click()
  await expect(drawer.getByText('请选择学习区', { exact: true })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('button', { name: '打开侧边菜单' })).toBeFocused()
  await page.getByRole('link', { name: '默认学习区', exact: true }).click()
  await openNavigation(page)
  for (const name of [
    '全部学习区',
    '进度与复习',
    '评估与证据',
    '导师历史',
    '长期记忆',
    '任务中心',
    '知识与参考',
    'NoteSync',
    '离线学习',
    'Studio 内容库',
    '设置与能力',
    '数据与隐私',
    '设备管理',
    '管理学习区与目标',
  ]) {
    await expect(drawer.getByRole('link', { name, exact: true })).toBeVisible()
  }
  await drawer.getByRole('link', { name: '管理学习区与目标', exact: true }).click()
  await expect(page.getByText('编辑学习区', { exact: true })).toBeVisible()
  await page.reload()
  await expect(page.getByText('编辑学习区', { exact: true })).toBeVisible()
  await page.goto(`/app/spaces/${defaultSpace}?goal=invalid&panel=unknown`)
  await expect(page.getByRole('heading', { name: '学习区地址无效', exact: true })).toBeVisible()
  await expect(page.getByLabel('今天想学会什么？', { exact: true })).toHaveCount(0)
})

test('来源按集合懒加载，同名文档保持归属，读取权限失效立即隐藏目录', async ({ page }) => {
  await pairNotebook(page)
  const space = await createSpace(page, 'Notebook 来源目录')
  await seedSources(page, space.id, '目录甲')
  await seedSources(page, space.id, '目录乙')
  const trees: string[] = [],
    exports: string[] = []
  page.on('request', (request) => {
    if (/\/tree$/.test(request.url())) trees.push(request.url())
    if (/\/export$/.test(request.url())) exports.push(request.url())
  })
  await page.goto(`/app/spaces/${space.id}`)
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources.getByText('目录甲', { exact: true })).toBeVisible()
  expect(trees).toEqual([])
  await sources.getByText('目录甲', { exact: true }).click()
  await expect(sources.getByText('even.md', { exact: true })).toHaveCount(1)
  await sources.getByText('目录乙', { exact: true }).click()
  await expect(sources.getByText('even.md', { exact: true })).toHaveCount(2)
  expect(new Set(trees).size).toBe(2)
  expect(exports).toEqual([])
  await page.route('**/v1/web/session', async (route) => {
    const response = await route.fetch()
    const body = await response.json()
    body.device.scopes = body.device.scopes.filter((scope: string) => scope !== 'knowledge:read')
    await route.fulfill({ response, json: body })
  })
  await page.evaluate(() => window.dispatchEvent(new Event('focus')))
  await expect(sources).toContainText('当前配对没有来源读取权限')
  await expect(sources.getByText('目录甲', { exact: true })).toHaveCount(0)
  await expect(sources.getByText('even.md', { exact: true })).toHaveCount(0)
})

test('归档学习区禁止新建目标与课堂，恢复后目标区可继续写入', async ({ page }) => {
  await pairNotebook(page)
  const space = await createSpace(page, 'Notebook 归档验收')
  await webAPI(page, 'PUT', `/v1/learning-spaces/${space.id}`, {
    operation_id: randomUUID(),
    expected_version: space.version,
    name: space.name,
    description: '',
    status: 'archived',
  })
  await page.goto(`/app/spaces/${space.id}`)
  await expect(page.getByRole('region', { name: '课堂', exact: true })).toContainText(
    '仅可查看历史',
  )
  await expect(page.getByRole('button', { name: '仅保存目标', exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '新建教学会话', exact: true })).toHaveCount(0)
  await page.getByRole('link', { name: '管理学习区与目标', exact: true }).click()
  await page.getByText('编辑学习区', { exact: true }).click()
  await page.getByRole('button', { name: '恢复学习区', exact: true }).click()
  await page.getByRole('button', { name: '确认恢复学习区', exact: true }).click()
  await page.getByRole('link', { name: '← 进入课堂工作区', exact: true }).click()
  await expect(page.getByLabel('今天想学会什么？', { exact: true })).toBeEnabled()
})

test('切区后来源目录的迟到响应不进入新区，也不覆盖新区目标草稿', async ({ page }) => {
  await pairNotebook(page)
  const a = await createSpace(page, 'Notebook 迟到甲')
  const b = await createSpace(page, 'Notebook 迟到乙')
  await seedSources(page, a.id, '迟到甲来源')
  await seedSources(page, b.id, '迟到乙来源')
  let release!: () => void
  let started!: () => void
  let delivered!: () => void
  const gate = new Promise<void>(resolve => { release = resolve })
  const seen = new Promise<void>(resolve => { started = resolve })
  const finished = new Promise<void>(resolve => { delivered = resolve })
  await page.route('**/v1/knowledge/collections*', async route => {
    if (route.request().headers()['x-learning-space-id'] !== a.id) {
      await route.continue()
      return
    }
    const response = await route.fetch()
    expect(response.ok()).toBe(true)
    started()
    await gate
    await route.fulfill({ response })
    delivered()
  })
  await page.goto(`/app/spaces/${a.id}`)
  await seen
  await openNavigation(page)
  await page.getByText('切换学习区', { exact: true }).click()
  await page.getByRole('dialog', { name: '侧边菜单' }).getByRole('link', { name: b.name, exact: true }).click()
  const draft = page.getByLabel('今天想学会什么？', { exact: true })
  await draft.fill('新区正在写的目标')
  release()
  await finished
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources).toContainText('迟到乙来源')
  await expect(sources).not.toContainText('迟到甲来源')
  await expect(draft).toHaveValue('新区正在写的目标')
})
