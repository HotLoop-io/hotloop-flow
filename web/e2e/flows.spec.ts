// Renaming and deleting a flow tab. The tab bar could add a flow and nothing
// else, so a mistyped name stayed and a flow nobody wanted could only be removed
// by editing the file. What the screen does is checked against the flows the
// runtime holds after a deploy.

import { expect, test } from '@playwright/test';
import { allNodes, canvasNode, deployed, dialog, focusCanvas, seed, signIn, tabState, threeNodes, topDeploy, type Entry } from './helpers';

const tabs = (page: import('@playwright/test').Page) => page.locator('[data-test="tab-flow"]');

/** Two flows. The inject on the first is also wired to a debug on the second, which the file allows. */
function twoFlows(): Entry[] {
  const first = threeNodes();
  first.find((e) => e.id === 'i1')!.wires = [['c1', 'd2']];
  return [
    ...first,
    { id: 't2', type: 'tab', label: 'Packaging' },
    { id: 'i2', type: 'inject', z: 't2', name: 'case', props: [{ p: 'payload', v: '1', vt: 'num' }], x: 120, y: 120, wires: [['d2']] },
    { id: 'd2', type: 'debug', z: 't2', name: 'packed', complete: 'payload', x: 340, y: 120, wires: [] },
  ];
}

async function deploy(page: import('@playwright/test').Page): Promise<void> {
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
}

test('a flow is renamed from its tab, and a blank name is refused', async ({ page, request }) => {
  await seed(request, twoFlows());
  await signIn(page);
  await expect(tabs(page)).toHaveText(['Line 3', 'Packaging']);

  await tabs(page).first().dblclick();
  await expect(dialog(page)).toContainText('Flow');
  await expect(page.locator('[data-test="flow-name"]')).toHaveValue('Line 3');
  await page.locator('[data-test="flow-name"]').fill('   ');
  await page.locator('[data-test="flow-done"]').click();
  await expect(page.locator('[data-test="flow-error"]')).toContainText('needs a name');
  await page.locator('[data-test="flow-name"]').fill('Line three');
  await page.locator('[data-test="flow-done"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(tabs(page)).toHaveText(['Line three', 'Packaging']);
  await expect(tabState(page)).toHaveText('unsaved changes');

  // The same dialog from the button, on the flow being looked at.
  await tabs(page).nth(1).click();
  await page.locator('[data-test="flow-properties"]').click();
  await expect(page.locator('[data-test="flow-name"]')).toHaveValue('Packaging');
  await page.locator('[data-test="flow-cancel"]').click();
  await expect(tabs(page)).toHaveText(['Line three', 'Packaging']);

  await deploy(page);
  const flows = await deployed(request);
  expect(flows.find((e) => e.id === 't1')!.label).toBe('Line three');
  expect(flows.find((e) => e.id === 't2')!.label).toBe('Packaging');
  // Nothing else about the flow moved.
  expect(flows.filter((e) => e.z === 't1')).toHaveLength(3);
});

test('a flow is deleted with what is on it, after it asks, and undo brings it back', async ({ page, request }) => {
  await seed(request, twoFlows());
  await signIn(page);

  await tabs(page).nth(1).click();
  await expect(allNodes(page)).toHaveCount(2);
  await page.locator('[data-test="flow-properties"]').click();
  await page.locator('[data-test="flow-delete"]').click();
  await expect(page.locator('[data-test="flow-confirm-text"]')).toContainText('its 2 nodes');

  // Asked, and kept.
  await page.locator('[data-test="flow-delete-cancel"]').click();
  await expect(page.locator('[data-test="flow-delete-confirm"]')).toBeHidden();
  await page.locator('[data-test="flow-cancel"]').click();
  await expect(tabs(page)).toHaveCount(2);

  await page.locator('[data-test="flow-properties"]').click();
  await page.locator('[data-test="flow-delete"]').click();
  await page.locator('[data-test="flow-delete-confirm"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(tabs(page)).toHaveText(['Line 3']);
  // Back on the flow that is left, drawn.
  await expect(tabs(page).first()).toHaveClass(/active/);
  await expect(canvasNode(page, 'i1')).toBeVisible();
  await expect(tabState(page)).toHaveText('unsaved changes');

  // Undo puts it all back.
  await focusCanvas(page);
  await page.keyboard.press('Control+Z');
  await expect(tabs(page)).toHaveText(['Line 3', 'Packaging']);
  await tabs(page).nth(1).click();
  await expect(allNodes(page)).toHaveCount(2);

  // Delete it for good.
  await page.locator('[data-test="flow-properties"]').click();
  await page.locator('[data-test="flow-delete"]').click();
  await page.locator('[data-test="flow-delete-confirm"]').click();
  await deploy(page);

  const flows = await deployed(request);
  expect(flows.some((e) => e.id === 't2')).toBe(false);
  expect(flows.some((e) => e.z === 't2')).toBe(false);
  expect(flows.some((e) => e.id === 'd2' || e.id === 'i2')).toBe(false);
  // The wire from the other flow to a node that is gone went with it.
  expect(flows.find((e) => e.id === 'i1')!.wires).toEqual([['c1']]);
  expect(flows.filter((e) => e.z === 't1')).toHaveLength(3);
});

test('the only flow cannot be deleted', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);
  await page.locator('[data-test="flow-properties"]').click();
  await page.locator('[data-test="flow-delete"]').click();
  await expect(page.locator('[data-test="flow-error"]')).toContainText('only flow');
  await expect(page.locator('[data-test="flow-delete-confirm"]')).toBeHidden();
  await page.locator('[data-test="flow-cancel"]').click();
  await expect(tabs(page)).toHaveCount(1);
  await expect(allNodes(page)).toHaveCount(3);
});

/** Catch, Status and Complete nodes on the first flow, watching nodes on both. */
function watchers(): Entry[] {
  const at = (id: string, type: string, y: number, scope: string[]): Entry => ({ id, type, z: 't1', name: id, scope, x: 120, y, wires: [[]] });
  return [
    ...twoFlows(),
    at('k1', 'catch', 300, ['c1', 'd2']),
    at('s1', 'status', 360, ['d2', 'c1']),
    at('p1', 'complete', 420, ['d2']),
    at('k2', 'catch', 480, ['d2']),
    at('s2', 'status', 540, ['d2']),
  ];
}

test('deleting a flow takes its nodes out of the Catch, Status and Complete scopes that named them', async ({ page, request }) => {
  await seed(request, watchers());
  await signIn(page);
  await tabs(page).nth(1).click();
  await page.locator('[data-test="flow-properties"]').click();
  await page.locator('[data-test="flow-delete"]').click();
  await page.locator('[data-test="flow-delete-confirm"]').click();
  await deploy(page);

  const flows = await deployed(request);
  const scope = (id: string) => flows.find((e) => e.id === id)!.scope;
  expect(scope('k1')).toEqual(['c1']);
  expect(scope('s1')).toEqual(['c1']);
  // Complete watches nothing when its list is empty, so emptying it changes nothing.
  expect(scope('p1')).toEqual([]);
  // An empty list on a Catch or a Status is the whole flow, which is not what a
  // node that only ever watched one thing meant. These keep their last id and
  // stay as quiet as they were.
  expect(scope('k2')).toEqual(['d2']);
  expect(scope('s2')).toEqual(['d2']);
});

test('deleting a node takes it out of the scopes that named it', async ({ page, request }) => {
  await seed(request, watchers());
  await signIn(page);
  await canvasNode(page, 'c1').locator('.node-body').click();
  await focusCanvas(page);
  await page.keyboard.press('Delete');
  await deploy(page);

  const flows = await deployed(request);
  const scope = (id: string) => flows.find((e) => e.id === id)!.scope;
  expect(scope('k1')).toEqual(['d2']);
  expect(scope('s1')).toEqual(['d2']);
  expect(scope('p1')).toEqual(['d2']);
});
