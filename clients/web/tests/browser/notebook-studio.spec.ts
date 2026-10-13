import { test, expect, type Page } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { pairNotebook, createSpace, webAPI, seedSources } from './notebook-fixture'

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

test('所选来源生成指南和简报、引用定位、刷新重开、取消并重新生成版本', async ({ page }) => {
  await configureStudioModel(page)
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'Studio 生成验收')
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}?panel=studio`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const sources = page.getByRole('region', { name: '来源', exact: true })
  await expect(sources.getByRole('article')).toHaveCount(2)
  await sources.getByRole('checkbox', { name: /examples.md/ }).uncheck()
  const outgoing: any[] = []
  page.on('request', (r) => {
    if (r.url().endsWith('/studio/generations') && r.method() === 'POST')
      outgoing.push(r.postDataJSON())
  })
  for (const kind of ['学习指南', '简报']) {
    await studio.getByRole('button', { name: new RegExp(`^${kind}`) }).click()
    await studio.getByRole('button', { name: `开始生成${kind}`, exact: true }).click()
    await studio.getByRole('button', { name: '打开已保存产物' }).click()
    await expect(
      studio.getByRole('heading', {
        name: kind === '学习指南' ? '核心要点' : '一页综述',
        exact: true,
      }),
    ).toBeVisible()
    await studio.getByRole('button', { name: '查看引用 1', exact: true }).first().click()
    await expect(sources.getByRole('region', { name: '引用 1 核验片段' })).toContainText(
      '偶数可以被 2 整除',
    )
    await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  }
  expect(outgoing).toHaveLength(2)
  expect(outgoing.every((c) => c.goal_id === null && c.source_selection.items.length === 1)).toBe(
    true,
  )
  await page.reload()
  await studio.getByRole('button', { name: /夹具学习指南.*第 1 版/ }).click()
  await expect(studio.getByRole('heading', { name: '术语', exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  await studio.getByRole('button', { name: /^学习指南/ }).click()
  await studio.getByRole('button', { name: '开始生成学习指南', exact: true }).click()
  await expect(studio.getByText('正在生成', { exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '取消生成', exact: true }).click()
  await expect(studio.getByText('已停止', { exact: true })).toBeVisible({ timeout: 15000 })
  await studio.getByRole('button', { name: '返回 Studio', exact: true }).click()
  await studio.getByRole('button', { name: /夹具学习指南.*第 1 版/ }).click()
  await studio.getByRole('button', { name: '重新生成新版本' }).click()
  await studio.getByRole('button', { name: '开始生成学习指南', exact: true }).click()
  await studio.getByRole('button', { name: '打开已保存产物' }).click()
  await expect(studio.getByText(/已保存第 2 版/)).toBeVisible()
  await studio.getByLabel('产物版本').selectOption('1')
  await expect(studio.getByText(/已保存第 1 版/)).toBeVisible()
})

test('测验明确选择目标、正式逐题作答、帮助限制与丢响应核对，原课堂草稿保留', async ({ page }) => {
  test.setTimeout(120000)
  await configureStudioModel(page)
  await pairNotebook(page, 'references')
  const space = await createSpace(page, 'Studio 正式测验')
  const source = await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  const studio = page.getByRole('region', { name: 'Studio', exact: true })
  const classroom = page.getByRole('region', { name: '课堂', exact: true })
  await expect(studio.getByRole('button', { name: /^测验/ })).toBeDisabled()
  expect((await webAPI(page, 'GET', '/v1/learning/goals', undefined, space.id)).items).toHaveLength(
    0,
  )
  await page.goto(`/app/spaces/${space.id}/manage`)
  const goalText = '偶数测验与课堂共存'
  await page.getByLabel('今天想学会什么？', { exact: true }).fill(goalText)
  await page.getByRole('button', { name: '仅保存目标', exact: true }).click()
  await expect(page.getByText('目标已保存，尚未启动教学。')).toBeVisible()
  const goal = (await webAPI(page, 'GET', '/v1/learning/goals', undefined, space.id)).items[0]
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
      source: '测验浏览器夹具',
      details: { ...goal.management.details, scope_snapshot_id: source.scopeId },
    },
    space.id,
  )
  await page.reload()
  await page.getByRole('link', { name: goalText, exact: true }).click()
  await page.getByRole('button', { name: '新建教学会话', exact: true }).click()
  await classroom.getByRole('button', { name: '开始学习', exact: true }).click()
  await classroom.getByRole('button', { name: '生成学习路线', exact: true }).click()
  await classroom.getByRole('button', { name: '生成下一活动', exact: true }).click()
  await classroom.getByRole('button', { name: '开始当前活动', exact: true }).click()
  const originalId = new URL(page.url()).pathname.split('/').at(-1)!
  await classroom
    .getByRole('textbox', { name: '我的正式答案', exact: true })
    .fill('ORIGINAL_CLASSROOM_DRAFT')
  const original = await webAPI(
    page,
    'GET',
    `/v1/tutoring/sessions/${originalId}`,
    undefined,
    space.id,
  )
  await page
    .getByRole('region', { name: '来源', exact: true })
    .getByRole('checkbox', { name: /examples.md/ })
    .uncheck()
  const outgoing: any[] = []
  page.on('request', (r) => {
    if (r.url().endsWith('/studio/generations') && r.method() === 'POST')
      outgoing.push(r.postDataJSON())
  })
  await studio.getByRole('button', { name: /^测验/ }).click()
  await studio.getByRole('button', { name: '开始生成测验', exact: true }).click()
  await studio.getByRole('button', { name: '打开已保存产物' }).click()
  await expect(studio.getByRole('heading', { name: '夹具偶数测验', exact: true })).toBeVisible()
  expect(outgoing[0]).toMatchObject({ kind: 'quiz', goal_id: goal.goal_id, goal_version: 2 })
  expect(outgoing[0].source_selection.items).toHaveLength(1)
  await page.route('**/quiz/start', async (route) => {
    await route.fetch()
    await route.abort('failed')
  })
  await studio.getByRole('button', { name: '开始作答', exact: true }).click()
  await expect(studio.getByText('开始结果待核对，请先核对原操作。')).toBeVisible()
  await studio.getByRole('button', { name: '核对原开始操作', exact: true }).click()
  await page.unroute('**/quiz/start')
  const quiz = studio.getByRole('region', { name: '第 1 题作答', exact: true })
  await quiz.getByRole('button', { name: '开始当前活动', exact: true }).click()
  const answer = quiz.getByRole('textbox', { name: '我的正式答案', exact: true })
  await answer.fill('偶数可被 2 整除，例如 2。')
  await expect(quiz.getByRole('button', { name: '提交正式答案', exact: true })).toBeDisabled()
  await expect(quiz.getByLabel('作答帮助等级')).toHaveValue('')
  await quiz.getByLabel('作答帮助等级').selectOption('hint')
  let answerPosts = 0
  await page.route('**/v1/learning/content/*/answers', async (route) => {
    answerPosts++
    expect(route.request().postDataJSON().help).toBe('hint')
    await route.fetch()
    await route.abort('failed')
  })
  await quiz.getByRole('button', { name: '提交正式答案', exact: true }).click()
  await expect(quiz.getByText('已核对原操作：提交已保存。', { exact: true })).toBeVisible()
  expect(answerPosts).toBe(1)
  await page.unroute('**/v1/learning/content/*/answers')
  await quiz.getByRole('button', { name: '获取教学反馈', exact: true }).click()
  await expect(quiz.getByRole('region', { name: '教学反馈', exact: true })).toContainText('已接纳')
  await quiz.getByRole('button', { name: '查看完毕，继续学习', exact: true }).click()
  await expect(studio.getByText('已完成 1 / 2 题', { exact: true })).toBeVisible()
  await studio.getByRole('button', { name: '开始第 2 题', exact: true }).click()
  const second = studio.getByRole('region', { name: '第 2 题作答', exact: true })
  await second.getByRole('button', { name: '开始当前活动', exact: true }).click()
  await second
    .getByRole('textbox', { name: '我的正式答案', exact: true })
    .fill('偶数能被 2 整除，例如 4。')
  await expect(second.getByLabel('作答帮助等级')).toHaveValue('none')
  await second.getByRole('button', { name: '提交正式答案', exact: true }).click()
  await second.getByRole('button', { name: '获取教学反馈', exact: true }).click()
  await expect(second.getByRole('region', { name: '教学反馈', exact: true })).toContainText(
    '已接纳',
  )
  await second.getByRole('button', { name: '查看完毕，继续学习', exact: true }).click()
  await expect(studio.getByText('已完成 2 / 2 题', { exact: true })).toBeVisible()
  await expect(classroom.getByRole('textbox', { name: '我的正式答案', exact: true })).toHaveValue(
    'ORIGINAL_CLASSROOM_DRAFT',
  )
  const unchanged = await webAPI(
    page,
    'GET',
    `/v1/tutoring/sessions/${originalId}`,
    undefined,
    space.id,
  )
  expect(unchanged.session.aggregate_version).toBe(original.session.aggregate_version)
  expect(unchanged.work_item.activity.activity_id).toBe(original.work_item.activity.activity_id)
  await studio.getByRole('link', { name: '查看学习进度', exact: true }).click()
  await expect(page.getByText('夹具偶数测验 · 第 1 题', { exact: false }).first()).toBeVisible()
  await page.goBack()
  await expect(classroom.getByRole('textbox', { name: '我的正式答案', exact: true })).toHaveValue(
    'ORIGINAL_CLASSROOM_DRAFT',
  )
  await studio.getByRole('button', { name: /夹具偶数测验.*第 1 版/ }).click()
  await expect(studio.getByText('已完成 2 / 2 题', { exact: true })).toBeVisible()
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
  await expect(studio.getByRole('button', { name: /^测验/ })).toBeDisabled()
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
