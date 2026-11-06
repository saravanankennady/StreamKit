import { join } from 'node:path';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  resolve: {
    // Hooks and tests must share one copy of the client's classes, built from the source in the tree.
    alias: { '@elnova-streamkit/client': join(__dirname, '../sdk-js/src/index.ts') },
    dedupe: ['react', 'react-dom'],
  },
  esbuild: { jsx: 'automatic' },
  test: {
    include: ['test/**/*.test.tsx'],
    environment: 'jsdom',
  },
});
