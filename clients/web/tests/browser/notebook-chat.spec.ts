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
import AxeBuilder from '@axe-core/playwright'

test('切换对话菜单支持键盘选择与 Esc 返回焦点', async ({ page }) => {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await createSpace(page, '对话菜单键盘验收')
  await webAPI(page, 'POST', '/v1/learning/conversations', {
    id: randomUUID(), mode: 'study', saved: true, title: '较早的对话',
  }, space.id)
  const latest = await webAPI(page, 'POST', '/v1/learning/conversations', {
    id: randomUUID(), mode: 'study', saved: true, title: '最近的对话',
  }, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  const trigger = page.getByRole('button', { name: '切换对话', exact: true })
  await trigger.focus()
  await page.keyboard.press('ArrowDown')
  const list = page.getByRole('listbox', { name: '切换对话', exact: true })
  await expect(trigger).toHaveAttribute('aria-expanded', 'true')
  await expect(list.getByRole('option', { name: '新对话', exact: true })).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(list.getByRole('option', { name: '最近的对话', exact: true })).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(list.getByRole('option', { name: '较早的对话', exact: true })).toBeFocused()
  await page.keyboard.press('ArrowUp')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(new RegExp(`conversation=${latest.id}`))
  await expect(list).toHaveCount(0)
  await trigger.focus()
  await page.keyboard.press('Enter')
  await expect(list.getByRole('option', { name: '最近的对话', exact: true })).toHaveAttribute('aria-selected', 'true')
  await page.keyboard.press('Escape')
  await expect(list).toHaveCount(0)
  await expect(trigger).toHaveAttribute('aria-expanded', 'false')
  await expect(trigger).toBeFocused()
})

test('UX 正常提交无核对提示，思考与发送停止切换，失败保留原请求可重试', async ({ page }) => {
  test.setTimeout(120000)
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(), expected_version: 0, name: '对话体验验收', description: '', status: 'active', direction: '理解偶数',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  const input = page.getByLabel('向导师提问', { exact: true })
  const send = page.getByRole('button', { name: '发送提问', exact: true })
  await expect(send).toBeDisabled()
  const initialHeight = (await input.boundingBox())!.height
  await input.fill('多行问题\n'.repeat(30))
  expect((await input.boundingBox())!.height).toBeGreaterThan(initialHeight)
  expect((await input.boundingBox())!.height).toBeLessThanOrEqual(144)
  expect(await input.evaluate(el => el.scrollHeight > el.clientHeight)).toBe(true)
  expect(await input.evaluate(el => getComputedStyle(el).borderTopWidth)).toBe('0px')
  await input.fill('UX 慢速思考')
  await expect(send).toBeEnabled()
  await captureNotebook(page, 'input-idle', '对话')
  const submitted: Record<string, unknown>[] = []
  let release!: () => void
  const gate = new Promise<void>(resolve => { release = resolve })
  await page.route('**/v1/learning/conversations/*/turns', async route => {
    submitted.push(route.request().postDataJSON())
    const response = await route.fetch()
    await gate
    await route.fulfill({ response })
  })
  await input.dispatchEvent('keydown', { key: 'Enter', code: 'Enter', isComposing: true })
  expect(submitted).toHaveLength(0)
  await input.press('Shift+Enter')
  await expect(input).toHaveValue('UX 慢速思考\n')
  await input.press('Enter')
  await expect.poll(() => submitted.length).toBe(1)
  await expect(page.locator('.chat-thinking')).toBeVisible()
  await expect(page.locator('.submission-notice')).toHaveCount(0)
  release()
  const stop = page.getByRole('button', { name: '停止生成', exact: true })
  await expect(stop).toBeEnabled()
  await expect(page.locator('.chat-input button')).toHaveCount(1)
  await expect(page.locator('.chat-panel > [aria-live="polite"]')).toHaveText('学习导师正在思考')
  await captureNotebook(page, 'thinking-stop', '对话')
  await page.emulateMedia({ reducedMotion: 'reduce' })
  expect(await page.locator('.chat-thinking-dots i').first().evaluate(el => getComputedStyle(el).animationName)).toBe('none')
  const thinkingAudit = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa']).analyze()
  expect(thinkingAudit.violations.map(v => v.id)).toEqual([])
  let releaseStop!: () => void
  const stopGate = new Promise<void>(resolve => { releaseStop = resolve })
  const stopResponses: number[] = []
  await page.route('**/v1/learning/runs/*/commands', async route => {
    expect(route.request().postDataJSON().kind).toBe('stop')
    const response = await route.fetch()
    stopResponses.push(response.status())
    await stopGate
    await route.fulfill({ response })
  })
  await stop.click()
  await expect(stop).toBeDisabled()
  await expect(stop).toHaveAttribute('aria-busy', 'true')
  releaseStop()
  await expect(send).toBeVisible()
  await expect(page.locator('.chat-thinking')).toHaveCount(0)
  await expect(page.locator('.chat-ai').getByText('已停止', { exact: true })).toBeVisible()
  await expect(page.locator('.chat-panel')).not.toContainText('结果未知')
  expect(stopResponses).toEqual([202])
  await page.screenshot({ path: test.info().outputPath('chat-stopped.png'), fullPage: true })
  await page.reload()
  await expect(page.locator('.chat-ai').getByText('已停止', { exact: true })).toBeVisible()
  await expect(page.locator('.chat-panel')).not.toContainText('结果未知')
  // Exercise a saved partial answer at the history boundary as well. The
  // deterministic study model normally publishes only complete structured text.
  const historyPath = /\/v1\/learning\/conversations\/[0-9a-f-]+(?:\?.*)?$/
  await page.route(historyPath, async route => {
    const response = await route.fetch()
    const history = await response.json()
    for (const turn of history.items) {
      if (turn.status === 'cancelled') turn.output = '停止前已生成的部分讲解。'
    }
    await route.fulfill({ response, json: history })
  })
  await page.reload()
  await expect(page.locator('.chat-answer')).toContainText('停止前已生成的部分讲解。')
  await expect(page.locator('.chat-ai').getByText('已停止生成', { exact: true })).toBeVisible()
  await expect(page.locator('.chat-panel')).not.toContainText('结果未知')
  await page.unroute(historyPath)
  await page.unroute('**/v1/learning/runs/*/commands')
  await page.unroute('**/v1/learning/conversations/*/turns')

  const attempts: Record<string, unknown>[] = []
  await page.route('**/v1/learning/conversations/*/turns', async route => {
    attempts.push(route.request().postDataJSON())
    if (attempts.length === 1) {
      const response = await route.fetch()
      expect(response.ok()).toBe(true)
      return route.abort('failed')
    }
    await route.continue()
  })
  await input.fill('用例子解释偶数')
  await send.click()
  const notice = page.locator('.submission-notice').filter({ hasText: '提交结果待核对' })
  await expect(notice).toContainText('提交结果待核对')
  await expect(notice).toHaveAttribute('role', 'status')
  await captureNotebook(page, 'submission-unknown', '对话')
  const failureAudit = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa']).analyze()
  expect(failureAudit.violations.map(v => v.id)).toEqual([])
  await notice.getByRole('button', { name: '重试', exact: true }).click()
  await expect(page.locator('.chat-answer')).toBeVisible()
  await expect.poll(() => attempts.length).toBe(2)
  await expect(input).toHaveValue('')
  await expect(notice).toHaveCount(0)
  expect(attempts[1]).toEqual(attempts[0])
  await expect(page.locator('.chat-panel > [aria-live="polite"]')).toHaveText('学习导师第 2 轮回答已完成')
  await expect(send).toBeDisabled()
  await page.unroute('**/v1/learning/conversations/*/turns')
  let unavailable = true
  const serverAttempts: Record<string, unknown>[] = []
  await page.route('**/v1/learning/conversations/*/turns', async route => {
    serverAttempts.push(route.request().postDataJSON())
    if (unavailable) return route.fulfill({ status: 503, json: { code: 'unavailable' } })
    await route.continue()
  })
  await input.fill('服务器恢复后继续')
  await send.click()
  await expect(notice).toContainText('提交结果待核对')
  let releaseCheck!: () => void
  const checkGate = new Promise<void>(resolve => { releaseCheck = resolve })
  await page.route('**/v1/learning/operations/*', async route => {
    await checkGate
    await route.continue()
  })
  await notice.getByRole('button', { name: '核对', exact: true }).click()
  await expect(notice).toHaveCount(0)
  // Checking a lost receipt is not a new model request.
  await expect(page.locator('.chat-thinking')).toHaveCount(0)
  releaseCheck()
  await expect(notice).toContainText('提交结果待核对')
  await page.unroute('**/v1/learning/operations/*')
  const conversationId = new URL(page.url()).searchParams.get('conversation')!
  const savedConversation = await webAPI(page, 'GET', `/v1/learning/conversations/${conversationId}`, undefined, space.id)
  await page.getByRole('button', { name: '新建对话', exact: true }).click()
  await page.getByRole('button', { name: '切换对话', exact: true }).click()
  await page.getByRole('option', { name: savedConversation.conversation.title, exact: true }).click()
  // Restoring a draft checks its operation first. A 404 leaves the original
  // request available for an explicit retry, without automatically resending.
  await expect(notice).toContainText('提交结果待核对')
  expect(serverAttempts).toHaveLength(1)
  unavailable = false
  await notice.getByRole('button', { name: '重试', exact: true }).click()
  await expect.poll(() => serverAttempts.length).toBe(2)
  await expect(input).toHaveValue('')
  await expect(notice).toHaveCount(0)
  expect(serverAttempts[1]).toEqual(serverAttempts[0])
  // Distinct completed turns still announce completion when the model answers
  // too quickly for a queued/running poll to be observed.
  await expect(page.locator('.chat-panel > [aria-live="polite"]')).toHaveText('学习导师第 3 轮回答已完成')
})

test('GFM 回答表格在桌面与窄屏可读，空态概要去除 Markdown 标记', async ({ page }) => {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(), expected_version: 0, name: 'GFM 表格与概要',
    description: '', status: 'active', direction: '理解偶数与整除',
  })
  await seedSources(page, space.id, '偶数对照资料', [
    '# 偶数对照', '',
    '| 概念 | 定义 | 例子 | 依据 | 复习状态 |',
    '| :--- | --- | ---: | --- | --- |',
    '| 偶数 | $n=2k$ | 2、4、6 | 原资料 [1] | ~~待确认~~ 已核验 |', '',
    '- [x] 已阅读', '- [ ] 待复习', '',
    'https://example.org/even', '',
    '```text', '$x$ [1]', '```', '',
    '<img src=x onerror=alert(1)>',
  ].join('\n'))
  await page.goto(`/app/spaces/${space.id}`)
  const overview = page.locator('.chat-source-summary')
  await expect(overview).toContainText('偶数对照')
  await expect(overview).toContainText('已核验')
  await expect(overview).toContainText('$x$ [1]')
  await expect(overview).not.toContainText('```')
  await expect(overview).not.toContainText('~~')
  await expect(overview).not.toContainText('|')
  await expect(overview.locator('table, pre, code, input')).toHaveCount(0)
  await captureNotebook(page, 'gfm-overview', '对话')
  // The deterministic model echoes the first selected source; do not depend on UUID ordering.
  await page.getByRole('checkbox', { name: '选择全部来源', exact: true }).uncheck()
  await page.getByRole('checkbox', { name: /^选择来源：even.md/ }).check()
  await page.getByLabel('向导师提问', { exact: true }).fill('解释这张偶数对照表')
  await page.getByRole('button', { name: '发送提问', exact: true }).click()
  const answer = page.locator('.chat-answer')
  const table = answer.getByRole('table')
  await expect(table.getByRole('columnheader')).toHaveText(['概念', '定义', '例子', '依据', '复习状态'])
  await expect(table.getByRole('cell', { name: '2、4、6', exact: true })).toBeVisible()
  await expect(table.locator('math')).toHaveCount(1)
  await expect(answer.locator('del')).toHaveText('待确认')
  await expect(answer.locator('input[type="checkbox"]')).toHaveCount(2)
  for (const task of await answer.locator('input[type="checkbox"]').all()) await expect(task).toBeDisabled()
  await expect(answer.locator('pre code')).toHaveText('$x$ [1]\n')
  await expect(answer.locator('img, script')).toHaveCount(0)
  await expect(answer.getByRole('link', { name: /https:\/\/example.org\/even/ })).toHaveAttribute('rel', 'noopener noreferrer')
  await captureNotebook(page, 'gfm-table', '对话')
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 844 })
    if (width === 390) await page.getByRole('tab', { name: '对话', exact: true }).click()
    await expect(table).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    expect(await page.locator('.chat-stream').evaluate(element => element.scrollWidth <= element.clientWidth + 1)).toBe(true)
    if (width === 390) {
      const region = answer.getByRole('region', { name: '表格，可横向滚动', exact: true })
      expect(await region.evaluate(element => element.scrollWidth > element.clientWidth)).toBe(true)
      await region.focus()
      await page.keyboard.press('ArrowRight')
      await expect.poll(() => region.evaluate(element => element.scrollLeft)).toBeGreaterThan(0)
    }
  }
  await table.getByRole('button', { name: '核验引用 1', exact: true }).click()
  await expect(page.getByRole('region', { name: '来源查看器', exact: true })).toContainText('偶数')
  await page.reload()
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  await expect(answer.getByRole('table')).toBeVisible()
})

test('学习区回答渲染 Markdown、公式与引用，代码和 HTML 保持安全', async ({ page }) => {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await createSpace(page, 'Markdown 渲染验收')
  await webAPI(page, 'PUT', `/v1/learning-spaces/${space.id}`, {
    operation_id: randomUUID(),
    expected_version: space.version,
    name: space.name,
    description: '',
    status: 'active',
    direction: '理解公式与资料',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  await page.getByLabel('向导师提问').fill('Markdown 渲染验收')
  await page.getByRole('button', { name: '发送提问' }).click()
  const answer = page.locator('.chat-answer')
  await expect(answer.getByRole('heading', { name: 'Markdown 学习回答', level: 1 })).toBeVisible()
  await expect(answer.locator('strong')).toHaveText('重点')
  await expect(answer.locator('em')).toHaveText('解释')
  await expect(answer.locator('ul > li')).toHaveCount(2)
  await expect(answer.locator('ol > li')).toHaveCount(1)
  await expect(answer.locator('blockquote')).toContainText('资料需要核验')
  await expect(answer.locator('math')).toHaveCount(2)
  await expect(answer.locator('math[display="block"]')).toHaveCount(1)
  await expect(answer.locator('pre code')).toHaveText('const x = "$x$ [1]"\n')
  await expect(answer.locator('img, script')).toHaveCount(0)
  await expect(answer.getByRole('link', { name: /资料链接/ })).toHaveAttribute(
    'rel',
    'noopener noreferrer',
  )
  await captureNotebook(page, 'markdown', '对话')
  await answer.getByRole('button', { name: '核验引用 1' }).click()
  await expect(page.getByRole('region', { name: '来源查看器', exact: true })).toContainText('偶数')
  await page.reload()
  await expect(answer.getByRole('heading', { name: 'Markdown 学习回答', level: 1 })).toBeVisible()
})

test('C5a 网页概要显示标题与正文摘要，截断查看器给出短提示', async ({ page }, testInfo) => {
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(), expected_version: 0, name: '网页概要验收', description: '', status: 'active', direction: '理解偶数',
  })
  await seedSources(page, space.id)
  const metadata = { kind: 'url', title: '偶数的定义', locator: 'https://example.org/even', final_url: 'https://example.org/even', parser: 'safe-main-text-v2', coverage: 'partial_text_limit', fingerprint: 'INTERNAL_FINGERPRINT', status: 'partial' }
  // Only the read DTO presentation is controlled here; identities and scope are
  // from real imported sources. Actual fetch/receipt authorization stays in notebook-sources.
  await page.route('**/v1/knowledge/sources?*', async route => {
    const response = await route.fetch(), body = await response.json()
    for (const item of body.items) if (item.path === 'even.md') {
      item.path = 'web-INTERNAL_ID.md'
      item.metadata = metadata
    }
    await route.fulfill({ response, json: body })
  })
  await page.route('**/v1/knowledge/sources/*/documents/*', async route => {
    const response = await route.fetch(), body = await response.json()
    if (body.path === 'even.md') {
      body.path = 'web-INTERNAL_ID.md'
      body.metadata = metadata
      body.fragments[0].text = `    ${JSON.stringify(metadata)}\n\n    \`\`\`text\n    ${body.fragments[0].text}\n    \`\`\``
    }
    await route.fulfill({ response, json: body })
  })
  await page.goto(`/app/spaces/${space.id}`)
  const overview = page.locator('.chat-source-summary')
  await expect(overview).toContainText('偶数的定义')
  await expect(overview).toContainText('偶数可以被 2 整除')
  for (const internal of ['INTERNAL_ID', 'INTERNAL_FINGERPRINT', 'even.md', '"parser"']) await expect(overview).not.toContainText(internal)
  await expect(overview).not.toContainText('```')
  await page.screenshot({ path: testInfo.outputPath('web-overview.png') })
  await page.getByRole('button', { name: '打开来源：偶数的定义', exact: true }).click()
  const viewer = page.getByRole('region', { name: '来源查看器', exact: true })
  await expect(viewer.getByText('已保存 · 网址 · 部分解析：正文已截断（16 KB 上限）', { exact: true })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('web-partial-viewer.png') })
})

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
  await expect(chat.locator('.chat-panel > [aria-live="polite"]')).toHaveText('')
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
