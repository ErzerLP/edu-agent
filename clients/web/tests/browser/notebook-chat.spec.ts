import { test, expect } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import {
  configureNotebookModel,
  createSpace,
  openNavigation,
  pairNotebook,
  seedSources,
  webAPI,
} from './notebook-fixture'
import { captureNotebook } from './notebook-visual'

test('来源行打开原文，菜单预算仍用于对话提交', async ({ page }) => {
  page.setDefaultTimeout(10000)
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await createSpace(page, '来源与菜单展示验收')
  await webAPI(page, 'PUT', `/v1/learning-spaces/${space.id}`, {
    operation_id: randomUUID(),
    expected_version: space.version,
    name: space.name,
    description: '',
    status: 'active',
    direction: '理解偶数',
  })
  await seedSources(
    page,
    space.id,
    String.raw`公式资料 \(2k\) $ x^2 $ \[2+2=4\] $$\sum_{i=1}^2 i=3$$`,
  )
  await page.goto(`/app/spaces/${space.id}`)
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await sources.getByRole('button', { name: '打开来源：even.md', exact: true }).click()
  const reader = sources.getByRole('region', { name: '来源查看器', exact: true })
  await reader.getByText(/原文片段 1/).click()
  await expect(reader).toContainText('偶数可以被 2 整除')
  await reader.getByRole('button', { name: '关闭来源', exact: true }).click()
  await expect(
    sources.getByRole('button', { name: '打开来源：even.md', exact: true }),
  ).toBeFocused()
  await sources.getByRole('checkbox', { name: '选择全部来源' }).uncheck()
  await sources.getByRole('checkbox', { name: /^选择来源：even.md/ }).check()
  await openNavigation(page)
  const menu = page.getByRole('dialog', { name: '侧边菜单', exact: true })
  await menu.getByText('对话设置', { exact: true }).click()
  await menu.getByLabel('请求次数上限', { exact: true }).fill('1')
  await menu.getByLabel('Token 预留上限', { exact: true }).fill('10000')
  await page.keyboard.press('Escape')
  const submitted = page.waitForRequest(
    (r) => r.method() === 'POST' && /\/conversations\/[^/]+\/turns$/.test(r.url()),
  )
  await page.getByLabel('向导师提问').fill('解释偶数')
  await page.getByRole('button', { name: '发送提问' }).click()
  expect((await submitted).postDataJSON()).toMatchObject({ request_budget: 1, token_budget: 10000 })
  await expect(page.getByRole('button', { name: '核验引用 1' })).toBeVisible()
  const answer = page.locator('.chat-answer')
  await expect(answer.locator('math')).toHaveCount(4)
  await expect(answer.locator('math[display="block"]')).toHaveCount(2)
  await answer.getByRole('button', { name: '核验引用 1' }).click()
  await expect(reader).toContainText('偶数可以被 2 整除')
})

test('C2 方向、空对话、引用、实时改选、方向修订、保存重开与 Studio 笔记', async ({ page }) => {
  test.setTimeout(120_000)
  page.setDefaultTimeout(15000)
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  await captureNotebook(page, 'home')
  await page.getByRole('button', { name: '新建学习区', exact: true }).click()
  const create = page.getByRole('dialog', { name: '新建学习区' })
  const name = `对话学习 ${randomUUID().slice(0, 8)}`
  await create.getByLabel('学习区名称', { exact: true }).fill(name)
  await create.getByLabel('学习方向', { exact: true }).fill('理解偶数与整除')
  await create.getByRole('button', { name: '创建学习区', exact: true }).click()
  await expect(page.getByRole('heading', { name, exact: true, level: 1 })).toBeVisible()
  const spaceId = new URL(page.url()).pathname.split('/')[3]
  await seedSources(page, spaceId)
  await page.reload()
  const chat = page.getByRole('region', { name: '对话', exact: true })
  await expect(chat.getByText('学习区概要', { exact: true })).toBeVisible()
  await expect(chat.locator('.chat-direction')).toContainText('理解偶数与整除')
  await expect(chat.getByRole('button', { name: '概要引用 1' })).toBeVisible()
  await expect(page.getByLabel('今天想学会什么？', { exact: true })).toHaveCount(0)
  await expect(page.getByText(/课堂.*冻结资料|未选来源，不引用资料/)).toHaveCount(0)
  await captureNotebook(page, 'empty', '对话')
  await chat.getByLabel('向导师提问').fill('请解释偶数的含义')
  await chat.getByRole('button', { name: '发送提问' }).click()
  await expect(chat.getByRole('button', { name: '核验引用 1' })).toBeVisible()
  await expect(page).toHaveURL(/conversation=/)
  const conversationId = new URL(page.url()).searchParams.get('conversation')!
  await captureNotebook(page, 'cited', '对话')
  await chat.getByRole('button', { name: '核验引用 1' }).click()
  await expect(
    page.getByRole('region', { name: '来源', exact: true }).locator('mark'),
  ).toBeVisible()
  await captureNotebook(page, 'sources', '来源')
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await sources.getByRole('checkbox', { name: '选择全部来源' }).uncheck()
  await sources.getByRole('checkbox', { name: /^选择来源：examples.md/ }).check()
  await chat.getByLabel('向导师提问').fill('根据现在的资料继续讲解')
  await chat.getByRole('button', { name: '发送提问' }).click()
  await expect(chat.getByRole('button', { name: '保存为笔记' })).toHaveCount(2)
  const turns = await webAPI(
    page,
    'GET',
    `/v1/learning/conversations/${conversationId}`,
    undefined,
    spaceId,
  )
  expect(turns.items[1].citations.map((c: { source: { path: string } }) => c.source.path)).toEqual([
    'examples.md',
  ])
  await page.getByRole('button', { name: '学习区设置', exact: true }).click()
  const settings = page.getByRole('dialog', { name: '学习区设置', exact: true })
  await settings.getByLabel('学习方向', { exact: true }).fill('从例子理解整除关系')
  await settings.getByRole('button', { name: '保存设置', exact: true }).click()
  await expect(settings).not.toBeVisible()
  await chat.getByLabel('向导师提问').fill('按新的方向讲解')
  await chat.getByRole('button', { name: '发送提问' }).click()
  await expect(chat.getByRole('button', { name: '保存为笔记' })).toHaveCount(3)
  const goals = (await webAPI(page, 'GET', '/v1/learning/goals', undefined, spaceId)).items
  expect(goals).toHaveLength(1)
  expect(goals[0]).toMatchObject({
    source: 'learning_direction',
    revision: 2,
    text: '从例子理解整除关系',
  })
  const identity = await (await page.request.get('/v1/web/session')).json()
  const manual = await page.request.put(`/v1/learning/goals/${goals[0].goal_id}`, {
    headers: {
      Origin: 'http://127.0.0.1:32929',
      'X-CSRF-Token': identity.csrf_token,
      'X-Learning-Space-ID': spaceId,
    },
    data: {
      operation_id: randomUUID(),
      payload_schema_version: 1,
      aggregate_type: 'goal',
      aggregate_id: goals[0].goal_id,
      expected_version: 2,
      previous_revision_id: goals[0].goal_revision_id,
      text: '不能绕过方向修订',
      source: 'user',
      details: goals[0].management.details,
    },
  })
  expect(manual.status()).toBe(409)
  await page.reload()
  await expect(chat.getByRole('button', { name: '保存为笔记' })).toHaveCount(3)
  await chat.getByRole('button', { name: '保存为笔记' }).last().click()
  await expect(page.getByRole('region', { name: 'Studio', exact: true })).toContainText(
    '对话笔记：按新的方向讲解',
  )
  await captureNotebook(page, 'studio', 'Studio')
  await chat.getByRole('button', { name: '新建对话' }).click()
  await expect(chat.getByText('学习区概要', { exact: true })).toBeVisible()
  await chat.getByRole('button', { name: '继续上次', exact: true }).click()
  await expect(chat.getByRole('button', { name: '保存为笔记' })).toHaveCount(3)
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(page.getByRole('tab', { name: '对话', exact: true })).toHaveAttribute(
    'aria-selected',
    'true',
  )
  await chat.getByLabel('向导师提问').fill('手机未发送草稿')
  await chat.getByRole('button', { name: '核验引用 1' }).last().click()
  await expect(page.getByRole('tab', { name: '来源', exact: true })).toHaveAttribute(
    'aria-selected',
    'true',
  )
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  await expect(chat.getByLabel('向导师提问')).toHaveValue('手机未发送草稿')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.route('**/v1/web/session', async (route) => {
    const response = await route.fetch()
    const body = await response.json()
    body.device.scopes = body.device.scopes.filter((scope: string) => scope !== 'knowledge:read')
    await route.fulfill({ response, json: body })
  })
  await page.evaluate(() => window.dispatchEvent(new Event('focus')))
  await expect(chat.locator('.chat-answer')).toHaveCount(0)
  await expect(chat.getByLabel('向导师提问')).toHaveValue('')
})

test('旧学习区方向只由区名预填，经确认后才能发送', async ({ page }) => {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '旧区名',
    description: 'PRIVATE_DESCRIPTION',
    status: 'active',
  })
  await page.goto(`/app/spaces/${space.id}`)
  await page.getByRole('button', { name: '确认学习方向', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '确认学习方向' })
  await expect(dialog.getByLabel('学习方向', { exact: true })).toHaveValue('旧区名')
  await dialog.getByRole('button', { name: '保存设置' }).click()
  await expect(dialog).not.toBeVisible()
  await expect(page.getByRole('region', { name: '对话', exact: true })).not.toContainText(
    'PRIVATE_DESCRIPTION',
  )
  await page.getByLabel('向导师提问').fill('你好')
  await expect(page.getByRole('button', { name: '发送提问' })).toBeEnabled()
})

test('新建学习区立即添加来源只打开一次', async ({ page }) => {
  await pairNotebook(page, 'learner')
  await page.getByRole('button', { name: '新建学习区', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '新建学习区', exact: true })
  await dialog.getByLabel('学习区名称', { exact: true }).fill('立即添加资料')
  await dialog.getByLabel('学习方向', { exact: true }).fill('宽泛地了解代数')
  await dialog.getByLabel('现在添加来源').check()
  await dialog.getByRole('button', { name: '创建学习区', exact: true }).click()
  const source = page.getByRole('dialog', { name: '添加来源', exact: true })
  await expect(source).toBeVisible()
  await source.getByRole('button', { name: '关闭添加来源', exact: true }).click()
  await expect(source).not.toBeVisible()
  await expect(page).not.toHaveURL(/add_sources=/)
  await page.reload()
  await expect(
    page.getByRole('heading', { name: '立即添加资料', level: 1, exact: true }),
  ).toBeVisible()
  await expect(source).not.toBeVisible()
})

test('归档学习区不接受会被忽略的方向编辑，恢复后可修改方向', async ({ page }) => {
  page.setDefaultTimeout(10000)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: '归档方向保护',
    description: '',
    status: 'active',
    direction: '原学习方向',
  })
  await webAPI(page, 'PUT', `/v1/learning-spaces/${space.id}`, {
    operation_id: randomUUID(),
    expected_version: space.version,
    name: space.name,
    description: '',
    status: 'archived',
  })
  await page.goto(`/app/spaces/${space.id}`)
  await page.getByRole('button', { name: '学习区设置', exact: true }).click()
  const settings = page.getByRole('dialog', { name: '学习区设置', exact: true })
  await expect(settings.getByLabel('学习方向', { exact: true })).toHaveValue('原学习方向')
  await expect(settings.getByLabel('学习方向', { exact: true })).toBeDisabled()
  await settings.getByRole('button', { name: '恢复学习区', exact: true }).click()
  await page.getByRole('button', { name: '确认恢复学习区', exact: true }).click()
  await expect(settings).not.toBeVisible()
  await expect(page.getByText('已归档 · 只读', { exact: true })).not.toBeVisible()
  await page.getByRole('button', { name: '学习区设置', exact: true }).click()
  await expect(settings.getByLabel('学习方向', { exact: true })).toBeEnabled()
  await settings.getByLabel('学习方向', { exact: true }).fill('恢复后的学习方向')
  await settings.getByRole('button', { name: '保存设置', exact: true }).click()
  await expect(settings).not.toBeVisible()
  await page.reload()
  await expect(page.locator('.chat-direction')).toContainText('恢复后的学习方向')
})
