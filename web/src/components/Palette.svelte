<script lang="ts">
  // Every node type, grouped by what it is for, to drag onto the canvas or
  // double-click to place. Subflow templates are palette entries too, and so are
  // the configuration nodes this flow set already has.
  import type { Descriptor } from '../lib/api';
  import type { FlowEntry, Graph } from '../lib/graph';
  import Icon from './Icon.svelte';

  interface Props {
    graph: Graph;
    descriptors: Descriptor[];
    /** Changes whenever the graph does, so the lists follow it. */
    rev: number;
    editing: boolean;
    onplace: (d: Descriptor) => void;
    onnewconfig: (d: Descriptor) => void;
    oneditconfig: (c: FlowEntry) => void;
    ondeleteconfig: (c: FlowEntry) => void;
  }
  let { graph, descriptors, rev, editing, onplace, onnewconfig, oneditconfig, ondeleteconfig }: Props = $props();

  let filter = $state('');
  const byType = $derived(new Map(descriptors.map((d) => [d.type, d])));

  // Subflow templates are made here because the runtime never lists them: it
  // reserves their type prefix.
  function subflowDescriptor(t: FlowEntry): Descriptor {
    return {
      type: `subflow:${t.id}`,
      category: 'subflows',
      color: typeof t.color === 'string' && t.color ? t.color : '#DDAA99',
      icon: 'subflow',
      inputs: Array.isArray(t.in) ? (t.in as unknown[]).length : 0,
      outputs: Array.isArray(t.out) ? (t.out as unknown[]).length : 0,
      paletteLabel: String(t.name || 'subflow'),
      help: typeof t.info === 'string' && t.info ? t.info : 'A subflow',
      compatibility: { level: 'full' },
    };
  }

  const groups = $derived.by(() => {
    void rev;
    const f = filter.toLowerCase();
    const out = new Map<string, Descriptor[]>();
    for (const d of [...descriptors, ...graph.subflows().map(subflowDescriptor)]) {
      const hay = `${d.type} ${d.paletteLabel ?? ''} ${d.category}`.toLowerCase();
      if (f && !hay.includes(f)) continue;
      // Configuration nodes are shared by id and sit on no canvas, so they are
      // listed together and added by a dialog rather than dropped.
      const cat = d.isConfig ? 'configuration' : d.category;
      const g = out.get(cat) ?? [];
      g.push(d);
      out.set(cat, g);
    }
    return [...out.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  });

  const inUse = $derived.by(() => {
    void rev;
    const f = filter.toLowerCase();
    // Plain copies, made fresh each time: the nodes are edited in place, and a
    // list drawn from the node itself would never see a rename.
    return graph.configNodes()
      .filter((c) => !f || `${c.type} ${String(c.name ?? '')}`.toLowerCase().includes(f))
      .map((c) => ({ entry: c, id: c.id, type: c.type, name: String(c.name || c.type) }));
  });
</script>

<aside class="palette">
  <h3>Palette</h3>
  <input type="search" placeholder="Filter…" aria-label="Filter the palette" bind:value={filter} />
  <div class="palette-list">
    {#each groups as [cat, list] (cat)}
      <div class="palette-cat">{cat}</div>
      {#each list as d (d.type)}
        <div
          class="palette-item"
          class:palette-config-type={d.isConfig}
          role="button"
          tabindex="-1"
          draggable={d.isConfig ? undefined : 'true'}
          title={d.isConfig ? `${d.help ?? d.type} Double-click to add one.` : (d.help ?? d.type)}
          data-palette-type={d.type}
          ondragstart={(e) => e.dataTransfer?.setData('text/hotloop-flow-node', d.type)}
          ondblclick={() => {
            if (!editing) return;
            if (d.isConfig) onnewconfig(d);
            else onplace(d);
          }}
        >
          <span class="swatch" style:background={d.color}></span>
          <span>{d.paletteLabel ?? d.type}</span>
        </div>
      {/each}
    {/each}
    {#if inUse.length > 0}
      <div class="palette-cat">configuration in use</div>
      {#each inUse as c (c.id)}
        <div class="palette-item palette-config" role="button" tabindex="-1" title="Double-click to edit" data-test="config-node" data-config-id={c.id} ondblclick={() => oneditconfig(c.entry)}>
          <span class="swatch" style:background={byType.get(c.type)?.color ?? 'var(--hl-ink-soft)'}></span>
          <span class="palette-config-name">{c.name}</span>
          <span class="palette-config-type-name">{c.type}</span>
          <button class="ghost icon" type="button" title="Delete" aria-label="Delete" data-test="config-delete" onclick={(ev) => { ev.stopPropagation(); ondeleteconfig(c.entry); }}><Icon name="x" size={12} /></button>
        </div>
      {/each}
    {/if}
    {#if groups.length === 0 && inUse.length === 0}
      <div class="empty">Nothing matches.</div>
    {/if}
  </div>
</aside>
