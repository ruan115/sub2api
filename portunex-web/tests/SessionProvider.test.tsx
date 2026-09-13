import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SessionProvider, useSession } from '../src/app/SessionProvider';
import type { IdentityPort, Session } from '../src/modules/identity/model';
import { demoAccounts } from '../src/modules/identity/demoAccounts';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const session = (id = 'current', expiresAt = Date.now() + 60_000): Session => ({ user: { id, email: demoAccounts.admin.email, displayName: '演示管理员', role: 'admin' }, expiresAt: new Date(expiresAt).toISOString() });
let root: Root;
let container: HTMLDivElement;
let current: ReturnType<typeof useSession>;
function Probe() { current = useSession(); return null; }
beforeEach(() => { container = document.createElement('div'); document.body.append(container); root = createRoot(container); });
afterEach(async () => { await act(async () => root.unmount()); container.remove(); vi.useRealTimers(); });
async function mount(port: IdentityPort) { await act(async () => root.render(<SessionProvider port={port}><Probe /></SessionProvider>)); }

describe('serialized synthetic identity operations', () => {
  it('rejects same-tick duplicate login before a second request can start', async () => {
    const pending = deferred<Session>();
    const port = { getSession: vi.fn(async () => null), login: vi.fn(() => pending.promise), logout: vi.fn(async () => {}) };
    await mount(port);
    let first!: Promise<void>;
    await act(async () => {
      first = current.login(demoAccounts.admin);
      await expect(current.login(demoAccounts.admin)).rejects.toMatchObject({ code: 'unavailable' });
    });
    expect(port.login).toHaveBeenCalledTimes(1);
    expect(current.authPending).toBe(true);
    await act(async () => { pending.resolve(session()); await first; });
    expect(current.session?.user.id).toBe('current');
    expect(current.authPending).toBe(false);
  });

  it.each(['success', 'failure'] as const)('blocks re-login until deferred logout settles: %s', async outcome => {
    const pending = deferred<void>();
    const port = { getSession: vi.fn(async () => session('old')), login: vi.fn(async () => session('new')), logout: vi.fn(() => pending.promise) };
    await mount(port);
    let logout!: Promise<void>;
    await act(async () => {
      logout = current.logout();
      expect(current.logout()).toBe(logout);
      await expect(current.login(demoAccounts.admin)).rejects.toMatchObject({ code: 'unavailable' });
    });
    expect(port.logout).toHaveBeenCalledTimes(1);
    expect(port.login).not.toHaveBeenCalled();
    expect(current.session).toBeNull();
    expect(current.authPending).toBe(true);
    await act(async () => { if (outcome === 'success') pending.resolve(); else pending.reject(new Error('synthetic failure')); await logout; });
    expect(current.authPending).toBe(false);
    expect(current.notice).toContain(outcome === 'success' ? '已退出' : '登出失败');
    await act(async () => current.login(demoAccounts.admin));
    expect(current.session?.user.id).toBe('new');
    expect(current.notice).toBe('');
  });

  it.each(['success', 'failure'] as const)('logout waits for an in-flight login and suppresses its state update: %s', async outcome => {
    const pendingLogin = deferred<Session>();
    const pendingLogout = deferred<void>();
    const port = { getSession: vi.fn(async () => null), login: vi.fn(() => pendingLogin.promise), logout: vi.fn(() => pendingLogout.promise) };
    await mount(port);
    let login!: Promise<void>;
    let logout!: Promise<void>;
    let loginResult!: Promise<void>;
    await act(async () => {
      login = current.login(demoAccounts.admin);
      loginResult = login.catch(() => undefined);
      logout = current.logout();
    });
    expect(port.logout).not.toHaveBeenCalled();
    await act(async () => {
      if (outcome === 'success') pendingLogin.resolve(session('obsolete')); else pendingLogin.reject(new Error('synthetic login failure'));
      await loginResult;
    });
    expect(port.logout).toHaveBeenCalledTimes(1);
    expect(current.session).toBeNull();
    expect(current.authPending).toBe(true);
    await act(async () => { pendingLogout.resolve(); await logout; });
    expect(current.session).toBeNull();
    expect(current.authPending).toBe(false);
  });

  it('does not let a late initial session read undo a completed logout', async () => {
    const initial = deferred<Session | null>();
    const port = { getSession: vi.fn(() => initial.promise), login: vi.fn(async () => session()), logout: vi.fn(async () => {}) };
    await mount(port);
    await act(async () => current.logout());
    await act(async () => initial.resolve(session('obsolete')));
    expect(current.loading).toBe(false);
    expect(current.session).toBeNull();
    expect(current.notice).toContain('已退出');
  });

  it('releases the operation after an expired login response so a retry can succeed', async () => {
    const port = { getSession: vi.fn(async () => null), login: vi.fn().mockResolvedValueOnce(session('expired', Date.now() - 1)).mockResolvedValueOnce(session('new')), logout: vi.fn(async () => {}) };
    await mount(port);
    await act(async () => { await expect(current.login(demoAccounts.admin)).rejects.toMatchObject({ code: 'unauthorized' }); });
    expect(current.session).toBeNull();
    expect(current.authPending).toBe(false);
    await act(async () => current.login(demoAccounts.admin));
    expect(current.session?.user.id).toBe('new');
  });
});
