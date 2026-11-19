<script lang="ts">
  import { tr, trn } from '../../lib/i18n.svelte';
  // Click Trace on a debug message and the canvas lights the path it took. This
  // lists the same path a hop at a time: how long each node had the message and
  // what it looked like there. A trace can become a test, or be replayed into the
  // working copy to see whether a draft sends it the same way.
  import type { ReplayResult, TraceView } from '../../lib/api';
  import type { Graph } from '../../lib/graph';
  import { canvasId, fmtNanos, preview } from '../../lib/trace';

  export type TraceState =
    | { kind: 'empty' }
    | { kind: 'gone'; text: string }
    | { kind: 'replaying' }
    | { kind: 'trace'; trace: TraceView; focus: number | null; path: number[] | null }
    | { kind: 'replay'; result: ReplayResult };

  interface Props {
    view: TraceState;
    graph: Graph;
    rev: number;
    onpick: (id: string) => void;
    onmaketest: () => void;
    onreplay: () => void;
    onclear: () => void;
    onback: () => void;
  }
  let { view, graph, rev, onpick, onmaketest, onreplay, onclear, onback }: Props = $props();

  const hops = $derived.by(() => {
    void rev;
    if (view.kind !== 'trace') return [];
    const { trace, focus, path } = view;
    const onPath = new Set(path ?? trace.hops.map((h) => h.id));
    return trace.hops.map((h) => {
      const id = canvasId(h);
      const entry = graph.byId(id);
      return {
        h, id, onPath: onPath.has(h.id), focus: h.id === focus,
        name: entry ? graph.label(entry) : h.node,
        via: h.via === 'error' ? tr('panels.trace.via_error') : h.parents?.length ? tr('panels.trace.via_port', { n: h.port + 1 }) : h.entry === 'emit' ? tr('panels.trace.via_came_in') : tr('panels.trace.via_pressed'),
      };
    });
  });
</script>

<section class="section trace-section">
  <div class="section-head"><h3>{tr('panels.trace.trace')}</h3></div>
  <div class="section-body">
    <div class="trace" data-test="trace">
      {#if view.kind === 'empty'}
        <div class="empty">{tr('panels.trace.click_trace_on_a_debug')}</div>
      {:else if view.kind === 'gone'}
        <div class="empty" data-test="trace-gone">{view.text}</div>
      {:else if view.kind === 'replaying'}
        <div class="empty">{tr('panels.trace.replaying')}</div>
      {:else if view.kind === 'trace'}
        {@const total = view.trace.hops.reduce((t, h) => t + h.nanos, 0)}
        <div class="trace-head" data-test="trace-summary">{trn('panels.trace.summary', view.trace.hops.length, { time: fmtNanos(total) })}{view.trace.truncated ? tr('panels.trace.cut_short') : ''}</div>
        <div class="actions">
          <button class="ghost sm" data-test="trace-make-test" title={tr('panels.trace.make_a_flow_test_from')} onclick={onmaketest}>{tr('panels.trace.make_test')}</button>
          <button class="ghost sm" data-test="trace-replay" title={tr('panels.trace.put_this_message_into_the')} onclick={onreplay}>{tr('panels.trace.replay_into_draft')}</button>
          <button class="ghost sm" data-test="trace-clear" onclick={onclear}>{tr('panels.trace.clear')}</button>
        </div>
        {#each hops as x (x.h.id)}
          <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
          <div class="trace-hop" class:on-path={x.onPath} class:focus={x.focus} class:failed={!!x.h.error} data-test="trace-hop" data-node={x.h.node} data-hop={String(x.h.id)} onclick={() => onpick(x.id)}>
            <div class="meta">
              <span class="name">{x.name}</span>
              <span class="via">{x.via}</span>
              <span class="ns" data-test="trace-hop-time">{fmtNanos(x.h.nanos)}</span>
            </div>
            <pre class="diff-text trace-msg" data-test="trace-hop-msg">{preview(x.h.msg, x.h.cut, 160)}</pre>
            {#if x.h.error}<pre class="diff-text test-problem">{x.h.error}</pre>{/if}
          </div>
        {/each}
      {:else}
        {@const res = view.result}
        {#if res.problems?.length}
          <div class="test-summary fail" data-test="replay-summary">{tr('panels.trace.it_couldnt_be_replayed')}</div>
          {#each res.problems as p, i (i)}<pre class="diff-text test-problem">{p.message}</pre>{/each}
        {:else if res.same}
          <div class="test-summary pass" data-test="replay-summary">{tr('panels.trace.the_draft_sends_it_everywhere')}</div>
        {:else}
          {@const n = res.differences?.length ?? 0}
          <div class="test-summary fail" data-test="replay-summary">{trn('panels.trace.differs', n)}</div>
          {#each res.differences ?? [] as d, i (i)}
            <div class="test-result fail" data-test="replay-diff" data-node={d.node}>
              <div class="meta"><span class="kind test-fail">{tr('panels.trace.diff_' + d.kind)}</span><span class="name">{d.label}</span></div>
              {#each d.changes ?? [] as c, j (j)}<pre class="diff-text test-problem">{c}</pre>{/each}
              {#if d.kind === 'missing'}<pre class="diff-text test-problem">{tr('panels.trace.ended_here_before', { value: preview(d.before, false, 160) })}</pre>{/if}
              {#if d.kind === 'extra'}<pre class="diff-text test-problem">{tr('panels.trace.ends_here_now', { value: preview(d.after, false, 160) })}</pre>{/if}
            </div>
          {/each}
        {/if}
        <button class="ghost" data-test="replay-back" onclick={onback}>{tr('panels.trace.back_to_the_original_trace')}</button>
      {/if}
    </div>
  </div>
</section>
