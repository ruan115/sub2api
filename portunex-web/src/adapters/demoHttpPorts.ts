import type { ApplicationPorts } from '../app/ports';
import { parseSession } from '../modules/identity/parse';
import { parseUser } from '../modules/users/parse';
import { parseApiKey } from '../modules/apikeys/parse';
import { parseProvider } from '../modules/providers/parse';
import { invalidPayload, parsePage } from '../shared/parse';
import type { ListPort } from '../shared/ports';
import { PortError } from '../shared/ports';

/** Only the explicit recovery demo contract; never a legacy Portunex adapter. */
export function createDemoHttpPorts(fetcher: typeof fetch = window.fetch.bind(window), hostname = window.location.hostname): ApplicationPorts {
  if (!['localhost', '127.0.0.1', '::1', '[::1]'].includes(hostname)) throw new Error('Go demo requires a loopback webpage origin');
  const prefix = '/__recovery__/v1';
  async function request(path: string, options: RequestInit = {}, login = false): Promise<unknown> {
    const response = await fetcher(prefix + path, { ...options, credentials: 'same-origin', headers: { 'Content-Type': 'application/json', ...options.headers } });
    if (!response.ok) {
      const code = response.status === 401 || response.status === 403 ? login ? 'invalid_credentials' : 'unauthorized' : 'unavailable';
      // Do not reflect unknown server error bodies into the interface.
      throw new PortError(code, code === 'invalid_credentials' ? '演示账号或密码不正确。' : code === 'unauthorized' ? '当前演示会话没有访问权限。' : '本地 Go 演示服务暂时不可用。');
    }
    if (response.status === 204) return undefined;
    try { return await response.json(); } catch { return invalidPayload(); }
  }
  function list<T>(path: string, parseRow: (value: unknown) => T): ListPort<T> {
    return { async list(query, signal) { const search = new URLSearchParams({ query: query.query, page: String(query.page), page_size: String(query.pageSize) }); return parsePage(await request(`${path}?${search}`, { signal }), parseRow); } };
  }
  return {
    identity: {
      async getSession(signal) { try { return parseSession(await request('/session', { signal })); } catch (error) { if (error instanceof PortError && error.code === 'unauthorized') return null; throw error; } },
      async login(input, signal) { return parseSession(await request('/login', { method: 'POST', body: JSON.stringify({ email: input.email, password: input.password }), signal }, true)); },
      async logout() { await request('/logout', { method: 'POST' }); },
    },
    users: list('/users', parseUser), apiKeys: list('/api-keys', parseApiKey), providers: list('/providers', parseProvider),
  };
}
