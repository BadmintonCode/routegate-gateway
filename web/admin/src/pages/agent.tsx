import { useEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import 'highlight.js/styles/github.css'
import './agent.css'
import {
  Button, Card, Col, Collapse, Form, Input, InputNumber, Modal, Popconfirm, Radio,
  Row, Select, Space, Switch, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { DeleteOutlined, LoadingOutlined, RobotOutlined, UserOutlined } from '@ant-design/icons'
import { Bubble, Conversations, Sender } from '@ant-design/x'
import type { ConversationsProps } from '@ant-design/x'
import { del, get, post, put } from '../api'
import { useLang } from '../shared'

const { Text } = Typography

type ToolCall = { tool: string; args?: string; ok?: boolean; duration_ms?: number; phase?: 'start' | 'done' }
type ChatMsg = { role: 'user' | 'assistant'; content: string; thinking?: string; tools?: ToolCall[]; streaming?: boolean }
type ModelItem = {
  id: number; endpoint_id: number; provider_model_name: string; display_name: string
  enabled: boolean; is_default: boolean; max_tokens?: number | null; temperature?: number | null
  remark?: string; updated_at: string
}
type EndpointItem = {
  id: number; name: string; enabled: boolean; base_url?: string; remark: string
  api_key_masked?: string; has_api_key?: boolean; updated_at: string; models: ModelItem[]
}
type SessionItem = { session_key: string; title: string; model_id: number; updated_at: string }

type StreamHandlers = {
  onDelta: (text: string) => void
  onThinking: (text: string) => void
  onTool: (tool: ToolCall) => void
  onError: (message: string) => void
  onFinish: () => void
  onFail: (error: Error) => void
}

// ThinkingBlock renders the model's reasoning chain. While the turn is
// streaming it stays expanded and follows the growing text; once the answer
// completes it collapses to a toggleable header so the final answer leads.
function ThinkingBlock({ text, streaming }: { text: string; streaming?: boolean }) {
  const { s } = useLang()
  const [expanded, setExpanded] = useState(false)
  const preRef = useRef<HTMLPreElement>(null)

  useEffect(() => {
    if (streaming) setExpanded(true)
  }, [streaming])
  useEffect(() => {
    if (!streaming) setExpanded(false)
  }, [streaming])
  useEffect(() => {
    if (expanded && streaming) preRef.current?.scrollTo({ top: preRef.current.scrollHeight })
  }, [text, expanded, streaming])

  if (!text) return null
  return (
    <div className="routegate-thinking">
      <button type="button" className="routegate-thinking-toggle" onClick={() => setExpanded((v) => !v)}>
        <Text type="secondary">{streaming ? `${s.agent.thinkingLabel} …` : s.agent.thinkingLabel}</Text>
      </button>
      {expanded && <pre ref={preRef}>{text}</pre>}
    </div>
  )
}

// ToolTag renders a pending tool call (phase start, still executing) or a
// finished one with its audit outcome.
function ToolTag({ tool }: { tool: ToolCall }) {
  const { s } = useLang()
  if (tool.phase === 'start') {
    return <Tag color="default" icon={<LoadingOutlined />}>{tool.tool} · {s.agent.toolRunning}</Tag>
  }
  return <Tag color={tool.ok ? 'purple' : 'red'}>{tool.tool} · {tool.duration_ms}ms{tool.ok ? '' : ` · ${s.agent.toolFailed}`}</Tag>
}

// AssistantMessageBody is the shared assistant bubble body: reasoning chain,
// Markdown answer, and the tool audit tags.
function AssistantMessageBody({ item }: { item: ChatMsg }) {
  const hasTools = !!item.tools?.length
  return (
    <div>
      <ThinkingBlock text={item.thinking || ''} streaming={item.streaming} />
      <MarkdownMessage content={item.content} streaming={item.streaming} />
      {hasTools && (
        <div style={{ marginTop: 8, display: 'flex', flexWrap: 'wrap', gap: 4 }}>
          {item.tools!.map((tool, i) => <ToolTag key={i} tool={tool} />)}
        </div>
      )}
    </div>
  )
}

// Mermaid is loaded at runtime from the same-origin static asset
// /admin/mermaid.min.js (copied from the pinned npm package by
// scripts/copy-mermaid.mjs). Keeping it out of the Vite module graph keeps
// the Release Console 1 GB build container under its memory limit; lazy
// loading also means the ~3 MB parser bundle is only fetched when a chat
// answer actually contains a Mermaid block.
type MermaidAPI = {
  initialize: (config: Record<string, unknown>) => void
  render: (id: string, code: string) => Promise<{ svg: string }>
}

declare global {
  interface Window { mermaid?: MermaidAPI }
}

let mermaidLoader: Promise<MermaidAPI> | null = null
let mermaidConfigured = false

function loadMermaid(): Promise<MermaidAPI> {
  if (window.mermaid) return Promise.resolve(window.mermaid)
  if (!mermaidLoader) {
    mermaidLoader = new Promise((resolve, reject) => {
      const script = document.createElement('script')
      script.src = '/admin/mermaid.min.js'
      script.async = true
      script.onload = () => {
        if (window.mermaid) {
          if (!mermaidConfigured) {
            window.mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'default' })
            mermaidConfigured = true
          }
          resolve(window.mermaid)
        } else {
          mermaidLoader = null
          reject(new Error('mermaid bundle did not initialize'))
        }
      }
      script.onerror = () => {
        mermaidLoader = null
        reject(new Error('mermaid bundle could not be loaded'))
      }
      document.head.appendChild(script)
    })
  }
  return mermaidLoader
}

let mermaidSequence = 0

function MermaidBlock({ code, streaming }: { code: string; streaming?: boolean }) {
  const [svg, setSvg] = useState('')
  const [error, setError] = useState('')
  const idRef = useRef(`routegate-mermaid-${++mermaidSequence}`)

  useEffect(() => {
    let cancelled = false
    setSvg('')
    setError('')
    if (streaming || !code.trim()) return () => { cancelled = true }
    loadMermaid().then((api) => {
      // A fresh id per render avoids clashing with an earlier failed attempt.
      const renderID = `${idRef.current}-${++mermaidSequence}`
      return api.render(renderID, code)
    }).then((result) => {
      if (!cancelled) setSvg(result.svg)
    }).catch(() => {
      if (!cancelled) setError('Mermaid diagram could not be rendered')
    })
    return () => { cancelled = true }
  }, [code, streaming])

  if (svg) return <div className="routegate-mermaid" dangerouslySetInnerHTML={{ __html: svg }} />
  if (error) return <div className="routegate-mermaid-error"><Text type="secondary">{error}</Text><pre>{code}</pre></div>
  return <pre className="routegate-mermaid-source"><code>{code}</code></pre>
}

function MarkdownMessage({ content, streaming }: { content: string; streaming?: boolean }) {
  return <div className="routegate-markdown">
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      rehypePlugins={[[rehypeHighlight, { plainText: ['mermaid'] }] as any]}
      components={{
        a: ({ node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
        table: ({ node, ...props }) => <div className="routegate-markdown-table"><table {...props} /></div>,
        pre: ({ children, ...props }) => {
          const child = Array.isArray(children) ? children[0] : children
          const codeNode = child as any
          const className = codeNode?.props?.className || ''
          const match = /language-(\\w+)/.exec(className)
          const value = String(codeNode?.props?.children || '').replace(/\\n$/, '')
          if (match?.[1].toLowerCase() === 'mermaid') return <MermaidBlock code={value} streaming={streaming} />
          return <pre {...props}>{children}</pre>
        },
      }}
    >{content}</ReactMarkdown>
  </div>
}

export function AgentPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [tab, setTab] = useState('chat')
  const items = [{ key: 'chat', label: s.agent.chat, children: <ChatTab /> }]
  if (isSuper) {
    items.push({ key: 'models', label: s.agent.models, children: <ModelsTab /> })
  }
  return <Card><Tabs activeKey={tab} onChange={setTab} items={items} /></Card>
}

function ChatTab() {
  const { s } = useLang()
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [active, setActive] = useState('')
  const [messages, setMessages] = useState<ChatMsg[]>([])
  const [catalog, setCatalog] = useState<EndpointItem[]>([])
  const [modelID, setModelID] = useState(0)
  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)

  const loadCatalog = () => get('/llm/endpoints').then((r: any) => {
    const next = (r.items || []) as EndpointItem[]
    setCatalog(next)
    return next
  })
  const loadSessions = () => get('/assistant/sessions').then((r: any) => {
    const next = (r.items || []) as SessionItem[]
    setSessions(next)
    return next
  })

  useEffect(() => {
    Promise.all([loadSessions(), loadCatalog()]).then(([items]) => {
      if (items.length) openSession(items[0].session_key, items)
    }).catch((e) => message.error(e.message))
  }, [])
  useEffect(() => { scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight }) }, [messages])

  const openSession = (key: string, source = sessions) => {
    setActive(key)
    const session = source.find((item) => item.session_key === key)
    setModelID(session?.model_id || 0)
    get(`/assistant/sessions/${key}/messages`)
      .then((r: any) => setMessages((r.items || []).map((m: any) => ({ role: m.role, content: m.content, thinking: m.reasoning || '', tools: m.tool_calls || [] }))))
      .catch((e) => message.error(e.message))
  }

  const newSession = () => {
    post('/assistant/sessions', { model_id: modelID }).then((r: any) => {
      setMessages([]); setActive(r.session_key); setModelID(r.model_id || 0); loadSessions().catch(() => {})
    }).catch((e) => message.error(e.message))
  }

  const removeSession = (key: string) => {
    del(`/assistant/sessions/${key}`).then(() => {
      message.success(s.msg.deleted)
      const wasActive = key === active
      if (wasActive) { setActive(''); setMessages([]); setModelID(0) }
      loadSessions().then((items) => { if (wasActive && items.length) openSession(items[0].session_key, items) }).catch(() => {})
    }).catch((e: any) => message.error(e.message))
  }

  const switchModel = (id: number) => {
    setModelID(id)
    if (!active) return
    put(`/assistant/sessions/${active}`, { model_id: id }).then(() => {
      setSessions((prev) => prev.map((item) => item.session_key === active ? { ...item, model_id: id } : item))
    }).catch((e) => message.error(e.message))
  }

  const send = (text: string) => {
    const content = text.trim()
    if (!content || sending) return
    if (!active) {
      post('/assistant/sessions', { model_id: modelID }).then((r: any) => {
        setActive(r.session_key); setModelID(r.model_id || 0); loadSessions().catch(() => {}); streamTurn(r.session_key, content, modelID)
      }).catch((e) => message.error(e.message))
      return
    }
    streamTurn(active, content, modelID)
  }

  const streamTurn = (sessionKey: string, content: string, selectedModelID: number) => {
    setDraft('')
    setMessages((prev) => [...prev, { role: 'user', content }, { role: 'assistant', content: '', streaming: true }])
    setSending(true)
    const controller = new AbortController()
    abortRef.current = controller
    const patch = (fn: (message: ChatMsg) => ChatMsg) => setMessages((prev) => {
      if (!prev.length) return prev
      const next = prev.slice(); next[next.length - 1] = fn(next[next.length - 1]); return next
    })
    chatSSE(sessionKey, content, selectedModelID, controller.signal, {
      onDelta: (text) => patch((m) => ({ ...m, content: m.content + text })),
      onThinking: (text) => patch((m) => ({ ...m, thinking: (m.thinking || '') + text })),
      onTool: (tool) => patch((m) => {
        if (tool.phase === 'start') return { ...m, tools: [...(m.tools || []), tool] }
        // A done event replaces the matching pending start (same tool, most
        // recent); an unmatched done is still shown.
        const tools = [...(m.tools || [])]
        let idx = -1
        for (let i = tools.length - 1; i >= 0; i--) {
          if (tools[i].tool === tool.tool && tools[i].phase === 'start') { idx = i; break }
        }
        if (idx >= 0) tools[idx] = { ...tools[idx], ...tool, phase: 'done' }
        else tools.push({ ...tool, phase: 'done' })
        return { ...m, tools }
      }),
      onError: (text) => message.error(text || s.agent.streamFail),
      onFinish: () => { patch((m) => ({ ...m, streaming: false })); setSending(false); abortRef.current = null; loadSessions().catch(() => {}) },
      onFail: (error) => { setMessages((prev) => prev.length && prev[prev.length - 1].streaming && !prev[prev.length - 1].content ? prev.slice(0, -1) : prev); setSending(false); abortRef.current = null; message.error(error.message) },
    })
  }

  const stop = () => {
    abortRef.current?.abort(); abortRef.current = null; setSending(false)
    setMessages((prev) => { if (!prev.length) return prev; const next = prev.slice(); next[next.length - 1] = { ...next[next.length - 1], streaming: false }; return next })
  }

  const modelOptions = [{ value: 0, label: s.agent.endpointAuto }]
  for (const endpoint of catalog) {
    for (const model of endpoint.models || []) {
      if (endpoint.enabled && model.enabled) modelOptions.push({ value: model.id, label: `${endpoint.name} / ${model.display_name}` })
    }
  }
  const items: ConversationsProps['items'] = sessions.map((item) => ({ key: item.session_key, label: item.title || item.session_key.slice(0, 14) }))

  return <Row gutter={16}>
    <Col flex="220px">
      <Button type="primary" block onClick={newSession} style={{ marginBottom: 12 }}>{s.agent.newChat}</Button>
      <Conversations items={items} activeKey={active} onActiveChange={openSession} menu={(item) => ({ items: [{ key: 'delete', icon: <DeleteOutlined />, label: s.agent.deleteChat, danger: true, onClick: () => removeSession(item.key) }] })} />
      {!sessions.length && <Text type="secondary">{s.agent.emptyChat}</Text>}
    </Col>
    {/* minWidth: 0 lets the flex item shrink so wide assistant content (big
        Markdown tables) scrolls inside the bubble instead of blowing the
        page layout out (flexbox min-width:auto is the default). flex="1"
        (1 1 0%) also matters: with flex-basis auto the Row's flex-wrap
        decision uses the content's max-content width, so a wide table made
        this column wrap below the sidebar and take the full row. */}
    <Col flex="1" style={{ minWidth: 0, overflow: 'hidden' }}>
      <div style={{ marginBottom: 8 }}><Space wrap><Text type="secondary">{s.agent.endpoint}</Text><Select size="small" style={{ minWidth: 300, maxWidth: '100%' }} value={modelID} options={modelOptions} onChange={switchModel} disabled={!active && modelOptions.length === 1} /></Space></div>
      <div ref={scrollRef} style={{ height: '52vh', overflowY: 'auto', overflowX: 'hidden', padding: '4px 2px' }}>
        {!messages.length && <div style={{ textAlign: 'center', paddingTop: '18vh' }}><Text type="secondary">{s.agent.emptyChat}</Text></div>}
        <Bubble.List style={{ minWidth: 0 }} items={messages.map((item, index) => ({
          key: String(index), placement: item.role === 'user' ? 'end' : 'start', variant: item.role === 'user' ? 'filled' : 'shadow',
          avatar: item.role === 'user' ? { icon: <UserOutlined />, style: { color: '#1677ff', background: '#e6f4ff' } } : { icon: <RobotOutlined />, style: { color: '#722ed1', background: '#f9f0ff' } },
          content: item.content || (item.streaming ? s.agent.thinking : ''),
          loading: item.streaming && !item.content && !item.thinking,
          messageRender: item.role === 'assistant' && (item.content || item.thinking || item.tools?.length) ? () => <AssistantMessageBody item={item} /> : undefined,
        }))} />
      </div>
      <Sender placeholder={s.agent.inputPlaceholder} value={draft} onChange={setDraft} onSubmit={send} onCancel={stop} loading={sending} style={{ marginTop: 12 }} />
    </Col>
  </Row>
}

async function chatSSE(sessionKey: string, content: string, modelID: number, signal: AbortSignal, handlers: StreamHandlers) {
  let finished = false
  const finish = () => { if (!finished) { finished = true; handlers.onFinish() } }
  try {
    const response = await fetch('/admin/api/v1/assistant/chat', { method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ session_key: sessionKey, content, model_id: modelID }), signal })
    if (!response.ok || !response.body) { let text = `HTTP ${response.status}`; try { text = (await response.json())?.error?.message || text } catch { /* keep status */ }; throw new Error(text) }
    const reader = response.body.getReader(); const decoder = new TextDecoder(); let buffer = ''
    for (;;) {
      const { done, value } = await reader.read(); if (done) break; buffer += decoder.decode(value, { stream: true })
      let index: number
      while ((index = buffer.indexOf('\n\n')) >= 0) {
        const block = buffer.slice(0, index); buffer = buffer.slice(index + 2); let event = 'message'; let data = ''
        for (const line of block.split('\n')) { if (line.startsWith('event:')) event = line.slice(6).trim(); else if (line.startsWith('data:')) data += line.slice(5).trim() }
        if (!data) continue
        let payload: any; try { payload = JSON.parse(data) } catch { continue }
        if (event === 'delta') handlers.onDelta(payload.text || '')
        else if (event === 'thinking') handlers.onThinking(payload.text || '')
        else if (event === 'tool') handlers.onTool(payload as ToolCall)
        else if (event === 'error') handlers.onError(payload.message)
        else if (event === 'done') finish()
      }
    }
    finish()
  } catch (error: any) { if (error?.name === 'AbortError') { finish(); return }; handlers.onFail(error instanceof Error ? error : new Error(String(error))) }
}

function EndpointTestModal({ endpoint, onClose }: { endpoint: EndpointItem; onClose: () => void }) {
  const { s } = useLang()
  const models = (endpoint.models || []).filter((model) => model.enabled)
  const [modelID, setModelID] = useState(() => models.find((model) => model.is_default)?.id || models[0]?.id || 0)
  const [active, setActive] = useState('')
  const [messages, setMessages] = useState<ChatMsg[]>([])
  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)

  useEffect(() => () => abortRef.current?.abort(), [])
  useEffect(() => { scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight }) }, [messages])

  const changeModel = async (id: number) => {
    try {
      if (active) await put(`/assistant/sessions/${active}`, { model_id: id })
      setModelID(id)
    } catch (e: any) { message.error(e.message) }
  }

  const send = async (text: string) => {
    const content = text.trim()
    if (!content || sending || !modelID) return
    setDraft('')
    setSending(true)
    let sessionKey = active
    try {
      if (!sessionKey) {
        const session = await post('/assistant/sessions', { model_id: modelID })
        sessionKey = session.session_key
        setActive(sessionKey)
      }
      setMessages((prev) => [...prev, { role: 'user', content }, { role: 'assistant', content: '', streaming: true }])
      const controller = new AbortController()
      abortRef.current = controller
      const patch = (fn: (item: ChatMsg) => ChatMsg) => setMessages((prev) => {
        if (!prev.length) return prev
        const next = prev.slice()
        next[next.length - 1] = fn(next[next.length - 1])
        return next
      })
      chatSSE(sessionKey, content, modelID, controller.signal, {
        onDelta: (delta) => patch((item) => ({ ...item, content: item.content + delta })),
        onThinking: (text) => patch((item) => ({ ...item, thinking: (item.thinking || '') + text })),
        onTool: (tool) => patch((item) => {
          if (tool.phase === 'start') return { ...item, tools: [...(item.tools || []), tool] }
          const tools = [...(item.tools || [])]
          let idx = -1
          for (let i = tools.length - 1; i >= 0; i--) {
            if (tools[i].tool === tool.tool && tools[i].phase === 'start') { idx = i; break }
          }
          if (idx >= 0) tools[idx] = { ...tools[idx], ...tool, phase: 'done' }
          else tools.push({ ...tool, phase: 'done' })
          return { ...item, tools }
        }),
        onError: (text) => message.error(text || s.agent.streamFail),
        onFinish: () => { patch((item) => ({ ...item, streaming: false })); setSending(false); abortRef.current = null },
        onFail: (error) => { setMessages((prev) => prev.length && prev[prev.length - 1].streaming && !prev[prev.length - 1].content ? prev.slice(0, -1) : prev); setSending(false); abortRef.current = null; message.error(error.message) },
      })
    } catch (e: any) {
      setSending(false)
      message.error(e.message)
    }
  }

  const stop = () => {
    abortRef.current?.abort()
    abortRef.current = null
    setSending(false)
    setMessages((prev) => {
      if (!prev.length) return prev
      const next = prev.slice()
      next[next.length - 1] = { ...next[next.length - 1], streaming: false }
      return next
    })
  }

  const selectedModel = models.find((model) => model.id === modelID)

  return <Modal
    title={`${s.agent.testPage} · ${endpoint.name}`}
    open
    onCancel={onClose}
    footer={null}
    width={720}
    style={{ maxWidth: 'calc(100vw - 24px)' }}
    destroyOnClose
  >
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Row gutter={12} align="middle" wrap={false}>
        <Col flex="1" style={{ minWidth: 0 }}>
          <Text type="secondary">{s.agent.endpoint}</Text>
          <Select
            aria-label={s.agent.endpoint}
            value={modelID || undefined}
            options={models.map((model) => ({ value: model.id, label: `${model.display_name}${model.is_default ? ` · ${s.agent.epDefault}` : ''}` }))}
            onChange={changeModel}
            disabled={sending}
            style={{ width: '100%', marginTop: 4 }}
            placeholder={s.agent.modelEmpty}
          />
        </Col>
        <Col flex="none">
          <Text type="secondary">{s.agent.sessionInfo}</Text>
          <div style={{ marginTop: 4 }}>{active ? <Text code copyable>{active}</Text> : <Text type="secondary">—</Text>}</div>
        </Col>
      </Row>
      {selectedModel && <Text type="secondary">{endpoint.name} / {selectedModel.display_name} · {selectedModel.provider_model_name}</Text>}
      <div ref={scrollRef} style={{ height: 'min(48vh, 420px)', minHeight: 220, overflowY: 'auto', overflowX: 'hidden', padding: '4px 2px', minWidth: 0 }}>
        {!messages.length
          ? <div style={{ textAlign: 'center', paddingTop: 80 }}><Text type="secondary">{s.agent.emptyChat}</Text></div>
          : <Bubble.List style={{ minWidth: 0 }} items={messages.map((item, index) => ({
            key: String(index), placement: item.role === 'user' ? 'end' : 'start', variant: item.role === 'user' ? 'filled' : 'shadow',
            avatar: item.role === 'user' ? { icon: <UserOutlined />, style: { color: '#1677ff', background: '#e6f4ff' } } : { icon: <RobotOutlined />, style: { color: '#722ed1', background: '#f9f0ff' } },
            content: item.content || (item.streaming ? s.agent.thinking : ''), loading: item.streaming && !item.content && !item.thinking,
            messageRender: item.role === 'assistant' && (item.content || item.thinking || item.tools?.length) ? () => <AssistantMessageBody item={item} /> : undefined,
          }))} />}
      </div>
      <Sender placeholder={s.agent.inputPlaceholder} value={draft} onChange={setDraft} onSubmit={send} onCancel={stop} loading={sending} disabled={!modelID} />
    </Space>
  </Modal>
}

function ModelsTab() {
  const { s } = useLang()
  const [items, setItems] = useState<EndpointItem[]>([])
  const [endpointModal, setEndpointModal] = useState<any>(null)
  const [modelModal, setModelModal] = useState<{ endpointID: number; record?: ModelItem } | null>(null)
  const [testEndpoint, setTestEndpoint] = useState<EndpointItem | null>(null)
  const [discovering, setDiscovering] = useState<number | null>(null)
  const [endpointForm] = Form.useForm(); const [modelForm] = Form.useForm()
  const load = () => get('/llm/endpoints').then((r: any) => setItems(r.items || []))
  useEffect(() => { load() }, [])

  const openEndpoint = (record?: EndpointItem) => { setEndpointModal(record || {}); endpointForm.setFieldsValue(record ? { name: record.name, enabled: record.enabled, base_url: record.base_url, remark: record.remark || '', api_key: '' } : { name: '', enabled: true, base_url: '', remark: '', api_key: '' }) }
  const saveEndpoint = async (v: any) => { try { const body = { name: v.name, enabled: !!v.enabled, base_url: v.base_url, remark: v.remark || '', api_key: v.api_key || '' }; if (endpointModal?.id) await put(`/llm/endpoints/${endpointModal.id}`, body); else await post('/llm/endpoints', body); message.success(s.msg.saved); setEndpointModal(null); load() } catch (e: any) { message.error(e.message) } }
  const removeEndpoint = (record: EndpointItem) => del(`/llm/endpoints/${record.id}`).then(() => { message.success(s.msg.deleted); load() }).catch((e: any) => message.error(e.message))
  const openModel = (endpointID: number, record?: ModelItem) => { setModelModal({ endpointID, record }); modelForm.setFieldsValue(record ? { provider_model_name: record.provider_model_name, display_name: record.display_name, enabled: record.enabled, is_default: record.is_default, max_tokens: record.max_tokens, temperature: record.temperature, remark: record.remark || '' } : { provider_model_name: '', display_name: '', enabled: true, is_default: false, max_tokens: undefined, temperature: undefined, remark: '' }) }
  const saveModel = async (v: any) => { if (!modelModal) return; try { const body = { provider_model_name: v.provider_model_name, display_name: v.display_name, enabled: !!v.enabled, is_default: !!v.is_default, max_tokens: v.max_tokens || null, temperature: v.temperature ?? null, remark: v.remark || '' }; if (modelModal.record?.id) await put(`/llm/models/${modelModal.record.id}`, body); else await post(`/llm/endpoints/${modelModal.endpointID}/models`, body); message.success(s.msg.saved); setModelModal(null); load() } catch (e: any) { message.error(e.message) } }
  const removeModel = (record: ModelItem) => del(`/llm/models/${record.id}`).then(() => { message.success(s.msg.deleted); load() }).catch((e: any) => message.error(e.message))
  const discoverModels = (endpoint: EndpointItem) => { setDiscovering(endpoint.id); post(`/llm/endpoints/${endpoint.id}/models/discover`, {}).then((r: any) => { if (!r.ok) { message.error(r.error || s.agent.modelDiscoverFailed); return }; message.success(s.agent.modelDiscoverSuccess.replace('{count}', String(r.count)).replace('{added}', String(r.added)).replace('{existing}', String(r.existing))); load() }).catch((e: any) => message.error(e.message)).finally(() => setDiscovering(null)) }

  return <Space direction="vertical" style={{ width: '100%' }}>
    <Button type="primary" onClick={() => openEndpoint()}>{s.agent.epNew}</Button>
    <Collapse items={items.map((endpoint) => ({ key: String(endpoint.id), label: <Space>{endpoint.name}{!endpoint.enabled && <Tag>{s.misc.no}</Tag>}</Space>, extra: <Space onClick={(e) => e.stopPropagation()}><Button size="small" onClick={() => openEndpoint(endpoint)}>{s.action.edit}</Button><Popconfirm title={s.agent.epConfirmDelete} onConfirm={() => removeEndpoint(endpoint)}><Button size="small" danger icon={<DeleteOutlined />} /></Popconfirm></Space>, children: <Card type="inner"><Space direction="vertical" style={{ width: '100%' }}><Text type="secondary">{endpoint.base_url || '—'} · {endpoint.api_key_masked || 'no API key'}</Text><Space><Button size="small" onClick={() => openModel(endpoint.id)}>{s.agent.modelNew}</Button><Button size="small" loading={discovering === endpoint.id} disabled={!endpoint.enabled} onClick={() => discoverModels(endpoint)}>{discovering === endpoint.id ? s.agent.modelDiscovering : s.agent.modelDiscover}</Button><Button size="small" type="primary" disabled={!endpoint.enabled || !endpoint.models?.some((model) => model.enabled)} onClick={() => setTestEndpoint(endpoint)}>{s.agent.testPage}</Button></Space><Table size="small" rowKey="id" dataSource={endpoint.models || []} locale={{ emptyText: s.agent.modelEmpty }} columns={[{ title: s.agent.epDisplayName, dataIndex: 'display_name', render: (name: string, model: ModelItem) => <Space size={6}>{model.is_default && <Tag color="gold">{s.agent.epDefault}</Tag>}<span>{name}</span></Space> }, { title: s.agent.epModel, dataIndex: 'provider_model_name' }, { title: s.agent.epEnabled, dataIndex: 'enabled', render: (v: boolean) => v ? <Tag color="green">{s.misc.yes}</Tag> : <Tag>{s.misc.no}</Tag> }, { title: s.audit.detail, render: (_: any, model: ModelItem) => <Space><Button size="small" onClick={() => openModel(endpoint.id, model)}>{s.action.edit}</Button><Popconfirm title={s.agent.modelConfirmDelete} onConfirm={() => removeModel(model)}><Button size="small" danger icon={<DeleteOutlined />} /></Popconfirm></Space> }]} /></Space></Card> }))} />
    {testEndpoint && <EndpointTestModal endpoint={testEndpoint} onClose={() => setTestEndpoint(null)} />}
    <Modal title={endpointModal?.id ? s.agent.epEdit : s.agent.epNew} open={!!endpointModal} onCancel={() => setEndpointModal(null)} onOk={() => endpointForm.submit()} width={640}><Form form={endpointForm} layout="vertical" onFinish={saveEndpoint}><Form.Item name="name" label={s.agent.epName} rules={[{ required: true }]}><Input /></Form.Item><Form.Item name="base_url" label={s.agent.epURL} rules={[{ required: true }]}><Input placeholder="https://api.deepseek.com/v1" /></Form.Item><Form.Item name="api_key" label={s.agent.epKey} extra={endpointModal?.id ? s.agent.epKeyKeep : s.agent.epKeyExtra}><Input.Password placeholder="optional for local endpoints" autoComplete="new-password" /></Form.Item><Form.Item name="enabled" label={s.agent.epEnabled} valuePropName="checked"><Switch /></Form.Item><Form.Item name="remark" label={s.agent.epRemark}><Input /></Form.Item></Form></Modal>
    <Modal title={modelModal?.record ? s.agent.modelEdit : s.agent.modelNew} open={!!modelModal} onCancel={() => setModelModal(null)} onOk={() => modelForm.submit()} width={640}><Form form={modelForm} layout="vertical" onFinish={saveModel}><Row gutter={12}><Col span={12}><Form.Item name="provider_model_name" label={s.agent.epModel} rules={[{ required: true }]}><Input placeholder="deepseek-reasoner" /></Form.Item></Col><Col span={12}><Form.Item name="display_name" label={s.agent.epDisplayName} rules={[{ required: true }]}><Input placeholder="DeepSeek Reasoner" /></Form.Item></Col></Row><Row gutter={12}><Col span={8}><Form.Item name="enabled" label={s.agent.epEnabled} valuePropName="checked"><Switch /></Form.Item></Col><Col span={8}><Form.Item name="is_default" label={s.agent.epDefault} valuePropName="checked"><Switch /></Form.Item></Col><Col span={8}><Form.Item name="max_tokens" label={s.agent.epMaxTokens}><InputNumber min={1} max={1000000} style={{ width: '100%' }} /></Form.Item></Col></Row><Form.Item name="temperature" label={s.agent.epTemperature}><InputNumber min={0} max={2} step={0.1} style={{ width: '100%' }} /></Form.Item><Form.Item name="remark" label={s.agent.epRemark}><Input /></Form.Item></Form></Modal>
  </Space>
}
