import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    environment: 'node',
    globalSetup: ['test/global-setup.ts'],
    testTimeout: 180000,
    hookTimeout: 300000,
    // The servers and peers are real, so one file at a time keeps the load on the machine honest.
    fileParallelism: false,
  },
});
