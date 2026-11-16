// P3: an intensity chart draws a grid as colors, smallest to largest, on the panel.

import { expect, test } from '@playwright/test';
import { seed, signIn, type Entry } from './helpers';

test('an intensity chart draws its grid as colors and keeps the shape of the grid', async ({ page }) => {
  const e = (id: string, type: string, x: number, y: number, extra: Record<string, unknown> = {}, wires: string[][] = [[]], ins: string[][] = [[]]): Entry =>
    ({ id, type, z: 'main', x, y, wires, ew_in: ins, ...extra });
  await seed(page.request, [
    { id: 'main', type: 'tab', label: 'Heat', ew_diagram: {} },
    e('g', 'dia-const', 100, 100, { value: '[[0,0,0,0],[0,10,5,0],[10,0,0,10]]' }, [['c']], [['y']]),
    e('c', 'dia-chart', 400, 100, { label: 'Heat map', mode: 'intensity' }),
    e('flat', 'dia-const', 100, 220, { value: '[1,2,3]' }, [['bad']], [['y']]),
    e('bad', 'dia-chart', 400, 220, { label: 'Not a grid', mode: 'intensity' }),
  ]);
  await signIn(page, 'g');
  await page.evaluate(() => { location.hash = '#/panel/main'; location.reload(); });
  await expect(page.locator('[data-test="panel"]')).toBeVisible();
  await page.locator('[data-test="panel-run"]').click();
  const canvas = page.locator('[data-test="chart"][data-id="c"] [data-test="chart-grid"]');
  await expect(canvas).toHaveAttribute('width', '4');
  await expect(canvas).toHaveAttribute('height', '3');
  await expect(page.locator('[data-test="chart"][data-id="c"] [data-test="chart-count"]')).toHaveText('3 by 4, 0.00 to 10.00');
  const px = await canvas.evaluate((c: HTMLCanvasElement) => {
    const d = c.getContext('2d')!;
    const at = (x: number, y: number) => Array.from(d.getImageData(x, y, 1, 1).data);
    return { low: at(0, 0), mid: at(2, 1), high: at(1, 1) };
  });
  // The smallest is the cool end of the ramp, the largest the hot end, the middle between.
  expect(px.low[2]).toBeGreaterThan(px.low[0]!);
  expect(px.high[0]).toBeGreaterThan(px.high[2]!);
  expect(px.mid).not.toEqual(px.low);
  expect(px.mid).not.toEqual(px.high);
  // A flat array is not a grid, and the run says so instead of drawing something.
  await expect(page.locator('[data-test="chart"][data-id="bad"] [data-test="chart-count"]')).toHaveText('no data yet');
});
