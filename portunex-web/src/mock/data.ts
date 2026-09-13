import type { UserRow } from '../modules/users/model';
import type { ApiKeyRow } from '../modules/apikeys/model';
import type { ProviderRow } from '../modules/providers/model';

export const users: UserRow[] = [
  { id: 'demo-user-01', name: '演示管理员', email: 'admin@example.invalid', role: 'admin', status: 'active', createdAt: '2026-09-01T08:00:00Z' },
  { id: 'demo-user-02', name: '产品团队', email: 'product@example.invalid', role: 'user', status: 'active', createdAt: '2026-09-02T08:00:00Z' },
  { id: 'demo-user-03', name: '研发工作区', email: 'engineering@example.invalid', role: 'user', status: 'active', createdAt: '2026-09-03T08:00:00Z' },
  { id: 'demo-user-04', name: '评估环境', email: 'evaluation@example.invalid', role: 'user', status: 'disabled', createdAt: '2026-09-04T08:00:00Z' },
];
export const apiKeys: ApiKeyRow[] = [
  { id: 'demo-key-01', name: '产品原型', keyHint: 'demo-key-••••-01', owner: '产品团队', status: 'active', lastUsedAt: '2026-09-12T09:20:00Z' },
  { id: 'demo-key-02', name: '研发集成', keyHint: 'demo-key-••••-02', owner: '研发工作区', status: 'active', lastUsedAt: '2026-09-12T08:45:00Z' },
  { id: 'demo-key-03', name: '归档测试', keyHint: 'demo-key-••••-03', owner: '评估环境', status: 'disabled', lastUsedAt: null },
];
export const providers: ProviderRow[] = [
  { id: 'demo-provider-01', name: 'Claude · 演示通道', type: 'Claude', status: 'ready', modelCount: 3, updatedAt: '2026-09-12T09:00:00Z' },
  { id: 'demo-provider-02', name: 'OpenAI · 演示通道', type: 'OpenAI', status: 'ready', modelCount: 4, updatedAt: '2026-09-12T09:00:00Z' },
  { id: 'demo-provider-03', name: 'Gemini · 演示通道', type: 'Gemini', status: 'cooling', modelCount: 2, updatedAt: '2026-09-12T09:00:00Z' },
];
