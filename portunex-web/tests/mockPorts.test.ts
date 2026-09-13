import { describe, expect, it } from 'vitest';
import { createMockPorts } from '../src/mock/createMockPorts';
import { demoAccounts } from '../src/modules/identity/demoAccounts';

describe('mock ports enforce their own boundaries', () => {
  it('denies direct unauthenticated and ordinary-user list access', async () => {
    const { ports } = createMockPorts({ latency: 0 });
    const query = { query: '', page: 1, pageSize: 8 };
    await expect(ports.users.list(query)).rejects.toMatchObject({ code: 'unauthorized' });
    await ports.identity.login(demoAccounts.user);
    await expect(ports.providers.list(query)).rejects.toMatchObject({ code: 'unauthorized' });
    await expect(ports.apiKeys.list(query)).rejects.toMatchObject({ code: 'unauthorized' });
  });

  it('supports filtering, pagination, logout and expired sessions', async () => {
    let now = Date.now();
    const { ports } = createMockPorts({ latency: 0, now: () => now, sessionTTL: 100 });
    await ports.identity.login(demoAccounts.admin);
    expect((await ports.users.list({ query: '产品', page: 1, pageSize: 1 })).total).toBe(1);
    expect((await ports.users.list({ query: '', page: 2, pageSize: 2 })).items).toHaveLength(2);
    now += 101;
    expect(await ports.identity.getSession()).toBeNull();
    await expect(ports.users.list({ query: '', page: 1, pageSize: 8 })).rejects.toMatchObject({ code: 'unauthorized' });
    await ports.identity.login(demoAccounts.admin);
    await ports.identity.logout();
    expect(await ports.identity.getSession()).toBeNull();
  });

  it('aborts a pending list without delivering stale rows', async () => {
    const { ports, controls } = createMockPorts({ latency: 0 });
    await ports.identity.login(demoAccounts.admin);
    controls.setScenario('loading');
    const controller = new AbortController();
    const request = ports.users.list({ query: '', page: 1, pageSize: 8 }, controller.signal);
    controller.abort();
    await expect(request).rejects.toMatchObject({ name: 'AbortError' });
  });
});
