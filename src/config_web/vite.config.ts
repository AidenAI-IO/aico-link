import { cpSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

const root = fileURLToPath(new URL('./web', import.meta.url));
export default defineConfig({
  root,
  publicDir: false,
  plugins: [
    react(),
    {
      name: 'device-static-assets',
      closeBundle() {
        for (const path of ['assets/icons']) {
          cpSync(`${root}/${path}`, `${root}/../dist/${path}`, { recursive: true });
        }
      },
    },
  ],
  server: {
    proxy: {
      '/api': process.env.AICO_CONFIG_WEB_URL || 'http://192.168.42.1',
      '/login': process.env.AICO_CONFIG_WEB_URL || 'http://192.168.42.1',
    },
  },
  build: {
    outDir: '../dist',
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
  test: { include: ['react/**/*.test.{ts,tsx}'], environment: 'jsdom', restoreMocks: true },
});
