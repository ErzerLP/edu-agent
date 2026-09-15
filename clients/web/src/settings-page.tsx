import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useIdentity } from './lib/session'
import { ApiError, learningClient, unwrap } from './api/client'
import {
  capabilitiesSchema,
  limitsSchema,
  settingsReason,
  settingsSchema,
  type ConnectionView,
  type Limits,
  type Settings,
} from './api/settings'
import type { components } from './api/schema'
import { Button } from './components/ui/button'
import { Confirm, ErrorState } from './components/common'

type Target = 'teaching' | 'mentor' | 'search'
type Update = components['schemas']['LearningSettingsUpdate']
type Change = Omit<Update, 'expected_revision'>
const titles: Record<Target, string> = {
  teaching: '教学模型',
  mentor: 'Web 导师模型',
  search: '搜索',
}
const limitFields: [keyof Limits, string, number, number][] = [
  ['research_requests', '单次研究请求预算', 1, 1000],
  ['research_tokens', '单次研究 Token 预算', 1024, 10000000],
  ['output_tokens', '单次输出 Token 上限', 64, 32768],
  ['context_tokens', '上下文 Token 上限', 4096, 2000000],
  ['idle_timeout_seconds', '无响应超时（秒）', 5, 600],
  ['concurrency', '并发上限', 1, 16],
  ['storage_mib', '存储额度（MiB）', 16, 1048576],
]

function ConnectionCard({
  target,
  config,
  effective,
  value,
  write,
  probe,
  pending,
  update,
  test,
}: {
  target: Target
  config: ConnectionView
  effective: ConnectionView
  value: Settings
  write: boolean
  probe: boolean
  pending: boolean
  update: (change: Change) => Promise<void>
  test: (target: Target) => Promise<void>
}) {
  const [editing, setEditing] = useState(false)
  const secretInput = useRef<HTMLInputElement>(null)
  const title = titles[target]
  const reused = target === 'mentor' && value.mentor_uses_teaching
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (pending) return
    const fields = new FormData(event.currentTarget)
    const newKey = secretInput.current?.value ?? ''
    // 新值仅进入此次请求；不存入 React Query、草稿、地址或浏览器 storage。
    if (secretInput.current) secretInput.current.value = ''
    const connection = {
      enabled: fields.get('enabled') === 'on',
      provider: target === 'search' ? ('brave' as const) : ('openai_compatible' as const),
      endpoint: String(fields.get('endpoint') ?? '').trim(),
      model: target === 'search' ? '' : String(fields.get('model') ?? '').trim(),
      auth_mode: fields.get('auth_mode') === 'none' ? ('none' as const) : ('bearer' as const),
    }
    await update({ target, connection, ...(newKey ? { new_key: newKey } : {}) })
  }
  return (
    <section className="panel settings-card" aria-label={title}>
      <h3>{title}</h3>
      <p>
        {effective.configured ? '已配置' : '未配置'} · {effective.enabled ? '已启用' : '未启用'} ·{' '}
        {settingsReason(effective.reason)}
      </p>
      <p>
        数据发送位置：<span className="settings-endpoint">{effective.endpoint || '尚未配置'}</span>
      </p>
      {target !== 'search' && <p>模型：{effective.model || '尚未配置'}</p>}
      <p>
        凭据：
        {effective.has_key
          ? '已安全保存，不能查看旧值'
          : effective.auth_mode === 'none'
            ? '明确使用无鉴权端点'
            : '未保存'}
      </p>
      {effective.probe && (
        <p>
          上次测试：{new Date(effective.probe.checked_at).toLocaleString('zh-CN')}；请求{' '}
          {effective.probe.requests} 次；估价未知。
          {effective.probe.structured_json && ' 支持结构化 JSON。'}
          {effective.probe.native_schema && ' 支持原生 Schema。'}
        </p>
      )}
      {reused && <p>显式复用教学模型。独立导师配置仍保留；关闭复用后继续使用。</p>}
      {target === 'mentor' && (
        <Button
          variant="outline"
          disabled={!write || pending}
          onClick={() => void update({ mentor_uses_teaching: !value.mentor_uses_teaching })}
        >
          {reused ? '使用独立导师配置' : '显式复用教学模型'}
        </Button>
      )}
      <div className="actions">
        <Button
          variant="outline"
          disabled={!write || pending || reused}
          onClick={() => setEditing(!editing)}
        >
          {editing ? '取消编辑' : '配置或替换凭据'}
        </Button>
        <Confirm
          label="测试连接"
          title={`测试${title}连接？`}
          disabled={!probe || pending}
          onConfirm={() => void test(target)}
        >
          服务器将向 {effective.endpoint || '已保存端点'} 发送固定公开样本，最多等待 15
          秒，可能消耗额度或产生费用。不发送私人目标、正文或成绩。测试成功仅代表本次结果。
        </Confirm>
        <Confirm
          label="清除凭据"
          title={`清除${title}凭据？`}
          disabled={!write || pending || !config.has_key || reused}
          onConfirm={() => void update({ target, clear_key: true })}
        >
          清除服务端当前文件中的凭据。教学运行中的旧连接需重启后释放；旧备份和提供商授权需另行清理或撤销。
        </Confirm>
      </div>
      {editing && !reused && (
        <form autoComplete="off" onSubmit={(event) => void submit(event)}>
          <fieldset disabled={pending || !write}>
            <legend>{title}连接配置</legend>
            <label>
              <input type="checkbox" name="enabled" defaultChecked={config.enabled} />
              启用{title}
            </label>
            <label>
              端点
              <input
                name="endpoint"
                type="url"
                maxLength={2048}
                defaultValue={
                  config.endpoint ||
                  (target === 'search' ? 'https://api.search.brave.com/res/v1/web/search' : '')
                }
                placeholder={
                  target === 'search' ? 'Brave Web Search 地址' : '服务器允许列表中的模型 Base URL'
                }
              />
            </label>
            {target !== 'search' && (
              <>
                <label>
                  模型名称
                  <input name="model" maxLength={200} defaultValue={config.model} />
                </label>
                <label>
                  鉴权方式
                  <select name="auth_mode" defaultValue={config.auth_mode}>
                    <option value="bearer">使用服务端 Key</option>
                    <option value="none">明确使用无鉴权端点</option>
                  </select>
                </label>
              </>
            )}
            <label>
              新 Key（留空保留同端点旧值）
              <input
                ref={secretInput}
                type="password"
                autoComplete="new-password"
                maxLength={4096}
                spellCheck={false}
              />
            </label>
            <p className="hint">
              修改端点或鉴权方式会清除不匹配的旧凭据。Key
              提交后立即清空；失败重试需重新输入。密钥不影响目标保存。
            </p>
            <Button type="submit">保存{title}配置</Button>
          </fieldset>
        </form>
      )}
    </section>
  )
}

function BudgetForm({
  value,
  disabled,
  update,
}: {
  value: Settings
  disabled: boolean
  update: (change: Change) => Promise<void>
}) {
  const [error, setError] = useState('')
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const data = new FormData(event.currentTarget)
    const result = limitsSchema.safeParse(
      Object.fromEntries(limitFields.map(([key]) => [key, Number(data.get(key))])),
    )
    if (!result.success) {
      setError('请检查预算范围：输出须小于上下文，且不超过研究 Token 预算。')
      return
    }
    setError('')
    void update({
      limits: result.data,
      policy: { private_queries: data.get('private_queries') === 'on' },
    })
  }
  return (
    <section className="panel">
      <h3>预算与外发策略</h3>
      <p>
        这些预算供后续运行在接受和续行时执行。教学没有固定轮数上限，但请求、上下文、并发和存储各有边界。活跃
        SSE 的无响应计时由后续流式运行重置，不作为整次教学时长。
      </p>
      <form onSubmit={submit}>
        <fieldset disabled={disabled}>
          <legend>资源上限</legend>
          <div className="form-grid">
            {limitFields.map(([key, label, min, max]) => (
              <label key={key}>
                {label}
                <input
                  name={key}
                  type="number"
                  required
                  min={min}
                  max={max}
                  step={1}
                  defaultValue={value.limits[key]}
                />
              </label>
            ))}
          </div>
          <label>
            <input
              name="private_queries"
              type="checkbox"
              defaultChecked={value.policy.private_queries}
            />
            允许后续运行将私人目标、正文、成绩用于外部查询（默认关闭）
          </label>
          <p>
            模型运行会把所需学习上下文发送到所选模型端点；搜索仅发送获准的查询。提供商保留策略由其自身决定。当前测试始终只使用固定公开样本。
          </p>
          {error && <p role="alert">{error}</p>}
          <Button type="submit">保存预算与外发策略</Button>
        </fieldset>
      </form>
    </section>
  )
}

export function SettingsPage() {
  const { session, prefix } = useIdentity()
  const cache = useQueryClient()
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [message, setMessage] = useState('')
  const mounted = useRef(true)
  const operation = useRef<AbortController | null>(null)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      operation.current?.abort()
    }
  }, [])
  const settings = useQuery({
    queryKey: [...prefix, 'settings'],
    queryFn: ({ signal }) =>
      unwrap(learningClient(session).GET('/v1/settings', { signal }), settingsSchema),
  })
  const capabilities = useQuery({
    queryKey: [...prefix, 'capabilities'],
    queryFn: ({ signal }) =>
      unwrap(learningClient(session).GET('/v1/capabilities', { signal }), capabilitiesSchema),
  })
  const run = async (change?: Change, target?: Target) => {
    if (!settings.data || operation.current) return
    const controller = new AbortController()
    operation.current = controller
    setPending(true)
    setError(undefined)
    setMessage('')
    try {
      const client = learningClient(session)
      const params = { header: { Origin: session.server_id, 'X-CSRF-Token': session.csrf_token } }
      const result = target
        ? await unwrap(
            client.POST('/v1/settings/probes', {
              params,
              body: { target, expected_revision: settings.data.revision, consent: true },
              signal: controller.signal,
            }),
            settingsSchema,
          )
        : await unwrap(
            client.PUT('/v1/settings', {
              params,
              body: { ...change, expected_revision: settings.data.revision },
              signal: controller.signal,
            }),
            settingsSchema,
          )
      if (!mounted.current) return
      cache.setQueryData([...prefix, 'settings'], result)
      void cache.invalidateQueries({ queryKey: [...prefix, 'capabilities'] })
      setMessage(target ? '连接测试已完成，请查看卡片中的实际结果。' : '配置已保存。')
    } catch (failure) {
      if (mounted.current) setError(failure)
    } finally {
      operation.current = null
      if (mounted.current) setPending(false)
    }
  }
  const value = settings.data
  const write = !!value?.writable && session.device.scopes.includes('settings:write')
  const probe = !!value?.writable && session.device.scopes.includes('settings:probe')
  return (
    <>
      <section className="intro">
        <span className="eyebrow">设置与能力</span>
        <h1>清楚知道，现在能做什么。</h1>
      </section>
      <section className="panel">
        <h2>显示</h2>
        <p>可使用页面顶部的主题按钮切换深浅主题。浏览器只持久保存主题偏好。</p>
      </section>
      <section className="panel">
        <h2>教学授权</h2>
        <p>
          保存目标不启动研究或教学，也不要求配置模型或搜索。运行授权属于具体任务，不由设置权限替代。
        </p>
        <p>
          {write
            ? '此设备拥有显式授予的配置写权限。'
            : '此设备可以查看非敏感状态。写入和测试需要操作者使用 settings 配对档案重新授予专门权限。'}
        </p>
      </section>
      <section className="panel">
        <h2>模型与搜索</h2>
        <p>
          教学与 Web 导师使用服务端独立配置，可显式复用；CLI 本地 Key
          不上传或迁移。未知价格仅展示用量或“估价未知”，不推算金额。
        </p>
        {settings.isPending && <p role="status">正在读取真实配置…</p>}
        {settings.error && (
          <ErrorState error={settings.error} retry={() => void settings.refetch()} />
        )}
        {!value?.writable && value && (
          <p>服务器尚未启用受保护的设置文件，当前只能读取状态。请由服务器操作者启用配置存储。</p>
        )}
        {value?.teaching_restart_required && (
          <p className="notice">
            教学配置已保存，现有教学运行需重启服务后应用；正在运行的连接仍使用重启前配置。配置探测使用新值。
          </p>
        )}
        {value && (
          <details>
            <summary>允许的模型端点</summary>
            <ul>
              {value.model_endpoints.map((endpoint) => (
                <li className="settings-endpoint" key={endpoint}>
                  {endpoint}
                </li>
              ))}
            </ul>
            <p>本地/自托管端点由服务器操作者精确授权。此列表不授权搜索或任意网页抓取。</p>
          </details>
        )}
        {error instanceof ApiError && error.code === 'invalid_settings' ? (
          <p role="alert">配置格式、预算范围或端点许可无效。请检查输入；新的 Key 需重新填写。</p>
        ) : (
          !!error && <ErrorState error={error} retry={() => void settings.refetch()} />
        )}
        {message && <p role="status">{message}</p>}
        {pending && <p role="status">正在处理，请稍候…</p>}
      </section>
      {value && (
        <>
          {(['teaching', 'mentor', 'search'] as const).map((target) => (
            <ConnectionCard
              key={`${target}-${value.revision}`}
              target={target}
              config={value[target]}
              effective={target === 'mentor' ? value.effective_mentor : value[target]}
              value={value}
              write={write}
              probe={probe}
              pending={pending}
              update={(change) => run(change)}
              test={(target) => run(undefined, target)}
            />
          ))}
          <BudgetForm
            key={`limits-${value.revision}`}
            value={value}
            disabled={!write || pending}
            update={(change) => run(change)}
          />
        </>
      )}
      <section className="panel">
        <h2>可用能力</h2>
        {capabilities.error && (
          <ErrorState error={capabilities.error} retry={() => void capabilities.refetch()} />
        )}
        {capabilities.data && (
          <dl>
            {(
              [
                ['schema', '模型结构化输出'],
                ['rendering', '学习页面渲染'],
                ['answering', 'Web 作答'],
                ['search', '搜索连接'],
                ['persistence', '目标持久化'],
                ['research', '研究执行'],
                ['web_mentor', 'Web 导师执行'],
              ] as const
            ).map(([key, title]) => (
              <div key={key}>
                <dt>{title}</dt>
                <dd>
                  {capabilities.data[key].available
                    ? '已就绪'
                    : settingsReason(capabilities.data[key].reason)}
                </dd>
              </div>
            ))}
          </dl>
        )}
      </section>
      <section className="panel">
        <h2>会话数据</h2>
        <p>
          已保存目标由服务端持久化；未提交正文和新 Key
          仅在本标签页内存，刷新、退出或身份失效时清除。模型与搜索密钥属于服务器配置，不随学习记录删除；需在此显式清除并处理备份。
        </p>
      </section>
      <section className="panel">
        <h2>设备</h2>
        <p>{session.device.display_name}</p>
        <p>会话到期：{new Date(session.expires_at).toLocaleString('zh-CN')}</p>
        <p>
          退出只结束此浏览器会话；设备撤销需在本机管理入口操作。撤销后配置写入与探测权限立即失效。
        </p>
      </section>
    </>
  )
}
