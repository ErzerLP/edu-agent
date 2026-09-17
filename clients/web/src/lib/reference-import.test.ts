import { describe, expect, it } from 'vitest'
import { editImport, emptyImport, importDocuments, pastedItem, scanFiles, textDocument } from './reference-import'

describe('浏览器真实来源与原操作草稿', () => {
  it('逐项报告编码、不支持和成功，只提交明确选择的实际内容', async () => {
    const items = await scanFiles([new File(['# 来源\n正文'], 'a.md'), new File(['纯文本'], 'a.txt'), new File([new Uint8Array([0xff, 0xfe])], 'bad.txt'), new File(['不得上传'], 'private.pdf')])
    expect(items.map(i => i.status)).toEqual(['ready', 'ready', 'error', 'unsupported'])
    items[1].selected = false
    expect(importDocuments(items)).toEqual([{ path: 'a.md', markdown: '# 来源\n正文' }])
    expect(items[3].markdown).toBe('')
  })
  it('同名不静默覆盖，纯文本保留代码围栏', async () => {
    const items = await scanFiles([new File(['第一来源'], 'README.md'), new File(['第二来源'], 'README.md')])
    expect(() => importDocuments(items)).toThrow('路径重复')
    expect(textDocument('文字', '```\n原文')).toContain('````text\n```\n原文\n````')
    expect(pastedItem('原文', '内容').markdown).toContain('内容')
  })
  it('未知提交禁止编辑，结果核对后修改使批准失效', () => {
    const draft = { ...emptyImport(), unknown: true }
    expect(() => editImport(draft, { paste: '新内容' })).toThrow('原操作')
    const edited = editImport({ ...draft, unknown: false }, { paste: '新内容' })
    expect(edited.preview).toBeUndefined(); expect(edited.request).toBeUndefined(); expect(edited.paste).toBe('新内容')
  })
})
