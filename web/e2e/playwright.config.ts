import { defineConfig } from '@playwright/test';

const port = Number(process.env.HOTLOOP_FLOW_E2E_PORT ?? 18990);

// One runtime, one browser, one test at a time. The tests deploy to the same
// runtime and read the same deployment log, and running them in parallel would
// be testing the tests.
export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: 'node server.mjs',
    url: `http://127.0.0.1:${port}/health`,
    reuseExistingServer: false,
    timeout: 30_000,
  },
});
