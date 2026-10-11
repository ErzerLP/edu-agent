import { createServer } from 'node:http'
import { randomUUID } from 'node:crypto'

export const nocturneFixtureURL = 'http://127.0.0.1:32932'
// 只用于本地夹具的公开 canary；错误正文故意包含它们，用于验证服务端脱敏。
export const nocturneFixtureSecrets = [
  'browser-nocturne-fixture-only-api-token',
  Buffer.alloc(32, 0x34).toString('base64url'),
  Buffer.alloc(32, 0x35).toString('base64url'),
]

// 协议来自 nocturne/client.go 与 bridge_test.go 的 strictHTTPFake。
// 只替换远端 HTTP；准入、交付、对账、删除和回执仍由真实 Go/PG 服务执行。
export async function startNocturneFixture() {
  const paths = new Map()
  const versions = new Map()
  let nextID = 1
  let available = true
  let failCapabilities = 0
  let failures = 0
  let writes = 0
  let boot = 1
  const server = createServer(async (request, response) => {
    const url = new URL(request.url, nocturneFixtureURL)
    const chunks = []
    for await (const chunk of request) chunks.push(chunk)
    let body
    try { body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : {} }
    catch { response.writeHead(400); response.end(); return }
    const send = (value, status = 200) => {
      response.writeHead(status, { 'Content-Type': 'application/json' })
      response.end(JSON.stringify(value))
    }
    if (url.pathname === '/fixture') {
      if (request.method === 'POST') {
        if (typeof body.available === 'boolean') {
          if (!available && body.available) boot++
          available = body.available
        }
        if (body.failCapabilities === 1) failCapabilities = 1
      }
      return send({ available, failures, writes, paths: [...paths.values()], versions: [...versions.values()] })
    }
    if (['/sse', '/messages', '/mcp', '/api/settings', '/api/browse/domains'].includes(url.pathname)) return send({}, 404)
    const maintenance = url.pathname.startsWith('/internal/')
    const token = nocturneFixtureSecrets[maintenance ? 1 : 0]
    if (request.headers.authorization !== `Bearer ${token}`) return send({}, 401)
    if (request.headers['x-namespace'] !== 'edu-agent') return send({}, 422)
    if (url.pathname === '/health') return send({ status: 'ok', database: 'connected' })
    if (url.pathname === '/internal/edu-agent/capabilities') {
      if (failCapabilities) {
        failCapabilities--
        failures++
        // 临时维护凭据故障发生在写入发送前；原 outbox 进入 dead，允许正式重放。
        return send({ detail: nocturneFixtureSecrets.join(' ') }, 401)
      }
      return send({ upstream_commit: '54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254', compat_revision: 'edu-agent-maintenance-v1', boot_epoch: `browser-boot-${boot}` })
    }
    if (url.pathname === '/internal/edu-agent/backups') return send({ validated: true, manifest_sha256: 'a'.repeat(64), artifacts: [] })
    if (url.pathname === '/internal/edu-agent/backups/prune') return send({ operation_id: body.operation_id, deleted_paths: [], manifest_sha256: 'a'.repeat(64) })
    if (!available) return send({ detail: nocturneFixtureSecrets.join(' ') }, 503)
    const nodePath = url.searchParams.get('path')
    const uri = path => `core://${path}`
    if (url.pathname === '/api/browse/node') {
      if (request.method === 'POST') {
        if (body.domain !== 'core' || !['', 'edu-agent'].includes(body.parent_path)) return send({}, 422)
        const path = body.parent_path ? `${body.parent_path}/${body.title}` : body.title
        if (paths.has(path)) return send({}, 409)
        const node = { path, node_uuid: randomUUID(), memory_id: nextID++, content: body.content, priority: body.priority, disclosure: body.disclosure }
        paths.set(path, node)
        versions.set(node.memory_id, version(node))
        if (path !== 'edu-agent') writes++
        return send({ success: true, uri: uri(path), memory_id: node.memory_id })
      }
      if (url.searchParams.get('domain') !== 'core') return send({}, 422)
      const node = paths.get(nodePath)
      if (!node) return send({}, 404)
      if (request.method === 'GET') return send({ node: {
        path: node.path, domain: 'core', uri: uri(node.path), name: node.path.split('/').at(-1), content: node.content,
        priority: node.priority, disclosure: node.disclosure, created_at: '2026-10-11T00:00:00Z', is_virtual: false,
        aliases: [], node_uuid: node.node_uuid, glossary_keywords: [], glossary_matches: [],
      }, children: [], breadcrumbs: [{ path: '', label: 'root' }] })
      if (request.method === 'PUT') {
        if (Object.keys(body).some(key => !['content', 'priority', 'disclosure'].includes(key))) return send({}, 422)
        const previous = versions.get(node.memory_id)
        node.memory_id = nextID++
        previous.deprecated = true
        previous.migrated_to = node.memory_id
        Object.assign(node, body)
        versions.set(node.memory_id, version(node))
        writes++
        return send({ success: true, memory_id: node.memory_id })
      }
      if (request.method === 'DELETE') {
        versions.get(node.memory_id).deprecated = true
        paths.delete(nodePath)
        return send({ success: true, uri: uri(nodePath) })
      }
    }
    const references = url.pathname.match(/^\/internal\/edu-agent\/nodes\/([^/]+)\/references$/)
    if (references) {
      const nodeID = references[1]
      const nodes = [...paths.values()].filter(node => node.node_uuid === nodeID)
      const ids = [...versions.values()].filter(value => value.node_uuid === nodeID).map(value => value.id)
      return send({ node_uuid: nodeID, complete: true, active_memory_id: nodes[0]?.memory_id ?? 0, memory_ids: ids,
        paths: nodes.map(node => ({ namespace: 'edu-agent', domain: 'core', path: node.path, uri: uri(node.path), alias: false })),
        edge_ids: [], glossary_keywords: [], search_document_ids: [], access_log_ids: [], boot_uris: [], review_references: [] })
    }
    if (url.pathname === '/api/browse/search') {
      const q = url.searchParams.get('q')
      const results = [...paths.values()].filter(node => node.path.includes(q)).map(node => ({ domain: 'core', path: node.path, uri: uri(node.path), name: node.path.split('/').at(-1), snippet: node.content, priority: node.priority, disclosure: node.disclosure }))
      return send({ query: q, results, count: results.length })
    }
    const orphan = url.pathname.match(/^\/api\/maintenance\/orphans\/(\d+)$/)
    if (orphan) {
      const id = Number(orphan[1]), value = versions.get(id)
      if (!value) return send({}, 404)
      if (request.method === 'GET') return send(value)
      if (request.method === 'DELETE') {
        if ([...paths.values()].some(node => node.memory_id === id)) return send({}, 409)
        versions.delete(id)
        return send({ deleted_memory_id: id, chain_repaired_to: value.migrated_to, rows_before: {}, rows_after: {} })
      }
    }
    if (url.pathname === '/api/maintenance/orphans') return send([...versions.values()].filter(value => value.deprecated))
    return send({}, 404)
  })
  await new Promise(resolve => server.listen(32932, '127.0.0.1', resolve))
  return server
}

function version(node) {
  return { id: node.memory_id, node_uuid: node.node_uuid, content: node.content, created_at: '2026-10-11T00:00:00Z', deprecated: false, migrated_to: 0, category: 'deprecated', migration_target: null }
}
