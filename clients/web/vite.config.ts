import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'
import { execFileSync } from 'node:child_process'

export default defineConfig({
  base: '/app/',
  plugins: [
    react(),
    tailwindcss(),
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
