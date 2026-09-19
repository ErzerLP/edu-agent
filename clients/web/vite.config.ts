import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'
import { execFileSync } from 'node:child_process'
// @ts-expect-error 构建插件使用原生 ESM，由 Vite 执行。
import { offlineWorker } from './scripts/offline-worker.mjs'

export default defineConfig({
  base: '/app/',
  plugins: [
    react(),
    tailwindcss(),
    offlineWorker(),
    {
      name: 'copy-go-assets',
      closeBundle() {
        execFileSync(process.execPath, ['scripts/copy-assets.mjs'], { stdio: 'inherit' })
      },
    },
  ],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  build: { sourcemap: false },
})
