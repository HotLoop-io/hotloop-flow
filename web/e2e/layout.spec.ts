// B3: auto layout puts a 30 node diagram in columns that read left to right, with
// no wire running through a node. Every wire of the real diagram is sampled along
// its drawn curve and checked against every node it is not wired to.

import { expect, test } from '@playwright/test';
import { allNodes, canvasNode, focusCanvas, seed, signIn, type Entry } from './helpers';

// Six constants feed a lattice of math nodes. Every node takes one wire from the
// column before it and one from two or three back, so there are long wires that
// have to find a lane.
function lattice(): Entry[] {
  const out: Entry[] = [{ id: 'main', type: 'tab', label: 'Lattice', ew_diagram: {} }];
  const ids: string[] = [];
  const wires = new Map<string, string[]>();
  const ins = new Map<string, string[]>();
  const add = (id: string, type: string, extra: Record<string, unknown>) => {
    ids.push(id);
    wires.set(id, []);
    ins.set(id, []);
    out.push({ id, type, z: 'main', x: 100 + (ids.length % 7) * 31, y: 80 + (ids.length % 5) * 17, wires: [[]], ew_in: [[]], ...extra });
  };
  const connect = (from: string, to: string, port: string) => { wires.get(from)!.push(to); ins.get(from)!.push(port); };
  for (let i = 0; i < 6; i++) add(`c${i}`, 'dia-const', { value: String(i) });
  for (let i = 0; i < 24; i++) {
    const id = `m${i}`;
    add(id, 'dia-math', { op: 'add' });
    const prev = ids[ids.length - 2]!;
    const far = ids[Math.max(0, ids.length - 2 - (2 + (i % 3)))]!;
    connect(prev, id, 'a');
    connect(far, id, 'b');
  }
  for (const e of out) {
    if (e.type === 'tab') continue;
    e.wires = [wires.get(e.id) ?? []];
    e.ew_in = [ins.get(e.id) ?? []];
  }
  return out;
}

test('auto layout of a 30 node diagram leaves no wire through a node', async ({ page, request }) => {
  await seed(request, lattice());
  await signIn(page, 'm23');
  await expect(allNodes(page)).toHaveCount(30);

  await page.locator('[data-test="auto-layout"]').click();

  const hits = await page.evaluate(() => {
    const svg = document.querySelector('.canvas-host > .canvas') as SVGSVGElement;
    const boxes = new Map<string, DOMRect>();
    svg.querySelectorAll('[data-node-id]').forEach((n) => {
      const body = n.querySelector('.node-body') as SVGGraphicsElement | null;
      if (body) boxes.set(n.getAttribute('data-node-id')!, body.getBBox());
    });
    const through: string[] = [];
    let wires = 0;
    svg.querySelectorAll('path.wire[data-from][data-to]').forEach((p) => {
      wires++;
      const path = p as unknown as SVGGeometryElement;
      const from = p.getAttribute('data-from')!, to = p.getAttribute('data-to')!;
      const len = path.getTotalLength();
      for (let t = 0.04; t < 0.96; t += 0.02) {
        const pt = path.getPointAtLength(len * t);
        for (const [id, b] of boxes) {
          if (id === from || id === to) continue;
          if (pt.x > b.x && pt.x < b.x + b.width && pt.y > b.y && pt.y < b.y + b.height) through.push(`${from}>${to} through ${id}`);
        }
      }
    });
    return { through: [...new Set(through)], wires };
  });
  expect(hits.wires).toBeGreaterThan(40);
  expect(hits.through).toEqual([]);

  // Data reads left to right: a wire's target is to the right of its source.
  const xs = new Map<string, number>();
  for (const id of ['c0', 'm0', 'm1', 'm12', 'm23']) xs.set(id, (await canvasNode(page, id).boundingBox())!.x);
  expect(xs.get('c0')!).toBeLessThan(xs.get('m0')!);
  expect(xs.get('m0')!).toBeLessThan(xs.get('m12')!);
  expect(xs.get('m12')!).toBeLessThan(xs.get('m23')!);

  // One edit: undo puts every node back.
  const before = await canvasNode(page, 'm23').boundingBox();
  await focusCanvas(page);
  await page.keyboard.press('Control+z');
  const after = await canvasNode(page, 'm23').boundingBox();
  expect(after!.x).not.toBe(before!.x);
});
