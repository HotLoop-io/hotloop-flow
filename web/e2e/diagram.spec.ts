// Diagrams in the browser: a tab whose nodes run when every input is ready, with
// a bar to run and debug it and a panel page an operator opens. Every claim on the
// screen is checked against what the runtime has, and the runtime is the binary
// that ships.

import { expect, test, type Page } from '@playwright/test';
import { allNodes, canvasNode, deployed, drag, nodeBody, seed, select, signIn, stableCentre, topDeploy, type Entry } from './helpers';

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
  // B4: and the symbol its type is drawn with, our own, at the middle of the body.
  await expect(canvasNode(page, 'loop').locator('.node-symbol[data-symbol="loop"]')).toHaveCount(1);
  await expect(canvasNode(page, 'total').locator('.node-symbol[data-symbol="indicator"]')).toHaveCount(1);
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);

  // B2: a wire of a diagram is named at its middle, by the output it comes from.
  await expect(page.locator('.canvas-host > .canvas .wire-type[data-wire-name="sum"]')).toHaveCount(1);
  await expect(page.locator('.canvas-host > .canvas .wire-type[data-wire-name="sum"] text')).toHaveText('sum');
  await expect(page.locator('.canvas-host > .canvas .wire-type[data-wire-name="iterations"]')).toHaveCount(1);

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

// G1: the run records what it did, and Replay plays it again on the canvas, slowed
// down, after the run is over.
test('the last run is replayed on the canvas from what it recorded', async ({ page, request }) => {
  await seed(request, counter());
  await signIn(page, 'loop');

  // Nothing has run yet, so there is nothing to replay, and it says so.
  await page.locator('[data-test="diagram-replay"]').click();
  await expect(page.locator('[data-test="notice"]')).toContainText('Run the diagram once, then replay it.');

  await page.locator('[data-test="diagram-run"]').click();
  await expect(state(page)).toHaveAttribute('data-state', 'done');

  await page.locator('[data-test="diagram-replay-speed"]').selectOption('30');
  await page.locator('[data-test="diagram-replay"]').click();
  const progress = page.locator('[data-test="diagram-replay-progress"]');
  await expect(progress).toBeVisible();
  expect(Number(await progress.getAttribute('data-total'))).toBeGreaterThan(40);
  // Values move on the wires while it plays, as they did in the run.
  await expect(page.locator('.canvas-host > .canvas path.wire.live').first()).toBeAttached();
  await expect.poll(async () => Number(await progress.getAttribute('data-index'))).toBeGreaterThan(5);

  // It ends by itself and leaves what the run left.
  await expect(page.locator('[data-test="diagram-replay"]')).toBeVisible({ timeout: 30_000 });
  await expect(progress).toHaveCount(0);
  const wire = page.locator('.canvas-host > .canvas path.wire[data-from="loop"][data-to="total"]');
  await expect(wire.locator('title')).toHaveText('45');

  // And it can be stopped part of the way.
  await page.locator('[data-test="diagram-replay-speed"]').selectOption('700');
  await page.locator('[data-test="diagram-replay"]').click();
  await expect(progress).toBeVisible();
  await page.locator('[data-test="diagram-replay-stop"]').click();
  await expect(progress).toHaveCount(0);
  await expect(page.locator('[data-test="diagram-replay"]')).toBeVisible();
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

// The case structure's cases are rows with a diagram picker, not a JSON box.
test('a case structure is edited as rows of a case name and the diagram it runs', async ({ page, request }) => {
  const e = (z: string, id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}): Entry => ({ id, type, z, x, y, wires: [], ...extra });
  await seed(request, [
    { id: 'main', type: 'tab', label: 'Choose', ew_diagram: {} },
    { id: 'heat', type: 'tab', label: 'Heat body', ew_diagram: { role: 'body' } },
    { id: 'idle', type: 'tab', label: 'Idle body', ew_diagram: { role: 'body' } },
    e('main', 'sel', 'dia-case', 300, 120, { cases: JSON.stringify({ true: 'Heat body' }), default: '', ins: '', outs: '' }),
  ]);
  await signIn(page, 'sel');

  await canvasNode(page, 'sel').locator('.node-body').dblclick();
  const d = page.locator('.dialog');
  const rows = d.locator('[data-test="case-row"]');
  // The stored name of a tab is shown as that tab.
  await expect(rows).toHaveCount(1);
  await expect(rows.nth(0).locator('[data-test="case-name"]')).toHaveValue('true');
  await expect(rows.nth(0).locator('[data-test="case-body"] option:checked')).toHaveText('Heat body');

  await d.locator('[data-test="case-add"]').click();
  await rows.nth(1).locator('[data-test="case-name"]').fill('false');
  await rows.nth(1).locator('[data-test="case-body"]').selectOption({ label: 'Idle body' });
  await d.locator('[data-test="diagram-pick"]').selectOption({ label: 'Idle body' });
  await d.locator('button', { hasText: 'Done' }).click();
  await expect(d).toHaveCount(0);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('two cases and a default');
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  const saved = (await deployed(request)).find((x) => x.id === 'sel')!;
  expect(JSON.parse(String(saved.cases))).toEqual({ true: 'heat', false: 'idle' });
  expect(saved.default).toBe('idle');
});

// H1 in the editor: select nodes of a diagram, make a sub-diagram, and the
// connector is worked out and the nodes move to a tab of their own.
test('the selection of a diagram is cut out into a sub-diagram', async ({ page, request }) => {
  const e = (id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
    ({ id, type, z: 'main', x, y, wires, ew_in: ins, ...extra });
  await seed(request, [
    { id: 'main', type: 'tab', label: 'Arithmetic', ew_diagram: {} },
    e('a', 'dia-const', 100, 80, { value: '2' }, [['add']], [['a']]),
    e('b', 'dia-const', 100, 160, { value: '3' }, [['add']], [['b']]),
    e('add', 'dia-math', 300, 120, { op: 'add' }, [['out']], [['value']]),
    e('out', 'dia-indicator', 500, 120, { label: 'Sum', kind: 'number', style: 'big', digits: 0 }),
  ]);
  await signIn(page, 'add');
  const button = page.locator('[data-test="selection-to-subdiagram"]');
  await expect(button).toBeDisabled();
  await select(page, 'add');
  await expect(button).toBeEnabled();
  await button.click();
  await page.locator('[data-test="selection-subdiagram-input"]').fill('Adder');
  await page.locator('[data-test="selection-subdiagram-ok"]').click();
  await expect(page.locator('.dialog')).toHaveCount(0);

  // On the diagram: a, b, out and one sub-diagram node where add was.
  await expect(allNodes(page)).toHaveCount(4);
  await expect(page.locator('[data-test="tab-flow"]', { hasText: 'Adder' })).toHaveCount(1);
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-note"]').fill('cut add out');
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  const sub = (await deployed(request)).find((x) => x.type === 'dia-sub')!;
  expect(sub.name).toBe('Adder');
  expect(sub.ins).toBe('a,b');
  expect(sub.outs).toBe('add_result');

  // And it still adds up.
  await page.locator('[data-test="diagram-run"]').click();
  await expect(state(page)).toHaveAttribute('data-state', 'done');
  const wire = page.locator('.canvas-host > .canvas path.wire[data-to="out"]');
  await expect(wire.locator('title')).toHaveText('5');
});

// B1: the canvas zooms from 5 to 400 percent, about the pointer, and says where it is.
test('the canvas zooms out past 25 percent and in to 400 percent', async ({ page, request }) => {
  await seed(request, counter());
  await signIn(page, 'loop');
  const zoom = async () => Number(await page.locator('.canvas-host > .canvas').getAttribute('data-zoom'));
  const box = (await page.locator('.canvas-host > .canvas').boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  for (let i = 0; i < 60; i++) await page.mouse.wheel(0, 120);
  await expect.poll(zoom).toBeLessThanOrEqual(25);
  expect(await zoom()).toBeGreaterThanOrEqual(5);
  for (let i = 0; i < 120; i++) await page.mouse.wheel(0, -120);
  await expect.poll(zoom).toBe(400);
});

// P5: a diagram sets properties of its panel objects by name, and the panel an
// operator opens shows them.
test('a diagram turns an indicator red, disables a control and hides an object by name', async ({ page, request }) => {
  const e = (id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
    ({ id, type, z: 'main', x, y, wires, ew_in: ins, ...extra });
  await seed(request, [
    { id: 'main', type: 'tab', label: 'Guard', ew_diagram: {} },
    e('p', 'dia-const', 80, 60, { value: '42' }, [['ind']], [['value']]),
    e('ind', 'dia-indicator', 700, 60, { label: 'Pressure', kind: 'number', style: 'big', digits: 0 }),
    e('ctl', 'dia-control', 80, 200, { label: 'Setpoint', kind: 'number', style: 'entry' }),
    e('sp', 'dia-const', 80, 260, { value: '1' }, [['note']], [['value']]),
    e('note', 'dia-indicator', 700, 200, { label: 'Spare', kind: 'number', style: 'plain', digits: 0 }),
    e('red', 'dia-const', 80, 320, { value: '"red"' }, [['setc']], [['value']]),
    e('setc', 'dia-panel-set', 300, 320, { target: 'Pressure', property: 'color' }),
    e('off', 'dia-const', 80, 400, { value: 'false' }, [['sete', 'setv']], [['value', 'value']]),
    e('sete', 'dia-panel-set', 300, 400, { target: 'Setpoint', property: 'enabled' }),
    e('setv', 'dia-panel-set', 300, 480, { target: 'Spare', property: 'visible' }),
  ]);
  await signIn(page, 'setc');
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);
  await page.locator('[data-test="diagram-run"]').click();
  await expect(page.locator('[data-test="diagram-state"]')).toHaveAttribute('data-state', 'done');

  await page.evaluate(() => { location.hash = '#/panel/main'; location.reload(); });
  await expect(page.locator('[data-test="panel"]')).toBeVisible();
  await expect(page.locator('[data-test="ind"][data-id="ind"]')).toHaveAttribute('data-color', 'red');
  await expect(page.locator('[data-test="ctl"][data-id="ctl"] [data-test="ctl-number"]')).toBeDisabled();
  await expect(page.locator('[data-test="ind"][data-id="note"]')).toBeHidden();
});

// The instrument write switch: deploy says which instruments a diagram would write
// to, in the diagram bar, before anyone runs it.
test('a diagram that would write to an instrument says which one before it runs', async ({ page, request }) => {
  const e = (id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
    ({ id, type, z: 'main', x, y, wires, ew_in: ins, ...extra });
  await seed(request, [
    { id: 'main', type: 'tab', label: 'Supply', ew_diagram: {} },
    e('open', 'dia-instr-open', 100, 100, { address: '10.0.0.5:5025' }, [['on'], []], [['session'], []]),
    e('on', 'dia-instr-query', 340, 100, { command: 'OUTP ON', reply: 'none' }),
  ]);
  await signIn(page, 'on');
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);
  await page.locator('[data-test="diagram-warnings"]').click();
  await expect(page.locator('[data-test="diagram-warning-list"]')).toContainText('writes to tcp://10.0.0.5:5025');
  await expect(page.locator('[data-test="diagram-warning-list"]')).toContainText('instrument.write');
});
