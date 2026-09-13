import { createRoot } from 'react-dom/client';
import { App } from './App';
import { createMockPorts } from '../mock/createMockPorts';
import { createDemoHttpPorts } from '../adapters/demoHttpPorts';
import './styles.css';

const root = createRoot(document.getElementById('root')!);
const configured = import.meta.env.VITE_RECOVERY_BACKEND ?? 'mock';
if (configured === 'mock' || configured === '') {
  const mock = createMockPorts();
  root.render(<App ports={mock.ports} controls={mock.controls} mode="mock" />);
} else if (configured === 'go') {
  try { root.render(<App ports={createDemoHttpPorts()} mode="go" />); }
  catch { root.render(<main className="boot-state" role="alert"><h1>仅允许本地 Go 演示</h1><p>请通过回环地址访问，不会连接任何外部 API。</p></main>); }
} else root.render(<main className="boot-state" role="alert"><h1>演示模式配置无效</h1><p>仅支持 mock 或 go，不接受自定义远程服务地址。</p></main>);
