import { useEffect, useMemo, useState } from 'react'
import { AutoComplete, Button, Card, Form, Input, Modal, Popconfirm, Select, Space, Table, Tag, Typography, message } from 'antd'
import { del, get, post, put } from '../api'
import { useLang } from '../shared'

const { Text } = Typography

// --- Policies ---

export function PoliciesPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [routes, setRoutes] = useState<any[]>([])
  const [identities, setIdentities] = useState<any[]>([])
  const [modal, setModal] = useState<any>(null)
  const [form] = Form.useForm()
  const load = () => Promise.all([get('/policies'), get('/routes'), get('/identities')])
    .then(([p, r, i]) => { setItems(p.items || []); setRoutes(r.items || []); setIdentities(i.items || []) })
  useEffect(() => { load() }, [])
  const selectedRouteId = Form.useWatch('route_id', form)
  const [routeOperations, setRouteOperations] = useState<any[]>([])
  useEffect(() => {
    if (!selectedRouteId) { setRouteOperations([]); return }
    get(`/routes/${selectedRouteId}/operations`).then((r) => setRouteOperations(r.items || [])).catch(() => setRouteOperations([]))
  }, [selectedRouteId])

  // Matrix view: caller × route aggregation (ADR §9).
  const matrix = useMemo(() => {
    const callers = Array.from(new Set(items.map((p) => p.caller)))
    const routeNames = Array.from(new Set(items.map((p) => p.route)))
    const cells: Record<string, Record<string, any[]>> = {}
    items.forEach((p) => {
      cells[p.caller] = cells[p.caller] || {}
      cells[p.caller][p.route] = cells[p.caller][p.route] || []
      cells[p.caller][p.route].push(p)
    })
    return { callers, routeNames, cells }
  }, [items])
  const matrixScrollWidth = Math.max(720, 184 + matrix.routeNames.length * 176)

  const open = (record?: any) => {
    setModal(record || {})
    form.setFieldsValue(record || { method: '*', path_pattern: '/v1/**', effect: 'allow' })
  }
  const save = async (v: any) => {
    try {
      if (modal?.id) await put(`/policies/${modal.id}`, { ...v, expected_revision: 0 })
      else await post('/policies', { ...v, expected_revision: 0 })
      message.success(s.msg.saved); setModal(null); load()
    } catch (e: any) { message.error(e.message) }
  }
  const cols = [
    { title: s.pol.route, dataIndex: 'route' },
    { title: s.pol.caller, dataIndex: 'caller', render: (c: string) => (c === '*' ? <Tag color="orange">*</Tag> : c) },
    { title: s.pol.method, dataIndex: 'method' },
    { title: s.pol.pattern, dataIndex: 'path_pattern' },
    { title: s.pol.effect, dataIndex: 'effect', render: (e: string) => <Tag color={e === 'allow' ? 'green' : 'red'}>{e === 'allow' ? s.dash.allow : s.dash.deny}</Tag> },
    {
      title: s.pol.expires, dataIndex: 'expires_at', render: (v: string, rec: any) =>
        v ? <Tag color={rec.expiring ? 'volcano' : 'default'}>{v}</Tag> : '—',
    },
    {
      title: s.action.edit, render: (_: any, rec: any) => (
        <Space>
          <Button size="small" disabled={!isSuper} onClick={() => open(rec)}>{s.action.edit}</Button>
          <Popconfirm title={s.pol.confirmDelete} onConfirm={async () => {
            try { await del(`/policies/${rec.id}`); message.success(s.msg.deleted); load() }
            catch (e: any) { message.error(e.message) }
          }}><Button size="small" danger disabled={!isSuper}>{s.action.delete}</Button></Popconfirm>
        </Space>
      ),
    },
  ]
  return (
    <>
      <Card title={s.pol.matrixTitle} size="small" style={{ marginBottom: 16 }}>
        <Table
          size="small" pagination={false} rowKey="caller"
          dataSource={matrix.callers.map((c) => ({ caller: c }))}
          tableLayout="fixed"
          scroll={{ x: matrixScrollWidth }}
          columns={[
            { title: s.pol.caller, dataIndex: 'caller', key: 'caller', width: 184, fixed: 'left' as const },
            ...matrix.routeNames.map((r) => ({
              title: <span style={{ display: 'block', lineHeight: 1.35, overflowWrap: 'anywhere' }}>{r}</span>,
              key: `route-${r}`,
              width: 176,
              render: (_: any, row: any) => {
                const rules = matrix.cells[row.caller]?.[r] || []
                return rules.length ? (
                  <Space size={4}>{rules.map((x) => (
                    <Tag key={x.id} color={x.effect === 'allow' ? 'green' : 'red'}>{x.method} {x.path_pattern}</Tag>
                  ))}</Space>
                ) : <Text type="secondary">—</Text>
              },
            })),
          ]}
        />
      </Card>
      <Button type="primary" disabled={!isSuper} onClick={() => open()} style={{ marginBottom: 16 }}>{s.pol.new}</Button>
      <Table rowKey="id" dataSource={items} columns={cols} size="small" />
      <Modal title={modal?.id ? s.pol.edit : s.pol.new} open={!!modal} onCancel={() => setModal(null)} onOk={() => form.submit()}>
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item name="route_id" label={s.pol.routeSelect} rules={[{ required: true }]}>
            <Select options={routes.map((r) => ({ value: r.id, label: `${r.name} (${r.path_prefix})` }))} />
          </Form.Item>
          <Form.Item name="caller" label={s.pol.callerSelect} rules={[{ required: true }]}>
            <Select allowClear showSearch options={identities.filter((i) => i.status === 'active').map((i) => ({ value: i.name }))} placeholder="service-a" />
          </Form.Item>
          <Form.Item name="method" label={s.pol.method}><Select options={[{ value: '*' }, ...['GET', 'POST', 'PUT', 'DELETE', 'PATCH'].map((m) => ({ value: m }))]} /></Form.Item>
          <Form.Item name="path_pattern" label={s.pol.pattern} rules={[{ required: true }]}>
            <AutoComplete
              options={routeOperations.map((o) => ({ value: o.path, label: `${o.method} ${o.path}` }))}
              filterOption={false}
              onSelect={(value: string) => {
                const op = routeOperations.find((o) => o.path === value)
                if (op && op.method && op.method !== '*') form.setFieldsValue({ method: op.method })
              }}
              placeholder="/v1/users/**"
            />
          </Form.Item>
          <Form.Item name="effect" label={s.pol.effect}><Select options={[{ value: 'allow', label: s.dash.allow }, { value: 'deny', label: s.dash.deny }]} /></Form.Item>
          <Form.Item name="expires_at" label={`${s.pol.expires} (${s.pol.expiresHint})`}><Input placeholder="2027-01-01" /></Form.Item>
          <Form.Item name="remark" label={s.routes.remark}><Input /></Form.Item>
        </Form>
      </Modal>
    </>
  )
}
