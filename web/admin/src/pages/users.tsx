import { useEffect, useState } from 'react'
import { Button, Form, Input, Modal, Popconfirm, Select, Space, Table, Tag, message } from 'antd'
import { get, post, put } from '../api'
import { useLang } from '../shared'

// --- Users ---

export function UsersPage() {
  const { s } = useLang()
  const [items, setItems] = useState<any[]>([])
  const [create, setCreate] = useState(false)
  const [form] = Form.useForm()
  const load = () => get('/users').then((r) => setItems(r.items || []))
  useEffect(() => { load() }, [])
  const cols = [
    { title: s.users.username, dataIndex: 'username' },
    { title: s.users.role, dataIndex: 'role', render: (r: string) => <Tag color={r === 'superadmin' ? 'blue' : 'default'}>{r}</Tag> },
    { title: s.users.status, dataIndex: 'status', render: (st: string) => <Tag color={st === 'active' ? 'green' : 'red'}>{st}</Tag> },
    { title: s.users.lastLogin, dataIndex: 'last_login_at', render: (v: string) => v || '—' },
    {
      title: s.action.edit, render: (_: any, rec: any) => (
        <Space>
          <Button size="small" onClick={async () => {
            const next = rec.status === 'active' ? 'disabled' : 'active'
            try { await put(`/users/${rec.id}`, { status: next }); message.success(s.msg.saved); load() }
            catch (e: any) { message.error(e.message) }
          }}>{rec.status === 'active' ? s.action.disable : s.action.enable}</Button>
          <Popconfirm title={s.users.confirmReset} onConfirm={async () => {
            const pw = crypto.randomUUID().replace(/-/g, '')
            try {
              await put(`/users/${rec.id}`, { new_password: pw })
              Modal.info({ title: s.users.newPwdShown, content: pw })
            } catch (e: any) { message.error(e.message) }
          }}><Button size="small">{s.action.resetPassword}</Button></Popconfirm>
        </Space>
      ),
    },
  ]
  return (
    <>
      <Button type="primary" onClick={() => setCreate(true)} style={{ marginBottom: 16 }}>{s.users.new}</Button>
      <Table rowKey="id" dataSource={items} columns={cols} size="small" />
      <Modal title={s.users.new} open={create} onCancel={() => setCreate(false)} onOk={() => form.submit()}>
        <Form form={form} layout="vertical" onFinish={async (v) => {
          try {
            await post('/users', v)
            message.success(s.msg.userCreated)
            setCreate(false); form.resetFields(); load()
          } catch (e: any) { message.error(e.message) }
        }}>
          <Form.Item name="username" label={s.users.username} rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="password" label={s.users.initialPwd} rules={[{ required: true, min: 10 }]}><Input.Password /></Form.Item>
          <Form.Item name="display_name" label={s.users.displayName}><Input /></Form.Item>
          <Form.Item name="role" label={s.users.role} initialValue="user"><Select options={[{ value: 'user' }, { value: 'superadmin' }]} /></Form.Item>
        </Form>
      </Modal>
    </>
  )
}
