// Auto layout: where nodes go so that data reads left to right and no wire has to
// run through a node.
//
// The diagram is layered. A node's column is the longest chain of wires leading
// to it, so a wire always runs forward. A wire that skips columns gets a
// stand-in in each column it crosses, a slot nothing else may take, and the
// stand-ins are ordered with everything else, so the wire has a lane through
// the columns and no node sits in it. Within a column the order is the average
// position of the neighbours (the barycenter), swept down and up a few times,
// which is what untangles the crossings. Then each node is moved toward the
// middle of what it is wired to, never past a neighbour in its column.
//
// A loop in the wiring (a message flow can have one) has its closing wire left
// out of the ranking, so it runs backward instead of making the layout loop.

export interface LayoutNode { id: string; h: number }
/** fromDy and toDy are how far the wire's two ports sit below the middle of their nodes. */
export interface LayoutEdge { from: string; to: string; fromDy?: number; toDy?: number }
export interface LayoutOptions { colGap: number; rowGap: number; width: number }
export interface Placed { x: number; y: number }

interface V { id: string; h: number; real: boolean; col: number; y: number; ins: V[]; outs: V[] }

export function layout(nodes: LayoutNode[], edges: LayoutEdge[], o: LayoutOptions): Map<string, Placed> {
  const byId = new Map<string, V>();
  for (const n of nodes) byId.set(n.id, { id: n.id, h: n.h, real: true, col: 0, y: 0, ins: [], outs: [] });
  const wires: [V, V][] = [];
  const dys = new Map<string, [number, number]>();
  const seen = new Set<string>();
  for (const e of edges) {
    const a = byId.get(e.from), b = byId.get(e.to);
    const key = `${e.from}>${e.to}`;
    if (!a || !b || a === b || seen.has(key)) continue;
    seen.add(key);
    wires.push([a, b]);
    dys.set(key, [e.fromDy ?? 0, e.toDy ?? 0]);
  }

  // Break cycles: a wire back to a node still being visited closes a loop.
  const state = new Map<V, number>();
  const forward = new Map<V, V[]>();
  for (const [a, b] of wires) forward.set(a, [...(forward.get(a) ?? []), b]);
  const back = new Set<string>();
  const visit = (v: V): void => {
    state.set(v, 1);
    for (const w of forward.get(v) ?? []) {
      if (state.get(w) === 1) back.add(`${v.id}>${w.id}`);
      else if (!state.has(w)) visit(w);
    }
    state.set(v, 2);
  };
  for (const n of nodes) { const v = byId.get(n.id)!; if (!state.has(v)) visit(v); }
  const dag = wires.filter(([a, b]) => !back.has(`${a.id}>${b.id}`));

  // Columns: the longest path from a source.
  const incoming = new Map<V, V[]>();
  for (const [a, b] of dag) incoming.set(b, [...(incoming.get(b) ?? []), a]);
  const colOf = new Map<V, number>();
  const col = (v: V): number => {
    const known = colOf.get(v);
    if (known !== undefined) return known;
    colOf.set(v, 0);
    const c = Math.max(-1, ...(incoming.get(v) ?? []).map(col)) + 1;
    colOf.set(v, c);
    return c;
  };
  for (const n of nodes) byId.get(n.id)!.col = col(byId.get(n.id)!);

  // Stand-ins for the columns a wire crosses.
  const all: V[] = [...byId.values()];
  let k = 0;
  const link = (a: V, b: V) => { a.outs.push(b); b.ins.push(a); };
  for (const [a, b] of dag) {
    let prev = a;
    for (let c = a.col + 1; c < b.col; c++) {
      const s: V = { id: `~${k++}`, h: 20, real: false, col: c, y: 0, ins: [], outs: [] };
      all.push(s);
      link(prev, s);
      prev = s;
    }
    link(prev, b);
  }
  // Wires that run backward take no part in the ordering.

  const cols: V[][] = [];
  for (const v of all) (cols[v.col] ??= []).push(v);
  for (let c = 0; c < cols.length; c++) cols[c] ??= [];

  // Start from the order the nodes already have on the page, so a layout of a
  // diagram that is nearly tidy moves little.
  const startY = new Map<string, number>();
  nodes.forEach((n, i) => startY.set(n.id, i));
  for (const list of cols) list.sort((a, b) => (startY.get(a.id) ?? 1e9) - (startY.get(b.id) ?? 1e9));

  const position = (): void => { for (const list of cols) list.forEach((v, i) => ((v as V & { pos?: number }).pos = i)); };
  const pos = (v: V): number => (v as V & { pos?: number }).pos ?? 0;
  const sweep = (list: V[], toward: (v: V) => V[]): void => {
    const score = new Map<V, number>();
    for (const v of list) {
      const ns = toward(v);
      score.set(v, ns.length ? ns.reduce((s, n) => s + pos(n), 0) / ns.length : pos(v));
    }
    list.sort((a, b) => score.get(a)! - score.get(b)! || pos(a) - pos(b));
    list.forEach((v, i) => ((v as V & { pos?: number }).pos = i));
  };
  position();
  for (let pass = 0; pass < 6; pass++) {
    for (let c = 1; c < cols.length; c++) sweep(cols[c]!, (v) => v.ins);
    for (let c = cols.length - 2; c >= 0; c--) sweep(cols[c]!, (v) => v.outs);
  }

  // Heights: stack each column, centre it on the tallest.
  const stack = (list: V[]): number => {
    let y = 0;
    for (const v of list) { v.y = y + v.h / 2; y += v.h + o.rowGap; }
    return Math.max(0, y - o.rowGap);
  };
  const heights = cols.map(stack);
  const tallest = Math.max(0, ...heights);
  cols.forEach((list, c) => { const shift = (tallest - heights[c]!) / 2; for (const v of list) v.y += shift; });

  // Pull each node toward the middle of its wires, keeping the order and the gap.
  for (let pass = 0; pass < 8; pass++) {
    const order = pass % 2 === 0 ? cols.map((_, i) => i) : cols.map((_, i) => cols.length - 1 - i);
    for (const c of order) {
      const list = cols[c]!;
      const want = list.map((v) => {
        const ns = pass % 2 === 0 ? v.ins : v.outs;
        return ns.length ? ns.reduce((s, n) => s + n.y, 0) / ns.length : v.y;
      });
      let floor = -Infinity;
      list.forEach((v, i) => { v.y = Math.max(want[i]!, floor + 0); floor = v.y + v.h / 2 + o.rowGap; if (i + 1 < list.length) floor += list[i + 1]!.h / 2; });
      // Push back from the bottom so nothing overlaps its neighbour below.
      let ceil = Infinity;
      for (let i = list.length - 1; i >= 0; i--) {
        const v = list[i]!;
        v.y = Math.min(v.y, ceil);
        ceil = v.y - v.h / 2 - o.rowGap - (i > 0 ? list[i - 1]!.h / 2 : 0);
      }
    }
  }

  // The curve a wire is drawn with is not the straight line the lanes assumed,
  // so check each long wire against the nodes in the columns it crosses and move
  // a node that is in the way up or down, clear of the curve, then separate the
  // column again. A few passes settle it; a node that cannot be cleared stays.
  const real = cols.map((list) => list.filter((v) => v.real));
  const pad = 8;
  // The lowest and highest the curve is anywhere over x0 to x1 of the page.
  const curveSpan = (cx0: number, cy0: number, cx1: number, cy1: number, from: number, to: number): [number, number] => {
    const dx = cx1 - cx0, dy = cy1 - cy0;
    const dist = Math.sqrt(dx * dx + dy * dy);
    const off = Math.max(40, Math.min(dist * 0.5, 120));
    let lo = Infinity, hi = -Infinity;
    for (let i = 0; i <= 80; i++) {
      const t = i / 80, u = 1 - t;
      const px = u * u * u * cx0 + 3 * u * u * t * (cx0 + off) + 3 * u * t * t * (cx1 - off) + t * t * t * cx1;
      if (px < from || px > to) continue;
      const py = u * u * u * cy0 + 3 * u * u * t * cy0 + 3 * u * t * t * cy1 + t * t * t * cy1;
      lo = Math.min(lo, py);
      hi = Math.max(hi, py);
    }
    return lo === Infinity ? [cy0, cy0] : [lo, hi];
  };
  const colX = (c: number): number => c * (o.width + o.colGap) + o.width / 2;
  const separate = (list: V[]): void => {
    list.sort((a, b) => a.y - b.y);
    let floor = -Infinity;
    for (const v of list) { v.y = Math.max(v.y, floor + v.h / 2); floor = v.y + v.h / 2 + o.rowGap; }
  };
  for (let pass = 0; pass < 60; pass++) {
    let moved = false;
    for (const [a, b] of dag) {
      if (b.col - a.col < 2) continue;
      const [fy, ty] = dys.get(`${a.id}>${b.id}`) ?? [0, 0];
      const x0 = colX(a.col) + o.width / 2, x1 = colX(b.col) - o.width / 2;
      for (let c = a.col + 1; c < b.col; c++) {
        for (const v of real[c]!) {
          const [lo, hi] = curveSpan(x0, a.y + fy, x1, b.y + ty, colX(c) - o.width / 2 - pad, colX(c) + o.width / 2 + pad);
          const room = v.h / 2 + pad;
          if (v.y + room > lo && v.y - room < hi) {
            // Clear of the wire whichever side is nearer.
            v.y = v.y - lo < hi - v.y ? lo - room - 2 : hi + room + 2;
            moved = true;
          }
        }
      }
    }
    for (const list of real) separate(list);
    if (!moved) break;
  }

  const out = new Map<string, Placed>();
  let minY = Infinity;
  for (const v of all) if (v.real) minY = Math.min(minY, v.y - v.h / 2);
  for (const n of nodes) {
    const v = byId.get(n.id)!;
    out.set(n.id, { x: v.col * (o.width + o.colGap) + o.width / 2, y: v.y - minY + v.h / 2 });
  }
  return out;
}
