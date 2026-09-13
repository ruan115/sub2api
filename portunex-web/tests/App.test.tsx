import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../src/app/App';
import type { Session } from '../src/modules/identity/model';
import { createMockPorts, type MockScenario } from '../src/mock/createMockPorts';

let root: Root;
let container: HTMLDivElement;
const session = (role: 'admin' | 'user' = 'admin', expiresAt = Date.now() + 600_000): Session => ({ user: { id: 'synthetic', email: `${role}@example.invalid`, displayName: role === 'admin' ? '演示管理员' : '演示用户', role }, expiresAt: new Date(expiresAt).toISOString() });
beforeEach(() => { container = document.createElement('div'); document.body.append(container); root = createRoot(container); window.history.replaceState({}, '', '/auth'); });
afterEach(async () => { await act(async () => root.unmount()); container.remove(); });
async function settle() { await act(async () => { await new Promise(resolve => setTimeout(resolve, 15)); }); }
async function mount(mock: ReturnType<typeof createMockPorts>, path = '/auth') { window.history.replaceState({}, '', path); await act(async () => { root.render(<App ports={mock.ports} controls={mock.controls} />); }); await settle(); }
function button(text: string) { const result = [...container.querySelectorAll('button')].find(item => item.textContent?.trim() === text); if (!result) throw new Error(`Missing button: ${text}`); return result; }
async function click(element: Element) { await act(async () => { element.dispatchEvent(new MouseEvent('click', { bubbles: true })); }); }
async function scenario(value: MockScenario) { const select = container.querySelector('select[aria-label="演示数据状态"]') as HTMLSelectElement; await act(async () => { select.value = value; select.dispatchEvent(new Event('change', { bubbles: true })); }); await settle(); }

describe('synthetic identity and permissions', () => {
  it('requires login, uses explicit synthetic credentials and logs out', async () => {
    const mock = createMockPorts({ latency: 0 });
    await mount(mock, '/dashboard/users');
    expect(window.location.pathname).toBe('/auth');
    expect(container.textContent).toContain('不要输入真实密码');
    await click(button('管理员'));
    await act(async () => { container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
    await settle();
    expect(window.location.pathname).toBe('/dashboard');
    expect(container.textContent).toContain('未连接线上');
    expect(container.querySelector('nav a[href="/dashboard/users"]')).not.toBeNull();
    await click(button('退出'));
    await settle();
    expect(window.location.pathname).toBe('/auth');
    expect(container.textContent).toContain('已退出当前演示会话');
  });

  it('shows safe login failure and permits retry', async () => {
    const mock = createMockPorts({ latency: 0 });
    await mount(mock);
    await act(async () => { container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
    await settle();
    expect(container.querySelector('[role="alert"]')?.textContent).toContain('演示账号');
    expect(button('进入演示工作区→').disabled).toBe(false);
  });

  it.each(['success', 'failure'] as const)('disables login until the pending logout settles: %s', async outcome => {
    const mock = createMockPorts({ latency: 0, session: session() });
    let finish!: () => void;
    let fail!: (error: Error) => void;
    const pending = new Promise<void>((resolve, reject) => { finish = resolve; fail = reject; });
    vi.spyOn(mock.ports.identity, 'logout').mockImplementation(() => pending);
    const login = vi.spyOn(mock.ports.identity, 'login');
    await mount(mock, '/dashboard');
    await click(button('退出'));
    expect(container.textContent).toContain('正在退出当前演示会话');
    expect(button('管理员').disabled).toBe(true);
    expect(button('正在处理会话…→').disabled).toBe(true);
    expect([...container.querySelectorAll('input')].every(input => input.disabled)).toBe(true);
    await act(async () => { container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
    expect(login).not.toHaveBeenCalled();
    await act(async () => { if (outcome === 'success') finish(); else fail(new Error('synthetic logout failure')); });
    expect(button('管理员').disabled).toBe(false);
    await click(button('管理员'));
    await act(async () => { container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
    await settle();
    expect(login).toHaveBeenCalledTimes(1);
    expect(window.location.pathname).toBe('/dashboard');
    expect(container.textContent).not.toContain('登出失败');
  });

  it.each(['/dashboard/users', '/dashboard/api-keys', '/dashboard/providers'])('blocks ordinary users before a list port call: %s', async path => {
    const mock = createMockPorts({ latency: 0, session: session('user') });
    const users = vi.spyOn(mock.ports.users, 'list');
    const keys = vi.spyOn(mock.ports.apiKeys, 'list');
    const providers = vi.spyOn(mock.ports.providers, 'list');
    await mount(mock, path);
    expect(container.textContent).toContain('普通用户不能访问管理列表');
    expect(container.querySelectorAll('nav a')).toHaveLength(1);
    expect(users).not.toHaveBeenCalled(); expect(keys).not.toHaveBeenCalled(); expect(providers).not.toHaveBeenCalled();
  });

  it('expires a session automatically and returns to login', async () => {
    vi.useFakeTimers();
    const mock = createMockPorts({ latency: 0, session: session('admin', Date.now() + 50) });
    window.history.replaceState({}, '', '/dashboard');
    await act(async () => { root.render(<App ports={mock.ports} />); });
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(container.textContent).toContain('你好，演示管理员');
    await act(async () => { await vi.advanceTimersByTimeAsync(60); });
    expect(window.location.pathname).toBe('/auth');
    expect(container.textContent).toContain('演示会话已过期');
  });
});

describe.each([
  ['/dashboard/users', '用户管理', '产品团队'],
  ['/dashboard/api-keys', 'API Key 管理', '产品原型'],
  ['/dashboard/providers', 'Provider 管理', 'Claude · 演示通道'],
])('%s list states', (path, title, content) => {
  it('renders loading, ready, empty, error and unauthorized states', async () => {
    const mock = createMockPorts({ latency: 0, session: session() });
    await mount(mock, path); await settle();
    expect(container.querySelector('h1')?.textContent).toBe(title);
    expect(container.textContent).toContain(content);
    await scenario('loading');
    expect(container.textContent).toContain(`正在加载${title}`);
    await scenario('empty');
    expect(container.textContent).toContain(`暂无${title}`);
    await scenario('error');
    expect(container.querySelector('[role="alert"]')?.textContent).toContain('暂时无法加载');
    await scenario('unauthorized');
    expect(container.querySelector('[role="alert"]')?.textContent).toContain('暂无访问权限');
    await scenario('ready');
    expect(container.textContent).toContain(content);
  });
});

it('preserves browser back navigation and unknown route fallback', async () => {
  const mock = createMockPorts({ latency: 0, session: session() });
  await mount(mock, '/dashboard');
  await click(container.querySelector('nav a[href="/dashboard/users"]')!); await settle();
  expect(window.location.pathname).toBe('/dashboard/users');
  await act(async () => { window.history.replaceState({}, '', '/dashboard'); window.dispatchEvent(new PopStateEvent('popstate')); });
  expect(container.textContent).toContain('从一个清晰的工作台开始');
  await act(async () => { window.history.replaceState({}, '', '/missing'); window.dispatchEvent(new PopStateEvent('popstate')); });
  expect(container.textContent).toContain('页面不存在');
});
