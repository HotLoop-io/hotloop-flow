// A node pointing at a config node that is not in the flow file: a flow was
// deleted and took its flow-scoped broker with it while a subflow still pointed
// at it, or somebody hand-edited the file. The editor says so when it loads,
// and again when a deploy leaves a node not running because of it.

import { expect, test } from '@playwright/test';
import { canvasNode, centre, drag, nodeBody, seed, signIn, tabState, topDeploy, type Entry } from './helpers';

const flow: Entry[] = [
  { id: 't1', type: 'tab', label: 'Line 3' },
  {
    id: 'sf', type: 'subflow', name: 'Publish', in: [{ x: 40, y: 40, wires: [{ id: 'm1' }] }], out: [],
  },
  { id: 'm1', type: 'mqtt out', z: 'sf', name: 'to the broker', broker: 'deleted-broker', topic: 'line3/out', x: 200, y: 60, wires: [] },
  { id: 'i1', type: 'subflow:sf', z: 't1', name: 'publisher', x: 120, y: 120, wires: [] },
  { id: 'm2', type: 'mqtt in', z: 't1', name: 'from the line', broker: 'also-gone', topic: 'line3/in', x: 340, y: 120, wires: [['d1']] },
  { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 560, y: 120, wires: [] },
];

const errorLines = (page: import('@playwright/test').Page) => page.locator('.log-line[data-topic="error"]');

test('the editor says which nodes point at a config node that is not in the file, when it loads', async ({ page, request }) => {
  await seed(request, flow);
  await signIn(page);

  await expect(errorLines(page).filter({ hasText: 'mqtt out m1 (to the broker)' })).toContainText('"deleted-broker"');
  await expect(errorLines(page).filter({ hasText: 'mqtt out m1 (to the broker)' })).toContainText('no node with that id is in the file');
  await expect(errorLines(page).filter({ hasText: 'mqtt in m2 (from the line)' })).toContainText('"also-gone"');
});

test('a deploy that leaves a node not running for want of its config node says so on that node', async ({ page, request }) => {
  await seed(request, flow);
  await signIn(page);
  await errorLines(page).first().waitFor();

  // Nudge a node so there is something to deploy, then deploy it.
  const from = await centre(nodeBody(page, 'm2'));
  await drag(page, from, { x: from.x, y: from.y + 60 });
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');

  // The failure is logged against the node, in words that say what to do.
  const failure = page.locator('.log-line[data-topic="error"][data-node="m2"]', { hasText: 'did not start' });
  await expect(failure).toContainText('mqtt in:');
  await expect(failure).toContainText('"also-gone"');
  await expect(failure).toContainText('pick a mqtt-broker');
  // And the node next to it is still running: the debug node is on the canvas.
  await expect(canvasNode(page, 'd1')).toBeVisible();
});
