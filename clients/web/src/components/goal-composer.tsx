import { useEffect, useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useIdentity } from '@/lib/session'
import { ApiError, learningClient, unwrap } from '@/api/client'
import {
  composerSchema,
  detailsSchema,
  goalResult,
  goalSchema,
  type Goal,
  type GoalDraft,
} from '@/api/runtime'
import type { components } from '@/api/schema'
import type { z } from 'zod'
import { Button } from './ui/button'
import { CapabilityGate, ErrorState } from './common'

type SavedDraft = { values: z.input<typeof composerSchema>; id: string; base?: Goal }
const blank = (): GoalDraft => ({ text: '', details: detailsSchema.parse({ name: '新学习目标' }) })

export function GoalComposer({
  spaceId,
  goal,
  disabled = false,
}: {
  spaceId: string
  goal?: Goal
  disabled?: boolean
}) {
  const { session, drafts, prefix } = useIdentity()
  const query = useQueryClient()
  const key = JSON.stringify([...prefix, spaceId, goal?.goal_id ?? 'new', 'composer'])
  const previous = drafts.get<SavedDraft>(key)
  const [id, setId] = useState(() => previous?.id ?? goal?.goal_id ?? crypto.randomUUID())
  const [base, setBase] = useState(previous?.base ?? goal)
  const initial =
    previous?.values ?? (goal ? { text: goal.text, details: goal.management.details } : blank())
  const form = useForm<z.input<typeof composerSchema>, unknown, GoalDraft>({
    resolver: zodResolver(composerSchema),
    defaultValues: initial,
  })
  const [error, setError] = useState<unknown>()
  const [saved, setSaved] = useState<Goal>()
  const [notice, setNotice] = useState('')
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  useEffect(() => {
    const sub = form.watch(() => {
      const values = form.getValues()
      const baseline = base ? { text: base.text, details: base.management.details } : blank()
      if (JSON.stringify(values) !== JSON.stringify(baseline)) drafts.set(key, { values, id, base })
      else drafts.delete(key)
    })
    return () => sub.unsubscribe()
  }, [base, key, id])
  useEffect(() => {
    if (goal && !form.formState.isDirty && !drafts.get(key)) {
      setBase(goal)
      form.reset({ text: goal.text, details: goal.management.details })
      drafts.delete(key)
    }
  }, [goal?.revision])
  const save = async (values: GoalDraft) => {
    setError(undefined)
    setSaved(undefined)
    setNotice('')
    const payload = {
      payload_schema_version: 1 as const,
      aggregate_type: 'goal' as const,
      aggregate_id: id,
      expected_version: base?.revision ?? 0,
      text: values.text,
      source: 'web',
      details: values.details,
      ...(base ? { previous_revision_id: base.goal_revision_id } : {}),
    }
    const body: components['schemas']['LearningGoalRequest'] = {
      ...payload,
      operation_id: drafts.operation(key, payload),
    }
    try {
      const client = learningClient(session, spaceId)
      const response = await unwrap(
        base
          ? client.PUT('/v1/learning/goals/{goalID}', { params: { path: { goalID: id } }, body })
          : client.POST('/v1/learning/goals', { body }),
        goalResult,
      )
      if (response.result.learning_space_id !== spaceId || response.result.goal_id !== id)
        throw new ApiError(502, 'invalid_response')
      if (JSON.stringify(drafts.get<SavedDraft>(key)?.values) === JSON.stringify(values))
        drafts.delete(key)
      void query.invalidateQueries({ queryKey: [...prefix, spaceId] })
      if (mounted.current) {
        drafts.delete(key)
        setBase(response.result)
        setSaved(response.result)
        form.reset(values)
      }
    } catch (e) {
      if (mounted.current) setError(e)
    }
  }
  const refreshVersion = async () => {
    try {
      const latest = await unwrap(
        learningClient(session, spaceId).GET('/v1/learning/goals/{goalID}', {
          params: { path: { goalID: id } },
        }),
        goalSchema,
      )
      if (!mounted.current) return
      setBase(latest)
      drafts.set(key, { values: form.getValues(), id, base: latest })
      setError(undefined)
      setNotice(`已读取版本 ${latest.revision}。你的输入已保留，请核对后再次保存。`)
    } catch (e) {
      setError(e)
    }
  }
  return (
    <section className="panel composer">
      <form
        onSubmit={form.handleSubmit(save)}
        onKeyDown={(event) => {
          if (
            event.key === 'Enter' &&
            (event.nativeEvent.isComposing || event.target instanceof HTMLInputElement)
          )
            event.preventDefault()
        }}
      >
        <fieldset
          disabled={disabled || form.formState.isSubmitting || !session.capabilities.save_goal}
        >
          <label htmlFor={`goal-text-${id}`}>{goal ? '学习目标' : '今天想学会什么？'}</label>
          <textarea
            id={`goal-text-${id}`}
            rows={4}
            placeholder="比如：我想看懂一篇英文研究论文，并能用自己的话解释它的核心观点。"
            aria-describedby={`draft-note-${id}`}
            aria-invalid={!!form.formState.errors.text}
            {...form.register('text', {
              onChange: (event) => {
                if (!goal && !form.formState.dirtyFields.details?.name)
                  form.setValue(
                    'details.name',
                    [...event.target.value].slice(0, 120).join('') || '新学习目标',
                  )
              },
            })}
          />
          {form.formState.errors.text && (
            <p className="field-error" role="alert">
              {form.formState.errors.text.message}
            </p>
          )}
          <p id={`draft-note-${id}`} className="hint">
            参考资料可选。未保存的输入仅保留在此标签页，刷新会丢失；保存目标不会开始教学。
          </p>
          <details className="structured" open={goal ? true : undefined}>
            <summary>细化目标（可选）</summary>
            <div className="form-grid">
              <label>
                目标名称
                <input {...form.register('details.name')} maxLength={120} />
              </label>
              <label>
                优先级
                <select {...form.register('details.priority')}>
                  <option value="low">低</option>
                  <option value="normal">普通</option>
                  <option value="high">高</option>
                </select>
              </label>
              {(
                [
                  ['expected_outcome', '期望结果'],
                  ['scope', '学习范围'],
                  ['exclusions', '暂不学习'],
                  ['self_assessment', '当前基础（自述）'],
                  ['purpose', '学习用途'],
                  ['completion_criteria', '完成标准'],
                ] as const
              ).map(([field, label]) => (
                <label key={field}>
                  {label}
                  <textarea rows={2} maxLength={4000} {...form.register(`details.${field}`)} />
                </label>
              ))}
              <label>
                每周可投入分钟
                <input
                  type="number"
                  min={1}
                  max={10080}
                  {...form.register('details.weekly_minutes', {
                    setValueAs: (v) => (v === '' ? undefined : Number(v)),
                    onChange: () =>
                      form.setValue(
                        'details.timezone',
                        form.getValues('details.timezone') || 'UTC',
                      ),
                  })}
                />
              </label>
              <label>
                截止时间（UTC，可选）
                <input
                  type="datetime-local"
                  value={
                    form.watch('details.deadline')
                      ? new Date(form.watch('details.deadline')!).toISOString().slice(0, 16)
                      : ''
                  }
                  onChange={(event) => {
                    form.setValue(
                      'details.deadline',
                      event.target.value ? `${event.target.value}:00Z` : undefined,
                      { shouldDirty: true },
                    )
                    form.setValue('details.timezone', 'UTC')
                  }}
                />
              </label>
            </div>
          </details>
          {form.formState.errors.details && (
            <p role="alert" className="field-error">
              请检查目标名称、字段长度和时间预算。
            </p>
          )}
          <div className="composer-actions">
            <CapabilityGate
              available={session.capabilities.start_learning}
              reason="自动研究与教学入口尚未接入，当前可先保存目标。"
            >
              <Button disabled>开始学习</Button>
            </CapabilityGate>
            <Button type="submit" variant="outline">
              {form.formState.isSubmitting ? '正在保存…' : goal || base ? '保存修改' : '仅保存目标'}
            </Button>
            <CapabilityGate
              available={session.capabilities.references}
              reason="参考绑定将在功能就绪后开放。"
            >
              <Button variant="ghost" disabled>
                补充参考（可选）
              </Button>
            </CapabilityGate>
          </div>
        </fieldset>
        {!!error && <ErrorState error={error} />}
        {error instanceof ApiError && error.status === 409 && base && (
          <Button variant="outline" onClick={() => void refreshVersion()}>
            读取最新版本并保留输入
          </Button>
        )}
        {notice && <p role="status">{notice}</p>}
        {saved && (
          <div className="notice" role="status">
            目标已保存，尚未启动教学。
            <Link to="/spaces/$spaceId/goals/$goalId" params={{ spaceId, goalId: saved.goal_id }}>
              查看目标
            </Link>
            {!goal && (
              <Button
                variant="outline"
                onClick={() => {
                  setId(crypto.randomUUID())
                  setBase(undefined)
                  setSaved(undefined)
                  form.reset(blank())
                  drafts.delete(key)
                }}
              >
                再建一个目标
              </Button>
            )}
          </div>
        )}
      </form>
    </section>
  )
}
