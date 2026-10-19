// Tracing, clicked through in a real browser against the real binary: a debug
// message lights the path it took across a split, a join and a subflow, with
// the time each node had it and the message at every hop; the trace becomes a
// test; and the message goes into a draft of the flow and comes back compared.
// Every claim on the screen is checked against the API as well.

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

const password = 'e2e-admin-password';

async function token(request: APIRequestContext): Promise<string> {
  const res = await request.post('/auth/token', { data: { username: 'admin', password } });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).access_token as string;
}

/**
 * One reading becomes three, each is marked, a join puts them back together, a
 * subflow stamps the result and a Debug node shows it. A second, unconnected
 * pair sits beside it, a branch the message never takes.
 */
const fan: unknown[] = [
  { id: 't1', type: 'tab', label: 'Line 3' },
  { id: 'go', type: 'inject', z: 't1', name: 'go', props: [{ p: 'payload', v: '[1,2,3]', vt: 'json' }], x: 100, y: 120, wires: [['split']] },
  { id: 'split', type: 'split', z: 't1', name: 'split', x: 240, y: 120, wires: [['bump']] },
  {
    id: 'bump', type: 'change', z: 't1', name: 'bump',
    rules: [{ t: 'set', p: 'seen', pt: 'msg', to: 'yes', tot: 'str' }], x: 380, y: 120, wires: [['join']],
  },
  { id: 'join', type: 'join', z: 't1', name: 'join', mode: 'custom', build: 'array', count: '3', timeout: '30', x: 520, y: 120, wires: [['inst']] },
  {
    id: 'sf', type: 'subflow', name: 'stamp',
    in: [{ x: 0, y: 0, wires: [{ id: 'mark' }] }], out: [{ x: 0, y: 0, wires: [{ id: 'mark', port: 0 }] }],
  },
  {
    id: 'mark', type: 'change', z: 'sf', name: 'mark',
    rules: [{ t: 'set', p: 'stamped', pt: 'msg', to: 'true', tot: 'bool' }], x: 100, y: 100, wires: [[]],
  },
  { id: 'inst', type: 'subflow:sf', z: 't1', name: 'stamper', x: 660, y: 120, wires: [['end']] },
  { id: 'end', type: 'debug', z: 't1', name: 'end', complete: 'payload', x: 800, y: 120, wires: [] },
  { id: 'idle', type: 'inject', z: 't1', name: 'idle', props: [{ p: 'payload', v: '1', vt: 'num' }], x: 100, y: 260, wires: [['quiet']] },
  { id: 'quiet', type: 'debug', z: 't1', name: 'quiet', complete: 'payload', x: 240, y: 260, wires: [] },
];

const canvas = '.canvas-host > .canvas';
const node = (page: Page, id: string) => page.locator(`${canvas} [data-node-id="${id}"]`);

async function signIn(page: Page): Promise<void> {
  await page.goto('/');
  await page.locator('#u').fill('admin');
  await page.locator('#p').fill(password);
  await page.locator('form button[type="submit"]').click();
  await expect(node(page, 'end')).toBeVisible();
  await expect(page.locator('.conn')).toContainText('live');
}

test('a debug message lights the path it took, becomes a test, and replays into a draft', async ({ page, request }) => {
  const auth = { Authorization: `Bearer ${await token(request)}` };
  const deployed = await request.post('/flows', { headers: auth, data: fan });
  expect(deployed.ok(), await deployed.text()).toBeTruthy();
  await signIn(page);

  const press = await request.post('/inject/go', { headers: auth });
  expect(press.ok(), await press.text()).toBeTruthy();
  const line = page.locator('.log-line', { hasText: '[1,2,3]' }).last();
  await expect(line).toBeVisible();

  // Nothing is lit until somebody asks.
  await expect(page.locator(`${canvas}.tracing`)).toHaveCount(0);

  await line.locator('[data-test="debug-trace"]').click();
  await expect(page.locator(`${canvas}.tracing`)).toHaveCount(1);

  // Every node on the way is lit, the subflow instance included; the branch
  // the message never took is not.
  for (const id of ['go', 'split', 'bump', 'join', 'inst', 'end']) {
    await expect(node(page, id), `${id} should be lit`).toHaveClass(/trace-hit/);
  }
  await expect(node(page, 'end')).toHaveClass(/trace-focus/);
  for (const id of ['idle', 'quiet']) {
    await expect(node(page, id), `${id} should not be lit`).not.toHaveClass(/trace-hit/);
  }

  // The five wires it travelled, and none of the other one.
  await expect(page.locator(`${canvas} .wire.trace-on`)).toHaveCount(5);
  await expect(page.locator(`${canvas} .wire[data-from="idle"]`)).not.toHaveClass(/trace-on/);

  // Timing on the node, and the three pieces of the split counted.
  const time = (id: string) => node(page, id).locator('[data-trace-time]');
  await expect(time('bump')).toHaveText(/\d+(\.\d)? (ns|µs|ms|s) ×3/);
  await expect(time('join')).toHaveText(/ ×3$/);
  await expect(time('split')).toHaveText(/^\d+(\.\d)? (ns|µs|ms|s)$/);
  await expect(node(page, 'idle').locator('[data-trace-time]')).toHaveCount(0);

  // The sidebar says the same thing hop by hop, with the message at each.
  const rows = page.locator('[data-test="trace-hop"]');
  await expect(page.locator('[data-test="trace-summary"]')).toContainText('hops');
  const count = await rows.count();
  expect(count).toBeGreaterThanOrEqual(11); // go, split, 3 bump, 3 join, stamper, mark, end
  await expect(rows.filter({ hasText: 'bump' })).toHaveCount(3);
  await expect(rows.filter({ hasText: 'join' })).toHaveCount(3);
  const last = rows.last();
  await expect(last).toContainText('end');
  await expect(last.locator('[data-test="trace-hop-msg"]')).toHaveText('[1,2,3]');
  await expect(last.locator('[data-test="trace-hop-time"]')).toHaveText(/^\d+(\.\d)? (ns|µs|ms|s)$/);
  await expect(last).toHaveClass(/focus/);

  // And the API serves the same trace.
  const list = await (await request.get('/traces', { headers: auth })).json();
  expect(list.traces.length).toBeGreaterThanOrEqual(1);
  const one = await (await request.get(`/traces/${list.traces[0].id}`, { headers: auth })).json();
  expect(one.trace.hops.length).toBe(count);

  // The trace becomes a test in two clicks.
  const before = (await (await request.get('/tests', { headers: auth })).json()).count as number;
  await page.locator('[data-test="trace-make-test"]').click();
  const draft = page.locator('[data-test="test-draft"]');
  await expect(draft).toBeVisible();
  await expect(draft).toHaveValue(/node: go/);
  await expect(draft).toHaveValue(/node: end/);
  await expect(draft).toHaveValue(/payload:\n\s+- 1\n\s+- 2\n\s+- 3|payload: \[1, 2, 3\]/);
  await page.locator('[data-test="test-add"]').click();
  await expect(page.locator('[data-test="tests-note"]')).toContainText('Added:');
  const kept = await (await request.get('/tests', { headers: auth })).json();
  expect(kept.count).toBe(before + 1);
  expect(kept.tests).toContain('node: end');

  // Replay it into a draft with the Change node taken out. The working copy is
  // what gets replayed into, deployed or not, so nothing is deployed here.
  await node(page, 'bump').click();
  await page.keyboard.press('Delete');
  await expect(node(page, 'bump')).toHaveCount(0);
  await page.locator('[data-test="trace-replay"]').click();
  await expect(page.locator('[data-test="replay-summary"]')).toContainText('differs');
  const diff = page.locator('[data-test="replay-diff"][data-node="end"]');
  await expect(diff).toBeVisible();
  await expect(diff).toContainText('missing');
  // The replay's own path is what's lit now: it got as far as the split.
  await expect(node(page, 'split')).toHaveClass(/trace-hit/);
  await expect(node(page, 'join')).not.toHaveClass(/trace-hit/);

  // Back to the original.
  await page.locator('[data-test="replay-back"]').click();
  await expect(page.locator('[data-test="trace-hop"]')).toHaveCount(count);

  // Clear puts the canvas back.
  await page.locator('[data-test="trace-clear"]').click();
  await expect(page.locator(`${canvas}.tracing`)).toHaveCount(0);
});
