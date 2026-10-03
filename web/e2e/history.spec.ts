// The editor's history, review and rollback, clicked through in a real browser
// against the real binary. Every assertion about what happened is checked twice
// where it matters: once on the screen, once against the API, because a UI
// that says "deployed" over a runtime that didn't is the worst kind of green.

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

const users = {
  admin: 'e2e-admin-password',
  sam: 'e2e-sam-password',
} as const;
type User = keyof typeof users;

async function tokenFor(request: APIRequestContext, user: User): Promise<string> {
  const res = await request.post('/auth/token', { data: { username: user, password: users[user] } });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).access_token as string;
}

/** A change node that sets the payload, wired to a debug node. */
function lineThree(name: string, x = 300): unknown[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3' },
    {
      id: 'c1', type: 'change', z: 't1', name,
      rules: [{ t: 'set', p: 'payload', pt: 'msg', to: 'one', tot: 'str' }],
      x, y: 120, wires: [['d1']],
    },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 560, y: 120, wires: [] },
  ];
}

/** Deploys over the API as somebody else would, and returns the deployment number. */
async function deployAs(request: APIRequestContext, user: User, flows: unknown[], note = ''): Promise<number> {
  const token = await tokenFor(request, user);
  const res = await request.post('/flows', {
    headers: { Authorization: `Bearer ${token}` },
    data: note ? { flows, note } : flows,
  });
  expect(res.ok(), await res.text()).toBeTruthy();
  return (await res.json()).deployment as number;
}

async function live(request: APIRequestContext): Promise<{ rev: string; flows: { id: string; name?: string; x?: number }[] }> {
  const token = await tokenFor(request, 'admin');
  const res = await request.get('/flows', { headers: { Authorization: `Bearer ${token}` } });
  return res.json();
}

async function latestDeployment(request: APIRequestContext): Promise<{ seq: number; user?: string; note?: string; kind: string; rollbackOf?: number }> {
  const token = await tokenFor(request, 'admin');
  const res = await request.get('/deployments?limit=1', { headers: { Authorization: `Bearer ${token}` } });
  return (await res.json()).deployments[0];
}

async function signIn(page: Page): Promise<void> {
  await page.goto('/');
  await page.locator('#u').fill('admin');
  await page.locator('#p').fill(users.admin);
  await page.locator('form button[type="submit"]').click();
  await expect(page.locator('.canvas [data-node-id="c1"]').first()).toBeVisible();
}

async function rename(page: Page, id: string, name: string): Promise<void> {
  await page.locator(`.canvas [data-node-id="${id}"]`).first().dblclick();
  const input = page.locator('.dialog input[type="text"]').first();
  await input.fill(name);
  await page.locator('.dialog button', { hasText: 'Done' }).click();
  await expect(page.locator('.dialog')).toHaveCount(0);
}

const nodeLabel = (page: Page, id: string) => page.locator(`.canvas-host > .canvas [data-node-id="${id}"] .node-label`);
const topDeploy = (page: Page) => page.locator('.topbar button', { hasText: 'Deploy' });

test('review shows the change on the canvas and in words, and the note lands in the history', async ({ page, request }) => {
  await deployAs(request, 'admin', lineThree('set'), 'seed');
  await signIn(page);
  // The event stream authenticates with its token as a subprotocol, never in
  // the URL. A browser that didn't accept that negotiation would never say live.
  await expect(page.locator('.conn')).toContainText('live');

  await rename(page, 'c1', 'set two');
  await topDeploy(page).click();

  const review = page.locator('[data-test="review"]');
  await expect(review).toBeVisible();
  await expect(page.locator('[data-test="review-summary"]')).toHaveText('1 changed');
  await expect(page.locator('[data-test="review-text"]')).toContainText('name: "set" -> "set two"');
  await expect(page.locator('.canvas-host > .canvas [data-node-id="c1"]')).toHaveClass(/diff-changed/);
  // The debug node didn't change and isn't marked.
  await expect(page.locator('.canvas-host > .canvas [data-node-id="d1"]')).not.toHaveClass(/diff-/);

  await page.locator('[data-test="deploy-note"]').fill('Linie 3: Umbenennung');
  await page.locator('[data-test="deploy-confirm"]').click();

  await expect(review).toHaveCount(0);
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  await expect(page.locator('.canvas-host > .canvas [data-node-id="c1"]')).not.toHaveClass(/diff-/);
  const top = page.locator('[data-test="history-item"]').first();
  await expect(top.locator('[data-test="history-note"]')).toHaveText('Linie 3: Umbenennung');
  await expect(top).toContainText('admin');

  // And the runtime agrees.
  const d = await latestDeployment(request);
  expect(d.user).toBe('admin');
  expect(d.note).toBe('Linie 3: Umbenennung');
  expect((await live(request)).flows.find((e) => e.id === 'c1')?.name).toBe('set two');
});

test('dragging a node reviews as moved, never as a change', async ({ page, request }) => {
  await deployAs(request, 'admin', lineThree('set'), 'seed');
  await signIn(page);

  const node = page.locator('.canvas-host > .canvas [data-node-id="c1"] .node-body');
  const box = (await node.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 120, box.y + box.height / 2 + 60, { steps: 8 });
  await page.mouse.up();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('unsaved changes');

  await topDeploy(page).click();
  await expect(page.locator('[data-test="review-summary"]')).toHaveText('1 moved');
  await expect(page.locator('[data-test="review-text"]')).toContainText('Layout only');
  await expect(page.locator('.canvas-host > .canvas [data-node-id="c1"]')).toHaveClass(/diff-moved/);

  // Backing out deploys nothing.
  const before = await live(request);
  await page.locator('[data-test="deploy-cancel"]').click();
  await expect(page.locator('[data-test="review"]')).toHaveCount(0);
  expect((await live(request)).rev).toBe(before.rev);
});

test('a deployment from the history opens read-only, with its changes drawn', async ({ page, request }) => {
  await deployAs(request, 'admin', lineThree('set'), 'seed');
  const seq = await deployAs(request, 'sam', lineThree('set by sam'), 'sam changed it');
  await signIn(page);

  const item = page.locator(`[data-test="history-item"][data-seq="${seq}"]`);
  await expect(item).toContainText('sam changed it');
  await item.locator('[data-test="history-changes"]').click();

  const banner = page.locator('[data-test="view-banner"]');
  await expect(banner).toBeVisible();
  await expect(banner).toContainText('sam');
  await expect(banner).toContainText('1 changed');
  await expect(page.locator('[data-test="tab-state"]')).toContainText(`viewing deployment ${seq}`);

  const viewed = page.locator('.view-host [data-node-id="c1"]');
  await expect(viewed).toHaveClass(/diff-changed/);
  await expect(viewed.locator('.node-label')).toHaveText('set by sam');

  // Read-only: a drag moves nothing, and Deploy stays off.
  const body = viewed.locator('.node-body');
  const x = await body.getAttribute('x');
  const box = (await body.boundingBox())!;
  await page.mouse.move(box.x + 10, box.y + 10);
  await page.mouse.down();
  await page.mouse.move(box.x + 150, box.y + 80, { steps: 6 });
  await page.mouse.up();
  await expect(body).toHaveAttribute('x', x!);
  await expect(topDeploy(page)).toBeDisabled();

  await page.locator('[data-test="view-close"]').click();
  await expect(banner).toBeHidden();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
});

test('rolling back from the history puts the old flows back, as a new record', async ({ page, request }) => {
  const good = await deployAs(request, 'admin', lineThree('known good'), 'the good one');
  await deployAs(request, 'sam', lineThree('broken'), 'oops');
  await signIn(page);
  await expect(nodeLabel(page, 'c1')).toHaveText('broken');

  await page.locator(`[data-test="history-item"][data-seq="${good}"] [data-test="history-rollback"]`).click();
  await page.locator('[data-test="rollback-note"]').fill('broken stopped the labeller');
  await page.locator('[data-test="rollback-confirm"]').click();

  await expect(nodeLabel(page, 'c1')).toHaveText('known good');
  const top = page.locator('[data-test="history-item"]').first();
  await expect(top).toContainText(`rollback to #${good}`);
  await expect(top.locator('[data-test="history-note"]')).toHaveText(`rollback to deployment ${good}: broken stopped the labeller`);

  const d = await latestDeployment(request);
  expect(d.kind).toBe('rollback');
  expect(d.rollbackOf).toBe(good);
  expect((await live(request)).flows.find((e) => e.id === 'c1')?.name).toBe('known good');
});

test('a deploy conflict shows what the other person changed, and loading theirs throws mine away', async ({ page, request }) => {
  await deployAs(request, 'admin', lineThree('set'), 'seed');
  await signIn(page);
  await rename(page, 'c1', 'mine');

  // Somebody else gets there first.
  await deployAs(request, 'sam', lineThree('theirs'), 'sam got there first');

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();

  const changes = page.locator('[data-test="their-changes"]');
  await expect(changes).toContainText('name: "set" -> "theirs"');
  await expect(page.locator('.dialog')).toContainText('sam got there first');
  await expect(page.locator('.dialog')).toContainText('by sam');

  await page.locator('[data-test="conflict-theirs"]').click();
  await expect(nodeLabel(page, 'c1')).toHaveText('theirs');
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  expect((await live(request)).flows.find((e) => e.id === 'c1')?.name).toBe('theirs');
});

test('a deploy conflict can be overwritten on purpose', async ({ page, request }) => {
  await deployAs(request, 'admin', lineThree('set'), 'seed');
  await signIn(page);
  await rename(page, 'c1', 'mine');
  await deployAs(request, 'sam', lineThree('theirs'), 'sam got there first');

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('mine wins, talked to sam');
  await page.locator('[data-test="deploy-confirm"]').click();
  await page.locator('[data-test="conflict-mine"]').click();

  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  const d = await latestDeployment(request);
  expect(d.user).toBe('admin');
  expect(d.note).toBe('mine wins, talked to sam');
  expect((await live(request)).flows.find((e) => e.id === 'c1')?.name).toBe('mine');
});
