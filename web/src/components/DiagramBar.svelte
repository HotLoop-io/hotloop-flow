<script lang="ts">
  import { tr, trn } from '../lib/i18n.svelte';
  // The bar over a diagram: run and stop, highlight execution, pause and step,
  // breakpoints, the panel link, and the list of reasons the diagram cannot run.
  import type { DiagramProblem } from '../lib/api';

  interface Props {
    runState: string;
    error: string;
    problems: DiagramProblem[];
    warnings: DiagramProblem[];
    highlight: boolean;
    waiting: string[];
    hasSelection: boolean;
    /** A sub-diagram is selected, so it can be opened in place, and whether it is open now. */
    canExpand: boolean;
    expandedSelected: boolean;
    breakOnSelection: boolean;
    panelHref: string;
    nodeName: (id: string) => string;
    onrun: () => void;
    onstop: () => void;
    onhighlight: (on: boolean) => void;
    onpause: () => void;
    onresume: () => void;
    onstep: (mode: 'into' | 'over' | 'out') => void;
    onbreak: () => void;
    onsub: () => void;
    onexpand: () => void;
    onproblem: (nodeId: string) => void;
    /** Replay of the last run: whether it is playing, how far, the pace, and whether there is a run to play. */
    replaying: boolean;
    replayIndex: number;
    replayTotal: number;
    replaySpeed: number;
    canReplay: boolean;
    onreplay: () => void;
    onreplaystop: () => void;
    onreplayspeed: (ms: number) => void;
  }
  let { runState, error, problems, warnings, highlight, waiting, hasSelection, canExpand, expandedSelected, breakOnSelection, panelHref, nodeName, onrun, onstop, onhighlight, onpause, onresume, onstep, onbreak, onsub, onexpand, onproblem, replaying, replayIndex, replayTotal, replaySpeed, canReplay, onreplay, onreplaystop, onreplayspeed }: Props = $props();

  const running = $derived(runState === 'running' || runState === 'paused');
  let listOpen = $state(false);
  let notesOpen = $state(false);
</script>

<div class="diagram-bar" data-test="diagram-bar">
  <span class="bar-tag mono" title={tr('diagrambar.this_tab_is_a_flowlab')}>{tr('diagrambar.flowlab')}</span>
  <button class="primary" data-test="diagram-run" disabled={running || problems.length > 0} onclick={onrun}>{tr('diagrambar.run')}</button>
  <button class="ghost" data-test="diagram-stop" disabled={!running} onclick={onstop}>{tr('diagrambar.stop')}</button>
  <span class="sep"></span>
  <label class="check"><input type="checkbox" data-test="diagram-highlight" checked={highlight} onchange={(e) => onhighlight((e.currentTarget as HTMLInputElement).checked)} /> {tr('diagrambar.highlight_execution')}</label>
  <span class="sep"></span>
  <button class="ghost" data-test="diagram-pause" disabled={!running || runState === 'paused'} onclick={onpause}>{tr('diagrambar.pause')}</button>
  <button class="ghost" data-test="diagram-resume" disabled={runState !== 'paused'} onclick={onresume}>{tr('diagrambar.resume')}</button>
  <button class="ghost" data-test="diagram-step-into" disabled={runState !== 'paused'} onclick={() => onstep('into')}>{tr('diagrambar.step_into')}</button>
  <button class="ghost" data-test="diagram-step-over" disabled={runState !== 'paused'} onclick={() => onstep('over')}>{tr('diagrambar.step_over')}</button>
  <button class="ghost" data-test="diagram-step-out" disabled={runState !== 'paused'} onclick={() => onstep('out')}>{tr('diagrambar.step_out')}</button>
  <button class="ghost" data-test="diagram-break" disabled={!hasSelection} onclick={onbreak}>{breakOnSelection ? tr('diagrambar.clear_breakpoint') : tr('diagrambar.break_on_selected')}</button>
  <button class="ghost" data-test="selection-to-subdiagram" title={tr('diagrambar.cut_the_selected_nodes_out')} disabled={!hasSelection || running} onclick={onsub}>{tr('diagrambar.selection_to_subdiagram')}</button>
  <button class="ghost" data-test="subdiagram-expand" title={tr('diagrambar.draw_the_selected_subdiagram_open')} disabled={!canExpand} onclick={onexpand}>{expandedSelected ? tr('diagrambar.close_in_place') : tr('diagrambar.open_in_place')}</button>
  <span class="sep"></span>
  {#if replaying}
    <button class="primary" data-test="diagram-replay-stop" onclick={onreplaystop}>{tr('diagrambar.stop_replay')}</button>
    <span class="mono" data-test="diagram-replay-progress" data-index={replayIndex} data-total={replayTotal}>{tr('diagrambar.replaying', { i: replayIndex, n: replayTotal })}</span>
  {:else}
    <button class="ghost" data-test="diagram-replay" disabled={!canReplay} title={tr('diagrambar.replay_title')} onclick={onreplay}>{tr('diagrambar.replay_last_run')}</button>
  {/if}
  <label class="check">{tr('diagrambar.replay_speed')}
    <select data-test="diagram-replay-speed" value={String(replaySpeed)} onchange={(e) => onreplayspeed(Number((e.currentTarget as HTMLSelectElement).value))}>
      <option value="700">{tr('diagrambar.speed_slow')}</option>
      <option value="350">{tr('diagrambar.speed_normal')}</option>
      <option value="120">{tr('diagrambar.speed_fast')}</option>
      <option value="30">{tr('diagrambar.speed_very_fast')}</option>
    </select>
  </label>
  <span class="sep"></span>
  <a class="ghost link" data-test="diagram-panel" href={panelHref} target="_blank" rel="noopener">{tr('diagrambar.open_panel')}</a>
  <div class="spacer"></div>
  <span class="run-state mono" data-test="diagram-state" data-state={runState}>{tr('diagrambar.state_' + runState)}{waiting.length ? ' ' + tr('diagrambar.before_node', { name: nodeName(waiting[0]!.replace(/^(wire:|diagram:)/, '')) }) : ''}</span>
  {#if warnings.length}
    <button class="ghost" data-test="diagram-warnings" title={tr('diagrambar.what_this_diagram_does_that')} onclick={() => (notesOpen = !notesOpen)}>{trn('diagrambar.notes', warnings.length)}</button>
  {/if}
  {#if problems.length}
    <button class="ghost bad" data-test="diagram-problems" onclick={() => (listOpen = !listOpen)}>{trn('diagrambar.problems', problems.length)}</button>
  {/if}
</div>
{#if error}
  <div class="diagram-error" data-test="diagram-error">{error}</div>
{/if}
{#if listOpen && problems.length}
  <ul class="diagram-problems" data-test="diagram-problem-list">
    {#each problems as p}
      <li><button class="ghost" onclick={() => p.node && onproblem(p.node)}>{p.node ? nodeName(p.node) : tr('diagrambar.diagram')}{p.port ? tr('diagrambar.input_n', { n: p.port }) : ''}: {p.text}</button></li>
    {/each}
  </ul>
{/if}
{#if notesOpen && warnings.length}
  <ul class="diagram-problems" data-test="diagram-warning-list">
    {#each warnings as p}
      <li><button class="ghost" onclick={() => p.node && onproblem(p.node)}>{p.node ? nodeName(p.node) : tr('diagrambar.diagram')}: {p.text}</button></li>
    {/each}
  </ul>
{/if}
