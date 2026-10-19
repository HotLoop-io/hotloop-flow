// Marking a wire durable from the editor, and giving it a way out of a message
// its target refuses forever. Until now both were only keys in the flow file.
// The screen is checked against the file the runtime holds, and the last test
// runs the flow: a wire that gives up hands the message to a Catch node.

import { expect, test } from '@playwright/test';
import { allNodes, canvasNode, deployed, dialog, focusCanvas, press, seed, select, signIn, tabState, threeNodes, topDeploy, type Entry } from './helpers';

const wireEl = (page: import('@playwright/test').Page, from: string, to: string) =>
  page.locator(`.canvas-host > .canvas path.wire[data-from="${from}"][data-to="${to}"]`);

async function deploy(page: import('@playwright/test').Page): Promise<void> {
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
}

/** Double-clicks the middle of a wire. The nodes sit on one row, so the middle of the line is on it. */
async function openWire(page: import('@playwright/test').Page, from: string, to: string): Promise<void> {
  const box = (await wireEl(page, from, to).boundingBox())!;
  await page.mouse.dblclick(box.x + box.width / 2, box.y + box.height / 2);
  await expect(dialog(page)).toContainText('Wire');
}

test('a wire is marked durable with a limit, drawn differently, saved in the file, and unmarked again', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);
  await expect(wireEl(page, 'i1', 'c1')).not.toHaveClass(/durable/);

  await openWire(page, 'i1', 'c1');
  await expect(page.locator('[data-test="wire-ends"]')).toContainText('reading (output 1) to label');
  // Off, there is no limit to give.
  await expect(page.locator('[data-test="wire-durable"]')).not.toBeChecked();
  await expect(page.locator('[data-test="wire-attempts"]')).toBeDisabled();
  await page.locator('[data-test="wire-durable"]').check();
  await expect(page.locator('[data-test="wire-attempts"]')).toBeEnabled();
  await page.locator('[data-test="wire-attempts"]').fill('0');
  await page.locator('[data-test="wire-done"]').click();
  await expect(page.locator('[data-test="wire-error"]')).toContainText('whole number');
  await page.locator('[data-test="wire-attempts"]').fill('4');
  await page.locator('[data-test="wire-done"]').click();
  await expect(dialog(page)).toHaveCount(0);

  await expect(wireEl(page, 'i1', 'c1')).toHaveClass(/durable/);
  await expect(wireEl(page, 'c1', 'd1')).not.toHaveClass(/durable/);
  await expect(tabState(page)).toHaveText('unsaved changes');

  await deploy(page);
  let i1 = (await deployed(request)).find((e) => e.id === 'i1')!;
  expect(i1.ew_durable).toEqual(['c1']);
  expect(i1.ew_durable_attempts).toEqual({ c1: 4 });
  // Nothing else on the node moved.
  expect(i1.wires).toEqual([['c1']]);

  // Reopened, it shows what was saved. A blank limit goes back to keep-trying.
  await openWire(page, 'i1', 'c1');
  await expect(page.locator('[data-test="wire-durable"]')).toBeChecked();
  await expect(page.locator('[data-test="wire-attempts"]')).toHaveValue('4');
  await page.locator('[data-test="wire-attempts"]').fill('');
  await page.locator('[data-test="wire-done"]').click();
  await deploy(page);
  i1 = (await deployed(request)).find((e) => e.id === 'i1')!;
  expect(i1.ew_durable).toEqual(['c1']);
  expect('ew_durable_attempts' in i1).toBe(false);

  // Unmarked, both keys go.
  await openWire(page, 'i1', 'c1');
  await page.locator('[data-test="wire-durable"]').uncheck();
  await page.locator('[data-test="wire-done"]').click();
  await expect(wireEl(page, 'i1', 'c1')).not.toHaveClass(/durable/);
  await deploy(page);
  i1 = (await deployed(request)).find((e) => e.id === 'i1')!;
  expect('ew_durable' in i1).toBe(false);
  expect('ew_durable_attempts' in i1).toBe(false);
});

test('the mark goes with the node it leads to, is copied with both ends, and is dropped when only one end is copied', async ({ page, request }) => {
  const flows = threeNodes();
  const i1 = flows.find((e) => e.id === 'i1')!;
  i1.ew_durable = ['c1'];
  i1.ew_durable_attempts = { c1: 3 };
  await seed(request, flows);
  await signIn(page);
  await expect(wireEl(page, 'i1', 'c1')).toHaveClass(/durable/);

  // Both ends copied: the copy is durable to the copy of its target, not the original.
  await select(page, 'i1', 'c1');
  await page.keyboard.press('Control+C');
  await page.keyboard.press('Control+V');
  await expect(allNodes(page)).toHaveCount(5);
  await deploy(page);
  const got = await deployed(request);
  const copiedInject = got.find((e) => e.type === 'inject' && e.id !== 'i1')!;
  const copiedChange = got.find((e) => e.type === 'change' && e.id !== 'c1')!;
  expect(copiedInject.ew_durable).toEqual([copiedChange.id]);
  expect(copiedInject.ew_durable_attempts).toEqual({ [copiedChange.id]: 3 });
  expect(got.find((e) => e.id === 'i1')!.ew_durable).toEqual(['c1']);

  // Only the inject copied: its target did not come, so neither does the mark.
  await select(page, 'i1');
  await page.keyboard.press('Control+C');
  await page.keyboard.press('Control+V');
  await deploy(page);
  const again = (await deployed(request)).filter((e) => e.type === 'inject' && e.id !== 'i1' && e.id !== copiedInject.id);
  expect(again).toHaveLength(1);
  expect('ew_durable' in again[0]!).toBe(false);
  expect('ew_durable_attempts' in again[0]!).toBe(false);

  // Delete the target, and the mark on the node that led to it goes too.
  await select(page, 'c1');
  await focusCanvas(page);
  await page.keyboard.press('Delete');
  await expect(canvasNode(page, 'c1')).toHaveCount(0);
  await deploy(page);
  const left = (await deployed(request)).find((e) => e.id === 'i1')!;
  expect('ew_durable' in left).toBe(false);
  expect('ew_durable_attempts' in left).toBe(false);
});

test('a durable wire with a limit gives up on a message its target refuses and the Catch gets it', async ({ page, request }) => {
  const flow: Entry[] = [
    { id: 't1', type: 'tab', label: 'Line 3' },
    {
      id: 'i1', type: 'inject', z: 't1', name: 'reading', props: [{ p: 'payload', v: '50', vt: 'num' }],
      x: 120, y: 120, wires: [['f1']],
    },
    { id: 'f1', type: 'function', z: 't1', name: 'refuses', func: "throw new Error('historian says no');", outputs: 1, x: 340, y: 120, wires: [[]] },
    { id: 'k1', type: 'catch', z: 't1', name: 'park', scope: ['f1'], x: 340, y: 260, wires: [['d1']] },
    { id: 'd1', type: 'debug', z: 't1', name: 'parked', complete: 'error.message', x: 560, y: 260, wires: [] },
  ];
  await seed(request, flow);
  await signIn(page, 'i1');

  // Marked from the editor, not written into the file: two tries from the queue.
  await openWire(page, 'i1', 'f1');
  await page.locator('[data-test="wire-durable"]').check();
  await page.locator('[data-test="wire-attempts"]').fill('2');
  await page.locator('[data-test="wire-done"]').click();
  await deploy(page);

  await press(request, 'i1');
  // The first refusal is in memory and goes to the Catch as any node error does.
  await expect(page.locator('.log-line', { hasText: 'historian says no' }).first()).toBeVisible();
  // Then the wire tries again from the queue, and on the second refusal gives up
  // and says so, to the same Catch, and moves on.
  await expect(page.locator('.log-line', { hasText: 'gave up on a message after 2 attempts' }).first()).toBeVisible({ timeout: 15_000 });
});
