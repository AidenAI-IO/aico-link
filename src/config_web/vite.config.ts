import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

const root = fileURLToPath(new URL('./', import.meta.url));
export default defineConfig({
  root,
  plugins: [react()],
  server: {
    proxy: {
      '/api': process.env.AICO_CONFIG_WEB_URL || 'http://192.168.42.1',
      '/login': process.env.AICO_CONFIG_WEB_URL || 'http://192.168.42.1',
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2020',
    rolldownOptions: {
      output: {
        codeSplitting: { groups: [{ name: 'vendor', test: /node_modules/ }] },
      },
      input: {
        device: `${root}/index.html`,
        local: `${root}/local.html`,
        container: `${root}/container.html`,
        logs: `${root}/llm-logs.html`,
      },
    },
  },
  test: { include: ['src/**/*.test.{ts,tsx}'], environment: 'jsdom', restoreMocks: true },
});
