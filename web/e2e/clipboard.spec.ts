// Copy, cut, paste, import and export, in a real browser against the real
// binary. The clipboard is the browser's own: Ctrl+C and Ctrl+V go through the
// same events a person's keystrokes do. What lands on the canvas is checked
// again against what the runtime has after a deploy.

import { expect, test } from '@playwright/test';
import {
  allNodes, canvasNode, deployed, dialog, focusCanvas, notice, seed, select, signIn, tabState, threeNodes,
  topDeploy, type Entry,
} from './helpers';
import { readFileSync } from 'node:fs';

test.beforeEach(async ({ request }) => {
  await seed(request, threeNodes());
});

test('copy and paste makes new nodes with new ids, keeps the wire between them, and leaves the originals alone', async ({ page, request }) => {
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+C');
  await expect(notice(page)).toHaveText('Copied 2 nodes.');
  // Copying changes nothing.
  await expect(tabState(page)).toHaveText('saved');

  await page.keyboard.press('Control+V');
  await expect(notice(page)).toContainText('Added 2 nodes.');
  await expect(allNodes(page)).toHaveCount(5);
  await expect(tabState(page)).toHaveText('unsaved changes');

  // The paste is selected, and the originals are not.
  await expect(canvasNode(page, 'c1')).not.toHaveClass(/selected/);
  await expect(page.locator('.canvas-host > .canvas .node.selected')).toHaveCount(2);

  // A second paste is a second copy, not the first one painted over.
  await page.keyboard.press('Control+V');
  await expect(allNodes(page)).toHaveCount(7);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('pasted twice');
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');

  const flows = await deployed(request);
  const nodes = flows.filter((e) => e.type !== 'tab');
  expect(nodes).toHaveLength(7);
  expect(new Set(nodes.map((e) => e.id)).size).toBe(7);

  const changes = nodes.filter((e) => e.type === 'change');
  const debugs = nodes.filter((e) => e.type === 'debug');
  expect(changes).toHaveLength(3);
  const original = changes.find((e) => e.id === 'c1')!;
  expect(original.wires).toEqual([['d1']]);
  for (const copy of changes.filter((e) => e.id !== 'c1')) {
    // Each copy is wired to its own copy of the debug node, never to the original's.
    const target = copy.wires![0]![0]!;
    expect(target).not.toBe('d1');
    expect(debugs.map((d) => d.id)).toContain(target);
    expect(copy.z).toBe('t1');
    expect(copy.rules).toEqual(original.rules);
  }
  // The inject was not copied, so nothing is wired into a copy.
  expect(nodes.find((e) => e.id === 'i1')!.wires).toEqual([['c1']]);
});

test('a wire to something that was not copied is not carried over', async ({ page, request }) => {
  await signIn(page);
  await select(page, 'i1', 'c1');
  await page.keyboard.press('Control+C');
  await page.keyboard.press('Control+V');
  await expect(allNodes(page)).toHaveCount(5);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');

  const nodes = (await deployed(request)).filter((e) => e.type !== 'tab');
  const copiedChange = nodes.find((e) => e.type === 'change' && e.id !== 'c1')!;
  // The original change node goes on to the debug node. The copy's wire to it was cut.
  expect(copiedChange.wires).toEqual([[]]);
  const copiedInject = nodes.find((e) => e.type === 'inject' && e.id !== 'i1')!;
  expect(copiedInject.wires).toEqual([[copiedChange.id]]);
});

test('cut removes the nodes, paste brings them back, and undo undoes the paste', async ({ page }) => {
  await signIn(page);
  await select(page, 'd1');
  await page.keyboard.press('Control+X');
  await expect(notice(page)).toHaveText('Cut 1 node.');
  await expect(canvasNode(page, 'd1')).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(2);

  await page.keyboard.press('Control+V');
  await expect(allNodes(page)).toHaveCount(3);
  await expect(page.locator('.canvas-host > .canvas .node.selected .node-label')).toHaveText('out');

  await page.keyboard.press('Control+Z');
  await expect(allNodes(page)).toHaveCount(2);
});

test('copy with nothing selected does nothing, and typing in a field is left alone', async ({ page }) => {
  await signIn(page);
  await focusCanvas(page);
  await page.keyboard.press('Control+C');
  await expect(notice(page)).toBeHidden();

  // Pasting into the palette filter is a paste into a text box, not into the flow.
  await select(page, 'c1');
  await page.keyboard.press('Control+C');
  await expect(notice(page)).toHaveText('Copied 1 node.');
  await page.locator('.palette input[type="search"]').click();
  await page.keyboard.press('Control+V');
  await expect(allNodes(page)).toHaveCount(3);
  await expect(page.locator('.palette input[type="search"]')).toHaveValue(/"type": "change"/);
});

test('export shows a selection, a flow, or everything, and never a credential', async ({ page, request }) => {
  await seed(request, [
    ...threeNodes(),
    { id: 'b1', type: 'tls-config', name: 'plant tls', credentials: { passphrase: 'secret-e2e-value' } },
    { id: 't2', type: 'tab', label: 'Line 4' },
    { id: 'd2', type: 'debug', z: 't2', name: 'other', complete: 'payload', x: 200, y: 100, wires: [] },
  ]);
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.locator('[data-test="export-open"]').click();

  const text = page.locator('[data-test="export-text"]');
  const parse = async () => JSON.parse(await text.inputValue()) as Entry[];

  let entries = await parse();
  expect(entries.map((e) => e.id).sort()).toEqual(['c1', 'd1']);
  // Cut at the edge: the wire into c1 came from a node that is not in the export.
  expect(entries.find((e) => e.id === 'c1')!.wires).toEqual([['d1']]);
  await expect(page.locator('[data-test="export-info"]')).toContainText('2 entries');

  await page.locator('[data-test="export-flow"]').check();
  entries = await parse();
  expect(entries.map((e) => e.id).sort()).toEqual(['c1', 'd1', 'i1', 't1']);

  await page.locator('[data-test="export-all"]').check();
  entries = await parse();
  expect(entries.map((e) => e.id)).toEqual(expect.arrayContaining(['t1', 't2', 'd2', 'b1']));
  const raw = await text.inputValue();
  expect(raw).not.toContain('secret-e2e-value');
  expect(raw).not.toContain('"credentials"');
  await expect(page.locator('[data-test="export-info"]')).toContainText('credentials are never exported');

  // The same text is what Download saves.
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.locator('[data-test="export-download"]').click(),
  ]);
  expect(download.suggestedFilename()).toBe('hotloop-flow-all.json');
  expect(readFileSync(await download.path()!, 'utf8')).toBe(raw);

  await page.locator('[data-test="export-close"]').click();
  await expect(dialog(page)).toHaveCount(0);
});

test('export of a selection takes the configuration node it uses along', async ({ page, request }) => {
  await seed(request, [
    { id: 't1', type: 'tab', label: 'Line 3' },
    { id: 'b1', type: 'tls-config', name: 'plant tls' },
    { id: 'm1', type: 'http request', z: 't1', name: 'press', method: 'GET', url: 'http://127.0.0.1:9/', tls: 'b1', x: 160, y: 100, wires: [['d1']] },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 400, y: 100, wires: [] },
  ]);
  await signIn(page);
  await select(page, 'm1');
  await page.locator('[data-test="export-open"]').click();
  const entries = JSON.parse(await page.locator('[data-test="export-text"]').inputValue()) as Entry[];
  expect(entries.map((e) => e.id).sort()).toEqual(['b1', 'm1']);
});

test('import takes pasted JSON, gives everything new ids, and says what it added', async ({ page, request }) => {
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.locator('[data-test="export-open"]').click();
  const exported = await page.locator('[data-test="export-text"]').inputValue();
  await page.locator('[data-test="export-close"]').click();

  await page.locator('[data-test="import-open"]').click();
  await page.locator('[data-test="import-text"]').fill(exported);
  await page.locator('[data-test="import-go"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(notice(page)).toHaveText('Added 2 nodes.');
  await expect(allNodes(page)).toHaveCount(5);

  // Importing the same text again gives two more, never a clash.
  await page.locator('[data-test="import-open"]').click();
  await page.locator('[data-test="import-text"]').fill(exported);
  await page.locator('[data-test="import-go"]').click();
  await expect(allNodes(page)).toHaveCount(7);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  const nodes = (await deployed(request)).filter((e) => e.type !== 'tab');
  expect(nodes).toHaveLength(7);
  expect(new Set(nodes.map((e) => e.id)).size).toBe(7);
});

test('import refuses text that is not flows, and the dialog stays open with the reason', async ({ page }) => {
  await signIn(page);
  await page.locator('[data-test="import-open"]').click();
  const text = page.locator('[data-test="import-text"]');
  const err = page.locator('[data-test="import-error"]');
  const go = page.locator('[data-test="import-go"]');

  await go.click();
  await expect(err).toHaveText('There is nothing to import.');

  await text.fill('hello there');
  await go.click();
  await expect(err).toContainText('That is not JSON');

  await text.fill('[{"id":"a","type":"debug"},{"id":"a","type":"debug"}]');
  await go.click();
  await expect(err).toHaveText('The id a appears twice.');

  await text.fill('[{"type":"debug"}]');
  await go.click();
  await expect(err).toHaveText('Entry 1 has no id.');

  await text.fill('[{"id":"a","type":"debug","wires":["b"]}]');
  await go.click();
  await expect(err).toContainText('wires that are not a list of lists');

  await expect(dialog(page)).toHaveCount(1);
  await page.locator('[data-test="import-cancel"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(3);
  await expect(tabState(page)).toHaveText('saved');
});

test('a flow file chosen from disk comes in as its own flow, and a node type this runtime lacks shows grey', async ({ page, request }) => {
  await signIn(page);
  const file = JSON.stringify([
    { id: 'ft', type: 'tab', label: 'From another box' },
    { id: 'fa', type: 'inject', z: 'ft', name: 'go', props: [{ p: 'payload', v: '1', vt: 'num' }], x: 100, y: 80, wires: [['fb']] },
    { id: 'fb', type: 'node-red-contrib-nothing-like-this', z: 'ft', name: 'mystery', x: 320, y: 80, wires: [['fc']] },
    { id: 'fc', type: 'debug', z: 'ft', name: 'show', complete: 'payload', x: 540, y: 80, wires: [] },
  ]);
  await page.locator('[data-test="import-open"]').click();
  await page.locator('[data-test="import-file"]').setInputFiles({ name: 'flows.json', mimeType: 'application/json', buffer: Buffer.from(file) });
  await expect(page.locator('[data-test="import-text"]')).toHaveValue(file);
  await page.locator('[data-test="import-go"]').click();

  await expect(notice(page)).toContainText('Added 3 nodes, 1 flow.');
  await expect(notice(page)).toContainText('node-red-contrib-nothing-like-this');
  // It opened on the new flow, with the unknown type drawn as a grey, dashed box.
  await expect(page.locator('.tab.active')).toHaveText('From another box');
  await expect(allNodes(page)).toHaveCount(3);
  await expect(page.locator('.canvas-host > .canvas .node.unknown')).toHaveCount(1);
  await expect(page.locator('.canvas-host > .canvas .wire')).toHaveCount(2);

  // Deploying a flow with a node the runtime does not have is the runtime's call,
  // and it keeps the entry exactly as it was.
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  const flows = await deployed(request);
  expect(flows.find((e) => e.type === 'node-red-contrib-nothing-like-this')?.name).toBe('mystery');
  expect(flows.find((e) => e.type === 'tab' && e.label === 'From another box')).toBeTruthy();
});

test('importing onto a new flow puts loose nodes on a flow of their own', async ({ page }) => {
  await signIn(page);
  await select(page, 'c1');
  await page.keyboard.press('Control+C');
  await page.locator('[data-test="export-open"]').click();
  const text = await page.locator('[data-test="export-text"]').inputValue();
  await page.locator('[data-test="export-close"]').click();

  await page.locator('[data-test="import-open"]').click();
  await page.locator('[data-test="import-text"]').fill(text);
  await page.locator('[data-test="import-target"]').selectOption('new');
  await page.locator('[data-test="import-go"]').click();

  await expect(page.locator('.tab.active')).toHaveText('Imported flow');
  await expect(allNodes(page)).toHaveCount(1);
  await page.locator('.tab', { hasText: 'Line 3' }).click();
  await expect(allNodes(page)).toHaveCount(3);
});
