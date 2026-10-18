import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { learningClient, unwrap } from './api/client'
import { pageOf, spaceSchema } from './api/runtime'
import {
  usageSummary,
  usageTasks,
  usageCalls,
  type UsageTotals,
  type UsageTask,
  type UsageFilter,
} from './api/usage'
import { useIdentity } from './lib/session'
import { ErrorState, Pagination } from './components/common'
import { Button } from './components/ui/button'

const count = (n: number) => n.toLocaleString('zh-CN')
const token = (n: number | null) => (n === null ? '未知' : count(n))
const when = (date: string) => new Date(date).toLocaleString('zh-CN')
const taskLabels: Record<string, string> = {
  mentor: '导师对话',
  studio: 'Studio 生成',
  research: '研究',
  assessment: '练习评估',
  teaching: '教学生成',
  planning: '学习规划',
  knowledge: '知识处理',
  content_edit: '教学内容修改',
  offline_prepare: '离线备课',
  offline_assessment: '离线评估',
  model_probe: '模型连接探测',
}
const taskLabel = (kind: string) => taskLabels[kind] ?? '模型任务'
const stageLabels: Record<string, string> = {
  tool_step_summary: '本轮进展说明',
  mentor: '导师交流',
  study_answer: '导师讲解',
  studio_guide: '学习指南',
  studio_brief: '学习简报',
  studio_quiz: '测验生成',
  checking_synthesis: '研究综合',
  preparing_activity: '准备学习活动',
  assessment: '评估',
  directory_selection: '目录选择',
  planning: '规划建议',
  route: '路线生成',
  activity: '活动生成',
  free_answer: '自由回答',
  explanation: '讲解',
  content_edit: '内容修改',
  teaching: '教学模型探测',
  capabilities: '模型能力探测',
}
function stageLabel(kind: string, stage: string) {
  if (kind === 'model_probe') {
    return stage === 'mentor' ? '导师模型探测' : stageLabels[stage] ?? taskLabel(kind)
  }
  return stageLabels[stage] ?? taskLabel(kind)
}
function metricText(metric: UsageTotals['total']) {
  return metric.unknown_calls
    ? `已知 ${count(metric.known_sum)} Token；未知：${count(metric.unknown_calls)} 次调用`
    : `${count(metric.known_sum)} Token`
}
function Totals({ value }: { value: UsageTotals }) {
  return (
    <>
      <p>
        调用 {count(value.calls)} 次 · 处理中 {count(value.pending_calls)} 次 · 结果未知{' '}
        {count(value.outcome_unknown_calls)} 次
      </p>
      <dl className="usage-totals">
        {(
          [
            ['total', '合计'],
            ['input', '输入'],
            ['output', '输出'],
            ['reasoning', '其中推理'],
          ] as const
        ).map(([key, label]) => (
          <div key={key}>
            <dt>{label}</dt>
            <dd>{metricText(value[key])}</dd>
          </div>
        ))}
      </dl>
    </>
  )
}
function Calls({
  task,
  filter,
  close,
}: {
  task: UsageTask
  filter: UsageFilter
  close: () => void
}) {
  const { session, prefix } = useIdentity()
  const [cursors, setCursors] = useState([''])
  const result = useQuery({
    queryKey: [...prefix, 'usage', filter, 'calls', task.id, cursors.at(-1)],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, filter.space_id).GET('/v1/learning/usage/tasks/{taskID}/calls', {
          params: {
            path: { taskID: task.id },
            query: { ...filter, cursor: cursors.at(-1), limit: 20 },
          },
          signal,
        }),
        usageCalls,
      ),
  })
  return (
    <section className="panel" aria-label="调用明细">
      <div className="actions">
        <h2>调用明细</h2>
        <Button variant="ghost" onClick={close}>
          关闭明细
        </Button>
      </div>
      <p>{taskLabel(task.kind)} · {when(task.started_at)}</p>
      {result.isPending && <p role="status">正在读取调用明细…</p>}
      {result.error ? (
        <ErrorState error={result.error} retry={() => void result.refetch()} />
      ) : (
        result.data && (
          <>
            {!result.data.items.length && <p>此筛选范围内没有调用。</p>}
            {result.data.items.map((call) => (
              <article className="panel compact" key={call.id}>
                <h3>{call.model}</h3>
                <p>
                  {when(call.started_at)} ·{' '}
                  {stageLabel(task.kind, call.stage)} · 第 {call.attempt_no}{' '}
                  次调用
                </p>
                <p>
                  {
                    {
                      dispatching: '处理中',
                      received: '已收到响应',
                      failed: '请求失败',
                      cancelled: '已停止',
                      outcome_unknown: '结果未知',
                    }[call.state]
                  }
                </p>
                <dl className="usage-totals">
                  {(
                    [
                      ['输入 Token', call.input_tokens],
                      ['输出 Token', call.output_tokens],
                      ['其中推理 Token', call.reasoning_tokens],
                      ['合计 Token', call.total_tokens],
                    ] as const
                  ).map(([label, value]) => (
                    <div key={label}>
                      <dt>{label}</dt>
                      <dd>{token(value)}</dd>
                    </div>
                  ))}
                </dl>
                {call.usage_inconsistent && (
                  <p className="hint">模型返回的用量数字不一致，无法确认完整合计。</p>
                )}
              </article>
            ))}
            <Pagination
              page={cursors.length}
              previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
              next={
                result.data.next_cursor
                  ? () => setCursors([...cursors, result.data!.next_cursor!])
                  : undefined
              }
            />
          </>
        )
      )}
    </section>
  )
}
function Tasks({ filter }: { filter: UsageFilter }) {
  const { session, prefix } = useIdentity()
  const [cursors, setCursors] = useState([''])
  const [selected, setSelected] = useState<UsageTask>()
  const result = useQuery({
    queryKey: [...prefix, 'usage', filter, 'tasks', cursors.at(-1)],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, filter.space_id).GET('/v1/learning/usage/tasks', {
          params: { query: { ...filter, cursor: cursors.at(-1), limit: 10 } },
          signal,
        }),
        usageTasks,
      ),
  })
  return (
    <>
      <section className="section" aria-label="用量任务列表">
        <h2>任务列表</h2>
        <p className="hint">每个任务包含筛选范围内的实际调用。查看或刷新页面不会增加调用次数。</p>
        {result.isPending && <p role="status">正在读取任务…</p>}
        {result.error ? (
          <ErrorState error={result.error} retry={() => void result.refetch()} />
        ) : (
          result.data && (
            <>
              {!result.data.items.length && <p>此范围内尚无记录。启用统计前的用量未记录。</p>}
              {result.data.items.map((task) => (
                <article className="panel compact" key={task.id}>
                  <h3>{taskLabel(task.kind)} · {when(task.started_at)}</h3>
                  <p>
                    来自{{ web: '网页', cli: 'CLI 服务端任务', server: '后台任务' }[task.source]}
                  </p>
                  <Totals value={task.totals} />
                  <div className="actions">
                    <Button variant="outline" onClick={() => setSelected(task)}>
                      查看调用明细
                    </Button>
                    {task.run_id && task.space_id && (
                      <Link
                        to="/runs/run/$runId"
                        params={{ runId: task.run_id }}
                        search={{ space: task.space_id }}
                      >
                        查看原任务
                      </Link>
                    )}
                  </div>
                </article>
              ))}
              <Pagination
                page={cursors.length}
                previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
                next={
                  result.data.next_cursor
                    ? () => setCursors([...cursors, result.data!.next_cursor!])
                    : undefined
                }
              />
            </>
          )
        )}
      </section>
      {!result.error && selected && (
        <Calls
          key={selected.id}
          task={selected}
          filter={filter}
          close={() => setSelected(undefined)}
        />
      )}
    </>
  )
}
export function UsagePage() {
  const { session, prefix } = useIdentity()
  const cache = useQueryClient()
  const [space, setSpace] = useState('')
  const [model, setModel] = useState('')
  const [grain, setGrain] = useState<UsageFilter['grain']>('day')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [now, setNow] = useState(() => new Date().toISOString())
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
  const filter: UsageFilter = {
    from: from ? new Date(`${from}T00:00:00`).toISOString() : '1970-01-01T00:00:00Z',
    to: to ? new Date(`${to}T00:00:00`).toISOString() : now,
    timezone,
    grain,
    model: model || undefined,
    space_id: space || undefined,
  }
  const valid = new Date(filter.from) < new Date(filter.to)
  const spaces = useQuery({
    queryKey: [...prefix, 'usage-spaces'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning-spaces', {
          params: { query: { limit: 100 } },
          signal,
        }),
        pageOf(spaceSchema),
      ),
  })
  const summary = useQuery({
    queryKey: [...prefix, 'usage', filter, 'summary'],
    gcTime: 0,
    enabled: valid,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, filter.space_id).GET('/v1/learning/usage', {
          params: { query: filter },
          signal,
        }),
        usageSummary,
      ),
  })
  return (
    <>
      <section className="intro">
        <span className="eyebrow">真实模型用量</span>
        <h1>用量</h1>
        <p>
          {summary.data?.coverage === 'server'
            ? '统计全部服务端调用，包括导师、Studio、研究、评估、知识处理、教学和模型连接探测；CLI 本地直连不统计。'
            : '当前仅统计导师对话；CLI 本地直连不统计。'}
        </p>
      </section>
      <p className="hint">
        推理 Token
        是输出的子项，不重复加入合计。模型未报告的字段显示“未知”；有未知项时仅展示已知小计。
      </p>
      <form className="panel filters" onSubmit={(e) => e.preventDefault()}>
        <label>
          学习区范围
          <select value={space} onChange={(e) => setSpace(e.target.value)}>
            <option value="">全部学习区（全局）</option>
            {spaces.data?.items.map((s) => (
              <option value={s.id} key={s.id}>
                {s.name}
                {s.status === 'archived' ? '（已归档）' : ''}
              </option>
            ))}
          </select>
        </label>
        <label>
          时间分组
          <select value={grain} onChange={(e) => setGrain(e.target.value as UsageFilter['grain'])}>
            <option value="day">按日</option>
            <option value="week">按周</option>
            <option value="month">按月</option>
          </select>
        </label>
        <label>
          模型筛选
          <select value={model} onChange={(e) => setModel(e.target.value)}>
            <option value="">全部模型</option>
            {Array.from(
              new Set([
                ...(summary.data?.models.map((m) => m.model) ?? []),
                ...(model ? [model] : []),
              ]),
            ).map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
        <label>
          开始日期
          <input type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
        </label>
        <label>
          截止日期（不含）
          <input type="date" value={to} onChange={(e) => setTo(e.target.value)} />
        </label>
        <Button
          variant="outline"
          onClick={() => {
            setNow(new Date().toISOString())
            void cache.invalidateQueries({ queryKey: [...prefix, 'usage'] })
          }}
        >
          刷新用量
        </Button>
        <p className="hint">
          时区：{timezone}
          。从开始日期零点起，至截止日期零点前；周从周一开始。不选日期时，显示启用统计至本次刷新时间的累计。
        </p>
      </form>
      {spaces.error && <ErrorState error={spaces.error} retry={() => void spaces.refetch()} />}
      {!valid ? (
        <p role="alert">截止日期必须晚于开始日期。</p>
      ) : summary.error ? (
        <ErrorState error={summary.error} retry={() => void summary.refetch()} />
      ) : summary.data ? (
        <>
          <p>统计起始时间：{when(summary.data.coverage_since)}。更早的用量未记录。</p>
          {summary.data.full_coverage_since && (
            <p>全覆盖起点：{when(summary.data.full_coverage_since)}。此前仅记录导师对话。</p>
          )}
          <p className="hint">仅显示当前保留的记录；隐私清除会一并移除用量明细和累计。</p>
          <p className="hint">
            本次范围：{from ? when(filter.from) : '启用统计时'} 至 {when(filter.to)}（不含）。
          </p>
          <section className="panel" aria-label="所选范围用量">
            <h2>{space ? '学习区累计' : '全局累计'}</h2>
            <Totals value={summary.data.totals} />
          </section>
          <section className="section" aria-label="时间分布">
            <h2>时间分布</h2>
            <div className="usage-table">
              <table>
                <thead>
                  <tr>
                    <th>开始日期</th>
                    <th>调用次数</th>
                    <th>输入</th>
                    <th>输出</th>
                    <th>其中推理</th>
                    <th>合计</th>
                  </tr>
                </thead>
                <tbody>
                  {summary.data.buckets.map((b) => (
                    <tr key={b.start}>
                      <th>{new Date(b.start).toLocaleDateString('zh-CN')}</th>
                      <td>{count(b.calls)}</td>
                      {(['input', 'output', 'reasoning', 'total'] as const).map((key) => (
                        <td key={key}>{metricText(b[key])}</td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {!summary.data.buckets.length && <p>此范围内尚无调用记录。</p>}
          </section>
          <Tasks key={JSON.stringify(filter)} filter={filter} />
        </>
      ) : (
        <p role="status">正在读取用量…</p>
      )}
    </>
  )
}
