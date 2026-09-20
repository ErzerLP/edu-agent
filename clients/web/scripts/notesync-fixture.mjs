import { createServer } from 'node:http'

export const notesyncFixtureToken = 'browser-notesync-fixture-only-token'

// 复用固定 3.6.1 REST 包装、版本、权限探测和笔记字段；不代表真实上游验收。
export async function startNotesyncFixture() {
  const notes = new Map()
  let writes = 0
  let available = true
  const server = createServer(async (request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1:32931')
    const chunks = []
    for await (const chunk of request) chunks.push(chunk)
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : {}
    const send = (data, code = 1) => { response.setHeader('Content-Type', 'application/json'); response.end(JSON.stringify({ status: code === 1, code, message: '', data })) }
    if (url.pathname === '/fixture') {
      if (request.method === 'POST') {
        if (typeof body.available === 'boolean') available = body.available
        if (body.path && typeof body.content === 'string') {
          const previous = notes.get(body.path)
          notes.set(body.path, note(body.path, body.content, (previous?.version ?? 0) + 1))
        }
      }
      return send({ writes, notes: [...notes.values()] })
    }
    if (!available) { response.writeHead(503); response.end(); return }
    if (request.headers.authorization !== `Bearer ${notesyncFixtureToken}` || request.headers['x-client'] !== 'CLI') return send(null, 315)
    if (url.pathname === '/api/version') return send({ version: '3.6.1', gitTag: 'v3.6.1', buildTime: 'fixed', versionIsNew: false, versionNewName: '', versionNewLink: '', versionNewChangelog: '', versionNewChangelogContent: '', versionHistory: [], pluginVersionNewName: '', pluginVersionNewLink: '', pluginVersionNewChangelog: '', pluginVersionNewChangelogContent: '', pluginVersionHistory: [] })
    if (url.pathname === '/api/health') return send({ status: 'healthy', version: '3.6.1', uptime: 1, database: 'connected' })
    if (url.pathname === '/api/vault') return send([{ id: 1, vault: 'Knowledge', noteCount: notes.size, noteSize: 0, fileCount: 0, fileSize: 0, size: 0, createdAt: 'now', updatedAt: 'now' }])
    if (url.pathname === '/api/notes') {
      const page = Number(url.searchParams.get('page')), pageSize = Number(url.searchParams.get('pageSize'))
      return send({ list: [...notes.values()].slice((page - 1) * pageSize, page * pageSize), pager: { page, pageSize, totalRows: notes.size } })
    }
    if (url.pathname === '/api/note' && request.method === 'GET') return notes.has(url.searchParams.get('path')) ? send(notes.get(url.searchParams.get('path'))) : send(null, 430)
    if (url.pathname === '/api/note' && request.method === 'POST') {
      if (body.vault !== 'Knowledge') return send(null, 315)
      if (body.path.startsWith('../')) return send(null, 444)
      if (body.createOnly && notes.has(body.path)) return send(null, 431)
      writes++
      const value = note(body.path, body.content, (notes.get(body.path)?.version ?? 0) + 1)
      notes.set(body.path, value)
      return send(value)
    }
    response.writeHead(404); response.end()
  })
  await new Promise(resolve => server.listen(32931, '127.0.0.1', resolve))
  return server
}
function note(path, content, version) {
  return { id: 1, path, content, pathHash: 'ph', contentHash: 'ch', version, ctime: 1, mtime: version,
    size: Buffer.byteLength(content), clientName: 'edu-agent-notesync', clientType: 'CLI', clientVersion: '1',
    lastTime: version, updatedAt: '2026-09-18T00:00:00Z', createdAt: '2026-09-18T00:00:00Z', fileLinks: {} }
}
