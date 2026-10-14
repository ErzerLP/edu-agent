import { test, expect, type Locator, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { randomUUID } from 'node:crypto'
import { configureNotebookModel, pairNotebook, seedSources, setNotebookTheme, webAPI } from './notebook-fixture'

async function audit(page: Page, label: string) {
  await page.bringToFront()
  const controls = await page.locator('select').evaluateAll(elements => elements.map(element => {
    const select = element as HTMLSelectElement
    return {
      label: select.getAttribute('aria-label'),
      color: getComputedStyle(select).color,
      fill: getComputedStyle(select).getPropertyValue('-webkit-text-fill-color'),
      selectedColor: select.selectedOptions[0] && getComputedStyle(select.selectedOptions[0]).color,
      background: getComputedStyle(select).backgroundColor,
    }
  }))
  const result = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa']).analyze()
  // Contrast is also checked explicitly, so a tag change cannot silently omit it.
  await page.bringToFront()
  const contrast = await new AxeBuilder({ page }).withRules(['color-contrast']).analyze()
  // axe cannot resolve the top-layer dialog's backdrop overlap. Check its
  // actual opaque backgrounds as well, while retaining both axe assertions.
  const dialogContrast = label === '菜单' ? await page.locator('.notebook-drawer').evaluate(dialog => {
    const luminance = (color: string) => {
      const rgb = color.match(/[\d.]+/g)!.slice(0, 3).map(Number).map(value => value / 255)
      const linear = rgb.map(value => value <= .04045 ? value / 12.92 : ((value + .055) / 1.055) ** 2.4)
      return .2126 * linear[0] + .7152 * linear[1] + .0722 * linear[2]
    }
    return Array.from(dialog.querySelectorAll('h2, a, summary')).filter(element => element.getBoundingClientRect().height > 0).map(element => {
      let background: Element | null = element
      while (background && getComputedStyle(background).backgroundColor === 'rgba(0, 0, 0, 0)') background = background.parentElement
      const fg = getComputedStyle(element).color
      const bg = getComputedStyle(background!).backgroundColor
      const light = luminance(fg), dark = luminance(bg)
      return { text: element.textContent, foreground: fg, background: bg, ratio: (Math.max(light, dark) + .05) / (Math.min(light, dark) + .05) }
    })
  }) : []
  for (const color of dialogContrast) expect(color.ratio, `菜单文字对比度：${color.text}`).toBeGreaterThanOrEqual(4.5)
  if (label === '菜单') await page.screenshot({ path: test.info().outputPath('drawer-390.png') })
  await test.info().attach(`axe ${label}`, {
    body: JSON.stringify({
      label, tags: ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'],
      native_controls: controls,
      dialog_contrast: dialogContrast,
      violations: result.violations,
      contrast_violations: contrast.violations,
      contrast_incomplete: contrast.incomplete,
    }),
    contentType: 'application/json',
  })
  expect(result.violations.map(v => ({ id: v.id, nodes: v.nodes.map(n => ({ target: n.target, reason: n.failureSummary })) })), label).toEqual([])
  expect(contrast.violations, `${label}: color-contrast`).toEqual([])
}

async function tabTo(page: Page, target: Locator) {
  await page.bringToFront()
  await expect(target).toBeVisible()
  // Move backwards to earlier controls; headless Firefox does not wrap Tab
  // from the final page control through its browser chrome back to the page.
  const path: unknown[] = []
  for (let index = 0; index < 100; index++) {
    const state = await target.evaluate(element => {
      const current = document.activeElement
      return {
        focused: element === current,
        backwards: !!current && !!(element.compareDocumentPosition(current) & Node.DOCUMENT_POSITION_FOLLOWING),
      }
    })
    if (state.focused) return
    await page.keyboard.press(state.backwards ? 'Shift+Tab' : 'Tab')
    if (index < 25) path.push(await page.evaluate(() => ({
      tag: document.activeElement?.tagName,
      label: document.activeElement?.getAttribute('aria-label') ?? document.activeElement?.textContent?.slice(0, 80),
      focused: document.hasFocus(),
    })))
  }
  await test.info().attach('keyboard focus path', { body: JSON.stringify(path), contentType: 'application/json' })
  throw new Error(`键盘无法到达 ${await target.getAttribute('aria-label') ?? await target.textContent()}`)
}

async function studySpace(page: Page) {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(), expected_version: 0,
    name: '很长的学习区标题与内容边界' + 'LongTitle'.repeat(10), description: '', status: 'active', direction: '理解偶数与整除',
  })
  await seedSources(page, space.id)
  await page.goto(`/app/spaces/${space.id}`)
  await expect(page.locator('.chat-source-summary')).toContainText('偶数')
  return space
}

test('C5a 五宽度双主题、来源和 Studio、短视口与减少动态效果', async ({ page }, testInfo) => {
  test.setTimeout(240000)
  const space = await studySpace(page)
  await page.emulateMedia({ reducedMotion: 'reduce' })
  for (const theme of ['light', 'dark'] as const) {
    await setNotebookTheme(page, theme)
    await page.goto('/app/')
    await expect(page.getByRole('heading', { name: '你的学习区', exact: true })).toBeVisible()
    await audit(page, `首页 ${theme}`)
    await page.goto(`/app/spaces/${space.id}`)
    for (const width of [390, 768, 1024, 1280, 1440]) {
      await page.setViewportSize({ width, height: 844 })
      if (width <= 900) await page.getByRole('tab', { name: '对话', exact: true }).click()
      const input = page.getByLabel('向导师提问', { exact: true })
      await input.fill('切面板保留的草稿')
      const send = page.getByRole('button', { name: '发送提问', exact: true })
      const box = await send.boundingBox()
      const bottom = width <= 900 ? (await page.getByRole('tablist').boundingBox())!.y : 844
      expect(box!.y + box!.height, `${theme}/${width}: 发送按钮未被底栏遮挡`).toBeLessThanOrEqual(bottom)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await audit(page, `学习区 ${theme}/${width}`)
      await page.screenshot({ path: testInfo.outputPath(`chat-${theme}-${width}.png`) })
      if (width <= 900) {
        await page.getByRole('tab', { name: '来源', exact: true }).click()
        await audit(page, `来源 ${theme}/${width}`)
        await page.getByRole('tab', { name: 'Studio', exact: true }).click()
        await audit(page, `Studio ${theme}/${width}`)
        await page.getByRole('tab', { name: '对话', exact: true }).click()
        await expect(input).toHaveValue('切面板保留的草稿')
      }
    }
  }
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  const input = page.getByLabel('向导师提问', { exact: true })
  expect(await input.evaluate(element => Number.parseFloat(getComputedStyle(element).fontSize))).toBeGreaterThanOrEqual(16)
  // A reduced visual viewport exercises the keyboard resize listener without claiming a real IME.
  for (const width of [390, 768, 1024]) {
    await page.setViewportSize({ width, height: 844 })
    if (width <= 900) await page.getByRole('tab', { name: '对话', exact: true }).click()
    await page.evaluate(() => {
      Object.defineProperty(window.visualViewport!, 'height', { configurable: true, value: 440 })
      window.visualViewport!.dispatchEvent(new Event('resize'))
    })
    await expect.poll(async () => {
      const box = await page.getByRole('button', { name: '发送提问' }).boundingBox()
      return box!.y + box!.height
    }).toBeLessThanOrEqual(440)
    const inputBox = await input.boundingBox()
    expect(inputBox!.y + inputBox!.height).toBeLessThanOrEqual(440)
    await page.screenshot({ path: testInfo.outputPath(`visual-viewport-${width}.png`) })
    await page.evaluate(() => {
      delete (window.visualViewport as unknown as { height?: number }).height
      window.visualViewport!.dispatchEvent(new Event('resize'))
    })
  }
  await page.setViewportSize({ width: 390, height: 440 })
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  await expect.poll(async () => {
    const box = await page.getByRole('button', { name: '发送提问' }).boundingBox()
    return box!.y + box!.height
  }).toBeLessThanOrEqual(440)
  expect(await page.locator('.chat-stream').evaluate(element => getComputedStyle(element).scrollBehavior)).toBe('auto')
})

test('C5a 键盘菜单、来源、对话引用返回、练习反馈与 Studio 全流程', async ({ page }, testInfo) => {
  test.setTimeout(180000)
  const space = await studySpace(page)
  await page.setViewportSize({ width: 390, height: 844 })
  const menu = page.getByRole('button', { name: '打开侧边菜单', exact: true })
  await tabTo(page, menu)
  await page.keyboard.press('Enter')
  const drawer = page.getByRole('dialog', { name: '侧边菜单', exact: true })
  await audit(page, '菜单')
  for (const name of ['全部学习区', '进度与复习', '评估与证据', '导师历史', '任务中心', '知识与参考', 'NoteSync', 'Studio 内容库', '长期记忆', '设置与能力', '数据与隐私', '设备管理']) {
    await tabTo(page, drawer.getByRole('link', { name, exact: true }))
  }
  await page.keyboard.press('Escape')
  await expect(menu).toBeFocused()
  const chatTab = page.getByRole('tab', { name: '对话', exact: true })
  await tabTo(page, chatTab)
  await page.keyboard.press('ArrowLeft')
  await expect(page.getByRole('tab', { name: '来源', exact: true })).toBeFocused()
  const checkbox = page.getByRole('checkbox', { name: /选择来源：even.md/ })
  await tabTo(page, checkbox)
  await page.keyboard.press('Space')
  await expect(checkbox).not.toBeChecked()
  await page.keyboard.press('Space')
  const source = page.getByRole('button', { name: '打开来源：even.md', exact: true })
  await tabTo(page, source)
  await page.keyboard.press('Enter')
  const viewer = page.getByRole('region', { name: '来源查看器', exact: true })
  await expect(viewer).toContainText('偶数')
  await audit(page, '来源查看器')
  await tabTo(page, viewer.getByRole('button', { name: '关闭来源', exact: true }))
  await page.keyboard.press('Enter')
  await expect(source).toBeFocused()
  await tabTo(page, page.getByRole('tab', { name: '来源', exact: true }))
  await page.keyboard.press('ArrowRight')
  const input = page.getByLabel('向导师提问', { exact: true })
  await tabTo(page, input)
  const outline = await input.evaluate(element => getComputedStyle(element).outlineStyle)
  expect(outline).not.toBe('none')
  await page.keyboard.type('解释偶数')
  await page.keyboard.press('Shift+Enter')
  await page.keyboard.type('再给一个例子')
  await input.dispatchEvent('keydown', { key: 'Enter', code: 'Enter', isComposing: true })
  expect(new URL(page.url()).searchParams.has('conversation')).toBe(false)
  await page.keyboard.press('Enter')
  const citation = page.getByRole('button', { name: '核验引用 1', exact: true }).first()
  await expect(citation).toBeVisible()
  await expect(page.locator('.chat-panel [aria-live="polite"]')).toContainText('学习导师')
  await tabTo(page, citation)
  await page.keyboard.press('Enter')
  await expect(viewer).toContainText('偶数')
  const close = viewer.getByRole('button', { name: '关闭引用片段', exact: true })
  await expect(viewer.getByRole('region', { name: '引用 1 核验片段', exact: true })).toBeFocused()
  await tabTo(page, close)
  await page.keyboard.press('Enter')
  await expect(chatTab).toHaveAttribute('aria-selected', 'true')
  await expect(citation).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(viewer.getByRole('region', { name: '引用 1 核验片段', exact: true })).toBeFocused()
  const otherSource = page.getByRole('button', { name: '打开来源：examples.md', exact: true })
  await tabTo(page, otherSource)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('tab', { name: '来源', exact: true })).toHaveAttribute('aria-selected', 'true')
  await expect(viewer.getByRole('heading', { name: 'examples.md', exact: true })).toBeVisible()
  await tabTo(page, viewer.getByRole('button', { name: '关闭来源', exact: true }))
  await page.keyboard.press('Enter')
  await expect(otherSource).toBeFocused()
  await tabTo(page, page.getByRole('tab', { name: '来源', exact: true }))
  await page.keyboard.press('ArrowRight')
  const exerciseButton = page.getByRole('button', { name: '出一道题', exact: true })
  await tabTo(page, exerciseButton)
  await page.keyboard.press('Enter')
  const card = page.getByRole('article', { name: '对话练习', exact: true })
  const answer = card.getByLabel('练习答案')
  await expect(answer).toBeVisible()
  await tabTo(page, card.getByRole('button', { name: '稍后再做', exact: true }))
  await page.keyboard.press('Enter')
  await tabTo(page, card.getByRole('button', { name: '继续这道题', exact: true }))
  await page.keyboard.press('Enter')
  await tabTo(page, answer)
  await page.keyboard.type('偶数能被 2 整除，例如 4。')
  await page.keyboard.press('Enter')
  await expect(card).not.toContainText('已提交')
  await tabTo(page, card.getByLabel('我得到的帮助'))
  await page.keyboard.press('Home')
  await page.keyboard.press('ArrowDown')
  await page.keyboard.press('Tab')
  await tabTo(page, card.getByRole('button', { name: '提交答案', exact: true }))
  await page.keyboard.press('Enter')
  const feedback = page.getByRole('article', { name: '练习反馈', exact: true })
  await expect(feedback).toContainText('你找到了关键点')
  await expect(page.locator('.exercise-feedback-status')).toContainText('你找到了关键点')
  const basis = feedback.getByRole('button', { name: '查看依据 1', exact: true })
  await tabTo(page, basis)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('dialog', { name: '反馈依据' })).toContainText('偶数')
  await page.keyboard.press('Escape')
  await expect(basis).toBeFocused()
  await tabTo(page, feedback.getByRole('button', { name: '继续学习', exact: true }))
  await page.keyboard.press('Enter')
  await tabTo(page, chatTab)
  await page.keyboard.press('ArrowRight')
  const newNote = page.getByRole('button', { name: '＋ 添加笔记', exact: true })
  await tabTo(page, newNote)
  await page.keyboard.press('Enter')
  await tabTo(page, page.getByLabel('笔记标题', { exact: true }))
  await page.keyboard.type('键盘笔记')
  await tabTo(page, page.getByLabel('笔记正文', { exact: true }))
  await page.keyboard.type('已审阅的个人笔记')
  await audit(page, 'Studio 笔记编辑')
  await tabTo(page, page.getByRole('button', { name: '保存笔记', exact: true }))
  await page.keyboard.press('Enter')
  await expect(page.getByText(/已保存第 1 版/).first()).toBeVisible()
  await expect(page.getByRole('textbox', { name: '笔记正文', exact: true })).toHaveValue('已审阅的个人笔记')
  await page.goto(`/app/progress?space=${space.id}`)
  await expect(page.getByRole('link', { name: '理解偶数与整除', exact: true })).toBeVisible()
  for (const theme of ['light', 'dark'] as const) {
    await setNotebookTheme(page, theme)
    await audit(page, `进度 ${theme}`)
    await page.screenshot({ path: testInfo.outputPath(`progress-${theme}-390.png`) })
  }
})

test('C5a 练习与反馈卡五宽度双主题', async ({ page }, testInfo) => {
  test.setTimeout(120000)
  await studySpace(page)
  await page.getByRole('button', { name: '出一道题', exact: true }).click()
  const card = page.getByRole('article', { name: '对话练习', exact: true })
  await expect(card.getByLabel('练习答案')).toBeVisible()
  for (const theme of ['light', 'dark'] as const) {
    await setNotebookTheme(page, theme)
    for (const width of [390, 768, 1024, 1280, 1440]) {
      await page.setViewportSize({ width, height: 844 })
      if (width <= 900) await page.getByRole('tab', { name: '对话', exact: true }).click()
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await audit(page, `练习卡 ${theme}/${width}`)
      await page.screenshot({ path: testInfo.outputPath(`practice-${theme}-${width}.png`) })
    }
  }
  await card.getByLabel('练习答案').fill('偶数能被 2 整除，例如 4。')
  await card.getByLabel('我得到的帮助').selectOption('none')
  await card.getByRole('button', { name: '提交答案', exact: true }).click()
  const feedback = page.getByRole('article', { name: '练习反馈', exact: true })
  await expect(feedback).toContainText('你找到了关键点')
  for (const theme of ['light', 'dark'] as const) {
    await setNotebookTheme(page, theme)
    for (const width of [390, 768, 1024, 1280, 1440]) {
      await page.setViewportSize({ width, height: 844 })
      if (width <= 900) await page.getByRole('tab', { name: '对话', exact: true }).click()
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await audit(page, `反馈卡 ${theme}/${width}`)
      await page.screenshot({ path: testInfo.outputPath(`feedback-${theme}-${width}.png`) })
    }
  }
})

test('C5a 长标题、公式与代码留在窄屏消息内', async ({ page }, testInfo) => {
  await configureNotebookModel(page)
  await pairNotebook(page, 'learner')
  const space = await webAPI(page, 'POST', '/v1/learning-spaces', {
    operation_id: randomUUID(), expected_version: 0, name: '内容边界', description: '', status: 'active', direction: '理解代数表达式',
  })
  const formula = Array(24).fill('x_{123}').join('+')
  await seedSources(page, space.id, '长标题'.repeat(40), `# 公式与代码\n\n$${formula}$\n\n$$${formula}$$\n\n\`\`\`text\n${'LongUnbrokenCode'.repeat(40)}\n\`\`\`\n`)
  await page.goto(`/app/spaces/${space.id}`)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('tab', { name: '来源', exact: true }).click()
  await page.getByRole('checkbox', { name: '选择全部来源', exact: true }).uncheck()
  await page.getByRole('checkbox', { name: /选择来源：even.md/ }).check()
  await page.getByRole('tab', { name: '对话', exact: true }).click()
  const input = page.getByLabel('向导师提问', { exact: true })
  await input.fill('解释这些表达式')
  await page.getByRole('button', { name: '发送提问', exact: true }).click()
  const answer = page.locator('.chat-answer')
  await expect(answer.getByLabel('数学公式')).toHaveCount(2)
  for (const theme of ['light', 'dark'] as const) {
    await setNotebookTheme(page, theme)
    for (const width of [390, 768, 1024, 1280, 1440]) {
      await page.setViewportSize({ width, height: 844 })
      if (width <= 900) await page.getByRole('tab', { name: '对话', exact: true }).click()
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      expect(await page.locator('.chat-stream').evaluate(element => element.scrollWidth <= element.clientWidth + 1)).toBe(true)
      await audit(page, `公式代码 ${theme}/${width}`)
      await page.screenshot({ path: testInfo.outputPath(`math-code-${theme}-${width}.png`) })
    }
  }
})
