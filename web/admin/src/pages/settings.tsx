import { useEffect, useState } from 'react'
import { Alert, Button, Card, Col, Form, Input, InputNumber, Row, Switch, Tabs, Typography, message } from 'antd'
import { get, put } from '../api'
import { useLang } from '../shared'
import { AlertChannelsPage } from './alertChannels'
import { AlertRulesPage } from './alertRules'

const { Text } = Typography

// --- Settings ---

// SettingsPage maintains the panel-managed debug-logging configuration
// (settings.debug_log). Writes are superadmin-only with optimistic
// concurrency; instances apply the values on their next sync tick.
export function SettingsPage({ isSuper }: { isSuper: boolean }) {
  const { s } = useLang()
  const [form] = Form.useForm()
  const [bizForm] = Form.useForm()
  const [rev, setRev] = useState(0)
  const [running, setRunning] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [bizRev, setBizRev] = useState(0)
  const [bizLoaded, setBizLoaded] = useState(false)
  const load = () =>
    get('/settings/debug').then((d: any) => {
      setRev(d.config_revision || 0)
      setRunning(!!d.running)
      form.setFieldsValue(d)
      setLoaded(true)
    })
  const loadBiz = () =>
    get('/settings/biz').then((d: any) => {
      setBizRev(d.config_revision || 0)
      bizForm.setFieldsValue({ biz_code_path: d.code_path || '', biz_message_path: d.message_path || '' })
      setBizLoaded(true)
    })
  useEffect(() => {
    load().catch((e: any) => message.error(e.message))
    loadBiz().catch((e: any) => message.error(e.message))
  }, [])
  const save = (v: any) => {
    put('/settings/debug', { ...v, expected_revision: rev })
      .then(() => message.success(s.msg.saved))
      .then(load)
      .catch((e: any) => message.error(e.message))
  }
  const saveBiz = (v: any) => {
    put('/settings/biz', {
      code_path: (v.biz_code_path || '').trim(),
      message_path: (v.biz_message_path || '').trim(),
      expected_revision: bizRev,
    })
      .then(() => message.success(s.msg.saved))
      .then(loadBiz)
      .catch((e: any) => message.error(e.message))
  }
  const enabled = Form.useWatch('enabled', form)
  return (
    <div style={{ maxWidth: 1100 }}>
      <Tabs defaultActiveKey="debug" items={[
        {
          key: 'debug',
          label: s.settings.debugTab,
          children: <Card loading={!loaded} title={s.settings.title}>
            <Alert type="warning" showIcon message={s.settings.sensitiveWarn} style={{ marginBottom: 16 }} />
            <Form form={form} layout="vertical" onFinish={save} disabled={!isSuper}>
              <Form.Item name="enabled" valuePropName="checked" style={{ marginBottom: 12 }}>
                <Switch checkedChildren={s.misc.yes} unCheckedChildren={s.misc.no} />
              </Form.Item>
              <Text type="secondary">{s.settings.enabled} · {running ? s.settings.running : s.settings.notRunning}</Text>
              <Row gutter={16} style={{ marginTop: 12 }}>
                <Col span={16}>
                  <Form.Item name="file" label={s.settings.file}
                    rules={enabled ? [{ required: true, message: s.settings.fileRequired }] : []}>
                    <Input placeholder="/var/log/routegate/debug.log" />
                  </Form.Item>
                </Col>
                <Col span={4}>
                  <Form.Item name="max_mb" label={s.settings.maxMB}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
                </Col>
                <Col span={4}>
                  <Form.Item name="retain_days" label={s.settings.retainDays}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
                </Col>
              </Row>
              <Row gutter={16}>
                <Col span={6}>
                  <Form.Item name="request_body_max_bytes" label={s.settings.reqBody}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
                </Col>
                <Col span={6}>
                  <Form.Item name="response_body_max_bytes" label={s.settings.respBody}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
                </Col>
                <Col span={6}>
                  <Form.Item name="headers_max_bytes" label={s.settings.headers}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
                </Col>
                <Col span={6}>
                  <Form.Item name="queue_capacity" label={s.settings.queue}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
                </Col>
              </Row>
              <Text type="secondary">{s.settings.appliesHint}</Text>
              {isSuper && <div style={{ marginTop: 16 }}><Button type="primary" htmlType="submit">{s.action.save}</Button></div>}
            </Form>
          </Card>,
        },
        {
          key: 'biz',
          label: s.settings.bizTab,
          children: <Card loading={!bizLoaded} title={s.settings.bizTitle}>
            <Form form={bizForm} layout="vertical" onFinish={saveBiz} disabled={!isSuper}>
              <Row gutter={12}>
                <Col span={12}>
                  <Form.Item name="biz_code_path" label={s.routes.bizCodePath}><Input placeholder="code" /></Form.Item>
                </Col>
                <Col span={12}>
                  <Form.Item name="biz_message_path" label={s.routes.bizMessagePath}><Input placeholder="message" /></Form.Item>
                </Col>
              </Row>
              <Text type="secondary">{s.settings.bizHint}</Text>
              {isSuper && <div style={{ marginTop: 16 }}><Button type="primary" htmlType="submit">{s.action.save}</Button></div>}
            </Form>
          </Card>,
        },
        { key: 'channels', label: s.settings.alertChannelsTab, children: <AlertChannelsPage isSuper={isSuper} /> },
        { key: 'rules', label: s.settings.alertRulesTab, children: <AlertRulesPage isSuper={isSuper} /> },
      ]} />
    </div>
  )
}
