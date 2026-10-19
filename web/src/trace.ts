// Tracing in the editor: click a message in the debug sidebar and the canvas
// lights the path it took, with how long each node had it and what it looked
// like there. The trace itself is the runtime's; everything here asks for it
// and draws it.

import type { HopView, ReplayResult, TraceView } from './api';
import type { Graph } from './graph';

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

export interface TracePanelActions {
  pick: (nodeId: string) => void;
  makeTest: () => void;
  replay: () => void;
  clear: () => void;
}

const el = <K extends keyof HTMLElementTagNameMap>(
  tag: K, attrs: Record<string, string> = {}, ...kids: (Node | string)[]
): HTMLElementTagNameMap[K] => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') n.className = v;
    else n.setAttribute(k, v);
  }
  n.append(...kids);
  return n;
};

/** Draws a trace into the sidebar: one row a hop, in the order they happened. */
export function renderTrace(
  host: HTMLElement, trace: TraceView, focus: number | null, path: number[] | null,
  graph: Graph, actions: TracePanelActions,
): void {
  host.replaceChildren();
  const onPath = new Set(path ?? trace.hops.map((h) => h.id));
  const total = trace.hops.reduce((t, h) => t + h.nanos, 0);

  const head = el('div', { class: 'trace-head', 'data-test': 'trace-summary' },
    `${trace.hops.length} hops, ${fmtNanos(total)} in nodes`
    + (trace.truncated ? ', cut short: the trace hit its hop limit' : ''));
  const test = el('button', { class: 'ghost', 'data-test': 'trace-make-test', title: 'Make a flow test from this trace' }, 'Make test');
  test.onclick = actions.makeTest;
  const replay = el('button', { class: 'ghost', 'data-test': 'trace-replay', title: 'Put this message into the working copy and compare' }, 'Replay into draft');
  replay.onclick = actions.replay;
  const clear = el('button', { class: 'ghost', 'data-test': 'trace-clear' }, 'Clear');
  clear.onclick = actions.clear;
  host.append(head, el('div', { class: 'actions' }, test, replay, clear));

  for (const h of trace.hops) {
    const id = canvasId(h);
    const entry = graph.byId(id);
    const name = entry ? graph.label(entry) : h.node;
    const via = h.via === 'error' ? 'error' : h.parents?.length ? `port ${h.port + 1}` : h.entry === 'emit' ? 'came in' : 'pressed';
    const row = el('div', {
      class: `trace-hop${onPath.has(h.id) ? ' on-path' : ''}${h.id === focus ? ' focus' : ''}${h.error ? ' failed' : ''}`,
      'data-test': 'trace-hop', 'data-node': h.node, 'data-hop': String(h.id),
    },
      el('div', { class: 'meta' },
        el('span', { class: 'name' }, name),
        el('span', { class: 'via' }, via),
        el('span', { class: 'ns', 'data-test': 'trace-hop-time' }, fmtNanos(h.nanos))),
      el('pre', { class: 'diff-text trace-msg', 'data-test': 'trace-hop-msg' }, preview(h.msg, h.cut, 160)));
    if (h.error) row.append(el('pre', { class: 'diff-text test-problem' }, h.error));
    row.onclick = () => actions.pick(id);
    host.append(row);
  }
}

/** Draws a replay's answer into the sidebar. */
export function renderReplay(host: HTMLElement, res: ReplayResult, back: () => void): void {
  host.replaceChildren();
  if (res.problems?.length) {
    host.append(el('div', { class: 'test-summary fail', 'data-test': 'replay-summary' }, "It couldn't be replayed."));
    for (const p of res.problems) host.append(el('pre', { class: 'diff-text test-problem' }, p.message));
  } else if (res.same) {
    host.append(el('div', { class: 'test-summary pass', 'data-test': 'replay-summary' },
      'The draft sends it everywhere the original went, with the same message.'));
  } else {
    const n = res.differences?.length ?? 0;
    host.append(el('div', { class: 'test-summary fail', 'data-test': 'replay-summary' },
      `The draft differs in ${n} place${n === 1 ? '' : 's'}.`));
    for (const d of res.differences ?? []) {
      const row = el('div', { class: `test-result fail`, 'data-test': 'replay-diff', 'data-node': d.node },
        el('div', { class: 'meta' }, el('span', { class: 'kind test-fail' }, d.kind), el('span', { class: 'name' }, d.label)));
      for (const c of d.changes ?? []) row.append(el('pre', { class: 'diff-text test-problem' }, c));
      if (d.kind === 'missing') row.append(el('pre', { class: 'diff-text test-problem' }, `ended here before: ${preview(d.before, false, 160)}`));
      if (d.kind === 'extra') row.append(el('pre', { class: 'diff-text test-problem' }, `ends here now: ${preview(d.after, false, 160)}`));
      host.append(row);
    }
  }
  const b = el('button', { class: 'ghost', 'data-test': 'replay-back' }, 'Back to the original trace');
  b.onclick = back;
  host.append(b);
}
