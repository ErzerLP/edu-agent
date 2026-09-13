import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import {
  ApiError,
  identityKey,
  publicClient,
  readSession,
  sameIdentity,
  unwrap,
} from '@/api/client'
import { sessionSchema, type Session } from '@/api/runtime'
import { DraftStore } from './drafts'
import { Button } from '@/components/ui/button'
import { ErrorState } from '@/components/common'

type LearningIdentity = {
  session: Session
  drafts: DraftStore
  logout: () => Promise<void>
  prefix: readonly (string | number)[]
}
const Context = createContext<LearningIdentity | null>(null)
export function useIdentity() {
  const value = useContext(Context)
  if (!value) throw new Error('缺少学习身份')
  return value
}
const pairingSchema = z.object({
  code: z.string().trim().min(1, '请输入配对码').max(100),
  display_name: z.string().trim().min(1).max(100),
})

function Pairing({ onPaired, restore }: { onPaired: (s: Session) => void; restore: () => void }) {
  const form = useForm({
    resolver: zodResolver(pairingSchema),
    defaultValues: { code: '', display_name: '我的浏览器' },
  })
  const [error, setError] = useState<unknown>()
  return (
    <main className="pairing panel">
      <span className="eyebrow">知络 KNOWLEDGE MESH</span>
      <h1>让好奇心，有处安放。</h1>
      <p>配对浏览器，保存你的学习目标，随时回来继续。</p>
      <form
        onSubmit={form.handleSubmit(async (values) => {
          setError(undefined)
          try {
            const s = await unwrap(
              publicClient.POST('/v1/web/pairings', {
                params: { header: { Origin: location.origin } },
                body: values,
              }),
              sessionSchema,
            )
            form.reset()
            onPaired(s)
          } catch (e) {
            setError(e)
          }
        })}
        onKeyDown={(event) => {
          if (event.key === 'Enter' && event.nativeEvent.isComposing) event.preventDefault()
        }}
      >
        <label>
          配对码
          <input
            autoComplete="off"
            {...form.register('code')}
            aria-invalid={!!form.formState.errors.code}
          />
        </label>
        {form.formState.errors.code && <p role="alert">{form.formState.errors.code.message}</p>}
        <label>
          浏览器名称
          <input autoComplete="off" {...form.register('display_name')} />
        </label>
        <p className="hint">从本机管理页获取一次性配对码。此入口仅用于学习，不提供管理权限。</p>
        {!!error && <ErrorState error={error} />}
        <Button type="submit" disabled={form.formState.isSubmitting}>
          配对并进入
        </Button>
        <Button variant="ghost" onClick={restore}>
          重新读取已有会话
        </Button>
      </form>
    </main>
  )
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const query = useQueryClient()
  const [session, setSession] = useState<Session>()
  const current = useRef<Session | undefined>(undefined)
  const epoch = useRef(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<unknown>()
  const drafts = useMemo(() => new DraftStore(), [])
  const clear = () => {
    epoch.current++
    current.current = undefined
    drafts.clear()
    void query.cancelQueries()
    query.clear()
    setSession(undefined)
  }
  const adopt = (s: Session) => {
    if (!current.current || !sameIdentity(current.current, s)) {
      epoch.current++
      drafts.clear()
      query.clear()
    }
    current.current = s
    setSession(s)
    setError(undefined)
  }
  const refresh = async () => {
    const started = epoch.current
    try {
      const s = await readSession()
      if (started === epoch.current) adopt(s)
    } catch (e) {
      if (started === epoch.current) {
        if (e instanceof ApiError && e.status === 401) clear()
        else setError(e)
      }
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => {
    void refresh()
  }, [])
  useEffect(() => {
    const expired = (event: Event) => {
      if (
        current.current &&
        (event as CustomEvent).detail === JSON.stringify(identityKey(current.current))
      )
        clear()
    }
    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (drafts.dirty) {
        event.preventDefault()
        event.returnValue = ''
      }
    }
    window.addEventListener('web-identity-lost', expired)
    window.addEventListener('beforeunload', beforeUnload)
    return () => {
      window.removeEventListener('web-identity-lost', expired)
      window.removeEventListener('beforeunload', beforeUnload)
    }
  }, [])
  useEffect(() => {
    if (!session) return
    const focus = () => {
      void refresh()
    }
    const timer = setInterval(focus, 30_000)
    const expiry = setTimeout(clear, Math.max(0, Date.parse(session.expires_at) - Date.now()))
    window.addEventListener('focus', focus)
    return () => {
      clearInterval(timer)
      clearTimeout(expiry)
      window.removeEventListener('focus', focus)
    }
  }, [session])
  if (loading)
    return (
      <main className="loading" role="status">
        正在读取浏览器会话…
      </main>
    )
  if (!session)
    return (
      <>
        {!!error && <ErrorState error={error} retry={() => void refresh()} />}
        <Pairing onPaired={adopt} restore={() => void refresh()} />
      </>
    )
  const logout = async () => {
    const result = await publicClient.POST('/v1/web/logout', {
      params: { header: { Origin: location.origin, 'X-CSRF-Token': session.csrf_token } },
    })
    if (!result.response.ok) throw new ApiError(result.response.status, 'logout_failed')
    clear()
  }
  return (
    <Context.Provider
      key={JSON.stringify(identityKey(session))}
      value={{ session, drafts, logout, prefix: identityKey(session) }}
    >
      {children}
    </Context.Provider>
  )
}
