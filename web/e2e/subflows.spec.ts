// Subflows, made and edited in the browser: a template on a canvas of its own
// with its ports wired, its properties, an instance with a value of its own,
// and the whole thing running after a deploy. Every claim on the screen is
// checked against what the runtime has.

import { expect, test } from '@playwright/test';
import {
  allNodes, canvasNode, centre, deployed, dialog, drag, dropFromPalette, notice, press, seed, signIn, tabState,
  threeNodes, topDeploy, wire, type Entry,
} from './helpers';

const port = (page: import('@playwright/test').Page, kind: 'in' | 'out') =>
  page.locator(`.canvas-host > .canvas [data-node-id^="@${kind}:"]`);

test('a subflow is made, wired, given a property, used with its own value, and runs', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);

  // A new subflow opens on a canvas of its own, with its one input and its one output.
  await page.locator('[data-test="tab-add-subflow"]').click();
  await page.locator('[data-test="new-subflow-input"]').fill('Scale');
  await page.locator('[data-test="new-subflow-ok"]').click();
  await expect(page.locator('.tab-subflow.active')).toHaveText('Scale');
  await expect(allNodes(page)).toHaveCount(2);
  await expect(port(page, 'in').locator('.node-label')).toHaveText('input');
  await expect(port(page, 'out').locator('.node-label')).toHaveText('output');

  // A change node inside it that sets the payload from a property called FACTOR.
  await dropFromPalette(page, 'change');
  const rules = dialog(page).locator('.list-row');
  await dialog(page).locator('button', { hasText: 'Add Rules' }).click();
  await expect(rules).toHaveCount(1);
  await rules.locator('.field', { has: page.locator('label', { hasText: 'Property' }) }).locator('input').fill('payload');
  const to = rules.locator('.field', { has: page.locator('label', { hasText: /^To$/ }) });
  await to.locator('select').selectOption('env');
  await to.locator('input').fill('FACTOR');
  await dialog(page).locator('button', { hasText: 'Done' }).click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(3);

  const inner = page.locator('.canvas-host > .canvas .node:not([data-node-id^="@"])');
  const innerId = (await inner.getAttribute('data-node-id'))!;

  // Input to the change node, and the change node to the output.
  await drag(page, await centre(port(page, 'in').locator('.port-out')), await centre(canvasNode(page, innerId).locator('.node-body')));
  await wire(page, innerId, (await port(page, 'out').getAttribute('data-node-id'))!);
  await expect(page.locator('.canvas-host > .canvas .wire')).toHaveCount(2);

  // The template can't be wired straight through itself.
  await drag(page, await centre(port(page, 'in').locator('.port-out')), await centre(port(page, 'out').locator('.node-body')));
  await expect(page.locator('.canvas-host > .canvas .wire')).toHaveCount(2);

  // The property the instances will set.
  await page.locator('[data-test="subflow-properties"]').click();
  await page.locator('[data-test="subflow-env-add"]').click();
  await page.locator('[data-test="subflow-env-name"]').fill('FACTOR');
  await page.locator('[data-test="subflow-env-value"]').fill('2');
  await page.locator('[data-test="subflow-done"]').click();
  await expect(dialog(page)).toHaveCount(0);

  // The palette has it, under subflows, and a flow can use it.
  await page.locator('.tab', { hasText: 'Line 3' }).first().click();
  const item = page.locator('.palette-item[data-palette-type^="subflow:"]');
  await expect(item).toHaveText('Scale');
  await item.dblclick();
  await expect(dialog(page)).toContainText('Subflow: Scale');
  await expect(page.locator('[data-test="instance-env-FACTOR"]')).toHaveValue('2');
  await page.locator('[data-test="instance-name"]').fill('scaler');
  await page.locator('[data-test="instance-env-FACTOR"]').fill('777');
  await page.locator('[data-test="instance-done"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(4);

  const instance = page.locator('.canvas-host > .canvas .node:not([data-node-id="i1"],[data-node-id="c1"],[data-node-id="d1"])');
  const instanceId = (await instance.getAttribute('data-node-id'))!;
  await expect(instance.locator('.node-label')).toHaveText('scaler');
  await expect(instance.locator('.port-out')).toHaveCount(1);
  // It lands at the top left of the view, on top of the inject. Move it clear.
  const at = await centre(instance.locator('.node-body'));
  await drag(page, at, { x: at.x + 40, y: at.y + 150 });
  await wire(page, 'i1', instanceId);
  await wire(page, instanceId, 'd1');

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('scale subflow');
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');

  // What the runtime has is a real subflow, its internals, and an instance with its own value.
  const flows = await deployed(request);
  const tpl = flows.find((e) => e.type === 'subflow')!;
  expect(tpl.name).toBe('Scale');
  expect(tpl.in).toEqual([expect.objectContaining({ wires: [{ id: innerId }] })]);
  expect(tpl.out).toEqual([expect.objectContaining({ wires: [{ id: innerId, port: 0 }] })]);
  expect(tpl.env).toEqual([{ name: 'FACTOR', type: 'str', value: '2' }]);
  expect(flows.find((e) => e.id === innerId)!.z).toBe(tpl.id);
  const inst = flows.find((e) => e.id === instanceId)!;
  expect(inst.type).toBe(`subflow:${tpl.id}`);
  expect(inst.z).toBe('t1');
  expect(inst.wires).toEqual([['d1']]);
  expect(inst.env).toEqual([{ name: 'FACTOR', type: 'str', value: '777' }]);

  // And it runs: the value this instance set comes out of the subflow.
  await press(request, 'i1');
  await expect(page.locator('.log-line', { hasText: '777' }).last()).toBeVisible();
});

test('a template follows its properties: ports, instances, and deleting is refused while one is in use', async ({ page, request }) => {
  const flows: Entry[] = [
    ...threeNodes().filter((e) => e.id !== 'c1'),
    {
      id: 'sf1', type: 'subflow', name: 'Scale', info: '', category: '', color: '#DDAA99',
      in: [{ x: 60, y: 60, wires: [{ id: 'n1' }] }],
      out: [{ x: 540, y: 60, wires: [{ id: 'n1', port: 0 }] }],
      env: [{ name: 'FACTOR', type: 'num', value: '2' }],
    },
    { id: 'n1', type: 'change', z: 'sf1', name: 'inside', rules: [], x: 280, y: 80, wires: [[]] },
    { id: 's1', type: 'subflow:sf1', z: 't1', name: 'scaler', x: 340, y: 120, env: [], wires: [['d1']] },
  ];
  flows.find((e) => e.id === 'i1')!.wires = [['s1']];
  await seed(request, flows);
  await signIn(page);

  // Double-clicking the instance opens it, and from there the template.
  await expect(canvasNode(page, 's1').locator('.port-out')).toHaveCount(1);
  await canvasNode(page, 's1').dblclick();
  await expect(dialog(page)).toContainText('Subflow: Scale');
  await page.locator('[data-test="instance-edit"]').click();
  await expect(page.locator('.tab-subflow.active')).toHaveText('Scale');
  await expect(canvasNode(page, 'n1')).toBeVisible();
  await expect(allNodes(page)).toHaveCount(3);

  // The template's ports can be moved, and Delete does not remove them.
  const inPort = port(page, 'in');
  const before = await centre(inPort.locator('.node-body'));
  await drag(page, before, { x: before.x, y: before.y + 100 });
  const after = await centre(inPort.locator('.node-body'));
  expect(after.y).toBeGreaterThan(before.y + 80);
  await inPort.locator('.node-body').click();
  await page.keyboard.press('Delete');
  await expect(allNodes(page)).toHaveCount(3);

  // A second output on the template gives every instance a second port.
  await page.locator('[data-test="subflow-properties"]').click();
  await page.locator('[data-test="subflow-outputs"]').fill('2');
  await page.locator('[data-test="subflow-name"]').fill('Scaler v2');
  await page.locator('[data-test="subflow-done"]').click();
  await expect(page.locator('.tab-subflow.active')).toHaveText('Scaler v2');
  await expect(port(page, 'out')).toHaveCount(2);

  await page.locator('.tab', { hasText: 'Line 3' }).first().click();
  await expect(canvasNode(page, 's1').locator('.port-out')).toHaveCount(2);
  await expect(page.locator('.palette-item[data-palette-type="subflow:sf1"]')).toHaveText('Scaler v2');

  // Deleting a template that is still placed is refused, with the reason.
  await page.locator('.tab-subflow').click();
  await page.locator('[data-test="subflow-properties"]').click();
  await page.locator('[data-test="subflow-delete"]').click();
  await expect(page.locator('[data-test="subflow-error"]')).toHaveText('1 instance is still in use. Delete them first.');
  await page.locator('[data-test="subflow-cancel"]').click();

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  const live = await deployed(request);
  const tpl = live.find((e) => e.id === 'sf1')!;
  expect(tpl.name).toBe('Scaler v2');
  expect((tpl.out as unknown[]).length).toBe(2);
  expect(live.find((e) => e.id === 's1')!.wires).toEqual([['d1'], []]);
  // The moved port was saved where it was put.
  expect((tpl.in as { y: number }[])[0]!.y).toBeGreaterThan(100);

  // With the instance gone it can be deleted, and takes its contents with it.
  await page.locator('.tab', { hasText: 'Line 3' }).first().click();
  await canvasNode(page, 's1').locator('.node-body').click();
  await page.keyboard.press('Delete');
  await page.locator('.tab-subflow').click();
  await page.locator('[data-test="subflow-properties"]').click();
  await page.locator('[data-test="subflow-delete"]').click();
  await expect(page.locator('.tab-subflow')).toHaveCount(0);
  await expect(page.locator('.tab.active')).toHaveText('Line 3');

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  const gone = await deployed(request);
  expect(gone.some((e) => e.id === 'sf1' || e.id === 'n1' || e.id === 's1')).toBe(false);
});

test('a subflow cannot be placed inside itself or inside one that holds it', async ({ page, request }) => {
  await seed(request, [
    ...threeNodes(),
    { id: 'sfa', type: 'subflow', name: 'Outer', in: [{ x: 60, y: 60, wires: [] }], out: [{ x: 500, y: 60, wires: [] }], env: [] },
    { id: 'sfb', type: 'subflow', name: 'Inner', in: [{ x: 60, y: 60, wires: [] }], out: [{ x: 500, y: 60, wires: [] }], env: [] },
    { id: 'ia', type: 'subflow:sfb', z: 'sfa', x: 280, y: 80, env: [], wires: [[]] },
  ]);
  await signIn(page);

  // Inside Outer, Outer itself is refused.
  await page.locator('.tab-subflow', { hasText: 'Outer' }).click();
  await expect(allNodes(page)).toHaveCount(3);
  await page.locator('.palette-item[data-palette-type="subflow:sfa"]').dblclick();
  await expect(notice(page)).toContainText('cannot be placed inside itself');
  await expect(allNodes(page)).toHaveCount(3);

  // Inside Inner, Outer is refused, because Outer already holds Inner.
  await page.locator('.tab-subflow', { hasText: 'Inner' }).click();
  await page.locator('.palette-item[data-palette-type="subflow:sfa"]').dblclick();
  await expect(notice(page)).toContainText('cannot be placed inside itself');
  await expect(allNodes(page)).toHaveCount(2);

  // On a flow, either is fine.
  await page.locator('.tab', { hasText: 'Line 3' }).first().click();
  await page.locator('.palette-item[data-palette-type="subflow:sfa"]').dblclick();
  await page.locator('[data-test="instance-done"]').click();
  await expect(allNodes(page)).toHaveCount(4);
});
