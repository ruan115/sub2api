import { AppLink } from '../shared/navigation';
import { useSession } from './SessionProvider';

export function DashboardPage({ mode }: { mode: 'mock' | 'go' }) {
  const { session } = useSession();
  const admin = session?.user.role === 'admin';
  return <section><div className="page-heading"><div><p className="eyebrow">概览 / 工作台</p><h1>你好，{session?.user.displayName}</h1><p>这里是你的本地演示工作区。</p></div><span className="read-only-badge">只读演示</span></div>
    <div className="welcome-panel"><div><span className="eyebrow">WORKSPACE OVERVIEW</span><h2>从一个清晰的工作台开始</h2><p>{admin ? '查看用户、Key 与 Provider 列表，验证基础交互。' : '当前是普通用户演示会话，管理列表由角色权限保护。'}</p></div><div className="workspace-emblem" aria-hidden="true"><span>p</span></div></div>
    <div className="summary-grid"><div><span>当前角色</span><strong>{admin ? '管理员' : '普通用户'}</strong><small>由演示会话决定</small></div><div><span>数据来源</span><strong>{mode === 'go' ? '本地 Go 内存' : '页面内 Mock'}</strong><small>全部为合成数据</small></div><div><span>线上连接</span><strong>未连接 <span className="green-dot" /></strong><small>没有生产 API 或数据库</small></div></div>
    <div className="section-title"><h2>{admin ? '管理工作区' : '账户访问'}</h2><span>当前可用入口</span></div>
    {admin ? <div className="module-grid">{[{ path: '/dashboard/users', icon: '01', title: '用户管理', text: '查看演示成员、角色与账户状态。' }, { path: '/dashboard/api-keys', icon: '02', title: 'API Key 管理', text: '查看不可用的合成 Key 标识与归属。' }, { path: '/dashboard/providers', icon: '03', title: 'Provider 管理', text: '查看演示模型通道与状态标签。' }].map(item => <AppLink href={item.path} className="module-card" key={item.path}><span className="module-number">{item.icon}</span><h3>{item.title}<span aria-hidden="true">↗</span></h3><p>{item.text}</p></AppLink>)}</div> : <div className="panel member-panel"><span className="state-symbol">✓</span><div><h3>普通用户会话正常</h3><p>管理入口不会显示，直接访问管理页面也会被拦截。可退出后选择管理员演示账号继续检查。</p></div></div>}
    <div className="scope-note"><strong>演示范围</strong><p>本切片仅提供登录、会话与只读列表。旧调用方的 HTTP 契约仍待验证；页面可用不代表线上业务已恢复。</p></div>
  </section>;
}
