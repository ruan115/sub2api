import { useEffect, useState } from 'react';
import type { ApplicationPorts } from './ports';
import { NavigationProvider, AppLink, useNavigation } from '../shared/navigation';
import { SessionProvider, useSession } from './SessionProvider';
import { LoginPage } from '../modules/identity/LoginPage';
import { UsersPage } from '../modules/users/UsersPage';
import { ApiKeysPage } from '../modules/apikeys/ApiKeysPage';
import { ProvidersPage } from '../modules/providers/ProvidersPage';
import { DashboardPage } from './DashboardPage';
import type { MockControls, MockScenario } from '../mock/createMockPorts';

const adminLinks = [{ href: '/dashboard/users', label: '用户管理', icon: '♙' }, { href: '/dashboard/api-keys', label: 'API Key 管理', icon: '⌘' }, { href: '/dashboard/providers', label: 'Provider 管理', icon: '▦' }];
export function App({ ports, mode = 'mock', controls }: { ports: ApplicationPorts; mode?: 'mock' | 'go'; controls?: MockControls }) {
  return <NavigationProvider><SessionProvider port={ports.identity}><Routes ports={ports} mode={mode} controls={controls} /></SessionProvider></NavigationProvider>;
}

function Routes({ ports, mode, controls }: { ports: ApplicationPorts; mode: 'mock' | 'go'; controls?: MockControls }) {
  const { path, navigate } = useNavigation();
  const { session, loading, authPending, login, logout, notice } = useSession();
  const [revision, setRevision] = useState(0);
  const [scenario, setScenario] = useState<MockScenario>('ready');
  useEffect(() => { if (loading) return; if (!session && path !== '/auth') navigate('/auth', true); else if (session && (path === '/auth' || path === '/')) navigate('/dashboard', true); }, [session, loading, path]);
  if (loading) return <main className="boot-state" role="status"><span className="spinner" /><p>正在读取本地演示会话…</p></main>;
  if (!session) return <LoginPage login={login} notice={notice} mode={mode} blocked={authPending} />;
  const admin = session.user.role === 'admin';
  const restricted = adminLinks.some(item => item.href === path);
  const heading = path === '/dashboard' ? '工作台' : adminLinks.find(item => item.href === path)?.label ?? '页面';
  function changeScenario(value: MockScenario) { setScenario(value); controls?.setScenario(value); setRevision(count => count + 1); }
  return <div className="console-layout"><aside className="sidebar"><AppLink href="/dashboard" className="wordmark"><span className="brand-mark" aria-hidden="true">p</span>portunex</AppLink><div className="workspace-select"><span className="workspace-avatar">本</span><div><strong>本地工作区</strong><small>恢复演示</small></div><span aria-hidden="true">⌄</span></div><nav aria-label="工作区导航"><span className="nav-caption">工作区</span><AppLink href="/dashboard" current={path === '/dashboard'}><span aria-hidden="true">▤</span>工作台</AppLink>{admin && <><span className="nav-caption">管理</span>{adminLinks.map(item => <AppLink key={item.href} href={item.href} current={path === item.href}><span aria-hidden="true">{item.icon}</span>{item.label}</AppLink>)}</>}</nav><div className="sidebar-bottom"><span className="green-dot" /><span>仅连接本地演示</span><small>RECOVERY / 0.1</small></div></aside>
    <div className="console-main"><header className="topbar"><div className="breadcrumb"><span>本地工作区</span><span aria-hidden="true">/</span><strong>{heading}</strong></div><div className="account-menu"><span className="avatar">{session.user.displayName.slice(0, 1)}</span><span>{session.user.displayName}<small>{admin ? '管理员' : '普通用户'}</small></span><button type="button" className="text-button" onClick={() => void logout()}>退出</button></div></header><div className="demo-banner" role="note"><span className="demo-dot" /><strong>本地演示</strong><span>合成数据 · 未连接线上 · {mode === 'go' ? 'Go 内存服务' : '页面内 Mock'}</span>{controls && <label>数据状态<select aria-label="演示数据状态" value={scenario} onChange={event => changeScenario(event.target.value as MockScenario)}><option value="ready">正常</option><option value="loading">持续加载</option><option value="empty">空列表</option><option value="error">模拟错误</option><option value="unauthorized">模拟未授权</option></select></label>}</div>
      <main className="page-content" id="main-content">{restricted && !admin ? <section className="panel access-denied" role="alert"><span className="state-symbol">⊘</span><h1>暂无访问权限</h1><p>普通用户不能访问管理列表。</p><AppLink href="/dashboard" className="button button--secondary">返回工作台</AppLink></section> : path === '/dashboard/users' ? <UsersPage port={ports.users} revision={revision} /> : path === '/dashboard/api-keys' ? <ApiKeysPage port={ports.apiKeys} revision={revision} /> : path === '/dashboard/providers' ? <ProvidersPage port={ports.providers} revision={revision} /> : path === '/dashboard' || path === '/auth' || path === '/' ? <DashboardPage mode={mode} /> : <section className="panel access-denied"><h1>页面不存在</h1><p>当前演示仅包含已列出的工作区页面。</p><AppLink href="/dashboard" className="button button--secondary">返回工作台</AppLink></section>}</main><footer className="console-footer"><span>Portunex · 本地恢复演示</span><span>合成数据不用于真实业务决策</span></footer></div></div>;
}
