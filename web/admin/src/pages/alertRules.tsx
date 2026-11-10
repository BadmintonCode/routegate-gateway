import { useEffect, useState } from 'react'
import { Button, Card, Form, Input, Modal, Popconfirm, Select, Space, Switch, Table, Tabs, Tag, Tooltip, Typography, message } from 'antd'
import { del, get, post, put } from '../api'
import type { Strings } from '../i18n'
import { buildRule, NUMERIC_OPS, OP_OPTIONS, SINGLE_VALUE_OPS, useLang } from '../shared'

const { Text } = Typography

// --- Alert rules ---

// alertLeafFromRow normalizes one condition row into a wire leaf (or null
// when the row is empty). Values are always stored as string arrays.
const alertLeafFromRow = (r: any) => {
  if (!r || !r.source) return null
  const vals = (Array.isArray(r.values) ? r.values : [r.values]).map((x: any) => String(x).trim()).filter(Boolean)
  return {
    source: r.source,
    path: r.source === 'body' ? (r.path || '') : undefined,
    op: r.op || 'in',
    values: SINGLE_VALUE_OPS.includes(r.op || 'in') ? vals.slice(0, 1) : vals,
  }
}
// opOptionsFor filters the operator list by source: the outcome verdict is a
// two-value enum, so only membership operators apply.
const opOptionsFor = (source: string, s: Strings) =>
  source === 'outcome'
    ? [{ value: 'in', label: s.routes.ruleIn }, { value: 'not_in', label: s.routes.ruleNotIn }]
    : OP_OPTIONS(s)

// AlertRulesPage manages composable alert rules (scope, one channel,
// conditions incl. the outcome source fed by route success rules) and shows
// the firing records. No noise suppression in v1 (owner decision).
export function AlertRulesPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [events, setEvents] = useState<any[]>([])
  const [modal, setModal] = useState<any>(null)
  const [routes, setRoutes] = useState<any[]>([])
  const [channels, setChannels] = useState<any[]>([])
  const [eventRule, setEventRule] = useState<number | ''>('')
  const [form] = Form.useForm()
  // Match document editing: the form tab handles flat rules; nested documents
  // are edited as JSON (the authoritative source at save time).
  const [matchTab, setMatchTab] = useState('form')
  const [matchJson, setMatchJson] = useState('')
  const load = () => Promise.all([get('/alerts/rules'), get('/alerts/events?page_size=20')]).then(([r, e]) => {
    setItems(r.items || [])
    setEvents(e.items || [])
  })
  const loadRefs = () => Promise.all([get('/routes'), get('/alerts/channels')]).then(([r, c]) => {
    setRoutes(r.items || [])
    setChannels((c.items || []).filter((x: any) => x.enabled))
  })
  useEffect(() => { load(); loadRefs() }, [])
  const open = (record?: any) => {
    setModal(record || {})
    const doc: any = record?.match || {}
    const nested = (doc.children || []).some((c: any) => (c.children || []).length > 0)
    setMatchTab(nested ? 'json' : 'form')
    setMatchJson(JSON.stringify(doc && Object.keys(doc).length ? doc : { mode: 'all', children: [] }, null, 2))
    form.setFieldsValue(record ? {
      name: record.name, enabled: !!record.enabled, scope_type: record.scope_type,
      route_ids: record.route_ids || [], channel_id: record.channel_id,
      match_mode: doc.mode || 'all',
      condRows: !nested ? (doc.children || []).map((c: any) => (c.children || []).length > 0
        ? { kind: 'group', mode: c.mode || 'all', rows: c.children.map((g: any) => ({ source: g.source, path: g.path || '', op: g.op, values: g.values || [] })) }
        : { kind: 'leaf', source: c.source, path: c.path || '', op: c.op, values: c.values || [] }) : [],
    } : { enabled: true, scope_type: 'all', route_ids: [], channel_id: undefined, match_mode: 'all', condRows: [{ source: 'outcome', path: '', op: 'in', values: ['failure'] }] })
  }
  const save = async (v: any) => {
    try {
      let matchDoc: any
      if (matchTab === 'json') {
        try {
          matchDoc = JSON.parse(matchJson || '{}')
        } catch (e: any) {
          message.error(`${s.routes.ruleJsonInvalid}: ${e.message}`)
          return
        }
      } else {
        const children = (v.condRows || []).filter((c: any) => c && (c.kind === 'group' ? (c.rows || []).length : c.source)).map((c: any) => {
          if (c.kind === 'group') {
            const leaves = (c.rows || []).map((g: any) => alertLeafFromRow(g)).filter(Boolean)
            return { mode: c.mode || 'all', children: leaves }
          }
          return alertLeafFromRow(c)
        }).filter(Boolean)
        matchDoc = { mode: v.match_mode || 'all', children }
      }
      const payload = {
        name: v.name, enabled: !!v.enabled, scope_type: v.scope_type,
        route_ids: v.scope_type === 'routes' ? (v.route_ids || []).map(Number) : [],
        channel_id: Number(v.channel_id), match_mode: matchDoc.mode || 'all',
        conditions: matchDoc,
      }
      if (modal?.id) await put(`/alerts/rules/${modal.id}`, payload)
      else await post('/alerts/rules', payload)
      message.success(s.msg.saved); setModal(null); load()
    } catch (e: any) { message.error(e.message) }
  }
  const remove = (rec: any) =>
    del(`/alerts/rules/${rec.id}`)
      .then(() => { message.success(s.msg.deleted); load() })
      .catch((e: any) => message.error(e.message))
  const cols: any[] = [
    { title: s.alertRules.name, dataIndex: 'name' },
    { title: s.alerts.enabled, dataIndex: 'enabled', render: (v: boolean) => (v ? <Tag color="green">{s.misc.yes}</Tag> : <Tag>{s.misc.no}</Tag>) },
    { title: s.alertRules.scope, dataIndex: 'scope_type', render: (v: string, rec: any) =>
      v === 'all' ? s.alertRules.scopeAll : (rec.route_ids || []).length },
    { title: s.alertRules.channel, dataIndex: 'channel_id' },
    { title: s.alertRules.matchMode, dataIndex: 'match_mode', render: (v: string) => (v === 'any' ? s.routes.ruleModeAny : s.routes.ruleModeAll) },
    { title: s.audit.time, dataIndex: 'updated_at' },
    {
      title: s.audit.detail, render: (_: any, rec: any) => (
        <Space>
          <Button size="small" disabled={!isSuper} onClick={() => open(rec)}>{s.action.edit}</Button>
          <Popconfirm title={s.alertRules.confirmDelete} onConfirm={() => remove(rec)}>
            <Button size="small" danger disabled={!isSuper}>{s.action.delete}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]
  const eventCols: any[] = [
    { title: s.audit.time, dataIndex: 'created_at', width: 170 },
    { title: s.alertRules.ruleName, dataIndex: 'rule_name' },
    { title: s.pol.route, dataIndex: 'route_name' },
    { title: s.alertRules.requestId, dataIndex: 'request_id' },
    { title: s.audit.status, dataIndex: 'status' },
    { title: s.audit.outcome, dataIndex: 'outcome', render: (v: string) => v || '—' },
    { title: s.audit.bizCode, dataIndex: 'biz_code', render: (v: string) => v || '—' },
    { title: s.audit.bizMessage, dataIndex: 'biz_message', ellipsis: { showTitle: true }, render: (v: string) => v || '—' },
    { title: s.alerts.enabled, dataIndex: 'sent', render: (v: boolean) => (v ? <Tag color="green">{s.alertRules.sent}</Tag> : <Tooltip title="send_error"><Tag color="orange">{s.alertRules.unsent}</Tag></Tooltip>) },
    { title: s.alertRules.sendError, dataIndex: 'send_error', ellipsis: { showTitle: true }, render: (v: string) => v || '—' },
  ]
  return (
    <Card>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" disabled={!isSuper} onClick={() => open()}>{s.alertRules.new}</Button>
        <Button onClick={() => load()}>{s.audit.refresh}</Button>
        <Select allowClear placeholder={s.alertRules.ruleFilter} style={{ width: 200 }}
          value={eventRule === '' ? undefined : eventRule}
          onChange={(v) => {
            const id = v === undefined ? '' : v
            setEventRule(id)
            get(`/alerts/events?page_size=20${id === '' ? '' : `&rule_id=${id}`}`).then((e: any) => setEvents(e.items || []))
          }}
          options={items.map((r: any) => ({ value: r.id, label: r.name }))} />
      </Space>
      <Table rowKey="id" dataSource={items} columns={cols} size="small" pagination={false} locale={{ emptyText: s.alertRules.empty }} />
      <Card type="inner" title={s.alertRules.eventsTitle} style={{ marginTop: 16 }}>
        <Table rowKey="id" dataSource={events} columns={eventCols} size="small"
          pagination={{ pageSize: 10 }} locale={{ emptyText: s.alertRules.eventsEmpty }} />
      </Card>
      <Modal title={modal?.id ? s.alertRules.edit : s.alertRules.new} open={!!modal} onCancel={() => setModal(null)} onOk={() => form.submit()} width={640}
        styles={{ body: { maxHeight: '62vh', overflowY: 'auto', paddingRight: 4 } }}>
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item name="name" label={s.alertRules.name} rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="enabled" label={s.alerts.enabled} valuePropName="checked"><Switch /></Form.Item>
          <Form.Item name="scope_type" label={s.alertRules.scope}>
            <Select onChange={() => form.setFieldValue('route_ids', [])}
              options={[{ value: 'all', label: s.alertRules.scopeAll }, { value: 'routes', label: s.alertRules.scopeRoutes }]} />
          </Form.Item>
          <Form.Item noStyle shouldUpdate={(prev, cur) => prev.scope_type !== cur.scope_type}>
            {() => form.getFieldValue('scope_type') === 'routes' ? (
              <Form.Item name="route_ids" label={s.alertRules.scopeRoutes} rules={[{ required: true, message: s.alertRules.scopeRequired }]}>
                <Select mode="multiple" options={routes.map((r: any) => ({ value: r.id, label: r.name }))} />
              </Form.Item>
            ) : null}
          </Form.Item>
          <Form.Item name="channel_id" label={s.alertRules.channel} rules={[{ required: true }]}>
            <Select options={channels.map((c: any) => ({ value: c.id, label: c.name }))} />
          </Form.Item>
          <Tabs size="small" activeKey={matchTab} onChange={(k) => {
            if (k === 'json') {
              const doc: any = buildRule({ ruleMode: form.getFieldValue('match_mode'), ruleRows: form.getFieldValue('condRows') }) ?? {}
              setMatchJson(JSON.stringify({ mode: doc.mode || 'all', children: doc.conditions || [] }, null, 2))
            } else {
              try {
                const doc = JSON.parse(matchJson || '{}')
                if ((doc.children || []).some((c: any) => (c.children || []).length > 0)) {
                  message.warning(s.alertRules.matchNestedNotice)
                }
                form.setFieldsValue({ match_mode: doc.mode || 'all', condRows: (doc.children || []).map((c: any) => ({ source: c.source, path: c.path || '', op: c.op || 'in', values: c.values || [] })) })
              } catch (e: any) {
                message.error(`${s.routes.ruleJsonInvalid}: ${e.message}`)
                setMatchTab('json')
              }
            }
            setMatchTab(k)
          }}
            items={[{ key: 'form', label: s.routes.ruleFormTab }, { key: 'json', label: s.routes.ruleJsonTab }]} />
          {matchTab === 'json' ? (
            <>
              <Form.Item style={{ marginBottom: 8 }}>
                <Input.TextArea value={matchJson} onChange={(e) => setMatchJson(e.target.value)}
                  rows={10} style={{ fontFamily: 'monospace', fontSize: 12 }} />
              </Form.Item>
              <Space style={{ marginBottom: 8 }}>
                <Button size="small" onClick={async () => {
                  try { await navigator.clipboard.writeText(matchJson); message.success(s.routes.ruleCopied) }
                  catch { message.error(s.routes.ruleCopyFail) }
                }}>{s.routes.ruleCopy}</Button>
                <Text type="secondary">{s.alertRules.matchJsonHint}</Text>
              </Space>
            </>
          ) : (
            <>
          <Form.Item name="match_mode" label={s.alertRules.matchMode}>
            <Select options={[{ value: 'all', label: s.routes.ruleModeAll }, { value: 'any', label: s.routes.ruleModeAny }]} />
          </Form.Item>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>{s.alertRules.conditionsHint}</Typography.Paragraph>
          <Form.Item label={s.routes.ruleConditions}>
            <Form.List name="condRows">
              {(fields, { add, remove }) => (
                <>
                  {fields.map(({ key, name }) => (
                    <Form.Item noStyle key={key} shouldUpdate={(prev, cur) => JSON.stringify(prev.condRows?.[name]) !== JSON.stringify(cur.condRows?.[name])}>
                      {() => {
                        const row = form.getFieldValue(['condRows', name]) || {}
                        if (row.kind === 'group') {
                          return (
                            <div key={key} style={{ border: '1px solid #d9d9d9', borderRadius: 6, padding: '8px 8px 4px', marginBottom: 6 }}>
                              <Space style={{ marginBottom: 6 }} align="baseline">
                                <Form.Item name={[name, 'mode']} style={{ marginBottom: 0 }}>
                                  <Select size="small" style={{ width: 150 }}
                                    options={[{ value: 'all', label: s.routes.ruleModeAll }, { value: 'any', label: s.routes.ruleModeAny }]} />
                                </Form.Item>
                                <Text type="secondary" style={{ fontSize: 12 }}>{s.alertRules.subgroup}</Text>
                                <Button type="text" danger size="small" onClick={() => remove(name)}>{s.action.delete}</Button>
                              </Space>
                              <Form.List name={[name, 'rows']}>
                                {(gfields, { add: gadd, remove: gremove }) => (
                                  <>
                                    {gfields.map(({ key: gkey, name: gname }) => (
                                      <div key={gkey} style={{ display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'nowrap', marginBottom: 4 }}>
                                        <Form.Item name={[gname, 'source']} initialValue="status" style={{ marginBottom: 0, flexShrink: 0 }}>
                                          <Select size="small" style={{ width: 108 }} onChange={(src) => {
                                            const cur = form.getFieldValue(['condRows', name, 'rows', gname, 'op'])
                                            if (src === 'outcome' && cur !== 'in' && cur !== 'not_in') {
                                              form.setFieldValue(['condRows', name, 'rows', gname, 'op'], 'in')
                                            }
                                          }} options={[
                                            { value: 'outcome', label: s.alertRules.sourceOutcome },
                                            { value: 'status', label: s.routes.ruleSourceStatus },
                                            { value: 'body', label: s.routes.ruleSourceBody },
                                            { value: 'biz_code', label: s.alertRules.sourceBizCode },
                                            { value: 'biz_message', label: s.alertRules.sourceBizMessage },
                                          ]} />
                                        </Form.Item>
                                        <Form.Item noStyle shouldUpdate={(prev, cur) => prev.condRows?.[name]?.rows?.[gname]?.source !== cur.condRows?.[name]?.rows?.[gname]?.source}>
                                          {() => {
                                            const grow = form.getFieldValue(['condRows', name, 'rows', gname]) || {}
                                            return (
                                              <>
                                                {grow.source === 'body' && (
                                                  <Form.Item name={[gname, 'path']} style={{ marginBottom: 0, flexShrink: 0 }}>
                                                    <Input size="small" placeholder="code" style={{ width: 100 }} />
                                                  </Form.Item>
                                                )}
                                                <Form.Item name={[gname, 'op']} initialValue="in" style={{ marginBottom: 0, flexShrink: 0 }}>
                                                  <Select size="small" style={{ width: 96 }} options={opOptionsFor(grow.source, s)} />
                                                </Form.Item>
                                                <Form.Item name={[gname, 'values']} style={{ marginBottom: 0, flex: 1, minWidth: 120 }}
                                                  getValueProps={(v) => ({ value: Array.isArray(v) ? (v[0] ?? '') : (v ?? '') })}
                                                  getValueFromEvent={(e) => [e.target.value]}>
                                                  {grow.source === 'outcome' ? (
                                                    <Select size="small" mode="multiple" style={{ width: '100%' }}
                                                      placeholder={s.alertRules.outcomeValuesHint}
                                                      options={[{ value: 'success', label: s.audit.outcomeSuccess }, { value: 'failure', label: s.audit.outcomeFailure }]} />
                                                  ) : (
                                                    <Input size="small" placeholder={NUMERIC_OPS.includes(grow.op) ? s.routes.ruleValueNumber : s.routes.ruleValueOne} />
                                                  )}
                                                </Form.Item>
                                                <Button type="text" danger size="small" onClick={() => gremove(gname)}>{s.action.delete}</Button>
                                              </>
                                            )
                                          }}
                                        </Form.Item>
                                      </div>
                                    ))}
                                    <Button type="dashed" size="small" onClick={() => gadd({ source: 'status', op: 'in', values: [] })}>{s.alertRules.addCondition}</Button>
                                  </>
                                )}
                              </Form.List>
                            </div>
                          )
                        }
                        return (
                          <div key={key} style={{ display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'nowrap', marginBottom: 6 }}>
                            <Form.Item name={[name, 'source']} initialValue="outcome" style={{ marginBottom: 0, flexShrink: 0 }}>
                              <Select style={{ width: 120 }} onChange={(src) => {
                                const cur = form.getFieldValue(['condRows', name, 'op'])
                                if (src === 'outcome' && cur !== 'in' && cur !== 'not_in') {
                                  form.setFieldValue(['condRows', name, 'op'], 'in')
                                }
                              }} options={[
                                { value: 'outcome', label: s.alertRules.sourceOutcome },
                                { value: 'status', label: s.routes.ruleSourceStatus },
                                { value: 'body', label: s.routes.ruleSourceBody },
                                { value: 'biz_code', label: s.alertRules.sourceBizCode },
                                { value: 'biz_message', label: s.alertRules.sourceBizMessage },
                              ]} />
                            </Form.Item>
                            <Form.Item noStyle shouldUpdate={(prev, cur) => prev.condRows?.[name]?.source !== cur.condRows?.[name]?.source}>
                              {() => {
                                const leaf = form.getFieldValue(['condRows', name]) || {}
                                return (
                                  <>
                                    {leaf.source === 'body' && (
                                      <Form.Item name={[name, 'path']} rules={[{ required: true, message: s.routes.rulePathRequired }]} style={{ marginBottom: 0, flexShrink: 0 }}>
                                        <Input placeholder="code" style={{ width: 110 }} />
                                      </Form.Item>
                                    )}
                                    <Form.Item name={[name, 'op']} initialValue="in" style={{ marginBottom: 0, flexShrink: 0 }}>
                                      <Select style={{ width: 104 }} options={opOptionsFor(leaf.source, s)} />
                                    </Form.Item>
                                    <Form.Item name={[name, 'values']} style={{ marginBottom: 0, flex: 1, minWidth: 140 }}>
                                      {leaf.source === 'outcome' ? (
                                        <Select mode="multiple" style={{ width: '100%' }}
                                          placeholder={s.alertRules.outcomeValuesHint}
                                          options={[{ value: 'success', label: s.audit.outcomeSuccess }, { value: 'failure', label: s.audit.outcomeFailure }]} />
                                      ) : (
                                        <Select mode="tags" open={false} tokenSeparators={[',']}
                                          style={{ width: '100%' }}
                                          placeholder={s.routes.ruleValuesHint} />
                                      )}
                                    </Form.Item>
                                  </>
                                )
                              }}
                            </Form.Item>
                            <Button type="text" danger size="small" style={{ flexShrink: 0 }} onClick={() => remove(name)}>{s.action.delete}</Button>
                          </div>
                        )
                      }}
                    </Form.Item>
                  ))}
                  <Space style={{ marginBottom: 8 }}>
                    <Button type="dashed" onClick={() => add({ kind: 'leaf', source: 'outcome', path: '', op: 'in', values: ['failure'] })}>{s.alertRules.addCondition}</Button>
                    <Button type="dashed" onClick={() => add({ kind: 'group', mode: 'any', rows: [{ source: 'status', op: 'in', values: [] }] })}>{s.alertRules.addSubgroup}</Button>
                  </Space>
                </>
              )}
            </Form.List>
          </Form.Item>
            </>
          )}
        </Form>
      </Modal>
    </Card>
  )
}
