import { useEffect, useState } from 'react'
import { Card, Space, Table, Tag } from 'antd'
import { get } from '../api'
import { useLang } from '../shared'

// --- Status ---

export function StatusPage() {
  const { s } = useLang()
  const [inst, setInst] = useState<any>(null)
  const [snap, setSnap] = useState<any>(null)
  const load = () => Promise.all([get('/gateway/instances'), get('/gateway/snapshot')]).then(([i, st]) => { setInst(i); setSnap(st) })
  useEffect(() => { load(); const t = setInterval(load, 5000); return () => clearInterval(t) }, [])
  if (!inst) return null
  const rev = inst.config_revision
  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card title={s.status.instances} size="small">
        <Table
          rowKey="instance_id" pagination={false} size="small"
          dataSource={inst.items || []}
          columns={[
            { title: s.status.instance, dataIndex: 'instance_id' },
            { title: s.status.version, dataIndex: 'version' },
            { title: s.status.loadedRev, dataIndex: 'loaded_revision', render: (v: number) =>
              <Tag color={v === rev ? 'green' : 'orange'}>{v}{v === rev ? ` (${s.status.current})` : ''}</Tag> },
            { title: s.status.lastSyncOk, dataIndex: 'last_sync_ok', render: (v: boolean) => (v ? s.status.yes : s.status.no) },
            { title: s.status.lastSync, dataIndex: 'last_sync_at' },
            { title: s.status.heartbeat, dataIndex: 'stale', render: (v: boolean) => (v ? <Tag color="red">{s.status.stale}</Tag> : s.status.healthy) },
          ]}
        />
      </Card>
      <Card title={s.status.snapshotTitle} size="small">
        <pre style={{ maxHeight: 400, overflow: 'auto', fontSize: 12 }}>{JSON.stringify(snap, null, 2)}</pre>
      </Card>
    </Space>
  )
}
