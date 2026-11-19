// The flow canvas.
//
// Native SVG and pointer events. Node-RED uses D3 for this; the parts of D3 that
// are actually needed here — a viewBox transform and some drag maths — are about
// forty lines, and pulling in the library would cost more than it saves and
// would still have to be themed.

import {
  type FlowEntry, type Graph, type Point,
  GROUP_HEAD, NODE_HEIGHT, NODE_WIDTH, PORT_RADIUS, PORT_STEP, SUBFLOW_PREFIX, inputPort, outputPort, snap, wireMid, wirePath,
} from './graph';
import { type DiffOverlay, wireKey } from './diff';
import { SYMBOLS } from './symbols';
import { type WireProblem, clashesByWire, wireTypeLabel } from './wiretypes';
import type { TestMark } from './tests';
import { type TraceOverlay, fmtNanos, tipFor } from './trace';
import { tr } from './i18n.svelte';

const SVG_NS = 'http://www.w3.org/2000/svg';

/** Two presses on one node within this long are a double-click. */
const DOUBLE_PRESS_MS = 400;

/** What the canvas is currently doing. */
type Mode =
  | { kind: 'idle' }
  | { kind: 'pan'; startClient: Point; startView: Point }
  | { kind: 'drag'; last: Point; moved: boolean }
  | { kind: 'wire'; fromId: string; port: number; cursor: Point }
  | { kind: 'marquee'; start: Point; current: Point };

/** A sub-diagram drawn open in place: where its frame is on the canvas. */
interface Frame {
  /** The sub-diagram node it is the open form of. */
  id: string;
  /** The tab the node is on, and the tab whose nodes are drawn inside. */
  tab: string;
  body: string;
  /** How many open sub-diagrams it sits inside. */
  depth: number;
  x: number;
  y: number;
  w: number;
  h: number;
}

interface Box { x0: number; y0: number; x1: number; y1: number }

const FRAME_HEAD = 26;
const FRAME_PAD = 18;
/** Room at each side of an open sub-diagram for its own connector, named. */
const FRAME_GUTTER = 64;
const FRAME_MIN_W = 220;
/** An open sub-diagram can hold open sub-diagrams, this many deep. */
const FRAME_DEPTH = 3;

export interface CanvasCallbacks {
  onEditNode: (entry: FlowEntry) => void;
  onSelectionChange: () => void;
  /** Something the person should be told, such as why a group could not be made. */
  onNotice?: (message: string) => void;
  /** A wire was double-clicked. */
  onEditWire?: (wire: { from: FlowEntry; port: number; to: FlowEntry }) => void;
  /** A sub-diagram was opened or closed in place. */
  onViewChange?: () => void;
}

export interface CanvasOptions {
  /**
   * Looking, not editing: pan, zoom and select, and nothing that changes the
   * graph. For a deployment from the history, where an edit would be an edit
   * to the past.
   */
  readOnly?: boolean;
}

export class Canvas {
  private svg: SVGSVGElement;
  private layers: {
    grid: SVGGElement;
    groups: SVGGElement;
    frames: SVGGElement;
    wires: SVGGElement;
    inner: SVGGElement;
    nodes: SVGGElement;
    overlay: SVGGElement;
  };

  private view = { x: 0, y: 0, w: 1200, h: 800 };
  private mode: Mode = { kind: 'idle' };
  private statuses = new Map<string, { fill: string; shape: string; text: string }>();
  private diff: DiffOverlay | null = null;
  private testMarks = new Map<string, TestMark>();
  private wireClashes = new Map<string, string[]>();
  private trace: TraceOverlay | null = null;
  /** Where a diagram run is: node id to start, ready, done, skipped, error. */
  private nodeStates = new Map<string, string>();
  /** Wires a token just crossed, by wire key, while highlight execution is on. */
  private litWires = new Set<string>();
  /** The last value on a wire, by wire key, for the tooltip. */
  private probeValues: Map<string, string> | null = null;
  private breakpoints = new Set<string>();
  private probed = new Set<string>();
  private readonly readOnly: boolean;
  private lastPress = { id: '', at: 0 };
  /** Where the pointer last was over the canvas, in canvas coordinates. */
  private pointer: Point | null = null;
  /**
   * The sub-diagrams drawn open, by the id of the sub-diagram node. This is how
   * the person is looking at the diagram, not part of it: it is not in the flow
   * file, it is not an undo step, and a reload closes them all.
   */
  private expanded = new Set<string>();
  /** Where each open sub-diagram is drawn, worked out afresh by every render. */
  private frames = new Map<string, Frame>();
  /** How far the nodes inside an open sub-diagram are moved from where the file puts them. */
  private offsets = new Map<string, Point>();

  constructor(
    private readonly host: HTMLElement,
    private readonly graph: Graph,
    private readonly cb: CanvasCallbacks,
    opts: CanvasOptions = {},
  ) {
    this.readOnly = opts.readOnly ?? false;
    this.svg = document.createElementNS(SVG_NS, 'svg');
    this.svg.setAttribute('class', 'canvas');
    this.svg.setAttribute('tabindex', '0');

    this.layers = {
      grid: this.group('layer-grid'),
      groups: this.group('layer-groups'),
      frames: this.group('layer-frames'),
      wires: this.group('layer-wires'),
      inner: this.group('layer-inner'),
      nodes: this.group('layer-nodes'),
      overlay: this.group('layer-overlay'),
    };
    this.svg.append(this.layers.grid, this.layers.groups, this.layers.frames, this.layers.wires, this.layers.inner, this.layers.nodes, this.layers.overlay);
    host.append(this.svg);

    this.bindEvents();
    this.resize();
    const watch = new ResizeObserver(() => this.resize());
    watch.observe(host);
    watch.observe(this.svg);
    graph.onChange(() => this.render());
  }

  private group(cls: string): SVGGElement {
    const g = document.createElementNS(SVG_NS, 'g');
    g.setAttribute('class', cls);
    return g;
  }

  private resize(): void {
    // What the viewBox is drawn onto is the svg, which is shorter than the host when
    // the bar over a diagram takes room at the top. The view has to have the
    // svg's own shape, or the browser letterboxes it and a pointer is read onto
    // the wrong part of the canvas.
    const own = this.svg.getBoundingClientRect();
    const rect = own.width > 0 && own.height > 0 ? own : this.host.getBoundingClientRect();
    // The viewBox keeps its scale; only the visible extent changes, so
    // resizing the window pans rather than zooming.
    const scale = this.view.w / (this.svg.clientWidth || rect.width || 1);
    this.view.w = rect.width * scale;
    this.view.h = rect.height * scale;
    this.applyView();
  }

  private applyView(): void {
    this.svg.setAttribute('viewBox', `${this.view.x} ${this.view.y} ${this.view.w} ${this.view.h}`);
    this.svg.setAttribute('data-zoom', String(Math.round(100 / this.scale)));
    this.renderGrid();
  }

  /** Converts a client point to canvas coordinates. */
  private toCanvas(clientX: number, clientY: number): Point {
    const rect = this.svg.getBoundingClientRect();
    return {
      x: this.view.x + ((clientX - rect.left) / rect.width) * this.view.w,
      y: this.view.y + ((clientY - rect.top) / rect.height) * this.view.h,
    };
  }

  private get scale(): number {
    const rect = this.svg.getBoundingClientRect();
    return rect.width > 0 ? this.view.w / rect.width : 1;
  }

  // ── Events ─────────────────────────────────────────────────────────────────

  private bindEvents(): void {
    this.svg.addEventListener('pointerdown', (e) => this.onPointerDown(e));
    this.svg.addEventListener('pointermove', (e) => this.onPointerMove(e));
    this.svg.addEventListener('pointerup', (e) => this.onPointerUp(e));
    this.svg.addEventListener('pointercancel', () => this.endInteraction());
    this.svg.addEventListener('wheel', (e) => this.onWheel(e), { passive: false });
    this.svg.addEventListener('contextmenu', (e) => e.preventDefault());
    this.svg.addEventListener('keydown', (e) => this.onKeyDown(e));


    // Dropping a node from the palette.
    this.svg.addEventListener('dragover', (e) => {
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
    });
    this.svg.addEventListener('drop', (e) => {
      e.preventDefault();
      if (this.readOnly) return;
      const type = e.dataTransfer?.getData('text/hotloop-flow-node');
      if (!type) return;
      const at = this.toCanvas(e.clientX, e.clientY);
      const entry = this.place(type, at);
      if (!entry) return;
      this.cb.onSelectionChange();
      this.cb.onEditNode(entry);
    });
  }

  private onPointerDown(e: PointerEvent): void {
    this.svg.focus();
    const target = e.target as Element;
    const at = this.toCanvas(e.clientX, e.clientY);

    // Middle button, or space-drag, pans. Right button pans too, because a
    // trackpad has no middle button and panning is the most common gesture.
    if (e.button === 1 || e.button === 2) {
      this.mode = { kind: 'pan', startClient: { x: e.clientX, y: e.clientY }, startView: { x: this.view.x, y: this.view.y } };
      this.svg.setPointerCapture(e.pointerId);
      return;
    }
    if (e.button !== 0) return;

    // A wire: two presses on it open its settings. A single press is still a
    // press on empty canvas, so a marquee can start over a wire.
    const wireEl = this.readOnly ? null : target.closest('path.wire[data-from][data-to]');
    // Alt-press on a wire deletes it. It has to be the press: the canvas takes
    // pointer capture on a press, so the wire never sees the click that follows
    // and a click handler on the wire does nothing. Alt rather than a plain
    // press, so brushing past one while selecting does not silently break a flow.
    if (wireEl && e.altKey) {
      this.graph.disconnect(wireEl.getAttribute('data-from')!, Number(wireEl.getAttribute('data-port')), wireEl.getAttribute('data-to')!);
      return;
    }
    if (wireEl && wireEl.hasAttribute('data-port')) {
      const key = `wire:${wireEl.getAttribute('data-from')}:${wireEl.getAttribute('data-port')}:${wireEl.getAttribute('data-to')}`;
      const now = performance.now();
      if (this.lastPress.id === key && now - this.lastPress.at < DOUBLE_PRESS_MS) {
        this.lastPress = { id: '', at: 0 };
        const from = this.graph.byId(wireEl.getAttribute('data-from')!);
        const to = this.graph.byId(wireEl.getAttribute('data-to')!);
        if (from && to) this.cb.onEditWire?.({ from, port: Number(wireEl.getAttribute('data-port')), to });
        return;
      }
      this.lastPress = { id: key, at: now };
    }

    // The small boxes that open a sub-diagram in place and close it again. A press
    // on one is only that: it never starts a drag or a selection.
    const toggleEl = target.closest('[data-expand],[data-collapse]');
    if (toggleEl) {
      this.toggleExpanded(toggleEl.getAttribute('data-expand') ?? toggleEl.getAttribute('data-collapse')!);
      return;
    }

    // A wire carries data-port too, which is the output it leaves from and not a place to start a new one.
    const portEl = this.readOnly ? null : target.closest('[data-port]:not(.wire)');
    if (portEl) {
      const fromId = portEl.getAttribute('data-node')!;
      const port = Number(portEl.getAttribute('data-port'));
      this.mode = { kind: 'wire', fromId, port, cursor: at };
      this.svg.setPointerCapture(e.pointerId);
      return;
    }

    const nodeEl = target.closest('[data-node-id]');
    if (nodeEl) {
      const id = nodeEl.getAttribute('data-node-id')!;

      // Two presses on the same node in quick succession open its dialog.
      // Counted here, not read off the browser: pointerup's detail is 0 by
      // spec and Chromium follows it, and the dblclick event never reaches a
      // node because every press re-renders the node it landed on. So the
      // editor's double-click did nothing in Chromium until the browser tests
      // went looking.
      const now = performance.now();
      const entry = this.graph.byId(id);
      if (!this.readOnly && entry && this.lastPress.id === id && now - this.lastPress.at < DOUBLE_PRESS_MS) {
        this.lastPress = { id: '', at: 0 };
        this.cb.onEditNode(entry);
        return;
      }
      this.lastPress = { id, at: now };
      const additive = e.shiftKey || e.ctrlKey || e.metaKey;
      if (additive) {
        if (this.graph.selection.has(id)) this.graph.selection.delete(id);
        else this.graph.selection.add(id);
      } else if (!this.graph.selection.has(id)) {
        this.graph.selection = new Set([id]);
      }
      this.dropInsideOfSelectedFrames();
      this.cb.onSelectionChange();
      if (this.readOnly) {
        this.render();
        return;
      }

      // Checkpoint before the drag rather than after, so moving several nodes
      // is one undo step.
      this.graph.checkpoint('move');
      this.mode = { kind: 'drag', last: at, moved: false };
      this.svg.setPointerCapture(e.pointerId);
      this.render();
      return;
    }

    // A group's border or its title bar. Its inside is not a hit, so a
    // marquee can still start over a group full of nodes.
    const groupEl = target.closest('[data-group-id]');
    if (groupEl) {
      const id = groupEl.getAttribute('data-group-id')!;
      const entry = this.graph.byId(id);
      const now = performance.now();
      if (!this.readOnly && entry && this.lastPress.id === id && now - this.lastPress.at < DOUBLE_PRESS_MS) {
        this.lastPress = { id: '', at: 0 };
        this.cb.onEditNode(entry);
        return;
      }
      this.lastPress = { id, at: now };
      if (e.shiftKey || e.ctrlKey || e.metaKey) {
        if (this.graph.selection.has(id)) this.graph.selection.delete(id);
        else this.graph.selection.add(id);
      } else if (!this.graph.selection.has(id)) {
        this.graph.selection = new Set([id]);
      }
      this.cb.onSelectionChange();
      if (this.readOnly) {
        this.render();
        return;
      }
      this.graph.checkpoint('move');
      this.mode = { kind: 'drag', last: at, moved: false };
      this.svg.setPointerCapture(e.pointerId);
      this.render();
      return;
    }

    // Empty canvas: marquee select.
    if (!e.shiftKey) {
      this.graph.selection.clear();
      this.cb.onSelectionChange();
    }
    this.mode = { kind: 'marquee', start: at, current: at };
    this.svg.setPointerCapture(e.pointerId);
    this.render();
  }

  private onPointerMove(e: PointerEvent): void {
    const at = this.toCanvas(e.clientX, e.clientY);
    this.pointer = at;

    switch (this.mode.kind) {
      case 'pan': {
        const scale = this.scale;
        this.view.x = this.mode.startView.x - (e.clientX - this.mode.startClient.x) * scale;
        this.view.y = this.mode.startView.y - (e.clientY - this.mode.startClient.y) * scale;
        this.applyView();
        break;
      }
      case 'drag': {
        const dx = at.x - this.mode.last.x;
        const dy = at.y - this.mode.last.y;
        if (dx !== 0 || dy !== 0) {
          this.graph.moveSelection(dx, dy);
          this.mode.last = at;
          this.mode.moved = true;
        }
        break;
      }
      case 'wire':
        this.mode.cursor = at;
        this.renderOverlay();
        break;
      case 'marquee':
        this.mode.current = at;
        this.renderOverlay();
        break;
      default:
        break;
    }
  }

  private onPointerUp(e: PointerEvent): void {
    const at = this.toCanvas(e.clientX, e.clientY);

    if (this.mode.kind === 'wire') {
      // Asked of the page, not read off the event: the canvas holds the pointer
      // for the whole drag, so the event's target is the canvas itself, and a
      // wire let go over a node never found the node. The browser tests were
      // the first thing to try it in a real browser.
      const target = document.elementFromPoint(e.clientX, e.clientY)?.closest('[data-node-id]');
      if (target) {
        const toId = target.getAttribute('data-node-id')!;
        const inEl = document.elementFromPoint(e.clientX, e.clientY)?.closest('[data-in-port]');
        // A wire joins two nodes of one diagram. With a sub-diagram open, a node of
        // its body is drawn beside the nodes of the diagram it is in, and a wire
        // between them would be a wire between two tabs, which the file cannot say.
        const a = this.graph.byId(this.mode.fromId), b = this.graph.byId(toId);
        if (a && b && a.z !== b.z) this.cb.onNotice?.(tr('canvas.wire_same_diagram'));
        else this.graph.connect(this.mode.fromId, this.mode.port, toId, inEl?.getAttribute('data-in-port') ?? undefined);
      }
    } else if (this.mode.kind === 'drag') {
      if (this.mode.moved) {
        // Snap once at the end rather than continuously, so dragging feels
        // smooth instead of stepping.
        this.graph.snapSelection();
        // Let go inside a group and you are in it; dragged out and you are not.
        this.graph.rehomeSelection();
      }
    } else if (this.mode.kind === 'marquee') {
      this.selectWithin(this.mode.start, at, e.shiftKey);
    }

    this.endInteraction();
  }

  private endInteraction(): void {
    this.mode = { kind: 'idle' };
    this.render();
  }

  private selectWithin(a: Point, b: Point, additive: boolean): void {
    const x1 = Math.min(a.x, b.x), x2 = Math.max(a.x, b.x);
    const y1 = Math.min(a.y, b.y), y2 = Math.max(a.y, b.y);
    if (!additive) this.graph.selection.clear();

    for (const { entry, box } of this.drawnNodes()) {
      // Intersection rather than containment: dragging a box across a row of
      // nodes should catch them, not require enclosing them exactly. An open
      // sub-diagram is the exception: its inside is not a hit, so a box drawn over
      // some of what is in it picks those nodes, and takes the sub-diagram itself
      // only when it surrounds the whole frame.
      const hit = this.frames.has(entry.id)
        ? box.x0 >= x1 && box.x1 <= x2 && box.y0 >= y1 && box.y1 <= y2
        : box.x1 >= x1 && box.x0 <= x2 && box.y1 >= y1 && box.y0 <= y2;
      if (hit) this.graph.selection.add(entry.id);
    }
    this.dropInsideOfSelectedFrames();
    this.cb.onSelectionChange();
  }

  /**
   * Every node the canvas is drawing, with the box it fills where it is drawn:
   * the nodes of the diagram on view, and the nodes inside each open sub-diagram.
   */
  private drawnNodes(): { entry: FlowEntry; box: Box }[] {
    const out: { entry: FlowEntry; box: Box }[] = [];
    const add = (n: FlowEntry): void => {
      const frame = this.frames.get(n.id);
      if (frame) { out.push({ entry: n, box: { x0: frame.x, y0: frame.y, x1: frame.x + frame.w, y1: frame.y + frame.h } }); return; }
      const o = this.offsets.get(n.id) ?? { x: 0, y: 0 };
      const nx = (n.x ?? 0) + o.x, ny = (n.y ?? 0) + o.y;
      out.push({ entry: n, box: { x0: nx - NODE_WIDTH / 2, y0: ny - NODE_HEIGHT / 2, x1: nx + NODE_WIDTH / 2, y1: ny + NODE_HEIGHT / 2 } });
    };
    for (const n of this.graph.visibleNodes(this.graph.activeTab)) add(n);
    for (const f of this.frames.values()) for (const n of this.graph.nodesOn(f.body)) add(n);
    return out;
  }

  /**
   * An open sub-diagram moves with its nodes drawn in it. If the frame and a node
   * inside it are both selected, moving the selection would move that node twice,
   * so the frame stands for them all.
   */
  private dropInsideOfSelectedFrames(): void {
    for (const f of this.frames.values()) {
      if (this.graph.selection.has(f.id)) this.dropDescendants(f.body);
    }
  }

  private dropDescendants(tab: string): void {
    for (const n of this.graph.nodesOn(tab)) {
      this.graph.selection.delete(n.id);
      const inner = this.frames.get(n.id);
      if (inner) this.dropDescendants(inner.body);
    }
  }

  private onWheel(e: WheelEvent): void {
    e.preventDefault();
    const factor = e.deltaY > 0 ? 1.12 : 1 / 1.12;

    // Zoom about the cursor rather than the origin, so the thing under the
    // pointer stays under the pointer.
    const at = this.toCanvas(e.clientX, e.clientY);
    // From 5 percent to 400 percent of the size the nodes are drawn at.
    const px = this.svg.getBoundingClientRect().width || 1;
    const nextW = clamp(this.view.w * factor, px / ZOOM_MAX, px / ZOOM_MIN);
    const applied = nextW / this.view.w;

    this.view.x = at.x - (at.x - this.view.x) * applied;
    this.view.y = at.y - (at.y - this.view.y) * applied;
    this.view.w = nextW;
    this.view.h *= applied;
    this.applyView();
  }

  private onKeyDown(e: KeyboardEvent): void {
    const mod = e.ctrlKey || e.metaKey;
    if (this.readOnly) return;

    if (mod && e.key.toLowerCase() === 'z') {
      e.preventDefault();
      if (e.shiftKey) this.graph.redo();
      else this.graph.undo();
      this.cb.onSelectionChange();
      return;
    }
    if (mod && e.key.toLowerCase() === 'a') {
      e.preventDefault();
      for (const n of this.graph.visibleNodes(this.graph.activeTab)) this.graph.selection.add(n.id);
      this.cb.onSelectionChange();
      this.render();
      return;
    }
    // Group and ungroup, with the keys Node-RED uses for them.
    if (mod && e.key.toLowerCase() === 'g') {
      e.preventDefault();
      if (e.shiftKey) {
        if (this.graph.ungroupSelection() === 0) this.cb.onNotice?.('Select a group to ungroup.');
      } else {
        const made = this.graph.groupSelection();
        if (typeof made === 'string') this.cb.onNotice?.(made);
      }
      this.cb.onSelectionChange();
      return;
    }
    if (e.key === 'Delete' || e.key === 'Backspace') {
      e.preventDefault();
      this.graph.deleteSelection();
      this.cb.onSelectionChange();
      return;
    }
    // Arrow keys nudge, which is how a graph actually gets tidied.
    const step = e.shiftKey ? 1 : 10;
    const nudge: Record<string, [number, number]> = {
      ArrowLeft: [-step, 0], ArrowRight: [step, 0], ArrowUp: [0, -step], ArrowDown: [0, step],
    };
    const delta = nudge[e.key];
    if (delta && this.graph.selection.size > 0) {
      e.preventDefault();
      this.graph.checkpoint('nudge');
      this.graph.moveSelection(delta[0], delta[1]);
    }
  }

  // ── Rendering ──────────────────────────────────────────────────────────────

  /** Applies a live status badge from the runtime. */
  setStatus(nodeId: string, status: { fill: string; shape: string; text: string } | null): void {
    if (status === null) this.statuses.delete(nodeId);
    else this.statuses.set(nodeId, status);
    this.renderNodes();
  }

  /**
   * Draws a semantic diff over the graph: outlines on what was added, changed
   * or moved, ghosts where removed nodes used to be, and the wires that came
   * and went. Null clears it.
   */
  setDiff(overlay: DiffOverlay | null): void {
    this.diff = overlay;
    this.render();
  }

  /**
   * Marks the nodes a run of the flow tests was about: a tick where a test
   * passed through, a cross where one failed, with the reason on hover. Null
   * clears them, which an edit does, since a mark on a node that has changed
   * since is a claim about a flow that no longer exists.
   */
  /**
   * What the runtime found wrong with the typed wires, from POST /flows/types.
   * Wires that can never agree are drawn as clashes; the rest of the types on
   * the wires come from the schemas on the nodes and need no answer from it.
   */
  setWireProblems(problems: WireProblem[]): void {
    const next = clashesByWire(problems);
    // The check runs a moment after every edit and mostly says what it said
    // before. Redrawing then throws away every wire element for nothing, which
    // is also what a double-click on a wire can land in the middle of.
    if (sameClashes(this.wireClashes, next)) return;
    this.wireClashes = next;
    this.renderWires();
  }

  /** The state of a diagram run, drawn on the nodes and wires. */
  setRun(opts: { nodes?: Map<string, string>; lit?: Set<string>; values?: Map<string, string>; breakpoints?: Set<string>; probed?: Set<string> }): void {
    if (opts.nodes) this.nodeStates = opts.nodes;
    if (opts.lit) this.litWires = opts.lit;
    if (opts.values) this.probeValues = opts.values;
    if (opts.breakpoints) this.breakpoints = opts.breakpoints;
    if (opts.probed) this.probed = opts.probed;
    this.render();
  }

  setTestMarks(marks: Map<string, TestMark> | null): void {
    this.testMarks = marks ?? new Map();
    this.renderNodes();
  }

  /**
   * Lights the path a traced message took: the nodes it went through with how
   * long each had it, the wires it travelled, and everything else dimmed.
   * Null puts the canvas back.
   */
  setTrace(overlay: TraceOverlay | null): void {
    this.trace = overlay;
    this.svg.classList.toggle('tracing', overlay !== null);
    this.render();
  }

  /** Puts a new node, or a subflow instance, on the active canvas. */
  place(type: string, at: Point): FlowEntry | undefined {
    if (type.startsWith(SUBFLOW_PREFIX)) {
      const placed = this.graph.addInstance(type.slice(SUBFLOW_PREFIX.length), at);
      if (!placed) this.cb.onNotice?.(tr('canvas.subflow_inside_itself'));
      return placed;
    }
    return this.graph.addNode(type, at);
  }

  /** Where the pointer last was over the canvas, or null if it never has been. */
  lastPointer(): Point | null { return this.pointer; }

  /** The middle of what is in view. */
  viewCenter(): Point {
    return { x: this.view.x + this.view.w / 2, y: this.view.y + this.view.h / 2 };
  }

  /** Pans so a node is in the middle of the view, keeping the zoom. */
  centerOn(id: string): void {
    const n = this.graph.byId(id);
    if (!n || n.x === undefined || n.y === undefined) return;
    const o = this.offsets.get(id) ?? { x: 0, y: 0 };
    const x = (n.type === 'group' ? Number(n.x) + Number(n.w) / 2 : n.x) + o.x;
    const y = (n.type === 'group' ? Number(n.y) + Number(n.h) / 2 : n.y) + o.y;
    this.view.x = x - this.view.w / 2;
    this.view.y = y - this.view.h / 2;
    this.applyView();
  }

  render(): void {
    this.layout();
    this.renderGrid();
    this.renderGroups();
    this.renderFrames();
    this.renderWires();
    this.renderNodes();
    this.renderOverlay();
  }

  // ── Sub-diagrams open in place ─────────────────────────────────────────────

  /** Whether this sub-diagram node is drawn open. */
  isExpanded(id: string): boolean { return this.expanded.has(id); }

  /** Whether a node is a sub-diagram that has a body to open. */
  canExpand(n: FlowEntry | undefined): boolean { return !!n && n.type === 'dia-sub' && !!this.graph.bodyOf(n); }

  /** The sub-diagrams that are open now. */
  expandedIds(): string[] { return [...this.expanded]; }

  /**
   * Opens a sub-diagram in place, or closes it. Opened, its nodes and wires are
   * drawn inside a frame where the node was, and they are the same nodes as the
   * ones on its own tab: moving one, wiring two or editing one here is editing
   * that tab. Returns whether it is open afterwards.
   */
  toggleExpanded(id: string): boolean {
    const n = this.graph.byId(id);
    if (this.expanded.has(id)) {
      this.expanded.delete(id);
    } else {
      if (!n || !this.canExpand(n)) {
        this.cb.onNotice?.('This sub-diagram has no diagram to open yet. Pick its body in its properties.');
        return false;
      }
      const body = this.graph.bodyOf(n)!.id;
      const other = [...this.expanded].find((e) => this.graph.bodyOf(this.graph.byId(e) ?? n)?.id === body);
      if (other) {
        this.cb.onNotice?.(tr('canvas.body_already_open', { name: this.graph.label(this.graph.byId(other)!) }));
        return false;
      }
      this.expanded.add(id);
    }
    this.render();
    // Opened somewhere that does not fit the view, it is brought into view, since
    // the point of opening it is to look at it.
    const f = this.frames.get(id);
    if (f && (f.x < this.view.x || f.y < this.view.y || f.x + f.w > this.view.x + this.view.w || f.y + f.h > this.view.y + this.view.h)) this.fit();
    this.cb.onViewChange?.();
    return this.expanded.has(id);
  }

  private nodeBox(n: FlowEntry): Box {
    const H = this.graph.nodeHeight(n);
    const x = n.x ?? 0, y = n.y ?? 0;
    // A little above and below for the label over the ports and the status line under the body.
    return { x0: x - NODE_WIDTH / 2, y0: y - H / 2 - 16, x1: x + NODE_WIDTH / 2, y1: y + H / 2 + 16 };
  }

  /** The body to draw inside this node, if it is open, has a body and nothing makes that a loop. */
  private openBody(n: FlowEntry, depth: number, seen: Set<string>): string | undefined {
    if (!this.expanded.has(n.id) || depth >= FRAME_DEPTH) return undefined;
    const tab = this.graph.bodyOf(n)?.id;
    return tab && !seen.has(tab) ? tab : undefined;
  }

  /** How big the frame of an open sub-diagram is, and the box its nodes fill where the file puts them. */
  private frameSize(n: FlowEntry, body: string, depth: number, seen: Set<string>): { w: number; h: number; box: Box | null } {
    const box = this.tabBox(body, depth + 1, new Set([...seen, body]));
    const ports = this.graph.ports(n);
    const rows = Math.max(ports?.in.length ?? 1, ports?.out.length ?? 1);
    const innerW = box ? box.x1 - box.x0 : 0;
    const innerH = box ? box.y1 - box.y0 : 0;
    return {
      box,
      w: Math.max(FRAME_MIN_W, innerW + 2 * FRAME_GUTTER),
      h: Math.max(FRAME_HEAD + innerH + 2 * FRAME_PAD, FRAME_HEAD + rows * PORT_STEP + 24, 90),
    };
  }

  /** The box a tab's nodes fill, counting each open sub-diagram as its frame. */
  private tabBox(tab: string, depth: number, seen: Set<string>): Box | null {
    let box: Box | null = null;
    for (const n of this.graph.nodesOn(tab)) {
      if (n.x === undefined || n.y === undefined) continue;
      const body = this.openBody(n, depth, seen);
      let b: Box;
      if (body) {
        const size = this.frameSize(n, body, depth, seen);
        const H = this.graph.nodeHeight(n);
        b = { x0: n.x - NODE_WIDTH / 2, y0: n.y - H / 2, x1: n.x - NODE_WIDTH / 2 + size.w, y1: n.y - H / 2 + size.h };
      } else {
        b = this.nodeBox(n);
      }
      box = box ? { x0: Math.min(box.x0, b.x0), y0: Math.min(box.y0, b.y0), x1: Math.max(box.x1, b.x1), y1: Math.max(box.y1, b.y1) } : b;
    }
    return box;
  }

  /**
   * Works out where every open sub-diagram is drawn. The frame grows right and
   * down from the corner of the node it replaces, and its nodes are moved as one
   * piece to sit inside it. Nothing here touches the flow: the nodes keep the
   * coordinates the file gives them, and these are only how far to move them.
   */
  private layout(): void {
    for (const id of [...this.expanded]) {
      if (!this.graph.byId(id)) this.expanded.delete(id);
    }
    this.frames.clear();
    this.offsets.clear();
    if (this.expanded.size === 0) return;
    this.placeFrames(this.graph.activeTab, { x: 0, y: 0 }, 0, new Set([this.graph.activeTab]));
  }

  private placeFrames(tab: string, off: Point, depth: number, seen: Set<string>): void {
    for (const n of this.graph.nodesOn(tab)) {
      if (n.x === undefined || n.y === undefined) continue;
      const body = this.openBody(n, depth, seen);
      if (!body) continue;
      const size = this.frameSize(n, body, depth, seen);
      const H = this.graph.nodeHeight(n);
      const frame: Frame = { id: n.id, tab, body, depth, x: n.x - NODE_WIDTH / 2 + off.x, y: n.y - H / 2 + off.y, w: size.w, h: size.h };
      this.frames.set(n.id, frame);
      const bodyOff = {
        x: frame.x + FRAME_GUTTER - (size.box?.x0 ?? 0),
        y: frame.y + FRAME_HEAD + FRAME_PAD - (size.box?.y0 ?? 0),
      };
      for (const m of this.graph.nodesOn(body)) this.offsets.set(m.id, bodyOff);
      this.placeFrames(body, bodyOff, depth + 1, new Set([...seen, body]));
    }
  }

  /** Where a node's input port is drawn: on its frame when it is open. */
  private inAt(e: FlowEntry, k = 0): Point {
    const f = this.frames.get(e.id);
    if (f) return { x: f.x, y: f.y + FRAME_HEAD + 14 + k * PORT_STEP };
    const p = this.graph.inPos(e, k);
    const o = this.offsets.get(e.id);
    return o ? { x: p.x + o.x, y: p.y + o.y } : p;
  }

  /** Where a node's output port is drawn. */
  private outAt(e: FlowEntry, port: number): Point {
    const f = this.frames.get(e.id);
    if (f) return { x: f.x + f.w, y: f.y + FRAME_HEAD + 14 + port * PORT_STEP };
    const p = this.graph.outPos(e, port);
    const o = this.offsets.get(e.id);
    return o ? { x: p.x + o.x, y: p.y + o.y } : p;
  }

  /** Fits the view to the nodes on the active tab, and to any sub-diagram open in them. */
  fit(): void {
    this.layout();
    const nodes = this.graph.visibleNodes(this.graph.activeTab);
    if (nodes.length === 0) {
      this.view = { x: -100, y: -100, w: 1200, h: 800 };
      this.applyView();
      return;
    }
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const n of nodes) {
      minX = Math.min(minX, (n.x ?? 0) - NODE_WIDTH * 0.75);
      maxX = Math.max(maxX, (n.x ?? 0) + NODE_WIDTH * 0.75);
      minY = Math.min(minY, (n.y ?? 0) - NODE_HEIGHT * 2);
      maxY = Math.max(maxY, (n.y ?? 0) + NODE_HEIGHT * 2);
    }
    for (const f of this.frames.values()) {
      minX = Math.min(minX, f.x - 20);
      maxX = Math.max(maxX, f.x + f.w + 20);
      minY = Math.min(minY, f.y - 20);
      maxY = Math.max(maxY, f.y + f.h + 20);
    }
    // A group reaches past its nodes, and its title bar is what you grab.
    for (const g of this.graph.groupsOn(this.graph.activeTab)) {
      if (!(Number(g.w) > 0)) continue;
      minX = Math.min(minX, Number(g.x) - 20);
      maxX = Math.max(maxX, Number(g.x) + Number(g.w) + 20);
      minY = Math.min(minY, Number(g.y) - 20);
      maxY = Math.max(maxY, Number(g.y) + Number(g.h) + 20);
    }
    const rect = this.svg.getBoundingClientRect();
    const aspect = rect.height / (rect.width || 1);
    // As wide as the nodes need, and wide enough that they are not too tall for the
    // canvas either, with the shape of the canvas, so what is drawn is where the
    // pointer says it is.
    const w = Math.max(maxX - minX, 400, (maxY - minY) / (aspect || 1));
    this.view = { x: minX, y: minY, w, h: w * aspect };
    this.applyView();
  }

  private renderGrid(): void {
    this.layers.grid.replaceChildren();
    // Hidden when zoomed out, where it turns into visual noise rather than a
    // guide.
    if (this.scale > 2.2) return;

    const step = 20;
    const x0 = Math.floor(this.view.x / step) * step;
    const y0 = Math.floor(this.view.y / step) * step;
    const frag = document.createDocumentFragment();

    for (let x = x0; x < this.view.x + this.view.w; x += step) {
      const line = document.createElementNS(SVG_NS, 'line');
      line.setAttribute('x1', String(x));
      line.setAttribute('y1', String(this.view.y));
      line.setAttribute('x2', String(x));
      line.setAttribute('y2', String(this.view.y + this.view.h));
      line.setAttribute('class', 'grid-line');
      frag.append(line);
    }
    for (let y = y0; y < this.view.y + this.view.h; y += step) {
      const line = document.createElementNS(SVG_NS, 'line');
      line.setAttribute('x1', String(this.view.x));
      line.setAttribute('y1', String(y));
      line.setAttribute('x2', String(this.view.x + this.view.w));
      line.setAttribute('y2', String(y));
      line.setAttribute('class', 'grid-line');
      frag.append(line);
    }
    this.layers.grid.append(frag);
  }

  /** Groups sit under the wires and the nodes, outermost first. */
  private renderGroups(): void {
    this.layers.groups.replaceChildren();
    const frag = document.createDocumentFragment();
    const groups = this.graph.groupsOn(this.graph.activeTab);
    const depth = (g: FlowEntry): number => {
      let d = 0;
      for (let p = g.g; p; p = this.graph.byId(p)?.g) d++;
      return d;
    };
    groups.sort((a, b) => depth(a) - depth(b));

    for (const g of groups) {
      const x = Number(g.x), y = Number(g.y), w = Number(g.w), h = Number(g.h);
      if (!(w > 0 && h > 0)) continue;
      const style = (g.style as Record<string, unknown> | undefined) ?? {};
      const selected = this.graph.selection.has(g.id);

      const box = document.createElementNS(SVG_NS, 'g');
      box.setAttribute('class', `group${selected ? ' selected' : ''}`);
      box.setAttribute('data-group-id', g.id);

      const rect = document.createElementNS(SVG_NS, 'rect');
      rect.setAttribute('x', String(x));
      rect.setAttribute('y', String(y));
      rect.setAttribute('width', String(w));
      rect.setAttribute('height', String(h));
      rect.setAttribute('rx', '8');
      rect.setAttribute('class', 'group-box');
      if (typeof style.fill === 'string' && style.fill) {
        rect.style.fill = style.fill;
        rect.style.fillOpacity = '0.2';
      }
      if (typeof style.stroke === 'string' && style.stroke) rect.style.stroke = style.stroke;
      box.append(rect);

      const head = document.createElementNS(SVG_NS, 'rect');
      head.setAttribute('x', String(x));
      head.setAttribute('y', String(y));
      head.setAttribute('width', String(w));
      head.setAttribute('height', String(GROUP_HEAD));
      head.setAttribute('class', 'group-head');
      box.append(head);

      if (style.label !== false && g.name) {
        const label = document.createElementNS(SVG_NS, 'text');
        label.setAttribute('x', String(x + 10));
        label.setAttribute('y', String(y + GROUP_HEAD - 7));
        label.setAttribute('class', 'group-label');
        label.textContent = truncate(String(g.name), Math.max(8, Math.floor(w / 7)));
        box.append(label);
      }
      frag.append(box);
    }
    this.layers.groups.append(frag);
  }

  private renderWires(): void {
    this.layers.wires.replaceChildren();
    const frag = document.createDocumentFragment();
    const chips: SVGGElement[] = [];

    const tabs = [this.graph.activeTab, ...[...this.frames.values()].map((f) => f.body)];
    const drawn = tabs.flatMap((tab) => this.graph.wiresOn(tab).map((w) => ({ ...w, tab })));
    for (const { from, port, to, tab } of drawn) {
      const inK = this.graph.inIndex(from, port, to);
      const a = this.outAt(from, port);
      const b = this.inAt(to, inK);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', wirePath(a, b));
      const added = this.diff?.addedWires.has(wireKey(from.id, port, to.id));
      const lit = this.trace?.edges.get(`${from.id}>${to.id}`);
      const durable = this.graph.wireSettings(from.id, to.id).durable;
      path.setAttribute('class', `${added ? 'wire diff-added' : 'wire'}${durable ? ' durable' : ''}${lit === undefined ? '' : lit ? ' trace-on' : ' trace-side'}`);
      path.setAttribute('data-from', from.id);
      path.setAttribute('data-port', String(port));
      path.setAttribute('data-to', to.id);
      if (this.graph.ports(to)) path.setAttribute('data-in', String(inK));
      const lastSeen = this.probeValues?.get(`${from.id}.${this.graph.ports(from)?.out[port] ?? ''}>${to.id}.${this.graph.ports(to)?.in[inK] ?? ''}`);
      if (lastSeen !== undefined) {
        const tip = document.createElementNS(SVG_NS, 'title');
        tip.textContent = lastSeen;
        path.append(tip);
        path.setAttribute('data-last', lastSeen);
      }
      const wkey = `${from.id}.${this.graph.ports(from)?.out[port] ?? ''}>${to.id}.${this.graph.ports(to)?.in[inK] ?? ''}`;
      if (this.litWires.has(wkey)) path.classList.add('live');
      if (this.probed.has(`${tab}/${wkey}`)) path.classList.add('probed');

      // The type on the wire, when either end declares one: a chip at the
      // middle of the curve, and the whole story as the wire's tooltip. The chip
      // takes no pointer events, so it never gets between a press and the wire.
      const typed = wireTypeLabel(from, port, to, this.wireClashes);
      // A wire of a diagram has a name, the output it comes from, written at its
      // middle beside its type, so the diagram reads without hovering every wire.
      const wname = this.graph.isDiagramTab(tab) ? (this.graph.ports(from)?.out[port] ?? '') : '';
      if (typed || wname) {
        if (typed) {
          const tip = document.createElementNS(SVG_NS, 'title');
          tip.textContent = typed.title;
          path.append(tip);
          path.setAttribute('data-type-state', typed.state);
        }
        frag.append(path);

        const mid = wireMid(a, b);
        const text = truncate(wname && typed ? `${wname}: ${typed.text}` : (typed?.text ?? wname), 30);
        const width = Math.max(18, text.length * 6.4 + 12);
        const chip = document.createElementNS(SVG_NS, 'g');
        chip.setAttribute('class', `wire-type ${typed ? typed.state : 'name'}`);
        chip.setAttribute('data-wire-type', `${from.id}:${port}:${to.id}`);
        if (wname) chip.setAttribute('data-wire-name', wname);
        chip.setAttribute('transform', `translate(${mid.x} ${mid.y})`);
        const box = document.createElementNS(SVG_NS, 'rect');
        box.setAttribute('x', String(-width / 2));
        box.setAttribute('y', '-8');
        box.setAttribute('width', String(width));
        box.setAttribute('height', '16');
        box.setAttribute('rx', '8');
        const label = document.createElementNS(SVG_NS, 'text');
        label.setAttribute('text-anchor', 'middle');
        label.setAttribute('dy', '3.5');
        label.textContent = text;
        chip.append(box, label);
        chips.push(chip);
        continue;
      }

      frag.append(path);
    }
    // Chips go over every wire, so a wire crossing one never hides it.
    for (const c of chips) frag.append(c);

    // Wires that are gone, drawn faintly between where their ends are now, or
    // were, for an end that went with them.
    for (const { from, port, to } of this.diff?.removedWires ?? []) {
      if (from.z !== this.graph.activeTab || to.z !== this.graph.activeTab) continue;
      const total = Math.max(from.wires?.length ?? 0, port + 1);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', wirePath(outputPort(from, port, total), inputPort(to)));
      path.setAttribute('class', 'wire diff-removed');
      path.setAttribute('data-from', from.id);
      path.setAttribute('data-to', to.id);
      frag.append(path);
    }
    this.layers.wires.append(frag);
  }

  private renderNodes(): void {
    this.layers.nodes.replaceChildren();
    this.layers.inner.replaceChildren();
    const frag = document.createDocumentFragment();

    for (const ghost of this.diff?.ghosts ?? []) {
      if (ghost.z !== this.graph.activeTab || ghost.x === undefined) continue;
      const g = this.renderNode(ghost);
      g.classList.add('diff-removed');
      g.removeAttribute('data-node-id');
      g.setAttribute('data-ghost-id', ghost.id);
      frag.append(g);
    }
    for (const n of this.graph.visibleNodes(this.graph.activeTab)) {
      // An open sub-diagram is its frame, drawn a layer down.
      if (this.frames.has(n.id)) continue;
      frag.append(this.renderNode(n));
    }
    this.layers.nodes.append(frag);

    // What is inside each open sub-diagram: the nodes of its own tab, moved to sit
    // in its frame. They are the same entries, so everything that works on a node
    // works on them.
    const inner = document.createDocumentFragment();
    for (const f of this.frames.values()) {
      for (const n of this.graph.nodesOn(f.body)) {
        if (this.frames.has(n.id)) continue;
        const o = this.offsets.get(n.id) ?? { x: 0, y: 0 };
        const wrap = document.createElementNS(SVG_NS, 'g');
        wrap.setAttribute('class', 'inner-node');
        wrap.setAttribute('transform', `translate(${o.x} ${o.y})`);
        wrap.append(this.renderNode(n));
        inner.append(wrap);
      }
    }
    this.layers.inner.append(inner);
  }

  /** The frame of each sub-diagram that is open: its outline, title, closing box and connector. */
  private renderFrames(): void {
    this.layers.frames.replaceChildren();
    const frag = document.createDocumentFragment();
    for (const f of this.frames.values()) {
      const n = this.graph.byId(f.id);
      if (!n) continue;
      const selected = this.graph.selection.has(f.id);
      const runState = this.nodeStates.get(f.id);
      const g = document.createElementNS(SVG_NS, 'g');
      g.setAttribute('class', `frame${selected ? ' selected' : ''}${n.d ? ' disabled' : ''}${runState ? ` run-${runState}` : ''}${this.breakpoints.has(f.id) ? ' has-break' : ''}`);
      g.setAttribute('data-node-id', f.id);
      g.setAttribute('data-frame', f.id);
      g.setAttribute('data-depth', String(f.depth));

      const rect = (cls: string, x: number, y: number, w: number, h: number, rx = 0): SVGRectElement => {
        const r = document.createElementNS(SVG_NS, 'rect');
        r.setAttribute('x', String(x));
        r.setAttribute('y', String(y));
        r.setAttribute('width', String(w));
        r.setAttribute('height', String(h));
        if (rx) r.setAttribute('rx', String(rx));
        r.setAttribute('class', cls);
        return r;
      };
      g.append(rect('frame-bg', f.x, f.y, f.w, f.h, 8), rect('frame-head', f.x, f.y, f.w, FRAME_HEAD, 8), rect('frame-border', f.x, f.y, f.w, f.h, 8));

      const title = document.createElementNS(SVG_NS, 'text');
      title.setAttribute('x', String(f.x + 12));
      title.setAttribute('y', String(f.y + FRAME_HEAD - 9));
      title.setAttribute('class', 'frame-title');
      const bodyLabel = this.graph.byId(f.body)?.label;
      const own = this.graph.label(n);
      title.textContent = truncate(typeof bodyLabel === 'string' && bodyLabel && bodyLabel !== own ? `${own}  /  ${bodyLabel}` : own, Math.max(10, Math.floor((f.w - 60) / 7)));
      g.append(title);

      const close = document.createElementNS(SVG_NS, 'g');
      close.setAttribute('class', 'frame-toggle');
      close.setAttribute('data-collapse', f.id);
      close.setAttribute('role', 'button');
      close.setAttribute('aria-label', tr('canvas.close_node', { name: this.graph.label(n) }));
      const hit = rect('frame-toggle-box', f.x + f.w - FRAME_HEAD + 3, f.y + 3, FRAME_HEAD - 6, FRAME_HEAD - 6, 4);
      const bar = document.createElementNS(SVG_NS, 'path');
      bar.setAttribute('d', `M${f.x + f.w - FRAME_HEAD + 8} ${f.y + FRAME_HEAD / 2}h${FRAME_HEAD - 16}`);
      bar.setAttribute('class', 'frame-toggle-glyph');
      const tip = document.createElementNS(SVG_NS, 'title');
      tip.textContent = tr('canvas.close_subdiagram');
      close.append(hit, bar, tip);
      g.append(close);

      // The connector, on the frame's own edges, named.
      const ports = this.graph.ports(n);
      ports?.in.forEach((name, k) => {
        const p = this.inAt(n, k);
        const c = this.portCircle(p, 'port-in');
        c.setAttribute('data-in-port', name);
        c.setAttribute('data-node', n.id);
        const t = document.createElementNS(SVG_NS, 'text');
        t.setAttribute('x', String(p.x + 8));
        t.setAttribute('y', String(p.y + 3));
        t.setAttribute('class', 'port-name');
        t.textContent = name;
        g.append(c, t);
      });
      ports?.out.forEach((name, k) => {
        const p = this.outAt(n, k);
        const t = document.createElementNS(SVG_NS, 'text');
        t.setAttribute('x', String(p.x - 8));
        t.setAttribute('y', String(p.y + 3));
        t.setAttribute('text-anchor', 'end');
        t.setAttribute('class', 'port-name');
        t.textContent = name;
        const c = this.portCircle(p, 'port-out');
        c.setAttribute('data-port', String(k));
        c.setAttribute('data-node', n.id);
        g.append(t, c);
      });

      // Which node inside is each connector name, drawn as a faint dashed line, so
      // the way into the body and the way out of it can be followed with the eye.
      for (const m of this.graph.nodesOn(f.body)) {
        if (m.type !== 'dia-body-in' && m.type !== 'dia-body-out') continue;
        const key = typeof m.name === 'string' ? m.name : '';
        const o = this.offsets.get(m.id) ?? { x: 0, y: 0 };
        const mx = (m.x ?? 0) + o.x, my = (m.y ?? 0) + o.y;
        if (m.type === 'dia-body-in') {
          const k = ports?.in.indexOf(key) ?? -1;
          if (k < 0) continue;
          const p = this.inAt(n, k);
          g.append(this.bindLine(p.x, p.y, mx - NODE_WIDTH / 2, my));
        } else {
          const k = ports?.out.indexOf(key) ?? -1;
          if (k < 0) continue;
          const p = this.outAt(n, k);
          g.append(this.bindLine(mx + NODE_WIDTH / 2, my, p.x, p.y));
        }
      }
      frag.append(g);
    }
    this.layers.frames.append(frag);
  }

  private bindLine(x1: number, y1: number, x2: number, y2: number): SVGPathElement {
    const l = document.createElementNS(SVG_NS, 'path');
    l.setAttribute('d', `M${x1} ${y1}L${x2} ${y2}`);
    l.setAttribute('class', 'frame-bind');
    return l;
  }

  private renderNode(n: FlowEntry): SVGGElement {
    const d = this.graph.descriptor(n.type);
    const x = n.x ?? 0;
    const y = n.y ?? 0;
    const selected = this.graph.selection.has(n.id);

    const g = document.createElementNS(SVG_NS, 'g');
    const mark = this.diff?.marks.get(n.id);
    const runState = this.nodeStates.get(n.id);
    g.setAttribute('class', `node${selected ? ' selected' : ''}${n.d ? ' disabled' : ''}${mark ? ` diff-${mark}` : ''}${runState ? ` run-${runState}` : ''}${this.breakpoints.has(n.id) ? ' has-break' : ''}`);
    g.setAttribute('data-node-id', n.id);

    const H = this.graph.nodeHeight(n);
    const body = document.createElementNS(SVG_NS, 'rect');
    body.setAttribute('x', String(x - NODE_WIDTH / 2));
    body.setAttribute('y', String(y - H / 2));
    body.setAttribute('width', String(NODE_WIDTH));
    body.setAttribute('height', String(H));
    body.setAttribute('rx', '6');
    body.setAttribute('class', 'node-body');
    // Unknown types render grey rather than vanishing, so a flow using a node
    // this build does not implement is visibly wrong rather than silently
    // missing pieces.
    body.setAttribute('fill', d?.color ?? '#9e9e9e');
    g.append(body);

    // The node type's category color, as a strip down the left edge. It says
    // what kind of node this is without taking the label's background, so the
    // label reads the same in light and dark.
    const r = 6;
    const x0 = x - NODE_WIDTH / 2;
    const y0 = y - H / 2;
    const strip = document.createElementNS(SVG_NS, 'path');
    strip.setAttribute('d', `M${x0 + r} ${y0}H${x0 + 6}V${y0 + H}H${x0 + r}A${r} ${r} 0 0 1 ${x0} ${y0 + H - r}V${y0 + r}A${r} ${r} 0 0 1 ${x0 + r} ${y0}Z`);
    strip.setAttribute('class', 'node-strip');
    strip.setAttribute('fill', d?.color ?? '#9e9e9e');
    g.append(strip);

    if (!d) {
      const warn = document.createElementNS(SVG_NS, 'title');
      warn.textContent = tr('canvas.not_installed', { type: n.type });
      g.append(warn);
      g.classList.add('unknown');
    }

    // A FlowLab node wears the symbol its type is drawn with, at the middle of
    // its body between its input names and its output names.
    const symbol = this.graph.ports(n) && d?.icon ? SYMBOLS[d.icon] : undefined;
    if (symbol) {
      const sym = document.createElementNS(SVG_NS, 'path');
      sym.setAttribute('d', symbol);
      sym.setAttribute('class', 'node-symbol');
      sym.setAttribute('data-symbol', d!.icon);
      sym.setAttribute('transform', `translate(${x - 12 + 3} ${y - 12})`);
      g.append(sym);
    }

    const label = document.createElementNS(SVG_NS, 'text');
    label.setAttribute('x', String(x + 3));
    label.setAttribute('y', String(this.graph.ports(n) ? y - H / 2 - 5 : y + 4));
    label.setAttribute('class', 'node-label');
    label.setAttribute('text-anchor', 'middle');
    label.textContent = truncate(this.graph.label(n), 18);
    g.append(label);

    // Ports.
    const named = this.graph.ports(n);
    if (named) {
      named.in.forEach((name, k) => {
        const p = this.graph.inPos(n, k);
        const c = this.portCircle(p, 'port-in');
        c.setAttribute('data-in-port', name);
        c.setAttribute('data-node', n.id);
        const t = document.createElementNS(SVG_NS, 'text');
        t.setAttribute('x', String(p.x + 8));
        t.setAttribute('y', String(p.y + 3));
        t.setAttribute('class', 'port-name');
        t.textContent = name;
        g.append(c, t);
      });
    } else if (this.graph.inputCount(n) > 0) {
      const p = inputPort(n);
      g.append(this.portCircle(p, 'port-in'));
    }
    const outputs = this.graph.outputCount(n);
    for (let i = 0; i < outputs; i++) {
      const p = this.graph.outPos(n, i);
      if (named) {
        const t = document.createElementNS(SVG_NS, 'text');
        t.setAttribute('x', String(p.x - 8));
        t.setAttribute('y', String(p.y + 3));
        t.setAttribute('text-anchor', 'end');
        t.setAttribute('class', 'port-name');
        t.textContent = named.out[i] ?? '';
        g.append(t);
      }
      const c = this.portCircle(p, 'port-out');
      c.setAttribute('data-port', String(i));
      c.setAttribute('data-node', n.id);
      g.append(c);
    }

    // A sub-diagram with a body to open wears a small box at its corner that opens
    // it in place.
    if (this.canExpand(n)) {
      const open = document.createElementNS(SVG_NS, 'g');
      open.setAttribute('class', 'node-expand');
      open.setAttribute('data-expand', n.id);
      open.setAttribute('role', 'button');
      open.setAttribute('aria-label', tr('canvas.open_node', { name: this.graph.label(n) }));
      const box = document.createElementNS(SVG_NS, 'rect');
      box.setAttribute('x', String(x + NODE_WIDTH / 2 - 17));
      box.setAttribute('y', String(y - H / 2 + 3));
      box.setAttribute('width', '14');
      box.setAttribute('height', '14');
      box.setAttribute('rx', '3');
      const plus = document.createElementNS(SVG_NS, 'path');
      plus.setAttribute('d', `M${x + NODE_WIDTH / 2 - 13} ${y - H / 2 + 10}h6M${x + NODE_WIDTH / 2 - 10} ${y - H / 2 + 7}v6`);
      const tip = document.createElementNS(SVG_NS, 'title');
      tip.textContent = tr('canvas.open_subdiagram');
      open.append(box, plus, tip);
      g.append(open);
    }

    // The path of a traced message: how long it was in this node, and what
    // it looked like when it got there.
    const hit = this.trace?.nodes.get(n.id);
    if (hit) {
      g.classList.add('trace-hit', hit.onPath ? 'trace-path' : 'trace-side');
      if (hit.focus) g.classList.add('trace-focus');
      if (hit.hops.some((h) => h.error)) g.classList.add('trace-failed');
      const t = document.createElementNS(SVG_NS, 'text');
      t.setAttribute('x', String(x));
      t.setAttribute('y', String(y - NODE_HEIGHT / 2 - 6));
      t.setAttribute('text-anchor', 'middle');
      t.setAttribute('class', 'trace-time');
      t.setAttribute('data-trace-time', String(hit.nanos));
      t.textContent = `${fmtNanos(hit.nanos)}${hit.hops.length > 1 ? ` ×${hit.hops.length}` : ''}`;
      const tip = document.createElementNS(SVG_NS, 'title');
      tip.textContent = tipFor(hit);
      g.append(t, tip);
    }

    // How the last run of the flow tests went for this node.
    const test = this.testMarks.get(n.id);
    if (test) {
      const badge = document.createElementNS(SVG_NS, 'g');
      badge.setAttribute('class', `test-badge ${test.state}`);
      badge.setAttribute('data-test-mark', test.state);
      const ring = document.createElementNS(SVG_NS, 'circle');
      ring.setAttribute('cx', String(x + NODE_WIDTH / 2 - 2));
      ring.setAttribute('cy', String(y - NODE_HEIGHT / 2 + 2));
      ring.setAttribute('r', '8');
      const glyph = document.createElementNS(SVG_NS, 'text');
      glyph.setAttribute('x', String(x + NODE_WIDTH / 2 - 2));
      glyph.setAttribute('y', String(y - NODE_HEIGHT / 2 + 6));
      glyph.setAttribute('text-anchor', 'middle');
      glyph.textContent = test.state === 'pass' ? '✓' : '✕';
      const tip = document.createElementNS(SVG_NS, 'title');
      tip.textContent = test.title;
      badge.append(ring, glyph, tip);
      g.append(badge);
    }

    // Status badge from the runtime.
    const status = this.statuses.get(n.id);
    if (status) {
      const dot = document.createElementNS(SVG_NS, 'circle');
      dot.setAttribute('cx', String(x - NODE_WIDTH / 2 + 8));
      dot.setAttribute('cy', String(y + NODE_HEIGHT / 2 + 9));
      dot.setAttribute('r', '4');
      dot.setAttribute('class', `status-dot fill-${status.fill || 'grey'} shape-${status.shape || 'dot'}`);
      g.append(dot);

      const st = document.createElementNS(SVG_NS, 'text');
      st.setAttribute('x', String(x - NODE_WIDTH / 2 + 17));
      st.setAttribute('y', String(y + NODE_HEIGHT / 2 + 13));
      st.setAttribute('class', 'status-text');
      st.textContent = truncate(status.text, 24);
      g.append(st);
    }

    return g;
  }

  private portCircle(p: Point, cls: string): SVGCircleElement {
    const c = document.createElementNS(SVG_NS, 'circle');
    c.setAttribute('cx', String(p.x));
    c.setAttribute('cy', String(p.y));
    c.setAttribute('r', String(PORT_RADIUS));
    c.setAttribute('class', `port ${cls}`);
    return c;
  }

  private renderOverlay(): void {
    this.layers.overlay.replaceChildren();

    if (this.mode.kind === 'wire') {
      const from = this.graph.byId(this.mode.fromId);
      if (!from) return;
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', wirePath(this.outAt(from, this.mode.port), this.mode.cursor));
      path.setAttribute('class', 'wire wire-pending');
      this.layers.overlay.append(path);
      return;
    }

    if (this.mode.kind === 'marquee') {
      const { start, current } = this.mode;
      const r = document.createElementNS(SVG_NS, 'rect');
      r.setAttribute('x', String(Math.min(start.x, current.x)));
      r.setAttribute('y', String(Math.min(start.y, current.y)));
      r.setAttribute('width', String(Math.abs(current.x - start.x)));
      r.setAttribute('height', String(Math.abs(current.y - start.y)));
      r.setAttribute('class', 'marquee');
      this.layers.overlay.append(r);
    }
  }
}

/** The nearest and the farthest the canvas zooms: 400 and 5 percent. */
const ZOOM_MAX = 4;
const ZOOM_MIN = 0.05;

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v));
}

function truncate(s: string, max: number): string {
  return s.length <= max ? s : s.slice(0, max - 1) + '…';
}

export { snap };

function sameClashes(a: Map<string, string[]>, b: Map<string, string[]>): boolean {
  if (a.size !== b.size) return false;
  for (const [key, list] of a) {
    const other = b.get(key);
    if (!other || other.length !== list.length || other.some((m, i) => m !== list[i])) return false;
  }
  return true;
}
