// Configuration nodes, made from the palette. A node's dialog says "Add one from
// the palette first" when it needs a broker and there is none, so the palette has
// to be able to add one. What the screen shows is checked against the file the
// runtime holds after a deploy, because the runtime tells a configuration node
// from a node by what it does not have: no x, no y, no wires.

import { expect, test } from '@playwright/test';
import { canvasNode, deployed, dialog, dropFromPalette, focusCanvas, notice, seed, select, signIn, tabState, threeNodes, topDeploy } from './helpers';

const rows = (page: import('@playwright/test').Page) => page.locator('[data-test="config-node"]');
/** Whatever is on the canvas besides the three nodes the flow was seeded with. */
const added = (page: import('@playwright/test').Page) =>
  page.locator('.canvas-host > .canvas .layer-nodes > .node:not([data-node-id="i1"],[data-node-id="c1"],[data-node-id="d1"])');
const field = (page: import('@playwright/test').Page, label: string) =>
  dialog(page).locator('.field', { has: page.locator('label', { hasText: new RegExp('^' + label + '(?![a-z])') }) });

test('the palette adds a configuration node, a node can use it, and one in use is not deleted from under it', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);
  await expect(rows(page)).toHaveCount(0);

  // A node that needs a broker, with none to pick, tells you where to get one.
  await dropFromPalette(page, 'mqtt in');
  await expect(dialog(page)).toContainText('Add one from the palette first');
  await dialog(page).locator('button', { hasText: 'Cancel' }).click();
  await expect(dialog(page)).toHaveCount(0);
  await select(page, (await added(page).getAttribute('data-node-id'))!);
  await focusCanvas(page);
  await page.keyboard.press('Delete');
  await expect(added(page)).toHaveCount(0);

  // The broker type is in the palette, under configuration, and is added with a dialog.
  const type = page.locator('.palette-item[data-palette-type="mqtt-broker"]');
  await expect(type).toHaveAttribute('title', /Double-click to add one/);
  await expect(page.locator('.palette-cat', { hasText: /^configuration$/ })).toBeVisible();
  await type.dblclick();
  await field(page, 'Name').locator('input').fill('Line 3 broker');
  await field(page, 'Server').locator('input').fill('127.0.0.1');
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(rows(page)).toHaveCount(1);
  await expect(rows(page).first()).toContainText('Line 3 broker');
  await expect(notice(page)).toContainText('Added Line 3 broker');
  await expect(tabState(page)).toHaveText('unsaved changes');

  // Cancelling the dialog adds nothing.
  await type.dblclick();
  await dialog(page).locator('button', { hasText: 'Cancel' }).click();
  await expect(rows(page)).toHaveCount(1);

  // Now a node that needs one can pick it, and the hint is gone.
  await dropFromPalette(page, 'mqtt in');
  await expect(dialog(page)).not.toContainText('Add one from the palette first');
  await expect(field(page, 'Server').locator('select option')).toHaveText([/none/, 'Line 3 broker']);
  await field(page, 'Server').locator('select').selectOption({ label: 'Line 3 broker' });
  await field(page, 'Topic').locator('input').fill('line3/#');
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(dialog(page)).toHaveCount(0);

  // It is in use, so it cannot be deleted, and it says by how many.
  await rows(page).first().locator('[data-test="config-delete"]').click();
  await expect(notice(page)).toContainText('1 node uses it');
  await expect(rows(page)).toHaveCount(1);

  // Editing it from the list keeps it a configuration node.
  await rows(page).first().dblclick();
  await field(page, 'Name').locator('input').fill('Plant broker');
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(rows(page).first()).toContainText('Plant broker');

  // Take the node that uses it away, and it can go.
  const userId = (await added(page).getAttribute('data-node-id'))!;
  await select(page, userId);
  await focusCanvas(page);
  await page.keyboard.press('Delete');
  await expect(canvasNode(page, userId)).toHaveCount(0);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');

  const flows = await deployed(request);
  const broker = flows.find((e) => e.type === 'mqtt-broker')!;
  expect(broker.name).toBe('Plant broker');
  expect(broker.broker).toBe('127.0.0.1');
  // What makes it a configuration node to the runtime.
  expect('x' in broker).toBe(false);
  expect('y' in broker).toBe(false);
  expect('wires' in broker).toBe(false);
  expect(flows.some((e) => e.type === 'mqtt in')).toBe(false);

  await rows(page).first().locator('[data-test="config-delete"]').click();
  await expect(rows(page)).toHaveCount(0);
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  expect((await deployed(request)).some((e) => e.type === 'mqtt-broker')).toBe(false);
});
