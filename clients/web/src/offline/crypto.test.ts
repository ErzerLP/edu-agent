import { describe, expect, it } from 'vitest'
import { base64, canonical, open, original, parseSigned, passwordKey, seal } from './crypto'

describe('离线签名字节与认证加密', () => {
  it('拒绝重复字段、不安全整数、无效 Unicode 和尾随值，保留原始 payload', () => {
    for (const raw of [
      '{"a":1,"a":2}',
      '[9007199254740992]',
      '{"n":1.2}',
      '"\\ud800"',
      '{}{}',
      '[1,]',
    ])
      expect(() => parseSigned(raw)).toThrow()
    const raw = '{ "payload": { "z":1, "a":"原文\\n" }, "signature":"s" }'
    const value = parseSigned(raw) as { payload: object }
    expect(original(value.payload)).toBe('{ "z":1, "a":"原文\\n" }')
    expect(canonical(value.payload)).toBe('{"a":"原文\\n","z":1}')
    expect(canonical(parseSigned('{"seq":"9223372036854775807"}'))).toBe(
      '{"seq":"9223372036854775807"}',
    )
  })
  it('正文必须经同一密钥及归属解密，错误口令、改 nonce 或标签全部拒绝', async () => {
    const salt = base64(crypto.getRandomValues(new Uint8Array(16)))
    const key = await passwordKey('独立离线长口令-1234567890', salt)
    const sealed = await seal(key, '私有题目与未同步答案', '原设备:1')
    expect(await open(key, sealed, '原设备:1')).toBe('私有题目与未同步答案')
    await expect(open(key, sealed, '另一设备:1')).rejects.toThrow()
    const wrong = await passwordKey('另一个离线长口令-1234567890', salt)
    await expect(open(wrong, sealed, '原设备:1')).rejects.toThrow()
    await expect(
      open(key, { ...sealed, ciphertext: sealed.ciphertext.slice(0, -5) }, '原设备:1'),
    ).rejects.toThrow()
    expect(JSON.stringify(sealed)).not.toContain('私有题目')
  })
})
