import { describe, it, expect, vi } from 'vitest'
import {
  scanImportFiles,
  selectedSources,
  uploadRemaining,
  continueImport,
  base64,
  type LocalImport,
} from './import-files'
import type { ImportJob } from '../api/import-jobs'

const source = async (name = 'a.md', text = '# 原文') =>
  selectedSources(await scanImportFiles([new File([text], name)]))[0]
const job = (sources: LocalImport[]): ImportJob =>
  ({
    id: '任务',
    status: 'uploading',
    approved: false,
    batches: sources.map((s) => ({ item: s.item, operation_id: s.item.path, status: 'pending' })),
  }) as ImportJob

describe('任务来源和恢复', () => {
  it('接受超过单请求容量的完整任务，单文件仍有界，纯文本保留原文', async () => {
    const rows = await scanImportFiles(
      Array.from({ length: 5 }, (_, i) => new File(['x'.repeat(3500000)], `${i}.md`)),
    )
    expect(selectedSources(rows).reduce((n, s) => n + s.item.bytes, 0)).toBeGreaterThan(16 << 20)
    const invalid = await scanImportFiles([
      new File(['x'.repeat((4 << 20) + 1)], 'large.md'),
      new File([new Uint8Array([255])], 'broken.md'),
      new File(['x'], 'a.pdf'),
    ])
    expect(invalid.every((row) => !!row.error && !row.selected)).toBe(true)
    const text = await source('中文.txt', '```\n原文🙂')
    expect(text.item.path).toBe('中文.txt.md')
    expect(new TextDecoder().decode(text.content)).toContain('````text\n```\n原文🙂\n````')
    expect(base64(text.content)).toBe(Buffer.from(text.content).toString('base64'))
  })
  it('不悄悄选择转换后路径冲突的文件', async () => {
    const rows = await scanImportFiles([new File(['x'], 'a.txt'), new File(['x'], 'a.txt.md')])
    expect(rows.every((row) => !row.selected && row.error)).toBe(true)
  })
  it('先读取原任务，核对全部来源，再只补传缺失项', async () => {
    const sources = [await source(), await source('b.md')]
    const original = job(sources)
    original.batches[0].status = 'completed'
    const calls: string[] = []
    const api = {
      get: vi.fn(async () => {
        calls.push('get')
        return original
      }),
      command: vi.fn(async () => {
        calls.push('upload')
        return original
      }),
    }
    await uploadRemaining(api, original.id, sources, new AbortController().signal, () => {})
    expect(calls).toEqual(['get', 'upload'])
    expect(api.command).toHaveBeenCalledWith(
      expect.objectContaining({ id: original.id, action: 'upload', batch: 1 }),
    )
    api.command.mockClear()
    await expect(
      uploadRemaining(
        api,
        original.id,
        [await source('a.md', '已变更'), sources[1]],
        new AbortController().signal,
        () => {},
      ),
    ).rejects.toThrow('摘要变化')
    expect(api.command).not.toHaveBeenCalled()
  })
  it('断线或页面关闭后不上传下一批', async () => {
    const sources = [await source(), await source('b.md')]
    const original = job(sources)
    const controller = new AbortController()
    const api = {
      get: async () => original,
      command: vi.fn(async () => {
        controller.abort()
        return original
      }),
    }
    await expect(
      uploadRemaining(api, original.id, sources, controller.signal, () => {}),
    ).rejects.toThrow()
    expect(api.command).toHaveBeenCalledTimes(1)
  })
  it('继续先对账，结果未知立即停止，不分配新 operation', async () => {
    const original = job([await source(), await source('b.md')])
    original.approved = true
    original.status = 'committing'
    const calls: string[] = []
    const api = {
      get: async () => {
        calls.push('get')
        return original
      },
      command: vi.fn(async () => {
        calls.push('continue')
        return {
          ...original,
          batches: original.batches.map((b) => ({ ...b, status: 'unknown' as const })),
        }
      }),
    }
    await continueImport(api, original.id, new AbortController().signal, () => {}, true)
    expect(calls).toEqual(['get', 'continue'])
    expect(api.command).toHaveBeenCalledWith({ id: original.id, action: 'continue' })
  })
  it('已取消或失去批准时对账后不继续', async () => {
    const original = job([await source()])
    original.status = 'cancelled'
    const api = { get: async () => original, command: vi.fn() }
    await continueImport(api, original.id, new AbortController().signal, () => {}, true)
    expect(api.command).not.toHaveBeenCalled()
  })
})
