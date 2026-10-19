// What the editor specs share: a way to put the runtime in a known state over
// the API, a way into the editor, and the few canvas gestures that need real
// mouse movement. Nothing here is a mock. Every spec talks to the binary that
// ships, and checks what the screen says against what the API says.

import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test';

export const password = 'e2e-admin-password';

export async function token(request: APIRequestContext): Promise<string> {
  const res = await request.post('/auth/token', { data: { username: 'admin', password } });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).access_token as string;
}

export type Entry = { id: string; type: string; z?: string; g?: string; name?: string; x?: number; y?: number; wires?: string[][]; [k: string]: unknown };

/** Replaces everything that is deployed. */
export async function seed(request: APIRequestContext, flows: unknown[]): Promise<void> {
  const res = await request.post('/flows', { headers: { Authorization: `Bearer ${await token(request)}` }, data: flows });
  expect(res.ok(), await res.text()).toBeTruthy();
}

/** What is deployed right now. */
export async function deployed(request: APIRequestContext): Promise<Entry[]> {
  const res = await request.get('/flows', { headers: { Authorization: `Bearer ${await token(request)}` } });
  expect(res.ok()).toBeTruthy();
  return (await res.json()).flows as Entry[];
}

export async function press(request: APIRequestContext, injectId: string): Promise<void> {
  const res = await request.post(`/inject/${injectId}`, { headers: { Authorization: `Bearer ${await token(request)}` } });
  expect(res.ok(), await res.text()).toBeTruthy();
}

export async function signIn(page: Page, waitFor = 'd1'): Promise<void> {
  await page.goto('/');
  await page.locator('#u').fill('admin');
  await page.locator('#p').fill(password);
  await page.locator('form button[type="submit"]').click();
  await expect(canvasNode(page, waitFor)).toBeVisible();
  await expect(page.locator('.conn')).toContainText('live');
}

/** A node on the working canvas, not the read-only one for the history. */
export const canvasNode = (page: Page, id: string): Locator =>
  page.locator(`.canvas-host > .canvas [data-node-id="${id}"]`);

export const nodeBody = (page: Page, id: string): Locator => canvasNode(page, id).locator('.node-body');

export const allNodes = (page: Page): Locator => page.locator('.canvas-host > .canvas .layer-nodes > .node');

export const notice = (page: Page): Locator => page.locator('[data-test="notice"]');

export const tabState = (page: Page): Locator => page.locator('[data-test="tab-state"]');

export const topDeploy = (page: Page): Locator => page.locator('.topbar button', { hasText: 'Deploy' });

/** Where the middle of an element is on the page. */
export async function centre(loc: Locator): Promise<{ x: number; y: number }> {
  const b = await loc.boundingBox();
  if (!b) throw new Error('not on screen');
  return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
}

/** Drags from one point to another with a real pointer. */
export async function drag(page: Page, from: { x: number; y: number }, to: { x: number; y: number }): Promise<void> {
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move((from.x + to.x) / 2, (from.y + to.y) / 2, { steps: 5 });
  await page.mouse.move(to.x, to.y, { steps: 5 });
  await page.mouse.up();
}

/** Wires an output port to a node with a drag, as a person would. */
export async function wire(page: Page, fromNode: string, toNode: string, port = 0): Promise<void> {
  const out = canvasNode(page, fromNode).locator('.port-out').nth(port);
  await drag(page, await centre(out), await centre(nodeBody(page, toNode)));
}

/** Selects nodes by clicking, shift-clicking for the rest. */
export async function select(page: Page, ...ids: string[]): Promise<void> {
  for (const [i, id] of ids.entries()) {
    await nodeBody(page, id).click({ modifiers: i === 0 ? [] : ['Shift'] });
  }
  for (const id of ids) await expect(canvasNode(page, id)).toHaveClass(/selected/);
}

/** Focuses the canvas without clicking a node, so keyboard shortcuts land on it. */
export async function focusCanvas(page: Page): Promise<void> {
  await page.locator('.canvas-host > .canvas').focus();
}

/** Opens a palette item by double-click, which drops the node and opens its dialog. */
export async function dropFromPalette(page: Page, type: string): Promise<void> {
  await page.locator(`.palette-item[data-palette-type="${type}"]`).dblclick();
}

export const dialog = (page: Page): Locator => page.locator('.dialog');

/** A flow of three nodes: an inject, a change that labels, and a debug. */
export function threeNodes(label = 'one'): Entry[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3' },
    {
      id: 'i1', type: 'inject', z: 't1', name: 'reading',
      props: [{ p: 'payload', v: '50', vt: 'num' }], x: 120, y: 120, wires: [['c1']],
    },
    {
      id: 'c1', type: 'change', z: 't1', name: 'label',
      rules: [{ t: 'set', p: 'payload', pt: 'msg', to: label, tot: 'str' }],
      x: 340, y: 120, wires: [['d1']],
    },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 560, y: 120, wires: [] },
  ];
}
