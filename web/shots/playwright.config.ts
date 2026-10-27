// The README's screenshots, taken the way the editor's browser tests run: the
// real binary and a real Chrome, one worker. screenshots.sh starts both.
//
//   SHOTS_URL      where the runtime is (the one with its own sign-in)
//   SHOTS_DASH_URL where the runtime that waits for the dashboard is
//   SHOTS_CDP      host:port of a Chrome already running with remote debugging
//   SHOTS_OUT      where the PNGs go (docs/screenshots)
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 300_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: process.env.SHOTS_RESULTS ?? '/tmp/flow-shots-results',
  use: {
    baseURL: process.env.SHOTS_URL ?? 'http://127.0.0.1:47124',
    viewport: { width: 1440, height: 900 },
    locale: 'en-US',
    // The deployment log and the debug dock print times, so they print UTC.
    timezoneId: 'UTC',
    actionTimeout: 15_000,
  },
});
