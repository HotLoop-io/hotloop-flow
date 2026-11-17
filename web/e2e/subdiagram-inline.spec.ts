// B1: a sub-diagram opens in place. Its nodes and wires are drawn inside a frame
// where the node was, instead of being on a tab (or a window) of their own, and they
// are the same nodes: move one, wire two, edit one, and the body's own tab changes.
// Everything here is the binary that ships, driven in a real browser.

import { mkdirSync } from 'node:fs';
import path from 'node:path';
import { expect, test, type Locator, type Page } from '@playwright/test';
import { box, canvasNode, deployed, drag, centre, nodeBody, seed, select, signIn, topDeploy, type Entry } from './helpers';

const shots = process.env.FLOWLAB_SHOTS;
async function shot(page: Page, name: string): Promise<void> {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  await page.screenshot({ path: path.join(shots, `${name}.png`) });
}

const n = (z: string, id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
  ({ id, type, z, x, y, wires, ew_in: ins, ...extra });

// 5 goes in, the body adds ten, 15 comes out.
function addTen(): Entry[] {
  return [
    { id: 'main', type: 'tab', label: 'Inline', ew_diagram: {} },
    { id: 'body', type: 'tab', label: 'Add ten', ew_diagram: { role: 'body' } },
    n('main', 'src', 'dia-const', 100, 140, { value: '5' }, [['sub']], [['x']]),
    n('main', 'sub', 'dia-sub', 300, 140, { name: 'Add ten', body: 'body', ins: 'x', outs: 'y' }, [['res']], [['value']]),
    n('main', 'res', 'dia-indicator', 1150, 140, { label: 'Result', kind: 'number', style: 'big', digits: 0 }),
    n('body', 'bi', 'dia-body-in', 60, 60, { name: 'x' }, [['m']], [['a']]),
    n('body', 'ten', 'dia-const', 60, 150, { value: '10' }, [['m']], [['b']]),
    n('body', 'm', 'dia-math', 260, 100, { op: 'add' }, [['bo']], [['value']]),
    n('body', 'bo', 'dia-body-out', 460, 100, { name: 'y' }),
  ];
}

// The diagram bar floats over the top of the canvas and wraps as the window narrows, so these run on a window it fits.
test.use({ viewport: { width: 1600, height: 900 } });

const canvas = (page: Page): Locator => page.locator('.canvas-host > .canvas');
const frameOf = (page: Page, id: string): Locator => canvas(page).locator(`[data-frame="${id}"]`);
const openButton = (page: Page): Locator => page.locator('[data-test="subdiagram-expand"]');

async function openSub(page: Page): Promise<void> {
  await select(page, 'sub');
  await openButton(page).click();
  await expect(frameOf(page, 'sub')).toHaveCount(1);
}

test('a sub-diagram opens where it is and closes again, with no second window', async ({ page, request }) => {
  await seed(request, addTen());
  await signIn(page, 'sub');
  await expect(frameOf(page, 'sub')).toHaveCount(0);
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);

  // Closed, the node is a node, with a small box that opens it. Its body is not on this canvas.
  await expect(canvasNode(page, 'sub').locator('[data-expand="sub"]')).toHaveCount(1);
  await expect(canvasNode(page, 'm')).toHaveCount(0);
  await expect(openButton(page)).toBeDisabled();
  await shot(page, 'editor-desktop-light-closed');

  await openSub(page);
  await expect(openButton(page)).toHaveText('Close in place');

  // Open: the body's nodes are drawn, inside the frame, on this tab and in this window.
  for (const id of ['bi', 'ten', 'm', 'bo']) await expect(canvasNode(page, id)).toBeVisible();
  expect(page.context().pages()).toHaveLength(1);
  await expect(page.locator('[data-test="tab-flow"].active')).toHaveText('Inline');
  const f = await box(frameOf(page, 'sub').locator('.frame-border'));
  for (const id of ['bi', 'ten', 'm', 'bo']) {
    const b = await box(nodeBody(page, id));
    expect(b.x, `${id} inside the frame, left`).toBeGreaterThanOrEqual(f.x);
    expect(b.y, `${id} inside the frame, top`).toBeGreaterThanOrEqual(f.y);
    expect(b.x + b.width, `${id} inside the frame, right`).toBeLessThanOrEqual(f.x + f.width);
    expect(b.y + b.height, `${id} inside the frame, bottom`).toBeLessThanOrEqual(f.y + f.height);
  }
  // The body's own wires are drawn, and the connector is named on the frame.
  await expect(canvas(page).locator('path.wire[data-from="bi"][data-to="m"]')).toHaveCount(1);
  await expect(canvas(page).locator('path.wire[data-from="m"][data-to="bo"]')).toHaveCount(1);
  await expect(frameOf(page, 'sub').locator('.port-name')).toContainText(['x', 'after', 'y', 'err', 'done']);
  // The wires that lead to the sub-diagram end on its frame, not where the node was.
  const into = await box(canvas(page).locator('path.wire[data-from="src"][data-to="sub"]'));
  expect(Math.abs(into.x + into.width - f.x)).toBeLessThan(8);
  const outOf = await box(canvas(page).locator('path.wire[data-from="sub"][data-to="res"]'));
  expect(Math.abs(outOf.x - (f.x + f.width))).toBeLessThan(8);
  await shot(page, 'editor-desktop-light-open');

  // Close it from the frame, and the node is back as it was.
  await frameOf(page, 'sub').locator('.frame-toggle').click();
  await expect(frameOf(page, 'sub')).toHaveCount(0);
  await expect(canvasNode(page, 'sub')).toBeVisible();
  await expect(canvasNode(page, 'm')).toHaveCount(0);

  // And open it from the box on the node, without selecting it first.
  await canvasNode(page, 'sub').locator('[data-expand="sub"]').click();
  await expect(frameOf(page, 'sub')).toHaveCount(1);
  await expect(canvasNode(page, 'sub').locator('.node-body')).toHaveCount(0);
});

test('what is edited inside an open sub-diagram is edited in its body, and it still runs', async ({ page, request }) => {
  await seed(request, addTen());
  await signIn(page, 'sub');
  await openSub(page);

  // Move a node inside: it moves by the distance dragged, and undo puts it back.
  const before = (await deployed(request)).find((x) => x.id === 'm')!;
  const x0 = (await box(nodeBody(page, 'm'))).x;
  const from = await centre(nodeBody(page, 'm'));
  await drag(page, from, { x: from.x + 60, y: from.y + 40 });
  await expect.poll(async () => Math.round((await box(nodeBody(page, 'm'))).x - x0)).toBeGreaterThan(40);
  await page.keyboard.press('Control+z');
  await expect.poll(async () => Math.abs(Math.round((await box(nodeBody(page, 'm'))).x - x0))).toBeLessThan(2);

  // Edit a node inside: double-click it, change its value, and the diagram adds 20.
  await nodeBody(page, 'ten').dblclick();
  const dlg = page.locator('.dialog');
  await dlg.locator('.field', { hasText: 'Value' }).locator('input, textarea').first().fill('20');
  await dlg.locator('button', { hasText: 'Done' }).click();
  await expect(dlg).toHaveCount(0);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('add twenty inside the open sub-diagram');
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  const saved = await deployed(request);
  expect(saved.find((x) => x.id === 'ten')!.value).toBe('20');
  // The node stayed where the file puts it: an open sub-diagram moves nothing in the file.
  expect(saved.find((x) => x.id === 'm')!.x).toBe(before.x);
  expect(saved.find((x) => x.id === 'm')!.z).toBe('body');

  await page.locator('[data-test="diagram-run"]').click();
  await expect(page.locator('[data-test="diagram-state"]')).toHaveAttribute('data-state', 'done');
  await expect(canvas(page).locator('path.wire[data-from="sub"][data-to="res"] title')).toHaveText('25');
});

test('a wire drawn between a node and a node inside an open sub-diagram is refused in words', async ({ page, request }) => {
  await seed(request, addTen());
  await signIn(page, 'sub');
  await openSub(page);
  const out = canvasNode(page, 'src').locator('.port-out').first();
  await drag(page, await centre(out), await centre(nodeBody(page, 'm')));
  await expect(page.locator('[data-test="notice"]')).toContainText('A wire joins two nodes of the same diagram');
  const wires = (await deployed(request)).find((x) => x.id === 'src')!.wires;
  expect(wires).toEqual([['sub']]);
});

test('sub-diagrams open inside sub-diagrams, and a sub-diagram that holds itself does not loop', async ({ page, request }) => {
  const flows: Entry[] = [
    { id: 'main', type: 'tab', label: 'Nested', ew_diagram: {} },
    { id: 'mid', type: 'tab', label: 'Middle', ew_diagram: { role: 'body' } },
    { id: 'leaf', type: 'tab', label: 'Leaf', ew_diagram: { role: 'body' } },
    n('main', 'outer', 'dia-sub', 120, 120, { name: 'Outer', body: 'mid', ins: 'x', outs: 'y' }, [[]], [[]]),
    n('mid', 'mi', 'dia-body-in', 60, 60, { name: 'x' }, [['inner']], [['x']]),
    n('mid', 'inner', 'dia-sub', 240, 60, { name: 'Inner', body: 'leaf', ins: 'x', outs: 'y' }, [['mo']], [['value']]),
    n('mid', 'mo', 'dia-body-out', 440, 60, { name: 'y' }),
    // A sub-diagram of the leaf that names the middle again: a loop of bodies.
    n('leaf', 'li', 'dia-body-in', 60, 60, { name: 'x' }, [['again']], [['x']]),
    n('leaf', 'again', 'dia-sub', 240, 60, { name: 'Again', body: 'mid', ins: 'x', outs: 'y' }, [['lo']], [['value']]),
    n('leaf', 'lo', 'dia-body-out', 440, 60, { name: 'y' }),
  ];
  await seed(request, flows);
  await signIn(page, 'outer');
  await select(page, 'outer');
  await openButton(page).click();
  await expect(frameOf(page, 'outer')).toHaveCount(1);
  await expect(canvasNode(page, 'mi')).toBeVisible();
  // The middle holds a sub-diagram of its own, with its own box to open it.
  await canvasNode(page, 'inner').locator('[data-expand="inner"]').click();
  await expect(frameOf(page, 'inner')).toHaveCount(1);
  await expect(frameOf(page, 'inner')).toHaveAttribute('data-depth', '1');
  await expect(canvasNode(page, 'li')).toBeVisible();
  // Its body names the middle, which is already open on this canvas: no third frame, and no hang.
  await expect(frameOf(page, 'again')).toHaveCount(0);
  await expect(canvasNode(page, 'again')).toBeVisible();
  // Closing the outer one closes what is drawn inside it, and opening it again brings back the inner one.
  await frameOf(page, 'outer').locator('.frame-toggle').click();
  await expect(canvas(page).locator('[data-frame]')).toHaveCount(0);
  await canvasNode(page, 'outer').locator('[data-expand="outer"]').click();
  await expect(canvas(page).locator('[data-frame]')).toHaveCount(2);
  await shot(page, 'editor-desktop-light-nested');
});

test('an open sub-diagram can be selected, moved and zoomed as one piece', async ({ page, request }) => {
  await seed(request, addTen());
  await signIn(page, 'sub');
  await openSub(page);
  const zoom = async () => Number(await canvas(page).getAttribute('data-zoom'));
  // Drag the frame by its title bar: the node moves, and everything inside goes with it.
  // Two presses on one thing within 400 ms are a double-click, which opens its dialog; let that pass.
  await page.waitForTimeout(450);
  const head = await box(frameOf(page, 'sub').locator('.frame-head'));
  const m0 = await box(nodeBody(page, 'm'));
  await drag(page, { x: head.x + 40, y: head.y + head.height / 2 }, { x: head.x + 40 + 50, y: head.y + head.height / 2 + 30 });
  await expect.poll(async () => Math.round((await box(nodeBody(page, 'm'))).x - m0.x)).toBeGreaterThanOrEqual(40);
  const moved = (await deployed(request)).find((x) => x.id === 'm')!;
  // Nothing is written until deploy, and the body's nodes keep their own coordinates when it is.
  expect(moved.x).toBe(260);

  // Zoom out to 25 percent and in to 400 with the frame open: the frame is still there and still one piece.
  const c = await box(canvas(page));
  await page.mouse.move(c.x + c.width / 2, c.y + c.height / 2);
  for (let i = 0; i < 14 && (await zoom()) > 25; i++) await page.mouse.wheel(0, 200);
  await expect.poll(zoom).toBeLessThanOrEqual(25);
  await expect(frameOf(page, 'sub')).toHaveCount(1);
  for (let i = 0; i < 40 && (await zoom()) < 400; i++) await page.mouse.wheel(0, -200);
  await expect.poll(zoom).toBe(400);
  await expect(frameOf(page, 'sub')).toHaveCount(1);
});

test('a marquee drawn inside an open sub-diagram selects its nodes, and delete and undo work on them', async ({ page, request }) => {
  await seed(request, addTen());
  await signIn(page, 'sub');
  await openSub(page);
  await page.waitForTimeout(450);
  const bi = await box(nodeBody(page, 'bi'));
  const ten = await box(nodeBody(page, 'ten'));
  const m = await box(nodeBody(page, 'm'));
  // From empty frame to empty frame: the inside of a frame is not a hit, so this starts a marquee.
  await drag(page, { x: bi.x - 14, y: bi.y - 10 }, { x: m.x + m.width + 4, y: ten.y + ten.height + 6 });
  for (const id of ['bi', 'ten', 'm']) await expect(canvasNode(page, id)).toHaveClass(/selected/);
  await expect(canvasNode(page, 'bo')).not.toHaveClass(/selected/);
  await expect(frameOf(page, 'sub')).not.toHaveClass(/selected/);
  await page.keyboard.press('Delete');
  for (const id of ['bi', 'ten', 'm']) await expect(canvasNode(page, id)).toHaveCount(0);
  await expect(canvasNode(page, 'bo')).toHaveCount(1);
  await page.keyboard.press('Control+z');
  for (const id of ['bi', 'ten', 'm']) await expect(canvasNode(page, id)).toHaveCount(1);
});

// The same thing in the dark, and on a phone with a finger: the frame is drawn in the
// theme, the body is readable, the page does not scroll sideways, and the bar's button
// (a finger-sized target) opens it.
for (const scheme of ['light', 'dark'] as const) {
  test(`an open sub-diagram on a desktop in the ${scheme} theme`, async ({ page, request }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await seed(request, addTen());
    await signIn(page, 'sub');
    await openSub(page);
    await expect(canvasNode(page, 'm')).toBeVisible();
    await shot(page, `editor-desktop-${scheme}-open`);
  });

  test.describe(`on a phone, ${scheme}`, () => {
    test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });
    test('a sub-diagram opens in place on a phone', async ({ page, request }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await seed(request, addTen());
      await signIn(page, 'sub');
      // A finger on the node, then on the bar's button: the bar takes its own room at this
      // width, so nothing is under it.
      await nodeBody(page, 'sub').tap();
      await expect(canvasNode(page, 'sub')).toHaveClass(/selected/);
      await expect(openButton(page)).toBeEnabled();
      expect((await box(openButton(page))).height, 'the bar button').toBeGreaterThanOrEqual(24);
      await openButton(page).tap();
      await expect(frameOf(page, 'sub')).toHaveCount(1);
      await expect(canvasNode(page, 'm')).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth), 'the page scrolls sideways').toBeLessThanOrEqual(0);
      await shot(page, `editor-phone-${scheme}-open`);
      await openButton(page).tap();
      await expect(frameOf(page, 'sub')).toHaveCount(0);
    });
  });
}
