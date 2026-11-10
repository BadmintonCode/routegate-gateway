import { useEffect, useState } from 'react'
import { Button, Col, Drawer, Form, Input, InputNumber, Modal, Popconfirm, Row, Select, Space, Switch, Table, Tabs, Tag, Typography, message } from 'antd'
import { del, get, post, put } from '../api'
import { buildRule, NUMERIC_OPS, OP_OPTIONS, SINGLE_VALUE_OPS, useLang } from '../shared'

const { Text } = Typography

// --- Routes ---

export function RoutesPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [modal, setModal] = useState<any>(null)
  const [form] = Form.useForm()
  // Registered-operations drawer (Round 2: managed UI).
  const [opsRoute, setOpsRoute] = useState<any>(null)
  const [ops, setOps] = useState<any[]>([])
  const [opModal, setOpModal] = useState<any>(null)
  const [opForm] = Form.useForm()
  // Certificate drawer (Round 3/4: second entry to the identity store,
  // semantically separate from operations per owner feedback).
  const [certRoute, setCertRoute] = useState<any>(null)
  const [callers, setCallers] = useState<any[]>([])
  const [certModal, setCertModal] = useState(false)
  const [certForm] = Form.useForm()
  // Rule JSON tab state: bidirectional editing of the success rule.
  const [ruleTab, setRuleTab] = useState('form')
  const [ruleJson, setRuleJson] = useState('')
  const load = () => get('/routes').then((r) => setItems(r.items || []))
  useEffect(() => { load() }, [])
  const openOps = (rec: any) => { setOpsRoute(rec); loadOps(rec.id) }
  const loadOps = (routeId: any) => get(`/routes/${routeId}/operations`).then((r) => setOps(r.items || []))
  const openCerts = (rec: any) => { setCertRoute(rec); loadCallers(rec.id) }
  const loadCallers = (routeId: any) => get(`/routes/${routeId}/callers`).then((r) => setCallers(r.items || [])).catch(() => setCallers([]))
  const open = (record?: any) => {
    setModal(record || {})
    form.setFieldsValue(record ? {
      ...record,
      methods: record.methods || [],
      targetRows: (record.targets && record.targets.length ? record.targets : [{ host: '', port: 8080, weight: 1 }]),
      ruleMode: record.success_rule?.mode || 'all',
      ruleRows: record.success_rule?.conditions?.length ? record.success_rule.conditions : [],
      biz_code_path: record.biz_code_path || '',
      biz_message_path: record.biz_message_path || '',
    } : { ruleMode: 'all', ruleRows: [], biz_code_path: '', biz_message_path: '', targetRows: [{ host: '', port: 8080, weight: 1 }] })
  }
  const onRuleTabChange = (k: string) => {
    if (k === 'json') {
      setRuleJson(JSON.stringify(buildRule(form.getFieldsValue()) ?? {}, null, 2))
    } else {
      try {
        const rule = ruleFromJson(ruleJson)
        form.setFieldsValue({ ruleMode: rule.mode, ruleRows: rule.rows })
      } catch (e: any) {
        message.error(`${s.routes.ruleJsonInvalid}: ${e.message}`)
        setRuleTab('form')
      }
    }
    setRuleTab(k)
  }

  const save = async (v: any) => {
    try {
      const payload = {
        name: v.name, path_prefix: v.path_prefix, methods: v.methods || [],
        targets: (v.targetRows || []).map((t: any) => ({ host: t.host, port: Number(t.port), weight: Number(t.weight || 1) })),
        strip_prefix: !!v.strip_prefix, remark: v.remark || '',
        success_rule: buildRule(v),
        biz_code_path: (v.biz_code_path || '').trim(),
        biz_message_path: (v.biz_message_path || '').trim(),
        expected_revision: 0,
      }
      if (modal?.id) await put(`/routes/${modal.id}`, payload)
      else await post('/routes', payload)
      message.success(s.msg.saved)
      setModal(null); load()
    } catch (e: any) { message.error(e.message) }
  }
  const cols = [
    { title: s.routes.name, dataIndex: 'name' },
    { title: s.routes.prefix, dataIndex: 'path_prefix' },
    { title: s.routes.methods, dataIndex: 'methods', render: (m: any) => (m?.length ? m.join(',') : s.routes.all) },
    { title: s.routes.targets, dataIndex: 'targets', render: (t: any) => (t || []).map((x: any) => `${x.host}:${x.port}`).join(', ') },
    { title: s.routes.opsCount, dataIndex: 'ops_count' },
    { title: s.routes.strip, dataIndex: 'strip_prefix', render: (v: boolean) => (v ? s.misc.yes : s.misc.no) },
    { title: s.routes.remark, dataIndex: 'remark' },
    {
      title: s.action.edit, render: (_: any, rec: any) => (
        <Space>
          <Button size="small" onClick={() => openOps(rec)}>{s.routes.opsManage}</Button>
          <Button size="small" onClick={() => openCerts(rec)}>{s.routes.certManage}</Button>
          <Button size="small" disabled={!isSuper} onClick={() => open(rec)}>{s.action.edit}</Button>
          <Popconfirm title={s.routes.confirmDelete} onConfirm={async () => {
            try { await del(`/routes/${rec.id}`); message.success(s.msg.deleted); load() }
            catch (e: any) { message.error(e.message) }
          }}>
            <Button size="small" danger disabled={!isSuper}>{s.action.delete}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]
  return (
    <>
      <Button type="primary" disabled={!isSuper} onClick={() => open()} style={{ marginBottom: 16 }}>{s.routes.new}</Button>
      <Table rowKey="id" dataSource={items} columns={cols} size="small" />
      <Modal title={modal?.id ? s.routes.edit : s.routes.new} width={620} open={!!modal} onCancel={() => setModal(null)} onOk={() => form.submit()}
        styles={{ body: { maxHeight: '62vh', overflowY: 'auto', paddingRight: 4 } }}>
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item name="name" label={s.routes.name} rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="path_prefix" label={s.routes.prefix} rules={[{ required: true }]}><Input placeholder="/alpha" /></Form.Item>
          <Form.Item name="methods" label={s.routes.methods}>
            <Select mode="multiple" options={['GET', 'POST', 'PUT', 'DELETE', 'PATCH'].map((m) => ({ value: m }))} />
          </Form.Item>
          <Form.Item label={s.routes.targets} required>
            <Form.List name="targetRows">
              {(fields, { add, remove }) => (
                <>
                  {fields.map(({ key, name, ...restField }) => (
                    <Space key={key} style={{ display: 'flex', marginBottom: 4 }} align="baseline">
                      <Form.Item name={[name, 'host']} rules={[{ required: true, message: s.routes.targetHost }]} style={{ marginBottom: 0 }}>
                        <Input placeholder={s.routes.targetHost} style={{ width: 170 }} />
                      </Form.Item>
                      <Form.Item name={[name, 'port']} rules={[{ required: true, message: s.routes.targetPort }]} style={{ marginBottom: 0 }}>
                        <InputNumber placeholder="8080" min={1} max={65535} style={{ width: 90 }} />
                      </Form.Item>
                      <Form.Item name={[name, 'weight']} initialValue={1} style={{ marginBottom: 0 }}>
                        <InputNumber min={1} style={{ width: 70 }} />
                      </Form.Item>
                      <Button type="text" danger disabled={fields.length <= 1} onClick={() => remove(name)}>{s.action.delete}</Button>
                    </Space>
                  ))}
                  <Button type="dashed" block onClick={() => add({ host: '', port: 8080, weight: 1 })}>{s.routes.addTarget}</Button>
                </>
              )}
            </Form.List>
          </Form.Item>
          <Form.Item name="strip_prefix" label={s.routes.strip} valuePropName="checked"><Switch /></Form.Item>
          <Form.Item name="remark" label={s.routes.remark}><Input /></Form.Item>
          <Row gutter={12}>
            <Col span={12}>
              <Form.Item name="biz_code_path" label={s.routes.bizCodePath}>
                <Input placeholder="code" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="biz_message_path" label={s.routes.bizMessagePath}>
                <Input placeholder="message" />
              </Form.Item>
            </Col>
          </Row>
          <Typography.Paragraph type="secondary" style={{ marginTop: -8, marginBottom: 8 }}>{s.routes.bizHintRoute}</Typography.Paragraph>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>{s.routes.ruleTitle}</Typography.Paragraph>
          <Tabs size="small" activeKey={ruleTab} onChange={onRuleTabChange}
            items={[
              { key: 'form', label: s.routes.ruleFormTab },
              { key: 'json', label: s.routes.ruleJsonTab },
            ]} />
          {ruleTab === 'json' ? (
            <>
              <Form.Item style={{ marginBottom: 8 }}>
                <Input.TextArea value={ruleJson} onChange={(e) => setRuleJson(e.target.value)}
                  rows={8} style={{ fontFamily: 'monospace', fontSize: 12 }} />
              </Form.Item>
              <Space style={{ marginBottom: 8 }}>
                <Button size="small" onClick={async () => {
                  try { await navigator.clipboard.writeText(ruleJson); message.success(s.routes.ruleCopied) }
                  catch { message.error(s.routes.ruleCopyFail) }
                }}>{s.routes.ruleCopy}</Button>
                <Text type="secondary">{s.routes.ruleJsonHint}</Text>
              </Space>
            </>
          ) : (
            <>
          <Form.Item name="ruleMode" label={s.routes.ruleMode}>
            <Select options={[{ value: 'all', label: s.routes.ruleModeAll }, { value: 'any', label: s.routes.ruleModeAny }]} />
          </Form.Item>
          </>
          )}
          <Form.Item label={s.routes.ruleConditions} style={{ marginBottom: 8 }}>
            <Form.List name="ruleRows">
              {(fields, { add, remove }) => (
                <>
                  {fields.map(({ key, name }) => (
                    <div key={key} style={{ display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'nowrap', marginBottom: 6 }}>
                      <Form.Item name={[name, 'source']} initialValue="status" style={{ marginBottom: 0, flexShrink: 0 }}>
                        <Select style={{ width: 108 }}
                          options={[{ value: 'status', label: s.routes.ruleSourceStatus }, { value: 'body', label: s.routes.ruleSourceBody }]} />
                      </Form.Item>
                      <Form.Item noStyle shouldUpdate={(prev, cur) =>
                        prev.ruleRows?.[name]?.source !== cur.ruleRows?.[name]?.source ||
                        prev.ruleRows?.[name]?.op !== cur.ruleRows?.[name]?.op}>
                        {() => {
                          const row = form.getFieldValue(['ruleRows', name]) || {}
                          const single = SINGLE_VALUE_OPS.includes(row.op)
                          return (
                            <>
                              {row.source === 'body' && (
                                <Form.Item name={[name, 'path']} rules={[{ required: true, message: s.routes.rulePathRequired }]} style={{ marginBottom: 0, flexShrink: 0 }}>
                                  <Input placeholder="code" style={{ width: 110 }} />
                                </Form.Item>
                              )}
                              <Form.Item name={[name, 'op']} initialValue="in" style={{ marginBottom: 0, flexShrink: 0 }}>
                                <Select style={{ width: 104 }} options={OP_OPTIONS(s)} />
                              </Form.Item>
                              <Form.Item name={[name, 'values']} style={{ marginBottom: 0, flex: 1, minWidth: 140 }}
                                getValueProps={(v) => ({ value: Array.isArray(v) ? (v[0] ?? '') : (v ?? '') })}
                                getValueFromEvent={(e) => [e.target.value]}>
                                {single
                                  ? <Input placeholder={NUMERIC_OPS.includes(row.op) ? s.routes.ruleValueNumber : s.routes.ruleValueOne} />
                                  : <Select mode="tags" open={false} tokenSeparators={[',']} style={{ width: '100%' }} placeholder={s.routes.ruleValuesHint} />}
                              </Form.Item>
                            </>
                          )
                        }}
                      </Form.Item>
                      <Button type="text" danger size="small" style={{ flexShrink: 0 }} onClick={() => remove(name)}>{s.action.delete}</Button>
                    </div>
                  ))}
                  <Button type="dashed" block onClick={() => add({ source: 'status', op: 'in', values: [] })}>{s.routes.ruleAdd}</Button>
                </>
              )}
            </Form.List>
          </Form.Item>
        </Form>
      </Modal>
      <Drawer
        title={`${s.routes.opsManage} ${s.routes.opsDrawerSuffix} · ${opsRoute?.name || ''}`}
        open={!!opsRoute}
        onClose={() => setOpsRoute(null)}
        width={560}
      >
        <Button type="primary" disabled={!isSuper} onClick={() => {
          setOpModal({ _new: true, route_id: opsRoute?.id })
          opForm.setFieldsValue({ method: 'GET', path: '' })
        }} style={{ marginBottom: 16 }}>{s.routes.opNew}</Button>
        <Table rowKey="id" dataSource={ops} size="small" pagination={false}
          columns={[
            { title: s.routes.opMethod, dataIndex: 'method' },
            { title: s.routes.opPath, dataIndex: 'path', render: (v: string) => <Text code>{v}</Text> },
            { title: s.ident.name, dataIndex: 'name', render: (v: string) => v || '—' },
            { title: s.action.edit, render: (_: any, rec: any) => (
              <Space>
                <Button size="small" disabled={!isSuper} onClick={() => { setOpModal(rec); opForm.setFieldsValue(rec) }}>{s.action.edit}</Button>
                <Popconfirm title={s.routes.confirmDeleteOp} onConfirm={async () => {
                  try { await del(`/operations/${rec.id}`); message.success(s.msg.deleted); loadOps(opsRoute.id); load() }
                  catch (e: any) { message.error(e.message) }
                }}><Button size="small" danger disabled={!isSuper}>{s.action.delete}</Button></Popconfirm>
              </Space>
            )},
          ]}
        />
        <Modal
          title={opModal?.id ? s.routes.opEdit : s.routes.opNew}
          open={!!opModal}
          onCancel={() => setOpModal(null)}
          onOk={() => opForm.submit()}
        >
          <Form form={opForm} layout="vertical" onFinish={async (v) => {
            try {
              const payload = { method: v.method || '*', path: v.path, name: v.name || '', remark: v.remark || '', expected_revision: 0 }
              if (opModal?.id) await put(`/operations/${opModal.id}`, payload)
              else await post(`/routes/${opModal.route_id}/operations`, payload)
              message.success(s.msg.saved)
              setOpModal(null); loadOps(opsRoute.id); load()
            } catch (e: any) { message.error(e.message) }
          }}>
            <Form.Item name="method" label={s.routes.opMethod} initialValue="GET">
              <Select options={[{ value: '*' }, ...['GET', 'POST', 'PUT', 'DELETE', 'PATCH'].map((m) => ({ value: m }))]} />
            </Form.Item>
            <Form.Item name="path" label={s.routes.opPath} rules={[{ required: true }]} extra={s.routes.opHint}>
              <Input placeholder="/v1/users/**" />
            </Form.Item>
            <Form.Item name="name" label={s.routes.opName}><Input /></Form.Item>
            <Form.Item name="remark" label={s.routes.opRemark}><Input /></Form.Item>
          </Form>
        </Modal>
      </Drawer>
      <Drawer
        title={`${s.routes.certManage} · ${certRoute?.name || ''}`}
        open={!!certRoute}
        onClose={() => setCertRoute(null)}
        width={560}
      >
        <Space direction="vertical" style={{ width: '100%' }} size={8}>
          <Text type="secondary" style={{ fontSize: 12 }}>{s.routes.callersHint}</Text>
        </Space>
        <Button type="primary" disabled={!isSuper} onClick={() => setCertModal(true)} style={{ margin: '8px 0' }}>{s.routes.registerCert}</Button>
        <Table rowKey="caller" size="small" pagination={false} dataSource={callers}
          columns={[
            { title: s.pol.caller, dataIndex: 'caller' },
            { title: s.ident.status, dataIndex: 'status', render: (st: string) =>
              st === 'active' ? <Tag color="green">{st}</Tag>
                : st === 'revoked' ? <Tag color="red">{st}</Tag>
                : <Tag color="orange">{s.routes.notRegistered}</Tag> },
            { title: s.ident.notAfter, dataIndex: 'not_after', render: (v: string) => v || '—' },
          ]}
        />
        <Modal title={s.routes.registerCert} open={certModal} onCancel={() => setCertModal(false)} onOk={() => certForm.submit()}>
          <Form form={certForm} layout="vertical" onFinish={async (v) => {
            try {
              await post('/identities', { name: v.name, pem: v.pem, remark: v.remark || '' })
              message.success(s.msg.saved)
              setCertModal(false); certForm.resetFields()
              if (certRoute) loadCallers(certRoute.id)
            } catch (e: any) { message.error(e.message) }
          }}>
            <Form.Item name="name" label={s.routes.identityName} rules={[{ required: true }]}><Input placeholder="service-a" /></Form.Item>
            <Form.Item name="pem" label={s.routes.certPem} rules={[{ required: true }]}><Input.TextArea rows={6} /></Form.Item>
            <Form.Item name="remark" label={s.routes.opRemark}><Input /></Form.Item>
          </Form>
        </Modal>
      </Drawer>
    </>
  )
}

// ruleFromJson maps a pasted JSON document back into form rows. Throws a
// readable error for malformed input; the backend validates the final rule.
const ruleFromJson = (raw: string) => {
  let parsed: any
  try { parsed = JSON.parse(raw || '{}') } catch (e: any) { throw new Error(`invalid JSON: ${e.message}`) }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('expected a JSON object')
  const conds = parsed.conditions
  if (conds !== undefined && !Array.isArray(conds)) throw new Error('"conditions" must be an array')
  const rows = (conds || []).map((c: any) => {
    if (!c || (c.source !== 'status' && c.source !== 'body')) throw new Error(`condition source must be status or body`)
    if (!['in', 'not_in', 'eq', 'neq', 'gt', 'gte', 'lt', 'lte', 'contains', 'not_contains'].includes(c.op)) throw new Error(`unknown op: ${c.op}`)
    const vals = c.values === undefined ? [] : (Array.isArray(c.values) ? c.values : [c.values]).map((x: any) => String(x))
    return { source: c.source, path: c.source === 'body' ? String(c.path || '') : '', op: c.op || 'in', values: vals }
  })
  return { mode: parsed.mode === 'any' ? 'any' : 'all', rows }
}
