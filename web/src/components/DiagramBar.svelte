<script lang="ts">
  // The bar over a diagram: run and stop, highlight execution, pause and step,
  // breakpoints, the panel link, and the list of reasons the diagram cannot run.
  import type { DiagramProblem } from '../lib/api';

  interface Props {
    runState: string;
    error: string;
    problems: DiagramProblem[];
    highlight: boolean;
    waiting: string[];
    hasSelection: boolean;
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
    onproblem: (nodeId: string) => void;
  }
  let { runState, error, problems, highlight, waiting, hasSelection, breakOnSelection, panelHref, nodeName, onrun, onstop, onhighlight, onpause, onresume, onstep, onbreak, onproblem }: Props = $props();

  const running = $derived(runState === 'running' || runState === 'paused');
  let listOpen = $state(false);
</script>

<div class="diagram-bar" data-test="diagram-bar">
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
  <span class="sep"></span>
  <a class="ghost link" data-test="diagram-panel" href={panelHref} target="_blank" rel="noopener">Open panel</a>
  <div class="spacer"></div>
  <span class="run-state mono" data-test="diagram-state" data-state={runState}>{runState}{waiting.length ? ` before ${nodeName(waiting[0]!.replace(/^(wire:|diagram:)/, ''))}` : ''}</span>
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
