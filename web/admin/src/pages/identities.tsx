import { useEffect, useState } from 'react'
import { Alert, Button, Form, Input, Modal, Popconfirm, Space, Table, Tag, Typography, message } from 'antd'
import { del, get, post } from '../api'
import { useLang } from '../shared'

const { Text } = Typography

// --- Identities ---

export function IdentitiesPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [create, setCreate] = useState(false)
  const [gen, setGen] = useState(false)
  const [genResult, setGenResult] = useState<any>(null)
  const [form] = Form.useForm()
  const [genForm] = Form.useForm()
  const load = () => get('/identities').then((r) => setItems(r.items || []))
  useEffect(() => { load() }, [])
  const download = (name: string, content: string, filename: string) => {
    const blob = new Blob([content], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob); a.download = filename; a.click()
  }
  const cols = [
    { title: s.ident.name, dataIndex: 'name' },
    { title: s.ident.keyId, dataIndex: 'key_id', render: (k: string) => <Text code>{k.slice(0, 16)}…</Text> },
    { title: s.ident.status, dataIndex: 'status', render: (st: string) => <Tag color={st === 'active' ? 'green' : 'red'}>{st}</Tag> },
    { title: s.ident.source, dataIndex: 'source' },
    { title: s.ident.policies, dataIndex: 'policy_count' },
    { title: s.ident.notAfter, dataIndex: 'not_after' },
    { title: s.ident.remark, dataIndex: 'remark' },
    {
      title: s.action.edit, render: (_: any, rec: any) => (
        <Space>
          {rec.status === 'active' && (
            <Popconfirm title={s.ident.confirmRevoke} onConfirm={async () => {
              try { await post(`/identities/${rec.id}/revoke`, { reason: 'revoked via panel' }); message.success(s.msg.deleted); load() }
              catch (e: any) { message.error(e.message) }
            }}><Button size="small" danger disabled={!isSuper}>{s.action.revoke}</Button></Popconfirm>
          )}
          <Popconfirm title={s.ident.confirmDelete} onConfirm={async () => {
            try { await del(`/identities/${rec.id}`); message.success(s.msg.deleted); load() }
            catch (e: any) { message.error(e.message) }
          }}><Button size="small" disabled={!isSuper}>{s.action.delete}</Button></Popconfirm>
        </Space>
      ),
    },
  ]
  return (
    <>
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" disabled={!isSuper} onClick={() => setCreate(true)}>{s.ident.upload}</Button>
        <Button disabled={!isSuper} onClick={() => { setGenResult(null); setGen(true) }}>{s.ident.generate}</Button>
      </Space>
      <Table rowKey="id" dataSource={items} columns={cols} size="small" />
      <Modal title={s.ident.uploadTitle} open={create} onCancel={() => setCreate(false)} onOk={() => form.submit()}>
        <Form form={form} layout="vertical" onFinish={async (v) => {
          try {
            await post('/identities', { name: v.name, pem: v.pem, remark: v.remark || '' })
            message.success(s.msg.saved); setCreate(false); form.resetFields(); load()
          } catch (e: any) { message.error(e.message) }
        }}>
          <Form.Item name="name" label={s.ident.identityName} rules={[{ required: true }]}><Input placeholder="service-a" /></Form.Item>
          <Form.Item name="pem" label={s.ident.certPem} rules={[{ required: true }]}><Input.TextArea rows={6} /></Form.Item>
          <Form.Item name="remark" label={s.ident.remark}><Input /></Form.Item>
        </Form>
      </Modal>
      <Modal title={s.ident.genTitle} open={gen} onCancel={() => { setGen(false); setGenResult(null) }} footer={null}>
        {genResult ? (
          <Space direction="vertical" style={{ width: '100%' }}>
            <Alert type="success" showIcon message={s.ident.genAlert} />
            <Space>
              <Button type="primary" onClick={() => download(genResult.name, genResult.private_key, `${genResult.name}.key`)}>{s.action.downloadKey}</Button>
              <Button onClick={() => download(genResult.name, genResult.certificate, `${genResult.name}.crt`)}>{s.action.downloadCert}</Button>
            </Space>
          </Space>
        ) : (
          <Form form={genForm} layout="vertical" onFinish={async (v) => {
            try {
              const r = await post('/identities/generate', { name: v.name, days: Number(v.days || 90), remark: v.remark || '' })
              setGenResult(r); genForm.resetFields(); load()
            } catch (e: any) { message.error(e.message) }
          }}>
            <Form.Item name="name" label={s.ident.identityName} rules={[{ required: true }]}><Input placeholder="service-b" /></Form.Item>
            <Form.Item name="days" label={s.ident.validity} initialValue={90}><Input /></Form.Item>
            <Form.Item name="remark" label={s.ident.remark}><Input /></Form.Item>
            <Button type="primary" htmlType="submit" block>{s.ident.generate}</Button>
          </Form>
        )}
      </Modal>
    </>
  )
}
