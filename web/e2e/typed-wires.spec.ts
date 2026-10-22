// Types drawn on the wires. Two nodes declare what they send and what they take,
// the editor writes it on the wire between them, and a change that makes the two
// ends disagree is drawn as a clash and refused at deploy. All of it against the
// real binary: the clash comes from the runtime's own check (POST /flows/types),
// the same one the deploy refuses with.

import { expect, test, type Page } from '@playwright/test';
import { canvasNode, deployed, dialog, notice, seed, signIn, topDeploy, type Entry } from './helpers';

const message = (temp: string) => ({
  type: 'object',
  properties: { payload: { type: 'object', required: ['temp'], properties: { temp: { type: temp } } } },
});

/** A sensor that promises payload.temp is `sends`, wired to a limit check that takes a number. */
function flow(sends: string, extra: Entry[] = []): Entry[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3' },
    {
      id: 's1', type: 'change', z: 't1', name: 'sensor', rules: [], x: 140, y: 120, wires: [['l1']],
      ew_outSchemas: [message(sends)],
    },
    {
      id: 'l1', type: 'change', z: 't1', name: 'limit', rules: [], x: 420, y: 120, wires: [['d1']],
      ew_inSchema: message('number'),
    },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 680, y: 120, wires: [] },
    ...extra,
  ];
}

const chip = (page: Page, from: string, to: string) =>
  page.locator(`.canvas-host > .canvas [data-wire-type="${from}:0:${to}"]`);
const plainWires = (page: Page) => page.locator('.canvas-host > .canvas path.wire');

async function setSensorTemp(page: Page, type: string): Promise<void> {
  await canvasNode(page, 's1').locator('.node-body').dblclick();
  const types = dialog(page).locator('[data-test="types"]');
  await expect(types).toBeVisible();
  const out = types.locator('[data-test="schema-out-1"]');
  await out.fill(JSON.stringify(message(type)));
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(dialog(page)).toHaveCount(0);
}

test('types are written on the wires, a clash is drawn red and refused at deploy, and fixing it deploys', async ({ page, request }) => {
  await seed(request, flow('number'));
  await signIn(page, 'd1');

  // Both ends declare a type and they agree: the sender's type on the wire.
  await expect(chip(page, 's1', 'l1')).toHaveClass(/ok/);
  await expect(chip(page, 's1', 'l1')).toContainText('payload {temp: number}');
  // The wire into the debug node has no type at either end, so it has no chip.
  await expect(chip(page, 'l1', 'd1')).toHaveCount(0);
  await expect(plainWires(page)).toHaveCount(2);

  // Make the sensor promise text. The runtime says the two can never agree.
  await setSensorTemp(page, 'string');
  await expect(chip(page, 's1', 'l1')).toHaveClass(/clash/);
  await expect(chip(page, 's1', 'l1')).toContainText('≠ payload.temp');
  await expect(page.locator('path.wire[data-from="s1"][data-to="l1"]')).toHaveAttribute('data-type-state', 'clash');
  await expect(page.locator('path.wire[data-from="s1"][data-to="l1"] title')).toContainText(
    'the sender sends string and the receiver takes number');
  if (process.env.TYPED_WIRES_SHOT) await page.screenshot({ path: `${process.env.TYPED_WIRES_SHOT}-clash.png` });

  // Deploy is refused, and what is running is what was running.
  const alerted = new Promise<string>((resolve) => page.once('dialog', (d) => { resolve(d.message()); void d.dismiss(); }));
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  expect(await alerted).toContain('payload.temp: the sender sends string and the receiver takes number');
  const live = await deployed(request);
  expect(live.find((e) => e.id === 's1')!.ew_outSchemas).toEqual([message('number')]);

  // Put it back: the clash goes, the type is the sender's again, and the deploy lands.
  await setSensorTemp(page, 'number');
  await expect(chip(page, 's1', 'l1')).toHaveClass(/ok/);
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  if (process.env.TYPED_WIRES_SHOT) await page.screenshot({ path: `${process.env.TYPED_WIRES_SHOT}-ok.png` });
});

test('a wire with a type at one end only is drawn muted, and a flow without types has no chips at all', async ({ page, request }) => {
  const half = flow('number');
  delete (half[2] as Entry).ew_inSchema;
  await seed(request, half);
  await signIn(page, 'd1');
  await expect(chip(page, 's1', 'l1')).toHaveClass(/half/);

  const none = flow('number');
  delete (none[1] as Entry).ew_outSchemas;
  delete (none[2] as Entry).ew_inSchema;
  await seed(request, none);
  await page.reload();
  await expect(canvasNode(page, 'd1')).toBeVisible();
  await expect(page.locator('.canvas-host > .canvas .wire-type')).toHaveCount(0);
  await expect(plainWires(page)).toHaveCount(2);
});

test('the Types section keeps what was written, refuses what is not a schema, and adds nothing when left empty', async ({ page, request }) => {
  await seed(request, flow('number'));
  await signIn(page, 'd1');

  // Not JSON: the dialog says so and stays open.
  await canvasNode(page, 's1').locator('.node-body').dblclick();
  const out = dialog(page).locator('[data-test="schema-out-1"]');
  await expect(out).toHaveValue(/"temp"/);
  await out.fill('{nope');
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(dialog(page).locator('.dialog-error')).toContainText('not valid JSON');
  await dialog(page).locator('button', { hasText: 'Cancel' }).click();

  // An untouched node's dialog opened and saved leaves it without the keys.
  await canvasNode(page, 'd1').locator('.node-body').dblclick();
  await expect(dialog(page).locator('[data-test="types"]')).toBeVisible();
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  const live = await deployed(request);
  const d1 = live.find((e) => e.id === 'd1')!;
  expect('ew_inSchema' in d1).toBe(false);
  expect('ew_outSchemas' in d1).toBe(false);
  void notice;
});

test('an Inject literal the next node can never take is drawn as a clash and refused at deploy', async ({ page, request }) => {
  const takes = (t: string) => ({ type: 'object', properties: { payload: { type: t } } });
  await seed(request, [
    { id: 't1', type: 'tab', label: 'Line 3' },
    {
      id: 'i1', type: 'inject', z: 't1', name: 'reading',
      props: [{ p: 'payload', v: '42', vt: 'num' }], x: 140, y: 120, wires: [['l1']],
    },
    { id: 'l1', type: 'change', z: 't1', name: 'limit', rules: [], x: 420, y: 120, wires: [['d1']], ew_inSchema: takes('number') },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 680, y: 120, wires: [] },
  ]);
  await signIn(page, 'd1');
  await expect(chip(page, 'i1', 'l1')).toHaveClass(/half/);

  // The receiver now takes text. The Inject sets the number 42, which it can never take.
  await canvasNode(page, 'l1').locator('.node-body').dblclick();
  await dialog(page).locator('[data-test="schema-in"]').fill(JSON.stringify(takes('string')));
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(chip(page, 'i1', 'l1')).toHaveClass(/clash/);
  await expect(chip(page, 'i1', 'l1')).toContainText('≠ payload');
  await expect(page.locator('path.wire[data-from="i1"][data-to="l1"] title')).toContainText(
    'the inject sets 42, which the receiver does not take: got number, want string');
  if (process.env.TYPED_WIRES_SHOT) await page.screenshot({ path: `${process.env.TYPED_WIRES_SHOT}-inject-clash.png` });

  const alerted = new Promise<string>((resolve) => page.once('dialog', (d) => { resolve(d.message()); void d.dismiss(); }));
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  expect(await alerted).toContain('inject "reading" (i1) output 1 -> "limit" (l1): payload: the inject sets 42');
  expect((await deployed(request)).find((e) => e.id === 'l1')!.ew_inSchema).toEqual(takes('number'));
});
