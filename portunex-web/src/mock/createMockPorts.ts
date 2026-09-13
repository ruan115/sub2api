import type { ApplicationPorts } from '../app/ports';
import type { Session } from '../modules/identity/model';
import type { ListPort } from '../shared/ports';
import { PortError } from '../shared/ports';
import { users, apiKeys, providers } from './data';
import { demoAccounts } from '../modules/identity/demoAccounts';

export type MockScenario = 'ready' | 'empty' | 'error' | 'unauthorized' | 'loading';
export interface MockControls {
  setScenario(scenario: MockScenario): void;
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(new DOMException('Aborted', 'AbortError')); return; }
    const abort = () => { clearTimeout(timer); reject(new DOMException('Aborted', 'AbortError')); };
    const timer = setTimeout(() => { signal?.removeEventListener('abort', abort); resolve(); }, ms);
    signal?.addEventListener('abort', abort, { once: true });
  });
}

export function createMockPorts(options: { latency?: number; session?: Session | null; sessionTTL?: number; now?: () => number } = {}): { ports: ApplicationPorts; controls: MockControls } {
  const now = options.now ?? Date.now;
  const latency = options.latency ?? 220;
  let session = options.session ?? null;
  let scenario: MockScenario = 'ready';
  const identity: ApplicationPorts['identity'] = {
    async getSession(signal) {
      await wait(latency, signal);
      if (session && Date.parse(session.expiresAt) <= now()) session = null;
      return session;
    },
    async login(input, signal) {
      await wait(latency, signal);
      const role = input.email === demoAccounts.admin.email ? 'admin' : input.email === demoAccounts.user.email ? 'user' : null;
      if (!role || input.password !== demoAccounts[role].password) throw new PortError('invalid_credentials', '请使用下方提供的演示账号。此页面不接受真实登录。');
      session = { user: { id: role === 'admin' ? 'demo-user-01' : 'demo-user-02', email: input.email, displayName: role === 'admin' ? '演示管理员' : '演示用户', role }, expiresAt: new Date(now() + (options.sessionTTL ?? 20 * 60 * 1000)).toISOString() };
      return session;
    },
    async logout() { session = null; },
  };
  function list<T extends object>(rows: T[]): ListPort<T> {
    return {
      async list(query, signal) {
        await wait(latency, signal);
        if (!session || session.user.role !== 'admin' || Date.parse(session.expiresAt) <= now()) throw new PortError('unauthorized', '当前演示会话没有管理权限。');
        if (scenario === 'loading') await new Promise<void>((_, reject) => {
          if (signal?.aborted) { reject(new DOMException('Aborted', 'AbortError')); return; }
          signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
        });
        if (scenario === 'unauthorized') throw new PortError('unauthorized', '模拟未授权响应。');
        if (scenario === 'error') throw new PortError('unavailable', '这是合成的请求失败，可切换状态后重试。');
        const term = query.query.trim().toLocaleLowerCase();
        const matching = scenario === 'empty' ? [] : rows.filter(row => Object.values(row).some(value => String(value).toLocaleLowerCase().includes(term)));
        return { items: matching.slice((query.page - 1) * query.pageSize, query.page * query.pageSize).map(row => ({ ...row })), total: matching.length, page: query.page, pageSize: query.pageSize };
      },
    };
  }
  return { ports: { identity, users: list(users), apiKeys: list(apiKeys), providers: list(providers) }, controls: { setScenario(value) { scenario = value; } } };
}
