// The editor view: palette, tabs, canvas, review and deploy, history, and the
// live sidebar.

import type { Api, Deployment, Descriptor, DiffResult, NodeStat, RuntimeEvent } from './api';
import { ApiError } from './api';
import { Canvas } from './canvas';
import { editNode } from './dialog';
import { type DiffOverlay, overlayFrom, summarise } from './diff';
import { Graph, type FlowEntry } from './graph';
import { confirmRollback, describe, resolveConflict } from './history';
import { setDarkMode } from './theme';

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

  const topbar = el('div', { class: 'topbar' },
    el('div', { class: 'brand' }, el('span', {}, 'Hot', el('span', { class: 'mark' }, 'Loop'), ' Flow'),
      el('span', { class: 'version' }, version)),
    el('div', { class: 'spacer' }),
    el('div', { class: 'conn mono' }, connDot, connText),
    fitBtn, themeBtn, deployBtn, signOut,
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
  });

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
    const result = await editNode(graph, entry, byType.get(entry.type));
    if (result) graph.updateNode(entry.id, result.props);
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
      for (const d of descriptors) {
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
          item.addEventListener('dragstart', (e) => {
            (e as DragEvent).dataTransfer?.setData('text/hotloop-flow-node', d.type);
          });
          // Double-click drops it at the centre of the view, for anyone who
          // would rather not drag.
          item.addEventListener('dblclick', () => {
            const entry = graph.addNode(d.type, { x: 200, y: 120 });
            void openEditor(entry);
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
    if (!viewing) {
      const add = el('button', { class: 'tab tab-add', title: 'Add a flow' }, '+');
      add.onclick = () => {
        const label = prompt('Name for the new flow', `Flow ${graph.tabs().length + 1}`);
        if (label) {
          graph.addTab(label);
          renderTabs();
          canvas.render();
        }
      };
      tabBar.append(add);
    }
    tabBar.append(el('div', { class: 'spacer' }));

    const state = el('div', { class: 'tab-state mono', 'data-test': 'tab-state' },
      viewing ? `viewing deployment ${viewing.seq}, read only`
        : graph.dirty ? 'unsaved changes' : 'saved');
    tabBar.append(state);
    deployBtn.disabled = !graph.dirty || viewing !== null || reviewing;
  }

  // ── Sidebar ────────────────────────────────────────────────────────────────
  const logBody = el('div', { class: 'log' }, el('div', { class: 'empty' }, 'Waiting for events…'));
  const statsBody = el('tbody');
  const reviewSlot = el('div', { class: 'review-slot' });
  const historyBody = el('div', { class: 'history', 'data-test': 'history' },
    el('div', { class: 'empty' }, 'Loading…'));

  function renderSidebar(): void {
    const clear = el('button', { class: 'ghost' }, 'Clear');
    clear.onclick = () => logBody.replaceChildren(el('div', { class: 'empty' }, 'Cleared.'));
    const refreshHistoryBtn = el('button', { class: 'ghost', title: 'Reload the deployment log' }, 'Refresh');
    refreshHistoryBtn.onclick = () => void refreshHistory();

    sidebar.replaceChildren(
      reviewSlot,
      el('div', { class: 'side-head' }, el('h3', {}, 'History'), el('div', { class: 'spacer' }), refreshHistoryBtn),
      historyBody,
      el('div', { class: 'side-head' }, el('h3', {}, 'Debug'), el('div', { class: 'spacer' }), clear),
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
    logBody.querySelector('.empty')?.remove();

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

    logBody.append(el('div', { class: 'log-line' },
      el('span', { class: 'log-time' }, new Date(e.at).toLocaleTimeString()),
      el('span', { class: `log-topic ${e.topic}` }, e.topic),
      el('span', { class: 'log-body' }, text)));

    while (logBody.children.length > MAX_LOG) logBody.firstChild?.remove();
    logBody.scrollTop = logBody.scrollHeight;
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
