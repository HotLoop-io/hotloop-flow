// Groups, made and edited in the browser: grouping a selection, naming and
// colouring it, moving it as one, dragging nodes in and out, nesting, and
// taking it apart. What the screen shows is checked against what the runtime
// has after a deploy, because a group is a list in the file that has to agree
// with the boxes on the canvas.

import { expect, test } from '@playwright/test';
import {
  allNodes, canvasNode, centre, deployed, dialog, drag, focusCanvas, nodeBody, notice, seed, select, signIn, tabState,
  threeNodes, topDeploy, type Entry,
} from './helpers';

const groups = (page: import('@playwright/test').Page) => page.locator('.canvas-host > .canvas .group');
const head = (page: import('@playwright/test').Page, n = 0) => page.locator('.canvas-host > .canvas .group-head').nth(n);

/** Three nodes in a row and a fourth underneath that belongs to nothing. */
function withLoose(): Entry[] {
  return [
    ...threeNodes(),
    { id: 'c2', type: 'change', z: 't1', name: 'loose', rules: [], x: 340, y: 300, wires: [[]] },
  ];
}

async function deploy(page: import('@playwright/test').Page): Promise<void> {
  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
}

test('a selection is grouped, named and coloured, and the file agrees with the box', async ({ page, request }) => {
  await seed(request, withLoose());
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(1);
  // The new group is what is selected, not its nodes.
  await expect(groups(page).first()).toHaveClass(/selected/);

  // The box encloses what is in it.
  const box = (await groups(page).locator('.group-box').boundingBox())!;
  for (const id of ['c1', 'd1']) {
    const n = (await nodeBody(page, id).boundingBox())!;
    expect(n.x).toBeGreaterThanOrEqual(box.x);
    expect(n.y).toBeGreaterThanOrEqual(box.y);
    expect(n.x + n.width).toBeLessThanOrEqual(box.x + box.width);
    expect(n.y + n.height).toBeLessThanOrEqual(box.y + box.height);
  }
  const outside = (await nodeBody(page, 'c2').boundingBox())!;
  expect(outside.y).toBeGreaterThan(box.y + box.height);

  // Name and colour it from its title bar.
  await head(page).dblclick();
  await page.locator('[data-test="group-name"]').fill('Labeller');
  await page.locator('[data-test="group-fill"]').fill('nope');
  await page.locator('[data-test="group-done"]').click();
  await expect(page.locator('[data-test="group-error"]')).toContainText('hex colour');
  await page.locator('[data-test="group-fill"]').fill('#3366cc');
  await page.locator('[data-test="group-stroke"]').fill('#112233');
  await page.locator('[data-test="group-done"]').click();
  await expect(dialog(page)).toHaveCount(0);
  await expect(page.locator('.canvas-host > .canvas .group-label')).toHaveText('Labeller');
  await expect(groups(page).locator('.group-box')).toHaveCSS('fill', 'rgb(51, 102, 204)');

  await deploy(page);
  const flows = await deployed(request);
  const g = flows.find((e) => e.type === 'group')!;
  expect(g.name).toBe('Labeller');
  expect(g.z).toBe('t1');
  expect([...(g.nodes as string[])].sort()).toEqual(['c1', 'd1']);
  expect(g.style).toEqual({ label: true, fill: '#3366cc', stroke: '#112233' });
  // c1 is at x 340 and d1 at 560: a node is 130 wide, the group leaves 16 around it.
  expect(g.x).toBe(340 - 65 - 16);
  expect(g.w).toBe(560 + 65 - (340 - 65) + 32);
  expect(flows.find((e) => e.id === 'c1')!.g).toBe(g.id);
  expect(flows.find((e) => e.id === 'd1')!.g).toBe(g.id);
  expect(flows.find((e) => e.id === 'i1')!.g).toBeUndefined();
  expect(flows.find((e) => e.id === 'c2')!.g).toBeUndefined();
});

test('moving a group moves everything in it, and nothing else', async ({ page, request }) => {
  await seed(request, withLoose());
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(1);
  const at = await centre(head(page));
  await drag(page, at, { x: at.x, y: at.y + 90 });
  await deploy(page);

  const flows = await deployed(request);
  const byId = (id: string) => flows.find((e) => e.id === id)!;
  const moved = (byId('c1').y as number) - 120;
  expect(moved).toBeGreaterThan(40);
  expect((byId('d1').y as number) - 120).toBe(moved);
  expect(byId('i1').y).toBe(120);
  expect(byId('c2').y).toBe(300);
  const g = flows.find((e) => e.type === 'group')!;
  expect(g.y).toBe((byId('c1').y as number) - 17 - 16 - 22);
});

test('nodes dragged into a group join it and dragged out leave it', async ({ page, request }) => {
  await seed(request, withLoose());
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(1);

  // The loose node goes up into the group.
  const loose = await centre(nodeBody(page, 'c2'));
  const target = await centre(nodeBody(page, 'c1'));
  await drag(page, loose, { x: target.x, y: target.y + 6 });
  await deploy(page);
  let flows = await deployed(request);
  const group = flows.find((e) => e.type === 'group')!;
  expect(flows.find((e) => e.id === 'c2')!.g).toBe(group.id);
  expect(group.nodes as string[]).toContain('c2');

  // And back out.
  const inside = await centre(nodeBody(page, 'c2'));
  await drag(page, inside, { x: inside.x, y: inside.y + 150 });
  await deploy(page);
  flows = await deployed(request);
  expect(flows.find((e) => e.id === 'c2')!.g).toBeUndefined();
  expect(flows.find((e) => e.type === 'group')!.nodes as string[]).not.toContain('c2');
});

test('ungroup takes the box away and keeps the nodes, and undo brings the group back', async ({ page, request }) => {
  await seed(request, withLoose());
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(1);

  await page.keyboard.press('Control+Shift+G');
  await expect(groups(page)).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(4);
  // The nodes that were in it are what is selected now.
  await expect(page.locator('.canvas-host > .canvas .node.selected')).toHaveCount(2);

  await page.keyboard.press('Control+Z');
  await expect(groups(page)).toHaveCount(1);
  // Ungroup works on a selected group, so pick it by its title bar.
  await head(page).click();
  await page.keyboard.press('Control+Shift+G');
  await expect(groups(page)).toHaveCount(0);
  await deploy(page);
  const flows = await deployed(request);
  expect(flows.some((e) => e.type === 'group')).toBe(false);
  expect(flows.find((e) => e.id === 'c1')!.g).toBeUndefined();
});

test('a group cannot be built across two others, and says why', async ({ page, request }) => {
  await seed(request, withLoose());
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(1);

  // One in the group and one outside it.
  await select(page, 'c1', 'c2');
  await page.keyboard.press('Control+G');
  await expect(notice(page)).toHaveText('Everything selected has to be in the same group, or in none.');
  await expect(groups(page)).toHaveCount(1);

  await focusCanvas(page);
  await page.keyboard.press('Control+Shift+G');
  await expect(notice(page)).toHaveText('Select a group to ungroup.');
});

test('groups nest, and deleting a group deletes what is in it', async ({ page, request }) => {
  await seed(request, withLoose());
  await signIn(page);
  await select(page, 'c1', 'd1');
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(1);

  // The group and the inject, grouped again.
  await head(page).click();
  await nodeBody(page, 'i1').click({ modifiers: ['Shift'] });
  await page.keyboard.press('Control+G');
  await expect(groups(page)).toHaveCount(2);

  await deploy(page);
  const flows = await deployed(request);
  const all = flows.filter((e) => e.type === 'group');
  const inner = all.find((g) => (g.nodes as string[]).includes('c1'))!;
  const outer = all.find((g) => g.id !== inner.id)!;
  expect(inner.g).toBe(outer.id);
  expect([...(outer.nodes as string[])].sort()).toEqual([inner.id, 'i1'].sort());
  expect(flows.find((e) => e.id === 'i1')!.g).toBe(outer.id);
  expect(outer.w as number).toBeGreaterThan(inner.w as number);

  // Select the outer group by its border and delete it: all of it goes.
  await page.locator('button[title="Fit to view"]').click();
  await head(page, 0).click();
  await page.keyboard.press('Delete');
  await expect(groups(page)).toHaveCount(0);
  await expect(allNodes(page)).toHaveCount(1);
  await expect(canvasNode(page, 'c2')).toBeVisible();
  await deploy(page);
  const left = (await deployed(request)).filter((e) => e.type !== 'tab').map((e) => e.id);
  expect(left).toEqual(['c2']);
});

test('a group that came from another tool is drawn, labelled and coloured', async ({ page, request }) => {
  await seed(request, [
    ...threeNodes().map((e) => (e.id === 'c1' || e.id === 'd1' ? { ...e, g: 'g1' } : e)),
    { id: 'g1', type: 'group', z: 't1', name: 'from elsewhere', style: { label: true, fill: '#ffbfbf', stroke: '#999999' }, nodes: ['c1', 'd1'], x: 260, y: 60, w: 400, h: 120 },
  ]);
  await signIn(page);
  await expect(groups(page)).toHaveCount(1);
  await expect(page.locator('.canvas-host > .canvas .group-label')).toHaveText('from elsewhere');
  await expect(groups(page).locator('.group-box')).toHaveCSS('fill', 'rgb(255, 191, 191)');
  // Nothing was touched by looking at it.
  await expect(tabState(page)).toHaveText('saved');
});
