import type { ListPort } from '../../shared/ports';
import { ListPage } from '../../shared/ListPage';
import { Status, dateLabel } from '../../shared/Status';
import type { UserRow } from './model';

export function UsersPage({ port, revision }: { port: ListPort<UserRow>; revision: number }) {
  return <ListPage title="用户管理" description="查看演示用户、角色与账户状态。" port={port} revision={revision} columns={[
    { key: 'name', label: '用户', render: row => <div className="cell-person"><span className="avatar avatar--small">{row.name.slice(0, 1)}</span><div><strong>{row.name}</strong><small>{row.email}</small></div></div> },
    { key: 'role', label: '角色', render: row => <Status value={row.role} /> },
    { key: 'status', label: '账户状态', render: row => <Status value={row.status} /> },
    { key: 'createdAt', label: '加入时间', render: row => dateLabel(row.createdAt) },
  ]} />;
}
