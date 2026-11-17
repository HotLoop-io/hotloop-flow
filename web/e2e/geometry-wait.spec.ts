// A canvas element that is redrawn is, for a moment, not on screen. A test that reads where
// a port is in that moment must wait for it and not fail with "not on screen": the Editor job
// failed once on main (889526a, subdiagram-inline "a wire drawn between a node and a node
// inside an open sub-diagram is refused in words") exactly that way.

import { expect, test } from '@playwright/test';
import { canvasNode, centre, seed, signIn } from './helpers';

test('a port that is being redrawn is waited for, and not reported as not on screen', async ({ page, request }) => {
  await seed(request, [
    { id: 't', type: 'tab', label: 'T' },
    { id: 'a', type: 'inject', z: 't', x: 100, y: 100, wires: [['b']], props: [{ p: 'payload', v: '1', vt: 'num' }] },
    { id: 'b', type: 'debug', z: 't', x: 400, y: 100, wires: [] },
  ]);
  await signIn(page, 'a');
  const port = canvasNode(page, 'a').locator('.port-out').first();
  const here = await centre(port);

  // The port goes away the way it does in a redraw, and comes back 300 ms later.
  await page.evaluate(() => {
    const el = document.querySelector('.canvas-host > .canvas [data-node-id="a"] .port-out') as SVGElement;
    el.style.display = 'none';
    setTimeout(() => (el.style.display = ''), 300);
  });
  const again = await centre(port);
  expect(Math.abs(again.x - here.x)).toBeLessThan(1);
  expect(Math.abs(again.y - here.y)).toBeLessThan(1);
});
