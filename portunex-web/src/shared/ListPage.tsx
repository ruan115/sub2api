import { useEffect, useState, type ReactNode } from 'react';
import { PortError, type ListPort, type Page } from './ports';

interface Column<T> { key: string; label: string; render(row: T): ReactNode }
type State<T> = { kind: 'loading' } | { kind: 'ready'; page: Page<T> } | { kind: 'error' | 'unauthorized'; message: string };

export function ListPage<T extends { id: string }>({ title, description, port, columns, revision }: { title: string; description: string; port: ListPort<T>; columns: Column<T>[]; revision: number }) {
  const [query, setQuery] = useState('');
  const [page, setPage] = useState(1);
  const [retry, setRetry] = useState(0);
  const [state, setState] = useState<State<T>>({ kind: 'loading' });
  useEffect(() => {
    const controller = new AbortController();
    setState({ kind: 'loading' });
    port.list({ query, page, pageSize: 8 }, controller.signal).then(result => {
      if (!controller.signal.aborted) setState({ kind: 'ready', page: result });
    }).catch(error => {
      if (controller.signal.aborted) return;
      setState({ kind: error instanceof PortError && error.code === 'unauthorized' ? 'unauthorized' : 'error', message: error instanceof PortError ? error.message : '演示请求暂时失败，请稍后重试。' });
    });
    return () => controller.abort();
  }, [port, query, page, retry, revision]);
  return <section aria-labelledby="list-title">
    <div className="page-heading"><div><p className="eyebrow">管理 / {title}</p><h1 id="list-title">{title}</h1><p>{description}</p></div><button className="button button--secondary" onClick={() => setRetry(value => value + 1)}>↻ 刷新列表</button></div>
    <div className="panel">
      <div className="table-toolbar"><label className="search-field"><span aria-hidden="true">⌕</span><input aria-label={`搜索${title}`} placeholder={`搜索${title}…`} value={query} onChange={event => { setQuery(event.target.value); setPage(1); }} /></label><span className="subtle">{state.kind === 'ready' ? `${state.page.total} 条合成记录` : '本地数据集'}</span></div>
      {state.kind === 'loading' && <div className="state-panel" role="status"><span className="spinner" /><h2>正在加载{title}</h2><p>正在读取本地演示数据。</p></div>}
      {(state.kind === 'error' || state.kind === 'unauthorized') && <div className="state-panel" role="alert"><span className="state-symbol">{state.kind === 'unauthorized' ? '⊘' : '!'}</span><h2>{state.kind === 'unauthorized' ? '暂无访问权限' : '暂时无法加载'}</h2><p>{state.message}</p><button className="button button--secondary" onClick={() => setRetry(value => value + 1)}>重试</button></div>}
      {state.kind === 'ready' && state.page.items.length === 0 && <div className="state-panel" role="status"><span className="state-symbol">∅</span><h2>{query ? '没有找到匹配记录' : `暂无${title}`}</h2><p>{query ? '试试其他搜索条件。' : '当前演示数据集为空，没有请求线上数据。'}</p></div>}
      {state.kind === 'ready' && state.page.items.length > 0 && <div className="table-scroll"><table><thead><tr>{columns.map(column => <th key={column.key} scope="col">{column.label}</th>)}</tr></thead><tbody>{state.page.items.map(row => <tr key={row.id}>{columns.map(column => <td key={column.key}>{column.render(row)}</td>)}</tr>)}</tbody></table></div>}
      <div className="table-footer"><span>仅用于界面与权限验证 · 不提供写入操作</span><div><button className="page-button" aria-label="上一页" disabled={page <= 1 || state.kind !== 'ready'} onClick={() => setPage(value => value - 1)}>‹</button><span>第 {page} 页</span><button className="page-button" aria-label="下一页" disabled={state.kind !== 'ready' || page * 8 >= state.page.total} onClick={() => setPage(value => value + 1)}>›</button></div></div>
    </div>
  </section>;
}
