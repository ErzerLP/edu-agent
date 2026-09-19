import { useEffect, useRef, useState } from 'react'
import { Button } from './components/ui/button'
import {
  answerOperation,
  readPack,
  supportsAnswer,
  verifyPack,
  verifyRoot,
  type Item,
  type Selection,
} from './offline/protocol'
import {
  allowPersistence,
  broadcast,
  observe,
  presence,
  support,
  Vault,
  type QueueEntry,
  type VaultState,
} from './offline/vault'
import {
  call,
  discard,
  download,
  enroll,
  OfflineError,
  purgeIfRequired,
  readOfflineSession,
  synchronize,
  type Result,
} from './offline/sync'
import { saveShell } from './offline/shell'

const labels: Record<QueueEntry['state'], string> = {
  queued: '已保存未同步',
  unknown: '同步结果未知',
  confirmed: '服务端已确认',
  rejected: '服务端已拒绝',
  blocked: '同步受阻，待处置',
  conflict: '内容或操作冲突，待处置',
}
const evidence: Record<string, string> = {
  accepted: '服务端已接纳学习证据',
  provisional: '未形成正式证据，需查看原因或人工复核',
  pending_evaluation: '服务端待评估',
  not_eligible: '不具备学习证据资格',
  not_applicable: '不适用学习证据',
  unchanged: '学习证据未改变',
}
function selectionFromURL(): Selection | undefined {
  const params = new URLSearchParams(location.search)
  const values = ['space', 'goal', 'session'].map((key) => params.get(key) ?? '')
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
  const version = params.get('version') ?? ''
  if (!values.every((value) => uuid.test(value)) || !/^[1-9][0-9]*$/.test(version)) return undefined
  return {
    spaceId: values[0],
    goalId: values[1],
    sessionId: values[2],
    sessionVersion: version,
    name: '原教学会话',
  }
}
function Answer({
  item,
  saved,
  expired,
  onSave,
}: {
  item: Item
  saved?: QueueEntry
  expired: boolean
  onSave: (answer: string) => Promise<void>
}) {
  const [answer, setAnswer] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    if (!answer) return
    const beforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', beforeUnload)
    return () => window.removeEventListener('beforeunload', beforeUnload)
  }, [answer])
  if (saved) {
    const result: Result | undefined = saved.result ? JSON.parse(saved.result) : undefined
    return (
      <section aria-label="原提交状态">
        <p role="status">{labels[saved.state]}</p>
        <details>
          <summary>核对原答案与操作</summary>
          <pre className="offline-text">{JSON.parse(saved.operation).payload.answer}</pre>
          <p>操作：{item.authorization.payload.operation_id}</p>
          <p>提交：{item.authorization.payload.submission_id}</p>
        </details>
        {result && (
          <>
            <p>{evidence[result.evidence_status ?? 'unchanged']}</p>
            <p>服务端原因：{result.reason_codes.join('、') || '无'}</p>
            {result.ingest_receipt && <p>原接收回执：{result.ingest_receipt.receipt_id}</p>}
          </>
        )}
      </section>
    )
  }
  if (!supportsAnswer(item)) return <p>未知作答协议：安全只读，不允许提交。</p>
  if (expired)
    return (
      <p>
        已达到本地可开始期限或检测到时钟回退。保持只读；已保存的原提交仍可联网核对，由服务端裁决。
      </p>
    )
  return (
    <form
      onSubmit={async (event) => {
        event.preventDefault()
        setBusy(true)
        setError('')
        try {
          await onSave(answer)
          setAnswer('')
        } catch (error) {
          setError(error instanceof Error ? error.message : '本次答案未保存')
        } finally {
          setBusy(false)
        }
      }}
    >
      <label>
        离线答案
        <textarea
          value={answer}
          onChange={(event) => setAnswer(event.target.value)}
          rows={5}
          maxLength={65536}
          disabled={busy}
        />
      </label>
      <p>
        {answer
          ? '当前输入尚未保存，关闭页面可能丢失。'
          : '离线只保存答案，不评分、不调用模型，不推进在线课堂。'}
      </p>
      {error && <p role="alert">未保存：{error}</p>}
      <Button type="submit" disabled={busy || !answer.trim()}>
        {busy ? '正在写入并核对…' : '加密保存原答案'}
      </Button>
      <p className="hint">保存后答案不可修改。服务器接纳之前不属于正式学习证据。</p>
    </form>
  )
}
export function OfflinePage() {
  const [status, setStatus] = useState<'checking' | 'present' | 'absent' | 'lost'>('checking')
  const [state, setState] = useState<VaultState>()
  const vault = useRef<Vault | undefined>(undefined)
  const epoch = useRef(0)
  const [password, setPassword] = useState('')
  const [repeat, setRepeat] = useState('')
  const [consent, setConsent] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [enabled, setEnabled] = useState(false)
  const [online, setOnline] = useState(navigator.onLine)
  const selection = selectionFromURL()
  const lock = () => {
    epoch.current++
    vault.current?.lock()
    vault.current = undefined
    setState(undefined)
    setPassword('')
    setRepeat('')
  }
  const refresh = async () => {
    const current = vault.current
    const started = epoch.current
    if (!current) return
    const next = await current.read()
    await verifyRoot(next.owner)
    for (const pack of next.packs) await verifyPack(pack, next.owner)
    if (started === epoch.current) setState(next)
  }
  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await fn()
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : '操作未完成；请核对存储和网络状态')
      if (
        e instanceof OfflineError &&
        ['offline_purge_required', 'content_redacted', 'privacy_clear_in_progress'].includes(e.code)
      ) {
        lock()
        broadcast('lock')
        await reconnect()
      }
    } finally {
      setBusy(false)
    }
  }
  const reconnect = async () => {
    let cleaning = false
    try {
      const session = await readOfflineSession()
      if (!session.content_allowed || session.purge) {
        cleaning = true
        const current = vault.current
        lock()
        setNotice('服务端要求清除：正在清理本地库并核对正式回执…')
        await purgeIfRequired(session, current)
        setStatus('absent')
        setNotice('本地受管数据已清除，正式清除回执已核对。')
      } else if (vault.current) {
        await synchronize(vault.current)
        await refresh()
      }
    } catch (e) {
      if (
        e instanceof OfflineError &&
        ['offline_purge_required', 'content_redacted', 'privacy_clear_in_progress'].includes(e.code)
      ) {
        lock()
        broadcast('lock')
        setNotice('正文权限已关闭，离线库已锁定；请核对待清除任务。')
      }
      if (cleaning) setNotice('清除或正式回执尚未完成，请联网核对后重试。')
      if (vault.current || cleaning) setError(e instanceof Error ? e.message : '重连核对未完成')
    }
  }
  useEffect(() => {
    void support()
      .then(presence)
      .then(setStatus)
      .catch((e) => {
        setError(e.message)
        setStatus('lost')
      })
    void call('capabilities')
      .then((raw) => setEnabled(JSON.parse(raw).enabled === true))
      .catch(() => {})
    if (navigator.onLine) void reconnect()
    const changed = () => {
      setOnline(navigator.onLine)
      if (navigator.onLine) void reconnect()
    }
    const stop = observe((type) => {
      if (type === 'changed')
        void refresh().catch((e) => {
          lock()
          setError(e.message)
        })
      else {
        lock()
        if (type === 'purged') {
          setStatus('absent')
          setNotice('其他标签页已开始清除本地库。')
        }
      }
    })
    window.addEventListener('online', changed)
    window.addEventListener('offline', changed)
    return () => {
      stop()
      window.removeEventListener('online', changed)
      window.removeEventListener('offline', changed)
      vault.current?.lock()
    }
  }, [])
  useEffect(() => {
    if (!state) return
    const timer = setTimeout(() => {
      lock()
      broadcast('lock')
      setNotice('解锁已满 15 分钟，已自动锁定。')
    }, 15 * 60_000)
    return () => clearTimeout(timer)
  }, [!!state])
  const remaining =
    state?.queue.filter((q) => q.state !== 'confirmed' && q.state !== 'rejected').length ?? 0
  return (
    <main className="main offline-page">
      <a href="/app/">返回在线学习</a>
      <h1>加密离线学习</h1>
      <p role="status">
        {online
          ? '网络显示已连接；同步结果以原操作回执为准。'
          : '当前断网：已下载内容可阅读，保存不等于同步。'}
      </p>
      <p>
        浏览器可能清理存储；未同步答案没有服务器备份。口令或密钥丢失无法恢复。离线设备无法即时获知撤销或被远程擦除，重连时核对清除任务。
      </p>
      {error && <p role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {!state && (
        <section className="panel">
          <h2>{status === 'present' ? '解锁本地离线库' : '启用本地离线库'}</h2>
          {status === 'checking' && <p>正在检查本地存储能力…</p>}
          {status === 'lost' && (
            <p>
              存储支持不足，或发现离线库/标记部分丢失。不会以空库覆盖；如曾保存，未同步内容可能已丢失。
            </p>
          )}
          {status === 'absent' && (
            <p>
              未发现离线库。如果以前保存过，本浏览器数据可能已全部被清理；无法证明旧答案仍在。新库不会恢复旧内容。
            </p>
          )}
          {(status === 'absent' || status === 'present') && (
            <form
              onSubmit={(event) => {
                event.preventDefault()
                const secret = password
                const confirmation = repeat
                setPassword('')
                setRepeat('')
                void run(async () => {
                  const started = epoch.current
                  let opened: Vault
                  if (status === 'present') opened = await Vault.unlock(secret)
                  else {
                    if (!consent || secret !== confirmation || [...secret].length < 12)
                      throw new Error('请明确同意，并输入两次相同的至少 12 字符独立长口令')
                    await allowPersistence()
                    const owner = await enroll()
                    await saveShell()
                    opened = await Vault.create(secret, owner)
                  }
                  if (started !== epoch.current) {
                    opened.lock()
                    throw new Error('操作期间库已被其他标签页锁定或清除')
                  }
                  vault.current = opened
                  setStatus('present')
                  if (navigator.onLine) await reconnect()
                })
              }}
            >
              <label>
                解锁口令
                <input
                  type="password"
                  autoComplete="off"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  required
                  minLength={status === 'absent' ? 12 : undefined}
                />
              </label>
              {status === 'absent' && (
                <>
                  <label>
                    再次输入口令
                    <input
                      type="password"
                      autoComplete="off"
                      value={repeat}
                      onChange={(e) => setRepeat(e.target.value)}
                      required
                      minLength={12}
                    />
                  </label>
                  <label>
                    <input
                      type="checkbox"
                      checked={consent}
                      onChange={(e) => setConsent(e.target.checked)}
                    />
                    允许保存所选包、答案和队列，并保留最多 37
                    天的原设备专用同步身份；了解浏览器存储和口令丢失风险。
                  </label>
                </>
              )}
              <Button
                type="submit"
                disabled={busy || (status === 'absent' && (!enabled || !consent))}
              >
                {status === 'present' ? '解锁' : '授权并创建加密离线库'}
              </Button>
              {status === 'absent' && !enabled && (
                <p>服务器尚未启用浏览器离线功能，或目前无法联网核对。</p>
              )}
            </form>
          )}
        </section>
      )}
      {state && (
        <>
          <section className="panel">
            <h2>离线包列表</h2>
            <p>
              存储状态：已解锁 · 未同步或待处置 {remaining} 条 · 已保存 {state.packs.length} 包
            </p>
            <p>
              原设备：{state.owner.deviceId}；同步身份到期：
              {new Date(state.owner.expiresAt).toLocaleString()}
            </p>
            <Button
              onClick={() => {
                lock()
                broadcast('lock')
                setNotice('所有标签页的离线库已锁定。')
              }}
            >
              锁定离线库
            </Button>
            <Button
              disabled={busy || !online}
              onClick={() =>
                void run(async () => {
                  await synchronize(vault.current!)
                  setNotice('原操作核对完成，请查看各条服务端回执。')
                })
              }
            >
              核对原操作并同步
            </Button>
            {state.pending ? (
              <>
                <p>下载结果未知：已保存原请求。恢复只重试该请求。</p>
                <Button
                  disabled={busy || !online}
                  onClick={() => void run(() => download(vault.current!))}
                >
                  核对并恢复原下载
                </Button>
              </>
            ) : selection ? (
              <>
                <p>
                  明确下载：学习区 {selection.spaceId}，目标 {selection.goalId}，教学会话{' '}
                  {selection.sessionId}（版本 {selection.sessionVersion}
                  ）。包冻结当时上下文，离线只显示已取得的资料。本次明确下载将原设备专用同步身份续期至最多
                  37 天。
                </p>
                <Button
                  disabled={busy || !online || !enabled}
                  onClick={() =>
                    void run(async () => {
                      await download(vault.current!, selection)
                      setNotice('签名包已加密保存并回读核对，可以断网阅读。')
                    })
                  }
                >
                  下载并加密保存此会话的离线包
                </Button>
              </>
            ) : (
              <p>请从在线课堂的“下载离线学习包”选择明确目标与会话，再回来保存。</p>
            )}
          </section>
          {state.packs.map((saved) => {
            const pack = readPack(saved.response).pack.payload
            const now = Math.max(Date.now(), state.clockFloor, state.serverTime)
            const expired =
              now >= Date.parse(pack.eligible_until) || Date.now() + 60_000 < state.clockFloor
            return (
              <section className="panel" key={pack.pack_id}>
                <h2>离线包 {pack.pack_id}</h2>
                <p>
                  原学习区 {saved.intent.selection.spaceId} · 原目标 {saved.intent.selection.goalId}{' '}
                  · 原会话 {pack.parent_session_id}
                </p>
                <p>
                  断网可阅读 · 本地开始有效期至 {new Date(pack.eligible_until).toLocaleString()}
                  （约剩{' '}
                  {Math.max(0, Math.floor((Date.parse(pack.eligible_until) - now) / 3600000))}{' '}
                  小时）· 服务端审计期限至 {new Date(pack.archive_until).toLocaleString()}
                </p>
                {pack.truncated && <p>服务端只签发了部分活动：{pack.truncated_reason}</p>}
                {pack.items.map((item) => (
                  <article key={item.authorization.payload.operation_id}>
                    <h3>已签发活动（版本 {item.activity.revision}）</h3>
                    <pre className="offline-text">{item.activity.prompt}</pre>
                    <p>
                      知识版本：{item.activity.knowledge_revision_id}；目标修订：
                      {item.activity.goal_revision_id}
                    </p>
                    <details>
                      <summary>冻结标准与已取得资料</summary>
                      {item.activity.rubric.items.map((r) => (
                        <p key={r.rubric_item_id}>{r.criterion}</p>
                      ))}
                      {item.activity.knowledge_references.map((ref, index) => (
                        <pre className="offline-text" key={index}>
                          {ref.slice}
                        </pre>
                      ))}
                    </details>
                    <Answer
                      item={item}
                      expired={expired}
                      saved={state.queue.find(
                        (q) =>
                          JSON.parse(q.operation).operation_id ===
                          item.authorization.payload.operation_id,
                      )}
                      onSave={async (answer) => {
                        const operation = await answerOperation(item, answer)
                        await vault.current!.update((s) => {
                          if (
                            s.queue.some(
                              (q) =>
                                JSON.parse(q.operation).operation_id ===
                                item.authorization.payload.operation_id,
                            )
                          )
                            throw new Error('另一标签页已保存原答案，请先核对')
                          if (
                            Math.max(Date.now(), s.clockFloor, s.serverTime) >=
                              Date.parse(pack.eligible_until) ||
                            Date.now() + 60_000 < s.clockFloor
                          )
                            throw new Error('已达到期限或检测到时钟回退，未保存新答案')
                          s.queue.push({ operation, state: 'queued' })
                        })
                        await refresh()
                      }}
                    />
                  </article>
                ))}
              </section>
            )
          })}
        </>
      )}
      {status !== 'checking' && (
        <section className="panel">
          <h2>本地清除与丢失处理</h2>
          <p>
            清除本地包不会删除服务端已接纳的学习事实。清除包含包、答案、索引、队列、缓存与包装密钥；未同步数据无法找回。
          </p>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => {
              if (
                !window.confirm(
                  state
                    ? `确认清除全部本地离线包和密钥？其中 ${remaining} 条未同步或待处置记录将无法恢复；服务端学习事实不会删除。`
                    : '离线库尚未解锁或已损坏，无法核对未同步数量。确认销毁全部本地离线内容、未知数量的未同步答案及密钥？',
                )
              )
                return
              void run(async () => {
                const current = vault.current
                lock()
                await discard(current)
                setStatus('absent')
                setNotice(
                  '本地库和缓存已清除。服务端学习事实没有删除；离线时无法确认同步身份已注销。',
                )
              })
            }}
          >
            清除全部本地离线数据
          </Button>
          <Button variant="ghost" disabled={!online || busy} onClick={() => void run(reconnect)}>
            核对待清除任务与回执
          </Button>
        </section>
      )}
    </main>
  )
}
