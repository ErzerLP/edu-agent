import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests/browser',
  workers: 1,
  retries: 0,
  timeout: 60_000,
  use: {
    baseURL: 'http://127.0.0.1:32929',
    headless: true,
    launchOptions: { executablePath: process.env.WEB_CHROMIUM_PATH },
  },
  webServer: {
    command: 'node scripts/test-server.mjs',
    url: 'http://127.0.0.1:32929/livez',
    timeout: 60_000,
    reuseExistingServer: false,
    stderr: 'ignore',
  },
})
