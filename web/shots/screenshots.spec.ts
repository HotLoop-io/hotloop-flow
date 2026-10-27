// Takes the README's screenshots of the editor, in light and in dark, of an
// oven line with a few days of history behind it. Every shot waits until the
// screen shows what it is for, has nothing focused and nothing under the mouse,
// and ends on a whole row, never through one.
//
// The plant is plant-flow.json. Its history is three deployments by three
// people, made through the API, so the History panel has real records in it.
// Nothing here lives in the product.

import { chromium, expect, test as base, type APIRequestContext, type Browser, type Page } from '@playwright/test';
import { lookup } from 'node:dns/promises';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

type Entry = { id: string; [k: string]: unknown };
const plant = JSON.parse(readFileSync(fileURLToPath(new URL('./plant-flow.json', import.meta.url)), 'utf8')) as Entry[];
const out = process.env.SHOTS_OUT ?? 'docs/screenshots';
const W = 1440;

let shared: Promise<Browser> | undefined;
const test = base.extend({
  browser: [
    async ({}, use) => {
      shared ??= (async () => {
        const cdp = process.env.SHOTS_CDP;
        if (cdp) {
          // DevTools refuses a Host header that isn't an address or localhost.
          const [host, port] = cdp.split(':');
          const { address } = await lookup(host!);
          return chromium.connectOverCDP(`http://${address}:${port}`);
        }
        return chromium.launch({ args: ['--no-sandbox'] });
      })();
      await use(await shared);
    },
    { scope: 'worker' },
  ],
});

const password = (user: string) => `demo-${user}-password`;

async function token(request: APIRequestContext, user: string): Promise<string> {
  const res = await request.post('/auth/token', { data: { username: user, password: password(user) } });
  expect(res.ok(), await res.text()).toBeTruthy();
  return ((await res.json()) as { access_token: string }).access_token;
}

async function deploy(request: APIRequestContext, user: string, flows: Entry[], note: string): Promise<void> {
  const res = await request.post('/flows', { headers: { Authorization: `Bearer ${await token(request, user)}` }, data: { flows, note } });
  expect(res.ok(), await res.text()).toBeTruthy();
}

/** Presses the inject node once per reading, a beat apart, as somebody at the line would. */
async function readings(request: APIRequestContext, n: number): Promise<void> {
  const t = await token(request, 'admin');
  for (let i = 0; i < n; i++) {
    const res = await request.post('/inject/i1', { headers: { Authorization: `Bearer ${t}` } });
    expect(res.ok(), await res.text()).toBeTruthy();
    await new Promise((r) => setTimeout(r, 250));
  }
}

test.beforeAll(async ({ playwright, baseURL }) => {
  const request = await playwright.request.newContext({ baseURL });
  // Two records by two people, so the History panel is the real thing: Kim put
  // the oven on the line with the alarm at the data sheet's 205, and the
  // supervisor drew the group round it so nobody drags half of it.
  const first = plant.filter((e) => e.id !== 'gI');
  await deploy(request, 'kim', first, 'Oven 3 on the line, alarm at 205 from the data sheet');
  await deploy(request, 'admin', plant, 'Grouped oven 3 so it moves as one');
  await request.dispose();
});

/** Nothing under the mouse and nothing focused: a picture of a hover is a picture of how it was taken. */
async function calm(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  await page.mouse.move(W - 1, 2);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.waitForTimeout(350);
}

async function signIn(page: Page, user = 'admin'): Promise<void> {
  await page.goto('/');
  await page.locator('#u').fill(user);
  await page.locator('#p').fill(password(user));
  await page.locator('form button[type="submit"]').click();
  await expect(page.locator('.canvas-host > .canvas [data-node-id="s1"]')).toBeVisible();
  await expect(page.locator('.conn')).toContainText('live');
}

/** The editor, in the oven's tab, fit to what is on it. */
async function open(page: Page, request: APIRequestContext, height = 1000): Promise<void> {
  await page.setViewportSize({ width: W, height });
  await signIn(page);
  // No scroll bars in a picture, whatever the browser draws by default.
  await page.addStyleTag({ content: '* { scrollbar-width: none !important; }' });
  await page.getByRole('button', { name: 'Fit to view' }).click();
  await page.waitForTimeout(300);
  await readings(request, 6);
  // Three nodes print one line each: the readings under the alarm, and the two over it.
  await expect(page.locator('.log-line')).toHaveCount(6);
}

/**
 * A list that scrolls inside its panel shows a half row at its edge, and a half
 * row in a picture reads as a bug. This hides the rows an edge would slice, so
 * every row that is shown is whole, and the scroll bars that would be drawn.
 */
async function wholeRows(page: Page): Promise<void> {
  await page.evaluate(() => {
    const scrollers = ['.palette', '.log', '.history', '.dock .runtime, .dock section:last-child', '.rail'];
    const rows = '.palette li, .palette-item, .log-line, .history-item, .dock tbody tr, .trace-hop, .rail > section';
    for (const sel of scrollers) {
      for (const box of Array.from(document.querySelectorAll<HTMLElement>(sel))) {
        let edge = box.getBoundingClientRect();
        // Start on a whole row: scroll the first one an edge slices off the top.
        if (box.scrollHeight > box.clientHeight && box.scrollTop > 0) {
          const first = Array.from(box.querySelectorAll<HTMLElement>(rows)).find((r) => r.getBoundingClientRect().top >= edge.top - 1);
          if (first) box.scrollTop += first.getBoundingClientRect().top - edge.top;
          edge = box.getBoundingClientRect();
        }
        for (const row of Array.from(box.querySelectorAll<HTMLElement>(rows))) {
          const b = row.getBoundingClientRect();
          if (b.height === 0) continue;
          if ((b.top < edge.top - 1 && b.bottom > edge.top + 1) || (b.top < edge.bottom - 1 && b.bottom > edge.bottom + 1)) {
            row.style.visibility = 'hidden';
          }
        }
      }
    }
  });
}

/** No box round anything: not the canvas, not a field, not a button. */
async function noRing(page: Page): Promise<void> {
  const ringed = await page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null;
    return el && el !== document.body ? `${el.tagName} ${el.className}` : '';
  });
  expect(ringed, 'something is focused').toBe('');
}

/** What the picture is for, checked on the screen before and after the colors change. */
async function both(page: Page, name: string, ready: () => Promise<void>): Promise<void> {
  for (const scheme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    await expect(page.locator('html')).toHaveAttribute('data-theme', scheme);
    await ready();
    await calm(page);
    await wholeRows(page);
    await noRing(page);
    await page.screenshot({ path: `${out}/${name}${scheme === 'dark' ? '-dark' : ''}.png` });
  }
}

test('the editor, an oven line running', async ({ page, request }) => {
  await open(page, request);
  await expect(page.locator('.canvas-host > .canvas [data-node-id="s1"] .node-label')).toHaveText('over 205?');
  await both(page, 'editor', async () => {
    await expect(page.locator('.history-item')).toHaveCount(2);
  });
});

test('a change, reviewed before it goes to the line', async ({ page, request }) => {
  await open(page, request);
  // The step that turns the oven's raw count into degrees says what it does.
  await page.locator('.canvas-host > .canvas [data-node-id="r1"]').dblclick();
  await page.locator('.dialog input[type="text"]').first().fill('scale to C');
  await page.locator('.dialog button', { hasText: 'Done' }).click();
  await expect(page.locator('.dialog')).toHaveCount(0);
  await page.getByRole('button', { name: 'Deploy', exact: true }).first().click();
  await expect(page.locator('[data-test="review"]')).toBeVisible();
  await expect(page.locator('[data-test="review-summary"]')).toHaveText('1 changed');
  await expect(page.locator('.canvas-host > .canvas [data-node-id="r1"]')).toHaveClass(/diff-changed/);
  await page.locator('[data-test="deploy-note"]').fill('Says what the step does, which is scale, not just what comes out.');
  await both(page, 'review', async () => {
    await expect(page.locator('[data-test="review-text"]')).toContainText('scale to C');
    // The words of the diff fit their box: nothing runs off the right edge.
    const fits = await page.locator('[data-test="review-text"]').evaluate((e) => ({ w: e.scrollWidth - e.clientWidth, h: e.scrollHeight - e.clientHeight }));
    expect(fits.w, 'the review text runs off its box').toBeLessThanOrEqual(1);
    expect(fits, 'the review text is cut by its box').toMatchObject({ h: 0 });
  });
});

test('debug, and a message traced back along its wires', async ({ page, request }) => {
  await open(page, request);
  // The alarm that fired: its Trace button lights the path it took.
  const alarm = page.locator('.log-line', { hasText: 'alarm' }).first();
  await alarm.hover();
  await alarm.locator('[data-test="debug-trace"]').click();
  await expect(page.locator('.trace-hop').first()).toBeVisible();
  await both(page, 'debug', async () => {
    await expect(page.locator('.trace-hop').first()).toBeVisible();
  });
});

test('sign-in, from the runtime and from the dashboard', async ({ page, baseURL }) => {
  await page.setViewportSize({ width: 800, height: 500 });
  await page.goto(baseURL!);
  await expect(page.locator('form button[type="submit"]')).toBeVisible();
  await both(page, 'signin', async () => {
    await expect(page.getByText('Sign in to the runtime.')).toBeVisible();
  });

  await page.goto(process.env.SHOTS_DASH_URL ?? 'http://127.0.0.1:47125');
  await expect(page.getByText('Open this from the EmberCORE dashboard')).toBeVisible();
  await both(page, 'dashboard-signin', async () => {
    await expect(page.getByText('Open this from the EmberCORE dashboard')).toBeVisible();
  });
});
