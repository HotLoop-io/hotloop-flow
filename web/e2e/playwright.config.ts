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
  // The page is judged by what it shows, so every expect waits on the real event
  // for as long as the test may run. The default 5 s is a guess at how fast the
  // browser and the runtime are, and a box carrying other work is slower than that.
  expect: { timeout: 20_000 },
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
