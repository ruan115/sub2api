import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import type { IdentityPort, LoginInput, Session } from '../modules/identity/model';
import { PortError } from '../shared/ports';

interface SessionState { session: Session | null; loading: boolean; authPending: boolean; notice: string; login(input: LoginInput): Promise<void>; logout(): Promise<void> }
interface AuthOperation { kind: 'login' | 'logout'; promise: Promise<void> }
const SessionContext = createContext<SessionState | null>(null);

export function SessionProvider({ port, children }: { port: IdentityPort; children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState(true);
  const [authPending, setAuthPending] = useState(false);
  const [notice, setNotice] = useState('');
  const activeOperation = useRef<AuthOperation | null>(null);
  const authRevision = useRef(0);
  const currentSession = useRef<Session | null>(null);
  function replaceSession(value: Session | null) { currentSession.current = value; setSession(value); }
  useEffect(() => {
    const controller = new AbortController();
    const revision = authRevision.current;
    port.getSession(controller.signal).then(value => { if (!controller.signal.aborted && revision === authRevision.current) replaceSession(value); }).catch(() => { if (!controller.signal.aborted && revision === authRevision.current) setNotice('无法读取本地演示会话，请重新登录。'); }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [port]);
  useEffect(() => {
    if (!session) return;
    const expire = () => { if (currentSession.current === session) { replaceSession(null); setNotice('演示会话已过期，请重新登录。'); } };
    const remaining = Date.parse(session.expiresAt) - Date.now();
    if (!Number.isFinite(remaining) || remaining <= 0) { expire(); return; }
    const timer = setTimeout(expire, Math.min(remaining, 2_147_483_647));
    return () => clearTimeout(timer);
  }, [session]);
  function finish(operation: AuthOperation) {
    if (activeOperation.current !== operation) return;
    activeOperation.current = null; setAuthPending(false);
  }
  function login(input: LoginInput): Promise<void> {
    // A ref closes the same-tick gap before React can disable the form.
    if (activeOperation.current) return Promise.reject(new PortError('unavailable', '会话操作尚未完成，请稍后重试。'));
    const operation: AuthOperation = { kind: 'login', promise: Promise.resolve() };
    activeOperation.current = operation; authRevision.current++; setAuthPending(true);
    operation.promise = Promise.resolve().then(async () => {
      const value = await port.login(input);
      if (!Number.isFinite(Date.parse(value.expiresAt)) || Date.parse(value.expiresAt) <= Date.now()) throw new PortError('unauthorized', '演示服务返回了已过期会话。');
      if (activeOperation.current === operation) { replaceSession(value); setNotice(''); }
    }).finally(() => finish(operation));
    return operation.promise;
  }
  function logout(): Promise<void> {
    const previous = activeOperation.current;
    if (previous?.kind === 'logout') return previous.promise;
    const operation: AuthOperation = { kind: 'logout', promise: Promise.resolve() };
    activeOperation.current = operation; authRevision.current++; setAuthPending(true);
    replaceSession(null); setNotice('正在退出当前演示会话…');
    operation.promise = Promise.resolve().then(async () => {
      // Let an in-flight login finish setting its cookie before revoking it.
      if (previous) await previous.promise.catch(() => undefined);
      await port.logout();
      if (activeOperation.current === operation) setNotice('已退出当前演示会话。');
    }).catch(() => {
      if (activeOperation.current === operation) setNotice('页面会话已清理，但本地服务登出失败；请重试或关闭演示服务。');
    }).finally(() => finish(operation));
    return operation.promise;
  }
  return <SessionContext.Provider value={{ session, loading, authPending, notice, login, logout }}>{children}</SessionContext.Provider>;
}
export function useSession() { const value = useContext(SessionContext); if (!value) throw new Error('SessionProvider required'); return value; }
