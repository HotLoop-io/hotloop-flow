// The flow canvas.
//
// Native SVG and pointer events. Node-RED uses D3 for this; the parts of D3 that
// are actually needed here — a viewBox transform and some drag maths — are about
// forty lines, and pulling in the library would cost more than it saves and
// would still have to be themed.

import {
  type FlowEntry, type Graph, type Point,
  GROUP_HEAD, NODE_HEIGHT, NODE_WIDTH, PORT_RADIUS, SUBFLOW_PREFIX, inputPort, outputPort, snap, wireMid, wirePath,
} from './graph';
import { type DiffOverlay, wireKey } from './diff';
import { type WireProblem, clashesByWire, wireTypeLabel } from './wiretypes';
import type { TestMark } from './tests';
import { type TraceOverlay, fmtNanos, tipFor } from './trace';

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

export interface CanvasCallbacks {
  onEditNode: (entry: FlowEntry) => void;
  onSelectionChange: () => void;
  /** Something the person should be told, such as why a group could not be made. */
  onNotice?: (message: string) => void;
  /** A wire was double-clicked. */
  onEditWire?: (wire: { from: FlowEntry; port: number; to: FlowEntry }) => void;
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
    wires: SVGGElement;
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
  private readonly readOnly: boolean;
  private lastPress = { id: '', at: 0 };
  /** Where the pointer last was over the canvas, in canvas coordinates. */
  private pointer: Point | null = null;

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
      wires: this.group('layer-wires'),
      nodes: this.group('layer-nodes'),
      overlay: this.group('layer-overlay'),
    };
    this.svg.append(this.layers.grid, this.layers.groups, this.layers.wires, this.layers.nodes, this.layers.overlay);
    host.append(this.svg);

    this.bindEvents();
    this.resize();
    new ResizeObserver(() => this.resize()).observe(host);
    graph.onChange(() => this.render());
  }

  private group(cls: string): SVGGElement {
    const g = document.createElementNS(SVG_NS, 'g');
    g.setAttribute('class', cls);
    return g;
  }

  private resize(): void {
    const rect = this.host.getBoundingClientRect();
    // The viewBox keeps its scale; only the visible extent changes, so
    // resizing the window pans rather than zooming.
    const scale = this.view.w / (this.svg.clientWidth || rect.width || 1);
    this.view.w = rect.width * scale;
    this.view.h = rect.height * scale;
    this.applyView();
  }

  private applyView(): void {
    this.svg.setAttribute('viewBox', `${this.view.x} ${this.view.y} ${this.view.w} ${this.view.h}`);
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
        this.graph.connect(this.mode.fromId, this.mode.port, toId);
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

    for (const n of this.graph.visibleNodes(this.graph.activeTab)) {
      const nx = n.x ?? 0, ny = n.y ?? 0;
      const left = nx - NODE_WIDTH / 2, right = nx + NODE_WIDTH / 2;
      const top = ny - NODE_HEIGHT / 2, bottom = ny + NODE_HEIGHT / 2;
      // Intersection rather than containment: dragging a box across a row of
      // nodes should catch them, not require enclosing them exactly.
      if (right >= x1 && left <= x2 && bottom >= y1 && top <= y2) {
        this.graph.selection.add(n.id);
      }
    }
    this.cb.onSelectionChange();
  }

  private onWheel(e: WheelEvent): void {
    e.preventDefault();
    const factor = e.deltaY > 0 ? 1.12 : 1 / 1.12;

    // Zoom about the cursor rather than the origin, so the thing under the
    // pointer stays under the pointer.
    const at = this.toCanvas(e.clientX, e.clientY);
    const nextW = clamp(this.view.w * factor, 300, 12000);
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
      if (!placed) this.cb.onNotice?.('A subflow cannot be placed inside itself, or inside one that holds it.');
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
    const x = n.type === 'group' ? Number(n.x) + Number(n.w) / 2 : n.x;
    const y = n.type === 'group' ? Number(n.y) + Number(n.h) / 2 : n.y;
    this.view.x = x - this.view.w / 2;
    this.view.y = y - this.view.h / 2;
    this.applyView();
  }

  render(): void {
    this.renderGrid();
    this.renderGroups();
    this.renderWires();
    this.renderNodes();
    this.renderOverlay();
  }

  /** Fits the view to the nodes on the active tab. */
  fit(): void {
    const nodes = this.graph.visibleNodes(this.graph.activeTab);
    if (nodes.length === 0) {
      this.view = { x: -100, y: -100, w: 1200, h: 800 };
      this.applyView();
      return;
    }
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const n of nodes) {
      minX = Math.min(minX, (n.x ?? 0) - NODE_WIDTH);
      maxX = Math.max(maxX, (n.x ?? 0) + NODE_WIDTH);
      minY = Math.min(minY, (n.y ?? 0) - NODE_HEIGHT * 2);
      maxY = Math.max(maxY, (n.y ?? 0) + NODE_HEIGHT * 2);
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
    const w = Math.max(maxX - minX, 400);
    this.view = { x: minX, y: minY, w, h: Math.max(w * aspect, maxY - minY) };
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

    for (const { from, port, to } of this.graph.wiresOn(this.graph.activeTab)) {
      const total = this.graph.outputCount(from);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', wirePath(outputPort(from, port, total), inputPort(to)));
      const added = this.diff?.addedWires.has(wireKey(from.id, port, to.id));
      const lit = this.trace?.edges.get(`${from.id}>${to.id}`);
      const durable = this.graph.wireSettings(from.id, to.id).durable;
      path.setAttribute('class', `${added ? 'wire diff-added' : 'wire'}${durable ? ' durable' : ''}${lit === undefined ? '' : lit ? ' trace-on' : ' trace-side'}`);
      path.setAttribute('data-from', from.id);
      path.setAttribute('data-port', String(port));
      path.setAttribute('data-to', to.id);

      // The type on the wire, when either end declares one: a chip at the
      // middle of the curve, and the whole story as the wire's tooltip. The chip
      // takes no pointer events, so it never gets between a press and the wire.
      const typed = wireTypeLabel(from, port, to, this.wireClashes);
      if (typed) {
        const tip = document.createElementNS(SVG_NS, 'title');
        tip.textContent = typed.title;
        path.append(tip);
        path.setAttribute('data-type-state', typed.state);
        frag.append(path);

        const mid = wireMid(outputPort(from, port, total), inputPort(to));
        const text = truncate(typed.text, 26);
        const width = Math.max(18, text.length * 6.4 + 12);
        const chip = document.createElementNS(SVG_NS, 'g');
        chip.setAttribute('class', `wire-type ${typed.state}`);
        chip.setAttribute('data-wire-type', `${from.id}:${port}:${to.id}`);
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
      frag.append(this.renderNode(n));
    }
    this.layers.nodes.append(frag);
  }

  private renderNode(n: FlowEntry): SVGGElement {
    const d = this.graph.descriptor(n.type);
    const x = n.x ?? 0;
    const y = n.y ?? 0;
    const selected = this.graph.selection.has(n.id);

    const g = document.createElementNS(SVG_NS, 'g');
    const mark = this.diff?.marks.get(n.id);
    g.setAttribute('class', `node${selected ? ' selected' : ''}${n.d ? ' disabled' : ''}${mark ? ` diff-${mark}` : ''}`);
    g.setAttribute('data-node-id', n.id);

    const body = document.createElementNS(SVG_NS, 'rect');
    body.setAttribute('x', String(x - NODE_WIDTH / 2));
    body.setAttribute('y', String(y - NODE_HEIGHT / 2));
    body.setAttribute('width', String(NODE_WIDTH));
    body.setAttribute('height', String(NODE_HEIGHT));
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
    const y0 = y - NODE_HEIGHT / 2;
    const strip = document.createElementNS(SVG_NS, 'path');
    strip.setAttribute('d', `M${x0 + r} ${y0}H${x0 + 6}V${y0 + NODE_HEIGHT}H${x0 + r}A${r} ${r} 0 0 1 ${x0} ${y0 + NODE_HEIGHT - r}V${y0 + r}A${r} ${r} 0 0 1 ${x0 + r} ${y0}Z`);
    strip.setAttribute('class', 'node-strip');
    strip.setAttribute('fill', d?.color ?? '#9e9e9e');
    g.append(strip);

    if (!d) {
      const warn = document.createElementNS(SVG_NS, 'title');
      warn.textContent = `${n.type} is not installed in this runtime`;
      g.append(warn);
      g.classList.add('unknown');
    }

    const label = document.createElementNS(SVG_NS, 'text');
    label.setAttribute('x', String(x + 3));
    label.setAttribute('y', String(y + 4));
    label.setAttribute('class', 'node-label');
    label.setAttribute('text-anchor', 'middle');
    label.textContent = truncate(this.graph.label(n), 18);
    g.append(label);

    // Ports.
    if (this.graph.inputCount(n) > 0) {
      const p = inputPort(n);
      g.append(this.portCircle(p, 'port-in'));
    }
    const outputs = this.graph.outputCount(n);
    for (let i = 0; i < outputs; i++) {
      const p = outputPort(n, i, outputs);
      const c = this.portCircle(p, 'port-out');
      c.setAttribute('data-port', String(i));
      c.setAttribute('data-node', n.id);
      g.append(c);
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
      const total = this.graph.outputCount(from);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', wirePath(outputPort(from, this.mode.port, total), this.mode.cursor));
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
