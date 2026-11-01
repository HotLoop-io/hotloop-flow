// Diagrams in the browser: a tab whose nodes run when every input is ready, with
// a bar to run and debug it and a panel page an operator opens. Every claim on the
// screen is checked against what the runtime has, and the runtime is the binary
// that ships.

import { expect, test, type Page } from '@playwright/test';
import { canvasNode, deployed, drag, nodeBody, seed, signIn, stableCentre, topDeploy, type Entry } from './helpers';

// A counter loop: a carried sum of the pass numbers 0 to 9, which stops at ten
// passes and gives 45. The main diagram shows both on indicators.
function counter(): Entry[] {
  const n = (z: string, id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}): Entry => ({ id, type, z, x, y, wires: [], ...extra });
  const wired = (e: Entry, outs: string[][], ins: string[][]): Entry => ({ ...e, wires: outs, ew_in: ins });
  return [
    { id: 'main', type: 'tab', label: 'Counter', ew_diagram: {} },
    { id: 'body', type: 'tab', label: 'Counter body', ew_diagram: { role: 'body' } },
    wired(n('main', 'loop', 'dia-while', 300, 120, { body: 'Counter body', carried: 'sum', start: '{"sum": 0}', ins: '', outs: '', maxIterations: 0 }),
      [['total'], ['passes'], [], [], []], [['value'], ['value'], [], [], []]),
    n('main', 'total', 'dia-indicator', 560, 100, { label: 'Total', kind: 'number', style: 'big', digits: 0 }),
    n('main', 'passes', 'dia-indicator', 560, 180, { label: 'Passes', kind: 'number', style: 'plain', digits: 0 }),
    wired(n('body', 'iter', 'dia-body-in', 100, 80, { name: 'iter' }), [['add', 'ge'], []], [['b', 'a'], []]),
    wired(n('body', 'sumIn', 'dia-body-in', 100, 200, { name: 'sum' }), [['add'], []], [['a'], []]),
    wired(n('body', 'add', 'dia-math', 300, 120, { op: 'add' }), [['sumOut'], []], [['value'], []]),
    n('body', 'sumOut', 'dia-body-out', 520, 120, { name: 'sum' }),
    wired(n('body', 'nine', 'dia-const', 100, 320, { value: '9' }), [['ge'], []], [['b'], []]),
    wired(n('body', 'ge', 'dia-compare', 300, 300, { op: 'ge' }), [['stop'], []], [['value'], []]),
    n('body', 'stop', 'dia-body-out', 520, 300, { name: 'stop' }),
  ];
}

const state = (page: Page) => page.locator('[data-test="diagram-state"]');

test('a diagram is run from the bar and its panel shows the answer', async ({ page, request }) => {
  await seed(request, counter());
  await signIn(page, 'loop');

  await expect(page.locator('[data-test="diagram-bar"]')).toBeVisible();
  // Named ports are drawn on the node, with their names.
  await expect(canvasNode(page, 'loop').locator('.port-name')).not.toHaveCount(0);
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);

  await page.locator('[data-test="diagram-run"]').click();
  await expect(state(page)).toHaveAttribute('data-state', 'done');

  // What the wires carried is kept after the run: hover a wire for its last value.
  const wire = page.locator('.canvas-host > .canvas path.wire[data-from="loop"][data-to="total"]');
  await expect(wire.locator('title')).toHaveText('45');

  // The panel is the same page an operator is given.
  await page.evaluate(() => { location.hash = '#/panel/main'; location.reload(); });
  await expect(page.locator('[data-test="panel"]')).toBeVisible();
  await expect(page.locator('[data-test="ind"][data-id="total"] [data-test="ind-value"]')).toHaveText('45');
  await expect(page.locator('[data-test="ind"][data-id="passes"] [data-test="ind-value"]')).toHaveText('10');
});

test('a breakpoint pauses the run before its node, and step and resume carry on', async ({ page, request }) => {
  await seed(request, counter());
  await signIn(page, 'loop');

  await nodeBody(page, 'loop').click();
  await page.locator('[data-test="diagram-break"]').click();
  await expect(canvasNode(page, 'loop')).toHaveClass(/has-break/);

  await page.locator('[data-test="diagram-run"]').click();
  await expect(state(page)).toHaveAttribute('data-state', 'paused');
  await expect(state(page)).toContainText('before');
  // Nothing after the loop has been given a value while it waits.
  await expect(page.locator('.canvas-host > .canvas path.wire[data-from="loop"][data-to="total"] title')).toHaveCount(0);

  // Step over runs the loop's whole body and stops before the next node.
  await page.locator('[data-test="diagram-step-over"]').click();
  await expect(state(page)).toContainText('before');
  await page.locator('[data-test="diagram-break"]').click(); // clears the breakpoint
  await page.locator('[data-test="diagram-resume"]').click();
  await expect(state(page)).toHaveAttribute('data-state', 'done');
});

test('a wire is dropped on a named input, and the file says which', async ({ page, request }) => {
  await seed(request, [
    { id: 'main', type: 'tab', label: 'Sum', ew_diagram: {} },
    { id: 'one', type: 'dia-const', z: 'main', x: 100, y: 100, wires: [], value: '1' },
    { id: 'two', type: 'dia-const', z: 'main', x: 100, y: 240, wires: [], value: '2' },
    { id: 'sum', type: 'dia-math', z: 'main', x: 420, y: 170, wires: [], op: 'sub' },
  ]);
  await signIn(page, 'one');
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(1);

  // Drag from each constant's value output to the input it should feed: two to a, one to b.
  const out = (id: string) => canvasNode(page, id).locator('.port-out').first();
  const input = (name: string) => canvasNode(page, 'sum').locator(`[data-in-port="${name}"]`);
  await drag(page, await stableCentre(page, out('two')), await stableCentre(page, input('a')));
  await drag(page, await stableCentre(page, out('one')), await stableCentre(page, input('b')));
  // The bar speaks for what is deployed, and a diagram runs from that, so the
  // problem stays until the fix is deployed.
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(1);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('two minus one');
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);
  await expect.poll(async () => (await deployed(request)).find((e) => e.id === 'two')?.['ew_in']).toEqual([['a']]);
  const file = await deployed(request);
  expect(file.find((e) => e.id === 'one')?.['ew_in']).toEqual([['b']]);
  expect(file.find((e) => e.id === 'two')?.wires?.[0]).toEqual(['sum']);
});
