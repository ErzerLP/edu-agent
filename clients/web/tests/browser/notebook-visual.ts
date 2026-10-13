import { mkdir } from 'node:fs/promises'
import type { Page } from '@playwright/test'

// Optional evidence capture; assertions stay in the business scenarios.
export async function captureNotebook(
  page: Page,
  state: string,
  panel?: '来源' | '对话' | 'Studio',
) {
  const directory = process.env.WEB_VISUAL_DIR
  if (!directory) return
  await mkdir(directory, { recursive: true })
  const original = page.viewportSize()
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: width === 1440 ? 900 : 844 })
    if (width === 390 && panel) await page.getByRole('tab', { name: panel, exact: true }).click()
    await page.screenshot({ path: `${directory}/current-${state}-${width}.png` })
  }
  if (original) await page.setViewportSize(original)
}
