import { useEffect, useState } from 'react'
import { Button, Card, Col, Form, Input, Modal, Popconfirm, Row, Select, Space, Switch, Table, Tag, message } from 'antd'
import { del, get, post, put } from '../api'
import { useLang } from '../shared'

// --- Alert channels ---

// AlertChannelsPage manages multiple outbound alert channels (Feishu/Lark).
// Tokens are credentials: the list shows masked values only, edits with an
// empty token keep the stored credential, and every channel has a
// send-test action.
export function AlertChannelsPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [modal, setModal] = useState<any>(null)
  const [testing, setTesting] = useState<number | null>(null)
  const [form] = Form.useForm()
  const load = () => get('/alerts/channels').then((r) => setItems(r.items || []))
  useEffect(() => { load() }, [])
  const open = (record?: any) => {
    setModal(record || {})
    form.setFieldsValue(record ? {
      name: record.name, enabled: !!record.enabled, platform: record.platform || 'feishu',
      webhook_token: '',
    } : { enabled: true, platform: 'feishu', webhook_token: '' })
  }
  const save = async (v: any) => {
    try {
      const payload = {
        name: v.name, enabled: !!v.enabled, platform: v.platform,
        webhook_token: v.webhook_token || '',
      }
      if (modal?.id) await put(`/alerts/channels/${modal.id}`, payload)
      else await post('/alerts/channels', payload)
      message.success(s.msg.saved); setModal(null); load()
    } catch (e: any) { message.error(e.message) }
  }
  const remove = (rec: any) =>
    del(`/alerts/channels/${rec.id}`)
      .then(() => { message.success(s.msg.deleted); load() })
      .catch((e: any) => message.error(e.message))
  const test = (rec: any) => {
    setTesting(rec.id)
    post(`/alerts/channels/${rec.id}/test`, {})
      .then((r: any) => {
        if (r.ok) message.success(s.alerts.testOk)
        else message.error(`${s.alerts.testFail}: ${r.error || ('HTTP ' + r.status + ' ' + (r.response || ''))}`)
      })
      .catch((e: any) => message.error(e.message))
      .finally(() => setTesting(null))
  }
  const cols: any[] = [
    { title: s.alerts.name, dataIndex: 'name' },
    { title: s.alerts.platform, dataIndex: 'platform', render: (p: string) => (p === 'lark' ? s.settings.alertPlatformLark : s.settings.alertPlatformFeishu) },
    { title: s.alerts.enabled, dataIndex: 'enabled', render: (v: boolean) => (v ? <Tag color="green">{s.misc.yes}</Tag> : <Tag>{s.misc.no}</Tag>) },
    { title: s.alerts.token, dataIndex: 'webhook_masked', render: (v: string) => v || '—' },
    { title: s.audit.time, dataIndex: 'updated_at' },
    {
      title: s.audit.detail, render: (_: any, rec: any) => (
        <Space>
          <Button size="small" disabled={!isSuper} onClick={() => open(rec)}>{s.action.edit}</Button>
          <Button size="small" loading={testing === rec.id} disabled={!isSuper} onClick={() => test(rec)}>{s.alerts.test}</Button>
          <Popconfirm title={s.alerts.confirmDelete} onConfirm={() => remove(rec)}>
            <Button size="small" danger disabled={!isSuper}>{s.action.delete}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]
  return (
    <Card>
      <Button type="primary" disabled={!isSuper} onClick={() => open()} style={{ marginBottom: 16 }}>{s.alerts.new}</Button>
      <Table rowKey="id" dataSource={items} columns={cols} size="small" locale={{ emptyText: s.alerts.empty }} />
      <Modal title={modal?.id ? s.alerts.edit : s.alerts.new} open={!!modal} onCancel={() => setModal(null)} onOk={() => form.submit()} width={560}>
        <Form form={form} layout="vertical" onFinish={save}>
          <Form.Item name="name" label={s.alerts.name} rules={[{ required: true }]}><Input /></Form.Item>
          <Row gutter={12}>
            <Col span={8}>
              <Form.Item name="enabled" label={s.alerts.enabled} valuePropName="checked"><Switch /></Form.Item>
            </Col>
            <Col span={16}>
              <Form.Item name="platform" label={s.alerts.platform}>
                <Select options={[{ value: 'feishu', label: s.settings.alertPlatformFeishu }, { value: 'lark', label: s.settings.alertPlatformLark }]} />
              </Form.Item>
            </Col>
          </Row>
          <Form.Item name="webhook_token" label={s.alerts.token}
            extra={modal?.id ? s.alerts.tokenEditExtra : s.alerts.tokenExtra}
            rules={modal?.id ? [] : [{ required: true, message: s.alerts.tokenRequired }]}>
            <Input.Password placeholder={modal?.id ? s.alerts.tokenKeepHint : 'hook token'} autoComplete="new-password" />
          </Form.Item>
        </Form>
      </Modal>
    </Card>
  )
}
