import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useIdentity } from '@/lib/session'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { spaceSchema, type Space } from '@/api/runtime'
import { Button } from '../ui/button'
import { ErrorState } from '../common'

export function SpaceDialog({
  space,
  confirmDirection = false,
  onCreated,
}: {
  space?: Space
  confirmDirection?: boolean
  onCreated?: (space: Space, addSources: boolean) => void
}) {
  const { session, prefix, drafts } = useIdentity()
  const query = useQueryClient()
  const dialog = useRef<HTMLDialogElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const live = useRef(true)
  const key = JSON.stringify([...prefix, space?.id ?? 'new', 'space-direction'])
  const initial = drafts.get<{ name: string; direction: string }>(key)
  const [name, setName] = useState(initial?.name ?? space?.name ?? '')
  const [direction, setDirection] = useState(
    initial?.direction ?? space?.direction ?? (confirmDirection ? space?.name : '') ?? '',
  )
  const [base, setBase] = useState(space)
  const [addSources, setAddSources] = useState(false)
  const [confirmLifecycle, setConfirmLifecycle] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  const open = () => {
    setBase(space)
    setConfirmLifecycle(false)
    if (!drafts.get(key)) {
      setName(space?.name ?? '')
      setDirection(space?.direction ?? (confirmDirection ? space?.name : '') ?? '')
    }
    dialog.current?.showModal()
  }
  const save = async (status = base?.status ?? 'active', lifecycle = false) => {
    if (busy || !name.trim() || (!lifecycle && !direction.trim())) return
    setBusy(true)
    setError(undefined)
    const payload = {
      name: name.trim(),
      description: base?.description ?? '',
      status,
      expected_version: base?.version ?? 0,
      ...(status === 'active' && !lifecycle ? { direction: direction.trim() } : {}),
    }
    const body = { ...payload, operation_id: drafts.operation(key + ':save', payload) }
    try {
      const api = learningClient(session)
      const result = await unwrap(
        base
          ? api.PUT('/v1/learning-spaces/{spaceID}', {
              params: { path: { spaceID: base.id } },
              body,
            })
          : api.POST('/v1/learning-spaces', { body }),
        spaceSchema,
      )
      if (!live.current) return
      drafts.delete(key)
      drafts.delete(key + ':save')
      setBase(result)
      dialog.current?.close()
      await query.invalidateQueries({ queryKey: prefix })
      if (live.current && !space) onCreated?.(result, addSources)
    } catch (e) {
      if (live.current) setError(e)
    } finally {
      if (live.current) setBusy(false)
    }
  }
  const title = confirmDirection ? '确认学习方向' : space ? '学习区设置' : '新建学习区'
  return (
    <>
      <Button ref={trigger} aria-label={title} variant="outline" disabled={confirmDirection && space?.status === 'archived'} onClick={open}>
        {title}
      </Button>
      <dialog
        ref={dialog}
        className="notebook-space-dialog"
        aria-label={title}
        onClose={() => trigger.current?.focus()}
      >
        <h2>{title}</h2>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && e.nativeEvent.isComposing) e.preventDefault()
          }}
        >
          <fieldset disabled={busy || !session.capabilities.save_goal}>
            <label>
              学习区名称
              <input
                aria-label="学习区名称"
                autoFocus
                required
                maxLength={120}
                placeholder="例如：我想弄懂的线性代数"
                value={name}
                onChange={(e) => {
                  setName(e.target.value)
                  drafts.set(key, { name: e.target.value, direction })
                }}
              />
            </label>
            <label>
              学习方向
              <textarea
                aria-label="学习方向"
                disabled={base?.status === 'archived'}
                required
                rows={3}
                maxLength={2000}
                placeholder="用一句话，说说你想学会什么"
                value={direction}
                onChange={(e) => {
                  setDirection(e.target.value)
                  drafts.set(key, { name, direction: e.target.value })
                }}
              />
            </label>
            {!space && (
              <label className="check-label">
                <input
                  type="checkbox"
                  checked={addSources}
                  onChange={(e) => setAddSources(e.target.checked)}
                />
                现在添加来源
              </label>
            )}
            <div className="actions">
              <Button variant="outline" type="button" onClick={() => dialog.current?.close()}>
                取消
              </Button>
              <Button type="submit">{space ? '保存设置' : '创建学习区'}</Button>
            </div>
            {base && !confirmDirection && (
              confirmLifecycle ? (
                <div role="group" aria-label="确认变更学习区状态">
                  <p>归档后保留已有历史。</p>
                  <div className="actions">
                    <Button variant="outline" autoFocus onClick={() => setConfirmLifecycle(false)}>
                      取消变更
                    </Button>
                    <Button
                      variant="destructive"
                      onClick={() => void save(base.status === 'archived' ? 'active' : 'archived', true)}
                    >
                      确认{base.status === 'archived' ? '恢复学习区' : '归档学习区'}
                    </Button>
                  </div>
                </div>
              ) : (
                <Button variant="outline" onClick={() => setConfirmLifecycle(true)}>
                  {base.status === 'archived' ? '恢复学习区' : '归档学习区'}
                </Button>
              )
            )}
          </fieldset>
        </form>
        {!!error && <ErrorState error={error} />}
        {error instanceof ApiError && error.status === 409 && base && (
          <Button
            onClick={async () => {
              try {
                const next = await unwrap(
                  learningClient(session).GET('/v1/learning-spaces/{spaceID}', {
                    params: { path: { spaceID: base.id } },
                  }),
                  spaceSchema,
                )
                if (live.current) {
                  setBase(next)
                  setError(undefined)
                }
              } catch (e) {
                if (live.current) setError(e)
              }
            }}
          >
            读取最新版本后重新确认
          </Button>
        )}
      </dialog>
    </>
  )
}
