import { test, expect, type Locator, type Page } from '@playwright/test'
import { mkdir, writeFile } from 'node:fs/promises'
import { createSpace, openNavigation, pairNotebook, seedSources } from './notebook-fixture'

async function expectCentered(page: Page, dialog: Locator) {
  const box = await dialog.boundingBox()
  const viewport = page.viewportSize()!
  expect(box).not.toBeNull()
  expect(Math.abs(box!.x + box!.width / 2 - viewport.width / 2)).toBeLessThanOrEqual(1)
  expect(Math.abs(box!.y + box!.height / 2 - viewport.height / 2)).toBeLessThanOrEqual(1)
}

// Observe every animation frame from the first visible frame, including a slow collection read.
function measureOpening(page: Page) {
  return page.evaluate(
    () =>
      new Promise<number[][]>((resolve) => {
        const samples: number[][] = []
        let start: number | undefined
        const frame = (time: number) => {
          const dialog = document.querySelector<HTMLDialogElement>('dialog[aria-label="添加来源"]')
          if (dialog?.open) {
            start ??= time
            const rect = dialog.getBoundingClientRect()
            const select = dialog.querySelector('select')?.getBoundingClientRect()
            const close = dialog.querySelector('button')?.getBoundingClientRect()
            samples.push([
              time - start,
              rect.x,
              rect.y,
              rect.width,
              rect.height,
              select?.x ?? 0,
              select?.y ?? 0,
              select?.width ?? 0,
              close?.x ?? 0,
              close?.y ?? 0,
            ])
            if (time - start >= 2000) return resolve(samples)
          }
          requestAnimationFrame(frame)
        }
        requestAnimationFrame(frame)
      }),
  )
}

for (const width of [1440, 390]) {
  test(`学习方向和来源弹窗居中，打开后两秒保持稳定（${width}px）`, async ({ page }) => {
    await page.setViewportSize({ width, height: width === 1440 ? 900 : 844 })
    await pairNotebook(page, 'learner')
    const space = await createSpace(page, '弹窗布局回归')
    const { collectionId } = await seedSources(page, space.id)
    await page.goto(`/app/spaces/${space.id}?panel=sources`)
    if (width === 390) await page.getByRole('tab', { name: '对话', exact: true }).click()
    await page.getByRole('button', { name: '确认学习方向', exact: true }).click()
    const direction = page.getByRole('dialog', { name: '确认学习方向', exact: true })
    const directionBox = await direction.boundingBox()
    const evidence = process.env.WEB_VISUAL_DIR
    if (evidence) {
      await mkdir(evidence, { recursive: true })
      await page.screenshot({ path: `${evidence}/direction-dialog-${width}.png` })
    }
    await direction.getByRole('button', { name: '取消', exact: true }).click()
    if (width === 390) await page.getByRole('tab', { name: '来源', exact: true }).click()
    await page.route('**/v1/knowledge/collections', async (route) => {
      const response = await route.fetch()
      await new Promise((resolve) => setTimeout(resolve, 700))
      await route.fulfill({ response })
    })
    const source = page.getByRole('dialog', { name: '添加来源', exact: true })
    const measurements: number[][][] = []
    for (const restored of [false, true]) {
      const measurement = measureOpening(page)
      await page.getByRole('button', { name: '＋ 添加来源', exact: true }).click()
      await expect(source).toBeVisible()
      measurements.push(await measurement)
      if (evidence) {
        await page.screenshot({
          path: `${evidence}/source-dialog-${width}${restored ? '-restored' : ''}.png`,
        })
        await writeFile(
          `${evidence}/dialog-frames-${width}.json`,
          JSON.stringify({ directionBox, measurements }),
        )
      }
      if (!restored) await source.getByLabel('保存到已关联集合').selectOption(collectionId)
      await source.getByRole('button', { name: '关闭添加来源', exact: true }).click()
    }
    if (width === 390) await page.getByRole('tab', { name: '对话', exact: true }).click()
    await page.getByRole('button', { name: '确认学习方向', exact: true }).click()
    await expectCentered(page, direction)
    await page.keyboard.press('Escape')
    if (width === 390) await page.getByRole('tab', { name: '来源', exact: true }).click()
    await page.getByRole('button', { name: '＋ 添加来源', exact: true }).click()
    await expectCentered(page, source)
    await page.keyboard.press('Escape')
    for (const samples of measurements) {
      for (let column = 1; column < samples[0].length; column++) {
        const values = samples.map((sample) => sample[column])
        expect(
          Math.max(...values) - Math.min(...values),
          `layout column ${column}`,
        ).toBeLessThanOrEqual(1)
      }
    }
    await openNavigation(page)
    expect(
      (await page.getByRole('dialog', { name: '侧边菜单', exact: true }).boundingBox())!.x,
    ).toBe(0)
  })
}
