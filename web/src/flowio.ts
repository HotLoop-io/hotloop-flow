// Moving flows in and out of the editor: what Copy and Cut put on the
// clipboard, what Paste and Import take off it, and what Export shows.
//
// The format is the flow file itself, an array of entries, because that is what
// every other flow tool reads and writes. A selection exported here pastes into
// another runtime and a selection exported there pastes here. Nothing in this
// file touches the page: it takes a graph and returns entries, or the other way
// round, so the rules are in one place and the same for the clipboard, the
// Import dialog and a dropped file.

import { type FlowEntry, type Graph, type Point, NODE_HEIGHT, NODE_WIDTH, SUBFLOW_PREFIX, generateId, parsePortId } from './graph';

export type ExportScope = 'selection' | 'flow' | 'all';

/** Keys that are about structure, never an id held in a setting. */
const STRUCTURAL = new Set(['id', 'type', 'z', 'g', 'x', 'y', 'w', 'h', 'wires', 'name', 'info', 'in', 'out']);

/**
 * The entries for a scope, ready to write out.
 *
 * Wires and group membership are cut at the edge of what is exported, so the
 * result is a flow that stands alone. Anything the entries point at that has to
 * come along does: the configuration nodes they use, the subflow templates they
 * place, and what is inside those. Credentials never leave: a password does not
 * belong in a clipboard, a chat window or a pasted bug report.
 */
export function exportEntries(graph: Graph, scope: ExportScope): FlowEntry[] {
  const all = graph.all();
  const picked = new Set<string>();

  if (scope === 'all') {
    for (const e of all) picked.add(e.id);
  } else if (scope === 'flow') {
    const tab = graph.activeTab;
    for (const e of all) if (e.id === tab || e.z === tab) picked.add(e.id);
  } else {
    const ids = [...graph.selection].filter((id) => !parsePortId(id));
    // A selected group is its whole contents.
    const stack = [...ids];
    while (stack.length > 0) {
      const id = stack.pop()!;
      if (picked.has(id)) continue;
      picked.add(id);
      if (graph.isGroup(id)) for (const e of all) if (e.g === id) stack.push(e.id);
    }
  }

  pullInDependencies(graph, picked);

  const out: FlowEntry[] = [];
  for (const e of all) {
    if (!picked.has(e.id)) continue;
    const copy = structuredClone(e) as FlowEntry;
    delete copy.credentials;
    if (copy.wires) copy.wires = copy.wires.map((port) => port.filter((id) => picked.has(id)));
    if (copy.g && !picked.has(copy.g)) delete copy.g;
    if (copy.type === 'group' && Array.isArray(copy.nodes)) {
      copy.nodes = (copy.nodes as string[]).filter((id) => picked.has(id));
    }
    out.push(copy);
  }
  return out;
}

/** Adds the configuration nodes and subflow templates the picked entries need. */
function pullInDependencies(graph: Graph, picked: Set<string>): void {
  const all = graph.all();
  const byId = new Map(all.map((e) => [e.id, e]));
  const queue = [...picked];
  while (queue.length > 0) {
    const e = byId.get(queue.pop()!);
    if (!e) continue;
    const add = (id: string) => {
      if (picked.has(id) || !byId.has(id)) return;
      picked.add(id);
      queue.push(id);
    };

    // A placed subflow brings its template, and the template its contents.
    if (e.type.startsWith(SUBFLOW_PREFIX)) {
      const tpl = e.type.slice(SUBFLOW_PREFIX.length);
      add(tpl);
    }
    if (e.type === 'subflow') for (const m of all) if (m.z === e.id) add(m.id);

    // A setting that names a configuration node.
    const d = graph.descriptor(e.type);
    for (const p of d?.props ?? []) {
      if (p.kind !== 'configRef') continue;
      const v = e[p.name];
      if (typeof v === 'string' && v) {
        const target = byId.get(v);
        if (target && graph.isConfig(target)) add(v);
      }
    }
  }
}

/** The text that goes on the clipboard or in the Export box. */
export function toText(entries: FlowEntry[]): string {
  return JSON.stringify(entries, null, 2);
}

export interface Parsed {
  entries?: FlowEntry[];
  error?: string;
}

/** Reads pasted or chosen text as flow entries, or says exactly what is wrong. */
export function parseEntries(text: string): Parsed {
  const trimmed = text.trim();
  if (trimmed === '') return { error: 'There is nothing to import.' };
  let value: unknown;
  try {
    value = JSON.parse(trimmed);
  } catch (ex) {
    return { error: `That is not JSON: ${(ex as Error).message}` };
  }
  const list = Array.isArray(value) ? value : [value];
  if (list.length === 0) return { error: 'That is an empty list.' };

  const seen = new Set<string>();
  for (let i = 0; i < list.length; i++) {
    const e = list[i] as Record<string, unknown> | null;
    if (typeof e !== 'object' || e === null || Array.isArray(e)) {
      return { error: `Entry ${i + 1} is not an object.` };
    }
    if (typeof e.id !== 'string' || e.id === '') return { error: `Entry ${i + 1} has no id.` };
    if (typeof e.type !== 'string' || e.type === '') return { error: `Entry ${i + 1} (${e.id}) has no type.` };
    if (seen.has(e.id)) return { error: `The id ${e.id} appears twice.` };
    seen.add(e.id);
    if (e.wires !== undefined && !(Array.isArray(e.wires) && e.wires.every((w) => Array.isArray(w)))) {
      return { error: `Entry ${i + 1} (${e.id}) has wires that are not a list of lists.` };
    }
  }
  return { entries: list as FlowEntry[] };
}

export interface ImportOptions {
  /** The flow or subflow canvas that loose nodes land on. */
  scope: string;
  /** Where the top left of what is pasted should go. Without it, a little down and right of where it was. */
  at?: Point;
  /** Put everything on a flow of its own, named this, instead of on `scope`. */
  intoNewFlow?: string;
}

export interface ImportResult {
  /** The ids that are now on the canvas, and are the new selection. */
  added: string[];
  nodes: number;
  groups: number;
  flows: number;
  subflows: number;
  /** Config nodes the graph already had, which the pasted nodes now share. */
  reused: number;
  /** Node types this runtime does not have, which will show grey. */
  unknown: string[];
  /** The flow or subflow to show afterwards. */
  show: string;
}

/**
 * Puts entries into the graph under fresh ids.
 *
 * Every id is new, so pasting twice gives two copies and importing a file
 * twice never collides. The exception is a configuration node the graph
 * already has under the same id: that is the same broker, and a second copy of
 * it would open a second connection to something that counts them, so the
 * pasted nodes share the one already here.
 */
export function importEntries(graph: Graph, entries: FlowEntry[], opts: ImportOptions): ImportResult {
  const existing = new Map(graph.all().map((e) => [e.id, e]));
  const incoming = structuredClone(entries) as FlowEntry[];
  const present = new Set(incoming.map((e) => e.id));

  const idMap = new Map<string, string>();
  let reused = 0;
  const skipped = new Set<string>();
  for (const e of incoming) {
    const have = existing.get(e.id);
    if (have && have.type === e.type && graph.isConfig(e) && graph.isConfig(have)) {
      idMap.set(e.id, e.id);
      skipped.add(e.id);
      reused++;
      continue;
    }
    idMap.set(e.id, generateId());
  }
  const mapId = (id: unknown): string | undefined => (typeof id === 'string' ? idMap.get(id) : undefined);

  const fresh = incoming.filter((e) => !skipped.has(e.id));
  const hasTabs = fresh.some((e) => e.type === 'tab');

  // The flow the loose nodes land on.
  let landing = opts.scope;
  let createdTab: FlowEntry | null = null;
  if (opts.intoNewFlow !== undefined) {
    createdTab = { id: generateId(), type: 'tab', label: opts.intoNewFlow || 'Imported flow', disabled: false, info: '' };
    landing = createdTab.id;
  }

  // Where it goes: shift the lot so its top left is at the asked-for point,
  // unless it brings its own flows, in which case it keeps its own layout.
  let dx = 20, dy = 20;
  if (!hasTabs && !createdTab) {
    const box = boundsOf(fresh);
    if (box && opts.at) { dx = opts.at.x - box.minX; dy = opts.at.y - box.minY; }
  } else {
    dx = 0; dy = 0;
  }

  // An instance whose template is in the same paste is not unknown, it just
  // is not in the graph yet.
  const arriving = new Set(fresh.filter((t) => t.type === 'subflow').map((t) => SUBFLOW_PREFIX + idMap.get(t.id)!));
  const comesWithItsTemplate = (type: string): boolean => arriving.has(type);

  const out: FlowEntry[] = [];
  let nodes = 0, groups = 0, flows = 0, subflows = 0;
  const unknown = new Set<string>();
  const added: string[] = [];

  for (const e of fresh) {
    e.id = idMap.get(e.id)!;

    if (e.type === 'tab') {
      flows++;
      out.push(e);
      continue;
    }

    // Home: a flow in the pasted set, else the flow being pasted onto.
    const home = mapId(e.z);
    if (e.type === 'subflow') {
      subflows++;
      delete e.z;
    } else if (home) {
      e.z = home;
    } else if (graph.isConfig(e) && !createdTab) {
      // A configuration node that was global stays global.
      delete e.z;
    } else {
      e.z = landing;
    }

    const parent = mapId(e.g);
    if (parent) e.g = parent; else delete e.g;

    if (e.type.startsWith(SUBFLOW_PREFIX)) {
      const tpl = e.type.slice(SUBFLOW_PREFIX.length);
      if (present.has(tpl)) e.type = SUBFLOW_PREFIX + idMap.get(tpl)!;
    }

    if (e.type === 'subflow') {
      for (const kind of ['in', 'out'] as const) {
        const ports = e[kind];
        if (!Array.isArray(ports)) continue;
        for (const p of ports as { wires?: { id: string }[] }[]) {
          p.wires = (p.wires ?? []).map((w) => ({ ...w, id: mapId(w.id) ?? '' })).filter((w) => w.id !== '');
        }
      }
    }

    if (e.wires) {
      e.wires = e.wires.map((port) => port.map((id) => mapId(id)).filter((id): id is string => id !== undefined));
    }
    if (e.type === 'group') {
      groups++;
      e.nodes = ((e.nodes as string[] | undefined) ?? []).map((id) => mapId(id)).filter((id): id is string => id !== undefined);
    }

    // Any other setting that holds an id of something that came along: a
    // configuration node, a link target, the nodes a Catch is scoped to.
    for (const [k, v] of Object.entries(e)) {
      if (STRUCTURAL.has(k) || k === 'nodes' || k === 'credentials') continue;
      if (typeof v === 'string') {
        const m = mapId(v);
        if (m && v.length >= 8) e[k] = m;
      } else if (Array.isArray(v) && v.every((x) => typeof x === 'string')) {
        e[k] = (v as string[]).map((x) => (x.length >= 8 ? mapId(x) ?? x : x));
      }
    }

    if (e.x !== undefined) e.x = Number(e.x) + dx;
    if (e.y !== undefined) e.y = Number(e.y) + dy;

    if (e.type !== 'group' && e.type !== 'subflow' && !graph.descriptor(e.type) && !comesWithItsTemplate(e.type)) {
      unknown.add(e.type);
    }
    if (e.type !== 'group' && e.type !== 'subflow') nodes++;
    out.push(e);
  }

  if (createdTab) {
    flows++;
    out.unshift(createdTab);
  }

  // A group that arrives inside a pasted set keeps its members: only the top of
  // each tree is selected afterwards, so a second paste does not nest.
  for (const e of out) {
    if (e.type === 'tab' || e.type === 'subflow') continue;
    if (graph.isConfig(e)) continue;
    if (!e.g) added.push(e.id);
  }

  graph.insert(out, 'import');

  const show = out.find((e) => e.type === 'tab')?.id ?? landing;
  return { added, nodes, groups, flows, subflows, reused, unknown: [...unknown].sort(), show };
}

function boundsOf(entries: FlowEntry[]): { minX: number; minY: number } | null {
  let minX = Infinity, minY = Infinity;
  for (const e of entries) {
    if (e.type === 'tab' || e.type === 'subflow' || e.x === undefined || e.y === undefined) continue;
    const x = Number(e.x), y = Number(e.y);
    if (!Number.isFinite(x) || !Number.isFinite(y)) continue;
    if (e.type === 'group') {
      minX = Math.min(minX, x); minY = Math.min(minY, y);
    } else {
      minX = Math.min(minX, x - NODE_WIDTH / 2);
      minY = Math.min(minY, y - NODE_HEIGHT / 2);
    }
  }
  return minX === Infinity ? null : { minX, minY };
}

/** One line saying what an import did, for the screen and for the tests. */
export function describeImport(r: ImportResult): string {
  const parts: string[] = [];
  if (r.nodes) parts.push(`${r.nodes} node${r.nodes === 1 ? '' : 's'}`);
  if (r.groups) parts.push(`${r.groups} group${r.groups === 1 ? '' : 's'}`);
  if (r.flows) parts.push(`${r.flows} flow${r.flows === 1 ? '' : 's'}`);
  if (r.subflows) parts.push(`${r.subflows} subflow${r.subflows === 1 ? '' : 's'}`);
  let s = parts.length ? `Added ${parts.join(', ')}.` : 'Nothing to add.';
  if (r.reused) s += ` ${r.reused} configuration node${r.reused === 1 ? ' was' : 's were'} already here and shared.`;
  if (r.unknown.length) s += ` Not in this runtime, shown grey: ${r.unknown.join(', ')}.`;
  return s;
}
