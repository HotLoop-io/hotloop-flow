<script lang="ts">
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
        via: h.via === 'error' ? 'error' : h.parents?.length ? `port ${h.port + 1}` : h.entry === 'emit' ? 'came in' : 'pressed',
      };
    });
  });
</script>

<section class="section trace-section">
  <div class="section-head"><h3>Trace</h3></div>
  <div class="section-body">
    <div class="trace" data-test="trace">
      {#if view.kind === 'empty'}
        <div class="empty">Click Trace on a debug message to see the path it took.</div>
      {:else if view.kind === 'gone'}
        <div class="empty" data-test="trace-gone">{view.text}</div>
      {:else if view.kind === 'replaying'}
        <div class="empty">Replaying…</div>
      {:else if view.kind === 'trace'}
        {@const total = view.trace.hops.reduce((t, h) => t + h.nanos, 0)}
        <div class="trace-head" data-test="trace-summary">{view.trace.hops.length} hops, {fmtNanos(total)} in nodes{view.trace.truncated ? ', cut short: the trace hit its hop limit' : ''}</div>
        <div class="actions">
          <button class="ghost" data-test="trace-make-test" title="Make a flow test from this trace" onclick={onmaketest}>Make test</button>
          <button class="ghost" data-test="trace-replay" title="Put this message into the working copy and compare" onclick={onreplay}>Replay into draft</button>
          <button class="ghost" data-test="trace-clear" onclick={onclear}>Clear</button>
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
          <div class="test-summary fail" data-test="replay-summary">It couldn't be replayed.</div>
          {#each res.problems as p, i (i)}<pre class="diff-text test-problem">{p.message}</pre>{/each}
        {:else if res.same}
          <div class="test-summary pass" data-test="replay-summary">The draft sends it everywhere the original went, with the same message.</div>
        {:else}
          {@const n = res.differences?.length ?? 0}
          <div class="test-summary fail" data-test="replay-summary">The draft differs in {n} place{n === 1 ? '' : 's'}.</div>
          {#each res.differences ?? [] as d, i (i)}
            <div class="test-result fail" data-test="replay-diff" data-node={d.node}>
              <div class="meta"><span class="kind test-fail">{d.kind}</span><span class="name">{d.label}</span></div>
              {#each d.changes ?? [] as c, j (j)}<pre class="diff-text test-problem">{c}</pre>{/each}
              {#if d.kind === 'missing'}<pre class="diff-text test-problem">ended here before: {preview(d.before, false, 160)}</pre>{/if}
              {#if d.kind === 'extra'}<pre class="diff-text test-problem">ends here now: {preview(d.after, false, 160)}</pre>{/if}
            </div>
          {/each}
        {/if}
        <button class="ghost" data-test="replay-back" onclick={onback}>Back to the original trace</button>
      {/if}
    </div>
  </div>
</section>
