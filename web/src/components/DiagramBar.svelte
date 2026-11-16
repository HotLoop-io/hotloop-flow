<script lang="ts">
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
  }
  let { runState, error, problems, warnings, highlight, waiting, hasSelection, canExpand, expandedSelected, breakOnSelection, panelHref, nodeName, onrun, onstop, onhighlight, onpause, onresume, onstep, onbreak, onsub, onexpand, onproblem }: Props = $props();

  const running = $derived(runState === 'running' || runState === 'paused');
  let listOpen = $state(false);
  let notesOpen = $state(false);
</script>

<div class="diagram-bar" data-test="diagram-bar">
  <span class="bar-tag mono" title="This tab is a FlowLab diagram">FlowLab</span>
  <button class="primary" data-test="diagram-run" disabled={running || problems.length > 0} onclick={onrun}>Run</button>
  <button class="ghost" data-test="diagram-stop" disabled={!running} onclick={onstop}>Stop</button>
  <span class="sep"></span>
  <label class="check"><input type="checkbox" data-test="diagram-highlight" checked={highlight} onchange={(e) => onhighlight((e.currentTarget as HTMLInputElement).checked)} /> Highlight execution</label>
  <span class="sep"></span>
  <button class="ghost" data-test="diagram-pause" disabled={!running || runState === 'paused'} onclick={onpause}>Pause</button>
  <button class="ghost" data-test="diagram-resume" disabled={runState !== 'paused'} onclick={onresume}>Resume</button>
  <button class="ghost" data-test="diagram-step-into" disabled={runState !== 'paused'} onclick={() => onstep('into')}>Step into</button>
  <button class="ghost" data-test="diagram-step-over" disabled={runState !== 'paused'} onclick={() => onstep('over')}>Step over</button>
  <button class="ghost" data-test="diagram-step-out" disabled={runState !== 'paused'} onclick={() => onstep('out')}>Step out</button>
  <button class="ghost" data-test="diagram-break" disabled={!hasSelection} onclick={onbreak}>{breakOnSelection ? 'Clear breakpoint' : 'Break on selected'}</button>
  <button class="ghost" data-test="selection-to-subdiagram" title="Cut the selected nodes out into a sub-diagram, with its inputs and outputs worked out from the wires that cross the selection" disabled={!hasSelection || running} onclick={onsub}>Selection to sub-diagram</button>
  <button class="ghost" data-test="subdiagram-expand" title="Draw the selected sub-diagram open where it is, with its nodes and wires inside a frame, instead of going to its tab" disabled={!canExpand} onclick={onexpand}>{expandedSelected ? 'Close in place' : 'Open in place'}</button>
  <span class="sep"></span>
  <a class="ghost link" data-test="diagram-panel" href={panelHref} target="_blank" rel="noopener">Open panel</a>
  <div class="spacer"></div>
  <span class="run-state mono" data-test="diagram-state" data-state={runState}>{runState}{waiting.length ? ` before ${nodeName(waiting[0]!.replace(/^(wire:|diagram:)/, ''))}` : ''}</span>
  {#if warnings.length}
    <button class="ghost" data-test="diagram-warnings" title="What this diagram does that is worth knowing before you deploy it" onclick={() => (notesOpen = !notesOpen)}>{warnings.length} note{warnings.length === 1 ? '' : 's'}</button>
  {/if}
  {#if problems.length}
    <button class="ghost bad" data-test="diagram-problems" onclick={() => (listOpen = !listOpen)}>{problems.length} problem{problems.length === 1 ? '' : 's'}</button>
  {/if}
</div>
{#if error}
  <div class="diagram-error" data-test="diagram-error">{error}</div>
{/if}
{#if listOpen && problems.length}
  <ul class="diagram-problems" data-test="diagram-problem-list">
    {#each problems as p}
      <li><button class="ghost" onclick={() => p.node && onproblem(p.node)}>{p.node ? nodeName(p.node) : 'Diagram'}{p.port ? `, input ${p.port}` : ''}: {p.text}</button></li>
    {/each}
  </ul>
{/if}
{#if notesOpen && warnings.length}
  <ul class="diagram-problems" data-test="diagram-warning-list">
    {#each warnings as p}
      <li><button class="ghost" onclick={() => p.node && onproblem(p.node)}>{p.node ? nodeName(p.node) : 'Diagram'}: {p.text}</button></li>
    {/each}
  </ul>
{/if}
