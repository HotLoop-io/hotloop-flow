<script lang="ts">
  // The few icons the editor uses, drawn as plain strokes. No icon font, nothing to fetch.
  interface Props { name: 'fit' | 'search' | 'plus' | 'x'; size?: number }
  let { name, size = 16 }: Props = $props();
</script>

<svg width={size} height={size} viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
  {#if name === 'fit'}
    <path d="M2 6V2h4M14 6V2h-4M2 10v4h4M14 10v4h-4" />
  {:else if name === 'search'}
    <circle cx="7" cy="7" r="4.5" /><path d="M10.5 10.5L14 14" />
  {:else if name === 'plus'}
    <path d="M8 3v10M3 8h10" />
  {:else}
    <path d="M3 3l10 10M13 3L3 13" />
  {/if}
</svg>
