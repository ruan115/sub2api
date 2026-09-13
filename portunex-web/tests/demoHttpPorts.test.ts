import { describe, expect, it, vi } from 'vitest';
import { createDemoHttpPorts } from '../src/adapters/demoHttpPorts';
import { demoAccounts } from '../src/modules/identity/demoAccounts';
import { apiKeys, providers, users } from '../src/mock/data';

const response = (status: number, value?: unknown) => ({ ok: status >= 200 && status < 300, status, json: async () => value }) as Response;
const session = () => ({ user: { id: 'demo-user-01', displayName: '演示管理员', email: demoAccounts.admin.email, role: 'admin' }, expiresAt: new Date(Date.now() + 60_000).toISOString() });
const page = (items: unknown[]) => ({ items, total: items.length, page: 1, pageSize: 8 });

describe('explicit Go demo adapter', () => {
  it('only calls same-origin recovery paths with cookie credentials', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response(200, { items: [], total: 0, page: 2, pageSize: 4 }));
    const ports = createDemoHttpPorts(fetcher, '127.0.0.1');
    await ports.users.list({ query: '产品 & A', page: 2, pageSize: 4 });
    const [path, options] = fetcher.mock.calls[0];
    expect(path).toBe('/__recovery__/v1/users?query=%E4%BA%A7%E5%93%81+%26+A&page=2&page_size=4');
    expect(options?.credentials).toBe('same-origin');
    fetcher.mockResolvedValueOnce(response(200, session()));
    await ports.identity.login(demoAccounts.admin);
    expect(fetcher.mock.calls[1][0]).toBe('/__recovery__/v1/login');
    expect(fetcher.mock.calls[1][1]?.method).toBe('POST');
    expect(JSON.parse(fetcher.mock.calls[1][1]?.body as string)).toEqual({ email: demoAccounts.admin.email, password: demoAccounts.admin.password });
  });

  it('treats an absent session as anonymous and maps authorization failures safely', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response(401));
    const ports = createDemoHttpPorts(fetcher, 'localhost');
    expect(await ports.identity.getSession()).toBeNull();
    await expect(ports.users.list({ query: '', page: 1, pageSize: 8 })).rejects.toMatchObject({ code: 'unauthorized' });
    await expect(ports.identity.login(demoAccounts.admin)).rejects.toMatchObject({ code: 'invalid_credentials' });
  });

  it('handles empty logout and refuses public origins', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response(204));
    const ports = createDemoHttpPorts(fetcher, 'localhost');
    await expect(ports.identity.logout()).resolves.toBeUndefined();
    expect(fetcher.mock.calls[0][1]?.method).toBe('POST');
    expect(() => createDemoHttpPorts(fetcher, 'production.example')).toThrow('loopback');
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('validates each row and never returns unknown fields', async () => {
    const fetcher = vi.fn<typeof fetch>();
    const ports = createDemoHttpPorts(fetcher, 'localhost');
    const query = { query: '', page: 1, pageSize: 8 };
    for (const [port, rows] of [[ports.users, users], [ports.apiKeys, apiKeys], [ports.providers, providers]] as const) {
      fetcher.mockResolvedValueOnce(response(200, page(rows.map(row => ({ ...row, unexpected: 'not copied' })))));
      const result = await port.list(query);
      expect(result.items).toEqual(rows);
    }
  });

  it.each([
    ['provider type', 'providers', page([{ ...providers[0], type: 'openai-compatible' }])],
    ['provider status', 'providers', page([{ ...providers[0], status: 'active' }])],
    ['provider count', 'providers', page([{ ...providers[0], modelCount: -1 }])],
    ['user role', 'users', page([{ ...users[0], role: 'superadmin' }])],
    ['user status', 'users', page([{ ...users[0], status: 'banned' }])],
    ['key status', 'apiKeys', page([{ ...apiKeys[0], status: 'ready' }])],
    ['key date', 'apiKeys', page([{ ...apiKeys[0], lastUsedAt: 'yesterday' }])],
    ['missing items', 'users', { total: 0, page: 1, pageSize: 8 }],
    ['invalid total', 'users', { ...page([]), total: -1 }],
    ['fractional page', 'users', { ...page([]), page: 1.5 }],
    ['zero page size', 'users', { ...page([]), pageSize: 0 }],
    ['snake case page size', 'users', { items: [], total: 0, page: 1, page_size: 8 }],
    ['too many rows', 'users', { ...page(users), pageSize: 1 }],
  ] as const)('rejects malformed %s with a fixed safe error', async (_name, module, payload) => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response(200, payload));
    const ports = createDemoHttpPorts(fetcher, 'localhost');
    await expect(ports[module].list({ query: '', page: 1, pageSize: 8 })).rejects.toMatchObject({ code: 'unavailable', message: '本地演示服务返回的数据格式不符合演示接口。' });
  });

  it('rejects unknown session roles and malformed expiration dates', async () => {
    const fetcher = vi.fn<typeof fetch>();
    const ports = createDemoHttpPorts(fetcher, 'localhost');
    fetcher.mockResolvedValueOnce(response(200, { ...session(), user: { ...session().user, role: 'owner' } }));
    await expect(ports.identity.getSession()).rejects.toMatchObject({ code: 'unavailable' });
    fetcher.mockResolvedValueOnce(response(200, { ...session(), expiresAt: '2099-99-99T00:00:00Z' }));
    await expect(ports.identity.login(demoAccounts.admin)).rejects.toMatchObject({ code: 'unavailable' });
  });

  it('treats expired sessions as anonymous and rejects an expired login', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(response(200, { ...session(), expiresAt: '2000-01-01T00:00:00Z' }));
    const ports = createDemoHttpPorts(fetcher, 'localhost');
    await expect(ports.identity.getSession()).resolves.toBeNull();
    await expect(ports.identity.login(demoAccounts.admin)).rejects.toMatchObject({ code: 'unauthorized' });
  });

  it('does not surface JSON parser errors or their potentially sensitive snippets', async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue({ ...response(200), json: async () => { throw new SyntaxError('private response fragment'); } } as Response);
    await expect(createDemoHttpPorts(fetcher, 'localhost').identity.getSession()).rejects.toMatchObject({ code: 'unavailable', message: '本地演示服务返回的数据格式不符合演示接口。' });
  });
});
