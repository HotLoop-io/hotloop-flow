// Tracing in the editor: click a message in the debug sidebar and the canvas
// lights the path it took, with how long each node had it and what it looked
// like there. The trace itself is the runtime's; the editor asks for it and the
// Trace panel draws it.

import type { HopView, TraceView } from './api';

/** What the canvas draws for one node a trace went through. */
export interface TraceMark {
  /** Every hop at this node, including the ones inside a subflow instance. */
  hops: HopView[];
  /** On the path to the message that was clicked. */
  onPath: boolean;
  /** The hop the message was clicked at. */
  focus: boolean;
  /** Total time spent in the node across those hops. */
  nanos: number;
}

export interface TraceOverlay {
  nodes: Map<string, TraceMark>;
  /** Wires the message travelled, keyed `from>to`; true when on the path. */
  edges: Map<string, boolean>;
}

/** The node on the canvas a hop belongs to. */
export const canvasId = (h: HopView): string => h.canvas || h.node;

/**
 * Works out what to light. path is the ids of the hops on the way to the one
 * clicked; without it every hop counts as on the path, which is what a replay
 * shows, since it has no one message to point at.
 */
export function overlayFrom(trace: TraceView, focus: number | null, path: number[] | null): TraceOverlay {
  const onPath = new Set(path ?? trace.hops.map((h) => h.id));
  const o: TraceOverlay = { nodes: new Map(), edges: new Map() };
  for (const h of trace.hops) {
    const id = canvasId(h);
    const mark = o.nodes.get(id) ?? { hops: [], onPath: false, focus: false, nanos: 0 };
    mark.hops.push(h);
    mark.nanos += h.nanos;
    if (onPath.has(h.id)) mark.onPath = true;
    if (h.id === focus) mark.focus = true;
    o.nodes.set(id, mark);

    for (const p of h.parents ?? []) {
      const from = trace.hops[p];
      if (!from) continue;
      const key = `${canvasId(from)}>${id}`;
      o.edges.set(key, (o.edges.get(key) ?? false) || (onPath.has(h.id) && onPath.has(p)));
    }
  }
  return o;
}

/** 840 ns, 12 µs, 3.4 ms, 1.2 s. */
export function fmtNanos(n: number): string {
  if (n < 1_000) return `${n} ns`;
  if (n < 1_000_000) return `${(n / 1_000).toFixed(n < 10_000 ? 1 : 0)} µs`;
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 0)} ms`;
  return `${(n / 1_000_000_000).toFixed(1)} s`;
}

/** A message, short enough for a row: its payload, or the whole thing when it has none. */
export function preview(msg: Record<string, unknown> | undefined, cut?: boolean, max = 90): string {
  if (!msg) return cut ? '(too big to keep)' : '(not kept)';
  const v = 'payload' in msg ? msg.payload : msg;
  let s: string;
  try {
    s = typeof v === 'string' ? JSON.stringify(v) : JSON.stringify(v) ?? 'undefined';
  } catch {
    s = String(v);
  }
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

/** Everything a node's tooltip says about the trace going through it. */
export function tipFor(mark: TraceMark): string {
  const lines = mark.hops.slice(0, 6).map((h) =>
    `${fmtNanos(h.nanos)}${h.error ? ` failed: ${h.error}` : ''}  ${preview(h.msg, h.cut, 120)}`);
  if (mark.hops.length > 6) lines.push(`and ${mark.hops.length - 6} more`);
  return lines.join('\n');
}
