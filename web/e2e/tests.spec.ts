// Flow tests from the editor, clicked through in a real browser against the
// real binary: a message the debug sidebar showed becomes a test in two clicks,
// the suite runs against the working copy, and the outcome lands on the nodes.
// Every claim on the screen is checked against the API as well.

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

const password = 'e2e-admin-password';

async function token(request: APIRequestContext, user = 'admin', pw = password): Promise<string> {
  const res = await request.post('/auth/token', { data: { username: user, password: pw } });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).access_token as string;
}

/** A reading from an Inject button, labelled by a Change node, shown by a Debug node. */
function pressed(label: string): unknown[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3' },
    {
      id: 'i1', type: 'inject', z: 't1', name: 'reading',
      props: [{ p: 'payload', v: '50', vt: 'num' }], x: 120, y: 120, wires: [['c1']],
    },
    {
      id: 'c1', type: 'change', z: 't1', name: 'label',
      rules: [{ t: 'set', p: 'payload', pt: 'msg', to: label, tot: 'str' }],
      x: 320, y: 120, wires: [['d1']],
    },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 540, y: 120, wires: [] },
  ];
}

async function deploy(request: APIRequestContext, flows: unknown[], user = 'admin', pw = password): Promise<void> {
  const res = await request.post('/flows', { headers: { Authorization: `Bearer ${await token(request, user, pw)}` }, data: flows });
  expect(res.ok(), await res.text()).toBeTruthy();
}

async function signIn(page: Page): Promise<void> {
  await page.goto('/');
  await page.locator('#u').fill('admin');
  await page.locator('#p').fill(password);
  await page.locator('form button[type="submit"]').click();
  await expect(page.locator('.canvas-host > .canvas [data-node-id="d1"]')).toBeVisible();
  await expect(page.locator('.conn')).toContainText('live');
}

const mark = (page: Page, id: string) =>
  page.locator(`.canvas-host > .canvas [data-node-id="${id}"] [data-test-mark]`);

test('a debug message becomes a test in two clicks, and the run is drawn on the nodes', async ({ page, request }) => {
  await deploy(request, pressed('LINE 3'));
  await signIn(page);

  // A reading goes through the running flow and the debug sidebar shows it.
  const press = await request.post('/inject/i1', { headers: { Authorization: `Bearer ${await token(request)}` } });
  expect(press.ok(), await press.text()).toBeTruthy();
  const line = page.locator('.log-line', { hasText: 'LINE 3' }).last();
  await expect(line).toBeVisible();

  // Click one: the draft, made from what went in and what came out.
  await line.locator('[data-test="debug-make-test"]').click();
  const draft = page.locator('[data-test="test-draft"]');
  await expect(draft).toBeVisible();
  await expect(draft).toHaveValue(/node: i1/);
  await expect(draft).toHaveValue(/payload: 50/);
  await expect(draft).toHaveValue(/node: d1/);
  await expect(draft).toHaveValue(/payload: LINE 3/);

  // Click two: it's in the suite.
  await page.locator('[data-test="test-add"]').click();
  await expect(page.locator('[data-test="tests-note"]')).toContainText('Added: out gets "LINE 3" from reading');
  await expect(page.locator('[data-test="tests-info"]')).toContainText('1 test in flows.test.yaml');
  const kept = await (await request.get('/tests', { headers: { Authorization: `Bearer ${await token(request)}` } })).json();
  expect(kept.count).toBe(1);
  expect(kept.tests).toContain('node: d1');

  // Run it against the working copy: it passes, and the nodes say so.
  await page.locator('[data-test="tests-run"]').click();
  await expect(page.locator('[data-test="tests-summary"]')).toHaveText("1 passed, 0 failed, 0 couldn't run");
  await expect(mark(page, 'd1')).toHaveAttribute('data-test-mark', 'pass');
  await expect(mark(page, 'i1')).toHaveAttribute('data-test-mark', 'pass');

  // Somebody changes the label. The same test against the new working copy
  // fails on the Debug node, with the reason, and the reason finds the node.
  await deploy(request, pressed('LINE 9'));
  await page.reload();
  await expect(page.locator('.canvas-host > .canvas [data-node-id="d1"]')).toBeVisible();
  await page.locator('[data-test="tests-run"]').click();
  await expect(page.locator('[data-test="tests-summary"]')).toHaveText("0 passed, 1 failed, 0 couldn't run");
  await expect(mark(page, 'd1')).toHaveAttribute('data-test-mark', 'fail');
  const problem = page.locator('.test-problem');
  await expect(problem).toContainText('msg.payload: wanted "LINE 3", got "LINE 9"');
  await problem.click();
  await expect(page.locator('.canvas-host > .canvas [data-node-id="d1"]')).toHaveClass(/selected/);

  // An edit means the marks describe a flow that no longer exists, so they go.
  await page.locator('.canvas-host > .canvas [data-node-id="c1"]').first().dblclick();
  await page.locator('.dialog input[type="text"]').first().fill('label two');
  await page.locator('.dialog button', { hasText: 'Done' }).click();
  await expect(mark(page, 'd1')).toHaveCount(0);

  // The whole suite is there to read and edit as YAML.
  await page.locator('[data-test="tests-edit"]').click();
  await expect(page.locator('[data-test="tests-yaml"]')).toHaveValue(/out gets "LINE 3" from reading/);
  await page.locator('[data-test="tests-cancel"]').click();
});
