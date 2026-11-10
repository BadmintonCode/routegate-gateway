import { useEffect, useState } from 'react'
import {
  ApiOutlined,
  AuditOutlined,
  BranchesOutlined,
  DashboardOutlined,
  IdcardOutlined,
  MonitorOutlined,
  RobotOutlined,
  SafetyCertificateOutlined,
  SettingOutlined,
  TeamOutlined,
} from '@ant-design/icons'
import { Button, Layout, Menu, Space, Typography } from 'antd'
import { post } from '../api'
import { LangSwitch, useLang, type Me } from '../shared'
import { Dashboard } from './dashboard'
import { RoutesPage } from './routes'
import { PoliciesPage } from './policies'
import { IdentitiesPage } from './identities'
import { UsersPage } from './users'
import { AuditPage } from './audit'
import { SettingsPage } from './settings'
import { StatusPage } from './status'
import { DeveloperPage } from './developer'
import { AgentPage } from './agent'

const { Sider, Content } = Layout
const { Text, Title } = Typography

// --- URL routing: /admin/<page>, pushState + popstate, no external router ---
const PAGES = ['dashboard', 'routes', 'policies', 'identities', 'users', 'audit', 'settings', 'developer', 'agent', 'status'] as const
type PageKey = (typeof PAGES)[number]

function pageFromLocation(): PageKey {
  const m = window.location.pathname.match(/^\/admin\/([a-z]+)/)
  const p = m?.[1]
  if (p === 'alerts' || p === 'alertrules') return 'settings'
  return p && (PAGES as readonly string[]).includes(p) ? p as PageKey : 'dashboard'
}
// ---------------------------------------------------------------------------

export function Shell({ me, onLogout }: { me: Me; onLogout: () => void }) {
  const { s } = useLang()
  const isSuper = me.role === 'superadmin'

  // Page state derives from the URL; navigations push history entries so
  // browser back/forward and refresh behave like real routes.
  const [page, setPageState] = useState<PageKey>(pageFromLocation)

  useEffect(() => {
    const onPop = () => setPageState(pageFromLocation())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  // Normalize the URL once on entry: bare /admin/ and unknown paths land on
  // the dashboard URL.
  useEffect(() => {
    const cur = pageFromLocation()
    if (window.location.pathname !== `/admin/${cur}`) {
      window.history.replaceState({}, '', `/admin/${cur}`)
      setPageState(cur)
    }
  }, [])

  const navigate = (p: PageKey) => {
    if (p !== page) {
      window.history.pushState({}, '', `/admin/${p}`)
      setPageState(p)
    }
  }

  // users is superadmin-only (ADR §8.1); direct URL entry redirects home.
  useEffect(() => {
    if ((page === 'users' || page === 'settings') && !isSuper) navigate('dashboard')
  }, [page, isSuper])

  const pages: Record<PageKey, JSX.Element> = {
    dashboard: <Dashboard />,
    routes: <RoutesPage isSuper={isSuper} />,
    policies: <PoliciesPage isSuper={isSuper} />,
    identities: <IdentitiesPage isSuper={isSuper} />,
    users: <UsersPage />,
    audit: <AuditPage isSuper={isSuper} />,
    settings: <SettingsPage isSuper={isSuper} />,
    status: <StatusPage />,
    // developer is self-service: visible to ALL authenticated users (own
    // tokens + API references only), so it is deliberately NOT in the
    // superadmin redirect guard above.
    developer: <DeveloperPage />,
    // agent is self-service like developer: chat is user-owned and the
    // model surface is a read-only tool whitelist; the Models tab renders
    // only for superadmins (AgentPage gating), so no redirect guard here.
    agent: <AgentPage isSuper={isSuper} />,
  }
  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider theme="dark" width={200}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '16px 20px' }}>
          <img src="/admin/favicon.svg" alt="RouteGate" width={30} height={30} />
          <span style={{ color: '#fff', fontWeight: 700, fontSize: 16, letterSpacing: 0.5 }}>RouteGate</span>
        </div>
        <Menu
          theme="dark" mode="inline" selectedKeys={[page]}
          onClick={(e) => navigate(e.key as PageKey)}
          items={[
            { key: 'dashboard', label: s.nav.dashboard, icon: <DashboardOutlined /> },
            { key: 'routes', label: s.nav.routes, icon: <BranchesOutlined /> },
            { key: 'policies', label: s.nav.policies, icon: <SafetyCertificateOutlined /> },
            { key: 'identities', label: s.nav.identities, icon: <IdcardOutlined /> },
            { key: 'users', label: s.nav.users, icon: <TeamOutlined />, disabled: !isSuper },
            { key: 'audit', label: s.nav.audit, icon: <AuditOutlined /> },
            { key: 'settings', label: s.nav.settings, icon: <SettingOutlined />, disabled: !isSuper },
            { key: 'developer', label: s.nav.developer, icon: <ApiOutlined /> },
            { key: 'agent', label: s.nav.agent, icon: <RobotOutlined /> },
            { key: 'status', label: s.nav.status, icon: <MonitorOutlined /> },
          ]}
        />
      </Sider>
      <Layout>
        <Content style={{ padding: 24, background: '#f5f5f5' }}>
          <div style={{ marginBottom: 16, display: 'flex', justifyContent: 'space-between' }}>
            <Title level={4} style={{ margin: 0 }}>{(s.nav as any)[page] ?? page}</Title>
            <Space>
              <LangSwitch />
              <Text type="secondary">{me.username} · {me.role}</Text>
              <Button size="small" onClick={() => post('/auth/logout').then(onLogout)}>{s.action.logout}</Button>
            </Space>
          </div>
          {pages[page]}
        </Content>
      </Layout>
    </Layout>
  )
}
