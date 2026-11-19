<script lang="ts">
  import { fmtNumber, fmtTime, tr } from '../../lib/i18n.svelte';
  // What the runtime says as it runs: debug output, errors, and messages a full
  // queue dropped, with the filters that keep a busy flow readable. A debug line
  // can be traced through the canvas or made into a test.
  import type { RuntimeEvent } from '../../lib/api';

  export interface LogLine {
    seq: number;
    e: RuntimeEvent;
    /** The node it is about, as a person would name it. */
    node: string;
    /** What it said. */
    body: string;
    nodeId: string;
  }

  type Scope = 'all' | 'flow' | 'selected';

  interface Props {
    lines: LogLine[];
    total: number;
    empty: string;
    scope: Scope;
    text: string;
    debug: boolean;
    error: boolean;
    dropped: boolean;
    onclear: () => void;
    ontrace: (line: LogLine) => void;
    ontest: (line: LogLine) => void;
  }
  let { lines, total, empty, scope = $bindable(), text = $bindable(), debug = $bindable(), error = $bindable(), dropped = $bindable(), onclear, ontrace, ontest }: Props = $props();

  let log: HTMLElement | undefined = $state();
  $effect(() => {
    void lines.length;
    if (log) log.scrollTop = log.scrollHeight;
  });

  const canAct = (l: LogLine) => l.e.topic === 'debug' && !!l.e.data.id && !!l.e.data.msgId;
</script>

<section class="debug">
  <div class="section-head">
    <h3>{tr('panels.debug.debug')}</h3>
    <div class="spacer"></div>
    <button class="ghost sm" onclick={onclear}>{tr('panels.debug.clear')}</button>
  </div>
  <div class="debug-filter">
    <select data-test="debug-scope" title={tr('panels.debug.which_nodes_to_show_messages')} bind:value={scope}>
      <option value="all">{tr('panels.debug.all_nodes')}</option>
      <option value="flow">{tr('panels.debug.this_flow')}</option>
      <option value="selected">{tr('panels.debug.selected_nodes')}</option>
    </select>
    <input type="search" placeholder={tr('panels.debug.filter_messages')} data-test="debug-filter" bind:value={text} />
    <div class="topic-filter">
      <label><input type="checkbox" data-test="debug-topic-debug" bind:checked={debug} />{tr('panels.debug.debug_2')}</label>
      <label><input type="checkbox" data-test="debug-topic-error" bind:checked={error} />{tr('panels.debug.error')}</label>
      <label><input type="checkbox" data-test="debug-topic-dropped" bind:checked={dropped} />{tr('panels.debug.dropped')}</label>
    </div>
    <span class="mono filter-count" data-test="debug-count">{lines.length === total ? fmtNumber(lines.length) : tr('panels.debug.count_of', { n: lines.length, total })}</span>
  </div>
  <div class="log" data-test="debug-log" bind:this={log}>
    {#if lines.length === 0}
      <div class="empty">{empty}</div>
    {/if}
    {#each lines as l (l.seq)}
      <div class="log-line" data-topic={l.e.topic} data-node={l.nodeId}>
        <span class="log-time">{fmtTime(l.e.at)}</span>
        <span class="log-topic {l.e.topic}">{l.e.topic}</span>
        <span class="log-node" title={l.node}>{l.node}</span>
        <span class="log-body">{l.body}</span>
        {#if canAct(l)}
          <span class="acts">
            <button class="ghost log-trace" data-test="debug-trace" title={tr('panels.debug.light_the_path_this_message')} onclick={() => ontrace(l)}>{tr('panels.debug.trace')}</button>
            <button class="ghost log-make-test" data-test="debug-make-test" title={tr('panels.debug.make_a_flow_test_from')} onclick={() => ontest(l)}>{tr('panels.debug.test')}</button>
          </span>
        {/if}
      </div>
    {/each}
  </div>
</section>
