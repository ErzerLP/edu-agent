import { describe, expect, it } from 'vitest'
import { changeBase, changeSchema, changeStatus } from './changes'

describe('独立教学变更协议', () => {
  it('排队与已批准不能显示为已生效，失效差异仍可读', () => {
    expect(changeStatus.queued_for_boundary).toContain('本题处理后')
    expect(changeStatus.approved).toContain('尚未生效')
    expect(changeStatus.stale).toContain('只读')
  })
  it('拒绝没有审批绑定和基础版本的伪成功', () => {
    expect(changeSchema.safeParse({ status: 'applied', revision: 2 }).success).toBe(false)
    expect(changeBase.safeParse({ goal_version: 2, session_version: -1 }).success).toBe(false)
  })
})
