import { defineConfig } from 'vitest/config';

export default defineConfig({
  server: { host: '127.0.0.1', port: 4178, strictPort: true, proxy: { '/__recovery__': { target: 'http://127.0.0.1:18090', changeOrigin: false } } },
  preview: { host: '127.0.0.1', port: 4178, strictPort: true },
  esbuild: { jsx: 'automatic' },
  test: { environment: 'jsdom', setupFiles: ['./tests/setup.ts'], clearMocks: true },
});
