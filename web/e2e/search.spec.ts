// Search across every flow and subflow, and the debug sidebar's filters, in a
// real browser. The search is driven the way a person would: Ctrl+F, type, pick.
// The debug filters are driven with real messages through the running flows.

import { expect, test } from '@playwright/test';
import { allNodes, canvasNode, dialog, press, seed, signIn, type Entry } from './helpers';

function plant(): Entry[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3' },
    { id: 'i1', type: 'inject', z: 't1', name: 'reading', props: [{ p: 'payload', v: '50', vt: 'num' }], x: 120, y: 120, wires: [['c1']] },
    { id: 'c1', type: 'change', z: 't1', name: 'label', rules: [{ t: 'set', p: 'payload', pt: 'msg', to: 'ALPHA', tot: 'str' }], x: 340, y: 120, wires: [['d1']] },
    { id: 'd1', type: 'debug', z: 't1', name: 'alpha out', complete: 'payload', x: 560, y: 120, wires: [] },

    { id: 't2', type: 'tab', label: 'Line 4' },
    { id: 'i2', type: 'inject', z: 't2', name: 'second reading', props: [{ p: 'payload', v: '60', vt: 'num' }], x: 120, y: 120, wires: [['f1']] },
    { id: 'f1', type: 'function', z: 't2', name: 'scaler', func: 'msg.payload = "BETA " + msg.payload;\nreturn msg;', outputs: 1, x: 340, y: 120, wires: [['d2']] },
    { id: 'd2', type: 'debug', z: 't2', name: 'beta out', complete: 'payload', x: 560, y: 120, wires: [] },
    { id: 'h1', type: 'http request', z: 't2', name: 'fetch plc', method: 'GET', url: 'http://plc.example.invalid/api/zebra', tls: 'b1', x: 340, y: 240, wires: [[]] },

    { id: 'b1', type: 'tls-config', name: 'plant tls' },

    { id: 'sf1', type: 'subflow', name: 'Scale', in: [{ x: 60, y: 60, wires: [{ id: 'n1' }] }], out: [{ x: 500, y: 60, wires: [{ id: 'n1', port: 0 }] }], env: [] },
    { id: 'n1', type: 'change', z: 'sf1', name: 'inside scale', rules: [], x: 280, y: 60, wires: [[]] },
  ];
}

const panel = (page: import('@playwright/test').Page) => page.locator('[data-test="search-panel"]');
const input = (page: import('@playwright/test').Page) => page.locator('[data-test="search-input"]');
const hits = (page: import('@playwright/test').Page) => page.locator('[data-test="search-hit"]');

test.describe('search', () => {
  test.beforeEach(async ({ request }) => { await seed(request, plant()); });

  test('Ctrl+F finds a node by what is inside it, and the hit takes you there', async ({ page }) => {
    await signIn(page);
    await expect(panel(page)).toBeHidden();
    await page.locator('.canvas-host > .canvas').focus();
    await page.keyboard.press('Control+F');
    await expect(panel(page)).toBeVisible();
    await expect(input(page)).toBeFocused();

    // A line of a function body on another flow, found without a case match.
    await input(page).fill('payload = "beta');
    await expect(hits(page)).toHaveCount(1);
    await expect(hits(page).first()).toContainText('scaler');
    await expect(hits(page).first()).toContainText('function · Line 4');
    await expect(hits(page).first()).toContainText('func:');
    await expect(page.locator('[data-test="search-count"]')).toHaveText('1 found');

    await hits(page).first().click();
    await expect(page.locator('.tab.active')).toHaveText('Line 4');
    await expect(canvasNode(page, 'f1')).toHaveClass(/selected/);
    await expect(allNodes(page)).toHaveCount(4);
  });

  test('search matches names, types, settings and ids, and Enter takes the first hit', async ({ page }) => {
    await signIn(page);
    await page.keyboard.press('Control+F');

    await input(page).fill('READING');
    await expect(hits(page)).toHaveCount(2);

    await input(page).fill('debug');
    await expect(hits(page)).toHaveCount(2);

    // A setting that is not on the node's face.
    await input(page).fill('example.invalid');
    await expect(hits(page)).toHaveCount(1);
    await expect(hits(page).first()).toContainText('url:');

    await input(page).fill('d2');
    await expect(hits(page).first()).toContainText('beta out');
    await input(page).press('Enter');
    await expect(page.locator('.tab.active')).toHaveText('Line 4');
    await expect(canvasNode(page, 'd2')).toHaveClass(/selected/);

    await input(page).fill('nothing could match this');
    await expect(hits(page)).toHaveCount(0);
    await expect(page.locator('[data-test="search-count"]')).toHaveText('Nothing found.');

    await input(page).press('Escape');
    await expect(panel(page)).toBeHidden();
  });

  test('a hit inside a subflow opens the subflow, a flow hit opens the flow, and a configuration node opens its dialog', async ({ page }) => {
    await signIn(page);
    await page.locator('[data-test="search-open"]').click();

    await input(page).fill('inside scale');
    await hits(page).first().click();
    await expect(page.locator('.tab-subflow.active')).toHaveText('Scale');
    await expect(canvasNode(page, 'n1')).toHaveClass(/selected/);

    await input(page).fill('Line 4');
    await expect(hits(page).first()).toContainText('flow ·');
    await hits(page).first().click();
    await expect(page.locator('.tab.active')).toHaveText('Line 4');
    await expect(page.locator('.canvas-host > .canvas .node.selected')).toHaveCount(0);

    await input(page).fill('plant tls');
    await expect(hits(page).first()).toContainText('configuration');
    await hits(page).first().click();
    await expect(dialog(page)).toBeVisible();
    await expect(dialog(page).locator('input[type="text"]').first()).toHaveValue('plant tls');
    await dialog(page).locator('button', { hasText: 'Cancel' }).click();
    await expect(dialog(page)).toHaveCount(0);
  });

  test('search is live: it follows the graph as it changes', async ({ page }) => {
    await signIn(page);
    await canvasNode(page, 'c1').locator('.node-body').click();
    await page.keyboard.press('Control+F');
    await input(page).fill('label');
    await expect(hits(page)).toHaveCount(1);

    // Delete the node with the search still open.
    await page.locator('.canvas-host > .canvas').focus();
    await page.keyboard.press('Delete');
    await expect(hits(page)).toHaveCount(0);
  });
});

test.describe('debug filtering', () => {
  const log = (page: import('@playwright/test').Page) => page.locator('[data-test="debug-log"] .log-line');
  const count = (page: import('@playwright/test').Page) => page.locator('[data-test="debug-count"]');

  test.beforeEach(async ({ request }) => { await seed(request, plant()); });

  test('text, topic, flow and selection each narrow the log, and widening brings the lines back', async ({ page, request }) => {
    await signIn(page);
    await press(request, 'i1');
    await press(request, 'i2');
    await expect(log(page)).toHaveCount(2);
    await expect(count(page)).toHaveText('2');
    await expect(log(page).filter({ hasText: 'ALPHA' })).toHaveCount(1);
    await expect(log(page).filter({ hasText: 'BETA 60' })).toHaveCount(1);

    // Text, case blind, applied to what is already in the log.
    await page.locator('[data-test="debug-filter"]').fill('beta');
    await expect(log(page)).toHaveCount(1);
    await expect(log(page).first()).toContainText('BETA 60');
    await expect(count(page)).toHaveText('1 of 2');
    await page.locator('[data-test="debug-filter"]').fill('');
    await expect(log(page)).toHaveCount(2);

    // By flow: Line 3 is showing, so only its debug node.
    await page.locator('[data-test="debug-scope"]').selectOption('flow');
    await expect(log(page)).toHaveCount(1);
    await expect(log(page).first()).toContainText('ALPHA');
    // Changing tab changes what the log shows, with no new message.
    await page.locator('.tab', { hasText: 'Line 4' }).click();
    await expect(log(page)).toHaveCount(1);
    await expect(log(page).first()).toContainText('BETA 60');

    // By selection.
    await page.locator('[data-test="debug-scope"]').selectOption('selected');
    await expect(log(page)).toHaveCount(0);
    await expect(page.locator('[data-test="debug-log"] .empty')).toContainText('2 messages are hidden by the filter');
    await canvasNode(page, 'd2').locator('.node-body').click();
    await expect(log(page)).toHaveCount(1);
    await expect(log(page).first()).toContainText('BETA 60');

    // A new message from a node that is filtered out does not appear.
    await press(request, 'i1');
    await expect(count(page)).toHaveText('1 of 3');
    await expect(log(page)).toHaveCount(1);

    // By topic.
    await page.locator('[data-test="debug-scope"]').selectOption('all');
    await expect(log(page)).toHaveCount(3);
    await page.locator('[data-test="debug-topic-debug"]').uncheck();
    await expect(log(page)).toHaveCount(0);
    await page.locator('[data-test="debug-topic-debug"]').check();
    await expect(log(page)).toHaveCount(3);

    // Clear empties it, and says so.
    await page.getByRole('button', { name: 'Clear' }).click();
    await expect(log(page)).toHaveCount(0);
    await expect(page.locator('[data-test="debug-log"] .empty')).toHaveText('Cleared.');
  });

  test('errors have their own switch', async ({ page, request }) => {
    // A function node that throws, so the log gets an error line as well as debug lines.
    await seed(request, [
      ...plant().filter((e) => e.id !== 'f1'),
      { id: 'f1', type: 'function', z: 't2', name: 'scaler', func: 'throw new Error("boom e2e");', outputs: 1, x: 340, y: 120, wires: [['d2']] },
    ]);
    await signIn(page);
    await press(request, 'i1');
    await press(request, 'i2');
    const log = page.locator('[data-test="debug-log"] .log-line');
    await expect(log.filter({ hasText: 'boom e2e' })).toHaveCount(1);
    await expect(log.filter({ hasText: 'ALPHA' })).toHaveCount(1);

    await page.locator('[data-test="debug-topic-error"]').uncheck();
    await expect(log.filter({ hasText: 'boom e2e' })).toHaveCount(0);
    await expect(log.filter({ hasText: 'ALPHA' })).toHaveCount(1);

    await page.locator('[data-test="debug-topic-error"]').check();
    await page.locator('[data-test="debug-topic-debug"]').uncheck();
    await expect(log.filter({ hasText: 'boom e2e' })).toHaveCount(1);
    await expect(log.filter({ hasText: 'ALPHA' })).toHaveCount(0);
  });
});
