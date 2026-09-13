import { cp, mkdir, readFile, rm } from 'node:fs/promises'
const source = new URL('../dist/', import.meta.url)
const target = new URL('../../../server/internal/webassets/dist/', import.meta.url)
const html = await readFile(new URL('index.html', source), 'utf8')
if (!html.includes('type="module"') || !html.includes('/app/assets/'))
  throw new Error('缺少真实前端资产')
await mkdir(target, { recursive: true })
await rm(new URL('assets/', target), { recursive: true, force: true })
await cp(source, target, { recursive: true })
