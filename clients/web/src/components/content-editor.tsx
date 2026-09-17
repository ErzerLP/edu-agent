import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from '@/api/client'
import { contentSelection, type EditAction } from '@/api/content'
import {
  contentHeader,
  contentSchema,
  type Block,
  type Content,
  type ContentSelection,
} from '@/api/teaching'
import type { Goal } from '@/api/runtime'
import { settingsSchema } from '@/api/settings'
import {
  isMentorTerminal,
  mentorCurrentSchema,
  mentorReceiptSchema,
  mentorSnapshotSchema,
  mentorStatus,
  observeMentor,
  type MentorSnapshot,
} from '@/api/mentor'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { ErrorState } from './common'
import { SafeMarkdown } from './content-blocks'

type Selected = { location: ContentSelection; block: Block; start: number; end: number }
export function useContentSelection(content?: Content) {
  const [selected, setSelected] = useState<Selected>()
  const current = useRef(content)
  current.current = content
  const sequence = useRef(0)
  useEffect(() => {
    setSelected(undefined)
    sequence.current++
  }, [content?.artifact_id])
  useEffect(
    () => () => {
      sequence.current++
    },
    [],
  )
  const select = async (block: Block, start: number, end: number) => {
    if (!content || start === end) return
    const identity = ++sequence.current
    const location = await contentSelection(content, block, start, end)
    if (
      identity === sequence.current &&
      current.current?.artifact_id === location.artifact_id &&
      current.current.version === location.version
    )
      setSelected({ location, block, start, end })
  }
  return {
    selected,
    select,
    clear: () => {
      sequence.current++
      setSelected(undefined)
    },
  }
}

export function ContentEditor({
  content,
  goal,
  selection,
  onUpdate,
  onRelocate,
  disabled = false,
}: {
  content: Content
  goal: Goal
  selection: ReturnType<typeof useContentSelection>
  onUpdate: (value: Content) => void
  onRelocate?: (value: Content) => void
  disabled?: boolean
}) {
  const { session, prefix, drafts } = useIdentity()
  const key = JSON.stringify([
    ...prefix,
    content.learning_space_id,
    content.artifact_id,
    'content-edit',
  ])
  const [prompt, setPrompt] = useState(() => drafts.get<string>(key) ?? '')
  const [action, setAction] = useState<EditAction>('explain')
  const [run, setRun] = useState<MentorSnapshot>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [tokens, setTokens] = useState(30000)
  const [save, setSave] = useState(false)
  const [requests, setRequests] = useState(1)
  const sessionId = useRef<string>(crypto.randomUUID())
  const mounted = useRef(true)
  const delivered = useRef('')
  const callback = useRef(onUpdate)
  callback.current = onUpdate
  const contentRef = useRef(content)
  contentRef.current = content
  const header = { 'X-Learning-Space-ID': content.learning_space_id }
  const client = () => learningClient(session, content.learning_space_id)
  const current = useQuery({
    queryKey: [...prefix, content.learning_space_id, goal.goal_id, 'content-edit-current'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        client().GET('/v1/learning/goals/{goalID}/content-edits', {
          params: { path: { goalID: goal.goal_id }, header },
          signal,
        }),
        mentorCurrentSchema,
      ),
  })
  const configuration = useQuery({
    queryKey: [...prefix, 'settings'],
    queryFn: ({ signal }) => unwrap(client().GET('/v1/settings', { signal }), settingsSchema),
  })
  const modelReady =
    configuration.data?.effective_mentor.enabled && configuration.data.effective_mentor.configured
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  useEffect(() => {
    if (current.data?.run) {
      setRun(current.data.run)
      sessionId.current = current.data.run.session_id
    }
  }, [current.data])
  useEffect(() => {
    if (!run) return
    return observeMentor(session, run, (next, failure) => {
      setRun(next)
      setError(failure)
    })
  }, [run?.run_id, key, session])
  const belongs =
    run?.content_edit?.request.selection.artifact_id === content.artifact_id &&
    run?.space_id === content.learning_space_id &&
    run?.goal_id === content.goal_id &&
    run?.privacy_generation === session.generation
  useEffect(() => {
    const result = run?.content_edit?.result
    if (
      !belongs ||
      !result ||
      run?.status !== 'succeeded' ||
      delivered.current === `${run.run_id}:${result.version}`
    )
      return
    let active = true
    const controller = new AbortController()
    void unwrap(
      client().GET('/v1/learning/content/{artifactID}', {
        params: {
          path: { artifactID: result.artifact_id },
          header: contentHeader(content.learning_space_id),
        },
        signal: controller.signal,
      }),
      contentSchema,
    )
      .then((value) => {
        if (
          !active ||
          value.artifact_id !== contentRef.current.artifact_id ||
          value.session_id !== contentRef.current.session_id ||
          value.learning_space_id !== contentRef.current.learning_space_id ||
          value.privacy_generation !== session.generation
        )
          return
        delivered.current = `${run.run_id}:${result.version}`
        if (value.version > contentRef.current.version) callback.current(value)
      })
      .catch((e) => {
        if (active) setError(e)
      })
    return () => {
      active = false
      controller.abort()
    }
  }, [run?.content_edit?.result?.version, run?.run_id, belongs, key])
  const refresh = async (id: string) => {
    const value = await unwrap(
      client().GET('/v1/learning/runs/{runID}', { params: { path: { runID: id }, header } }),
      mentorSnapshotSchema,
    )
    if (mounted.current) setRun(value)
  }
  const relocate = async () => {
    if (busy) return
    setBusy(true)
    try {
      const value = await unwrap(
        client().GET('/v1/learning/content/{artifactID}', {
          params: {
            path: { artifactID: content.artifact_id },
            header: contentHeader(content.learning_space_id),
          },
        }),
        contentSchema,
      )
      if (
        mounted.current &&
        value.artifact_id === contentRef.current.artifact_id &&
        value.learning_space_id === contentRef.current.learning_space_id &&
        value.privacy_generation === session.generation
      ) {
        selection.clear()
        setError(undefined)
        ;(onRelocate ?? callback.current)(value)
      }
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setBusy(false)
    }
  }
  const start = async () => {
    if (busy || !selection.selected || !prompt.trim() || !save) return
    setBusy(true)
    setError(undefined)
    const payload = {
      session_id: run?.session_id ?? sessionId.current,
      expected_version: goal.revision,
      prompt,
      save: true,
      request_budget: requests,
      token_budget: tokens,
      content_edit: { selection: selection.selected.location, action },
    }
    try {
      const receipt = await unwrap(
        client().POST('/v1/learning/goals/{goalID}/runs', {
          params: { path: { goalID: goal.goal_id }, header },
          body: { ...payload, operation_id: drafts.operation(key + ':create', payload) },
        }),
        mentorReceiptSchema,
      )
      if (mounted.current) await refresh(receipt.run_id)
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setBusy(false)
    }
  }
  const command = async (kind: 'stop' | 'continue_budget') => {
    if (!run || busy) return
    setBusy(true)
    setError(undefined)
    const payload = {
      kind,
      expected_version: run.version,
      ...(kind === 'continue_budget' ? { request_budget: requests, token_budget: tokens } : {}),
    }
    try {
      await unwrap(
        client().POST('/v1/learning/runs/{runID}/commands', {
          params: { path: { runID: run.run_id }, header },
          body: { ...payload, operation_id: drafts.operation(key + ':command', payload) },
        }),
        mentorReceiptSchema,
      )
      if (mounted.current) await refresh(run.run_id)
    } catch (e) {
      if (mounted.current) setError(e)
    } finally {
      if (mounted.current) setBusy(false)
    }
  }
  const active = run && !isMentorTerminal(run.status)
  const stale = selection.selected && selection.selected.location.version !== content.version
  const inactive =
    disabled ||
    !['draft', 'active'].includes(goal.management.status) ||
    !session.device.scopes.includes('learning:write') ||
    !session.device.scopes.includes('knowledge:read')
  return (
    <section className="content-editor" aria-label="选段导师">
      <h3>选段协作</h3>
      {!modelReady && <p>请先在设置中配置并启用 Web 导师模型。</p>}
      {modelReady && (
        <p className="hint">
          本次使用 {configuration.data?.effective_mentor.model} ·{' '}
          {configuration.data?.effective_mentor.endpoint}
        </p>
      )}
      {selection.selected ? (
        <div className="selection-chip">
          <span>
            已选第 {selection.selected.location.version} 版 ·{' '}
            {selection.selected.block.text
              ?.slice(selection.selected.start, selection.selected.end)
              .slice(0, 100)}
          </span>
          <Button variant="ghost" onClick={selection.clear}>
            移除选段
          </Button>
          <details>
            <summary>用键盘调整选段</summary>
            <p className="hint">在原文中用 Shift 与方向键选择，再点击“使用所选范围”。</p>
            <RangeSelector selected={selection.selected} select={selection.select} />
          </details>
        </div>
      ) : (
        <p className="hint">在正文选中文字，或用“选择此段”按钮。</p>
      )}
      {stale && <p role="alert">选区版本已过期。指令已保留，请重新选择当前版本中的位置。</p>}
      {(stale ||
        content.version !== content.committed_version ||
        (error instanceof ApiError && error.status === 409) ||
        run?.reason === 'selection_expired') && (
        <Button variant="outline" disabled={busy} onClick={() => void relocate()}>
          读取最新正文并重新选段
        </Button>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void start()
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.nativeEvent.isComposing || e.keyCode === 229))
            e.preventDefault()
        }}
      >
        <label>
          选段动作
          <select value={action} onChange={(e) => setAction(e.target.value as EditAction)}>
            <option value="explain">解释这里</option>
            <option value="example">换个例子</option>
            <option value="expand">展开</option>
            <option value="critique">指出问题</option>
            <option value="rewrite">局部改写</option>
          </select>
        </label>
        <label>
          选段指令
          <textarea
            rows={3}
            value={prompt}
            maxLength={4000}
            onChange={(e) => {
              setPrompt(e.target.value)
              drafts.set(key, e.target.value)
            }}
          />
        </label>
        <p className="hint">原题及原始教学正文附加说明；派生说明可局部替换。不会改变评分或目标。</p>
        <label className="check-label">
          <input type="checkbox" checked={save} onChange={(e) => setSave(e.target.checked)} />
          允许把所选正文及来源发送给已配置导师，并加密保存本次恢复正文（最多七日）
        </label>
        <details open={run?.status === 'paused_budget'}>
          <summary>本次模型预算</summary>
          <label>
            请求上限
            <input
              type="number"
              min={1}
              max={1000}
              value={requests}
              onChange={(e) => setRequests(Math.max(1, Number(e.target.value)))}
            />
          </label>
          <label>
            Token 上限
            <input
              type="number"
              min={1}
              max={10000000}
              value={tokens}
              onChange={(e) => setTokens(Math.max(1, Number(e.target.value)))}
            />
          </label>
          {belongs && run?.status === 'paused_budget' && (
            <Button
              type="button"
              disabled={busy || inactive}
              onClick={() => void command('continue_budget')}
            >
              增加上述预算并继续
            </Button>
          )}
        </details>
        <Button
          type="submit"
          disabled={
            busy ||
            inactive ||
            !!active ||
            !!stale ||
            !selection.selected ||
            !prompt.trim() ||
            !save ||
            !current.data ||
            !modelReady
          }
        >
          执行选段请求
        </Button>
      </form>
      {active && !belongs && (
        <p role="status">本目标另一个内容加工尚未结束，请先回到对应内容查看。</p>
      )}
      {belongs && run && (
        <div aria-label="选段操作结果">
          <p role="status">
            {mentorStatus[run.status]}
            {run.content_edit?.result ? ' · 正式新版本已保存' : ' · 原内容保留，以下为未提交输出'}
          </p>
          {run.reason === 'selection_expired' && (
            <p role="alert">选区过期或并发编辑冲突。指令已保留，请刷新正文后重新定位。</p>
          )}
          {run.output && <SafeMarkdown text={run.output} />}
          {run.content_edit?.result && (
            <Link
              to="/content/$artifactId"
              params={{ artifactId: content.artifact_id }}
              search={{
                space: content.learning_space_id,
                version: run.content_edit.result.version,
              }}
            >
              本段已更新 · 查看变化
            </Link>
          )}
          {active && (
            <Button
              variant="outline"
              disabled={busy || run.status === 'cancelling'}
              onClick={() => void command('stop')}
            >
              停止选段加工
            </Button>
          )}
        </div>
      )}
      {!!(error || current.error || configuration.error) && (
        <ErrorState
          error={error || current.error || configuration.error}
          retry={() => void current.refetch()}
        />
      )}
    </section>
  )
}

function RangeSelector({
  selected,
  select,
}: {
  selected: Selected
  select: (block: Block, start: number, end: number) => Promise<void>
}) {
  const input = useRef<HTMLTextAreaElement>(null)
  return (
    <>
      <textarea ref={input} aria-label="选段原文" readOnly value={selected.block.text} rows={5} />
      <Button
        type="button"
        variant="outline"
        onClick={() => {
          const el = input.current
          if (el && el.selectionEnd > el.selectionStart)
            void select(selected.block, el.selectionStart, el.selectionEnd)
        }}
      >
        使用所选范围
      </Button>
    </>
  )
}
