import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ApiError, learningClient, unwrap } from './api/client'
import {
  carryoverPage,
  carryoverSchema,
  conclusionLabels,
  dispositionLabels,
  feedbackPage,
  feedbackSchema,
  feedbackStatus,
  quoteEvidence,
  riskLabels,
  type Carryover,
  type DecisionRequest,
  type Feedback,
} from './api/feedback'
import { helpLabels, operationReceipt, operationResult } from './api/teaching'
import { useIdentity } from './lib/session'
import { Confirm, ErrorState, Pagination } from './components/common'
import { Button } from './components/ui/button'
import { SafeMarkdown } from './components/content-blocks'

export function FeedbackListPage({ spaceId }: { spaceId: string }) {
  const { session, prefix } = useIdentity()
  const [status, setStatus] = useState<'all' | 'pending' | 'provisional'>('all')
  const [cursors, setCursors] = useState([''])
  const records = useQuery({
    queryKey: [...prefix, spaceId, 'feedback-list', status, cursors.at(-1)],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/assessments', {
          params: { query: { status, cursor: cursors.at(-1), limit: 20 } },
          signal,
        }),
        feedbackPage,
      ),
  })
  return (
    <section className="panel section">
      <h1>评估与证据</h1>
      <p>查看原答案、真实评估和处置历史。模型建议经正式接纳后才可能成为学习证据。</p>
      <label>
        评估筛选
        <select
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as typeof status)
            setCursors([''])
          }}
        >
          <option value="all">全部记录</option>
          <option value="pending">待评估</option>
          <option value="provisional">待复核</option>
        </select>
      </label>
      <Button
        variant="outline"
        onClick={() => {
          setCursors([''])
          void records.refetch()
        }}
      >
        刷新记录
      </Button>
      {records.error ? (
        <ErrorState error={records.error} />
      ) : records.data ? (
        <>
          {records.data.items.length === 0 && <p>当前筛选下没有记录。</p>}
          {records.data.items.map((item) => (
            <div className="session-row" key={item.attempt_id}>
              <Link
                to="/spaces/$spaceId/feedback/$attemptId"
                params={{ spaceId, attemptId: item.attempt_id }}
              >
                {item.received_at} ·{' '}
                {item.disposition
                  ? dispositionLabels[item.disposition]
                  : feedbackStatus[item.status]}
              </Link>
            </div>
          ))}
          <Pagination
            page={cursors.length}
            previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
            next={
              records.data.next_cursor
                ? () => setCursors([...cursors, records.data!.next_cursor!])
                : undefined
            }
          />
        </>
      ) : (
        <p role="status">正在读取原作答记录…</p>
      )}
      <Carryovers spaceId={spaceId} />
    </section>
  )
}

export function FeedbackPage({ spaceId, attemptId }: { spaceId: string; attemptId: string }) {
  const { session, prefix } = useIdentity()
  const feedback = useQuery({
    queryKey: [...prefix, spaceId, attemptId, 'feedback'],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session, spaceId).GET('/v1/learning/attempts/{attemptID}/feedback', {
          params: { path: { attemptID: attemptId } },
          signal,
        }),
        feedbackSchema,
      ),
  })
  if (feedback.error)
    return <ErrorState error={feedback.error} retry={() => void feedback.refetch()} />
  if (!feedback.data) return <p role="status">正在读取原答案与评估…</p>
  const view = feedback.data
  if (view.learning_space_id !== spaceId || view.attempt.attempt_id !== attemptId)
    return <ErrorState error={new ApiError(502, 'invalid_response')} />
  const decision = view.decisions.at(-1)
  return (
    <article className="panel section feedback-detail">
      <Link to="/spaces/$spaceId/feedback" params={{ spaceId }}>
        返回评估与证据
      </Link>
      <h1>评估详情</h1>
      <p role="status">
        {feedbackStatus[view.status]}
        {decision && ` · ${dispositionLabels[decision.disposition]}`}
      </p>
      <div className="actions">
        <Button variant="outline" onClick={() => void feedback.refetch()}>
          核对最新状态
        </Button>
        <Link
          to="/spaces/$spaceId/learn/$sessionId"
          params={{ spaceId, sessionId: view.attempt.session_id }}
        >
          返回原教学会话
        </Link>
      </div>
      <section aria-label="接收回执">
        <h2>答案接收回执</h2>
        <p>接收时间：{view.receipt.received_at}</p>
        <p>
          原操作：{view.receipt.operation_id} · 事件序号：{view.receipt.event_seq}
        </p>
      </section>
      <div className="feedback-columns">
        <section aria-label="原题与标准">
          <h2>原题与标准</h2>
          <SafeMarkdown text={view.activity.prompt} />
          {view.activity.rubric.items.map((item) => (
            <p key={item.rubric_item_id}>
              {item.rubric_item_id}：{item.criterion}
            </p>
          ))}
        </section>
        <section aria-label="原答案与帮助">
          <h2>原答案与帮助</h2>
          <p className="answer-text">{view.attempt.answer}</p>
          <p>帮助等级：{helpLabels[view.attempt.help]}</p>
          <p>
            证据资格：
            {view.attempt.evidence_eligibility ? '具有作答资格，仍需通过正式接纳政策' : '不具备'}
            {view.attempt.evidence_ineligible_reason &&
              ` · ${view.attempt.evidence_ineligible_reason}`}
          </p>
          <p className="hint">
            提示、脚手架和答案揭示按原记录保留。高帮助不自动提升掌握；自述已会、阅读解释或点击继续也不是能力证据。
          </p>
        </section>
      </div>
      <details>
        <summary>原版本与来源</summary>
        <p>
          目标：{view.goal_revision.text} · 第 {view.goal_revision.revision} 版 ·{' '}
          {view.goal_revision.goal_revision_id}
        </p>
        <p>
          活动：{view.activity.activity_id} · 第 {view.activity.revision} 版
        </p>
        <p>
          评分标准：{view.activity.rubric.rubric_revision} · 政策：
          {view.activity.assessment_policy_version}
        </p>
        <p>
          路线：{view.activity.route_revision_id} · 步骤：{view.activity.route_step_id}
        </p>
        <p>知识版本：{view.activity.knowledge_revision_id}</p>
        <p>原 context：{view.knowledge_context_revision_id ?? '旧活动未保存此关联'}</p>
        {view.content ? (
          <p>
            作答内容：
            <Link
              to="/content/$artifactId"
              params={{ artifactId: view.content.artifact_id }}
              search={{ space: spaceId, version: view.content.version }}
            >
              {view.content.artifact_id} · 第 {view.content.version} 版
            </Link>
          </p>
        ) : (
          <p>此答案没有保存内容版本关联，无法确认当时的展示版本；原题及评分标准仍可查。</p>
        )}
        {view.activity.knowledge_references.map((ref) => (
          <section key={ref.node_revision_id}>
            <h3>来源 {ref.node_revision_id}</h3>
            <p>
              文档版本：{ref.document_revision_id ?? '未提供'} · 知识版本：
              {ref.knowledge_revision_id}
            </p>
            <blockquote>{ref.slice}</blockquote>
          </section>
        ))}
      </details>
      <section aria-label="讲解反馈">
        <h2>讲解反馈</h2>
        <p>解释与讨论保留在原教学会话，不单独证明能力。以下逐项引文来自本次正式评估。</p>
      </section>
      <section aria-label="确定规则核验">
        <h2>确定规则核验</h2>
        {view.activity.rubric.objective_rule ? (
          <>
            <p>仅按原字符串规则匹配，不执行数学等价、代码测试或口语评估。</p>
            <p>
              接受字符串：{view.activity.rubric.objective_rule.accepted_answers.join('、')}；
              {view.activity.rubric.objective_rule.case_sensitive ? '区分' : '不区分'}大小写；
              {view.activity.rubric.objective_rule.trim_space ? '去除' : '保留'}首尾空格。
            </p>
            <p>
              正式规则结果：
              {view.evidence
                .map(
                  (e) => conclusionLabels[e.outcome as keyof typeof conclusionLabels] ?? e.outcome,
                )
                .join('、') || '尚无有效 Evidence，不能据答案字符串自行认定已接纳'}
            </p>
          </>
        ) : (
          <p>本活动没有确定性规则核验。</p>
        )}
      </section>
      <section aria-label="模型评分建议">
        <h2>模型评分建议</h2>
        {view.assessment && view.activity.type !== 'objective' ? (
          <>
            <p>
              模型：{view.assessment.model_id} · 提示版本：{view.assessment.prompt_revision}
            </p>
            <p>
              标准覆盖：{view.assessment.rubric_complete ? '完整' : '不完整'}；模型不确定性：
              {view.assessment.confidence < 850
                ? '未达到自动接纳门槛'
                : '达到模型门槛，仍须核验引文与风险'}
              。
            </p>
            {view.assessment.items.map((item) => (
              <section key={item.rubric_item_id}>
                <h3>
                  {item.rubric_item_id} · {conclusionLabels[item.conclusion]}
                </h3>
                <p>答案依据：{item.answer_quote || '缺失'}</p>
                <p>来源依据：{item.knowledge_quote || '缺失'}</p>
                <p className="hint">来源版本：{item.knowledge_reference_id || '缺失'}</p>
              </section>
            ))}
          </>
        ) : (
          <p>{view.assessment ? '本次使用确定规则，没有模型评分。' : '尚未形成正式评估。'}</p>
        )}
        {[...new Set([...view.reasons, ...(view.assessment?.risk_flags ?? [])])].map((reason) => (
          <p className="notice" key={reason}>
            {riskLabels[reason] ?? reason}
          </p>
        ))}
      </section>
      <section aria-label="人工处置与历史">
        <h2>正式处置与人工复核</h2>
        {view.decisions.map((d) => (
          <details key={d.decision_id}>
            <summary>
              第 {d.version} 次 · {dispositionLabels[d.disposition]} ·{' '}
              {d.version === 1 ? '初始政策处置' : '人工处置'}
            </summary>
            <p>{d.reason || '初始接纳政策结果'}</p>
            <p>
              时间：{d.created_at} · 设备：{d.actor_device_id}
            </p>
            {d.items.map((i) => (
              <p key={i.rubric_item_id}>
                {i.rubric_item_id}：{conclusionLabels[i.conclusion]} · {i.answer_quote} ·{' '}
                {i.knowledge_quote}
              </p>
            ))}
          </details>
        ))}
        {view.assessment && (
          <AssessmentActions
            key={`${view.attempt.attempt_id}:${decision?.version}`}
            view={view}
            refresh={async () => {
              const result = await feedback.refetch()
              if (result.error) throw result.error
            }}
          />
        )}
      </section>
      <section aria-label="正式学习证据">
        <h2>正式学习证据（有效 Evidence）</h2>
        {view.evidence.length ? (
          view.evidence.map((e) => (
            <div key={e.evidence_id}>
              <p>
                {e.evidence_id} ·{' '}
                {conclusionLabels[e.outcome as keyof typeof conclusionLabels] ?? e.outcome}
              </p>
              <p>
                原帮助：{helpLabels[e.help as keyof typeof helpLabels] ?? e.help} · 接纳事件：
                {e.accepted_event_seq} · 政策：{e.acceptance_policy_version}
              </p>
            </div>
          ))
        ) : (
          <p>没有有效的正式学习证据。</p>
        )}
        <p className="hint">
          Evidence 被接纳也不等于已经掌握；结果、帮助和历史由既有学习政策统一解释。
        </p>
      </section>
    </article>
  )
}

type Correction = {
  conclusion: 'pass' | 'partial' | 'fail'
  answer: string
  reference: string
  quote: string
}
function AssessmentActions({ view, refresh }: { view: Feedback; refresh: () => Promise<void> }) {
  const { session, prefix, drafts } = useIdentity()
  const decision = view.decisions.at(-1)!
  const key = JSON.stringify([
    ...prefix,
    view.learning_space_id,
    view.attempt.attempt_id,
    'assessment-decision',
  ])
  const [pending, setPending] = useState(() => drafts.get<DecisionRequest>(key))
  const [reason, setReason] = useState('')
  const [kind, setKind] = useState<'confirm' | 'override' | 'void'>('void')
  const [corrections, setCorrections] = useState<Record<string, Correction>>(() =>
    Object.fromEntries(
      view.activity.rubric.items.map((r) => {
        const item = decision.items.find((i) => i.rubric_item_id === r.rubric_item_id)
        return [
          r.rubric_item_id,
          {
            conclusion:
              item?.conclusion === 'partial' || item?.conclusion === 'fail'
                ? item.conclusion
                : 'pass',
            answer: item?.answer_quote ?? '',
            reference: item?.knowledge_reference_id ?? '',
            quote: item?.knowledge_quote ?? '',
          },
        ]
      }),
    ),
  )
  const [prepared, setPrepared] = useState<DecisionRequest>()
  const [error, setError] = useState<unknown>()
  const [validation, setValidation] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const locked = useRef(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const client = () => learningClient(session, view.learning_space_id)
  const finish = async () => {
    drafts.delete(key)
    if (mounted.current) {
      setPending(undefined)
      setPrepared(undefined)
      setNotice('原处置已核对，正式记录已更新。')
      await refresh()
    }
  }
  const reconcile = async () => {
    if (!pending || locked.current) return
    locked.current = true
    setBusy(true)
    setError(undefined)
    try {
      const receipt = await unwrap(
        client().GET('/v1/tutoring/sessions/{sessionID}/operations/{operationID}', {
          params: {
            path: { sessionID: pending.aggregate_id, operationID: pending.operation_id },
            header: { 'X-Learning-Space-ID': view.learning_space_id },
          },
        }),
        operationReceipt,
      )
      if (receipt.status === 'succeeded') await finish()
      else if (receipt.status === 'rejected') {
        drafts.delete(key)
        setPending(undefined)
        setPrepared(undefined)
        setNotice('原处置已被拒绝，请刷新并比较记录后重新确认。')
        await refresh()
      }
    } catch (e) {
      if (mounted.current) {
        setError(e)
        setNotice('原操作尚无法确认。可继续核对，或按同一操作身份重试。')
      }
    } finally {
      locked.current = false
      if (mounted.current) setBusy(false)
    }
  }
  const submit = async (body: DecisionRequest) => {
    if (locked.current) return
    locked.current = true
    setBusy(true)
    setError(undefined)
    drafts.set(key, body)
    setPending(body)
    try {
      await unwrap(
        client().POST('/v1/learning/assessments/{assessmentID}/decisions', {
          params: { path: { assessmentID: view.assessment!.assessment_id } },
          body,
        }),
        operationResult,
      )
      await finish()
    } catch (e) {
      if (mounted.current) {
        setError(e)
        setNotice('处置结果尚未确认，请核对原操作。')
        if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
          drafts.delete(key)
          setPending(undefined)
          setPrepared(undefined)
          setNotice('处置未成功；请刷新并比较最新记录后重新确认。')
        }
      }
    } finally {
      locked.current = false
      if (mounted.current) setBusy(false)
    }
  }
  const prepare = async () => {
    setValidation('')
    setPrepared(undefined)
    if (!reason.trim()) {
      setValidation('请填写具体处置依据。')
      return
    }
    const base = {
      payload_schema_version: 1 as const,
      aggregate_type: 'session' as const,
      aggregate_id: view.attempt.session_id,
      expected_version: view.session_version,
      expected_disposition_version: decision.version,
      reason: reason.trim(),
    }
    try {
      let body: Omit<DecisionRequest, 'operation_id'> & {
        items?: import('./api/schema').components['schemas']['AssessmentItem'][]
      } = { ...base, kind }
      if (kind === 'override') {
        const items = await Promise.all(
          view.activity.rubric.items.map(async (r) => {
            const edit = corrections[r.rubric_item_id]
            const ref = view.activity.knowledge_references.find(
              (v) => v.node_revision_id === edit.reference,
            )
            if (
              !ref ||
              (r.required_reference_ids?.length &&
                !r.required_reference_ids.includes(ref.node_revision_id))
            )
              throw new Error('请选择此标准允许的原来源。')
            const answer = await quoteEvidence(view.attempt.answer, edit.answer),
              knowledge = await quoteEvidence(ref.slice, edit.quote)
            return {
              rubric_item_id: r.rubric_item_id,
              conclusion: edit.conclusion,
              answer_quote: edit.answer,
              answer_range: answer.range,
              answer_quote_sha256: answer.hash,
              knowledge_reference_id: ref.node_revision_id,
              knowledge_quote: edit.quote,
              knowledge_range: knowledge.range,
              knowledge_quote_sha256: knowledge.hash,
            }
          }),
        )
        body = { ...base, kind: 'override', items }
      }
      setPrepared({ ...body, operation_id: drafts.operation(key, body) } as DecisionRequest)
    } catch (e) {
      setValidation(e instanceof Error ? e.message : '请检查逐项依据。')
    }
  }
  const canApprove =
    session.device.scopes.includes('learning:write') &&
    session.device.scopes.includes('learning:approve')
  if (!canApprove) return <p>当前设备只有查看能力，复核需要 learning:write 与 learning:approve。</p>
  if (!view.allowed_decisions.length && !pending) return <p>此记录当前没有可用处置。</p>
  const labels = { confirm: '复核接纳', override: '覆盖评估', void: '作废评估' }
  return (
    <div className="section">
      {pending ? (
        <div className="notice">
          <p>原操作 {pending.operation_id} 的结果待核对。</p>
          <Button disabled={busy} onClick={() => void reconcile()}>
            核对原处置
          </Button>
          <Confirm
            label="重试原处置"
            title="按相同操作身份重试？"
            disabled={busy}
            onConfirm={() => void submit(pending)}
          >
            仍作用于原评估，不生成新的操作身份。
          </Confirm>
        </div>
      ) : (
        <>
          <label>
            处置动作
            <select
              value={kind}
              onChange={(e) => {
                setKind(e.target.value as typeof kind)
                setPrepared(undefined)
              }}
            >
              {view.allowed_decisions.map((k) => (
                <option key={k} value={k}>
                  {labels[k]}
                </option>
              ))}
            </select>
          </label>
          <label>
            处置原因
            <textarea
              maxLength={4000}
              value={reason}
              onChange={(e) => {
                setReason(e.target.value)
                setPrepared(undefined)
              }}
            />
          </label>
          {kind === 'override' &&
            view.activity.rubric.items.map((r) => {
              const edit = corrections[r.rubric_item_id]
              const update = (patch: Partial<Correction>) => {
                setCorrections({ ...corrections, [r.rubric_item_id]: { ...edit, ...patch } })
                setPrepared(undefined)
              }
              return (
                <fieldset key={r.rubric_item_id}>
                  <legend>{r.criterion}</legend>
                  <label>
                    修正结论
                    <select
                      value={edit.conclusion}
                      onChange={(e) =>
                        update({ conclusion: e.target.value as Correction['conclusion'] })
                      }
                    >
                      <option value="pass">达到要求</option>
                      <option value="partial">部分达到</option>
                      <option value="fail">尚未达到</option>
                    </select>
                  </label>
                  <label>
                    原答案引文
                    <textarea
                      value={edit.answer}
                      onChange={(e) => update({ answer: e.target.value })}
                    />
                  </label>
                  <label>
                    原来源版本
                    <select
                      value={edit.reference}
                      onChange={(e) => update({ reference: e.target.value, quote: '' })}
                    >
                      <option value="">请选择</option>
                      {view.activity.knowledge_references
                        .filter(
                          (ref) =>
                            !r.required_reference_ids?.length ||
                            r.required_reference_ids.includes(ref.node_revision_id),
                        )
                        .map((ref) => (
                          <option key={ref.node_revision_id} value={ref.node_revision_id}>
                            {ref.node_revision_id}
                          </option>
                        ))}
                    </select>
                  </label>
                  <label>
                    原来源引文
                    <textarea
                      value={edit.quote}
                      onChange={(e) => update({ quote: e.target.value })}
                    />
                  </label>
                </fieldset>
              )
            })}
          <Button disabled={busy || !reason.trim()} onClick={() => void prepare()}>
            检查处置内容
          </Button>
          {prepared && prepared.expected_version !== view.session_version && (
            <p role="alert">会话版本已变化，保留处置内容供比较；请重新检查内容。</p>
          )}
          {prepared && (
            <Confirm
              label={labels[kind]}
              title={`${labels[kind]}：原评估第 ${decision.version} 版？`}
              disabled={busy || prepared.expected_version !== view.session_version}
              onConfirm={() => void submit(prepared)}
            >
              <p>{reason}</p>
              <p>
                将追加{labels[kind]}事件；原题、答案与评分标准保留。会话或处置版本变化时拒绝提交。
              </p>
              {kind === 'override' &&
                Object.entries(corrections).map(([id, e]) => (
                  <p key={id}>
                    {id}：{conclusionLabels[e.conclusion]} · 答案：{e.answer} · 来源：{e.quote}
                  </p>
                ))}
            </Confirm>
          )}
        </>
      )}
      {validation && <p role="alert">{validation}</p>}
      {notice && <p role="status">{notice}</p>}
      {!!error && <ErrorState error={error} />}
    </div>
  )
}

function Carryovers({ spaceId }: { spaceId: string }) {
  const { session, prefix } = useIdentity()
  const [cursors, setCursors] = useState([''])
  const [status, setStatus] = useState<'open' | 'all'>('open')
  const defaultSpace = '00000000-0000-4000-8000-000000000001'
  const page = useQuery({
    queryKey: [...prefix, 'carryovers', status, cursors.at(-1)],
    gcTime: 0,
    enabled: spaceId === defaultSpace,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning/evidence-carryovers', {
          params: { query: { status, limit: 10, cursor: cursors.at(-1) } },
          signal,
        }),
        carryoverPage,
      ),
  })
  return (
    <section className="section">
      <h2>证据继承提案</h2>
      <p>继承审批按已有具体映射产生待验证关系，不复制 Evidence，也不自动迁移掌握度。</p>
      {spaceId !== defaultSpace ? (
        <p>
          既有继承协议仅支持默认学习区。
          <Link to="/spaces/$spaceId/feedback" params={{ spaceId: defaultSpace }}>
            查看默认区真实提案
          </Link>
        </p>
      ) : (
        <>
          <label>
            继承提案筛选
            <select
              value={status}
              onChange={(e) => {
                setStatus(e.target.value as typeof status)
                setCursors([''])
              }}
            >
              <option value="open">待审批</option>
              <option value="all">全部状态</option>
            </select>
          </label>
          <Button
            variant="outline"
            onClick={() => {
              setCursors([''])
              void page.refetch()
            }}
          >
            刷新继承提案
          </Button>
          {page.error ? (
            <ErrorState error={page.error} />
          ) : page.data ? (
            <>
              {page.data.items.length === 0 && <p>没有相应的继承提案。</p>}
              {page.data.items.map((p) => (
                <CarryoverCard key={p.proposal_id} initial={p} />
              ))}
              <Pagination
                page={cursors.length}
                previous={cursors.length > 1 ? () => setCursors(cursors.slice(0, -1)) : undefined}
                next={
                  page.data.next_cursor
                    ? () => setCursors([...cursors, page.data!.next_cursor!])
                    : undefined
                }
              />
            </>
          ) : (
            <p role="status">正在读取继承提案…</p>
          )}
        </>
      )}
    </section>
  )
}

function CarryoverCard({ initial }: { initial: Carryover }) {
  const { session, prefix, drafts } = useIdentity()
  const key = JSON.stringify([...prefix, initial.proposal_id, 'carryover-decision'])
  type Command = { operation_id: string; decision: 'approve' | 'reject'; reason: string }
  const [pending, setPending] = useState(() => drafts.get<Command>(key))
  const [reason, setReason] = useState('')
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const locked = useRef(false)
  const detail = useQuery({
    queryKey: [...prefix, 'carryover', initial.proposal_id],
    gcTime: 0,
    queryFn: ({ signal }) =>
      unwrap(
        learningClient(session).GET('/v1/learning/evidence-carryovers/{proposalID}', {
          params: { path: { proposalID: initial.proposal_id } },
          signal,
        }),
        carryoverSchema,
      ),
  })
  const proposal = detail.data ?? initial
  const labels = {
    open: '待审批',
    approved: '已批准待验证映射',
    rejected: '已拒绝',
    stale: '版本过期，只可比较',
    redacted: '已隐私清除',
  }
  const reconcile = async () => {
    const result = await detail.refetch()
    if (
      result.data?.decision?.operation_id === pending?.operation_id ||
      (result.data && result.data.status !== 'open')
    ) {
      drafts.delete(key)
      setPending(undefined)
    }
  }
  const submit = async (decision: 'approve' | 'reject', retry?: Command) => {
    if (locked.current) return
    locked.current = true
    setBusy(true)
    setError(undefined)
    const payload = { decision, reason: reason.trim() }
    const body = retry ?? { ...payload, operation_id: drafts.operation(key, payload) }
    drafts.set(key, body)
    setPending(body)
    try {
      await unwrap(
        learningClient(session).POST(
          decision === 'approve'
            ? '/v1/learning/evidence-carryovers/{proposalID}/approve'
            : '/v1/learning/evidence-carryovers/{proposalID}/reject',
          { params: { path: { proposalID: proposal.proposal_id } }, body },
        ),
        carryoverSchema,
      )
      drafts.delete(key)
      setPending(undefined)
      await detail.refetch()
    } catch (e) {
      setError(e)
      await reconcile()
    } finally {
      locked.current = false
      setBusy(false)
    }
  }
  if (detail.error) return <ErrorState error={detail.error} retry={() => void detail.refetch()} />
  return (
    <article className="panel section">
      <h3>继承提案 · {labels[proposal.status]}</h3>
      <p>{proposal.proposal_id}</p>
      {!proposal.redacted && (
        <>
          <p>原证据：{proposal.source_evidence_id}</p>
          <p>
            来源知识/节点版本：{proposal.source_knowledge_revision_id} /{' '}
            {proposal.source_node_revision_id}
          </p>
          <p>目标知识版本：{proposal.target_knowledge_revision_id}</p>
          <p>
            原知识提案：{proposal.knowledge_proposal_id} · 政策：{proposal.policy_version}
          </p>
          <ul>
            {proposal.candidates?.map((c) => (
              <li key={c.node_revision_id}>
                映射到节点 {c.node_id}，节点版本 {c.node_revision_id}，文档版本{' '}
                {c.document_revision_id}
              </li>
            ))}
          </ul>
          {proposal.decision && (
            <p>
              处置依据：{proposal.decision.reason} · 结果：{proposal.decision.outcome}
            </p>
          )}
        </>
      )}
      {pending && (
        <div className="notice">
          <p>结果待核对，原操作：{pending.operation_id}</p>
          <Button disabled={busy} onClick={() => void reconcile()}>
            核对原继承提案
          </Button>
          <Confirm
            label="重试原审批"
            title="按同一操作身份重试？"
            disabled={busy}
            onConfirm={() => void submit(pending.decision, pending)}
          >
            保留原提案、原因及映射；不会复制 Evidence。
          </Confirm>
        </div>
      )}
      {detail.data &&
        !pending &&
        proposal.status === 'open' &&
        !proposal.redacted &&
        session.device.scopes.includes('learning:approve') && (
          <>
            <label>
              继承处置原因
              <textarea
                value={reason}
                maxLength={4000}
                onChange={(e) => setReason(e.target.value)}
              />
            </label>
            <Confirm
              label="批准继承"
              title="批准上述具体映射？"
              disabled={busy || !reason.trim() || !proposal.candidates?.length}
              onConfirm={() => void submit('approve')}
            >
              <p>{reason}</p>
              <p>
                来源 {proposal.source_evidence_id}，目标 {proposal.target_knowledge_revision_id}
                。逐项映射将由服务端重新核对版本，过期不会获批。
              </p>
              {proposal.candidates?.map((c) => (
                <p key={c.node_revision_id}>
                  {c.node_revision_id} · {c.document_revision_id}
                </p>
              ))}
            </Confirm>
            <Confirm
              label="拒绝继承"
              title="拒绝此继承提案？"
              disabled={busy || !reason.trim()}
              onConfirm={() => void submit('reject')}
            >
              {reason}
            </Confirm>
          </>
        )}
      {!!error && <ErrorState error={error} />}
    </article>
  )
}
