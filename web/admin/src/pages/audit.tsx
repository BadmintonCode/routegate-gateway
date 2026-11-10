import { useEffect, useState } from 'react'
import { CopyOutlined, RobotOutlined } from '@ant-design/icons'
import { Alert, Button, Card, Descriptions, Drawer, Input, Popconfirm, Select, Space, Spin, Table, Tabs, Tag, Typography, message } from 'antd'
import { get } from '../api'
import type { Strings } from '../i18n'
import { useLang } from '../shared'

const { Text } = Typography

// --- Audit ---

export function AuditPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [tab, setTab] = useState('access')
  return (
    <Card>
      <Tabs
        activeKey={tab}
        onChange={setTab}
        items={[
          { key: 'access', label: s.audit.tabs.access, children: <AccessLogTab isSuper={isSuper} /> },
          { key: 'admin', label: s.audit.tabs.admin, children: <AdminAuditTab /> },
        ]}
      />
    </Card>
  )
}

function AccessLogTab({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [items, setItems] = useState<any[] | null>(null)
  const [note, setNote] = useState('')
  const [caller, setCaller] = useState('')
  const [decision, setDecision] = useState('')
  const [route, setRoute] = useState('')
  const [outcome, setOutcome] = useState('')
  // Debug detail: superadmin-only, lazily fetched per request. debugEnabled is
  // probed once so the button only appears when the opt-in sink is active.
  const [debugEnabled, setDebugEnabled] = useState(false)
  const [debugFor, setDebugFor] = useState('')
  const [debugRec, setDebugRec] = useState<any | null>(null)
  const [debugLoading, setDebugLoading] = useState(false)
  const load = (c = caller, d = decision, rt = route, oc = outcome) =>
    get(`/logs/access?limit=200${c ? `&caller=${encodeURIComponent(c)}` : ''}${d ? `&decision=${d}` : ''}${rt ? `&route=${encodeURIComponent(rt)}` : ''}${oc ? `&outcome=${oc}` : ''}`)
      .then((r) => { setItems(r.items || []); setNote(r.note || '') })
      .catch((e: any) => message.error(e.message))
  useEffect(() => { load(caller, decision, route, outcome) }, [caller, decision, route, outcome])
  useEffect(() => {
    if (!isSuper) return
    get('/logs/debug?limit=1').then((r: any) => setDebugEnabled(!!r.enabled)).catch(() => setDebugEnabled(false))
  }, [isSuper])
  const openDebug = (reqId: string) => {
    setDebugFor(reqId)
    setDebugRec(null)
    setDebugLoading(true)
    get(`/logs/debug/${encodeURIComponent(reqId)}`)
      .then((r: any) => setDebugRec(r))
      .catch((e: any) => { message.error(e.message); setDebugRec(null) })
      .finally(() => setDebugLoading(false))
  }
  const decisionTag = (d: string) => {
    const color = d === 'allow' ? 'green' : d === 'deny' ? 'red'
      : d === 'unauthenticated' ? 'volcano' : d === 'route_not_found' ? 'orange' : 'default'
    return <Tag color={color}>{(s.dash as any)[d] ?? d}</Tag>
  }
  const outcomeTag = (o: string) => {
    if (!o) return <Text type="secondary">—</Text>
    return o === 'success'
      ? <Tag color="green">{s.audit.outcomeSuccess}</Tag>
      : <Tag color="red">{s.audit.outcomeFailure}</Tag>
  }
  const cols: any[] = [
    { title: s.audit.time, dataIndex: 'ts' },
    { title: s.audit.callerFilter, dataIndex: 'caller', render: (v: string) => v || '—' },
    { title: s.pol.route, dataIndex: 'route', render: (v: string) => v || '—' },
    { title: s.audit.method, dataIndex: 'method' },
    { title: s.audit.path, dataIndex: 'path' },
    { title: s.audit.decision, dataIndex: 'decision', render: decisionTag },
    { title: s.audit.outcome, dataIndex: 'outcome', render: outcomeTag },
    { title: s.audit.bizCode, dataIndex: 'biz_code', render: (v: string) => v || '—' },
    { title: s.audit.bizMessage, dataIndex: 'biz_message', ellipsis: { showTitle: true }, render: (v: string) => v || '—' },
    { title: s.audit.status, dataIndex: 'status', render: (v: number) => v || '—' },
    { title: s.dash.matchedPolicy, dataIndex: 'matched_policy_id', render: (v: any) => v || '—' },
    { title: s.audit.duration, dataIndex: 'duration_ms' },
    { title: s.audit.upstream, dataIndex: 'upstream_host', render: (v: string) => v || '—' },
  ]
  if (isSuper && debugEnabled) {
    cols.push({
      title: '',
      render: (_: any, rec: any) => (
        <Button size="small" onClick={() => openDebug(rec.request_id)}>{s.audit.debugDetail}</Button>
      ),
    })
  }
  return (
    <>
      <Space style={{ marginBottom: 12 }} wrap>
        <Input.Search placeholder={s.audit.callerFilter} allowClear onSearch={(v) => setCaller(v)} style={{ width: 180 }} />
        <Input.Search placeholder={s.audit.routeFilter} allowClear onSearch={(v) => setRoute(v)} style={{ width: 160 }} />
        <Select allowClear placeholder={s.audit.decision} style={{ width: 160 }}
          value={decision || undefined}
          onChange={(v) => setDecision(v || '')}
          options={['allow', 'deny', 'unauthenticated', 'route_not_found', 'upstream_error'].map((d) => ({ value: d, label: (s.dash as any)[d] ?? d }))} />
        <Select allowClear placeholder={s.audit.outcome} style={{ width: 140 }}
          value={outcome || undefined}
          onChange={(v) => setOutcome(v || '')}
          options={[{ value: 'success', label: s.audit.outcomeSuccess }, { value: 'failure', label: s.audit.outcomeFailure }]} />
        <Button onClick={() => load(caller, decision, route, outcome)}>{s.audit.refresh}</Button>
      </Space>
      {note && <Alert type="info" showIcon message={note} style={{ marginBottom: 12 }} />}
      <Table rowKey={(r: any) => (r.ts + r.request_id)} dataSource={items || []} columns={cols} size="small"
        loading={items === null}
        locale={{ emptyText: s.audit.empty }}
        pagination={{ pageSize: 20 }} />
      <Drawer title={`${s.audit.debugDetail} · ${debugFor}`} width={720} open={!!debugFor} onClose={() => setDebugFor('')}>
        <Alert type="warning" showIcon message={s.audit.debugWarn} style={{ marginBottom: 12 }} />
        {debugLoading ? <Spin /> : debugRec ? <DebugDetail rec={debugRec} s={s} /> : <Text type="secondary">{s.audit.debugNoRecord}</Text>}
      </Drawer>
    </>
  )
}

// DebugDetail renders one sanitized debug record. Everything goes through text
// nodes / <pre> — never dangerouslySetInnerHTML.
function DebugDetail({ rec, s }: { rec: any; s: Strings }) {
  const copyDetails = async () => {
    try {
      if (!navigator.clipboard) throw new Error('clipboard unavailable')
      await navigator.clipboard.writeText(formatDebugDetails(rec, s))
      message.success(s.audit.debugCopied)
    } catch {
      message.error(s.audit.debugCopyFail)
    }
  }
  const copyAIPrompt = async () => {
    try {
      if (!navigator.clipboard) throw new Error('clipboard unavailable')
      await navigator.clipboard.writeText(`${s.audit.debugAIPromptIntro}\n\n${formatDebugDetails(rec, s)}`)
      message.success(s.audit.debugAIPromptCopied)
    } catch {
      message.error(s.audit.debugCopyFail)
    }
  }
  return (
    <Space direction="vertical" style={{ width: '100%' }} size={10}>
      <Space wrap>
        <Button icon={<CopyOutlined />} onClick={copyDetails}>{s.audit.debugCopy}</Button>
        <Button icon={<RobotOutlined />} onClick={copyAIPrompt}>{s.audit.debugAIPromptCopy}</Button>
      </Space>
      <Descriptions size="small" column={2} bordered>
        <Descriptions.Item label={s.audit.time}>{rec.ts}</Descriptions.Item>
        <Descriptions.Item label={s.audit.requestId}>{rec.request_id || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.instance}>{rec.instance_id || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.configRevision}>{rec.config_revision ?? '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.callerFilter}>{rec.caller || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.route}>{rec.route || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.method}>{rec.method || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.path} span={2}>{rec.path || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.decision}><Tag>{rec.decision || '—'}</Tag></Descriptions.Item>
        <Descriptions.Item label={s.audit.outcome}>{rec.outcome || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.status}>{rec.status || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.duration}>{rec.duration_ms ?? '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.matchedPolicy}>{rec.matched_policy_id || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.upstream}>{rec.upstream_host || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.bizCode}>{rec.biz_code || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.bizMessage}>{rec.biz_message || '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.requestBytes}>{rec.request_bytes ?? '—'}</Descriptions.Item>
        <Descriptions.Item label={s.audit.responseBytes}>{rec.response_bytes ?? '—'}</Descriptions.Item>
      </Descriptions>
      <PayloadView p={rec.request_body} label={s.audit.debugReqBody} s={s} />
      <PayloadView p={rec.response_body} label={s.audit.debugRespBody} s={s} />
      <PreSection title={s.audit.debugReqHeaders} value={rec.request_headers} />
      <PreSection title={s.audit.debugRespHeaders} value={rec.response_headers} />
      <PreSection title={s.audit.debugReqCookies} value={rec.request_cookies} />
      <PreSection title={s.audit.debugRespCookies} value={rec.response_cookies} />
    </Space>
  )
}

function formatDebugDetails(rec: any, s: Strings): string {
  const value = (v: any) => v === undefined || v === null || v === '' ? '—' : String(v)
  const objectValue = (v: any) => v && Object.keys(v).length ? JSON.stringify(v, null, 2) : '—'
  const payloadValue = (p: any) => {
    if (!p) return '—'
    const details = [p.content_type, p.content_encoding, p.encoding, p.truncated ? s.audit.debugTruncated : '', p.unavailable ? s.audit.debugUnavailable : ''].filter(Boolean).join(', ')
    return `${details ? `[${details}]\n` : ''}${p.unavailable ? '—' : (p.data || '(empty)')}`
  }
  const metadata: Array<[string, any]> = [
    [s.audit.time, rec.ts], [s.audit.requestId, rec.request_id], [s.audit.instance, rec.instance_id],
    [s.audit.configRevision, rec.config_revision], [s.audit.callerFilter, rec.caller], [s.audit.route, rec.route],
    [s.audit.method, rec.method], [s.audit.path, rec.path], [s.audit.decision, rec.decision],
    [s.audit.outcome, rec.outcome], [s.audit.status, rec.status], [s.audit.duration, rec.duration_ms],
    [s.audit.matchedPolicy, rec.matched_policy_id], [s.audit.upstream, rec.upstream_host],
    [s.audit.bizCode, rec.biz_code], [s.audit.bizMessage, rec.biz_message],
    [s.audit.requestBytes, rec.request_bytes], [s.audit.responseBytes, rec.response_bytes],
  ]
  return [
    ...metadata.map(([label, v]) => `${label}: ${value(v)}`),
    '', `${s.audit.debugReqHeaders}:\n${objectValue(rec.request_headers)}`,
    '', `${s.audit.debugRespHeaders}:\n${objectValue(rec.response_headers)}`,
    '', `${s.audit.debugReqCookies}:\n${objectValue(rec.request_cookies)}`,
    '', `${s.audit.debugRespCookies}:\n${objectValue(rec.response_cookies)}`,
    '', `${s.audit.debugReqBody}:\n${payloadValue(rec.request_body)}`,
    '', `${s.audit.debugRespBody}:\n${payloadValue(rec.response_body)}`,
  ].join('\n')
}

function PayloadView({ p, label, s }: { p: any; label: string; s: Strings }) {
  if (!p) return null
  return (
    <>
      <Text strong>{label}</Text>
      {p.content_type ? <Tag style={{ marginLeft: 8 }}>{p.content_type}</Tag> : null}
      {p.content_encoding ? <Tag style={{ marginLeft: 4 }}>{p.content_encoding}</Tag> : null}
      {p.truncated ? <Tag color="orange" style={{ marginLeft: 4 }}>{s.audit.debugTruncated}</Tag> : null}
      {p.unavailable ? <Tag style={{ marginLeft: 4 }}>{s.audit.debugUnavailable}</Tag> : null}
      {!p.unavailable && p.encoding === 'base64' && p.data ? <Tag style={{ marginLeft: 4 }}>{s.audit.debugBase64}</Tag> : null}
      <pre style={{ maxHeight: 260, overflow: 'auto', fontSize: 12, background: '#fafafa', padding: 8, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
        {p.unavailable ? '—' : (p.data || '(empty)')}
      </pre>
    </>
  )
}

function PreSection({ title, value }: { title: string; value: any }) {
  const empty = !value || Object.keys(value).length === 0
  return (
    <>
      <Text strong>{title}</Text>
      <pre style={{ maxHeight: 220, overflow: 'auto', fontSize: 12, background: '#fafafa', padding: 8 }}>
        {empty ? '—' : JSON.stringify(value, null, 2)}
      </pre>
    </>
  )
}

function AdminAuditTab() {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [action, setAction] = useState('')
  const load = (p = page, a = action) =>
    get(`/audit/admin?page=${p}&page_size=20${a ? `&action=${a}` : ''}`)
      .then((r) => { setItems(r.items || []); setTotal(r.total || 0) })
  useEffect(() => { load(page, action) }, [page, action])
  const cols = [
    { title: s.audit.time, dataIndex: 'created_at' },
    { title: s.audit.actor, dataIndex: 'actor' },
    { title: s.audit.action, dataIndex: 'action', render: (a: string) => <Tag>{a}</Tag> },
    { title: s.audit.resource, dataIndex: 'resource_id' },
    {
      title: s.audit.detail, render: (_: any, rec: any) => (
        <Popconfirm title={<pre style={{ maxHeight: 300, overflow: 'auto', maxWidth: 480 }}>{JSON.stringify({ before: rec.before_state, after: rec.after_state }, null, 2)}</pre>}
          okButtonProps={{ style: { display: 'none' } }} cancelButtonProps={{ style: { display: 'none' } }}>
          <Button size="small">{s.action.view}</Button>
        </Popconfirm>
      ),
    },
  ]
  return (
    <>
      <Space style={{ marginBottom: 16 }}>
        <Input.Search placeholder={s.audit.search} onSearch={(v) => { setAction(v); setPage(1) }} style={{ width: 260 }} />
      </Space>
      <Table rowKey="id" dataSource={items} columns={cols} size="small"
        pagination={{ current: page, total, pageSize: 20, onChange: (p) => setPage(p) }} />
    </>
  )
}
