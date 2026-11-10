import { useState } from 'react'
import { Alert, Button, Card, Form, Input, Space, Typography, message } from 'antd'
import { post } from '../api'
import { LangSwitch, useLang, type Me } from '../shared'

const { Link } = Typography

export function LoginPage({ onLogin }: { onLogin: (m: Me) => void }) {
  const { s } = useLang()
  const [form] = Form.useForm()
  const [busy, setBusy] = useState(false)
  return (
    <div style={{ display: 'grid', placeItems: 'center', minHeight: '100vh', background: '#f0f2f5' }}>
      <Card title={
        <Space size={8}>
          <img src="/admin/favicon.svg" alt="RouteGate" width={22} height={22} />
          <span>{s.login.title}</span>
        </Space>
      } style={{ width: 360 }} extra={<LangSwitch />}>
        <Form form={form} layout="vertical" onFinish={async (v) => {
          setBusy(true)
          try {
            const r = await post<any>('/auth/login', v)
            onLogin({ username: v.username, role: 'superadmin', must_change_password: r.must_change_password })
          } catch (e: any) {
            message.error(e.message)
          } finally { setBusy(false) }
        }}>
          <Form.Item name="username" label={s.login.username} rules={[{ required: true }]}><Input autoFocus /></Form.Item>
          <Form.Item name="password" label={s.login.password} rules={[{ required: true }]}><Input.Password /></Form.Item>
          <Button type="primary" htmlType="submit" block loading={busy}>{s.login.signIn}</Button>
        </Form>
        {/* Docs + console are public: plain anchor → full load → SPA boots the
            public developer page (App.tsx allows /admin/developer pre-login). */}
        <div style={{ marginTop: 16, textAlign: 'center' }}>
          <Link href="/admin/developer">{s.login.docsLink} →</Link>
        </div>
      </Card>
    </div>
  )
}

export function ForceChangePasswordPage({ me, onDone }: { me: Me; onDone: () => void }) {
  const { s } = useLang()
  const [form] = Form.useForm()
  return (
    <div style={{ display: 'grid', placeItems: 'center', minHeight: '100vh', background: '#f0f2f5' }}>
      <Card title={s.force.title} style={{ width: 420 }} extra={<LangSwitch />}>
        <Alert type="warning" showIcon message={s.force.alert} style={{ marginBottom: 16 }} />
        <Form form={form} layout="vertical" onFinish={async (v) => {
          try {
            // Forced first-login change: no current password needed (ADR §8.4).
            await post('/users/me/password', { next: v.next })
            message.success(s.msg.passwordChanged)
            onDone()
          } catch (e: any) { message.error(e.message) }
        }}>
          <Form.Item name="next" label={s.force.newPassword} rules={[{ required: true, min: 10 }]}>
            <Input.Password autoFocus />
          </Form.Item>
          <Form.Item name="confirm" label={s.force.confirmPassword} dependencies={['next']}
            rules={[
              { required: true },
              ({ getFieldValue }) => ({
                validator: (_, val) =>
                  !val || getFieldValue('next') === val
                    ? Promise.resolve()
                    : Promise.reject(new Error(s.force.mismatch)),
              }),
            ]}>
            <Input.Password />
          </Form.Item>
          <Button type="primary" htmlType="submit" block>{s.force.submit}</Button>
        </Form>
      </Card>
    </div>
  )
}
