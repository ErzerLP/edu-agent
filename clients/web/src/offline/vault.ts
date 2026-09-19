import { base64, bytes, open, passwordKey, seal, unbase64, type Sealed } from './crypto'
import { verifyRoot, type Owner, type PrepareIntent, type SavedPack } from './protocol'

export const databaseName = 'edu-browser-offline-v1'
export const markerName = 'edu-browser-offline-presence-v1'
const lockName = 'edu-browser-offline-v1'
const keyLockName = 'edu-browser-offline-keys-v1'
const maxBytes = 32 * 1024 * 1024
export type QueueEntry = {
  operation: string
  state: 'queued' | 'unknown' | 'confirmed' | 'rejected' | 'blocked' | 'conflict'
  result?: string
}
export type VaultState = {
  version: 1
  owner: Owner
  trust: string
  packs: SavedPack[]
  pending?: PrepareIntent
  queue: QueueEntry[]
  serverTime: number
  clockFloor: number
}
type Header = { version: 1; id: string; origin: string; salt: string; wrapped: Sealed }
type Snapshot = { revision: number; sealed: Sealed }
const channel = () => new BroadcastChannel(lockName)
export function broadcast(type: 'changed' | 'lock' | 'purged') {
  const c = channel()
  c.postMessage(type)
  c.close()
}
export function observe(callback: (type: string) => void) {
  if (typeof BroadcastChannel === 'undefined') return () => {}
  const c = channel()
  c.onmessage = (e) => callback(String(e.data))
  return () => c.close()
}
export const exclusive = <T>(fn: () => Promise<T>) =>
  navigator.locks.request(lockName, { mode: 'exclusive' }, fn)

function request<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result)
    req.onerror = () => reject(req.error)
  })
}
const transactionDone = (tx: IDBTransaction) =>
  new Promise<void>((resolve, reject) => {
    tx.oncomplete = () => resolve()
    tx.onabort = () => reject(tx.error ?? new Error('保存事务未提交'))
    tx.onerror = () => {}
  })
async function connect(create = false): Promise<IDBDatabase> {
  if (!create && !(await indexedDB.databases()).some((db) => db.name === databaseName))
    throw new Error('离线库已丢失或被浏览器清理；未同步答案没有服务器备份')
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(databaseName, 1)
    req.onupgradeneeded = () => {
      if (!create) {
        req.transaction!.abort()
        return
      }
      req.result.createObjectStore('vault')
    }
    req.onerror = () => reject(req.error)
    req.onblocked = () => reject(new Error('其他标签页阻止离线库升级，请关闭后重试'))
    req.onsuccess = () => {
      req.result.onversionchange = () => req.result.close()
      resolve(req.result)
    }
  })
}
async function readDisk(): Promise<{ header: Header; snapshot: Snapshot }> {
  const db = await connect()
  try {
    const tx = db.transaction('vault', 'readonly')
    const done = transactionDone(tx)
    const store = tx.objectStore('vault')
    const [header, snapshot] = await Promise.all([
      request<Header>(store.get('header')),
      request<Snapshot>(store.get('snapshot')),
    ])
    await done
    if (
      !header ||
      !snapshot ||
      header.version !== 1 ||
      header.origin !== location.origin ||
      !Number.isSafeInteger(snapshot.revision) ||
      snapshot.revision < 1 ||
      snapshot.revision > 100_000 ||
      !header.wrapped ||
      !snapshot.sealed
    )
      throw new Error('离线库部分丢失、损坏或版本不兼容，禁止继续保存')
    if (header.id !== localStorage.getItem(markerName))
      throw new Error('离线库完整性标记丢失，不能确认保存状态')
    return { header, snapshot }
  } finally {
    db.close()
  }
}
async function writeDisk(header: Header, snapshot: Snapshot, create = false) {
  const db = await connect(create)
  try {
    const tx = db.transaction('vault', 'readwrite', { durability: 'strict' })
    if (tx.durability !== 'strict') {
      tx.abort()
      throw new Error('浏览器不支持可靠保存事务')
    }
    const done = transactionDone(tx)
    try {
      tx.objectStore('vault').put(header, 'header')
      tx.objectStore('vault').put(snapshot, 'snapshot')
    } catch (error) {
      tx.abort()
      await done.catch(() => {})
      throw error
    }
    await done
  } finally {
    db.close()
  }
  const readback = await readDisk()
  if (JSON.stringify(readback) !== JSON.stringify({ header, snapshot }))
    throw new Error('保存后的回读核对失败，不能标记已保存')
}
const binding = (header: Header) => `edu-offline-v1:${header.origin}:${header.id}`
export async function support(): Promise<void> {
  if (
    !isSecureContext ||
    !crypto.subtle ||
    !navigator.locks ||
    !navigator.storage?.persist ||
    !indexedDB.databases ||
    typeof BroadcastChannel === 'undefined' ||
    !navigator.serviceWorker
  )
    throw new Error('浏览器缺少加密、持久化、跨标签页锁或离线页面能力，不保存数据')
  await crypto.subtle.importKey('raw', new Uint8Array(32), 'Ed25519', false, ['verify'])
  const probe = `${markerName}:probe`
  localStorage.setItem(probe, '1')
  localStorage.removeItem(probe)
}
export async function presence(): Promise<'present' | 'absent' | 'lost'> {
  const databases = await indexedDB.databases()
  const hasDB = databases.some((db) => db.name === databaseName)
  const hasMarker = !!localStorage.getItem(markerName)
  return hasDB && hasMarker ? 'present' : hasDB || hasMarker ? 'lost' : 'absent'
}
export async function allowPersistence(): Promise<void> {
  await support()
  if (!(await navigator.storage.persist()))
    throw new Error('持久存储权限未获批准（隐私模式或浏览器策略可能拒绝）；本次不保存')
  const estimate = await navigator.storage.estimate()
  if (estimate.quota === undefined || estimate.quota - (estimate.usage ?? 0) < maxBytes * 2)
    throw new Error('可用浏览器配额不足，无法创建离线库')
}
export class Vault {
  private releaseKey?: () => void
  private constructor(
    private header: Header,
    private key: CryptoKey | undefined,
  ) {}
  private async holdKey(): Promise<void> {
    await new Promise<void>((resolve, reject) => {
      void navigator.locks
        .request(keyLockName, { mode: 'shared', signal: AbortSignal.timeout(5000) }, async () => {
          await new Promise<void>((release) => {
            this.releaseKey = release
            resolve()
          })
        })
        .catch(reject)
    })
    try {
      await this.read()
    } catch (error) {
      this.lock()
      throw error
    }
  }
  static async create(password: string, owner: Owner): Promise<Vault> {
    if ([...password].length < 12) throw new Error('解锁口令至少 12 个字符，请使用独立长口令')
    await verifyRoot(owner)
    const vault = await exclusive(async () => {
      if ((await presence()) !== 'absent') throw new Error('已有离线库或丢失标记，请先核对并清除')
      const material = crypto.getRandomValues(new Uint8Array(32))
      const salt = base64(crypto.getRandomValues(new Uint8Array(16)))
      const header: Header = {
        version: 1,
        id: crypto.randomUUID(),
        origin: location.origin,
        salt,
        wrapped: { iv: '', ciphertext: '' },
      }
      try {
        header.wrapped = await seal(
          await passwordKey(password, salt),
          base64(material),
          binding(header),
        )
        const key = await crypto.subtle.importKey('raw', material, 'AES-GCM', false, [
          'encrypt',
          'decrypt',
        ])
        const vault = new Vault(header, key)
        const state: VaultState = {
          version: 1,
          owner,
          trust: owner.root,
          packs: [],
          queue: [],
          serverTime: 0,
          clockFloor: Date.now(),
        }
        // 先写不含正文的存在标记；崩溃后标记与库不一致会明确报告，不假装新库。
        localStorage.setItem(markerName, header.id)
        await writeDisk(
          header,
          { revision: 1, sealed: await seal(key, JSON.stringify(state), `${binding(header)}:1`) },
          true,
        )
        return vault
      } finally {
        material.fill(0)
      }
    })
    await vault.holdKey()
    return vault
  }
  static async unlock(password: string): Promise<Vault> {
    const vault = await exclusive(async () => {
      const { header, snapshot } = await readDisk()
      try {
        const raw = unbase64(
          await open(await passwordKey(password, header.salt), header.wrapped, binding(header)),
        )
        try {
          const key = await crypto.subtle.importKey('raw', raw, 'AES-GCM', false, [
            'encrypt',
            'decrypt',
          ])
          await open(key, snapshot.sealed, `${binding(header)}:${snapshot.revision}`)
          return new Vault(header, key)
        } finally {
          raw.fill(0)
        }
      } catch {
        throw new Error('无法解锁：口令错误、密钥失效或密文损坏；没有明文恢复方式')
      }
    })
    await vault.holdKey()
    return vault
  }
  lock() {
    this.key = undefined
    this.releaseKey?.()
    this.releaseKey = undefined
  }
  async read(): Promise<VaultState> {
    const { header, snapshot } = await readDisk()
    if (!this.key || header.id !== this.header.id) throw new Error('离线库已锁定或清除')
    try {
      const state: VaultState = JSON.parse(
        await open(this.key, snapshot.sealed, `${binding(header)}:${snapshot.revision}`),
      )
      if (
        state.version !== 1 ||
        !state.owner ||
        !Array.isArray(state.queue) ||
        !Array.isArray(state.packs)
      )
        throw new Error()
      return state
    } catch {
      throw new Error('离线库内容或完整性标记损坏，停止读写')
    }
  }
  // 调用者必须持有同一 Web Lock；所有修改都先重新读取当前已提交快照。
  async updateLocked(change: (state: VaultState) => void | Promise<void>): Promise<VaultState> {
    const state = await this.read()
    const { snapshot } = await readDisk()
    await change(state)
    state.clockFloor = Math.max(state.clockFloor, Date.now())
    const text = JSON.stringify(state)
    if (bytes(text).length > maxBytes || snapshot.revision >= 100_000)
      throw new Error('离线库达到安全容量或写入次数上限，请先同步并清除')
    if (!this.key) throw new Error('离线库已锁定')
    const next = {
      revision: snapshot.revision + 1,
      sealed: await seal(this.key, text, `${binding(this.header)}:${snapshot.revision + 1}`),
    }
    await writeDisk(this.header, next)
    broadcast('changed')
    return state
  }
  update(change: (state: VaultState) => void | Promise<void>) {
    return exclusive(() => this.updateLocked(change))
  }
}
export async function destroyLocked(): Promise<void> {
  broadcast('purged')
  await new Promise<void>((resolve, reject) => {
    const req = indexedDB.deleteDatabase(databaseName)
    req.onsuccess = () => resolve()
    req.onerror = () => reject(req.error)
    req.onblocked = () => reject(new Error('其他标签页仍占用离线库，清除待完成'))
  })
  for (const name of await caches.keys())
    if (name.startsWith('edu-offline-shell-')) await caches.delete(name)
  for (const registration of await navigator.serviceWorker.getRegistrations())
    if (registration.active?.scriptURL.startsWith(`${location.origin}/app/offline-sw.js`))
      await registration.unregister()
  if (
    (await indexedDB.databases()).some((db) => db.name === databaseName) ||
    (await caches.keys()).some((name) => name.startsWith('edu-offline-shell-'))
  )
    throw new Error('离线库或缓存清除未核对成功')
  localStorage.removeItem(markerName)
}
export async function destroy(vault?: Vault): Promise<void> {
  vault?.lock()
  broadcast('purged')
  // 等待所有解锁页面释放内存密钥；挂起或未响应页面不能被当作已清除。
  await navigator.locks.request(
    keyLockName,
    { mode: 'exclusive', signal: AbortSignal.timeout(5000) },
    () => exclusive(destroyLocked),
  )
}
