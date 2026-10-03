// The flow canvas.
//
// Native SVG and pointer events. Node-RED uses D3 for this; the parts of D3 that
// are actually needed here — a viewBox transform and some drag maths — are about
// forty lines, and pulling in the library would cost more than it saves and
// would still have to be themed.

import {
  type FlowEntry, type Graph, type Point,
  NODE_HEIGHT, NODE_WIDTH, PORT_RADIUS, inputPort, outputPort, snap, wirePath,
} from './graph';
import { type DiffOverlay, wireKey } from './diff';

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
    wires: SVGGElement;
    nodes: SVGGElement;
    overlay: SVGGElement;
  };

  private view = { x: 0, y: 0, w: 1200, h: 800 };
  private mode: Mode = { kind: 'idle' };
  private statuses = new Map<string, { fill: string; shape: string; text: string }>();
  private diff: DiffOverlay | null = null;
  private readonly readOnly: boolean;
  private lastPress = { id: '', at: 0 };

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
      wires: this.group('layer-wires'),
      nodes: this.group('layer-nodes'),
      overlay: this.group('layer-overlay'),
    };
    this.svg.append(this.layers.grid, this.layers.wires, this.layers.nodes, this.layers.overlay);
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
      const entry = this.graph.addNode(type, at);
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

    const portEl = this.readOnly ? null : target.closest('[data-port]');
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
      const target = (e.target as Element).closest('[data-node-id]');
      if (target) {
        const toId = target.getAttribute('data-node-id')!;
        this.graph.connect(this.mode.fromId, this.mode.port, toId);
      }
    } else if (this.mode.kind === 'drag') {
      if (this.mode.moved) {
        // Snap once at the end rather than continuously, so dragging feels
        // smooth instead of stepping.
        this.graph.snapSelection();
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

    for (const n of this.graph.nodesOn(this.graph.activeTab)) {
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
      for (const n of this.graph.nodesOn(this.graph.activeTab)) this.graph.selection.add(n.id);
      this.cb.onSelectionChange();
      this.render();
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

  render(): void {
    this.renderGrid();
    this.renderWires();
    this.renderNodes();
    this.renderOverlay();
  }

  /** Fits the view to the nodes on the active tab. */
  fit(): void {
    const nodes = this.graph.nodesOn(this.graph.activeTab);
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

  private renderWires(): void {
    this.layers.wires.replaceChildren();
    const frag = document.createDocumentFragment();

    for (const { from, port, to } of this.graph.wiresOn(this.graph.activeTab)) {
      const total = this.graph.outputCount(from);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', wirePath(outputPort(from, port, total), inputPort(to)));
      const added = this.diff?.addedWires.has(wireKey(from.id, port, to.id));
      path.setAttribute('class', added ? 'wire diff-added' : 'wire');
      path.setAttribute('data-from', from.id);
      path.setAttribute('data-port', String(port));
      path.setAttribute('data-to', to.id);

      // Clicking a wire deletes it. Alt-click rather than plain click, so
      // brushing past one while selecting does not silently break a flow.
      path.addEventListener('click', (e) => {
        if (!e.altKey || this.readOnly) return;
        e.stopPropagation();
        this.graph.disconnect(from.id, port, to.id);
      });
      frag.append(path);
    }

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
    for (const n of this.graph.nodesOn(this.graph.activeTab)) {
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

    if (!d) {
      const warn = document.createElementNS(SVG_NS, 'title');
      warn.textContent = `${n.type} is not installed in this runtime`;
      g.append(warn);
      g.classList.add('unknown');
    }

    const label = document.createElementNS(SVG_NS, 'text');
    label.setAttribute('x', String(x));
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
