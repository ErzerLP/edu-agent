import { afterAll, describe, expect, it, vi } from 'vitest'
vi.hoisted(() => vi.stubGlobal('window', { location: { origin: 'http://localhost' } }))
import { localState, receiptSchema, taskSnapshots } from './companion'
afterAll(() => vi.unstubAllGlobals())

describe('本地操作回执', () => {
  const base = { id: 'operation', device: 'device', conversation: 'conversation', run: 'run', state: 'completed' }
  it('未知、已收回执与命令成功分开', () => {
    expect(localState.unknown).toContain('未知')
    expect(localState.completed).toContain('不代表命令成功')
    const r = receiptSchema.parse({ ...base, value: { Written: 3, Outcome: 'partial' } })
    expect(r.value).toEqual({ Written: 3, Outcome: 'partial' })
    expect(taskSnapshots([r])).toEqual([])
  })
  it('任务使用实际可控制状态，后续快照覆盖旧状态', () => {
    const first = { ...base, value: { TaskID: 'task', State: 'running', Controllable: true, PTY: true } }
    const last = { ...base, value: { items: [{ TaskID: 'task', State: 'unknown', Controllable: false, PTY: true, ExitCode: null, OutputState: 'incomplete' }] } }
    expect(taskSnapshots([first, last])).toEqual(last.value.items)
  })
})
