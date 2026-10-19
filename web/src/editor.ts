// The editor view: palette, tabs, canvas, review and deploy, history, and the
// live sidebar.

import type { Api, Deployment, Descriptor, DiffResult, NodeStat, RuntimeEvent } from './api';
import { ApiError } from './api';
import { Canvas } from './canvas';
import { editNode } from './dialog';
import { describeImport, exportEntries, importEntries, parseEntries, toText } from './flowio';
import { exportDialog, importDialog } from './flowio-ui';
import { askText } from './modal';
import { search, type Hit } from './search';
import { groupDialog, instanceDialog, subflowDialog } from './structure-ui';
import { type DiffOverlay, overlayFrom, summarise } from './diff';
import { Graph, type FlowEntry } from './graph';
import { confirmRollback, describe, resolveConflict } from './history';
import { setDarkMode } from './theme';
import { twoFactorDialog } from './account';
import { draftTestDialog, editSuiteDialog, marksFrom, renderReport } from './tests';
import type { TestReport } from './api';

export interface EditorHandles {
  destroy(): void;
}

export function mountEditor(
  root: HTMLElement,
  api: Api,
  descriptors: Descriptor[],
  version: string,
  onSignOut: (() => void) | null,
  onSessionLost: () => void,
): EditorHandles {
  const byType = new Map(descriptors.map((d) => [d.type, d]));
  const graph = new Graph(byType);

  root.replaceChildren();
  root.className = 'editor';

  // ── Chrome ─────────────────────────────────────────────────────────────────
  const connDot = el('span', { class: 'dot grey' });
  const connText = el('span', {}, 'connecting');
  const deployBtn = el('button', { disabled: 'true' }, 'Deploy') as HTMLButtonElement;
  const themeBtn = el('button', { class: 'icon', title: 'Toggle theme' },
    el('span', { id: 'theme-icon' }, '🌙'));
  const fitBtn = el('button', { class: 'icon', title: 'Fit to view' }, '⤢');
  // No Sign out without a login to go back to. The badge takes its place so
  // nobody forgets this editor is open to anyone who can reach it.
  const signOut = onSignOut
    ? el('button', { class: 'ghost' }, 'Sign out')
    : el('span', { class: 'no-auth mono', title: 'Started with HOTLOOP_FLOW_INSECURE: anyone who can reach this port can deploy.' }, 'no login');

  // Two-factor sign-in, for anybody who wants it. Only with a login to protect.
  const mfaBtn = onSignOut
    ? el('button', { class: 'ghost', 'data-test': 'mfa-open', title: 'Two-factor sign-in' }, 'Two-factor')
    : null;
  if (mfaBtn) mfaBtn.onclick = () => void twoFactorDialog(api);

  const searchBtn = el('button', { class: 'ghost', 'data-test': 'search-open', title: 'Search every flow (Ctrl+F)' }, 'Search');
  const importBtn = el('button', { class: 'ghost', 'data-test': 'import-open', title: 'Import flows from JSON or a file' }, 'Import');
  const exportBtn = el('button', { class: 'ghost', 'data-test': 'export-open', title: 'Export the selection, this flow, or everything' }, 'Export');

  const topbar = el('div', { class: 'topbar' },
    el('div', { class: 'brand' }, el('span', {}, 'Hot', el('span', { class: 'mark' }, 'Loop'), ' Flow'),
      el('span', { class: 'version' }, version)),
    el('div', { class: 'spacer' }),
    el('div', { class: 'conn mono' }, connDot, connText),
    searchBtn, importBtn, exportBtn, fitBtn, themeBtn, deployBtn, ...(mfaBtn ? [mfaBtn] : []), signOut,
  );

  const tabBar = el('div', { class: 'tabbar' });
  const palette = el('aside', { class: 'palette' });
  const canvasHost = el('main', { class: 'canvas-host' });
  const sidebar = el('aside', { class: 'sidebar' });

  const workspace = el('div', { class: 'workspace' }, palette, canvasHost, sidebar);
  root.append(topbar, tabBar, workspace);

  // ── Canvas ─────────────────────────────────────────────────────────────────
  const canvas = new Canvas(canvasHost, graph, {
    onEditNode: (entry) => openEditor(entry),
    onSelectionChange: () => renderTabs(),
    onNotice: (m) => notice(m),
  });

  // A line of feedback over the canvas: what a copy took, what an import added,
  // why a group could not be made. It goes by itself.
  const toast = el('div', { class: 'toast', hidden: '', role: 'status', 'data-test': 'notice' });
  canvasHost.append(toast);
  let toastTimer: number | undefined;
  function notice(message: string): void {
    toast.textContent = message;
    toast.hidden = false;
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(() => { toast.hidden = true; }, 5000);
  }

  // A second, read-only canvas for looking at a deployment from the history.
  // Separate from the working copy on purpose: viewing the past must never
  // touch what you're in the middle of editing.
  const viewGraph = new Graph(byType);
  const viewHost = el('div', { class: 'view-host', hidden: '' });
  canvasHost.append(viewHost);
  const viewCanvas = new Canvas(viewHost, viewGraph, {
    onEditNode: () => undefined,
    onSelectionChange: () => undefined,
  }, { readOnly: true });
  const viewBanner = el('div', { class: 'view-banner', hidden: '', 'data-test': 'view-banner' });
  canvasHost.append(viewBanner);
  let viewing: Deployment | null = null;
  let viewOverlay: DiffOverlay | null = null;
  let reviewOverlay: DiffOverlay | null = null;
  let reviewing = false;

  /** The graph the tab bar and the canvas are showing right now. */
  const shown = () => (viewing ? viewGraph : graph);
  const shownCanvas = () => (viewing ? viewCanvas : canvas);

  async function openEditor(entry: FlowEntry): Promise<void> {
    if (viewing) return;
    // A template's own ports have nothing to edit: its shape is in its properties.
    if (entry.id.startsWith('@')) return;

    if (entry.type === 'group') {
      await groupDialog(graph, entry);
      return;
    }
    const template = graph.subflowOfType(entry.type);
    if (template) {
      const outcome = await instanceDialog(entry, template);
      if (outcome?.kind === 'save') graph.updateNode(entry.id, { name: outcome.name, env: outcome.env });
      if (outcome?.kind === 'edit') openScope(template.id);
      return;
    }
    const result = await editNode(graph, entry, byType.get(entry.type));
    if (result) graph.updateNode(entry.id, result.props);
  }

  /** Shows a flow or a subflow on the canvas. */
  function openScope(id: string): void {
    if (viewing) closeView();
    graph.activeTab = id;
    graph.selection.clear();
    renderTabs();
    canvas.render();
    canvas.fit();
  }

  async function editSubflow(id: string): Promise<void> {
    const tpl = graph.byId(id);
    if (!tpl) return;
    const outcome = await subflowDialog(graph, tpl);
    if (outcome === 'deleted') {
      renderTabs();
      canvas.render();
      canvas.fit();
    }
  }

  async function newSubflow(): Promise<void> {
    const name = await askText('New subflow', 'Name', `Subflow ${graph.subflows().length + 1}`, 'new-subflow');
    if (!name) return;
    graph.addSubflow(name);
    renderTabs();
    canvas.render();
    canvas.fit();
  }

  // ── Palette ────────────────────────────────────────────────────────────────
  function renderPalette(): void {
    palette.replaceChildren(el('h3', {}, 'Palette'));

    const search = el('input', { type: 'search', placeholder: 'Filter…' }) as HTMLInputElement;
    palette.append(search);

    const list = el('div', { class: 'palette-list' });
    palette.append(list);

    const draw = (filter: string) => {
      list.replaceChildren();
      const groups = new Map<string, Descriptor[]>();
      // Subflow templates are palette entries too, made here because the
      // runtime never lists them: it reserves their type prefix.
      const all = [...descriptors, ...graph.subflows().map((t) => subflowDescriptor(t))];
      for (const d of all) {
        if (d.isConfig) continue;
        const hay = `${d.type} ${d.paletteLabel ?? ''} ${d.category}`.toLowerCase();
        if (filter && !hay.includes(filter.toLowerCase())) continue;
        const g = groups.get(d.category) ?? [];
        g.push(d);
        groups.set(d.category, g);
      }

      for (const cat of [...groups.keys()].sort()) {
        list.append(el('div', { class: 'palette-cat' }, cat));
        for (const d of groups.get(cat)!) {
          const item = el('div', {
            class: 'palette-item',
            draggable: 'true',
            title: d.help ?? d.type,
          },
            el('span', { class: 'swatch', style: `background:${d.color}` }),
            el('span', {}, d.paletteLabel ?? d.type),
          );
          item.setAttribute('data-palette-type', d.type);
          item.addEventListener('dragstart', (e) => {
            (e as DragEvent).dataTransfer?.setData('text/hotloop-flow-node', d.type);
          });
          // Double-click drops it at the centre of the view, for anyone who
          // would rather not drag.
          item.addEventListener('dblclick', () => {
            if (viewing) return;
            const entry = canvas.place(d.type, { x: 200, y: 120 });
            if (entry) void openEditor(entry);
          });
          list.append(item);
        }
      }
      if (list.childElementCount === 0) {
        list.append(el('div', { class: 'empty' }, 'Nothing matches.'));
      }
    };

    search.addEventListener('input', () => draw(search.value));
    draw('');
    redrawPalette = () => draw(search.value);
  }

  /** Set by renderPalette: redraws the list with the filter as it stands. */
  let redrawPalette: () => void = () => undefined;
  let paletteSignature = '';
  /** What the palette shows of the subflows, so it is only redrawn when that changes. */
  function subflowSignature(): string {
    return JSON.stringify(graph.subflows().map((t) => [t.id, t.name, t.color, t.category]));
  }

  function subflowDescriptor(t: FlowEntry): Descriptor {
    return {
      type: `subflow:${t.id}`,
      category: 'subflows',
      color: typeof t.color === 'string' && t.color ? t.color : '#DDAA99',
      icon: 'subflow',
      inputs: Array.isArray(t.in) ? (t.in as unknown[]).length : 0,
      outputs: Array.isArray(t.out) ? (t.out as unknown[]).length : 0,
      paletteLabel: String(t.name || 'subflow'),
      help: typeof t.info === 'string' && t.info ? t.info : 'A subflow',
      compatibility: { level: 'full' },
    };
  }

  // ── Tabs ───────────────────────────────────────────────────────────────────
  function renderTabs(): void {
    tabBar.replaceChildren();
    const g = shown();
    const overlay = viewing ? viewOverlay : reviewOverlay;
    for (const t of g.tabs()) {
      const active = t.id === g.activeTab;
      const changed = overlay?.scopes.has(t.id) ? ' has-diff' : '';
      const tab = el('button', { class: `tab${active ? ' active' : ''}${changed}` },
        String(t.label ?? 'Flow'));
      tab.onclick = () => {
        g.activeTab = t.id;
        g.selection.clear();
        shownCanvas().render();
        renderTabs();
        shownCanvas().fit();
      };
      tabBar.append(tab);
    }
    for (const t of g.subflows()) {
      const active = t.id === g.activeTab;
      const changed = overlay?.scopes.has(t.id) ? ' has-diff' : '';
      const tab = el('button', {
        class: `tab tab-subflow${active ? ' active' : ''}${changed}`,
        'data-test': 'tab-subflow', 'data-scope': t.id, title: 'Subflow',
      }, String(t.name || 'subflow'));
      tab.onclick = () => {
        g.activeTab = t.id;
        g.selection.clear();
        shownCanvas().render();
        renderTabs();
        shownCanvas().fit();
      };
      tabBar.append(tab);
    }
    if (!viewing) {
      const add = el('button', { class: 'tab tab-add', 'data-test': 'tab-add', title: 'Add a flow' }, '+');
      add.onclick = () => {
        const label = prompt('Name for the new flow', `Flow ${graph.tabs().length + 1}`);
        if (label) {
          graph.addTab(label);
          renderTabs();
          canvas.render();
        }
      };
      tabBar.append(add);
      const addSub = el('button', { class: 'tab tab-add', 'data-test': 'tab-add-subflow', title: 'Add a subflow' }, '+ subflow');
      addSub.onclick = () => void newSubflow();
      tabBar.append(addSub);
    }
    tabBar.append(el('div', { class: 'spacer' }));

    if (!viewing && g.isSubflowScope(g.activeTab)) {
      const props = el('button', { class: 'ghost', 'data-test': 'subflow-properties' }, 'Subflow properties');
      props.onclick = () => void editSubflow(g.activeTab);
      tabBar.append(props);
    }

    const state = el('div', { class: 'tab-state mono', 'data-test': 'tab-state' },
      viewing ? `viewing deployment ${viewing.seq}, read only`
        : graph.dirty ? 'unsaved changes' : 'saved');
    tabBar.append(state);
    deployBtn.disabled = !graph.dirty || viewing !== null || reviewing;
    importBtn.disabled = viewing !== null;

    // The palette follows the subflows, and the debug filter follows the
    // flow and selection it is scoped to.
    const sig = subflowSignature();
    if (sig !== paletteSignature) {
      paletteSignature = sig;
      redrawPalette();
    }
    refilterIfScopeMoved();
  }

  // ── Sidebar ────────────────────────────────────────────────────────────────
  const logBody = el('div', { class: 'log', 'data-test': 'debug-log' }, el('div', { class: 'empty' }, 'Waiting for events…'));

  // ── Debug filtering ──
  //
  // The log keeps what it has shown, so a filter applies to what is already
  // there as well as to what arrives. Without that, narrowing the log to one
  // node would hide the line you were looking for and show you nothing until
  // the next message.
  type Scope = 'all' | 'flow' | 'selected';
  interface LogItem { e: RuntimeEvent; text: string; nodeId: string }
  const logItems: LogItem[] = [];
  const topicsOn = new Set<string>(['debug', 'error', 'dropped', 'other']);
  let scopeMode: Scope = 'all';
  let filterText = '';
  let cleared = false;
  let scopeKey = '';

  const scopeSelect = el('select', { 'data-test': 'debug-scope', title: 'Which nodes to show messages from' },
    el('option', { value: 'all' }, 'All nodes'),
    el('option', { value: 'flow' }, 'This flow'),
    el('option', { value: 'selected' }, 'Selected nodes')) as HTMLSelectElement;
  const textFilter = el('input', { type: 'search', placeholder: 'Filter messages…', 'data-test': 'debug-filter' }) as HTMLInputElement;
  const countText = el('span', { class: 'mono filter-count', 'data-test': 'debug-count' });
  const topicBoxes = el('div', { class: 'topic-filter' });
  for (const t of ['debug', 'error', 'dropped']) {
    const box = el('input', { type: 'checkbox', 'data-test': `debug-topic-${t}` }) as HTMLInputElement;
    box.checked = true;
    box.onchange = () => {
      if (box.checked) topicsOn.add(t); else topicsOn.delete(t);
      redrawLog();
    };
    topicBoxes.append(el('label', {}, box, t));
  }
  scopeSelect.onchange = () => { scopeMode = scopeSelect.value as Scope; scopeKey = ''; redrawLog(); };
  textFilter.addEventListener('input', () => { filterText = textFilter.value.trim().toLowerCase(); redrawLog(); });
  const filterBar = el('div', { class: 'debug-filter' }, scopeSelect, textFilter, topicBoxes, countText);
  const statsBody = el('tbody');
  const reviewSlot = el('div', { class: 'review-slot' });
  const historyBody = el('div', { class: 'history', 'data-test': 'history' },
    el('div', { class: 'empty' }, 'Loading…'));
  const testsBody = el('div', { class: 'tests', 'data-test': 'tests' },
    el('div', { class: 'empty' }, 'Loading…'));

  function renderSidebar(): void {
    const clear = el('button', { class: 'ghost' }, 'Clear');
    clear.onclick = () => { logItems.length = 0; cleared = true; redrawLog(); };
    const refreshHistoryBtn = el('button', { class: 'ghost', title: 'Reload the deployment log' }, 'Refresh');
    refreshHistoryBtn.onclick = () => void refreshHistory();
    const runTestsBtn = el('button', {
      class: 'ghost', 'data-test': 'tests-run',
      title: 'Run the flow tests against what is on the canvas, deployed or not',
    }, 'Run');
    runTestsBtn.onclick = () => void runTests();
    const editTestsBtn = el('button', { class: 'ghost', 'data-test': 'tests-edit', title: 'Edit the flow tests' }, 'Edit');
    editTestsBtn.onclick = () => void editTests();

    sidebar.replaceChildren(
      reviewSlot,
      el('div', { class: 'side-head' }, el('h3', {}, 'History'), el('div', { class: 'spacer' }), refreshHistoryBtn),
      historyBody,
      el('div', { class: 'side-head' }, el('h3', {}, 'Tests'), el('div', { class: 'spacer' }), runTestsBtn, editTestsBtn),
      testsBody,
      el('div', { class: 'side-head' }, el('h3', {}, 'Debug'), el('div', { class: 'spacer' }), clear),
      filterBar,
      logBody,
      el('div', { class: 'side-head' }, el('h3', {}, 'Runtime')),
      el('div', { class: 'scroll-x' },
        el('table', {},
          el('thead', {}, (() => {
            const tr = el('tr');
            for (const h of ['Node', 'In', 'Out', 'Err', 'Queue']) {
              tr.append(el('th', h === 'Node' ? {} : { class: 'num' }, h));
            }
            return tr;
          })()),
          statsBody)),
    );
  }

  function updateStats(nodes: NodeStat[]): void {
    statsBody.replaceChildren();
    for (const n of nodes) {
      const ratio = n.queueCap > 0 ? n.queueLen / n.queueCap : 0;
      const meter = el('span', { class: 'meter' },
        el('span', {
          class: ratio > 0.8 ? 'hot' : ratio > 0.4 ? 'warn' : '',
          style: `width:${Math.min(100, Math.round(ratio * 100))}%`,
        }));

      const tr = el('tr');
      tr.append(
        el('td', { class: 'mono' }, graph.byId(n.nodeId) ? graph.label(graph.byId(n.nodeId)!) : n.nodeId),
        el('td', { class: 'num' }, String(n.received)),
        el('td', { class: 'num' }, String(n.sent)),
        el('td', { class: 'num' }, n.errors > 0
          ? el('strong', { style: 'color:var(--danger-text)' }, String(n.errors))
          : '0'),
        (() => { const td = el('td', { class: 'num' }, `${n.queueLen}`); td.append(meter); return td; })(),
      );
      // Clicking a row selects the node on the canvas, which is how you get
      // from "this node is erroring" to the node itself.
      tr.onclick = () => {
        const entry = graph.byId(n.nodeId);
        if (!entry) return;
        if (entry.z) graph.activeTab = entry.z;
        graph.selection = new Set([n.nodeId]);
        renderTabs();
        canvas.render();
      };
      statsBody.append(tr);
    }
  }

  const MAX_LOG = 250;
  function appendEvent(e: RuntimeEvent): void {
    // Status events paint the canvas rather than filling the log — a chatty
    // node would otherwise drown out the debug output that was asked for.
    if (e.topic === 'status') {
      const id = String(e.data.nodeId ?? '');
      if (e.data.cleared) canvas.setStatus(id, null);
      else canvas.setStatus(id, {
        fill: String(e.data.fill ?? 'grey'),
        shape: String(e.data.shape ?? 'dot'),
        text: String(e.data.text ?? ''),
      });
      return;
    }

    let text: string;
    switch (e.topic) {
      case 'debug': text = `${e.data.name || e.data.id}  ${e.data.msg}`; break;
      case 'error': text = `${e.data.nodeId ?? ''} ${e.data.error}`.trim(); break;
      case 'dropped':
        text = `${e.data.nodeId} dropped a message (${e.data.policy})`;
        break;
      default: text = JSON.stringify(e.data);
    }

    const nodeId = String(e.data.id ?? e.data.nodeId ?? '');
    const item: LogItem = { e, text, nodeId };
    logItems.push(item);
    while (logItems.length > MAX_LOG) logItems.shift();
    cleared = false;
    if (passes(item)) {
      logBody.querySelector('.empty')?.remove();
      logBody.append(logLine(item));
      while (logBody.children.length > MAX_LOG) logBody.firstChild?.remove();
      logBody.scrollTop = logBody.scrollHeight;
    }
    updateCount();
  }

  /** The node on the canvas an event is about. A node inside a subflow has an id of instance:node. */
  function canvasNodeOf(id: string): FlowEntry | undefined {
    if (!id) return undefined;
    return graph.byId(id) ?? graph.byId(id.split(':')[0]!);
  }

  function passes(item: LogItem): boolean {
    const topic = ['debug', 'error', 'dropped'].includes(item.e.topic) ? item.e.topic : 'other';
    if (!topicsOn.has(topic)) return false;
    if (scopeMode !== 'all') {
      const node = canvasNodeOf(item.nodeId);
      if (!node) return false;
      if (scopeMode === 'flow' && node.z !== graph.activeTab) return false;
      if (scopeMode === 'selected' && !graph.selection.has(node.id)) return false;
    }
    if (filterText !== '') {
      const label = canvasNodeOf(item.nodeId);
      const hay = `${item.e.topic} ${label ? graph.label(label) : ''} ${item.text}`.toLowerCase();
      if (!hay.includes(filterText)) return false;
    }
    return true;
  }

  function updateCount(): void {
    const shown = logItems.filter(passes).length;
    countText.textContent = shown === logItems.length ? `${shown}` : `${shown} of ${logItems.length}`;
  }

  function redrawLog(): void {
    logBody.replaceChildren();
    const shown = logItems.filter(passes);
    if (shown.length === 0) {
      logBody.append(el('div', { class: 'empty' },
        cleared ? 'Cleared.'
          : logItems.length === 0 ? 'Waiting for events…'
            : `Nothing matches. ${logItems.length} message${logItems.length === 1 ? ' is' : 's are'} hidden by the filter.`));
    } else {
      const frag = document.createDocumentFragment();
      for (const item of shown) frag.append(logLine(item));
      logBody.append(frag);
      logBody.scrollTop = logBody.scrollHeight;
    }
    updateCount();
  }

  /** The flow or selection a scoped log follows moved: draw it again. */
  function refilterIfScopeMoved(): void {
    if (scopeMode === 'all') return;
    const key = `${scopeMode}|${graph.activeTab}|${[...graph.selection].sort().join(',')}`;
    if (key === scopeKey) return;
    scopeKey = key;
    redrawLog();
  }

  function logLine(item: LogItem): HTMLElement {
    const { e, text } = item;
    const line = el('div', { class: 'log-line', 'data-topic': e.topic, 'data-node': item.nodeId },
      el('span', { class: 'log-time' }, new Date(e.at).toLocaleTimeString()),
      el('span', { class: `log-topic ${e.topic}` }, e.topic),
      el('span', { class: 'log-body' }, text));
    // What a Debug node showed can become a test: what came in at the
    // source, and what came out here. One click opens the draft, a second
    // adds it to the suite.
    if (e.topic === 'debug' && e.data.id && e.data.msgId) {
      const make = el('button', {
        class: 'ghost log-make-test', 'data-test': 'debug-make-test',
        title: 'Make a flow test from this message',
      }, 'Test');
      make.onclick = () => void makeTest(String(e.data.id), String(e.data.msgId));
      line.append(make);
    }
    return line;
  }

  // ── Review and deploy ──────────────────────────────────────────────────────
  //
  // Deploy doesn't deploy. It shows what deploying would change, drawn on the
  // canvas and listed in words, from the engine's own diff against what is
  // live, and asks for a note. The second button deploys. Nobody should push to
  // a line without seeing what they're pushing, and the note is what the next
  // person reads in the history when they're trying to work out why.

  function reportFailures(res: { failures?: { id: string; type: string; error: string }[] }): void {
    // Per-node failures are not a failed deploy: the rest of the flow is
    // running. Reporting them rather than swallowing them is what stops
    // somebody wondering why one node does nothing.
    for (const f of res.failures ?? []) {
      appendEvent({
        topic: 'error',
        data: { nodeId: f.id, error: `${f.type}: ${f.error}` },
        at: new Date().toISOString(),
      });
    }
  }

  function endReview(): void {
    reviewing = false;
    reviewOverlay = null;
    reviewSlot.replaceChildren();
    canvas.setDiff(null);
    renderTabs();
  }

  async function startReview(): Promise<void> {
    if (viewing) closeView();
    deployBtn.disabled = true;
    try {
      const [diff, live] = await Promise.all([api.pendingDiff(graph.all()), api.flows()]);
      showReview(diff, live.flows as FlowEntry[]);
    } catch (ex) {
      failed(ex, 'Could not compare with what is running');
      renderTabs();
    }
  }

  function showReview(diff: DiffResult, live: FlowEntry[]): void {
    reviewing = true;
    reviewOverlay = overlayFrom(diff, live, graph.all());
    canvas.setDiff(reviewOverlay);

    const note = el('textarea', {
      placeholder: 'What changed and why. This goes in the deployment log.',
      'data-test': 'deploy-note',
    }) as HTMLTextAreaElement;
    const confirmBtn = el('button', { 'data-test': 'deploy-confirm' }, 'Deploy') as HTMLButtonElement;
    const cancelBtn = el('button', { class: 'ghost', 'data-test': 'deploy-cancel' }, 'Keep editing');
    cancelBtn.onclick = () => endReview();
    confirmBtn.onclick = async () => {
      confirmBtn.disabled = true;
      confirmBtn.textContent = 'Deploying…';
      const ok = await deploy(note.value);
      if (!ok) {
        confirmBtn.disabled = false;
        confirmBtn.textContent = 'Deploy';
      }
    };

    reviewSlot.replaceChildren(el('div', { class: 'review', 'data-test': 'review' },
      el('h3', {}, 'Review and deploy'),
      el('div', { class: 'mono', 'data-test': 'review-summary' }, summarise(diff)),
      el('pre', { class: 'diff-text', 'data-test': 'review-text' }, diff.text),
      note,
      el('div', { class: 'actions' }, confirmBtn, cancelBtn)));
    renderTabs();
    note.focus();
  }

  /** Deploys the working copy. Resolves false when it didn't happen. */
  async function deploy(note: string, rev = graph.rev): Promise<boolean> {
    try {
      const res = await api.deploy(graph.all(), rev, note);
      graph.rev = res.rev;
      graph.dirty = false;
      endReview();
      reportFailures(res);
      void refreshHistory();
      return true;
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 409) {
        return conflict(() => deploy(note, ''));
      }
      if (ex instanceof ApiError && ex.status === 422 && ex.body.tests) {
        // The deploy gate said no. The report goes where a run from here
        // would, failures marked on the nodes, so the fix starts at the
        // node rather than in a wall of text.
        showReport(ex.body.tests as TestReport);
      }
      failed(ex, 'Deploy failed');
      return false;
    }
  }

  /**
   * Somebody else deployed in between. Overwriting silently would throw away
   * their work and reloading silently would throw away yours, so it's the
   * operator's call, made looking at what the other person actually changed.
   */
  async function conflict(overwrite: () => Promise<boolean>): Promise<boolean> {
    let latest: Deployment | undefined;
    let theirChanges: string | null = null;
    let baseKnown = false;
    try {
      const { deployments } = await api.deployments(100);
      latest = deployments[0];
      const base = deployments.find((d) => d.rev === graph.rev);
      baseKnown = base !== undefined;
      if (base && latest && base.seq !== latest.seq) {
        theirChanges = (await api.deploymentDiff(base.seq, latest.seq)).text;
      }
    } catch {
      // Not being able to show their changes is no reason to hide the choice.
    }
    const choice = await resolveConflict({ latest, theirChanges, baseKnown });
    if (choice === 'mine') return overwrite();
    if (choice === 'theirs') {
      endReview();
      await load();
      void refreshHistory();
      return true;
    }
    return false;
  }

  function failed(ex: unknown, what: string): void {
    if (ex instanceof ApiError && ex.status === 401) {
      teardown();
      onSessionLost();
      return;
    }
    alert(`${what}: ${ex instanceof Error ? ex.message : ex}`);
  }

  deployBtn.onclick = () => void startReview();

  // ── Tests ──────────────────────────────────────────────────────────────────

  async function refreshTests(note = ''): Promise<void> {
    try {
      const t = await api.tests();
      const what = t.count === 0
        ? 'No tests yet. Make one from a message in the debug sidebar, or write them with Edit.'
        : `${t.count} test${t.count === 1 ? '' : 's'} in ${t.file}${t.gate ? '. Deploys have to pass them.' : '.'}`;
      testsBody.replaceChildren(...(note ? [el('div', { class: 'test-summary pass', 'data-test': 'tests-note' }, note)] : []),
        el('div', { class: 'empty', 'data-test': 'tests-info' }, what));
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        failed(ex, 'Tests');
        return;
      }
      testsBody.replaceChildren(el('div', { class: 'empty' },
        ex instanceof ApiError && ex.status === 403
          ? 'This account cannot read the flow tests.'
          : 'Could not load the flow tests.'));
    }
  }

  function showReport(report: TestReport): void {
    renderReport(testsBody, report, (id) => {
      const entry = graph.byId(id);
      if (!entry) return;
      if (entry.z) graph.activeTab = entry.z;
      graph.selection = new Set([id]);
      renderTabs();
      canvas.render();
    });
    canvas.setTestMarks(marksFrom(report));
  }

  async function runTests(): Promise<void> {
    testsBody.replaceChildren(el('div', { class: 'empty' }, 'Running…'));
    try {
      // The working copy, not what's deployed: the point is to find out
      // before the deploy.
      showReport(await api.runTests(graph.all()));
    } catch (ex) {
      failed(ex, 'Could not run the flow tests');
      void refreshTests();
    }
  }

  async function makeTest(node: string, msgId: string): Promise<void> {
    try {
      const added = await draftTestDialog(api, node, msgId);
      if (added) void refreshTests(`Added: ${added.join(', ')}`);
    } catch (ex) {
      failed(ex, 'Could not make the test');
    }
  }

  async function editTests(): Promise<void> {
    try {
      if (await editSuiteDialog(api)) void refreshTests('Saved.');
    } catch (ex) {
      failed(ex, 'Could not edit the tests');
    }
  }

  // ── History ────────────────────────────────────────────────────────────────

  let history: Deployment[] = [];

  async function refreshHistory(): Promise<void> {
    try {
      const res = await api.deployments(50);
      history = res.deployments;
      renderHistory(res.current);
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        failed(ex, 'History');
        return;
      }
      historyBody.replaceChildren(el('div', { class: 'empty' },
        ex instanceof ApiError && ex.status === 403
          ? 'This account cannot read the deployment log.'
          : 'Could not load the deployment log.'));
    }
  }

  function renderHistory(current: string): void {
    historyBody.replaceChildren();
    if (history.length === 0) {
      historyBody.append(el('div', { class: 'empty' }, 'Nothing deployed yet.'));
      return;
    }
    history.forEach((d, i) => {
      const isCurrent = i === 0 && d.rev === current;
      const item = el('div', {
        class: `history-item${isCurrent ? ' current' : ''}`,
        'data-test': 'history-item', 'data-seq': String(d.seq),
      },
        el('div', { class: 'meta' },
          el('span', { class: 'seq' }, `#${d.seq}`),
          el('span', { class: `kind ${d.kind}` }, d.rollbackOf ? `${d.kind} to #${d.rollbackOf}` : d.kind),
          el('span', { class: 'who' }, d.user || 'no login'),
          el('span', { class: 'who' }, new Date(d.time).toLocaleString())),
      );
      if (d.note) item.append(el('p', { class: 'note', 'data-test': 'history-note' }, d.note));

      const actions = el('div', { class: 'actions' });
      const show = el('button', { class: 'ghost', 'data-test': 'history-changes' }, 'Changes');
      show.onclick = () => void openView(d, history[i + 1] ?? null);
      actions.append(show);
      if (!isCurrent) {
        const back = el('button', { class: 'ghost', 'data-test': 'history-rollback' }, 'Roll back');
        back.onclick = () => void rollback(d);
        actions.append(back);
      }
      item.append(actions);
      historyBody.append(item);
    });
  }

  async function rollback(d: Deployment): Promise<void> {
    const note = await confirmRollback(d, graph.dirty);
    if (note === null) return;
    const attempt = async (rev: string): Promise<boolean> => {
      try {
        const res = await api.rollback(d.seq, rev, note);
        reportFailures(res);
        closeView();
        endReview();
        // Load what's running now, which is the rolled-back flows, rather than
        // patching the working copy, so the editor and the runtime agree.
        await load();
        void refreshHistory();
        return true;
      } catch (ex) {
        if (ex instanceof ApiError && ex.status === 409) return conflict(() => attempt(''));
        failed(ex, 'Rollback failed');
        return false;
      }
    };
    await attempt(graph.rev);
  }

  // ── Looking at a deployment ────────────────────────────────────────────────

  async function openView(d: Deployment, previous: Deployment | null): Promise<void> {
    try {
      const [cur, old, diff] = await Promise.all([
        api.deployment(d.seq),
        previous ? api.deployment(previous.seq) : Promise.resolve(null),
        previous ? api.deploymentDiff(previous.seq, d.seq) : Promise.resolve(null),
      ]);
      if (reviewing) endReview();
      viewing = d;
      viewGraph.load(cur.flows as FlowEntry[], cur.rev);
      viewOverlay = diff && old ? overlayFrom(diff, old.flows as FlowEntry[], cur.flows as FlowEntry[]) : null;
      // Open on the first tab with something changed on it, which is where
      // anybody looking at a deployment wants to be.
      const changedTab = viewGraph.tabs().find((t) => viewOverlay?.scopes.has(t.id));
      if (changedTab) viewGraph.activeTab = changedTab.id;

      viewHost.hidden = false;
      canvasHost.classList.add('viewing');
      viewCanvas.setDiff(viewOverlay);
      viewCanvas.fit();

      const back = el('button', { class: 'ghost', 'data-test': 'view-close' }, 'Back to editing');
      back.onclick = () => closeView();
      const what = `${describe(d)}${d.note ? `: ${d.note}` : ''}. ` +
        (diff ? `${summarise(diff)} since #${previous?.seq}.` : 'The first deployment in the log.');
      viewBanner.replaceChildren(el('span', { class: 'what', title: diff?.text ?? '' }, what), back);
      viewBanner.hidden = false;
      renderTabs();
    } catch (ex) {
      failed(ex, `Could not open deployment ${d.seq}`);
    }
  }

  function closeView(): void {
    if (!viewing) return;
    viewing = null;
    viewOverlay = null;
    viewHost.hidden = true;
    viewBanner.hidden = true;
    canvasHost.classList.remove('viewing');
    renderTabs();
    canvas.render();
  }

  // ── Search ─────────────────────────────────────────────────────────────────
  //
  // One box over the canvas that looks in every flow and subflow. A hit takes
  // you there: the right tab, the node selected, the view centred on it.

  const searchInput = el('input', { type: 'search', placeholder: 'Search every flow…', 'data-test': 'search-input' }) as HTMLInputElement;
  const searchCount = el('div', { class: 'search-count mono', 'data-test': 'search-count' });
  const searchResults = el('div', { class: 'search-results', 'data-test': 'search-results' });
  const searchClose = el('button', { class: 'ghost icon', title: 'Close', 'data-test': 'search-close' }, '✕');
  const searchPanel = el('div', { class: 'search-panel', hidden: '', 'data-test': 'search-panel' },
    el('div', { class: 'search-head' }, searchInput, searchClose), searchCount, searchResults);
  canvasHost.append(searchPanel);
  let hits: Hit[] = [];

  function runSearch(): void {
    hits = search(graph, searchInput.value);
    searchResults.replaceChildren();
    if (searchInput.value.trim() === '') {
      searchCount.textContent = '';
      return;
    }
    searchCount.textContent = hits.length === 0 ? 'Nothing found.' : `${hits.length} found${hits.length >= 200 ? ', showing the first 200' : ''}`;
    for (const h of hits) {
      const row = el('button', { class: 'search-hit', 'data-test': 'search-hit', 'data-id': h.id },
        el('span', { class: 'hit-label' }, h.label),
        el('span', { class: 'hit-meta mono' }, `${h.type} · ${h.scopeLabel}${h.config ? ' · configuration' : ''}`),
        el('span', { class: 'hit-snippet mono' }, h.field === 'name' ? '' : `${h.field}: ${h.snippet}`));
      row.onclick = () => reveal(h);
      searchResults.append(row);
    }
  }

  function openSearch(): void {
    searchPanel.hidden = false;
    searchInput.focus();
    searchInput.select();
    runSearch();
  }

  function closeSearch(): void {
    searchPanel.hidden = true;
    canvas.render();
  }

  /** Goes to a hit. */
  function reveal(h: Hit): void {
    if (viewing) closeView();
    const entry = graph.byId(h.id);
    if (!entry) return;
    if (h.config) {
      // A configuration node is on no canvas, so its dialog is where it is.
      void openEditor(entry);
      return;
    }
    if (h.scope && graph.isScope(h.scope)) graph.activeTab = h.scope;
    const isScope = entry.type === 'tab' || entry.type === 'subflow';
    graph.selection = isScope ? new Set() : new Set([entry.id]);
    renderTabs();
    canvas.render();
    if (isScope) canvas.fit(); else canvas.centerOn(entry.id);
  }

  searchBtn.onclick = () => (searchPanel.hidden ? openSearch() : closeSearch());
  searchClose.onclick = () => closeSearch();
  searchInput.addEventListener('input', runSearch);
  searchInput.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); closeSearch(); }
    if (e.key === 'Enter' && hits[0]) { e.preventDefault(); reveal(hits[0]); }
  });
  // The graph changed under an open search: its results are out of date.
  graph.onChange(() => { if (!searchPanel.hidden) runSearch(); });

  // ── Copy, cut, paste, import and export ────────────────────────────────────

  const textEntry = (t: EventTarget | null) =>
    t instanceof Element && t.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]') !== null;
  const dialogOpen = () => document.querySelector('.dialog-backdrop') !== null;
  // The clipboard of this page, for a browser that will not hand the page its
  // own clipboard back. Used only when the real one comes back empty.
  let held = '';
  let lastPaste: { x: number; y: number } | null = null;

  /**
   * Whether a clipboard event is ours: nobody is typing, and either the canvas
   * has the focus or nothing on the page is selected as text. A stray text
   * selection elsewhere, left by a double-click that wandered, must not stop
   * Ctrl+C on a node from copying it.
   */
  const ours = (e: ClipboardEvent) => {
    if (viewing || dialogOpen() || textEntry(e.target) || textEntry(document.activeElement)) return false;
    const onCanvas = document.activeElement?.classList.contains('canvas') ?? false;
    return onCanvas || !(window.getSelection()?.toString());
  };

  const selectedNodes = () => [...graph.selection].filter((id) => !id.startsWith('@') && graph.byId(id));

  function onCopy(e: ClipboardEvent, cut: boolean): void {
    if (!ours(e) || selectedNodes().length === 0) return;
    const entries = exportEntries(graph, 'selection');
    const text = toText(entries);
    held = text;
    e.clipboardData?.setData('text/plain', text);
    e.preventDefault();
    const n = entries.filter((x) => x.type !== 'group' && x.type !== 'subflow' && !graph.isConfig(x)).length;
    if (cut) {
      graph.deleteSelection();
      notice(`Cut ${n} node${n === 1 ? '' : 's'}.`);
    } else {
      notice(`Copied ${n} node${n === 1 ? '' : 's'}.`);
    }
  }

  function placeAt(): { x: number; y: number } {
    const base = canvas.lastPointer() ?? canvas.viewCenter();
    // Pasting twice without moving the pointer would stack the second copy
    // exactly on the first, where nobody can tell there are two.
    const at = lastPaste && lastPaste.x === base.x && lastPaste.y === base.y
      ? { x: base.x + 20, y: base.y + 20 } : { ...base };
    lastPaste = { x: at.x, y: at.y };
    return at;
  }

  function runImport(text: string, intoNewFlow?: string): void {
    const parsed = parseEntries(text);
    if (!parsed.entries) { notice(parsed.error ?? 'That could not be read.'); return; }
    const result = importEntries(graph, parsed.entries, {
      scope: graph.activeTab, at: placeAt(),
      ...(intoNewFlow !== undefined ? { intoNewFlow } : {}),
    });
    afterImport(result);
  }

  function afterImport(result: ReturnType<typeof importEntries>): void {
    graph.activeTab = result.show;
    graph.selection = new Set(result.added);
    renderTabs();
    canvas.render();
    notice(describeImport(result));
  }

  const copyListener = (e: ClipboardEvent) => onCopy(e, false);
  const cutListener = (e: ClipboardEvent) => onCopy(e, true);
  const pasteListener = (e: ClipboardEvent) => {
    if (!ours(e)) return;
    const text = e.clipboardData?.getData('text/plain') ?? '';
    const source = text !== '' ? text : held;
    if (source === '') return;
    e.preventDefault();
    runImport(source);
  };
  document.addEventListener('copy', copyListener);
  document.addEventListener('cut', cutListener);
  document.addEventListener('paste', pasteListener);

  exportBtn.onclick = () => exportDialog(graph);
  importBtn.onclick = async () => {
    const result = await importDialog(graph, canvas.lastPointer() ?? canvas.viewCenter());
    if (result) afterImport(result);
  };

  // Ctrl+F belongs to this page while the editor is up, because the browser's
  // own find cannot see inside the canvas.
  const onSearchKey = (e: KeyboardEvent) => {
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && e.key.toLowerCase() === 'f' && !dialogOpen()) {
      e.preventDefault();
      openSearch();
    }
  };
  document.addEventListener('keydown', onSearchKey);

  // ── Wiring ─────────────────────────────────────────────────────────────────
  fitBtn.onclick = () => canvas.fit();
  themeBtn.onclick = () => {
    const dark = !document.body.classList.contains('dark-mode');
    setDarkMode(dark);
    try { localStorage.setItem('theme', dark ? 'dark' : 'light'); } catch { /* private mode */ }
    const icon = document.getElementById('theme-icon');
    if (icon) icon.textContent = dark ? '☀️' : '🌙';
  };
  if (onSignOut) signOut.onclick = () => { teardown(); onSignOut(); };

  graph.onChange(() => {
    // An edit while the review is open makes the review a lie about what
    // would be deployed. Close it; pressing Deploy again reviews the new state.
    if (reviewing) endReview();
    // So are test marks on a flow that has changed since the run.
    canvas.setTestMarks(null);
    renderTabs();
  });

  // A close guard, because losing an afternoon of wiring to a stray Cmd-W is
  // not a mistake anyone should be able to make once.
  const beforeUnload = (e: BeforeUnloadEvent) => {
    if (graph.dirty) e.preventDefault();
  };
  window.addEventListener('beforeunload', beforeUnload);

  async function load(): Promise<void> {
    const doc = await api.flows();
    graph.load(doc.flows as FlowEntry[], doc.rev);
    renderTabs();
    canvas.render();
    canvas.fit();
    for (const w of doc.warnings ?? []) {
      appendEvent({ topic: 'error', data: { error: w }, at: new Date().toISOString() });
    }
  }

  let statsTimer: number | undefined;
  let historyTimer: number | undefined;
  let disconnect: (() => void) | null = null;

  function teardown(): void {
    window.removeEventListener('beforeunload', beforeUnload);
    document.removeEventListener('keydown', onSearchKey);
    document.removeEventListener('copy', copyListener);
    document.removeEventListener('cut', cutListener);
    document.removeEventListener('paste', pasteListener);
    if (statsTimer) window.clearInterval(statsTimer);
    if (historyTimer) window.clearInterval(historyTimer);
    disconnect?.();
  }

  renderPalette();
  renderSidebar();
  renderTabs();

  void load().catch((ex) => {
    if (ex instanceof ApiError && ex.status === 401) onSessionLost();
    else alert(`Could not load flows: ${ex}`);
  });

  const refresh = async () => {
    try {
      const { nodes } = await api.stats();
      updateStats(nodes);
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        teardown();
        onSessionLost();
      }
    }
  };
  void refresh();
  statsTimer = window.setInterval(refresh, 2000);

  // Somebody else's deploy shows up in the history without anybody pressing
  // refresh. Every fifteen seconds is often enough to notice and rare enough
  // not to matter on an edge link.
  void refreshHistory();
  historyTimer = window.setInterval(() => void refreshHistory(), 15000);
  void refreshTests();

  disconnect = api.connectEvents(appendEvent, (up) => {
    connDot.className = `dot ${up ? 'green' : 'red'}`;
    connText.textContent = up ? 'live' : 'reconnecting';
  });

  return { destroy: teardown };
}

function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string> = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else node.setAttribute(k, v);
  }
  for (const c of children) node.append(c);
  return node;
}
