// The flow graph model.
//
// Holds the Node-RED v1 entries the editor is working on, plus the selection and
// undo history. Deliberately separate from rendering: the canvas reads this and
// draws, and every mutation goes through here so undo is a single mechanism
// rather than something each interaction has to remember to do.

import type { Descriptor } from './api';

/** Pseudo node types for a subflow template's own input and output ports. */
export const PORT_IN = '@subflow-in';
export const PORT_OUT = '@subflow-out';
export const SUBFLOW_PREFIX = 'subflow:';

/**
 * The two flow-file keys behind a durable wire, on the node the wire leaves.
 * ew_durable lists the ids of the nodes its durable wires go to; the runtime
 * reads it as wiring, so changing it never restarts the node.
 * ew_durable_attempts maps one of those ids to how many times the target may
 * refuse a message before the wire gives up on it.
 */
export const DURABLE_KEY = 'ew_durable';
export const DURABLE_ATTEMPTS_KEY = 'ew_durable_attempts';

const PORT_ID = /^@(in|out):(.+):([0-9]+)$/;

/** The id the editor gives a template's port, which exists nowhere in the file. */
export const portId = (kind: 'in' | 'out', subflow: string, n: number): string => `@${kind}:${subflow}:${n}`;

export function parsePortId(id: string): { kind: 'in' | 'out'; subflow: string; n: number } | null {
  const m = PORT_ID.exec(id);
  return m ? { kind: m[1] as 'in' | 'out', subflow: m[2]!, n: Number(m[3]) } : null;
}

function portDescriptor(type: string): Descriptor {
  const input = type === PORT_IN;
  return {
    type, category: 'subflows', color: '#c9c9c9', icon: 'port',
    inputs: input ? 0 : 1, outputs: input ? 1 : 0,
    compatibility: { level: 'full' },
  };
}

function instanceDescriptor(type: string, tpl: FlowEntry): Descriptor {
  const inputs = Array.isArray(tpl.in) ? (tpl.in as unknown[]).length : 0;
  const outputs = Array.isArray(tpl.out) ? (tpl.out as unknown[]).length : 0;
  return {
    type, category: 'subflows',
    color: typeof tpl.color === 'string' && tpl.color ? tpl.color : '#DDAA99',
    icon: 'subflow', inputs, outputs,
    paletteLabel: typeof tpl.name === 'string' && tpl.name ? tpl.name : 'subflow',
    labelProp: 'name',
    compatibility: { level: 'full' },
  };
}

/** A flow-file entry. Unknown properties are preserved verbatim. */
export interface FlowEntry {
  id: string;
  type: string;
  z?: string;
  g?: string;
  name?: string;
  x?: number;
  y?: number;
  wires?: string[][];
  d?: boolean;
  [key: string]: unknown;
}

export interface Point { x: number; y: number; }

/** One input or output of a subflow template, as the flow file stores it. */
export interface SubflowPortDef {
  x?: number;
  y?: number;
  wires?: { id: string; port?: number }[];
}

/** Canvas geometry. Node-RED's proportions, because they are well judged. */
export const NODE_WIDTH = 130;
export const NODE_HEIGHT = 34;
export const PORT_RADIUS = 5;
export const GRID = 10;
/** Room a group leaves around what it holds, and for its label above it. */
export const GROUP_PAD = 16;
export const GROUP_HEAD = 22;

/** Where an output port sits on a node. */
export function outputPort(node: FlowEntry, port: number, total: number): Point {
  const x = (node.x ?? 0) + NODE_WIDTH / 2;
  const y = (node.y ?? 0) + portOffset(port, total);
  return { x, y };
}

/** Where the input port sits. A node has at most one. */
export function inputPort(node: FlowEntry): Point {
  return { x: (node.x ?? 0) - NODE_WIDTH / 2, y: node.y ?? 0 };
}

/**
 * Vertical offset of one port when a node has several.
 *
 * Ports are spread evenly around the node's centre so a three-output Switch
 * reads symmetrically rather than hanging off the top edge.
 */
function portOffset(port: number, total: number): number {
  if (total <= 1) return 0;
  const spacing = Math.min(NODE_HEIGHT / total, 13);
  return (port - (total - 1) / 2) * spacing;
}

/** Snaps a coordinate to the grid. */
export function snap(v: number): number {
  return Math.round(v / GRID) * GRID;
}

/**
 * The wire path between two points.
 *
 * A cubic bézier whose control points push horizontally, so a wire leaves a
 * node's output travelling right and arrives at an input travelling right —
 * which is what makes a graph readable when nodes are stacked vertically. The
 * control offset grows with distance and is clamped, so a short backward wire
 * still bows out enough to be followed rather than collapsing into a straight
 * line through the node it came from.
 */
export function wirePath(from: Point, to: Point): string {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const dist = Math.sqrt(dx * dx + dy * dy);

  let offset = Math.max(40, Math.min(dist * 0.5, 120));
  // Wiring backwards is normal — a loop, a retry path — and needs a wider bow
  // or the curve doubles back through both nodes.
  if (dx < 0) offset = Math.max(offset, Math.abs(dx) * 0.6 + 60);

  return `M ${from.x} ${from.y} C ${from.x + offset} ${from.y}, ${to.x - offset} ${to.y}, ${to.x} ${to.y}`;
}

/** One undoable change. */
interface Snapshot {
  entries: FlowEntry[];
  label: string;
}

export type ChangeListener = () => void;

/** The editor's working copy of a flow set. */
export class Graph {
  private entries: FlowEntry[] = [];
  private undoStack: Snapshot[] = [];
  private redoStack: Snapshot[] = [];
  private listeners: ChangeListener[] = [];

  /** The revision the entries were loaded at, for deploy conflict detection. */
  rev = '';
  /** Whether there are unsaved changes. */
  dirty = false;
  /** Selected entry ids. */
  selection = new Set<string>();
  /** The tab currently being edited. */
  activeTab = '';

  constructor(private readonly descriptors: Map<string, Descriptor>) {}

  /**
   * A node type's descriptor. Runtime types come from GET /nodes; the two port
   * pseudo-types and every subflow instance type are made here, because the
   * runtime reserves the subflow prefix and never lists them.
   */
  private desc(type: string): Descriptor | undefined {
    const known = this.descriptors.get(type);
    if (known) return known;
    if (type === PORT_IN || type === PORT_OUT) return portDescriptor(type);
    const tpl = this.subflowOfType(type);
    return tpl ? instanceDescriptor(type, tpl) : undefined;
  }

  onChange(fn: ChangeListener): void {
    this.listeners.push(fn);
  }

  private emit(): void {
    for (const fn of this.listeners) fn();
  }

  load(entries: FlowEntry[], rev: string): void {
    // Deep-copied on load so that editing never mutates what was fetched, which
    // is what lets a failed deploy be retried against the original.
    this.entries = structuredClone(entries);
    this.rev = rev;
    this.dirty = false;
    this.undoStack = [];
    this.redoStack = [];
    this.selection.clear();
    if (!this.activeTab || !this.tabs().some((t) => t.id === this.activeTab)) {
      this.activeTab = this.tabs()[0]?.id ?? '';
    }
    this.emit();
  }

  all(): FlowEntry[] { return this.entries; }

  tabs(): FlowEntry[] { return this.entries.filter((e) => e.type === 'tab'); }

  /** Nodes on a tab, excluding config nodes and structural entries. */
  nodesOn(tabId: string): FlowEntry[] {
    return this.entries.filter(
      (e) => e.z === tabId && e.type !== 'tab' && e.type !== 'subflow' && e.type !== 'group'
        && !this.isConfig(e),
    );
  }

  /**
   * Config nodes: referenced by id from other nodes rather than wired in.
   *
   * Detected structurally — no x, no y, no wires — which is the same rule the
   * runtime's parser applies, so the editor and the runtime never disagree
   * about what a node is.
   */
  isConfig(e: FlowEntry): boolean {
    if (e.type === 'tab' || e.type === 'subflow' || e.type === 'group') return false;
    const d = this.desc(e.type);
    if (d?.isConfig) return true;
    return e.x === undefined && e.y === undefined && e.wires === undefined;
  }

  byId(id: string): FlowEntry | undefined {
    const found = this.entries.find((e) => e.id === id);
    if (found) return found;
    const port = parsePortId(id);
    return port ? this.portNode(port.kind, port.subflow, port.n) : undefined;
  }

  /**
   * A subflow template's port as something the canvas can draw and wire. It is
   * a view made from the template each time it is asked for, so changing it
   * means changing the template: see moveSelection, connect and disconnect.
   */
  private portNode(kind: 'in' | 'out', subflow: string, n: number): FlowEntry | undefined {
    const tpl = this.entries.find((e) => e.id === subflow && e.type === 'subflow');
    const port = (tpl?.[kind] as SubflowPortDef[] | undefined)?.[n];
    if (!tpl || !port) return undefined;
    return {
      id: portId(kind, subflow, n),
      type: kind === 'in' ? PORT_IN : PORT_OUT,
      z: subflow,
      name: kind === 'in' ? 'input' : (((tpl.out as SubflowPortDef[]).length > 1) ? `output ${n + 1}` : 'output'),
      x: port.x ?? 0,
      y: port.y ?? 0,
      wires: kind === 'in' ? [(port.wires ?? []).map((w) => w.id)] : [],
    };
  }

  /** Template ports on a subflow's own canvas. */
  portNodes(scope: string): FlowEntry[] {
    const tpl = this.entries.find((e) => e.id === scope && e.type === 'subflow');
    if (!tpl) return [];
    const out: FlowEntry[] = [];
    ((tpl.in as SubflowPortDef[] | undefined) ?? []).forEach((_, n) => out.push(this.portNode('in', scope, n)!));
    ((tpl.out as SubflowPortDef[] | undefined) ?? []).forEach((_, n) => out.push(this.portNode('out', scope, n)!));
    return out;
  }

  /** Everything the canvas draws as a node on a scope: its nodes, plus ports on a subflow. */
  visibleNodes(scope: string): FlowEntry[] {
    return [...this.nodesOn(scope), ...this.portNodes(scope)];
  }

  // ── Subflows and groups: what there is ─────────────────────────────────────

  /** Subflow templates. They are edited on a canvas of their own, like a flow. */
  subflows(): FlowEntry[] { return this.entries.filter((e) => e.type === 'subflow'); }

  /** Whether the id is a flow or a subflow, the two things that hold a canvas. */
  isScope(id: string): boolean {
    return this.entries.some((e) => e.id === id && (e.type === 'tab' || e.type === 'subflow'));
  }

  isSubflowScope(id: string): boolean {
    return this.entries.some((e) => e.id === id && e.type === 'subflow');
  }

  /** The template an instance type points at. */
  subflowOfType(type: string): FlowEntry | undefined {
    if (!type.startsWith(SUBFLOW_PREFIX)) return undefined;
    const id = type.slice(SUBFLOW_PREFIX.length);
    return this.entries.find((e) => e.id === id && e.type === 'subflow');
  }

  instancesOf(subflowId: string): FlowEntry[] {
    return this.entries.filter((e) => e.type === SUBFLOW_PREFIX + subflowId);
  }

  groupsOn(scope: string): FlowEntry[] {
    return this.entries.filter((e) => e.type === 'group' && e.z === scope);
  }

  isGroup(id: string): boolean {
    return this.entries.some((e) => e.id === id && e.type === 'group');
  }

  descriptor(type: string): Descriptor | undefined {
    return this.desc(type);
  }

  /** How many outputs a node has, honouring a configurable count. */
  outputCount(e: FlowEntry): number {
    const d = this.desc(e.type);
    if (d?.outputsProp) {
      const v = e[d.outputsProp];
      const n = typeof v === 'number' ? v : Number(v);
      if (Number.isFinite(n) && n >= 0) return n;
    }
    if (e.wires) return e.wires.length;
    return d?.outputs ?? 0;
  }

  inputCount(e: FlowEntry): number {
    return this.desc(e.type)?.inputs ?? 0;
  }

  /** The label drawn on a node. */
  label(e: FlowEntry): string {
    if (e.name) return e.name;
    const d = this.desc(e.type);
    if (d?.labelProp) {
      const v = e[d.labelProp];
      if (typeof v === 'string' && v) return v;
    }
    return d?.paletteLabel ?? e.type;
  }

  // ── Mutation ───────────────────────────────────────────────────────────────

  /**
   * Records the current state before a change.
   *
   * Called explicitly at the start of an interaction rather than after each
   * mutation, so dragging twenty nodes is one undo step instead of twenty.
   */
  checkpoint(label: string): void {
    this.undoStack.push({ entries: structuredClone(this.entries), label });
    // Bounded: an editor left open for a day should not hold every state it
    // ever passed through.
    if (this.undoStack.length > 100) this.undoStack.shift();
    this.redoStack = [];
  }

  /** Marks the graph changed and notifies listeners. */
  touch(): void {
    this.dirty = true;
    this.emit();
  }

  undo(): boolean {
    const snap = this.undoStack.pop();
    if (!snap) return false;
    this.redoStack.push({ entries: structuredClone(this.entries), label: snap.label });
    this.entries = snap.entries;
    this.pruneSelection();
    this.dirty = true;
    this.emit();
    return true;
  }

  redo(): boolean {
    const snap = this.redoStack.pop();
    if (!snap) return false;
    this.undoStack.push({ entries: structuredClone(this.entries), label: snap.label });
    this.entries = snap.entries;
    this.pruneSelection();
    this.dirty = true;
    this.emit();
    return true;
  }

  canUndo(): boolean { return this.undoStack.length > 0; }
  canRedo(): boolean { return this.redoStack.length > 0; }

  private pruneSelection(): void {
    for (const id of [...this.selection]) {
      if (!this.byId(id)) this.selection.delete(id);
    }
  }

  addNode(type: string, at: Point): FlowEntry {
    this.checkpoint('add node');
    const d = this.desc(type);

    const entry: FlowEntry = {
      id: generateId(),
      type,
      z: this.activeTab,
      x: snap(at.x),
      y: snap(at.y),
      wires: Array.from({ length: d?.outputs ?? 0 }, () => []),
    };

    // Seed declared defaults so a freshly dropped node behaves as its dialog
    // says it will, rather than only after somebody opens and saves it.
    for (const p of d?.props ?? []) {
      if (p.default !== undefined) entry[p.name] = p.default;
    }

    this.entries.push(entry);
    this.selection = new Set([entry.id]);
    this.touch();
    return entry;
  }

  deleteSelection(): void {
    // A template's ports are part of its shape, changed in its properties and
    // not by Delete, and a group takes what is in it down with it.
    const doomed = this.withGroupMembers([...this.selection].filter((id) => !parsePortId(id)));
    if (doomed.size === 0) return;
    this.checkpoint('delete');
    const scopes = this.entries.filter((e) => doomed.has(e.id)).map((e) => e.z ?? '');
    this.entries = this.entries.filter((e) => !doomed.has(e.id));
    this.dropDeadReferences(doomed);
    this.selection.clear();
    this.refreshGroups(scopes);
    this.touch();
  }

  /**
   * The ids of the selection with every group's contents added, groups
   * included. What moving or deleting a group has to act on.
   */
  private withGroupMembers(ids: string[]): Set<string> {
    const out = new Set<string>();
    const add = (id: string) => {
      if (out.has(id)) return;
      out.add(id);
      if (this.isGroup(id)) {
        for (const e of this.entries) if (e.g === id) add(e.id);
      }
    };
    ids.forEach(add);
    return out;
  }

  /**
   * Takes deleted ids out of every list that names them. A wire left pointing
   * at something deleted is a flow file the runtime warns about on every load.
   */
  private dropDeadReferences(doomed: Set<string>): void {
    for (const e of this.entries) {
      if (e.wires) e.wires = e.wires.map((port) => port.filter((id) => !doomed.has(id)));
      if (e.g && doomed.has(e.g)) delete e.g;
      if (e.type === 'group' && Array.isArray(e.nodes)) {
        e.nodes = (e.nodes as string[]).filter((id) => !doomed.has(id));
      }
      if (e.type === 'subflow') {
        for (const kind of ['in', 'out'] as const) {
          for (const p of (e[kind] as SubflowPortDef[] | undefined) ?? []) {
            p.wires = (p.wires ?? []).filter((w) => !doomed.has(w.id));
          }
        }
      }
      // Link nodes name each other across flows, and a durable wire names the
      // node it goes to. Both are lists of ids that outlive what they name.
      if (Array.isArray(e.links)) e.links = (e.links as string[]).filter((id) => !doomed.has(id));
      this.forgetDurable(e, (id) => doomed.has(id));
    }
  }

  /** Takes the durable marks off the wires that go to nodes `gone` says are gone. */
  private forgetDurable(e: FlowEntry, gone: (id: string) => boolean): void {
    if (Array.isArray(e[DURABLE_KEY])) {
      const kept = (e[DURABLE_KEY] as string[]).filter((id) => !gone(id));
      if (kept.length > 0) e[DURABLE_KEY] = kept; else delete e[DURABLE_KEY];
    }
    const limits = e[DURABLE_ATTEMPTS_KEY];
    if (limits && typeof limits === 'object') {
      const kept = Object.fromEntries(Object.entries(limits as Record<string, unknown>).filter(([id]) => !gone(id)));
      if (Object.keys(kept).length > 0) e[DURABLE_ATTEMPTS_KEY] = kept; else delete e[DURABLE_ATTEMPTS_KEY];
    }
  }

  moveSelection(dx: number, dy: number): void {
    for (const id of this.withGroupMembers([...this.selection])) this.shift(id, dx, dy);
    this.touch();
  }

  /** Moves one thing: a node, a group's box, or a template's port. */
  private shift(id: string, dx: number, dy: number): void {
    const port = parsePortId(id);
    if (port) {
      const p = this.portDef(port.subflow, port.kind, port.n);
      if (p) { p.x = (p.x ?? 0) + dx; p.y = (p.y ?? 0) + dy; }
      return;
    }
    const e = this.byId(id);
    if (!e || e.x === undefined || e.y === undefined) return;
    e.x += dx;
    e.y += dy;
  }

  snapSelection(): void {
    for (const id of this.withGroupMembers([...this.selection])) {
      const port = parsePortId(id);
      if (port) {
        const p = this.portDef(port.subflow, port.kind, port.n);
        if (p) { p.x = snap(p.x ?? 0); p.y = snap(p.y ?? 0); }
        continue;
      }
      const e = this.byId(id);
      if (!e || e.x === undefined || e.y === undefined) continue;
      // A group's box is worked out from what is in it, not snapped.
      if (e.type === 'group') continue;
      e.x = snap(e.x);
      e.y = snap(e.y);
    }
    this.touch();
  }

  /** Connects an output port to a node's input. */
  connect(fromId: string, port: number, toId: string): boolean {
    const from = this.byId(fromId);
    const to = this.byId(toId);
    if (!from || !to || fromId === toId) return false;
    if (this.inputCount(to) === 0) return false;
    if (from.type === 'group' || to.type === 'group') return false;
    // A template's input cannot be wired straight to its output: the file has
    // no way to say it, and a subflow that does nothing is not a subflow.
    if (from.type === PORT_IN && to.type === PORT_OUT) return false;
    if (from.type === PORT_IN || to.type === PORT_OUT) return this.connectPort(from, port, to);

    this.checkpoint('connect');
    if (!from.wires) from.wires = [];
    while (from.wires.length <= port) from.wires.push([]);

    const existing = from.wires[port]!;
    // A duplicate wire would deliver the message twice, which is almost never
    // what somebody dragging a second time intended.
    if (existing.includes(toId)) return false;
    existing.push(toId);
    this.touch();
    return true;
  }

  /** Wiring that touches a template's port is stored on the template. */
  private connectPort(from: FlowEntry, port: number, to: FlowEntry): boolean {
    if (from.type === PORT_IN) {
      const p = parsePortId(from.id)!;
      const def = this.portDef(p.subflow, 'in', p.n);
      if (!def) return false;
      if (!def.wires) def.wires = [];
      if (def.wires.some((w) => w.id === to.id)) return false;
      this.checkpoint('connect');
      def.wires.push({ id: to.id });
      this.touch();
      return true;
    }
    const p = parsePortId(to.id)!;
    const def = this.portDef(p.subflow, 'out', p.n);
    if (!def) return false;
    if (!def.wires) def.wires = [];
    if (def.wires.some((w) => w.id === from.id && (w.port ?? 0) === port)) return false;
    this.checkpoint('connect');
    def.wires.push({ id: from.id, port });
    this.touch();
    return true;
  }

  private portDef(subflow: string, kind: 'in' | 'out', n: number): SubflowPortDef | undefined {
    const tpl = this.entries.find((e) => e.id === subflow && e.type === 'subflow');
    return (tpl?.[kind] as SubflowPortDef[] | undefined)?.[n];
  }

  disconnect(fromId: string, port: number, toId: string): void {
    const fromPort = parsePortId(fromId);
    const toPort = parsePortId(toId);
    if (fromPort?.kind === 'in') {
      const def = this.portDef(fromPort.subflow, 'in', fromPort.n);
      if (!def?.wires) return;
      this.checkpoint('disconnect');
      def.wires = def.wires.filter((w) => w.id !== toId);
      this.touch();
      return;
    }
    if (toPort?.kind === 'out') {
      const def = this.portDef(toPort.subflow, 'out', toPort.n);
      if (!def?.wires) return;
      this.checkpoint('disconnect');
      def.wires = def.wires.filter((w) => !(w.id === fromId && (w.port ?? 0) === port));
      this.touch();
      return;
    }
    const from = this.byId(fromId);
    if (!from?.wires?.[port]) return;
    this.checkpoint('disconnect');
    from.wires[port] = from.wires[port]!.filter((id) => id !== toId);
    // The durable mark is on the pair of nodes, so it goes with the last wire
    // between them and not before.
    if (!from.wires.some((targets) => targets.includes(toId))) this.forgetDurable(from, (id) => id === toId);
    this.touch();
  }

  /** Applies edited properties to a node. */
  updateNode(id: string, props: Record<string, unknown>): void {
    const e = this.byId(id);
    if (!e) return;
    this.checkpoint('edit node');
    Object.assign(e, props);

    // A configuration node has no wires, and giving it an empty list would
    // make the runtime read it as a node. Only a node that is wired keeps its
    // wires in step.
    if (this.isConfig(e)) { this.touch(); return; }

    // Keep the wires array in step with a changed output count, or the node
    // renders ports it cannot send on.
    const outputs = this.outputCount(e);
    if (!e.wires) e.wires = [];
    while (e.wires.length < outputs) e.wires.push([]);
    if (e.wires.length > outputs) e.wires = e.wires.slice(0, outputs);

    this.touch();
  }

  addTab(label: string): FlowEntry {
    this.checkpoint('add flow');
    const tab: FlowEntry = { id: generateId(), type: 'tab', label, disabled: false, info: '' };
    this.entries.push(tab);
    this.activeTab = tab.id;
    this.touch();
    return tab;
  }

  // ── Configuration nodes ────────────────────────────────────────────────────

  /**
   * Every configuration node, in file order. They have no place on a canvas, so
   * this is the only list of them there is.
   */
  configNodes(): FlowEntry[] {
    return this.entries.filter((e) => e.type !== 'tab' && e.type !== 'subflow' && e.type !== 'group'
      && !e.type.startsWith(SUBFLOW_PREFIX) && this.isConfig(e));
  }

  /**
   * Adds a configuration node: shared by id, wired to nothing, on no flow. It
   * carries no x, y or wires, which is how the runtime tells it from a node.
   */
  addConfig(type: string, props: Record<string, unknown>): FlowEntry {
    this.checkpoint('add configuration node');
    const entry: FlowEntry = { ...structuredClone(props), id: generateId(), type };
    delete entry.x;
    delete entry.y;
    delete entry.wires;
    delete entry.z;
    this.entries.push(entry);
    this.touch();
    return entry;
  }

  /** The nodes that name a configuration node by id. */
  usersOfConfig(id: string): FlowEntry[] {
    return this.entries.filter((e) => e.id !== id && Object.entries(e).some(([k, v]) => k !== 'id' && k !== 'z' && k !== 'g' && v === id));
  }

  /** Removes a configuration node, unless something still uses it. */
  deleteConfig(id: string): string | null {
    const cfg = this.configNodes().find((e) => e.id === id);
    if (!cfg) return 'There is no such configuration node.';
    const users = this.usersOfConfig(id);
    if (users.length > 0) {
      return `${users.length} node${users.length === 1 ? ' uses' : 's use'} it. Point ${users.length === 1 ? 'it' : 'them'} at another one first.`;
    }
    this.checkpoint('delete configuration node');
    this.entries = this.entries.filter((e) => e.id !== id);
    this.touch();
    return null;
  }

  // ── Flows ──────────────────────────────────────────────────────────────────

  renameTab(id: string, label: string): void {
    const tab = this.entries.find((e) => e.id === id && e.type === 'tab');
    if (!tab || tab.label === label) return;
    this.checkpoint('rename flow');
    tab.label = label;
    this.touch();
  }

  /**
   * Deletes a flow and everything on it: its nodes, its groups, and every wire
   * and link that led to them. A runtime with no flow has nowhere to put a
   * node, so the last one stays.
   */
  deleteTab(id: string): string | null {
    const tab = this.entries.find((e) => e.id === id && e.type === 'tab');
    if (!tab) return 'There is no such flow.';
    if (this.tabs().length <= 1) return 'This is the only flow. Add another before deleting it.';
    this.checkpoint('delete flow');
    const doomed = new Set(this.entries.filter((e) => e.z === id).map((e) => e.id));
    doomed.add(id);
    this.entries = this.entries.filter((e) => !doomed.has(e.id));
    this.dropDeadReferences(doomed);
    this.selection.clear();
    if (this.activeTab === id) this.activeTab = this.tabs()[0]!.id;
    this.touch();
    return null;
  }

  /** What is on a flow, for the question "are you sure". */
  countOn(tabId: string): number {
    return this.entries.filter((e) => e.z === tabId && e.type !== 'group' && !this.isConfig(e)).length;
  }

  // ── Selection to subflow ───────────────────────────────────────────────────

  /**
   * Moves the selected nodes into a subflow of their own and puts an instance
   * of it where they were. The wires that crossed the edge of the selection
   * become the subflow's ports, so the flow behaves as it did.
   *
   * A subflow has one input, so everything wired into the selection has to
   * arrive at a single node; an input that fed two different nodes from two
   * different places would have to feed both from both, and that is a
   * different flow. Every distinct output of a selected node that left the
   * selection becomes an output of the subflow.
   */
  selectionToSubflow(name: string): FlowEntry | string {
    const scope = this.activeTab;
    const ids = [...this.selection].filter((id) => !parsePortId(id));
    if (ids.length === 0) return 'Select the nodes to turn into a subflow first.';
    const picked = this.withGroupMembers(ids);
    const members = [...picked].map((id) => this.entries.find((e) => e.id === id)).filter((e): e is FlowEntry => e !== undefined);
    if (members.some((e) => e.z !== scope)) return 'Everything selected has to be on the flow you are looking at.';
    const nodes = members.filter((e) => e.type !== 'group');
    if (nodes.length === 0) return 'A group with nothing in it is not a subflow.';
    const inside = new Set(nodes.map((n) => n.id));

    // What crosses the edge, before anything is changed.
    const scopeTpl = this.entries.find((e) => e.id === scope && e.type === 'subflow');
    const outsiders = this.entries.filter((e) => e.z === scope && !inside.has(e.id) && e.wires && !this.isConfig(e));
    const entering = new Set<string>();
    for (const src of outsiders) {
      for (const port of src.wires!) for (const t of port) if (inside.has(t)) entering.add(t);
    }
    const tplIn = (scopeTpl?.in as SubflowPortDef[] | undefined) ?? [];
    for (const p of tplIn) for (const w of p.wires ?? []) if (inside.has(w.id)) entering.add(w.id);
    if (entering.size > 1) {
      return 'A subflow has one input, and this selection is wired into more than one node. Select so that everything arrives at a single node.';
    }

    type Leave = { key: string; node: FlowEntry; port: number };
    const leaves: Leave[] = [];
    const leaveKey = (n: FlowEntry, p: number) => `${n.id}:${p}`;
    const seenLeave = new Set<string>();
    for (const n of nodes) {
      (n.wires ?? []).forEach((targets, port) => {
        if (targets.some((t) => !inside.has(t)) && !seenLeave.has(leaveKey(n, port))) {
          seenLeave.add(leaveKey(n, port));
          leaves.push({ key: leaveKey(n, port), node: n, port });
        }
      });
    }
    const tplOut = (scopeTpl?.out as SubflowPortDef[] | undefined) ?? [];
    for (const p of tplOut) {
      for (const w of p.wires ?? []) {
        const n = nodes.find((x) => x.id === w.id);
        if (n && !seenLeave.has(leaveKey(n, w.port ?? 0))) {
          seenLeave.add(leaveKey(n, w.port ?? 0));
          leaves.push({ key: leaveKey(n, w.port ?? 0), node: n, port: w.port ?? 0 });
        }
      }
    }

    this.checkpoint('selection to subflow');

    // Where the nodes go inside the template, and the instance in the flow.
    const xs = nodes.map((n) => n.x ?? 0), ys = nodes.map((n) => n.y ?? 0);
    const minX = Math.min(...xs), maxX = Math.max(...xs), minY = Math.min(...ys), maxY = Math.max(...ys);
    const dx = 220 - minX, dy = 80 - minY;
    const at = { x: snap((minX + maxX) / 2), y: snap((minY + maxY) / 2) };

    const tplId = generateId();
    const instanceId = generateId();
    const tpl: FlowEntry = {
      id: tplId, type: 'subflow', name, info: '', category: '', env: [], color: '#DDAA99',
      in: entering.size === 1
        ? [{ x: 60, y: snap([...entering].map((id) => (nodes.find((n) => n.id === id)!.y ?? 0) + dy)[0]!), wires: [{ id: [...entering][0]! }] }]
        : [],
      out: leaves.map((l) => ({ x: snap(maxX + dx + 220), y: snap((l.node.y ?? 0) + dy), wires: [{ id: l.node.id, port: l.port }] })),
    };

    const instance: FlowEntry = {
      id: instanceId, type: SUBFLOW_PREFIX + tplId, z: scope, name: '', x: at.x, y: at.y, env: [],
      wires: leaves.map(() => []),
    };

    // The wires into the selection now go to the instance.
    if (entering.size === 1) {
      for (const src of outsiders) {
        src.wires = src.wires!.map((port) => {
          if (!port.some((t) => inside.has(t))) return port;
          return [...new Set(port.map((t) => (inside.has(t) ? instanceId : t)))];
        });
      }
      for (const p of tplIn) {
        if (!(p.wires ?? []).some((w) => inside.has(w.id))) continue;
        const next: { id: string }[] = [];
        for (const w of p.wires!) {
          const mapped = inside.has(w.id) ? instanceId : w.id;
          if (!next.some((x) => x.id === mapped)) next.push({ ...w, id: mapped });
        }
        p.wires = next;
      }
    }

    // The wires out of the selection now leave from the instance's outputs.
    leaves.forEach((l, i) => {
      const external = (l.node.wires?.[l.port] ?? []).filter((t) => !inside.has(t));
      instance.wires![i] = [...external];
      if (l.node.wires?.[l.port]) l.node.wires[l.port] = l.node.wires[l.port]!.filter((t) => inside.has(t));
      for (const p of tplOut) {
        p.wires = (p.wires ?? []).map((w) => (w.id === l.node.id && (w.port ?? 0) === l.port ? { id: instanceId, port: i } : w));
      }
    });

    // Move everything in. Wires that stayed inside are kept; the nodes keep
    // their groups when the group came along, and are taken out of it when not.
    const topParents = new Set(members.filter((e) => !e.g || !picked.has(e.g)).map((e) => e.g ?? ''));
    for (const e of members) {
      if (e.g && !picked.has(e.g)) delete e.g;
      e.z = tplId;
      if (e.x !== undefined) e.x = snap(e.x + dx);
      if (e.y !== undefined) e.y = snap(e.y + dy);
      if (e.wires) e.wires = e.wires.map((port) => port.filter((t) => inside.has(t)));
    }

    // The instance sits in the group the selection came out of, when all of it
    // came out of the one group.
    if (topParents.size === 1 && [...topParents][0]) instance.g = [...topParents][0]!;

    this.entries.push(tpl, instance);
    this.refreshGroups([scope, tplId]);
    this.selection = new Set([instanceId]);
    this.touch();
    return instance;
  }

  // ── Durable wires ──────────────────────────────────────────────────────────

  /**
   * How a wire is marked: durable or not, and how many times its target may
   * refuse one message before the wire gives up on it (null for never). The
   * file keeps this per pair of nodes, so it holds for every wire between them.
   */
  wireSettings(fromId: string, toId: string): { durable: boolean; attempts: number | null } {
    const from = this.entries.find((e) => e.id === fromId);
    const durable = Array.isArray(from?.[DURABLE_KEY]) && (from![DURABLE_KEY] as string[]).includes(toId);
    const limits = from?.[DURABLE_ATTEMPTS_KEY] as Record<string, unknown> | undefined;
    const n = limits?.[toId];
    return { durable, attempts: durable && typeof n === 'number' ? n : null };
  }

  setWireSettings(fromId: string, toId: string, settings: { durable: boolean; attempts: number | null }): void {
    const from = this.entries.find((e) => e.id === fromId);
    if (!from) return;
    const was = this.wireSettings(fromId, toId);
    if (was.durable === settings.durable && was.attempts === settings.attempts) return;
    this.checkpoint('edit wire');
    const ids = new Set(Array.isArray(from[DURABLE_KEY]) ? (from[DURABLE_KEY] as string[]) : []);
    const limits: Record<string, unknown> = { ...((from[DURABLE_ATTEMPTS_KEY] as Record<string, unknown> | undefined) ?? {}) };
    if (settings.durable) {
      ids.add(toId);
      if (settings.attempts !== null) limits[toId] = settings.attempts; else delete limits[toId];
    } else {
      ids.delete(toId);
      delete limits[toId];
    }
    if (ids.size > 0) from[DURABLE_KEY] = [...ids]; else delete from[DURABLE_KEY];
    if (Object.keys(limits).length > 0) from[DURABLE_ATTEMPTS_KEY] = limits; else delete from[DURABLE_ATTEMPTS_KEY];
    this.touch();
  }

  /** Every wire on a tab, as drawable segments. */
  wiresOn(tabId: string): { from: FlowEntry; port: number; to: FlowEntry }[] {
    const out: { from: FlowEntry; port: number; to: FlowEntry }[] = [];
    for (const p of this.portNodes(tabId)) {
      const parsed = parsePortId(p.id)!;
      for (const w of this.portDef(parsed.subflow, parsed.kind, parsed.n)?.wires ?? []) {
        const other = this.entries.find((e) => e.id === w.id);
        if (!other || other.z !== tabId) continue;
        if (parsed.kind === 'in') out.push({ from: p, port: 0, to: other });
        else out.push({ from: other, port: w.port ?? 0, to: p });
      }
    }
    for (const e of this.nodesOn(tabId)) {
      e.wires?.forEach((targets, port) => {
        for (const id of targets) {
          const to = this.byId(id);
          // A wire to a node on another tab is legal in the file but not
          // drawable here; the runtime reports it, the canvas skips it.
          if (to && to.z === tabId) out.push({ from: e, port, to });
        }
      });
    }
    return out;
  }

  // ── Subflow templates ──────────────────────────────────────────────────────

  /** A new, empty subflow with one input and one output, and its canvas. */
  addSubflow(name: string): FlowEntry {
    this.checkpoint('add subflow');
    const tpl: FlowEntry = {
      id: generateId(),
      type: 'subflow',
      name,
      info: '',
      category: '',
      in: [{ x: 60, y: 60, wires: [] }],
      out: [{ x: 540, y: 60, wires: [] }],
      env: [],
      color: '#DDAA99',
    };
    this.entries.push(tpl);
    this.activeTab = tpl.id;
    this.touch();
    return tpl;
  }

  /**
   * Changes what a template looks like from outside: its name, its colour, its
   * parameters, and how many ports it has. Every instance follows, because an
   * instance with a different number of outputs than its template draws ports
   * it cannot send on, or leaves ones it has unwired.
   */
  updateSubflow(id: string, shape: {
    name: string; info: string; category: string; color: string;
    inputs: number; outputs: number; env: { name: string; type: string; value: string }[];
  }): void {
    const tpl = this.entries.find((e) => e.id === id && e.type === 'subflow');
    if (!tpl) return;
    this.checkpoint('edit subflow');
    tpl.name = shape.name;
    tpl.info = shape.info;
    tpl.category = shape.category;
    tpl.color = shape.color;
    tpl.env = shape.env.filter((v) => v.name !== '');

    const inputs = Math.max(0, Math.min(1, shape.inputs));
    const outputs = Math.max(0, shape.outputs);
    const ins = (tpl.in as SubflowPortDef[] | undefined) ?? [];
    const outs = (tpl.out as SubflowPortDef[] | undefined) ?? [];
    while (ins.length > inputs) ins.pop();
    while (ins.length < inputs) ins.push({ x: 60, y: 60, wires: [] });
    while (outs.length > outputs) outs.pop();
    while (outs.length < outputs) outs.push({ x: 540, y: 60 + outs.length * 50, wires: [] });
    tpl.in = ins;
    tpl.out = outs;

    for (const inst of this.instancesOf(id)) {
      if (!inst.wires) inst.wires = [];
      while (inst.wires.length < outputs) inst.wires.push([]);
      if (inst.wires.length > outputs) inst.wires = inst.wires.slice(0, outputs);
    }
    if (inputs === 0) {
      const instIds = new Set(this.instancesOf(id).map((i) => i.id));
      for (const e of this.entries) {
        if (e.wires) e.wires = e.wires.map((port) => port.filter((t) => !instIds.has(t)));
      }
    }
    this.touch();
  }

  /**
   * Removes a template and everything inside it. Refused while an instance is
   * still on a canvas somewhere, because deleting what a running flow uses
   * would turn that flow's node into a grey box on the next load.
   */
  deleteSubflow(id: string): string | null {
    const tpl = this.entries.find((e) => e.id === id && e.type === 'subflow');
    if (!tpl) return 'There is no such subflow.';
    const used = this.instancesOf(id);
    if (used.length > 0) {
      return `${used.length} instance${used.length === 1 ? ' is' : 's are'} still in use. Delete them first.`;
    }
    this.checkpoint('delete subflow');
    const doomed = new Set(this.entries.filter((e) => e.z === id).map((e) => e.id));
    doomed.add(id);
    this.entries = this.entries.filter((e) => !doomed.has(e.id));
    this.dropDeadReferences(doomed);
    this.selection.clear();
    if (this.activeTab === id) this.activeTab = this.tabs()[0]?.id ?? '';
    this.touch();
    return null;
  }

  /** Places an instance of a template. */
  addInstance(subflowId: string, at: Point): FlowEntry | undefined {
    const tpl = this.entries.find((e) => e.id === subflowId && e.type === 'subflow');
    if (!tpl) return undefined;
    // A template placed inside itself, or inside one that already holds an
    // instance of it, never finishes expanding. The runtime stops at a depth;
    // the editor does not let it start.
    if (this.wouldNest(this.activeTab, subflowId)) return undefined;
    this.checkpoint('add subflow instance');
    const outputs = ((tpl.out as unknown[] | undefined) ?? []).length;
    const entry: FlowEntry = {
      id: generateId(),
      type: SUBFLOW_PREFIX + subflowId,
      z: this.activeTab,
      name: '',
      x: snap(at.x),
      y: snap(at.y),
      env: [],
      wires: Array.from({ length: outputs }, () => []),
    };
    this.entries.push(entry);
    this.selection = new Set([entry.id]);
    this.touch();
    return entry;
  }

  /** Whether placing an instance of `inner` on the canvas `scope` would make a cycle. */
  wouldNest(scope: string, inner: string): boolean {
    if (scope === inner) return true;
    const seen = new Set<string>();
    const reaches = (from: string): boolean => {
      if (from === scope) return true;
      if (seen.has(from)) return false;
      seen.add(from);
      for (const e of this.entries) {
        if (e.z === from && e.type.startsWith(SUBFLOW_PREFIX)) {
          if (reaches(e.type.slice(SUBFLOW_PREFIX.length))) return true;
        }
      }
      return false;
    };
    return reaches(inner);
  }

  // ── Groups ─────────────────────────────────────────────────────────────────

  /**
   * Wraps the selected nodes, and groups, in a new group. Everything selected
   * has to sit in the same place already: all loose, or all in one group. A
   * group built across two others would have to steal members from both.
   */
  groupSelection(): FlowEntry | string {
    const ids = [...this.selection].filter((id) => !parsePortId(id));
    const picked = ids.map((id) => this.byId(id)).filter((e): e is FlowEntry => e !== undefined);
    if (picked.length === 0) return 'Select something to group first.';
    const parents = new Set(picked.map((e) => e.g ?? ''));
    if (parents.size > 1) return 'Everything selected has to be in the same group, or in none.';
    if (new Set(picked.map((e) => e.z)).size > 1) return 'Everything selected has to be on the same flow.';
    // A group cannot be put inside itself: skip any selected group whose
    // ancestor is also selected, since moving the ancestor moves it anyway.
    const top = picked.filter((e) => !this.hasSelectedAncestor(e, ids));

    this.checkpoint('group');
    const parent = top[0]!.g;
    const group: FlowEntry = {
      id: generateId(),
      type: 'group',
      z: top[0]!.z,
      name: '',
      style: { label: true },
      nodes: top.map((e) => e.id),
      x: 0, y: 0, w: 0, h: 0,
    };
    if (parent) group.g = parent;
    this.entries.push(group);
    for (const e of top) e.g = group.id;
    if (parent) {
      const p = this.entries.find((e) => e.id === parent);
      if (p) p.nodes = [...((p.nodes as string[] | undefined) ?? []).filter((id) => !top.some((t) => t.id === id)), group.id];
    }
    this.refreshGroups([group.z ?? '']);
    this.selection = new Set([group.id]);
    this.touch();
    return group;
  }

  private hasSelectedAncestor(e: FlowEntry, ids: string[]): boolean {
    for (let g = e.g; g; g = this.entries.find((x) => x.id === g)?.g) {
      if (ids.includes(g)) return true;
    }
    return false;
  }

  /** Dissolves the selected groups. What was inside stays, one level up. */
  ungroupSelection(): number {
    const groups = [...this.selection].map((id) => this.byId(id)).filter((e) => e?.type === 'group') as FlowEntry[];
    if (groups.length === 0) return 0;
    this.checkpoint('ungroup');
    const freed: string[] = [];
    for (const g of groups) {
      for (const e of this.entries) {
        if (e.g !== g.id) continue;
        if (g.g) e.g = g.g;
        else delete e.g;
        freed.push(e.id);
      }
    }
    const doomed = new Set(groups.map((g) => g.id));
    this.entries = this.entries.filter((e) => !doomed.has(e.id));
    for (const e of this.entries) {
      if (e.type === 'group' && Array.isArray(e.nodes)) {
        e.nodes = (e.nodes as string[]).filter((id) => !doomed.has(id));
      }
    }
    // The parent of a dissolved group now holds its members directly.
    for (const g of groups) {
      if (!g.g) continue;
      const p = this.entries.find((e) => e.id === g.g);
      if (p) p.nodes = [...new Set([...((p.nodes as string[] | undefined) ?? []), ...freed.filter((id) => this.byId(id)?.g === p.id)])];
    }
    this.selection = new Set(freed);
    this.refreshGroups(groups.map((g) => g.z ?? ''));
    this.touch();
    return groups.length;
  }

  /** The group a node, wherever it sits, is drawn inside, innermost first. */
  groupAt(scope: string, p: Point, except: Set<string>): FlowEntry | undefined {
    let best: FlowEntry | undefined;
    let bestArea = Infinity;
    for (const g of this.groupsOn(scope)) {
      if (except.has(g.id)) continue;
      const x = Number(g.x), y = Number(g.y), w = Number(g.w), h = Number(g.h);
      if (!(w > 0 && h > 0)) continue;
      if (p.x < x || p.x > x + w || p.y < y || p.y > y + h) continue;
      if (w * h < bestArea) { best = g; bestArea = w * h; }
    }
    return best;
  }

  /**
   * After a drag: a node let go inside a group belongs to it, and one dragged
   * out of its group does not any more. The box was not resized during the
   * drag, so where it sits now is where the answer is.
   */
  rehomeSelection(): void {
    const moved = [...this.selection].map((id) => this.byId(id))
      .filter((e): e is FlowEntry => e !== undefined && e.type !== 'group' && !parsePortId(e.id) && e.x !== undefined);
    // A group dragged whole takes its members with it and changes nobody's home.
    const wholeGroups = this.withGroupMembers([...this.selection].filter((id) => this.isGroup(id)));
    for (const n of moved) {
      if (wholeGroups.has(n.id) || !n.z) continue;
      const except = new Set<string>();
      const target = this.groupAt(n.z, { x: n.x!, y: n.y! }, except);
      if ((target?.id ?? '') === (n.g ?? '')) continue;
      if (n.g) {
        const old = this.entries.find((e) => e.id === n.g);
        if (old) old.nodes = ((old.nodes as string[] | undefined) ?? []).filter((id) => id !== n.id);
      }
      if (target) {
        n.g = target.id;
        target.nodes = [...new Set([...((target.nodes as string[] | undefined) ?? []), n.id])];
      } else {
        delete n.g;
      }
    }
    // Whatever was dragged, the boxes around it are fitted again: a dragged
    // group was moved by the raw drag while its nodes snapped to the grid.
    const touched = [...this.selection].map((id) => this.byId(id)?.z ?? '');
    this.refreshGroups([...moved.map((n) => n.z ?? ''), ...touched]);
  }

  updateGroup(id: string, props: { name: string; fill: string; stroke: string; label: boolean }): void {
    const g = this.entries.find((e) => e.id === id && e.type === 'group');
    if (!g) return;
    this.checkpoint('edit group');
    g.name = props.name;
    const style: Record<string, unknown> = { ...((g.style as Record<string, unknown> | undefined) ?? {}) };
    if (props.fill) style.fill = props.fill; else delete style.fill;
    if (props.stroke) style.stroke = props.stroke; else delete style.stroke;
    style.label = props.label;
    g.style = style;
    this.touch();
  }

  /**
   * Fits every group's box to what is in it, innermost first so a group
   * holding another sees its final size. Also keeps each group's list of
   * members equal to the nodes that name it, which is what the runtime reads.
   */
  refreshGroups(scopes: Iterable<string>): void {
    const only = new Set(scopes);
    const groups = this.entries.filter((e) => e.type === 'group' && only.has(e.z ?? ''));
    const depth = (g: FlowEntry): number => {
      let d = 0;
      for (let p = g.g; p; p = this.entries.find((x) => x.id === p)?.g) d++;
      return d;
    };
    groups.sort((a, b) => depth(b) - depth(a));
    for (const g of groups) {
      const members = this.entries.filter((e) => e.g === g.id);
      g.nodes = members.map((m) => m.id);
      let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
      for (const m of members) {
        if (m.type === 'group') {
          if (!(Number(m.w) > 0)) continue;
          minX = Math.min(minX, Number(m.x)); minY = Math.min(minY, Number(m.y));
          maxX = Math.max(maxX, Number(m.x) + Number(m.w)); maxY = Math.max(maxY, Number(m.y) + Number(m.h));
        } else if (m.x !== undefined && m.y !== undefined) {
          minX = Math.min(minX, m.x - NODE_WIDTH / 2); minY = Math.min(minY, m.y - NODE_HEIGHT / 2);
          maxX = Math.max(maxX, m.x + NODE_WIDTH / 2); maxY = Math.max(maxY, m.y + NODE_HEIGHT / 2);
        }
      }
      if (minX === Infinity) continue;
      g.x = Math.round(minX - GROUP_PAD);
      g.y = Math.round(minY - GROUP_PAD - GROUP_HEAD);
      g.w = Math.round(maxX - minX + GROUP_PAD * 2);
      g.h = Math.round(maxY - minY + GROUP_PAD * 2 + GROUP_HEAD);
    }
  }

  // ── Bringing entries in ────────────────────────────────────────────────────

  /**
   * Adds entries that already have their final ids and places, as one undo
   * step. flowio.ts decides what they are; this is only the door.
   */
  insert(entries: FlowEntry[], label: string): void {
    this.checkpoint(label);
    this.entries.push(...entries);
    this.refreshGroups(entries.map((e) => e.z ?? ''));
    this.touch();
  }
}

/**
 * A flow-file id: sixteen hex characters, matching what the runtime generates.
 *
 * crypto.getRandomValues rather than Math.random — ids end up in a file that is
 * merged and diffed, and a collision produces a duplicate-id parse failure that
 * is very hard to explain.
 */
export function generateId(): string {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}
