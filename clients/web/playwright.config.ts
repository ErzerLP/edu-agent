import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests/browser',
  workers: 1,
  retries: 0,
  timeout: 60_000,
  use: {
    baseURL: 'http://127.0.0.1:32929',
    headless: true,
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium', launchOptions: { executablePath: process.env.WEB_CHROMIUM_PATH } },
    },
    ...(process.env.WEB_RELEASE_MATRIX === '1'
      ? [
          { name: 'firefox', use: { browserName: 'firefox' as const } },
          { name: 'webkit', use: { browserName: 'webkit' as const } },
        ]
      : []),
  ],
  webServer: {
    command: 'node scripts/test-server.mjs',
    url: 'http://127.0.0.1:32929/livez',
    timeout: 60_000,
    reuseExistingServer: false,
    stderr: 'ignore',
  },
})
