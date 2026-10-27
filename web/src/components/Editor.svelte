<script lang="ts">
  // The editor: palette, tabs, canvas, the live dock, and the rail with review,
  // history, tests and trace. The canvas and the graph under it are the engine in
  // src/lib; this is what holds them together and what a person touches.
  import { onMount, tick } from 'svelte';
  import { type Api, ApiError, type Deployment, type DiffResult, type Descriptor, type NodeStat, type RuntimeEvent, type TestReport, type TraceView } from '../lib/api';
  import { Canvas } from '../lib/canvas';
  import { type DiffOverlay, overlayFrom, summarise } from '../lib/diff';
  import { describeImport, exportEntries, importEntries, parseEntries, toText, type ImportResult } from '../lib/flowio';
  import { Graph, type FlowEntry } from '../lib/graph';
  import { describe, type Conflict as ConflictInfo } from '../lib/history';
  import { search, type Hit } from '../lib/search';
  import { marksFrom } from '../lib/tests';
  import { overlayFrom as traceOverlay } from '../lib/trace';
  import { applyTheme, isDark } from '../lib/theme';
  import { IN_KEY, OUT_KEY, type WireProblem } from '../lib/wiretypes';
  import { twoFactorDialog } from '../lib/account';
  import { openDialog } from '../lib/dialogs';
  import { draftTestDialog, draftTestFromTraceDialog, editSuiteDialog } from '../lib/testdialogs';
  import AskText from './dialogs/AskText.svelte';
  import Conflict from './dialogs/Conflict.svelte';
  import Export from './dialogs/Export.svelte';
  import FlowProps from './dialogs/FlowProps.svelte';
  import GroupProps from './dialogs/GroupProps.svelte';
  import Import from './dialogs/Import.svelte';
  import Instance, { type InstanceOutcome } from './dialogs/Instance.svelte';
  import NodeDialog from './dialogs/NodeDialog.svelte';
  import Rollback from './dialogs/Rollback.svelte';
  import SubflowProps from './dialogs/SubflowProps.svelte';
  import WireProps from './dialogs/WireProps.svelte';
  import Debug, { type LogLine } from './panels/Debug.svelte';
  import History from './panels/History.svelte';
  import Review from './panels/Review.svelte';
  import SearchPanel from './panels/Search.svelte';
  import Runtime from './panels/Runtime.svelte';
  import Tests, { type TestsView } from './panels/Tests.svelte';
  import Trace, { type TraceState } from './panels/Trace.svelte';
  import Palette from './Palette.svelte';
  import TabBar from './TabBar.svelte';
  import TopBar from './TopBar.svelte';

  interface Props {
    api: Api;
    descriptors: Descriptor[];
    version: string;
    canSignOut: boolean;
    onsignout: () => void;
    onsessionlost: () => void;
  }
  let { api, descriptors, version, canSignOut, onsignout, onsessionlost }: Props = $props();

  // svelte-ignore state_referenced_locally
  const byType = new Map(descriptors.map((d) => [d.type, d]));
  const graph = new Graph(byType);
  // A second, read-only canvas for looking at a deployment from the history.
  // Separate from the working copy on purpose: viewing the past must never touch
  // what you're in the middle of editing.
  const viewGraph = new Graph(byType);

  let host: HTMLElement | undefined = $state();
  let viewHost: HTMLElement | undefined = $state();
  let canvas!: Canvas;
  let viewCanvas!: Canvas;

  /** Bumped whenever something the screen draws from the graph may have changed. */
  let rev = $state(0);
  const bump = () => { rev++; };

  let dark = $state(isDark());
  let live = $state(null as boolean | null);

  // ── What is on show ────────────────────────────────────────────────────────
  let viewing = $state(null as Deployment | null);
  let viewBanner = $state({ what: '', title: '' });
  let viewOverlay = $state.raw(null as DiffOverlay | null);
  let reviewDiff = $state.raw(null as DiffResult | null);
  let reviewOverlay = $state.raw(null as DiffOverlay | null);
  let reviewBusy = $state(false);
  let deploying = $state(false);

  /** The graph the tab bar and the canvas are showing right now. */
  const shown = $derived(viewing ? viewGraph : graph);
  const shownCanvas = () => (viewing ? viewCanvas : canvas);

  // Plain copies, made fresh each time, so a rename shows: the entries
  // themselves are edited in place and a screen can't see that.
  const flows = $derived.by(() => { void rev; return shown.tabs().map((t) => ({ id: t.id, label: String(t.label ?? 'Flow') })); });
  const subflows = $derived.by(() => {
    void rev;
    return shown.subflows().map((t) => ({ id: t.id, label: String(t.name || 'subflow'), color: typeof t.color === 'string' && t.color ? t.color : '#DDAA99' }));
  });
  const active = $derived.by(() => { void rev; return shown.activeTab; });
  const dirty = $derived.by(() => { void rev; return graph.dirty; });
  const selectionSize = $derived.by(() => { void rev; return shown.selection.size; });
  const changedScopes = $derived((viewing ? viewOverlay : reviewOverlay)?.scopes ?? new Set<string>());
  const tabState = $derived(viewing ? `viewing deployment ${viewing.seq}, read only` : dirty ? 'unsaved changes' : 'saved');
  const showFlowProps = $derived.by(() => { void rev; return !viewing && !shown.isSubflowScope(shown.activeTab) && shown.tabs().some((t) => t.id === shown.activeTab); });
  const showSubflowProps = $derived.by(() => { void rev; return !viewing && shown.isSubflowScope(shown.activeTab); });

  // ── A line of feedback over the canvas ─────────────────────────────────────
  // What a copy took, what an import added, why a group could not be made. It
  // goes by itself.
  let noticeText = $state('');
  let noticeTimer: number | undefined;
  function notice(message: string): void {
    noticeText = message;
    window.clearTimeout(noticeTimer);
    noticeTimer = window.setTimeout(() => { noticeText = ''; }, 5000);
  }

  // ── Dialogs for things on the canvas ───────────────────────────────────────
  async function openEditor(entry: FlowEntry): Promise<void> {
    if (viewing) return;
    // A template's own ports have nothing to edit: its shape is in its properties.
    if (entry.id.startsWith('@')) return;

    if (entry.type === 'group') {
      await openDialog<boolean, { graph: Graph; group: FlowEntry }>(GroupProps, { graph, group: entry });
      return;
    }
    const template = graph.subflowOfType(entry.type);
    if (template) {
      const outcome = await openDialog<InstanceOutcome, { entry: FlowEntry; template: FlowEntry }>(Instance, { entry, template });
      if (outcome?.kind === 'save') graph.updateNode(entry.id, { name: outcome.name, env: outcome.env });
      if (outcome?.kind === 'edit') openScope(template.id);
      return;
    }
    const result = await openDialog<{ props: Record<string, unknown> } | null, { graph: Graph; entry: FlowEntry; descriptor: Descriptor | undefined }>(
      NodeDialog, { graph, entry, descriptor: byType.get(entry.type) });
    if (result) graph.updateNode(entry.id, result.props);
  }

  /** A wire's settings: durable, and how long it keeps trying a refused message. */
  async function editWire(w: { from: FlowEntry; port: number; to: FlowEntry }): Promise<void> {
    if (viewing) return;
    if (w.from.id.startsWith('@') || w.to.id.startsWith('@')) {
      notice("A wire on a subflow's own input or output cannot be durable. Mark the wire between the nodes inside, or the one that leaves the instance.");
      return;
    }
    await openDialog<boolean, { graph: Graph; from: FlowEntry; port: number; to: FlowEntry }>(WireProps, { graph, from: w.from, port: w.port, to: w.to });
  }

  /** A flow's name, and deleting it. */
  async function editFlow(id: string): Promise<void> {
    const tab = graph.byId(id);
    if (!tab || tab.type !== 'tab' || viewing) return;
    const outcome = await openDialog<'saved' | 'deleted' | null, { graph: Graph; tab: FlowEntry }>(FlowProps, { graph, tab });
    if (outcome === 'deleted') {
      graph.selection.clear();
      bump();
      canvas.render();
      canvas.fit();
    }
  }

  /** Adds a configuration node of a type from the palette, once its dialog is filled in. */
  async function newConfig(d: Descriptor): Promise<void> {
    if (viewing) return;
    const draft: FlowEntry = { id: '', type: d.type };
    for (const p of d.props ?? []) if (p.default !== undefined) draft[p.name] = p.default;
    const result = await openDialog<{ props: Record<string, unknown> } | null, { graph: Graph; entry: FlowEntry; descriptor: Descriptor | undefined }>(
      NodeDialog, { graph, entry: draft, descriptor: d });
    if (!result) return;
    const made = graph.addConfig(d.type, { ...draft, ...result.props });
    notice(`Added ${made.name || d.paletteLabel || d.type}. Pick it from the dialog of any node that needs one.`);
  }

  const askName = (title: string, initial: string, test: string) =>
    openDialog<string | null, { title: string; label: string; initial: string; test: string }>(AskText, { title, label: 'Name', initial, test });

  /** Turns the selection into a subflow and puts an instance where it was. */
  async function selectionToSubflow(): Promise<void> {
    if (viewing || graph.selection.size === 0) return;
    const name = await askName('Selection to subflow', `Subflow ${graph.subflows().length + 1}`, 'selection-subflow');
    if (!name) return;
    const made = graph.selectionToSubflow(name);
    if (typeof made === 'string') {
      notice(made);
      return;
    }
    notice(`Made the subflow ${name}. Its template is in the tab bar.`);
    bump();
    canvas.render();
  }

  /** Shows a flow or a subflow on the canvas. */
  function openScope(id: string): void {
    if (viewing) closeView();
    graph.activeTab = id;
    graph.selection.clear();
    bump();
    canvas.render();
    canvas.fit();
  }

  async function editSubflow(id: string): Promise<void> {
    const tpl = graph.byId(id);
    if (!tpl) return;
    const outcome = await openDialog<'saved' | 'deleted' | null, { graph: Graph; template: FlowEntry }>(SubflowProps, { graph, template: tpl });
    if (outcome === 'deleted') {
      bump();
      canvas.render();
      canvas.fit();
    }
  }

  async function newSubflow(): Promise<void> {
    const name = await askName('New subflow', `Subflow ${graph.subflows().length + 1}`, 'new-subflow');
    if (!name) return;
    graph.addSubflow(name);
    bump();
    canvas.render();
    canvas.fit();
  }

  function pickTab(id: string): void {
    const g = shown;
    g.activeTab = id;
    g.selection.clear();
    shownCanvas().render();
    bump();
    shownCanvas().fit();
  }

  function addFlow(): void {
    const label = prompt('Name for the new flow', `Flow ${graph.tabs().length + 1}`);
    if (label) {
      graph.addTab(label);
      bump();
      canvas.render();
    }
  }

  // ── The debug dock ─────────────────────────────────────────────────────────
  //
  // The log keeps what it has shown, so a filter applies to what is already
  // there as well as to what arrives. Without that, narrowing the log to one
  // node would hide the line you were looking for and show you nothing until
  // the next message.
  const MAX_LOG = 250;
  let logItems: LogLine[] = $state.raw([]);
  let logSeq = 0;
  let scopeMode: 'all' | 'flow' | 'selected' = $state('all');
  let filterText = $state('');
  let topicDebug = $state(true);
  let topicError = $state(true);
  let topicDropped = $state(true);
  let cleared = $state(false);

  /** The node on the canvas an event is about. A node inside a subflow has an id of instance:node. */
  function canvasNodeOf(id: string): FlowEntry | undefined {
    if (!id) return undefined;
    return graph.byId(id) ?? graph.byId(id.split(':')[0]!);
  }

  function passes(item: LogLine): boolean {
    const topic = ['debug', 'error', 'dropped'].includes(item.e.topic) ? item.e.topic : 'other';
    if (topic === 'debug' && !topicDebug) return false;
    if (topic === 'error' && !topicError) return false;
    if (topic === 'dropped' && !topicDropped) return false;
    if (scopeMode !== 'all') {
      const node = canvasNodeOf(item.nodeId);
      if (!node) return false;
      if (scopeMode === 'flow' && node.z !== graph.activeTab) return false;
      if (scopeMode === 'selected' && !graph.selection.has(node.id)) return false;
    }
    const needle = filterText.trim().toLowerCase();
    if (needle !== '') {
      const label = canvasNodeOf(item.nodeId);
      const hay = `${item.e.topic} ${label ? graph.label(label) : ''} ${item.node} ${item.body}`.toLowerCase();
      if (!hay.includes(needle)) return false;
    }
    return true;
  }

  const logLines = $derived.by(() => {
    void rev;
    return logItems.filter(passes);
  });
  const logEmpty = $derived(
    cleared ? 'Cleared.'
      : logItems.length === 0 ? 'Waiting for events…'
        : `Nothing matches. ${logItems.length} message${logItems.length === 1 ? ' is' : 's are'} hidden by the filter.`,
  );

  function appendEvent(e: RuntimeEvent): void {
    // Status events paint the canvas rather than filling the log: a chatty node
    // would otherwise drown out the debug output that was asked for.
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

    let node = '';
    let body: string;
    switch (e.topic) {
      case 'debug':
        node = String(e.data.name || e.data.id || '');
        body = String(e.data.msg);
        break;
      case 'error':
        node = String(e.data.nodeId ?? '');
        body = String(e.data.error);
        break;
      case 'dropped':
        node = String(e.data.nodeId ?? '');
        body = `dropped a message (${String(e.data.policy)})`;
        break;
      default:
        body = JSON.stringify(e.data);
    }
    const nodeId = String(e.data.id ?? e.data.nodeId ?? '');
    cleared = false;
    logItems = [...logItems, { seq: ++logSeq, e, node, body, nodeId }].slice(-MAX_LOG);
  }

  // ── Runtime stats ──────────────────────────────────────────────────────────
  let stats: NodeStat[] = $state.raw([]);
  const nameOf = (id: string) => { const e = graph.byId(id); return e ? graph.label(e) : id; };

  /** Selects a node on the canvas, going to its flow first. */
  function selectNode(id: string): void {
    const entry = graph.byId(id);
    if (!entry) return;
    if (entry.z) graph.activeTab = entry.z;
    graph.selection = new Set([id]);
    bump();
    canvas.render();
  }

  // ── Review and deploy ──────────────────────────────────────────────────────
  /**
   * Puts a deploy's answer in the log and returns the warnings it logged, so a
   * caller that is about to reload the flows, which logs the file's warnings
   * itself, can tell load() which ones are already on screen.
   */
  function reportFailures(res: { failures?: { id: string; type: string; error: string }[]; warnings?: string[] }): Set<string> {
    // What the file got wrong that isn't one node's failure, such as a node
    // pointing at a config node that isn't in it, comes back with the deploy and
    // goes in the log the way it does when the editor loads.
    const said = new Set<string>();
    for (const w of res.warnings ?? []) {
      appendEvent({ topic: 'error', data: { error: w }, at: new Date().toISOString() });
      said.add(w);
    }
    // Per-node failures are not a failed deploy: the rest of the flow is
    // running. Reporting them rather than swallowing them is what stops somebody
    // wondering why one node does nothing.
    for (const f of res.failures ?? []) {
      appendEvent({ topic: 'error', data: { nodeId: f.id, error: `${f.type}: ${f.error}` }, at: new Date().toISOString() });
    }
    return said;
  }

  function endReview(): void {
    reviewDiff = null;
    reviewOverlay = null;
    deploying = false;
    canvas.setDiff(null);
    bump();
  }

  async function startReview(): Promise<void> {
    if (viewing) closeView();
    reviewBusy = true;
    try {
      const [diff, liveFlows] = await Promise.all([api.pendingDiff(graph.all()), api.flows()]);
      reviewDiff = diff;
      reviewOverlay = overlayFrom(diff, liveFlows.flows as FlowEntry[], graph.all());
      canvas.setDiff(reviewOverlay);
      bump();
    } catch (ex) {
      failed(ex, 'Could not compare with what is running');
      bump();
    } finally {
      reviewBusy = false;
    }
  }

  /** Deploys the working copy. Resolves false when it didn't happen. */
  async function deploy(note: string, from = graph.rev): Promise<boolean> {
    deploying = true;
    try {
      const res = await api.deploy(graph.all(), from, note);
      graph.rev = res.rev;
      graph.dirty = false;
      endReview();
      reportFailures(res);
      void refreshHistory();
      return true;
    } catch (ex) {
      deploying = false;
      if (ex instanceof ApiError && ex.status === 409) {
        return conflict(() => deploy(note, ''));
      }
      if (ex instanceof ApiError && ex.status === 422 && ex.body.tests) {
        // The deploy gate said no. The report goes where a run from here would,
        // failures marked on the nodes, so the fix starts at the node rather
        // than in a wall of text.
        showReport(ex.body.tests as TestReport);
      }
      if (ex instanceof ApiError && ex.status === 422 && Array.isArray(ex.body.wires)) {
        // Two ends of a wire that can never agree. Drawn on the canvas, so the
        // fix starts at the wire.
        canvas.setWireProblems(ex.body.wires as WireProblem[]);
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
    const info: ConflictInfo = { latest, theirChanges, baseKnown };
    const choice = await openDialog<'mine' | 'theirs' | null, { conflict: ConflictInfo }>(Conflict, { conflict: info });
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
      onsessionlost();
      return;
    }
    alert(`${what}: ${ex instanceof Error ? ex.message : ex}`);
  }

  // ── Tests ──────────────────────────────────────────────────────────────────
  let testsView: TestsView = $state.raw({ kind: 'info', note: '', text: 'Loading…' });

  async function refreshTests(note = ''): Promise<void> {
    try {
      const t = await api.tests();
      const text = t.count === 0
        ? 'No tests yet. Make one from a message in the debug dock, or write them with Edit.'
        : `${t.count} test${t.count === 1 ? '' : 's'} in ${t.file}${t.gate ? '. Deploys have to pass them.' : '.'}`;
      testsView = { kind: 'info', note, text };
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        failed(ex, 'Tests');
        return;
      }
      testsView = {
        kind: 'info', note: '',
        text: ex instanceof ApiError && ex.status === 403 ? 'This account cannot read the flow tests.' : 'Could not load the flow tests.',
      };
    }
  }

  function showReport(report: TestReport): void {
    testsView = { kind: 'report', report };
    canvas.setTestMarks(marksFrom(report));
  }

  async function runTests(): Promise<void> {
    testsView = { kind: 'running' };
    try {
      // The working copy, not what's deployed: the point is to find out before the deploy.
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

  // ── Tracing ────────────────────────────────────────────────────────────────
  let traceView: TraceState = $state.raw({ kind: 'empty' });
  /** The trace on show, so Replay and Make test know what they're about. */
  let shownTrace: { trace: TraceView; focus: number | null; path: number[] | null } | null = null;

  function drawTrace(): void {
    if (!shownTrace) return;
    traceView = { kind: 'trace', ...shownTrace };
    canvas.setTrace(traceOverlay(shownTrace.trace, shownTrace.focus, shownTrace.path));
  }

  async function showTrace(node: string, msgId: string): Promise<void> {
    try {
      const found = await api.findTrace(msgId, node);
      shownTrace = { trace: found.trace, focus: found.focus, path: found.path };
      // On the tab the Debug node is on, which is where the path ends.
      const at = found.trace.hops[found.focus];
      const entry = at ? graph.byId(at.canvas || at.node) : undefined;
      if (entry?.z && graph.activeTab !== entry.z) {
        graph.activeTab = entry.z;
        bump();
      }
      drawTrace();
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 404) {
        traceView = { kind: 'gone', text: ex.message };
        canvas.setTrace(null);
        return;
      }
      failed(ex, 'Could not load the trace');
    }
  }

  function clearTrace(): void {
    shownTrace = null;
    canvas.setTrace(null);
    traceView = { kind: 'empty' };
  }

  async function makeTraceTest(traceId: string): Promise<void> {
    try {
      const added = await draftTestFromTraceDialog(api, traceId);
      if (added) void refreshTests(`Added: ${added.join(', ')}`);
    } catch (ex) {
      failed(ex, 'Could not make the test');
    }
  }

  async function replayTrace(traceId: string): Promise<void> {
    traceView = { kind: 'replaying' };
    try {
      // The working copy, deployed or not: that's what a draft is.
      const res = await api.replay(traceId, graph.all());
      traceView = { kind: 'replay', result: res };
      if (res.trace) canvas.setTrace(traceOverlay(res.trace, null, null));
    } catch (ex) {
      drawTrace();
      failed(ex, 'Could not replay the message');
    }
  }

  // ── History ────────────────────────────────────────────────────────────────
  let history: Deployment[] = $state.raw([]);
  let historyCurrent = $state('');
  let historyMessage = $state('Loading…');

  async function refreshHistory(): Promise<void> {
    try {
      const res = await api.deployments(50);
      history = res.deployments;
      historyCurrent = res.current;
      historyMessage = 'Nothing deployed yet.';
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        failed(ex, 'History');
        return;
      }
      history = [];
      historyMessage = ex instanceof ApiError && ex.status === 403
        ? 'This account cannot read the deployment log.'
        : 'Could not load the deployment log.';
    }
  }

  async function rollback(d: Deployment): Promise<void> {
    const note = await openDialog<string | null, { deployment: Deployment; unsaved: boolean }>(Rollback, { deployment: d, unsaved: graph.dirty });
    if (note === null) return;
    const attempt = async (from: string): Promise<boolean> => {
      try {
        const res = await api.rollback(d.seq, from, note);
        const said = reportFailures(res);
        closeView();
        endReview();
        // Load what's running now, which is the rolled-back flows, rather than
        // patching the working copy, so the editor and the runtime agree. The
        // rollback's answer already carried the file's warnings and they are in
        // the log, so the reload leaves those out instead of saying each twice.
        await load(said);
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
      if (reviewDiff) endReview();
      viewGraph.load(cur.flows as FlowEntry[], cur.rev);
      viewOverlay = diff && old ? overlayFrom(diff, old.flows as FlowEntry[], cur.flows as FlowEntry[]) : null;
      // Open on the first tab with something changed on it, which is where
      // anybody looking at a deployment wants to be.
      const changedTab = viewGraph.tabs().find((t) => viewOverlay?.scopes.has(t.id));
      if (changedTab) viewGraph.activeTab = changedTab.id;

      viewing = d;
      viewBanner = {
        what: `${describe(d)}${d.note ? `: ${d.note}` : ''}. ` + (diff ? `${summarise(diff)} since #${previous?.seq}.` : 'The first deployment in the log.'),
        title: diff?.text ?? '',
      };
      await tick();
      viewCanvas.setDiff(viewOverlay);
      viewCanvas.fit();
      bump();
    } catch (ex) {
      failed(ex, `Could not open deployment ${d.seq}`);
    }
  }

  function closeView(): void {
    if (!viewing) return;
    viewing = null;
    viewOverlay = null;
    bump();
    canvas.render();
  }

  // ── Search ─────────────────────────────────────────────────────────────────
  //
  // One box over the canvas that looks in every flow and subflow. A hit takes
  // you there: the right tab, the node selected, the view centred on it.
  let searchOpen = $state(false);
  let searchQuery = $state('');
  let searchPanel: { focus: () => void } | undefined = $state();
  const hits: Hit[] = $derived.by(() => {
    void rev;
    return searchOpen ? search(graph, searchQuery) : [];
  });

  async function openSearch(): Promise<void> {
    searchOpen = true;
    await tick();
    searchPanel?.focus();
  }

  function closeSearch(): void {
    searchOpen = false;
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
    bump();
    canvas.render();
    if (isScope) canvas.fit(); else canvas.centerOn(entry.id);
  }

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
  function ours(e: ClipboardEvent): boolean {
    if (viewing || dialogOpen() || textEntry(e.target) || textEntry(document.activeElement)) return false;
    const onCanvas = document.activeElement?.classList.contains('canvas') ?? false;
    return onCanvas || !window.getSelection()?.toString();
  }

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

  function afterImport(result: ImportResult): void {
    graph.activeTab = result.show;
    graph.selection = new Set(result.added);
    bump();
    canvas.render();
    notice(describeImport(result));
  }

  function runImport(text: string): void {
    const parsed = parseEntries(text);
    if (!parsed.entries) { notice(parsed.error ?? 'That could not be read.'); return; }
    afterImport(importEntries(graph, parsed.entries, { scope: graph.activeTab, at: placeAt() }));
  }

  function onPaste(e: ClipboardEvent): void {
    if (!ours(e)) return;
    const text = e.clipboardData?.getData('text/plain') ?? '';
    const source = text !== '' ? text : held;
    if (source === '') return;
    e.preventDefault();
    runImport(source);
  }

  async function doImport(): Promise<void> {
    const result = await openDialog<ImportResult | null, { graph: Graph; at: { x: number; y: number } }>(Import, { graph, at: canvas.lastPointer() ?? canvas.viewCenter() });
    if (result) afterImport(result);
  }

  // Ctrl+F belongs to this page while the editor is up, because the browser's
  // own find cannot see inside the canvas.
  function onKeyDown(e: KeyboardEvent): void {
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && e.key.toLowerCase() === 'f' && !dialogOpen()) {
      e.preventDefault();
      void openSearch();
    }
  }

  // A close guard, because losing an afternoon of wiring to a stray Cmd-W is not
  // a mistake anyone should be able to make once.
  function onBeforeUnload(e: BeforeUnloadEvent): void {
    if (graph.dirty) e.preventDefault();
  }

  function toggleTheme(): void {
    dark = !dark;
    applyTheme(dark);
  }

  // ── Typed wires ────────────────────────────────────────────────────────────
  // The runtime says which wires can never agree, and the canvas draws them.
  // Asked only when a node in the working copy declares a schema, so a flow
  // without any never makes the call, and a little after the last edit, so
  // dragging a node is not a request per pixel.
  let typeTimer: number | undefined;
  let typesAsked = 0;
  function scheduleTypeCheck(): void {
    window.clearTimeout(typeTimer);
    typeTimer = window.setTimeout(() => void checkTypes(), 300);
  }
  async function checkTypes(): Promise<void> {
    const entries = graph.all() as FlowEntry[];
    if (!entries.some((e) => IN_KEY in e || OUT_KEY in e)) {
      canvas.setWireProblems([]);
      return;
    }
    const ask = ++typesAsked;
    try {
      const { wires } = await api.flowTypes(entries);
      // An answer to an older copy than the one on the canvas is stale.
      if (ask === typesAsked) canvas.setWireProblems(wires);
    } catch {
      // The marks are help, not the gate. A deploy makes the same check and says so.
    }
  }

  async function load(alreadyLogged?: Set<string>): Promise<void> {
    const doc = await api.flows();
    graph.load(doc.flows as FlowEntry[], doc.rev);
    bump();
    canvas.render();
    canvas.fit();
    scheduleTypeCheck();
    for (const w of doc.warnings ?? []) {
      if (alreadyLogged?.has(w)) continue;
      appendEvent({ topic: 'error', data: { error: w }, at: new Date().toISOString() });
    }
  }

  let statsTimer: number | undefined;
  let historyTimer: number | undefined;
  let disconnect: (() => void) | null = null;

  function teardown(): void {
    window.clearInterval(statsTimer);
    window.clearInterval(historyTimer);
    window.clearTimeout(typeTimer);
    window.clearTimeout(noticeTimer);
    disconnect?.();
    disconnect = null;
  }

  onMount(() => {
    canvas = new Canvas(host!, graph, {
      onEditNode: (entry) => void openEditor(entry),
      onSelectionChange: bump,
      onNotice: notice,
      onEditWire: (w) => void editWire(w),
    });
    viewCanvas = new Canvas(viewHost!, viewGraph, {
      onEditNode: () => undefined,
      onSelectionChange: () => undefined,
    }, { readOnly: true });

    graph.onChange(() => {
      // An edit while the review is open makes the review a lie about what would
      // be deployed. Close it; pressing Deploy again reviews the new state.
      if (reviewDiff) endReview();
      // So are test marks on a flow that has changed since the run.
      canvas.setTestMarks(null);
      bump();
      scheduleTypeCheck();
    });

    void load().catch((ex) => {
      if (ex instanceof ApiError && ex.status === 401) onsessionlost();
      else alert(`Could not load flows: ${ex}`);
    });

    const refresh = async () => {
      try {
        stats = (await api.stats()).nodes;
      } catch (ex) {
        if (ex instanceof ApiError && ex.status === 401) {
          teardown();
          onsessionlost();
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

    disconnect = api.connectEvents(appendEvent, (up) => { live = up; });
    return teardown;
  });
</script>

<svelte:document oncopy={(e) => onCopy(e, false)} oncut={(e) => onCopy(e, true)} onpaste={onPaste} onkeydown={onKeyDown} />
<svelte:window onbeforeunload={onBeforeUnload} />

<div class="editor">
  <TopBar
    {version} {live} {canSignOut} {dark}
    deployDisabled={!dirty || viewing !== null || reviewDiff !== null || reviewBusy}
    importDisabled={viewing !== null}
    onsearch={() => (searchOpen ? closeSearch() : void openSearch())}
    onimport={() => void doImport()}
    onexport={() => void openDialog<null, { graph: Graph }>(Export, { graph })}
    onfit={() => shownCanvas().fit()}
    ontheme={toggleTheme}
    ondeploy={() => void startReview()}
    onmfa={() => void twoFactorDialog(api)}
    onsignout={() => { teardown(); onsignout(); }}
  />

  <TabBar
    {flows} {subflows} {active} changed={changedScopes} editing={viewing === null}
    canMakeSubflow={selectionSize > 0} {showFlowProps} {showSubflowProps}
    state={tabState} {dirty}
    onpick={pickTab}
    onedit={(id) => void editFlow(id)}
    onaddflow={addFlow}
    onaddsubflow={() => void newSubflow()}
    ontosubflow={() => void selectionToSubflow()}
    onflowprops={() => void editFlow(shown.activeTab)}
    onsubflowprops={() => void editSubflow(shown.activeTab)}
  />

  <Palette
    {graph} {descriptors} {rev} editing={viewing === null}
    onplace={(d) => {
      const entry = canvas.place(d.type, { x: 200, y: 120 });
      if (entry) void openEditor(entry);
    }}
    onnewconfig={(d) => void newConfig(d)}
    oneditconfig={(c) => void openEditor(c)}
    ondeleteconfig={(c) => {
      if (viewing) return;
      const problem = graph.deleteConfig(c.id);
      if (problem) notice(`${String(c.name || c.type)}: ${problem}`);
    }}
  />

  <main class="canvas-host" class:viewing={viewing !== null} bind:this={host}>
    {#if noticeText}
      <div class="toast" role="status" data-test="notice">{noticeText}</div>
    {/if}
    <div class="view-host" hidden={viewing === null} bind:this={viewHost}></div>
    {#if viewing}
      <div class="view-banner" data-test="view-banner">
        <span class="what" title={viewBanner.title}>{viewBanner.what}</span>
        <button class="ghost" data-test="view-close" onclick={closeView}>Back to editing</button>
      </div>
    {/if}
    {#if searchOpen}
      <SearchPanel bind:this={searchPanel} bind:query={searchQuery} {hits} onclose={closeSearch} onreveal={reveal} />
    {/if}
  </main>

  <div class="dock">
    <Debug
      lines={logLines} total={logItems.length} empty={logEmpty}
      bind:scope={scopeMode} bind:text={filterText} bind:debug={topicDebug} bind:error={topicError} bind:dropped={topicDropped}
      onclear={() => { logItems = []; cleared = true; }}
      ontrace={(l) => void showTrace(String(l.e.data.id), String(l.e.data.msgId))}
      ontest={(l) => void makeTest(String(l.e.data.id), String(l.e.data.msgId))}
    />
    <Runtime nodes={stats} name={nameOf} onpick={selectNode} />
  </div>

  <aside class="rail">
    {#if reviewDiff}
      <Review diff={reviewDiff} {deploying} ondeploy={(note) => void deploy(note)} oncancel={endReview} />
    {/if}
    {#if traceView.kind !== 'empty'}
      <Trace view={traceView} {graph} {rev} onpick={selectNode} onmaketest={() => shownTrace && void makeTraceTest(shownTrace.trace.id)} onreplay={() => shownTrace && void replayTrace(shownTrace.trace.id)} onclear={clearTrace} onback={drawTrace} />
    {/if}
    <History
      items={history} current={historyCurrent} message={historyMessage}
      onchanges={(d, previous) => void openView(d, previous)}
      onrollback={(d) => void rollback(d)}
      onrefresh={() => void refreshHistory()}
    />
    <Tests view={testsView} onrun={() => void runTests()} onedit={() => void editTests()} onpick={selectNode} />
    {#if traceView.kind === 'empty'}
      <Trace view={traceView} {graph} {rev} onpick={selectNode} onmaketest={() => undefined} onreplay={() => undefined} onclear={clearTrace} onback={drawTrace} />
    {/if}
  </aside>
</div>
