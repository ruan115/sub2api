import { useState, type FormEvent } from 'react';
import type { LoginInput } from './model';
import { demoAccounts } from './demoAccounts';
import { PortError } from '../../shared/ports';

export function LoginPage({ login, notice, mode, blocked = false }: { login(input: LoginInput): Promise<void>; notice: string; mode: 'mock' | 'go'; blocked?: boolean }) {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || blocked) return;
    setBusy(true); setError('');
    try { await login({ email, password }); }
    catch (failure) { setError(failure instanceof PortError ? failure.message : '本地演示服务暂时不可用，请重试。'); }
    finally { setBusy(false); }
  }
  function fill(role: 'admin' | 'user') { setEmail(demoAccounts[role].email); setPassword(demoAccounts[role].password); setError(''); }
  return <div className="auth-layout">
    <aside className="auth-intro"><div className="wordmark"><span className="brand-mark" aria-hidden="true">p</span>portunex<span className="brand-caption">CONSOLE</span></div><div><p className="eyebrow">本地恢复工作区</p><h1>管理入口，<br />重新就绪。</h1><p className="auth-description">用合成数据检查页面、会话与管理权限。<br />你的线上账户和服务不会受到影响。</p><div className="auth-scope"><span>01<span>用户与访问权限</span></span><span>02<span>API Key 目录</span></span><span>03<span>Provider 状态</span></span></div></div><p className="auth-footnote">恢复演示 · 只读列表 · 无真实凭据</p></aside>
    <main className="auth-main"><div className="auth-card"><div className="demo-label"><span />{mode === 'go' ? '本地 Go 演示' : '本地 Mock 演示'}</div><h2>登录工作区</h2><p className="subtle">请使用下方演示账号，不要输入真实密码。</p>
      {notice && <p role="status" className="inline-notice">{notice}</p>}
      <form onSubmit={submit} aria-label="演示登录">
        <label>演示邮箱<input type="email" required autoComplete="off" placeholder="选择下方演示账号" value={email} disabled={busy || blocked} onChange={event => setEmail(event.target.value)} /></label>
        <label>演示密码<input type="password" required autoComplete="off" value={password} disabled={busy || blocked} onChange={event => setPassword(event.target.value)} /></label>
        {error && <p role="alert" className="form-error">{error}</p>}
        <button className="button button--primary button--full" type="submit" disabled={busy || blocked}>{busy ? '正在登录…' : blocked ? '正在处理会话…' : '进入演示工作区'}<span aria-hidden="true">→</span></button>
      </form>
      <div className="demo-accounts"><span>填入演示账号</span><div><button type="button" onClick={() => fill('admin')} disabled={busy || blocked}>管理员</button><button type="button" onClick={() => fill('user')} disabled={busy || blocked}>普通用户</button></div><small>管理员可浏览三类管理列表；普通用户仅可查看工作台。</small></div>
      <p className="privacy-note">{mode === 'go' ? '仅连接此电脑上的 Go 内存演示服务，不访问生产数据库。' : '数据与会话只在当前页面内存中，刷新页面后需要重新登录。'}</p>
    </div></main>
  </div>;
}
