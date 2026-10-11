import { expect, test, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { existsSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { parseEnv } from 'node:util'
import type { MentorSnapshot } from '../../src/api/mentor'
import { sourceSchema } from '../../src/api/research'
import { settingsSchema } from '../../src/api/settings'

const space = '00000000-0000-4000-8000-000000000001'
const topic = 'RFC 9111 HTTP Caching'
const evidenceDirectory = process.env.WEB_LIVE_EVIDENCE_DIR!

async function pair(page: Page, profile: 'settings' | 'research') {
  const code = execFileSync(
    '../../server/edu-agentd',
    ['pairing-code', 'create', '--profile', profile],
    {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      env: {
        PATH: process.env.PATH,
        DATABASE_URL: process.env.TEST_DATABASE_URL,
        MIGRATE_ON_START: 'true',
      },
    },
  ).trim()
  await page.goto('/app/')
  await page.getByLabel('配对码', { exact: true }).fill(code)
  await page.getByRole('button', { name: '配对并进入' }).click()
  await expect(page.getByRole('heading', { name: '今天想学会什么？' })).toBeVisible()
}

// 每次外部动作前持久记账；失败也占一笔，显式重跑须复用同一证据目录。
function reserveBraveAttempt(kind: 'probe' | 'research') {
  const path = join(evidenceDirectory, 'brave-attempts.json')
  const attempts: { kind: string; at: string }[] = existsSync(path)
    ? JSON.parse(readFileSync(path, 'utf8'))
    : []
  if (attempts.length >= 20) throw new Error('已达到本次验收的 20 次 Brave 调用上限')
  attempts.push({ kind, at: new Date().toISOString() })
  writeFileSync(path, JSON.stringify(attempts, null, 2), { mode: 0o600 })
}

// 会捕获摘要被当成正文、正文/引文身份失配、采纳未持久化及设置响应回显凭据的回归。
test('真实 Brave：空资料目标的候选、正文、归纳、采纳与可追溯来源', async ({
  page,
  browser,
}, testInfo) => {
  if (process.env.WEB_LIVE_SEARCH !== '1') throw new Error('需要显式启用真实搜索')
  for (const option of ['trace', 'screenshot', 'video'] as const)
    expect(testInfo.project.use[option], '含凭据场景禁止录制').toBe('off')

  const secretPath = join(homedir(), '.config/edu-agent-acceptance/search.env')
  expect(statSync(secretPath).mode & 0o777, 'Brave 环境文件必须为 0600').toBe(0o600)
  const braveKey = parseEnv(readFileSync(secretPath, 'utf8')).EDU_ACC_BRAVE_API_KEY
  if (!braveKey) throw new Error('未配置 Brave Key')
  const errors: string[] = []
  const settingsLeaks: Promise<boolean>[] = []
  const settingsContext = await browser.newContext({ baseURL: 'http://127.0.0.1:32929' })
  const settingsPage = await settingsContext.newPage()
  for (const current of [page, settingsPage]) {
    current.on('pageerror', (error) =>
      errors.push(error.message.replaceAll(braveKey, '[redacted]')),
    )
    current.on('response', (response) => {
      if (new URL(response.url()).pathname.startsWith('/v1/settings'))
        settingsLeaks.push(response.text().then((body) => body.includes(braveKey)))
    })
  }
  let configured = false
  const evidence: Record<string, unknown> = {
    topic,
    query: '(site:www.rfc-editor.org) (RFC 9111 HTTP Caching)',
    model: 'local workspace fixture',
  }
  try {
    await pair(settingsPage, 'settings')
    await settingsPage.getByRole('link', { name: '设置与能力' }).click()
    const model = settingsPage.getByRole('region', { name: 'Web 导师模型', exact: true })
    await model.getByRole('button', { name: '配置或替换凭据' }).click()
    await model.getByLabel('端点', { exact: true }).fill('http://127.0.0.1:32930/v1')
    await model.getByLabel('模型名称').fill('browser-workspace-fixture')
    await model.getByLabel('鉴权方式').selectOption('none')
    await model.getByLabel('启用Web 导师模型').check()
    await model.getByRole('button', { name: '保存Web 导师模型配置' }).click()
    await expect(settingsPage.getByText('配置已保存。', { exact: true })).toBeVisible()

    const search = settingsPage.getByRole('region', { name: '搜索', exact: true })
    await search.getByRole('button', { name: '配置或替换凭据' }).click()
    await search
      .getByLabel('端点', { exact: true })
      .fill('https://api.search.brave.com/res/v1/web/search')
    await search.getByLabel('启用搜索', { exact: true }).check()
    // fill(value) 的失败调用日志可能打印参数；只在浏览器内写原生密码输入框。
    await search.getByLabel('新 Key（留空保留同端点旧值）').evaluate((input, key) => {
      ;(input as HTMLInputElement).value = key
    }, braveKey)
    const savedResponse = settingsPage.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === '/v1/settings' &&
        response.request().method() === 'PUT',
    )
    await search.getByRole('button', { name: '保存搜索配置' }).click()
    const saved = await savedResponse
    const savedBody = await saved.text()
    expect(savedBody.includes(braveKey), '设置响应不得回显 Key').toBe(false)
    expect(saved.ok()).toBe(true)
    expect(settingsSchema.parse(JSON.parse(savedBody)).search.has_key).toBe(true)
    configured = true
    await expect(search.getByText('已安全保存，不能查看旧值', { exact: false })).toBeVisible()
    await search.getByRole('button', { name: '测试连接', exact: true }).click()
    const probeResponse = settingsPage.waitForResponse(
      (response) => new URL(response.url()).pathname === '/v1/settings/probes',
    )
    reserveBraveAttempt('probe')
    const probeStarted = Date.now()
    await settingsPage.getByRole('button', { name: '确认测试连接', exact: true }).click()
    const probe = await probeResponse
    const probeBody = await probe.text()
    expect(probeBody.includes(braveKey), '探测响应不得回显 Key').toBe(false)
    const probeResult = settingsSchema.parse(JSON.parse(probeBody)).search.probe!
    evidence.probe = { ...probeResult, duration_ms: Date.now() - probeStarted }
    expect(probeResult.status, probeResult.reason).toBe('ready')
    expect(probeResult.requests).toBe(1)
    await expect(search).toContainText('最近测试通过')

    await pair(page, 'research')
    const identity = await (await page.request.get('/v1/web/session')).json()
    expect(identity.device.scopes).toContain('research:adopt')
    expect(identity.device.scopes).not.toContain('settings:write')
    await page
      .getByLabel('今天想学会什么？', { exact: true })
      .fill(`理解 HTTP 缓存 ${randomUUID().slice(0, 8)}`)
    await page.getByRole('button', { name: '仅保存目标', exact: true }).click()
    await page.getByRole('link', { name: '查看目标', exact: true }).click()
    const goalID = page.url().split('/').at(-1)!
    const headers = { 'X-Learning-Space-ID': space }
    const head = await page.request.get('/v1/knowledge/revisions/head', { headers })
    expect(head.status(), '独立空 schema 不应预置资料').toBe(404)
    await page.getByRole('link', { name: '研究相关知识与查看来源 →', exact: true }).click()
    const researchURL = page.url()
    await page.getByLabel('公开研究主题', { exact: true }).fill(topic)
    await page.getByRole('combobox', { name: /^来源范围/ }).selectOption('restrict')
    await page.getByLabel('网站域名（最多 5 个，用逗号分隔）').fill('www.rfc-editor.org')
    await page.getByLabel('确认以上主题可外发', { exact: false }).check()
    await page.getByLabel('加密保存运行七天', { exact: false }).check()
    await page.getByLabel('外部请求预算', { exact: true }).fill('1')
    const acceptedResponse = page.waitForResponse(
      (response) =>
        /\/v1\/learning\/goals\/[^/]+\/runs$/.test(new URL(response.url()).pathname) &&
        response.request().method() === 'POST',
    )
    reserveBraveAttempt('research')
    const searchStarted = Date.now()
    await page.getByRole('button', { name: '研究相关知识', exact: true }).click()
    const accepted = await acceptedResponse
    expect(accepted.ok()).toBe(true)
    const runID = (await accepted.json()).run_id
    const snapshot = async () => {
      const response = await page.request.get(`/v1/learning/runs/${runID}`, { headers })
      expect(response.ok()).toBe(true)
      const current = (await response.json()) as MentorSnapshot
      evidence.last_snapshot = current
      return current
    }
    await expect
      .poll(
        async () => {
          const current = await snapshot()
          evidence.discovery = current
          return ['queued', 'running'].includes(current.status)
        },
        { timeout: 45_000 },
      )
      .toBe(false)
    const discovered = await snapshot()
    evidence.search_duration_ms = Date.now() - searchStarted
    expect(discovered.status, discovered.reason).toBe('paused_budget')
    await expect(page.getByRole('button', { name: '按以上预算继续' })).toBeVisible()
    expect(discovered.requests_used).toBe(1)
    expect(discovered.research!.discovered).toBe(true)
    expect(discovered.research!.sources.length).toBeGreaterThan(0)
    evidence.candidates = discovered.research!.sources.map((source) => ({
      locator: source.locator,
      title: source.title,
    }))
    for (const candidate of discovered.research!.sources) {
      expect(candidate.status).toBe('candidate')
      expect(candidate.text).toBe('')
      expect(candidate.fragments).toEqual([])
      expect(candidate.fetched_at).toBeUndefined()
      const card = page.locator(`#source-${candidate.id}`)
      await expect(card).toContainText('候选，尚未读取')
      await expect(card).toContainText('没有可显示的原文副本。')
      await expect(card.locator('blockquote')).toHaveCount(0)
    }

    await page.getByLabel('外部请求预算', { exact: true }).fill('9')
    const readStarted = Date.now()
    const continuedResponse = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === `/v1/learning/runs/${runID}/commands` &&
        response.request().method() === 'POST',
    )
    await page.getByRole('button', { name: '按以上预算继续' }).click()
    const continued = await continuedResponse
    expect(continued.ok()).toBe(true)
    const continuedVersion = (await continued.json()).version
    await expect
      .poll(
        async () => {
          const current = await snapshot()
          return (
            current.version < continuedVersion || ['queued', 'running'].includes(current.status)
          )
        },
        {
          timeout: 90_000,
        },
      )
      .toBe(false)
    const completed = await snapshot()
    evidence.read_and_synthesis_duration_ms = Date.now() - readStarted
    expect(completed.saved).toBe(true)
    expect(['partial', 'succeeded'], completed.reason).toContain(completed.status)
    await expect(page.getByRole('status').filter({ hasText: /研究结束/ })).toBeVisible()
    const state = completed.research!
    expect(state.sources.map((source) => source.id)).toEqual(
      discovered.research!.sources.map((source) => source.id),
    )
    expect(state.synthesis!.points.length).toBeGreaterThan(0)
    const citation = state.synthesis!.points[0].citations[0]
    const source = state.sources.find((source) => source.id === citation.source_id)!
    const fragment = source.fragments.find((fragment) => fragment.id === citation.fragment_id)!
    expect(source.final_url).toMatch(/^https:\/\/(www\.)?rfc-editor\.org\//)
    expect(source.text).toContain('HTTP Caching')
    expect(source.fetched_at).toBeTruthy()
    expect(source.fingerprint).toMatch(/^[a-f0-9]{64}$/)
    expect(source.fragments.length).toBeGreaterThan(0)
    expect(citation.revision_id).toBe(source.revision_id)
    expect(Buffer.from(source.text).subarray(fragment.start, fragment.end).toString()).toBe(
      fragment.text,
    )
    expect(fragment.text).toContain(citation.quote)
    const sourceCard = page.locator(`#source-${source.id}`)
    await expect(sourceCard).toContainText(source.final_url)
    await expect(sourceCard).toContainText(
      source.coverage === 'complete_text' ? '完整文本' : '部分解析，可能有遗漏',
    )
    if (source.coverage !== 'complete_text') expect(completed.status).toBe('partial')
    await expect(page.getByRole('heading', { name: 'AI 综合要点', exact: true })).toBeVisible()
    await page.getByRole('link', { name: '查看支持片段', exact: true }).first().click()
    await expect(page).toHaveURL(new RegExp(`#fragment-${fragment.id}$`))
    const visibleFragment = page.locator(`#fragment-${fragment.id}`)
    await expect(visibleFragment).toHaveAttribute('open', '')
    await expect(visibleFragment.locator('blockquote')).toHaveText(fragment.text)
    await expect(visibleFragment.locator('summary')).toContainText(
      `字节 ${fragment.start}–${fragment.end}`,
    )

    await sourceCard.getByRole('button', { name: '纳入本目标参考', exact: true }).click()
    await expect(sourceCard).toContainText('已纳入本目标参考')
    const adopted = (await snapshot()).research!.sources.find(
      (current) => current.id === source.id,
    )!
    expect(adopted.status).toBe('adopted')
    expect(adopted.knowledge_revision_id).toBeTruthy()
    expect(adopted.collection_id).toBeTruthy()
    const sourceListResponse = await page.request.get(`/v1/learning/runs/${runID}/sources`, {
      headers,
    })
    expect(sourceListResponse.ok()).toBe(true)
    expect(
      (await sourceListResponse.json()).items.some(
        (item: { id: string; status: string }) =>
          item.id === source.id && item.status === 'adopted',
      ),
    ).toBe(true)
    const detailResponse = await page.request.get(
      `/v1/learning/runs/${runID}/sources/${source.id}`,
      { headers },
    )
    expect(detailResponse.ok()).toBe(true)
    const detail = sourceSchema.parse(await detailResponse.json())
    expect(detail.fragments).toEqual(source.fragments)
    expect(detail.locator).toBe(source.locator)
    await page.reload()
    await expect(page.getByRole('heading', { name: 'AI 综合要点', exact: true })).toBeVisible()
    await expect(page.locator(`#source-${source.id}`)).toContainText('已纳入本目标参考')
    await page.getByRole('link', { name: '查看支持片段', exact: true }).first().click()
    await expect(page.locator(`#fragment-${fragment.id} blockquote`)).toHaveText(fragment.text)
    const refreshed = await snapshot()
    expect(refreshed.research!.synthesis).toEqual(state.synthesis)
    expect(refreshed.requests_used).toBe(completed.requests_used)
    evidence.result = {
      goal_id: goalID,
      run_id: runID,
      research_url: researchURL,
      status: completed.status,
      requests_used: completed.requests_used,
      brave_calls: probeResult.requests + discovered.requests_used,
    }
    evidence.sources = state.sources.map((current) => ({
      locator: current.locator,
      final_url: current.final_url,
      status: current.id === source.id ? 'adopted' : current.status,
      coverage: current.coverage,
      failure: current.failure,
      fetched_at: current.fetched_at,
      fingerprint: current.fingerprint,
      fragment_count: current.fragments.length,
    }))
    evidence.adopted = {
      ...adopted,
      text: undefined,
      fragments: adopted.fragments.filter((current) => current.id === fragment.id),
    }
    evidence.citation = citation
    const modelStats = await page.request.get('http://127.0.0.1:32930/stats')
    expect(modelStats.ok()).toBe(true)
    evidence.model = await modelStats.json()
    expect((evidence.model as { calls: number }).calls).toBe(1)
    expect(errors, '浏览器不应有 pageerror').toEqual([])
  } finally {
    if (configured) {
      await settingsPage.reload()
      const search = settingsPage.getByRole('region', { name: '搜索', exact: true })
      await search.getByRole('button', { name: '清除凭据', exact: true }).click()
      await settingsPage.getByRole('button', { name: '确认清除凭据', exact: true }).click()
      await expect(search).toContainText('未保存')
    }
    await settingsContext.close()
    const leaked = (await Promise.all(settingsLeaks)).some(Boolean)
    evidence.settings_responses_checked = settingsLeaks.length
    evidence.settings_key_leaked = leaked
    evidence.pageerrors = errors
    writeFileSync(
      join(evidenceDirectory, `evidence-${Date.now()}.json`),
      JSON.stringify(evidence, null, 2),
      { mode: 0o600 },
    )
    expect(leaked, '所有设置 API 响应均不得包含 Key').toBe(false)
    expect(errors, '含设置清理步骤的浏览器 pageerror 应为空').toEqual([])
  }
})
