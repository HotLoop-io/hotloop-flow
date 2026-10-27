<script lang="ts">
  // The deployment log: who deployed what, when and why. Any entry can be
  // looked at, with its changes drawn on a read-only canvas, or rolled back to.
  import type { Deployment } from '../../lib/api';

  interface Props {
    items: Deployment[];
    current: string;
    /** What to say when there is nothing to list. */
    message: string;
    onchanges: (d: Deployment, previous: Deployment | null) => void;
    onrollback: (d: Deployment) => void;
    onrefresh: () => void;
  }
  let { items, current, message, onchanges, onrollback, onrefresh }: Props = $props();
</script>

<section class="section">
  <div class="section-head">
    <h3>History</h3>
    <div class="spacer"></div>
    <button class="ghost sm" title="Reload the deployment log" onclick={onrefresh}>Refresh</button>
  </div>
  <div class="section-body">
    <div class="history" data-test="history">
      {#if items.length === 0}
        <div class="empty">{message}</div>
      {/if}
      {#each items as d, i (d.seq)}
        {@const isCurrent = i === 0 && d.rev === current}
        <div class="history-item" class:current={isCurrent} data-test="history-item" data-seq={String(d.seq)}>
          <div class="meta">
            <span class="seq">#{d.seq}</span>
            <span class="kind {d.kind}">{d.rollbackOf ? `${d.kind} to #${d.rollbackOf}` : d.kind}</span>
            <span class="who">{d.user || 'no login'}</span>
            <span class="who">{new Date(d.time).toLocaleString()}</span>
          </div>
          {#if d.note}<p class="note" data-test="history-note">{d.note}</p>{/if}
          <div class="actions">
            <button class="ghost" data-test="history-changes" onclick={() => onchanges(d, items[i + 1] ?? null)}>Changes</button>
            {#if !isCurrent}<button class="ghost" data-test="history-rollback" onclick={() => onrollback(d)}>Roll back</button>{/if}
          </div>
        </div>
      {/each}
    </div>
  </div>
</section>
