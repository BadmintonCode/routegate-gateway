import { useEffect, useState } from 'react'
import { Alert, Button, Card, Col, Descriptions, Form, Input, Row, Select, Space, Statistic, Tag, message } from 'antd'
import { get, post } from '../api'
import { useLang } from '../shared'

// --- Dashboard ---

export function Dashboard() {
  const { s } = useLang()
  const [data, setData] = useState<any>(null)
  const load = () => Promise.all([get('/gateway/instances'), get('/gateway/config'), get('/policies')])
    .then(([inst, cfg, pol]) => setData({ inst, cfg, polCount: pol.items?.length ?? 0 }))
  useEffect(() => { load(); const t = setInterval(load, 5000); return () => clearInterval(t) }, [])
  if (!data) return null
  const rev = data.inst.config_revision
  const instances = data.inst.items || []
  const propagated = instances.filter((i: any) => i.loaded_revision === rev && !i.stale).length
  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Row gutter={16}>
        <Col span={5}><Card><Statistic title={s.dash.configRevision} value={rev} /></Card></Col>
        <Col span={5}><Card><Statistic title={s.dash.propagated} value={`${propagated}/${instances.length}`} /></Card></Col>
        <Col span={5}><Card><Statistic title={s.dash.routes} value={data.cfg.routes?.length ?? 0} /></Card></Col>
        <Col span={5}><Card><Statistic title={s.dash.identities} value={data.cfg.identities?.length ?? 0} /></Card></Col>
        <Col span={4}><Card><Statistic title={s.dash.policies} value={data.polCount} /></Card></Col>
      </Row>
      {instances.some((i: any) => i.stale) && <Alert type="warning" message={s.dash.staleWarn} />}
      <Card title={s.dash.testerTitle}>
        <Tester />
      </Card>
    </Space>
  )
}

function Tester() {
  const { s } = useLang()
  const [form] = Form.useForm()
  const [result, setResult] = useState<any>(null)
  const decisionTag = (d: string) => (
    <Tag color={d === 'allow' ? 'green' : d === 'deny' ? 'red' : 'orange'}>
      {(s.dash as any)[d] ?? d}
    </Tag>
  )
  return (
    <Space direction="vertical" style={{ width: '100%' }}>
      <Form form={form} layout="inline" onFinish={async (v) => {
        try { setResult(await post('/policies/test', v)) } catch (e: any) { message.error(e.message) }
      }}>
        <Form.Item name="caller" rules={[{ required: true }]}><Input placeholder={s.dash.testerCaller} style={{ width: 200 }} /></Form.Item>
        <Form.Item name="method" initialValue="GET">
          <Select style={{ width: 96 }} options={['GET', 'POST', 'PUT', 'DELETE', 'PATCH'].map((m) => ({ value: m }))} />
        </Form.Item>
        <Form.Item name="path" rules={[{ required: true }]}><Input placeholder={s.dash.testerPath} style={{ width: 260 }} /></Form.Item>
        <Button type="primary" htmlType="submit">{s.action.test}</Button>
      </Form>
      {result && (
        <Descriptions size="small" bordered column={2}>
          <Descriptions.Item label={s.dash.decision}>{decisionTag(result.decision)}</Descriptions.Item>
          <Descriptions.Item label={s.dash.route}>{result.route || '—'}</Descriptions.Item>
          <Descriptions.Item label={s.dash.subPath}>{result.sub_path || '—'}</Descriptions.Item>
          <Descriptions.Item label={s.dash.matchedPolicy}>{result.matched_policy_id || s.dash.none}</Descriptions.Item>
        </Descriptions>
      )}
    </Space>
  )
}
