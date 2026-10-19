// A click-through of every node dialog the runtime offers.
//
// The list of node types is not written here. It is read from the runtime's own
// GET /nodes, so a node added tomorrow is in this test tomorrow, with no change
// to it. For each type the dialog is opened the way a person opens one, every
// control in it is operated, and the dialog is closed both ways: cancelled, and
// saved. A node whose dialog throws, draws fewer fields than the node declares,
// or saves something the editor cannot read back fails here, in CI, instead of
// in front of an operator.

import { expect, test, type APIRequestContext, type Locator, type Page } from '@playwright/test';
import { allNodes, deployed, dialog, seed, signIn, token, type Entry } from './helpers';

interface Prop { name: string; kind: string; label?: string; required?: boolean; fields?: Prop[] }
interface Descriptor { type: string; paletteLabel?: string; isConfig?: boolean; props?: Prop[]; help?: string }

/** One configuration node of every type the runtime has, so a node that needs one can be saved. */
function configs(types: string[]): Entry[] {
  return types.map((type) => ({ id: `cfg-${type}`, type, name: `cfg ${type}` }));
}

function anchors(extra: Entry[] = []): Entry[] {
  return [
    { id: 't1', type: 'tab', label: 'Click-through' },
    { id: 'a1', type: 'inject', z: 't1', name: 'anchor one', props: [{ p: 'payload', v: '1', vt: 'num' }], x: 100, y: 120, wires: [[]] },
    { id: 'd1', type: 'debug', z: 't1', name: 'anchor two', complete: 'payload', x: 300, y: 120, wires: [] },
    ...extra,
  ];
}

async function nodeTypes(request: APIRequestContext): Promise<Descriptor[]> {
  const res = await request.get('/nodes', { headers: { Authorization: `Bearer ${await token(request)}` } });
  expect(res.ok()).toBeTruthy();
  const body = await res.json();
  return (Array.isArray(body) ? body : body.nodes) as Descriptor[];
}

/** Operates every control in an open dialog: selects through every option, checkboxes both ways, lists add and remove a row. */
async function operate(d: Locator): Promise<void> {
  // Lists first, because adding a row adds more controls to operate.
  const adds = d.locator('.field > button.ghost', { hasText: /^Add / });
  for (let i = 0; i < (await adds.count()); i++) {
    await adds.nth(i).click();
    await adds.nth(i).click();
    const rows = d.locator('.list-row');
    await expect(rows.first()).toBeVisible();
    // Take one of the two away again.
    await rows.last().locator('button.icon').click();
  }

  const selects = d.locator('select');
  for (let i = 0; i < (await selects.count()); i++) {
    const s = selects.nth(i);
    const values = await s.locator('option').evaluateAll((os) => os.map((o) => (o as HTMLOptionElement).value));
    for (const v of values) await s.selectOption(v);
  }

  const boxes = d.locator('input[type="checkbox"]');
  for (let i = 0; i < (await boxes.count()); i++) {
    await boxes.nth(i).click();
    await boxes.nth(i).click();
  }
}

/** Fills whatever is required and empty, the way somebody setting the node up would. */
async function fillRequired(d: Locator): Promise<void> {
  const fields = d.locator('.field:has(.required)');
  for (let i = 0; i < (await fields.count()); i++) {
    const f = fields.nth(i);
    const select = f.locator('select').last();
    if ((await select.count()) > 0 && (await f.locator('input[type="text"], textarea').count()) === 0) {
      const values = await select.locator('option').evaluateAll((os) => os.map((o) => (o as HTMLOptionElement).value));
      const pick = values.find((v) => v !== '');
      if (pick !== undefined) await select.selectOption(pick);
      continue;
    }
    const num = f.locator('input[type="number"]');
    if ((await num.count()) > 0) { if (!(await num.first().inputValue())) await num.first().fill('1'); continue; }
    const area = f.locator('textarea');
    if ((await area.count()) > 0) {
      if (!(await area.first().inputValue()).trim()) {
        const cls = (await area.first().getAttribute('class')) ?? '';
        await area.first().fill(cls.includes('lang-json') ? '{}' : cls.includes('lang-js') ? 'return msg;' : '1');
      }
      continue;
    }
    const text = f.locator('input[type="text"], input[type="password"]').last();
    if ((await text.count()) > 0 && !(await text.inputValue())) await text.fill('x');
  }
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

async function checkFields(page: Page, d: Descriptor): Promise<void> {
  const dlg = dialog(page);
  await expect(dlg).toBeVisible();
  await expect(dlg.locator('.dialog-head h2')).toHaveText(d.paletteLabel ?? d.type);

  // Every property the node declares is drawn, and the name is always there.
  const props = d.props ?? [];
  const expected = props.length + (props.some((p) => p.name === 'name') ? 0 : 1);
  await expect(dlg.locator('.dialog-body > .field, .dialog-body > .field-inline')).toHaveCount(expected);
  // Every label is the declared one.
  for (const p of props) {
    const shown = p.label ?? p.name;
    // The label, with the star a required one carries.
    const wanted = new RegExp('^\\s*' + escapeRegExp(shown) + '(\\s*\\*)?\\s*$');
    await expect(dlg.locator('.dialog-body label').filter({ hasText: wanted }).first(), `${d.type}: ${shown}`).toBeVisible();
  }
  if (d.help) await expect(dlg.locator('.dialog-help summary')).toHaveText('About this node');
}

test('every node type in the palette opens a dialog that draws all of its properties, takes every input, cancels, and saves', async ({ page, request }) => {
  test.setTimeout(8 * 60_000);

  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));

  // The list comes from the runtime, not from this file.
  const all = await nodeTypes(request);
  const configTypes = all.filter((d) => d.isConfig).map((d) => d.type);
  const palette = all.filter((d) => !d.isConfig);
  expect(palette.length).toBeGreaterThan(40);
  expect(configTypes.length).toBeGreaterThan(0);

  // The configuration nodes go in over the API, so nodes that need one can be saved.
  await seed(request, anchors(configs(configTypes)));
  await signIn(page);

  let opened = 0;
  for (const d of palette) {
    await test.step(`${d.type}: open, check, operate, cancel`, async () => {
      const before = await allNodes(page).count();
      await page.locator(`.palette-item[data-palette-type="${d.type}"]`).dblclick();
      await checkFields(page, d);
      await operate(dialog(page));
      await dialog(page).locator('button', { hasText: 'Cancel' }).click();
      await expect(dialog(page)).toHaveCount(0);
      await expect(allNodes(page)).toHaveCount(before + 1);

      // Open it again from the canvas and save it.
      const node = page.locator('.canvas-host > .canvas .node.selected').first();
      await node.locator('.node-body').dblclick();
      await checkFields(page, d);
      await fillRequired(dialog(page));
      await dialog(page).locator('button', { hasText: 'Done' }).click();
      await expect(dialog(page), `${d.type} would not save with its required fields filled`).toHaveCount(0);
      opened++;

      // Take it off again so the canvas stays the same size for the next one.
      await node.locator('.node-body').click();
      await page.keyboard.press('Delete');
      await expect(allNodes(page)).toHaveCount(before);
    });
  }
  expect(opened).toBe(palette.length);

  // The configuration nodes have no place on a canvas. Their dialogs open from search.
  for (const type of configTypes) {
    await test.step(`${type}: open from search, operate, cancel, save`, async () => {
      const d = all.find((x) => x.type === type)!;
      await page.locator('[data-test="search-open"]').click();
      await page.locator('[data-test="search-input"]').fill(`cfg ${type}`);
      await page.locator('[data-test="search-hit"]').first().click();
      await checkFields(page, d);
      await operate(dialog(page));
      await dialog(page).locator('button', { hasText: 'Cancel' }).click();
      await expect(dialog(page)).toHaveCount(0);

      await page.locator('[data-test="search-hit"]').first().click();
      await fillRequired(dialog(page));
      await dialog(page).locator('button', { hasText: 'Done' }).click();
      await expect(dialog(page), `${type} would not save`).toHaveCount(0);
      await page.locator('[data-test="search-close"]').click();
    });
  }

  expect(errors, 'the page threw while a dialog was open').toEqual([]);
});

test('a node type this runtime does not have opens as raw JSON, and what is saved is kept', async ({ page, request }) => {
  await seed(request, anchors([
    { id: 'u1', type: 'acme-widget', z: 't1', name: 'mystery', size: 3, x: 200, y: 260, wires: [[]] },
  ]));
  await signIn(page, 'd1');

  await page.locator('.canvas-host > .canvas [data-node-id="u1"] .node-body').dblclick();
  const dlg = dialog(page);
  await expect(dlg.locator('.banner')).toContainText('acme-widget is not installed in this runtime');
  const raw = dlg.locator('textarea.lang-json');
  expect(JSON.parse(await raw.inputValue())).toEqual({ name: 'mystery', size: 3 });

  // Broken JSON is refused with the reason, and the structural keys cannot be set by hand.
  await raw.fill('{ nope');
  await dlg.locator('button', { hasText: 'Done' }).click();
  await expect(dlg.locator('.dialog-error')).toContainText('not valid JSON');
  await raw.fill('{"name":"x","wires":[]}');
  await dlg.locator('button', { hasText: 'Done' }).click();
  await expect(dlg.locator('.dialog-error')).toContainText('wires is managed by the editor');

  await raw.fill('{"name":"renamed","size":4}');
  await dlg.locator('button', { hasText: 'Done' }).click();
  await expect(dialog(page)).toHaveCount(0);

  await page.locator('.topbar button', { hasText: 'Deploy' }).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(page.locator('[data-test="tab-state"]')).toHaveText('saved');
  const kept = (await deployed(request)).find((e) => e.id === 'u1')!;
  expect(kept.name).toBe('renamed');
  expect(kept.size).toBe(4);
  expect(kept.type).toBe('acme-widget');
});
