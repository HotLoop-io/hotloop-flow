// The semantic diff, turned into something the canvas can draw.
//
// The diff itself comes from the engine (GET /deployments/{a}/diff/{b} and
// POST /flows/diff), so the canvas, the API, the CLI and git all agree on what
// changed. This file only works out where to draw it: which nodes to outline,
// which removed nodes to draw as ghosts from the older document, and which
// wires appeared or went away.

import type { DiffResult } from './api';
import type { FlowEntry } from './graph';

export type DiffMark = 'added' | 'changed' | 'moved';

export interface DiffOverlay {
  /** How each surviving or new entry differs. Absent means unchanged. */
  marks: Map<string, DiffMark>;
  /** Entries that are gone, drawn faintly where they used to be. */
  ghosts: FlowEntry[];
  /** Wires that are new, keyed from:port:to. */
  addedWires: Set<string>;
  /** Wires that are gone, with both ends resolved so they can be drawn. */
  removedWires: { from: FlowEntry; port: number; to: FlowEntry }[];
  /** Tabs and subflows with anything on them that differs. */
  scopes: Set<string>;
}

export function wireKey(from: string, port: number, to: string): string {
  return `${from}:${port}:${to}`;
}

export function overlayFrom(result: DiffResult, older: FlowEntry[], newer: FlowEntry[]): DiffOverlay {
  const oldById = new Map(older.map((e) => [e.id, e]));
  const newById = new Map(newer.map((e) => [e.id, e]));
  const either = (id: string) => newById.get(id) ?? oldById.get(id);

  const o: DiffOverlay = {
    marks: new Map(), ghosts: [], addedWires: new Set(), removedWires: [], scopes: new Set(),
  };

  for (const e of result.entries) {
    if (e.z) o.scopes.add(e.z);
    if (e.kind === 'removed') {
      const ghost = oldById.get(e.id);
      if (!ghost) continue;
      o.ghosts.push(ghost);
      // A removed node takes its wires with it. Draw those too, or the ghost
      // floats there unconnected and the picture lies about what went away.
      ghost.wires?.forEach((targets, port) => {
        for (const to of targets) {
          const target = either(to);
          if (target) o.removedWires.push({ from: ghost, port, to: target });
        }
      });
      continue;
    }
    o.marks.set(e.id, e.kind);
    for (const w of e.wires ?? []) {
      if (w.added) {
        o.addedWires.add(wireKey(e.id, w.port, w.to));
        continue;
      }
      const from = either(e.id);
      const to = either(w.to);
      if (from && to) o.removedWires.push({ from, port: w.port, to });
    }
  }
  return o;
}

/** "2 added, 1 changed", or "No changes". */
export function summarise(result: DiffResult): string {
  const s = result.summary;
  const parts: string[] = [];
  if (s.added) parts.push(`${s.added} added`);
  if (s.removed) parts.push(`${s.removed} removed`);
  if (s.changed) parts.push(`${s.changed} changed`);
  if (s.moved) parts.push(`${s.moved} moved`);
  return parts.length ? parts.join(', ') : 'No changes';
}
