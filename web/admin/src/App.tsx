import { useEffect, useMemo, useReducer, useState } from 'react'
import { ConfigProvider } from 'antd'
import enUS from 'antd/locale/en_US'
import zhCN from 'antd/locale/zh_CN'
import { get, setUnauthorizedHandler } from './api'
import { STRINGS, initialLang, storeLang, type Lang } from './i18n'
import { LangContext, type LangCtx, type Me } from './shared'
import { ForceChangePasswordPage, LoginPage } from './pages/login'
import { PublicDeveloperPage } from './pages/developer'
import { Shell } from './pages/shell'

// /admin/developer is the ONE public panel route: API docs and the API console
// render for anonymous visitors (GET /meta/endpoints and /openapi.json are
// public server-side); the tokens tab self-gates with a sign-in hint. Every
// other path keeps the login-first behavior.
const isPublicDevPath = () => window.location.pathname.replace(/\/+$/, '') === '/admin/developer'

export default function App() {
  const [lang, setLangState] = useState<Lang>(initialLang)
  const setLang = (l: Lang) => { setLangState(l); storeLang(l) }
  const s = STRINGS[lang]

  const [me, setMe] = useState<Me | null>(null)
  const [checked, setChecked] = useState(false)

  useEffect(() => {
    setUnauthorizedHandler(() => setMe(null))
    get('/auth/me').then(setMe).catch(() => setMe(null)).finally(() => setChecked(true))
  }, [])

  // Pre-login, the URL decides between the login page and the public developer
  // page. Anonymous navigation between them is a full document load (plain
  // anchors), but Back/Forward across history entries written while still
  // authenticated only fires popstate — re-render so the gate re-reads the URL.
  const [, bumpHistory] = useReducer((n: number) => n + 1, 0)
  useEffect(() => {
    if (me) return
    window.addEventListener('popstate', bumpHistory)
    return () => window.removeEventListener('popstate', bumpHistory)
  }, [me])

  const ctx: LangCtx = useMemo(() => ({ lang, setLang, s }), [lang, s])

  if (!checked) return null
  return (
    <LangContext.Provider value={ctx}>
      <ConfigProvider locale={lang === 'zh' ? zhCN : enUS}>
        {!me ? (isPublicDevPath() ? <PublicDeveloperPage />
            : <LoginPage onLogin={setMe} />)
          : me.must_change_password ? <ForceChangePasswordPage me={me} onDone={() => setMe({ ...me, must_change_password: false })} />
          : <Shell me={me} onLogout={() => setMe(null)} />}
      </ConfigProvider>
    </LangContext.Provider>
  )
}
