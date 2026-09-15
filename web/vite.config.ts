import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // 固定 IPv4:Node 22 默认只绑 ::1,和 README/dev.sh 写的 127.0.0.1 对不上
    host: '127.0.0.1',
    proxy: {
      '/api':  { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/ws':   { target: 'ws://127.0.0.1:8080', ws: true },
      '/sub':  { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/dl':   { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/install.sh': { target: 'http://127.0.0.1:8080', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
  },
});
