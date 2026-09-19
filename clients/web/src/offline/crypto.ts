export const encoder = new TextEncoder()
export const decoder = new TextDecoder('utf-8', { fatal: true })
export const bytes = (text: string) => encoder.encode(text)
const wellFormed = (text: string) =>
  !/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u.test(text)
export function base64(data: Uint8Array): string {
  let result = ''
  for (const byte of data) result += String.fromCharCode(byte)
  return btoa(result).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
}
export function unbase64(text: string): Uint8Array<ArrayBuffer> {
  if (!/^[A-Za-z0-9_-]+$/.test(text)) throw new Error('加密数据编码无效')
  const result = Uint8Array.from(atob(text.replaceAll('-', '+').replaceAll('_', '/')), (c) =>
    c.charCodeAt(0),
  )
  if (base64(result) !== text) throw new Error('加密数据编码不规范')
  return result
}
export const digest = async (text: string) =>
  base64(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes(text))))
export const hexDigest = async (text: string) =>
  Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes(text))), (b) =>
    b.toString(16).padStart(2, '0'),
  ).join('')

// 原签名对象保留其传输字节；排序仅用于验证 JCS 摘要，不替换保存或提交的正文。
const originals = new WeakMap<object, string>()
export function original(value: object): string {
  const raw = originals.get(value)
  if (raw === undefined) throw new Error('缺少原始签名字节')
  return raw
}
export function canonical(value: unknown): string {
  if (value === null || typeof value === 'boolean') return JSON.stringify(value)
  if (typeof value === 'string') {
    if (!wellFormed(value)) throw new Error('文本包含无效 Unicode')
    return JSON.stringify(value)
  }
  if (typeof value === 'number' && Number.isSafeInteger(value)) return JSON.stringify(value)
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  if (typeof value === 'object' && value)
    return `{${Object.keys(value)
      .sort()
      .map((k) => `${canonical(k)}:${canonical((value as Record<string, unknown>)[k])}`)
      .join(',')}}`
  throw new Error('离线协议包含不支持的数字或 JSON 值')
}
export function parseSigned(text: string): unknown {
  if (bytes(text).length > 8 * 1024 * 1024) throw new Error('离线包超过 8 MiB')
  let position = 0
  const space = () => {
    while (/[\t\n\r ]/.test(text[position] ?? 'x')) position++
  }
  const fail = (): never => {
    throw new Error('离线协议 JSON 损坏或存在重复字段')
  }
  function string(): string {
    const start = position++
    while (position < text.length) {
      const character = text[position++]
      if (character === '\\') position++
      else if (character === '"') {
        const result: string = JSON.parse(text.slice(start, position))
        if (!wellFormed(result)) fail()
        return result
      }
    }
    return fail()
  }
  function read(depth = 0): unknown {
    if (depth > 64) fail()
    space()
    const start = position
    if (text[position] === '"') return string()
    if (text[position] === '{' || text[position] === '[') {
      const array = text[position++] === '['
      const close = array ? ']' : '}'
      const value: unknown[] | Record<string, unknown> = array ? [] : Object.create(null)
      space()
      if (text[position] !== close) {
        while (position < text.length) {
          if (array) (value as unknown[]).push(read(depth + 1))
          else {
            if (text[position] !== '"') fail()
            const key = string()
            if (Object.hasOwn(value, key)) fail()
            space()
            if (text[position++] !== ':') fail()
            ;(value as Record<string, unknown>)[key] = read(depth + 1)
          }
          space()
          if (text[position] === close) break
          if (text[position++] !== ',') fail()
          space()
        }
      }
      if (text[position++] !== close) fail()
      originals.set(value, text.slice(start, position))
      return value
    }
    const token = /^(?:true|false|null|-?(?:0|[1-9][0-9]*))/.exec(text.slice(position))?.[0]
    if (!token) return fail()
    position += token.length
    const value: unknown = JSON.parse(token)
    if (typeof value === 'number' && !Number.isSafeInteger(value)) fail()
    return value
  }
  const result = read()
  space()
  if (position !== text.length) fail()
  return result
}

export type Sealed = { iv: string; ciphertext: string }
export async function seal(key: CryptoKey, text: string, aad: string): Promise<Sealed> {
  const iv = crypto.getRandomValues(new Uint8Array(12))
  const encrypted = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv, additionalData: bytes(aad) },
    key,
    bytes(text),
  )
  return { iv: base64(iv), ciphertext: base64(new Uint8Array(encrypted)) }
}
export async function open(key: CryptoKey, data: Sealed, aad: string): Promise<string> {
  return decoder.decode(
    await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: unbase64(data.iv), additionalData: bytes(aad) },
      key,
      unbase64(data.ciphertext),
    ),
  )
}
export async function passwordKey(password: string, salt: string): Promise<CryptoKey> {
  const material = await crypto.subtle.importKey('raw', bytes(password), 'PBKDF2', false, [
    'deriveKey',
  ])
  return crypto.subtle.deriveKey(
    { name: 'PBKDF2', hash: 'SHA-256', salt: unbase64(salt), iterations: 600_000 },
    material,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt'],
  )
}
