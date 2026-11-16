// P2: the controls of a panel. A number is typed, spun, slid or turned; a switch is
// a switch, a push button or an LED; an array is a list the operator grows; a
// record is a fixed set of named fields. Each writes its value to the runtime when
// it is changed and the diagram reads it when the control runs. Everything here is
// the binary that ships, driven in a real browser the way an operator would, with
// a mouse on the desktop and a finger on a phone.

import { mkdirSync } from 'node:fs';
import path from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { seed, signIn, token, type Entry } from './helpers';

// Where screenshots go, when whoever is running these wants them kept.
const shots = process.env.FLOWLAB_SHOTS;
async function shot(page: Page, name: string): Promise<void> {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  await page.screenshot({ path: path.join(shots, `${name}.png`), fullPage: true });
}

function controls(): Entry[] {
  const e = (id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
    ({ id, type, z: 'main', x, y, wires, ew_in: ins, ...extra });
  const pair = (id: string, y: number, control: Record<string, unknown>, indicator: Record<string, unknown>): Entry[] => [
    e(id, 'dia-control', 120, y, control, [[`${id}-out`]], [['value']]),
    e(`${id}-out`, 'dia-indicator', 420, y, indicator),
  ];
  return [
    { id: 'main', type: 'tab', label: 'Panel controls', ew_diagram: {} },
    ...pair('speed', 60, { label: 'Speed', kind: 'number', style: 'knob', min: '0', max: '100', step: '5', default: '20', unit: 'rpm' }, { label: 'Speed read', kind: 'number', style: 'plain', digits: 0 }),
    ...pair('gain', 160, { label: 'Gain', kind: 'number', style: 'spin', min: '0', max: '10', step: '0.5', default: '2' }, { label: 'Gain read', kind: 'number', style: 'plain', digits: 1 }),
    ...pair('enable', 260, { label: 'Enable', kind: 'bool', style: 'led' }, { label: 'Enable read', kind: 'bool', style: 'led' }),
    ...pair('points', 360, { label: 'Setpoints', kind: 'array', elem: 'number', min: '0', max: '100', maxlen: '4' }, { label: 'Setpoints read', kind: 'text' }),
    ...pair('recipe', 460, { label: 'Recipe', kind: 'record', fields: 'rate:number, on:bool, note:string' }, { label: 'Recipe read', kind: 'text' }),
  ];
}

const ctl = (page: Page, id: string) => page.locator(`[data-test="ctl"][data-id="${id}"]`);
const ind = (page: Page, id: string) => page.locator(`[data-test="ind"][data-id="${id}-out"] [data-test="ind-value"]`);

/** Does the thing and waits for the runtime to answer the control it sets. */
async function writes(page: Page, act: () => Promise<unknown>): Promise<void> {
  await Promise.all([
    page.waitForResponse((r) => r.url().includes('/controls/') && r.request().method() === 'PUT' && r.ok()),
    act(),
  ]);
}

// What an operator sets stays on the panel across a deploy of the same diagram, so
// every test starts by putting each control back where it was designed to start.
async function startFresh(page: Page): Promise<void> {
  await seed(page.request, controls());
  const headers = { Authorization: `Bearer ${await token(page.request)}` };
  const starts: Record<string, unknown> = { speed: 20, gain: 2, enable: false, points: [], recipe: { rate: 0, on: false, note: '' } };
  for (const [node, value] of Object.entries(starts)) {
    const res = await page.request.put(`/diagrams/main/controls/${node}`, { headers, data: { value } });
    expect(res.ok(), await res.text()).toBeTruthy();
  }
}

async function openPanel(page: Page): Promise<void> {
  await startFresh(page);
  await signIn(page, 'speed');
  await expect(page.locator('[data-test="diagram-problems"]')).toHaveCount(0);
  await page.evaluate(() => { location.hash = '#/panel/main'; location.reload(); });
  await expect(page.locator('[data-test="panel"]')).toBeVisible();
  await expect(page.locator('[data-test="ctl"]')).toHaveCount(5);
}

/** The point on a knob where a value sits, to press there. */
async function knobAt(page: Page, fraction: number): Promise<{ x: number; y: number }> {
  const b = await ctl(page, 'speed').locator('[data-test="ctl-knob"]').boundingBox();
  if (!b) throw new Error('the knob is not on screen');
  const deg = ((135 + 270 * fraction) * Math.PI) / 180;
  const r = b.width * 0.4;
  return { x: b.x + b.width / 2 + r * Math.cos(deg), y: b.y + b.height / 2 + r * Math.sin(deg) };
}

test('each control writes what the operator sets and the diagram reads it', async ({ page }) => {
  await openPanel(page);
  await shot(page, 'panel-desktop-light-before');

  // The knob: the keyboard steps it by its step, and a press on the dial puts it there.
  const knob = ctl(page, 'speed').locator('[data-test="ctl-knob"]');
  await expect(knob).toHaveAttribute('aria-valuenow', '20');
  await knob.focus();
  await writes(page, () => page.keyboard.press('ArrowUp'));
  await expect(knob).toHaveAttribute('aria-valuenow', '25');
  await writes(page, () => page.keyboard.press('PageUp'));
  await expect(knob).toHaveAttribute('aria-valuenow', '75');
  await writes(page, () => page.keyboard.press('Home'));
  await expect(knob).toHaveAttribute('aria-valuenow', '0');
  const at = await knobAt(page, 0.6);
  await writes(page, () => page.mouse.click(at.x, at.y));
  await expect(knob).toHaveAttribute('aria-valuenow', '60');
  await expect(ctl(page, 'speed').locator('[data-test="ctl-knob-value"]')).toHaveText('60 rpm');

  // The spin entry: its buttons step it, held to its range, and it can still be typed.
  const spin = ctl(page, 'gain');
  await writes(page, () => spin.locator('[data-test="ctl-spin-up"]').click());
  await writes(page, () => spin.locator('[data-test="ctl-spin-up"]').click());
  await expect(spin.locator('[data-test="ctl-spin-input"]')).toHaveValue('3');
  await writes(page, () => spin.locator('[data-test="ctl-spin-down"]').click());
  await expect(spin.locator('[data-test="ctl-spin-input"]')).toHaveValue('2.5');
  await writes(page, async () => { await spin.locator('[data-test="ctl-spin-input"]').fill('10'); await page.keyboard.press('Tab'); });
  await expect(spin.locator('[data-test="ctl-spin-up"]')).toBeDisabled();

  // The LED is a switch you can see from across the room.
  const led = ctl(page, 'enable').locator('[data-test="ctl-led"]');
  await expect(led).toHaveAttribute('aria-checked', 'false');
  await writes(page, () => led.click());
  await expect(led).toHaveAttribute('aria-checked', 'true');

  // The array: add, type, remove, and the most it holds.
  const arr = ctl(page, 'points');
  await expect(arr.locator('[data-test="ctl-array-empty"]')).toBeVisible();
  for (const n of [1, 2]) {
    await writes(page, () => arr.locator('[data-test="ctl-array-add"]').click());
    await expect(arr.locator('[data-test="ctl-array-item"]')).toHaveCount(n);
  }
  await writes(page, async () => { await arr.locator('[data-test="ctl-array-value"]').nth(0).fill('12'); await page.keyboard.press('Tab'); });
  await writes(page, async () => { await arr.locator('[data-test="ctl-array-value"]').nth(1).fill('40'); await page.keyboard.press('Tab'); });
  await writes(page, () => arr.locator('[data-test="ctl-array-add"]').click());
  await expect(arr.locator('[data-test="ctl-array-item"]')).toHaveCount(3);
  await writes(page, () => arr.locator('[data-test="ctl-array-remove"]').nth(0).click());
  await expect(arr.locator('[data-test="ctl-array-item"]')).toHaveCount(2);
  await expect(arr.locator('[data-test="ctl-array-value"]').nth(0)).toHaveValue('40');
  await writes(page, () => arr.locator('[data-test="ctl-array-add"]').click());
  await writes(page, () => arr.locator('[data-test="ctl-array-add"]').click());
  await expect(arr.locator('[data-test="ctl-array-add"]')).toBeDisabled();

  // The record: each field is its own kind of input.
  const rec = ctl(page, 'recipe');
  await expect(rec.locator('[data-test="ctl-field"]')).toHaveCount(3);
  await writes(page, async () => { await rec.locator('[data-field="rate"] input').fill('3.5'); await page.keyboard.press('Tab'); });
  await writes(page, () => rec.locator('[data-field="on"] input').check());
  await writes(page, async () => { await rec.locator('[data-field="note"] input').fill('batch 4'); await page.keyboard.press('Tab'); });
  await expect(rec.locator('[data-field="rate"] input')).toHaveValue('3.5');
  await shot(page, 'panel-desktop-light-set');

  // Run: the diagram reads each control as the operator left it.
  await page.locator('[data-test="panel-run"]').click();
  await expect(page.locator('[data-test="panel-state"]')).toHaveAttribute('data-state', 'done');
  await expect(ind(page, 'speed')).toHaveText('60');
  await expect(ind(page, 'gain')).toHaveText('10.0');
  await expect(page.locator('[data-test="ind"][data-id="enable-out"]  [data-on="true"]')).toHaveCount(1);
  await expect.poll(async () => JSON.parse((await ind(page, 'points').textContent()) ?? 'null')).toEqual([40, 0, 0, 0]);
  await expect.poll(async () => JSON.parse((await ind(page, 'recipe').textContent()) ?? 'null')).toEqual({ note: 'batch 4', on: true, rate: 3.5 });
  await shot(page, 'panel-desktop-light-after-run');

  // What the operator set is still there after the page is opened again.
  await page.reload();
  await expect(page.locator('[data-test="panel"]')).toBeVisible();
  await expect(ctl(page, 'speed').locator('[data-test="ctl-knob"]')).toHaveAttribute('aria-valuenow', '60');
  await expect(ctl(page, 'points').locator('[data-test="ctl-array-item"]')).toHaveCount(4);
  await expect(ctl(page, 'recipe').locator('[data-field="note"] input')).toHaveValue('batch 4');
});

test('the runtime refuses what a control cannot hold, in words', async ({ page }) => {
  await startFresh(page);
  const headers = { Authorization: `Bearer ${await token(page.request)}` };
  const put = async (node: string, value: unknown) => {
    const res = await page.request.put(`/diagrams/main/controls/${node}`, { headers, data: { value } });
    // The refusal is JSON, so its quotes are escaped in the text; read the words, not the wire.
    const text = await res.text();
    let body = text;
    try { const j = JSON.parse(text); body = String(j.error ?? text); } catch { /* not JSON */ }
    return { status: res.status(), body, text };
  };
  expect((await put('points', [1, 500])).body).toContain('item 2: 500 is above the maximum 100');
  expect((await put('points', [1, 2, 3, 4, 5])).body).toContain('5 items is more than the most this control holds, 4');
  expect((await put('points', 'one')).status).toBe(422);
  expect((await put('recipe', { speed: 1 })).body).toContain('"speed" is not a field of this record');
  expect((await put('recipe', { rate: 'fast' })).body).toContain('field "rate": "fast" is not a number');
  expect((await put('speed', 101)).body).toContain('101 is above the maximum 100');
  const ok = await put('recipe', { rate: 1 });
  expect(ok.status).toBe(200);
  // A record always has every field, so what comes back is the whole record.
  expect(JSON.parse(ok.text).value).toEqual({ rate: 1, on: false, note: '' });
});

// A finger, on a phone: every control is a target a finger can hit, nothing runs off
// the side of the page, and the dial turns under a tap.
test.describe('on a phone', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  for (const scheme of ['light', 'dark'] as const) {
    test(`every control is touch sized and the page does not scroll sideways (${scheme})`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await openPanel(page);

      // Fill the array and the record so the phone layout is the busy one.
      const arr = ctl(page, 'points');
      for (const n of [1, 2, 3]) {
        await writes(page, () => arr.locator('[data-test="ctl-array-add"]').tap());
        await expect(arr.locator('[data-test="ctl-array-item"]')).toHaveCount(n);
      }

      const small = await page.evaluate(() => {
        const bad: string[] = [];
        const sel = '.panel-card button, .panel-card select, .panel-card input[type="text"], .panel-card input[type="number"], .panel-card input[type="range"], .panel-card label.switch, .panel-card label.field-row, .panel-card [role="slider"], .panel-card [role="switch"]';
        for (const el of document.querySelectorAll<HTMLElement>(sel)) {
          const r = el.getBoundingClientRect();
          if (r.width === 0 && r.height === 0) continue;
          if (r.height < 44 - 0.5 || r.width < 44 - 0.5) bad.push(`${el.tagName.toLowerCase()}[${el.getAttribute('data-test') ?? ''}] ${Math.round(r.width)}x${Math.round(r.height)}`);
        }
        return bad;
      });
      expect(small, `controls smaller than a finger: ${small.join(', ')}`).toEqual([]);

      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      expect(overflow, 'the page scrolls sideways').toBeLessThanOrEqual(0);

      // A tap on the spin button and a tap on the dial are what a finger does.
      await writes(page, () => ctl(page, 'gain').locator('[data-test="ctl-spin-up"]').tap());
      await expect(ctl(page, 'gain').locator('[data-test="ctl-spin-input"]')).toHaveValue('2.5');
      const at = await knobAt(page, 0.75);
      await writes(page, () => page.touchscreen.tap(at.x, at.y));
      await expect(ctl(page, 'speed').locator('[data-test="ctl-knob"]')).toHaveAttribute('aria-valuenow', '75');
      await shot(page, `panel-phone-${scheme}`);
    });
  }
});
