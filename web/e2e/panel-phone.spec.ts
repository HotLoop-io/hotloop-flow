// P4: the panel runs in a browser with nothing to install, on a phone, and several
// people can watch one run. A phone opens the panel while somebody else changes a
// control, and its indicator and chart follow within a second, nothing runs off the
// side of the page, and a second watcher on a desktop sees the same value.

import { expect, test, type Browser, type Page } from '@playwright/test';
import { seed, signIn, token, type Entry } from './helpers';

// A loop that counts every 100 ms and shows its pass number on an indicator and a
// chart, both inside the loop's body, so the panel moves while the run is live.
function diagram(): Entry[] {
  const n = (z: string, id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
    ({ id, type, z, x, y, wires, ew_in: ins, ...extra });
  return [
    { id: 'main', type: 'tab', label: 'Phone panel', ew_diagram: {} },
    { id: 'body', type: 'tab', label: 'Phone panel body', ew_diagram: { role: 'body' } },
    n('main', 'loop', 'dia-while', 200, 100, { body: 'Phone panel body', carried: '', ins: '', outs: '', start: '{}', maxIterations: 0 }),
    n('body', 'iter', 'dia-body-in', 100, 100, { name: 'iter' }, [['seen', 'hist', 'pace']], [['value', 'y', 'value']]),
    n('body', 'seen', 'dia-indicator', 400, 60, { label: 'Pass', kind: 'number', style: 'big', digits: 0 }),
    n('body', 'hist', 'dia-chart', 400, 160, { label: 'Pass history', mode: 'chart' }),
    n('body', 'pace', 'dia-wait', 300, 260, { ms: 100 }),
    n('body', 'no', 'dia-const', 100, 300, { value: 'false' }, [['stop']], [['value']]),
    n('body', 'stop', 'dia-body-out', 300, 300, { name: 'stop' }),
  ];
}

const ind = (page: Page) => page.locator('[data-test="ind"][data-id="seen"] [data-test="ind-value"]');

async function openPanel(browser: Browser, viewport: { width: number; height: number }, touch: boolean): Promise<Page> {
  const ctx = await browser.newContext({ viewport, hasTouch: touch });
  const page = await ctx.newPage();
  await signIn(page, 'loop');
  await page.evaluate(() => { location.hash = '#/panel/main'; location.reload(); });
  await expect(page.locator('[data-test="panel"]')).toBeVisible();
  await expect(page.locator('.conn')).toContainText('live');
  return page;
}

test('a phone and a desktop watch one live run, and both follow it within a second', async ({ browser, request }) => {
  test.setTimeout(120_000);
  await seed(request, diagram());
  const headers = { Authorization: `Bearer ${await token(request)}` };
  const phone = await openPanel(browser, { width: 390, height: 844 }, true);
  const desk = await openPanel(browser, { width: 1280, height: 800 }, false);

  // Somebody starts the run, and the run is live while both watch it.
  const started = await request.post('/diagrams/main/run', { headers, data: {} });
  expect(started.ok(), await started.text()).toBeTruthy();

  const num = async (page: Page) => Number((await ind(page).innerText()).trim());
  for (const page of [phone, desk]) {
    await expect.poll(async () => num(page), { timeout: 3000, message: 'the first pass never reached the panel' }).toBeGreaterThanOrEqual(0);
    // Each step of the count shows up inside a second of the one before it.
    for (let i = 0; i < 4; i++) {
      const seen = await num(page);
      await expect.poll(async () => num(page), { timeout: 1000, message: `the pass after ${seen} did not reach this panel within a second` }).toBeGreaterThan(seen);
    }
  }

  // The page fits the phone with values on it: no sideways scroll, the chart inside the screen.
  const overflow = await phone.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow, 'the panel scrolls sideways on a phone').toBeLessThanOrEqual(0);
  const chart = await phone.locator('[data-test="chart"][data-id="hist"]').boundingBox();
  expect(chart, 'the chart is not drawn').not.toBeNull();
  expect(chart!.x + chart!.width, 'the chart runs past the right edge').toBeLessThanOrEqual(390 + 0.5);
  for (const page of [phone, desk]) {
    await expect(page.locator('[data-test="chart"][data-id="hist"] [data-test="chart-count"]')).toContainText(/\d/);
  }
  // The same run, the same pass, on both: they are never far apart.
  const a = await num(phone), b = await num(desk);
  expect(Math.abs(a - b), `the phone is on pass ${a} and the desktop on ${b}`).toBeLessThanOrEqual(15);

  await request.post('/diagrams/main/stop', { headers, data: {} });
  await phone.context().close();
  await desk.context().close();
});
