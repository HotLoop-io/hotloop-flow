// Turning a selection into a subflow. The nodes move into a template, an
// instance takes their place, and the wires that crossed the edge of the
// selection become the template's input and outputs, so the flow does what it
// did. Checked against what the runtime holds after a deploy, and by running it.

import { expect, test } from '@playwright/test';
import { allNodes, deployed, dialog, press, seed, select, signIn, tabState, topDeploy, type Entry } from './helpers';

const toSubflow = (page: import('@playwright/test').Page) => page.locator('[data-test="selection-to-subflow"]');
const NOT_SEEDED = '.canvas-host > .canvas .layer-nodes > .node:not([data-node-id^="@"],[data-node-id="i1"],[data-node-id="i2"],[data-node-id="d1"],[data-node-id="d2"],[data-node-id="c1"],[data-node-id="c2"])';

const set = (id: string, to: string, x: number, wires: string[][], extra: Record<string, unknown> = {}): Entry => ({
  id, type: 'change', z: 't1', name: id, rules: [{ t: 'set', p: 'payload', pt: 'msg', to, tot: 'str' }], x, y: 120, wires, ...extra,
});

/** i1 feeds c1; c1 feeds c2 and d1; c2 feeds d2. Select c1 and c2 and the edge of the selection has one way in and two ways out. */
function chain(extra: Record<string, unknown> = {}): Entry[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3' },
    { id: 'i1', type: 'inject', z: 't1', name: 'reading', props: [{ p: 'payload', v: '50', vt: 'num' }], x: 120, y: 120, wires: [['c1']] },
    set('c1', 'from-c1', 340, [['c2', 'd1']], extra),
    set('c2', 'from-c2', 560, [['d2']], extra),
    { id: 'd1', type: 'debug', z: 't1', name: 'out1', complete: 'payload', x: 560, y: 260, wires: [] },
    { id: 'd2', type: 'debug', z: 't1', name: 'out2', complete: 'payload', x: 780, y: 120, wires: [] },
  ];
}

async function deploy(page: import('@playwright/test').Page): Promise<void> {
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
}

test('a selection becomes a subflow with its wires as ports, and the flow still does what it did', async ({ page, request }) => {
  await seed(request, chain());
  await signIn(page);
  await expect(toSubflow(page)).toBeDisabled();

  await select(page, 'c1', 'c2');
  await expect(toSubflow(page)).toBeEnabled();
  await toSubflow(page).click();
  await page.locator('[data-test="selection-subflow-input"]').fill('Labeller');
  await page.locator('[data-test="selection-subflow-ok"]').click();
  await expect(dialog(page)).toHaveCount(0);

  // On the flow: an instance where the two nodes were, wired in and out.
  await expect(allNodes(page)).toHaveCount(4);
  const instance = page.locator(NOT_SEEDED);
  await expect(instance).toHaveCount(1);
  const instanceId = (await instance.getAttribute('data-node-id'))!;
  await expect(instance.locator('.port-out')).toHaveCount(2);
  await expect(page.locator('.canvas-host > .canvas .wire')).toHaveCount(3);
  await expect(page.locator('[data-test="notice"]')).toContainText('Made the subflow Labeller');
  await expect(page.locator('.tab-subflow')).toHaveText('Labeller');

  // The template holds the two nodes, with a port in and one for each way out.
  await page.locator('.tab-subflow').click();
  await expect(page.locator('.canvas-host > .canvas [data-node-id^="@in:"]')).toHaveCount(1);
  await expect(page.locator('.canvas-host > .canvas [data-node-id^="@out:"]')).toHaveCount(2);
  await expect(allNodes(page)).toHaveCount(5);
  await expect(page.locator('.canvas-host > .canvas .wire')).toHaveCount(4);

  await deploy(page);
  const flows = await deployed(request);
  const tpl = flows.find((e) => e.type === 'subflow')!;
  expect(tpl.name).toBe('Labeller');
  expect(tpl.in).toEqual([expect.objectContaining({ wires: [{ id: 'c1' }] })]);
  expect(tpl.out).toEqual([
    expect.objectContaining({ wires: [{ id: 'c1', port: 0 }] }),
    expect.objectContaining({ wires: [{ id: 'c2', port: 0 }] }),
  ]);
  expect(flows.find((e) => e.id === 'c1')).toEqual(expect.objectContaining({ z: tpl.id, wires: [['c2']] }));
  expect(flows.find((e) => e.id === 'c2')).toEqual(expect.objectContaining({ z: tpl.id, wires: [[]] }));
  const inst = flows.find((e) => e.id === instanceId)!;
  expect(inst).toEqual(expect.objectContaining({ type: `subflow:${tpl.id}`, z: 't1', wires: [['d1'], ['d2']] }));
  expect(flows.find((e) => e.id === 'i1')!.wires).toEqual([[instanceId]]);

  // It runs: each debug gets what the node that used to feed it set.
  await page.locator('[data-test="tab-flow"]').first().click();
  await press(request, 'i1');
  await expect(page.locator('.log-line', { hasText: 'from-c1' }).last()).toBeVisible();
  await expect(page.locator('.log-line', { hasText: 'from-c2' }).last()).toBeVisible();

  // And undo puts the nodes back on the flow, the way they were.
  await page.locator('.canvas-host > .canvas').focus();
  await page.keyboard.press('Control+Z');
  await expect(page.locator('.tab-subflow')).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(5);
});

test('a selection with more than one way in is refused, and nothing changes', async ({ page, request }) => {
  await seed(request, [
    { id: 't1', type: 'tab', label: 'Line 3' },
    { id: 'i1', type: 'inject', z: 't1', name: 'one', props: [{ p: 'payload', v: '1', vt: 'num' }], x: 120, y: 120, wires: [['c1']] },
    { id: 'i2', type: 'inject', z: 't1', name: 'two', props: [{ p: 'payload', v: '2', vt: 'num' }], x: 120, y: 260, wires: [['c2']] },
    set('c1', 'a', 340, [[]]),
    { ...set('c2', 'b', 340, [[]]), y: 260 },
  ]);
  await signIn(page, 'i1');
  await select(page, 'c1', 'c2');
  await toSubflow(page).click();
  await page.locator('[data-test="selection-subflow-ok"]').click();
  await expect(page.locator('[data-test="notice"]')).toContainText('one input');
  await expect(page.locator('.tab-subflow')).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(4);
  await expect(tabState(page)).toHaveText('saved');
});

test('a group in the selection goes into the subflow with its nodes', async ({ page, request }) => {
  const flows = chain();
  for (const id of ['c1', 'c2']) flows.find((e) => e.id === id)!.g = 'g1';
  flows.push({
    id: 'g1', type: 'group', z: 't1', name: 'pair', style: { label: true }, nodes: ['c1', 'c2'],
    x: 259, y: 65, w: 422, h: 88,
  });
  await seed(request, flows);
  await signIn(page);
  await page.locator('.canvas-host > .canvas .group-head').click();
  await expect(page.locator('.canvas-host > .canvas .group')).toHaveClass(/selected/);
  await toSubflow(page).click();
  await page.locator('[data-test="selection-subflow-input"]').fill('Pair');
  await page.locator('[data-test="selection-subflow-ok"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(page.locator('.canvas-host > .canvas .group')).toHaveCount(0);
  await deploy(page);

  const got = await deployed(request);
  const tpl = got.find((e) => e.type === 'subflow')!;
  const group = got.find((e) => e.type === 'group')!;
  expect(group.z).toBe(tpl.id);
  expect([...(group.nodes as string[])].sort()).toEqual(['c1', 'c2']);
  expect(got.find((e) => e.id === 'c1')).toEqual(expect.objectContaining({ z: tpl.id, g: 'g1' }));
  expect(got.find((e) => e.id === 'c2')).toEqual(expect.objectContaining({ z: tpl.id, g: 'g1' }));
});

/**
 * i1 feeds c1, which reads GREETING from the flow's environment, and m1, which
 * publishes through a broker that belongs to the flow and a tls-config that
 * belongs to the broker. c1 feeds m1 and d1.
 */
function withConfig(shared: boolean): Entry[] {
  return [
    { id: 't1', type: 'tab', label: 'Line 3', env: [{ name: 'GREETING', type: 'str', value: 'hello' }, { name: 'SITE', type: 'str', value: 'line3' }, { name: 'UNUSED', type: 'str', value: 'x' }] },
    { id: 'tl1', type: 'tls-config', z: 't1', name: 'plant ca' },
    { id: 'b1', type: 'mqtt-broker', z: 't1', name: 'local', broker: '127.0.0.1', port: 1883, tls: 'tl1' },
    { id: 'i1', type: 'inject', z: 't1', name: 'reading', props: [{ p: 'payload', v: '50', vt: 'num' }], x: 120, y: 120, wires: [shared ? ['c1', 'm2'] : ['c1']] },
    {
      id: 'c1', type: 'change', z: 't1', name: 'greet',
      rules: [{ t: 'set', p: 'payload', pt: 'msg', to: 'GREETING', tot: 'env' }, { t: 'set', p: 'topic', pt: 'msg', to: '$(SITE)-out', tot: 'str' }],
      x: 340, y: 120, wires: [['m1', 'd1']],
    },
    { id: 'm1', type: 'mqtt out', z: 't1', name: 'publish', broker: 'b1', topic: 'line3/out', x: 560, y: 120, wires: [] },
    { id: 'd1', type: 'debug', z: 't1', name: 'out', complete: 'payload', x: 560, y: 260, wires: [] },
    ...(shared ? [{ id: 'm2', type: 'mqtt out', z: 't1', name: 'also', broker: 'b1', topic: 'line3/also', x: 340, y: 380, wires: [] } as Entry] : []),
  ];
}

async function makeSubflow(page: import('@playwright/test').Page): Promise<void> {
  await select(page, 'c1', 'm1');
  await toSubflow(page).click();
  await page.locator('[data-test="selection-subflow-input"]').fill('Greeter');
  await page.locator('[data-test="selection-subflow-ok"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await deploy(page);
}

test('a configuration node only the selection uses goes into the subflow with it, and the flow environment still reaches it', async ({ page, request }) => {
  await seed(request, withConfig(false));
  await signIn(page);
  await makeSubflow(page);

  const flows = await deployed(request);
  const tpl = flows.find((e) => e.type === 'subflow')!;
  // The broker and the tls-config it names are the subflow's now, not the flow's.
  expect(flows.find((e) => e.id === 'b1')).toEqual(expect.objectContaining({ z: tpl.id }));
  expect(flows.find((e) => e.id === 'tl1')).toEqual(expect.objectContaining({ z: tpl.id }));
  // The variables the moved nodes read are properties of the subflow, forwarding to the flow. The one nothing reads is not.
  expect(tpl.env).toEqual([{ name: 'GREETING', type: 'env', value: 'GREETING' }, { name: 'SITE', type: 'env', value: 'SITE' }]);

  // And it still says hello, from the flow's value.
  await press(request, 'i1');
  await expect(page.locator('.log-line', { hasText: 'hello' }).last()).toBeVisible();
});

test('a configuration node something outside the selection also uses becomes global, so both keep sharing it', async ({ page, request }) => {
  await seed(request, withConfig(true));
  await signIn(page);
  await makeSubflow(page);

  const flows = await deployed(request);
  const b1 = flows.find((e) => e.id === 'b1')!;
  const tl1 = flows.find((e) => e.id === 'tl1')!;
  expect(b1.z).toBeUndefined();
  expect(tl1.z).toBeUndefined();
  expect(flows.find((e) => e.id === 'm2')).toEqual(expect.objectContaining({ broker: 'b1', z: 't1' }));
  expect(flows.find((e) => e.id === 'm1')).toEqual(expect.objectContaining({ broker: 'b1' }));
});
