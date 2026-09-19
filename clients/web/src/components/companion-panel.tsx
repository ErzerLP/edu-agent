import { useEffect, useRef, useState } from 'react'
import { z } from 'zod'
import type { TutorConversation } from '@/api/conversations'
import { localRequest, localState, localViewSchema, pairSchema, receiptSchema, revokeLocal, taskSnapshots, type LocalPair, type LocalView } from '@/api/companion'
import { useIdentity } from '@/lib/session'
import { Button } from './ui/button'
import { Confirm } from './common'

const confirmed = z.record(z.string(), z.boolean())
const planSchema = z.object({ plan: z.string(), preview: z.string(), path: z.string(), status: z.literal('confirmation_required') })
const pretty = (v: unknown) => JSON.stringify(v, null, 2)

export function LocalConnectionSettings() {
  const { session } = useIdentity()
  const [enabled, setEnabled] = useState<boolean>()
  useEffect(() => { const abort = new AbortController(); void localRequest(session, { action: 'capabilities' }, z.object({ enabled: z.boolean() }), abort.signal).then(v => setEnabled(v.enabled)).catch(() => setEnabled(undefined)); return () => abort.abort() }, [session])
  return <section className="panel" aria-label="本地连接设置">
    <h2>本地连接</h2>
    <p>{enabled === undefined ? '尚未取得本地连接状态。' : enabled ? '服务器已启用可选本地连接，当前页面没有 OS 授权。' : '服务器未启用本地连接。线上导师、资料导入和学习功能仍可使用。'}</p>
    <p>安装独立 edu-companion 后，在需要使用的导师对话内打开“本地连接”，生成一次性配对码。运行程序并输入配对码，核对两端设备身份，再分别授权文件、原生 Shell 和模型外发。</p>
    <p>文件工作区不限制 Shell。路径、文件内容、命令、输入与终端输出会经学习服务器传输；选择模型外发后还会发到该对话显示的模型端点。不会读取 CLI 历史或钥匙串。</p>
    <p>切换对话或关闭页面撤销当前授权；通知丢失时租约到期收尾。刷新、退出或服务重启需要重新配对。无持久输出恢复，未知任务不能当作成功。</p>
  </section>
}

export function CompanionPanel({ conversation }: { conversation: TutorConversation }) {
  const { session } = useIdentity()
  const [pair, setPair] = useState<LocalPair>()
  const [view, setView] = useState<LocalView>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [files, setFiles] = useState(false)
  const [shell, setShell] = useState(false)
  const [model, setModel] = useState(false)
  const [path, setPath] = useState('.')
  const [hash, setHash] = useState('')
  const [content, setContent] = useState('')
  const [command, setCommand] = useState('')
  const [pty, setPTY] = useState(false)
  const [input, setInput] = useState('')
  const [offset, setOffset] = useState(0)
  const [stream, setStream] = useState('stdout')
  const [rows, setRows] = useState(24)
  const [cols, setCols] = useState(80)
  const [consumed, setConsumed] = useState<string[]>([])
  const [unconfirmed, setUnconfirmed] = useState<string>()
  const alive = useRef(true)
  const busyRef = useRef(false)
  const pendingPair = useRef<LocalPair | undefined>(undefined)
  useEffect(() => { alive.current = true; return () => { alive.current = false; if (pendingPair.current) void revokeLocal(session, pendingPair.current).catch(() => {}) } }, [session, conversation.id])
  useEffect(() => {
    if (!pair) return
    let cancelled = false
    let timer: number | undefined
    const abort = new AbortController()
    const poll = async () => {
      try { const v = await localRequest(session, { action: 'status', id: pair.id, secret: pair.secret }, localViewSchema, abort.signal); if (!cancelled) setView(v) }
      catch { if (!cancelled) { setError('连接状态无法确认；不会重发命令或输入。租约到期将停止受管任务。'); setView(old => old ? { ...old, state: 'disconnected' } : old) } }
      finally { if (!cancelled) timer = window.setTimeout(() => void poll(), 3000) }
    }
    void poll()
    return () => { cancelled = true; abort.abort(); window.clearTimeout(timer) }
  }, [pair, session])
  const act = async (action: () => Promise<void>) => {
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true); setError('')
    try { await action() } catch { if (alive.current) setError('请求未确认。请核对原操作回执；不要重新发送命令、文件提交或输入。') }
    finally { busyRef.current = false; if (alive.current) setBusy(false) }
  }
  const submit = async (tool: string, args: object) => {
    if (!pair) return
    const id = crypto.randomUUID()
    // 只发送一次。结果不明时保留身份，之后通过状态和回执观察。
    setUnconfirmed(id)
    await localRequest(session, { action: 'submit', id: pair.id, secret: pair.secret, operation: { id, run: '', tool, arguments: args } }, receiptSchema)
    setUnconfirmed(undefined)
    setView(await localRequest(session, { action: 'status', id: pair.id, secret: pair.secret }, localViewSchema))
    setError('')
  }
  const task = (task_id: string, action: string, extra: object = {}) => act(() => submit('task', { action, task_id, ...extra }))
  const connected = view?.state === 'connected' && !unconfirmed
  const receipts = view?.receipts ?? []
  return <section className="panel" aria-label="对话本地连接">
    <h3>本地连接</h3>
    <p>{view ? localState[view.state] ?? '状态未知' : '未连接，本对话没有本机工具。可继续使用线上导师和参考资料。'}</p>
    {!!error && <p role="status">{error}</p>}
    {unconfirmed && pair && <p role="status">操作 {unconfirmed} 的受理结果尚未确认。<Button disabled={busy} onClick={() => void act(async () => {
      const receipt = await localRequest(session, { action: 'receipt', id: pair.id, secret: pair.secret, operation_id: unconfirmed }, receiptSchema)
      setView(old => old ? { ...old, receipts: [...old.receipts.filter(r => r.id !== receipt.id), receipt] } : old)
      setUnconfirmed(undefined)
    })}>查询原操作回执</Button>查不到回执也不能据此重跑；可以撤销连接后在本机核对。</p>}
    {!pair && <Button disabled={busy || !conversation.writable} onClick={() => void act(async () => {
      const p = await localRequest(session, { action: 'pair' }, pairSchema)
      if (!alive.current) { void revokeLocal(session, p); return }
      pendingPair.current = p; setPair(p)
    })}>开始配对本机</Button>}
    {pair && !view?.device.id && <div>
      <p>在明确安装的本机 edu-companion 中输入以下一次性码，五分钟有效。凭据仅保留在当前标签页。</p>
      <code style={{ overflowWrap: 'anywhere' }}>{pair.id}.{pair.code}</code>
      <p>先核对程序显示的站点、OS 用户、工作区及风险，再输入“连接”。</p>
    </div>}
    {view?.device.id && <p style={{ overflowWrap: 'anywhere' }}>执行设备：{view.device.host} · {view.device.os} · OS 用户：{view.device.user}<br />工作区：{view.device.workspace}<br />设备身份：{view.device.id}<br />作用对话：{conversation.title}</p>}
    {view?.state === 'awaiting_authorization' && pair && <fieldset disabled={busy}>
      <legend>独立授权此设备与当前对话</legend>
      <p>请核对上方设备身份与本机终端相同。文件、路径、命令、输入和终端内容将经此学习服务器发送到浏览器，可能包含私人内容。Shell 使用此 OS 用户的原生权限，文件工作区不会限制 Shell。</p>
      <label><input type="checkbox" checked={files} onChange={e => setFiles(e.target.checked)} />允许工作区文件浏览、读取和逐次确认修改</label>
      <label><input type="checkbox" checked={shell} onChange={e => setShell(e.target.checked)} />允许原生 Shell、输入及任务控制（无命令白名单或文件沙箱）</label>
      <label><input type="checkbox" checked={model} onChange={e => setModel(e.target.checked)} />另外允许本地结果发到模型：{conversation.destination_provider} · {conversation.destination_endpoint}</label>
      <Button disabled={!files && !shell} onClick={() => void act(async () => {
        await localRequest(session, { action: 'grant', id: pair.id, secret: pair.secret, device: view.device.id, grant: { generation: session.generation, space: conversation.space_id, conversation: conversation.id, files, shell, model, destination: conversation.destination } }, confirmed)
        setView(await localRequest(session, { action: 'status', id: pair.id, secret: pair.secret }, localViewSchema))
      })}>确认设备、风险与本次授权</Button>
    </fieldset>}
    {pair && <Button variant="outline" disabled={busy} onClick={() => void act(async () => { await localRequest(session, { action: 'revoke', id: pair.id, secret: pair.secret }, confirmed); setView(await localRequest(session, { action: 'status', id: pair.id, secret: pair.secret }, localViewSchema)) })}>撤销连接并停止受管任务</Button>}
    {view?.state === 'revoked' && <Button disabled={busy} onClick={() => {
      pendingPair.current = undefined; setPair(undefined); setView(undefined); setError(''); setUnconfirmed(undefined)
      setFiles(false); setShell(false); setModel(false); setCommand(''); setInput(''); setContent(''); setHash(''); setPath('.'); setConsumed([])
    }}>清除本次页面状态并重新配对</Button>}
    {view?.grant && <p>文件：{view.grant.files ? '已授权' : '未授权'} · 原生 Shell：{view.grant.shell ? '已授权' : '未授权'} · 模型外发：{view.grant.model ? '已授权给显示的端点' : '未授权'}</p>}
    {view?.grant?.files && <fieldset disabled={!connected || busy}>
      <legend>工作区文件</legend>
      <label>相对路径<input value={path} onChange={e => setPath(e.target.value)} maxLength={4096} /></label>
      <Button onClick={() => void act(() => submit('list', { path }))}>浏览目录</Button>
      <Button onClick={() => void act(() => submit('read', { path, limit: 100 }))}>读取文件</Button>
      <Button onClick={() => void act(() => submit('stat', { path, hash: true }))}>读取当前版本</Button>
      <label>替换所需的当前 sha256（留空表示新建）<input value={hash} onChange={e => setHash(e.target.value)} /></label>
      <label>文件新内容<textarea value={content} maxLength={24000} onChange={e => setContent(e.target.value)} /></label>
      <Button onClick={() => void act(() => submit('prepare_write', { path, mode: hash ? 'replace' : 'create', content, ...(hash ? { expected_hash: hash } : {}) }))}>生成冻结预览</Button>
    </fieldset>}
    {receipts.map(r => { const p = planSchema.safeParse(r.value); return p.success && !consumed.includes(p.data.plan) ? <section key={r.id} aria-label="文件修改确认">
      <h4>待确认：{p.data.path}</h4><pre style={{ whiteSpace: 'pre-wrap', maxHeight: 360, overflow: 'auto' }}>{p.data.preview}</pre>
      <Confirm label="批准此冻结修改" title="确认在显示的设备上发布该修改？" disabled={!connected || busy} onConfirm={() => act(async () => { setConsumed(old => [...old, p.data.plan]); await submit('commit', { plan: p.data.plan }) })}>预览只针对当前文件版本；版本变化将拒绝发布。该授权不会批准后续修改。</Confirm>
      <Button disabled={!connected || busy} onClick={() => void act(async () => { setConsumed(old => [...old, p.data.plan]); await submit('discard', { plan: p.data.plan }) })}>拒绝修改</Button>
    </section> : null })}
    {view?.grant?.shell && <fieldset disabled={!connected || busy}>
      <legend>原生 Shell 与受管任务</legend>
      <label>命令<textarea value={command} maxLength={24000} onChange={e => setCommand(e.target.value)} /></label>
      <label><input type="checkbox" checked={pty} onChange={e => setPTY(e.target.checked)} />使用 PTY（stdout 为合并终端流）</label>
      <Confirm label="在此设备执行命令" title={`以 ${view.device.user} 在 ${view.device.host} 执行？`} disabled={!command.trim()} onConfirm={() => act(async () => { const text = command; setCommand(''); await submit('shell', { command: text, stdin: true, pty, wait_ms: 0 }) })}>Shell 具有原生权限，默认工作目录为 {view.device.workspace}，没有路径沙箱。<pre>{command}</pre></Confirm>
      <Button onClick={() => void act(() => submit('task', { action: 'list' }))}>查询任务列表</Button>
      <label>输入内容（保留换行）<textarea value={input} maxLength={24000} onChange={e => setInput(e.target.value)} /></label>
      <label>输出字节偏移<input type="number" min={0} value={offset} onChange={e => setOffset(Number(e.target.value))} /></label>
      <label>输出流<select value={stream} onChange={e => setStream(e.target.value)}><option value="stdout">stdout / PTY</option><option value="stderr">stderr（仅管道）</option></select></label>
      <label>PTY 行<input type="number" min={1} max={4096} value={rows} onChange={e => setRows(Number(e.target.value))} /></label>
      <label>PTY 列<input type="number" min={1} max={4096} value={cols} onChange={e => setCols(Number(e.target.value))} /></label>
      {taskSnapshots(receipts).map(t => <section key={t.TaskID} aria-label="本地任务">
        <p>{t.TaskID} · {t.State} · {t.Controllable ? '可控制' : '已停止控制，不能向旧 PID 发信号'}</p>
        <Button onClick={() => void task(t.TaskID, 'status')}>状态</Button>
        <Button onClick={() => void task(t.TaskID, 'read', { stream, offset, limit: 4096 })}>读取一页输出</Button>
        <Button disabled={!t.Controllable} onClick={() => { const text = input; setInput(''); void task(t.TaskID, 'input', { content: text }) }}>发送输入到此任务</Button>
        <Confirm label="停止任务" title="停止此任务及受管进程组？" disabled={!t.Controllable} onConfirm={() => task(t.TaskID, 'stop')}>未必能终止已脱离进程组的进程；请检查 CleanupIncomplete。</Confirm>
        {t.PTY ? <><Button disabled={!t.Controllable} onClick={() => void task(t.TaskID, 'interrupt')}>终端中断</Button><Button disabled={!t.Controllable} onClick={() => void task(t.TaskID, 'eof')}>终端 EOF</Button><Button disabled={!t.Controllable} onClick={() => void task(t.TaskID, 'resize', { rows, cols })}>调整终端大小</Button></> : <Button disabled={!t.Controllable} onClick={() => void task(t.TaskID, 'close_input')}>关闭输入</Button>}
      </section>)}
    </fieldset>}
    {!!receipts.length && <section aria-label="设备操作回执"><h4>设备回执</h4><p>退出码仅描述实际退出；Written 是已确认写入字节，不证明程序处理完成。Truncated / Incomplete 表示输出缺口，NextOffset 为下一页位置。Persistence / Saved 单独说明保存状态；本次输出仅在内存，退出不可恢复。</p>
      {receipts.map(r => <details key={r.id}><summary>{localState[r.state] ?? '状态未知'} · {r.id}{r.error ? ` · ${r.error}` : ''}</summary><p>执行：{view?.device.host} · OS 用户：{view?.device.user} · 工作区：{view?.device.workspace}</p><p>设备：{r.device} · 对话：{r.conversation} · 运行：{r.run}{r.task && <> · 任务：{r.task} · 原运行：{r.task_run}</>}</p><pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{pretty(r.value)}</pre></details>)}
    </section>}
  </section>
}
