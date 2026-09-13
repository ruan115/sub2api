import type { ListPort } from '../../shared/ports';
import { ListPage } from '../../shared/ListPage';
import { Status, dateLabel } from '../../shared/Status';
import type { ApiKeyRow } from './model';

export function ApiKeysPage({ port, revision }: { port: ListPort<ApiKeyRow>; revision: number }) {
  return <ListPage title="API Key 管理" description="展示合成 Key 的归属和状态，不包含可用密钥。" port={port} revision={revision} columns={[
    { key: 'name', label: '名称 / 标识', render: row => <div><strong>{row.name}</strong><small className="mono">{row.keyHint}</small></div> },
    { key: 'owner', label: '所属用户', render: row => row.owner },
    { key: 'status', label: '状态', render: row => <Status value={row.status} /> },
    { key: 'lastUsed', label: '最近使用', render: row => dateLabel(row.lastUsedAt) },
  ]} />;
}
