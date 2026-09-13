import type { ListPort } from '../../shared/ports';
import { ListPage } from '../../shared/ListPage';
import { Status, dateLabel } from '../../shared/Status';
import type { ProviderRow } from './model';

export function ProvidersPage({ port, revision }: { port: ListPort<ProviderRow>; revision: number }) {
  return <ListPage title="Provider 管理" description="查看合成通道目录；这里的状态不代表线上健康情况。" port={port} revision={revision} columns={[
    { key: 'name', label: 'Provider', render: row => <div><strong>{row.name}</strong><small>{row.id}</small></div> },
    { key: 'type', label: '模型平台', render: row => <span className="platform-tag">{row.type}</span> },
    { key: 'status', label: '演示状态', render: row => <Status value={row.status} /> },
    { key: 'models', label: '模型数', render: row => <span className="mono">{row.modelCount}</span> },
    { key: 'updatedAt', label: '更新时间', render: row => dateLabel(row.updatedAt) },
  ]} />;
}
