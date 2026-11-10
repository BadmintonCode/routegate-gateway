import { useEffect, useMemo, useState } from 'react'
import type { CSSProperties } from 'react'
import {
  Alert, AutoComplete, Button, Card, Collapse, Form, Input, InputNumber, Modal,
  Popconfirm, Radio, Select, Space, Spin, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { CopyOutlined } from '@ant-design/icons'
import { api, get, post } from '../api'
import { LangSwitch, useLang } from '../shared'

const { Text } = Typography

// --- Developer page: self-service API credentials and API references ---
// Backend contract (parallel task): user-scoped temporary Bearer tokens
// (format `rgt_<64 hex>`), an endpoint catalog and an OpenAPI document,
// all same-origin under /admin/api/v1. Token rows are the caller's own
// tokens only — there is no user picker anywhere on this page.

type TokenItem = {
  id: number
  name: string
  prefix: string
  created_at: string
  expires_at: string
  last_used_at: string | null
  revoked_at: string | null
}

type EndpointParam = { name: string; in: string; type: string; required: boolean; desc: string }

type Endpoint = {
  group: string
  method: string
  path: string
  auth: string
  summary: string
  params: EndpointParam[]
  body_example: object | null
}

type Catalog = { version: string; endpoints: Endpoint[] }

const MONO = { fontFamily: 'SFMono-Regular, Consolas, "Liberation Mono", Menlo, monospace' } as const

const PRE_STYLE: CSSProperties = {
  margin: 0, padding: 12, background: '#f6f6f6', borderRadius: 6,
  fontSize: 12, lineHeight: 1.6, overflow: 'auto', ...MONO,
}

const METHOD_COLOR: Record<string, string> = {
  GET: 'green', POST: 'blue', PUT: 'orange', DELETE: 'red', PATCH: 'purple',
}

// Token status derives from "now" vs the timestamps; same raw-string date
// display as every other page (no client-side date formatting).
const tokenStatus = (rec: TokenItem): 'valid' | 'expired' | 'revoked' =>
  rec.revoked_at ? 'revoked' : new Date(rec.expires_at).getTime() <= Date.now() ? 'expired' : 'valid'

const PRESET_DAYS = [7, 30, 90, 180, 365]

// --- Tab 1: my tokens ---
// Tokens are the caller's own credentials, so this tab only ever mounts for a
// signed-in session (DeveloperPage swaps in TokensLoginHint pre-login): that
// keeps an anonymous visit from firing /auth/tokens and triggering the global
// 401 → login redirect.
function MyTokens() {
  const { s } = useLang()
  const [items, setItems] = useState<TokenItem[]>([])
  const [loading, setLoading] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  // `created` holds the RAW token. It exists exactly once, lives only in this
  // React state and is dropped when the dialog closes — never persisted.
  const [created, setCreated] = useState<{ token: string; name: string; expires_at: string } | null>(null)
  const [form] = Form.useForm()
  const expiresIn = Form.useWatch('expires_in', form)

  const load = () => {
    setLoading(true)
    get('/auth/tokens')
      .then((r) => setItems(r.items || []))
      .catch((e: any) => message.error(e.message))
      .finally(() => setLoading(false))
  }
  useEffect(() => { load() }, [])

  const create = async (v: any) => {
    try {
      const resp = await post('/auth/tokens', {
        name: v.name,
        expires_in_days: v.expires_in === 'custom' ? v.custom_days : v.expires_in,
      })
      setCreateOpen(false)
      form.resetFields()
      setCreated(resp)
    } catch (e: any) { message.error(e.message) }
  }

  const copyToken = async () => {
    if (!created) return
    try {
      if (!navigator.clipboard) throw new Error('unavailable')
      await navigator.clipboard.writeText(created.token)
      message.success(s.dev.tokenCopied)
    } catch {
      message.warning(s.dev.tokenCopyFail)
    }
  }

  const revoke = (rec: TokenItem) =>
    api(`/auth/tokens/${rec.id}`, { method: 'DELETE' })
      .then(() => { message.success(s.dev.revoked); load() })
      .catch((e: any) => message.error(e.message))

  const statusTag = (st: string) =>
    st === 'valid' ? <Tag color="green">{s.dev.tokenStatusValid}</Tag>
      : st === 'expired' ? <Tag color="orange">{s.dev.tokenStatusExpired}</Tag>
        : <Tag color="red">{s.dev.tokenStatusRevoked}</Tag>

  const cols: any[] = [
    { title: s.dev.tokenName, dataIndex: 'name' },
    { title: s.dev.tokenPrefix, dataIndex: 'prefix', render: (p: string) => <Text code style={MONO}>{p}…</Text> },
    { title: s.dev.tokenCreatedAt, dataIndex: 'created_at' },
    { title: s.dev.tokenExpiresAt, dataIndex: 'expires_at' },
    { title: s.dev.tokenLastUsed, dataIndex: 'last_used_at', render: (v: string | null) => v || '—' },
    { title: s.dev.tokenStatus, dataIndex: 'status', render: (_: any, rec: TokenItem) => statusTag(tokenStatus(rec)) },
    {
      title: s.dev.actions,
      render: (_: any, rec: TokenItem) => (
        <Popconfirm title={s.dev.confirmRevoke} onConfirm={() => revoke(rec)}>
          <Button size="small" danger>{s.action.revoke}</Button>
        </Popconfirm>
      ),
    },
  ]

  return (
    <>
      <Button type="primary" onClick={() => setCreateOpen(true)} style={{ marginBottom: 16 }}>{s.dev.newToken}</Button>
      <Table
        rowKey="id" dataSource={items} columns={cols} size="small" loading={loading}
        locale={{ emptyText: s.dev.tokensEmpty }}
      />

      <Modal title={s.dev.newToken} open={createOpen} width={520}
        onCancel={() => setCreateOpen(false)} onOk={() => form.submit()}>
        <Form form={form} layout="vertical" onFinish={create} initialValues={{ expires_in: 30 }}>
          <Form.Item name="name" label={s.dev.tokenName}
            rules={[{ required: true }, { max: 64, message: s.dev.tokenNameMax }]}>
            <Input maxLength={64} placeholder={s.dev.tokenNamePlaceholder} />
          </Form.Item>
          <Form.Item name="expires_in" label={s.dev.tokenExpiry} rules={[{ required: true }]}>
            <Select options={[
              ...PRESET_DAYS.map((n) => ({ value: n, label: `${n} ${s.dev.days}` })),
              { value: 'custom', label: s.dev.tokenExpiryCustom },
            ]} />
          </Form.Item>
          {expiresIn === 'custom' ? (
            <Form.Item name="custom_days" label={s.dev.tokenExpiryCustomLabel} rules={[{ required: true }]}>
              <InputNumber min={1} max={365} precision={0} style={{ width: 180 }} />
            </Form.Item>
          ) : null}
        </Form>
      </Modal>

      {/* One-time raw-token dialog: the only moment the secret is visible. */}
      <Modal title={s.dev.tokenCreated} open={!!created} width={560} footer={null}
        onCancel={() => { setCreated(null); load() }}>
        <Alert type="warning" showIcon message={s.dev.tokenShownOnce} style={{ marginBottom: 12 }} />
        <div style={{ display: 'flex', gap: 8, alignItems: 'stretch' }}>
          <pre style={{ ...PRE_STYLE, flex: 1, userSelect: 'all' }}>{created?.token}</pre>
          <Button type="primary" onClick={copyToken}>{s.dev.tokenCopy}</Button>
        </div>
      </Modal>
    </>
  )
}

// Pre-login stand-in for the tokens tab: a friendly hint instead of data.
// The button is a plain anchor to /admin/ (full load → SPA boots → login page),
// so no authed API call is ever made from an anonymous visit.
function TokensLoginHint() {
  const { s } = useLang()
  return (
    <Alert
      type="info" showIcon
      message={s.dev.tokensNeedLogin}
      description={s.dev.tokensNeedLoginDesc}
      action={<Button type="primary" size="small" href="/admin/">{s.dev.goLogin}</Button>}
    />
  )
}

// --- Tab 2: API docs (endpoint catalog) ---
function ApiDocs() {
  const { s } = useLang()
  const [catalog, setCatalog] = useState<Catalog | null>(null)
  const [failed, setFailed] = useState(false)
  // null = the default (first group expanded) has not been overridden yet.
  const [open, setOpen] = useState<string[] | null>(null)
  // AI prompt modal: the fetched template text; open while loading too.
  const [promptOpen, setPromptOpen] = useState(false)
  const [promptText, setPromptText] = useState('')

  useEffect(() => {
    get('/meta/endpoints').then(setCatalog).catch(() => setFailed(true))
  }, [])

  const openPrompt = () => {
    setPromptOpen(true)
    setPromptText('')
    // The prompt endpoint serves text/markdown, so bypass the JSON api()
    // wrapper and read the raw body. Public endpoint: no session needed.
    // The template is parameterized with {{ADMIN_URL}}; substitute it with
    // the address the browser is actually visiting so the copied prompt
    // carries the real host and needs no manual edit.
    fetch('/admin/api/v1/meta/prompt')
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((t) => setPromptText(t.split('{{ADMIN_URL}}').join(window.location.origin)))
      .catch(() => setPromptText(s.dev.promptLoadFail))
  }

  const copyPrompt = () => {
    navigator.clipboard
      .writeText(promptText)
      .then(() => message.success(s.dev.promptCopied))
      .catch(() => message.error(s.dev.promptCopyFail))
  }

  // Group endpoints by `group`, preserving first-appearance order.
  const groups = useMemo(() => {
    const out: { group: string; endpoints: Endpoint[] }[] = []
    for (const e of catalog?.endpoints || []) {
      const g = out.find((x) => x.group === e.group)
      if (g) g.endpoints.push(e)
      else out.push({ group: e.group, endpoints: [e] })
    }
    return out
  }, [catalog])

  const activeKey = open ?? (groups.length ? [groups[0].group] : [])

  const authTag = (auth: string) =>
    auth === 'none' ? <Tag color="orange">{s.dev.authNone}</Tag>
      : auth === 'session' ? <Tag>{s.dev.authSession}</Tag>
        : <Tag color="blue">{s.dev.authSessionToken}</Tag>

  const paramCols: any[] = [
    { title: s.dev.paramName, dataIndex: 'name', width: 160, render: (v: string) => <Text code>{v}</Text> },
    { title: s.dev.paramIn, dataIndex: 'in', width: 90 },
    { title: s.dev.paramType, dataIndex: 'type', width: 130, render: (v: string) => <Text code>{v}</Text> },
    {
      title: s.dev.paramRequired, dataIndex: 'required', width: 100,
      render: (v: boolean) => (v ? <Tag color="red">{s.dev.required}</Tag> : <Text type="secondary">{s.dev.optional}</Text>),
    },
    { title: s.dev.paramDesc, dataIndex: 'desc' },
  ]

  const renderEndpoint = (e: Endpoint, i: number) => (
    <div key={`${e.method} ${e.path}`} style={{ padding: '12px 0', borderTop: i ? '1px solid #f0f0f0' : undefined }}>
      <Space wrap size={8}>
        <Tag color={METHOD_COLOR[e.method] || 'default'} style={MONO}>{e.method}</Tag>
        <Text code style={MONO}>{e.path}</Text>
        {authTag(e.auth)}
      </Space>
      {e.summary ? <div style={{ marginTop: 6 }}><Text type="secondary">{e.summary}</Text></div> : null}
      {e.params?.length ? (
        <>
          <div style={{ margin: '10px 0 4px' }}><Text strong>{s.dev.docsParams}</Text></div>
          <Table
            rowKey={(r: any) => `${r.in}:${r.name}`} dataSource={e.params}
            columns={paramCols} size="small" pagination={false}
          />
        </>
      ) : null}
      {e.body_example != null ? (
        <>
          <div style={{ margin: '10px 0 4px' }}><Text strong>{s.dev.docsBodyExample}</Text></div>
          <pre style={{ ...PRE_STYLE, maxHeight: 320 }}>{JSON.stringify(e.body_example, null, 2)}</pre>
        </>
      ) : null}
    </div>
  )

  if (failed) return <Alert type="error" showIcon message={s.dev.docsLoadFail} />
  if (!catalog) return <Spin />
  return (
    <>
      <Space wrap style={{ marginBottom: 12 }}>
        <Button type="primary" ghost icon={<CopyOutlined />} onClick={openPrompt}>{s.dev.promptCopy}</Button>
        <a href="/admin/api/v1/openapi.json" target="_blank" rel="noreferrer">{s.dev.docsOpenapi}</a>
        <a href="/admin/api/v1/meta/skill/SKILL.md" target="_blank" rel="noreferrer">{s.dev.docsSkill}</a>
        <a href="/admin/api/v1/meta/skill/ARCHITECTURE.md" target="_blank" rel="noreferrer">{s.dev.docsArch}</a>
        <a href="/admin/api/v1/meta/skill/AI-PROMPT.md" target="_blank" rel="noreferrer">{s.dev.docsAIPrompt}</a>
        <Text type="secondary">{s.dev.docsConsoleHint}</Text>
      </Space>
      <Modal
        title={s.dev.promptTitle}
        open={promptOpen}
        onCancel={() => setPromptOpen(false)}
        width={760}
        footer={[
          <Button key="copy" type="primary" icon={<CopyOutlined />} onClick={copyPrompt} disabled={!promptText}>{s.dev.promptCopy}</Button>,
          <Button key="close" onClick={() => setPromptOpen(false)}>{s.dev.promptClose}</Button>,
        ]}
      >
        <Alert type="info" showIcon message={`${s.dev.promptHint} ${s.dev.promptSubstituted}`} style={{ marginBottom: 12 }} />
        {promptText ? (
          <pre style={{ ...PRE_STYLE, maxHeight: 420 }}>{promptText}</pre>
        ) : (
          <Spin />
        )}
      </Modal>
      <Collapse
        activeKey={activeKey}
        onChange={(k) => setOpen(k as string[])}
        items={groups.map((g) => ({
          key: g.group,
          label: <Space><Text strong>{g.group}</Text><Tag>{g.endpoints.length}</Tag></Space>,
          children: g.endpoints.map(renderEndpoint),
        }))}
      />
    </>
  )
}

// --- Tab 3: API console ---
// The console calls fetch directly (not the api() wrapper) so that ANY status
// — including errors outside the happy path — is displayable.
//
// Credential hard rule: the Bearer token lives ONLY in React state here. It
// is never written to localStorage/sessionStorage and is sent for this
// single request only; a page refresh clears it.

type ConsoleResult = {
  url: string
  status: number
  statusText: string
  durationMs: number
  headers: [string, string][]
  text: string
  pretty: string | null
  errEnv: { code?: string; message?: string; request_id?: string } | null
}

const BODY_METHODS = ['POST', 'PUT', 'PATCH']
const RESP_DISPLAY_CAP = 50 * 1024

// Catalog paths are /admin/api/v1-relative; allow absolute /admin/… paths and
// free text with query strings appended.
const resolveUrl = (p: string): string => {
  const base = p.split('?')[0]
  if (base.startsWith('/admin/')) return p
  if (base.startsWith('/')) return '/admin/api/v1' + p
  return '/admin/api/v1/' + p
}

function ApiConsole({ authed = true }: { authed?: boolean }) {
  const { s } = useLang()
  const [endpoints, setEndpoints] = useState<Endpoint[]>([])
  const [method, setMethod] = useState('GET')
  const [path, setPath] = useState('/auth/tokens')
  const [bodyText, setBodyText] = useState('')
  const [credMode, setCredMode] = useState<'session' | 'bearer'>('session')
  const [bearer, setBearer] = useState('')
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<ConsoleResult | null>(null)

  useEffect(() => {
    get('/meta/endpoints').then((r: Catalog) => setEndpoints(r.endpoints || [])).catch(() => {})
  }, [])

  const needsBody = BODY_METHODS.includes(method)

  const send = async () => {
    const raw = path.trim()
    if (!raw) return
    let body: string | undefined
    if (needsBody && bodyText.trim()) {
      try { JSON.parse(bodyText) } catch {
        message.error(s.dev.consoleBodyInvalid)
        return
      }
      body = bodyText
    }
    const headers: Record<string, string> = {}
    if (credMode === 'bearer' && bearer.trim()) headers.Authorization = `Bearer ${bearer.trim()}`
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    const url = resolveUrl(raw)
    setSending(true)
    const t0 = performance.now()
    try {
      const res = await fetch(url, {
        method,
        headers,
        body,
        // Bearer mode omits the session cookie so the credential under test
        // is exactly the one selected.
        credentials: credMode === 'bearer' ? 'omit' : 'include',
      })
      const text = await res.text()
      const headersList: [string, string][] = []
      res.headers.forEach((v, k) => headersList.push([k, v]))
      let pretty: string | null = null
      let errEnv: ConsoleResult['errEnv'] = null
      try {
        const parsed = JSON.parse(text)
        if (parsed && typeof parsed === 'object') {
          pretty = JSON.stringify(parsed, null, 2)
          const err = (parsed as any)?.error
          if (err && typeof err === 'object' && (err.code || err.message)) {
            errEnv = { code: err.code, message: err.message, request_id: err.request_id }
          }
        }
      } catch { /* non-JSON body — shown as raw text below */ }
      setResult({
        url,
        status: res.status,
        statusText: res.statusText,
        durationMs: Math.round(performance.now() - t0),
        headers: headersList,
        text,
        pretty,
        errEnv,
      })
    } catch {
      message.error(s.dev.consoleRequestFailed)
    } finally {
      setSending(false)
    }
  }

  const statusColor = (st: number) =>
    st < 300 ? 'green' : st < 400 ? 'blue' : st < 500 ? 'orange' : 'red'

  const headerCols: any[] = [
    { title: s.dev.respHeaderName, dataIndex: 0, width: 280, render: (v: string) => <Text code>{v}</Text> },
    { title: s.dev.respHeaderValue, dataIndex: 1, render: (v: string) => <Text code>{v}</Text> },
  ]

  const disp = result ? (result.pretty ?? result.text) : ''
  const truncated = disp.length > RESP_DISPLAY_CAP
  const shown = truncated ? disp.slice(0, RESP_DISPLAY_CAP) : disp

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={12}>
      <div style={{ display: 'flex', gap: 8 }}>
        <Select
          value={method} onChange={(m) => setMethod(m)} style={{ width: 110 }}
          options={['GET', 'POST', 'PUT', 'DELETE', 'PATCH'].map((m) => ({ value: m }))}
        />
        <AutoComplete
          style={{ flex: 1 }}
          value={path}
          onChange={setPath}
          placeholder="/auth/tokens?limit=10"
          options={endpoints.map((e) => ({ value: e.path, label: `${e.method} ${e.path}`, endpoint: e }))}
          filterOption={(input, option) =>
            String(option?.value ?? '').toLowerCase().includes(input.toLowerCase())}
          onSelect={(_, opt: any) => {
            const e = opt.endpoint as Endpoint
            setMethod(e.method)
            if (e.body_example) setBodyText(JSON.stringify(e.body_example, null, 2))
          }}
        />
        <Button type="primary" loading={sending} onClick={send}>{s.dev.consoleSend}</Button>
      </div>

      {needsBody ? (
        <div>
          <div style={{ marginBottom: 4 }}><Text strong>{s.dev.consoleBody}</Text></div>
          <Input.TextArea
            value={bodyText} onChange={(e) => setBodyText(e.target.value)}
            rows={6} style={MONO} placeholder='{ "key": "value" }'
          />
        </div>
      ) : null}

      <div>
        <Space wrap align="center">
          <Text strong>{s.dev.consoleCredential}</Text>
          <Radio.Group value={credMode} onChange={(e) => setCredMode(e.target.value)}>
            <Radio value="session">{s.dev.credSession}</Radio>
            <Radio value="bearer">{s.dev.credBearer}</Radio>
          </Radio.Group>
          {credMode === 'bearer' ? (
            <Input.Password
              value={bearer}
              onChange={(e) => setBearer(e.target.value)}
              placeholder="rgt_…"
              autoComplete="new-password"
              style={{ width: 360 }}
            />
          ) : null}
        </Space>
        {credMode === 'bearer' ? (
          <Alert type="warning" showIcon message={s.dev.bearerWarn} style={{ marginTop: 8 }} />
        ) : null}
        {/* Anonymous visit, session mode: authed endpoints will 401 — that is
            the expected outcome and the console displays it as-is. Bearer mode
            (credentials: 'omit', Authorization header only) needs no session. */}
        {!authed && credMode === 'session' ? (
          <Alert type="info" showIcon message={s.dev.consoleSessionHint} style={{ marginTop: 8 }} />
        ) : null}
      </div>

      {result ? (
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          <Space wrap>
            <Tag color={statusColor(result.status)} style={{ fontSize: 13, padding: '2px 10px' }}>
              {result.status} {result.statusText}
            </Tag>
            <Text type="secondary">{result.durationMs} ms · {result.url}</Text>
          </Space>
          {result.errEnv ? (
            <Alert
              type={result.status >= 500 ? 'error' : 'warning'}
              showIcon
              message={result.errEnv.code || 'error'}
              description={
                <span>
                  {result.errEnv.message}
                  {result.errEnv.request_id ? (
                    <> · request_id: <Text code>{result.errEnv.request_id}</Text></>
                  ) : null}
                </span>
              }
            />
          ) : null}
          <div>
            <div style={{ marginBottom: 4 }}><Text strong>{s.dev.consoleHeaders}</Text></div>
            <Table
              dataSource={result.headers} columns={headerCols} size="small" pagination={false}
              rowKey={(_: any, i?: number) => String(i)}
            />
          </div>
          <div>
            <div style={{ marginBottom: 4 }}><Text strong>{s.dev.consoleBodyResp}</Text></div>
            {disp ? (
              <>
                <pre style={{ ...PRE_STYLE, maxHeight: 480, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{shown}</pre>
                {truncated ? (
                  <Text type="secondary" style={{ display: 'block', marginTop: 4 }}>{s.dev.consoleTruncated}</Text>
                ) : null}
              </>
            ) : (
              <Text type="secondary">{s.dev.consoleNoBody}</Text>
            )}
          </div>
        </Space>
      ) : null}
    </Space>
  )
}

// --- Page frame ---
// authed defaults to true (the in-shell usage); the public page passes false,
// which swaps the tokens tab for a sign-in hint and surfaces a session-mode
// hint in the console. Docs and console tabs work either way — the backend
// serves /meta/endpoints and /openapi.json publicly.
export function DeveloperPage({ authed = true }: { authed?: boolean }) {
  const { s } = useLang()
  return (
    <Card>
      <Tabs
        items={[
          { key: 'tokens', label: s.dev.tabs.tokens, children: authed ? <MyTokens /> : <TokensLoginHint /> },
          { key: 'docs', label: s.dev.tabs.docs, children: <ApiDocs /> },
          { key: 'console', label: s.dev.tabs.console, children: <ApiConsole authed={authed} /> },
        ]}
      />
    </Card>
  )
}

// --- Public frame (pre-login) ---
// Rendered by App when an anonymous visitor opens /admin/developer: the same
// developer page under a minimal branded header, with a sign-in way back. The
// header link is a plain anchor to /admin/ (full load → login page), matching
// the login page's own docs link — no client router exists pre-login.
export function PublicDeveloperPage() {
  const { s } = useLang()
  return (
    <div style={{ minHeight: '100vh', background: '#f5f5f5' }}>
      <div style={{
        background: '#001529', padding: '10px 24px', display: 'flex',
        alignItems: 'center', gap: 10, flexWrap: 'wrap',
      }}>
        <img src="/admin/favicon.svg" alt="RouteGate" width={26} height={26} />
        <span style={{ color: '#fff', fontWeight: 700, fontSize: 16, letterSpacing: 0.5 }}>RouteGate</span>
        <span style={{ color: 'rgba(255,255,255,0.65)', fontSize: 13 }}>{s.dev.publicBanner}</span>
        <div style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 12 }}>
          <LangSwitch />
          <a href="/admin/" style={{ color: 'rgba(255,255,255,0.85)' }}>{s.login.signIn}</a>
        </div>
      </div>
      <div style={{ padding: 24, maxWidth: 1100, margin: '0 auto' }}>
        <DeveloperPage authed={false} />
      </div>
    </div>
  )
}
