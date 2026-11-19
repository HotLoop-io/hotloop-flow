<script lang="ts">
  import { tr, trn } from '../../lib/i18n.svelte';
  // One box over the canvas that looks in every flow and subflow. A hit takes
  // you there: the right tab, the node selected, the view centred on it.
  import type { Hit } from '../../lib/search';

  interface Props {
    query: string;
    hits: Hit[];
    onclose: () => void;
    onreveal: (h: Hit) => void;
  }
  let { query = $bindable(), hits, onclose, onreveal }: Props = $props();

  let box: HTMLInputElement | undefined = $state();
  export function focus(): void {
    box?.focus();
    box?.select();
  }
</script>

<div class="search-panel" data-test="search-panel">
  <div class="search-head">
    <input
      type="search" placeholder={tr('panels.search.search_every_flow')} data-test="search-input" bind:this={box} bind:value={query}
      onkeydown={(e) => {
        if (e.key === 'Escape') { e.preventDefault(); onclose(); }
        if (e.key === 'Enter' && hits[0]) { e.preventDefault(); onreveal(hits[0]); }
      }}
    />
    <button class="ghost icon" title={tr('panels.search.close')} aria-label={tr('panels.search.close')} data-test="search-close" onclick={onclose}>✕</button>
  </div>
  <div class="search-count mono" data-test="search-count">
    {query.trim() === '' ? '' : hits.length === 0 ? tr('panels.search.nothing_found') : trn(hits.length >= 200 ? 'panels.search.found_capped' : 'panels.search.found', hits.length)}
  </div>
  <div class="search-results" data-test="search-results">
    {#if query.trim() !== ''}
      {#each hits as h, i (i)}
        <button class="search-hit" data-test="search-hit" data-id={h.id} onclick={() => onreveal(h)}>
          <span class="hit-label">{h.label}</span>
          <span class="hit-meta mono">{h.type} · {h.scopeLabel}{h.config ? ' · ' + tr('panels.search.configuration') : ''}</span>
          <span class="hit-snippet mono">{h.field === 'name' ? '' : `${h.field}: ${h.snippet}`}</span>
        </button>
      {/each}
    {/if}
  </div>
</div>
