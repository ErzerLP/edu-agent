import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { captureNotebook } from './notebook-visual'
import { randomUUID } from 'node:crypto'
import { pairNotebook, createSpace, webAPI, seedSources, setNotebookTheme } from './notebook-fixture'

async function configureStudioModel(page: Page) {
  await pairNotebook(page, 'settings')
  await page.goto('/app/settings')
  const model = page.getByRole('region', { name: 'Web 导师模型', exact: true })
  await model.getByRole('button', { name: '配置或替换凭据' }).click()
  await model.getByLabel('端点', { exact: true }).fill('http://127.0.0.1:32930/v1')
  await model.getByLabel('模型名称').fill('browser-studio-fixture')
  await model.getByLabel('鉴权方式').selectOption('none')
  await model.getByLabel('启用Web 导师模型').check()
  await model.getByRole('button', { name: '保存Web 导师模型配置' }).click()
  await expect(page.getByText('配置已保存。', { exact: true })).toBeVisible()
  const session = await (await page.request.get('/v1/web/session')).json()
  const logout = await page.request.post('/v1/web/logout', {
    headers: { Origin: 'http://127.0.0.1:32929', 'X-CSRF-Token': session.csrf_token },
  })
  expect(logout.ok()).toBe(true)
}

test('旧固定结构 API 生成指南和简报、引用定位、刷新重开、取消并重新生成版本', async ({ page }) => {
  test.setTimeout(120000)
  await configureStudioModel(page)
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'Studio 生成验收')
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources.getByRole('article')).toHaveCount(2)
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  // 新 Web 入口只建立待确认大纲；此用例继续验证旧固定结构 API 及旧产物读取。
  const catalog = await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)
  const selected = catalog.items.find((item: any) => item.path === 'even.md')
  expect(selected).toBeTruthy()
  const { collection_id, revision_id, document_id, document_revision_id } = selected
  const source = { collection_id, revision_id, document_id, document_revision_id }
  const session = await webAPI(page, 'GET', '/v1/web/session')
  const generateLegacy = (kind: 'guide' | 'brief', artifactId: string | null = null, version = 0) =>
    webAPI(page, 'POST', '/v1/learning/studio/generations', {
      operation_id: randomUUID(),
      privacy_generation: session.generation,
      kind,
      goal_id: null,
      goal_version: 0,
      artifact_id: artifactId,
      expected_version: version,
      source_selection: { mode: 'items', items: [source] },
    }, space.id)
  const openLegacyRun = async (runId: string) => {
    const runs = await webAPI(page, 'GET', '/v1/learning/runs?task_kind=studio', undefined, space.id)
    const index = runs.items.findIndex((run: any) => run.run_id === runId)
    expect(index).toBeGreaterThanOrEqual(0)
    await studio.getByLabel('生成记录', { exact: true }).click()
    const runButtons = studio.getByRole('region', { name: 'Studio 生成记录', exact: true }).getByRole('button')
    await expect(runButtons).toHaveCount(runs.items.length)
    const opened = page.waitForResponse(response =>
      new URL(response.url()).pathname === `/v1/learning/runs/${runId}` &&
      response.request().method() === 'GET',
    )
    await runButtons.nth(index).click()
    const snapshot = await (await opened).json()
    expect(snapshot.studio.request.goal_id).toBeNull()
    expect(snapshot.studio.request.source_selection.items).toEqual([source])
  }
  for (const kind of ['guide', 'brief'] as const) {
    const receipt = await generateLegacy(kind)
    await openLegacyRun(receipt.run_id)
    await studio.getByRole('button', { name: '打开已保存产物' }).click()
    await expect(
      studio.getByRole('heading', {
        name: kind === 'guide' ? '核心要点' : '一页综述',
        exact: true,
      }),
    ).toBeVisible()
    await studio.getByRole('button', { name: '查看引用 1', exact: true }).first().click()
    await expect(sources.getByRole('region', { name: '引用 1 核验片段' })).toContainText(
      '偶数可以被 2 整除',
    )
    await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  }
  const artifacts = await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)
  const originalGuide = artifacts.items.find((item: any) => item.kind === 'guide')
  expect(originalGuide.version).toBe(1)
  for (const theme of ['light', 'dark'] as const) {
    await setNotebookTheme(page, theme)
    const library = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa']).analyze()
    expect(library.violations.map(v => ({ id: v.id, nodes: v.nodes.map(n => n.target) })), `Studio 列表 ${theme}`).toEqual([])
    await page.screenshot({ path: test.info().outputPath(`studio-list-${theme}.png`) })
    await studio.getByRole('button', { name: /夹具学习指南.*第 1 版/ }).click()
    const document = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa']).analyze()
    expect(document.violations.map(v => ({ id: v.id, nodes: v.nodes.map(n => n.target) })), `Studio 指南 ${theme}`).toEqual([])
    await page.screenshot({ path: test.info().outputPath(`studio-guide-${theme}.png`) })
    await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  }
  await page.reload()
  await studio.getByRole('button', { name: /夹具学习指南.*第 1 版/ }).click()
  await expect(studio.getByRole('heading', { name: '术语', exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  const cancelled = await generateLegacy('guide')
  await openLegacyRun(cancelled.run_id)
  await expect(studio.getByText('正在生成', { exact: true })).toBeVisible()
  const stopResponses: number[] = []
  await page.route('**/v1/learning/runs/*/commands', async route => {
    expect(route.request().postDataJSON().kind).toBe('stop')
    const response = await route.fetch()
    stopResponses.push(response.status())
    await route.fulfill({ response })
  })
  await studio.getByRole('button', { name: '取消生成', exact: true }).click()
  await expect(studio.getByText('已停止', { exact: true })).toBeVisible({ timeout: 15000 })
  await expect(studio).not.toContainText('结果未知')
  await expect(studio).not.toContainText('user_stopped')
  expect(stopResponses).toEqual([202])
  await page.screenshot({ path: test.info().outputPath('studio-stopped.png'), fullPage: true })
  await page.unroute('**/v1/learning/runs/*/commands')
  await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  const regenerated = await generateLegacy('guide', originalGuide.artifact_id, originalGuide.version)
  await openLegacyRun(regenerated.run_id)
  await studio.getByRole('button', { name: '打开已保存产物' }).click()
  await expect(studio.getByText(/已保存第 2 版/)).toBeVisible()
  await studio.getByLabel('产物版本').selectOption('1')
  await expect(studio.getByText(/已保存第 1 版/)).toBeVisible()
})

test('C4 无手选目标生成测验、共享卡片作答与反馈、改方向后继续旧题组', async ({ page }) => {
  test.setTimeout(120000)
  page.setDefaultTimeout(15000)
  await configureStudioModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(),
    expected_version: 0,
    name: 'Studio 方向测验',
    description: '',
    status: 'active',
    direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const sources = page.getByRole('region', { name: '来源', exact: true })
  const chat = page.getByRole('region', { name: '对话', exact: true })
  await chat.getByLabel('向导师提问').fill('CHAT_C4_DRAFT')
  expect((await webAPI(page, 'GET', '/v1/learning/goals', undefined, space.id)).items).toHaveLength(
    0,
  )
  await expect(studio.getByRole('button', { name: /^测验/ })).toBeEnabled()
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  const outgoing: any[] = []
  page.on('request', (r) => {
    if (r.url().endsWith('/studio/generations') && r.method() === 'POST')
      outgoing.push(r.postDataJSON())
  })
  await studio.getByRole('button', { name: /^测验/ }).click()
  await expect(studio).toContainText('学习方向：理解偶数与整除')
  await expect(studio.getByRole('combobox')).toHaveCount(0)
  await studio.getByRole('button', { name: '开始生成测验', exact: true }).click()
  await studio.getByRole('button', { name: '打开已保存产物' }).click()
  expect(outgoing[0]).toMatchObject({
    kind: 'quiz',
    mode: 'direction',
    direction_version: 1,
    goal_id: null,
    goal_version: 0,
  })
  expect(outgoing[0].source_selection.items).toHaveLength(1)
  const list = await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)
  const id = list.items[0].artifact_id
  const frozen = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/artifacts/${id}?version=1`,
    undefined,
    space.id,
  )
  expect(frozen.body.quiz.direction.version).toBe(1)
  await page.route('**/quiz/start', async (route) => {
    await route.fetch()
    await route.abort('failed')
  })
  await studio.getByRole('button', { name: '开始作答', exact: true }).click()
  await expect(studio.getByText('开始结果待核对，请先核对原操作。')).toBeVisible()
  await studio.getByRole('button', { name: '核对原开始操作', exact: true }).click()
  await page.unroute('**/quiz/start')
  const first = studio.getByRole('region', { name: '第 1 题作答', exact: true })
  const card = first.getByRole('article', { name: '对话练习' })
  await expect(card.getByRole('button', { name: '提交答案', exact: true })).toBeDisabled()
  await card.getByLabel('练习答案').fill('偶数能被 2 整除，例如 4。')
  await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  await studio.getByRole('button', { name: /夹具偶数测验.*第 1 版/ }).click()
  await expect(card.getByLabel('练习答案')).toHaveValue('偶数能被 2 整除，例如 4。')
  // Saved quiz hints keep the original manifest after the left selection changes.
  await sources.getByRole('checkbox', { name: /examples.md/ }).check()
  await sources.getByRole('checkbox', { name: /even.md/ }).uncheck()
  await card.getByRole('button', { name: '给我一个提示' }).click()
  await expect(card).toContainText('先找出题目中的关键概念')
  await expect(card.getByLabel('我得到的帮助').locator('option[value="none"]')).toHaveCount(0)
  await card.getByLabel('我得到的帮助').selectOption('hint')
  await captureNotebook(page, 'studio-practice', 'Studio')
  let answerPosts = 0
  await page.route('**/v1/learning/content/*/answers', async (route) => {
    answerPosts++
    await route.fetch()
    await route.abort('failed')
  })
  await card.getByRole('button', { name: '提交答案', exact: true }).click()
  const feedback = first.getByRole('article', { name: '练习反馈' })
  await expect(feedback).toContainText('你找到了关键点')
  expect(answerPosts).toBe(1)
  await page.unroute('**/v1/learning/content/*/answers')
  await feedback.getByRole('button', { name: '查看依据 1' }).click()
  await expect(sources.getByRole('region', { name: /引用 1 核验片段/ })).toContainText('偶数')
  await captureNotebook(page, 'studio-feedback', 'Studio')
  await feedback.getByRole('button', { name: '继续学习', exact: true }).click()
  await expect(studio.getByText('已完成 1 / 2 题', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '学习区设置', exact: true }).click()
  const settings = page.getByRole('dialog', { name: '学习区设置', exact: true })
  await settings.getByLabel('学习方向', { exact: true }).fill('用新例子学习整除')
  await settings.getByRole('button', { name: '保存设置', exact: true }).click()
  await expect(settings).not.toBeVisible()
  await studio.getByRole('button', { name: '重新生成新版本', exact: true }).click()
  await studio.getByRole('button', { name: '开始生成测验', exact: true }).click()
  await studio.getByRole('button', { name: '打开已保存产物' }).click()
  expect(outgoing[1]).toMatchObject({
    mode: 'direction',
    direction_version: 2,
    goal_id: null,
    goal_version: 0,
  })
  const revised = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/artifacts/${id}?version=2`,
    undefined,
    space.id,
  )
  expect(revised.body.quiz.direction.goal_revision_id).not.toBe(
    frozen.body.quiz.direction.goal_revision_id,
  )
  await studio.getByLabel('产物版本').selectOption('1')
  await studio.getByRole('button', { name: '开始第 2 题', exact: true }).click()
  const second = studio.getByRole('region', { name: '第 2 题作答', exact: true })
  await second.getByLabel('练习答案').fill('4 可以被 2 整除，所以它是偶数。')
  await second.getByLabel('我得到的帮助').selectOption('none')
  await second.getByRole('button', { name: '提交答案', exact: true }).click()
  await expect(second.getByRole('article', { name: '练习反馈' })).toContainText('你找到了关键点')
  await second.getByRole('button', { name: '继续学习', exact: true }).click()
  await expect(studio.getByText('已完成 2 / 2 题', { exact: true })).toBeVisible()
  const state = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/artifacts/${id}/quiz?version=1`,
    undefined,
    space.id,
  )
  for (const q of state.questions) {
    const practice = await webAPI(
      page,
      'GET',
      `/v1/learning/studio/exercises/${q.exercise_id}`,
      undefined,
      space.id,
    )
    expect(practice.feedback.activity.goal_revision_id).toBe(frozen.body.quiz.direction.goal_revision_id)
    expect(practice.feedback.evidence).toHaveLength(1)
  }
  await expect(chat.getByLabel('向导师提问')).toHaveValue('CHAT_C4_DRAFT')
  await captureNotebook(page, 'studio-old-direction-complete', 'Studio')
  await page.reload()
  await studio.getByRole('button', { name: /夹具偶数测验.*第 2 版/ }).click()
  await studio.getByLabel('产物版本').selectOption('1')
  await expect(studio.getByText('已完成 2 / 2 题', { exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '查看第 1 题作答', exact: true }).click()
  await expect(first.getByRole('article', { name: '练习反馈' })).toContainText('你找到了关键点')
})

test('Studio 拒绝缺失 CSRF 的写入，读取撤权后清除尚未保存的草稿', async ({ page }) => {
  await pairNotebook(page)
  const space = await createSpace(page, 'Studio 权限隔离')
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await studio.getByRole('button', { name: '＋ 添加笔记' }).click()
  await studio.getByRole('textbox', { name: '笔记正文', exact: true }).fill('REVOKED_DRAFT')
  const denied = await page.request.post('/v1/learning/studio/artifacts', {
    headers: { Origin: 'http://127.0.0.1:32929', 'X-Learning-Space-ID': space.id },
    data: {
      operation_id: randomUUID(),
      expected_version: 0,
      privacy_generation: 1,
      kind: 'note',
      goal_id: null,
      title: '伪造保存',
      markdown: 'no csrf',
    },
  })
  expect(denied.status()).toBe(403)
  await page.route('**/v1/learning/studio/artifacts', (route) =>
    route.fulfill({
      status: 403,
      contentType: 'application/json',
      body: JSON.stringify({ error: { code: 'learning_content_forbidden' } }),
    }),
  )
  await expect(studio.getByText(/已隐藏正文并清除本区笔记草稿/)).toBeVisible({ timeout: 10000 })
  await expect(studio.getByRole('textbox')).toHaveCount(0)
  await page.unroute('**/v1/learning/studio/artifacts')
  await studio.getByRole('button', { name: '＋ 添加笔记' }).click({ timeout: 10000 })
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('')
})

test('笔记保存重开、冲突保留草稿，已保存版本转来源后不随笔记修改', async ({ page }) => {
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'Studio 无目标学习区')
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await studio.getByRole('button', { name: '＋ 添加笔记', exact: true }).click()
  await studio.getByLabel('笔记标题', { exact: true }).fill('矩阵秩笔记')
  await studio
    .getByRole('textbox', { name: '笔记正文', exact: true })
    .fill('# 秩的理解\n\nNOTE_SAVED_V1：向量排成矩阵后比较秩。')
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/已保存第 1 版/).first()).toBeVisible()
  await page.reload()
  await studio.getByRole('button', { name: /矩阵秩笔记.*第 1 版/ }).click()
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(
    /NOTE_SAVED_V1/,
  )
  const list = await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)
  const id = list.items[0].artifact_id
  const saved = await webAPI(
    page,
    'GET',
    `/v1/learning/studio/artifacts/${id}`,
    undefined,
    space.id,
  )
  await studio
    .getByRole('textbox', { name: '笔记正文', exact: true })
    .fill('LOCAL_DRAFT：我的修改必须保留。')
  await webAPI(
    page,
    'PUT',
    `/v1/learning/studio/artifacts/${id}`,
    {
      operation_id: randomUUID(),
      expected_version: 1,
      privacy_generation: saved.privacy_generation,
      kind: 'note',
      goal_id: null,
      title: '矩阵秩笔记',
      markdown: 'REMOTE_V2：另一标签页已保存。',
    },
    space.id,
  )
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(
    studio.getByText('保存版本冲突，本地草稿已保留。请读取最新版本并比较后再保存。'),
  ).toBeVisible()
  await expect(studio.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(
    'LOCAL_DRAFT：我的修改必须保留。',
  )
  await studio.getByRole('button', { name: '读取最新版本并保留草稿' }).click()
  await expect(studio.getByText('REMOTE_V2：另一标签页已保存。', { exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '以已核对版本保存草稿' }).click()
  await expect(studio.getByText(/已保存第 3 版/).first()).toBeVisible()
  await studio.getByLabel('转为来源的已保存版本').selectOption('1')
  await studio.getByRole('button', { name: '转为来源', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '笔记转为来源', exact: true })
  await expect(dialog).toContainText('第 1 版')
  await dialog.getByLabel('新集合名称').fill('笔记快照来源')
  await dialog.getByRole('button', { name: '创建并选用集合' }).click()
  await dialog.getByRole('button', { name: '生成服务端预览' }).click()
  await expect(dialog.getByText('尚未正式导入', { exact: true })).toBeVisible()
  expect((await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)).items).toEqual(
    [],
  )
  await dialog.getByRole('button', { name: '确认正式导入这 1 项资料' }).click()
  await expect(dialog.getByText('资料已保存')).toBeVisible()
  await dialog.getByRole('button', { name: '关闭笔记导入' }).click()
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources).toContainText('笔记第 1 版快照')
  const source = (await webAPI(page, 'GET', '/v1/knowledge/sources', undefined, space.id)).items[0]
  expect(source.metadata.derived_from).toEqual({ artifact_id: id, version: 1 })
  await sources.getByRole('button', { name: `打开来源：${source.path}`, exact: true }).click()
  const viewer = page.getByRole('region', { name: '来源查看器', exact: true })
  await viewer.getByText(/原文片段 1/).click()
  await expect(viewer).toContainText('NOTE_SAVED_V1')
  await expect(viewer).not.toContainText('LOCAL_DRAFT')
  await viewer.getByRole('button', { name: /关闭/ }).click()
  await studio
    .getByRole('textbox', { name: '笔记正文', exact: true })
    .fill('NOTE_V4：此修改不会同步到来源。')
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/已保存第 4 版/).first()).toBeVisible()
  await sources.getByRole('button', { name: `打开来源：${source.path}`, exact: true }).click()
  await viewer.getByText(/原文片段 1/).click()
  await expect(viewer).toContainText('NOTE_SAVED_V1')
  await expect(viewer).not.toContainText('NOTE_V4')
})

test('笔记草稿在标签页和学习区之间隔离，保存丢响应后核对原操作', async ({ page }) => {
  await pairNotebook(page)
  const space = await createSpace(page, 'Studio 草稿隔离')
  const other = await createSpace(page, 'Studio 另一区')
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  await studio.getByRole('button', { name: '＋ 添加笔记' }).click()
  await studio.getByLabel('笔记标题').fill('独立草稿')
  await studio.getByRole('textbox', { name: '笔记正文', exact: true }).fill('TAB_ONLY_DRAFT')
  const second = await page.context().newPage()
  await second.goto(`/app/spaces/${space.id}?panel=studio`)
  await second.getByRole('button', { name: '＋ 添加笔记' }).click()
  await expect(second.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('')
  await second.close()
  await page.getByRole('link', { name: '所有学习区', exact: true }).click()
  await page.getByRole('link', { name: 'Studio 另一区', exact: true }).click()
  await page.getByRole('button', { name: '＋ 添加笔记' }).click()
  await expect(page.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('')
  await page.getByRole('link', { name: '所有学习区', exact: true }).click()
  await page.getByRole('link', { name: 'Studio 草稿隔离', exact: true }).click()
  await page.getByRole('button', { name: '＋ 添加笔记' }).click()
  await expect(page.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue(
    'TAB_ONLY_DRAFT',
  )
  await page.route('**/v1/learning/studio/artifacts', async (route) => {
    if (route.request().method() === 'POST') {
      await route.fetch()
      await route.abort('failed')
    } else await route.continue()
  })
  await studio.getByRole('button', { name: '保存笔记', exact: true }).click()
  await expect(studio.getByText(/保存结果未知/)).toBeVisible()
  await studio.getByRole('button', { name: '核对笔记保存结果' }).click()
  await expect(studio.getByText(/已保存第 1 版/).first()).toBeVisible()
  expect(
    (await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, space.id)).items,
  ).toHaveLength(1)
  expect(
    (await webAPI(page, 'GET', '/v1/learning/studio/artifacts', undefined, other.id)).items,
  ).toHaveLength(0)
})
