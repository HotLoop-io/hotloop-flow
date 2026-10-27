<script lang="ts">
  // One property of a node, drawn from what its descriptor says it is. Lists
  // draw rows of these, so this draws itself.
  import type { PropDef } from '../../lib/api';
  import type { Graph } from '../../lib/graph';
  import { type Model, TYPE_LABELS, initRow, typeKeyOf, typesOf } from '../../lib/propform';
  import PropField from './PropField.svelte';

  interface Props {
    p: PropDef;
    model: Model;
    entry: Model;
    graph?: Graph | undefined;
  }
  let { p, model, entry, graph }: Props = $props();

  const label = $derived(p.label ?? p.name);
  const creds = $derived((entry.credentials as Record<string, unknown> | undefined) ?? {});
  const hasExisting = $derived(creds[p.name] !== undefined);
  const candidates = $derived(graph ? graph.all().filter((e) => e.type === p.configType) : []);
  const rows = $derived((model[p.name] as Model[] | undefined) ?? []);

  // A code box takes Tab as four spaces rather than leaving the field.
  function tab(e: KeyboardEvent): void {
    if (e.key !== 'Tab') return;
    e.preventDefault();
    const ta = e.currentTarget as HTMLTextAreaElement;
    const start = ta.selectionStart;
    const end = ta.selectionEnd;
    ta.value = ta.value.slice(0, start) + '    ' + ta.value.slice(end);
    ta.selectionStart = ta.selectionEnd = start + 4;
    model[p.name] = ta.value;
  }

  function addRow(): void {
    (model[p.name] as Model[]).push(initRow(p, {}));
  }
  function removeRow(i: number): void {
    (model[p.name] as Model[]).splice(i, 1);
  }
</script>

{#snippet head()}
  <label>{label}{#if p.required}<span class="required"> *</span>{/if}</label>
{/snippet}
{#snippet help()}
  {#if p.help}<div class="field-help">{p.help}</div>{/if}
{/snippet}

{#if p.kind === 'bool'}
  <div class="field field-inline">
    <label><input type="checkbox" bind:checked={model[p.name]} /> {label}</label>
    {@render help()}
  </div>
{:else if p.kind === 'select'}
  <div class="field">
    {@render head()}
    <select bind:value={model[p.name]}>
      {#each p.options ?? [] as opt (String(opt.value))}
        <option value={String(opt.value)}>{opt.label}</option>
      {/each}
    </select>
    {@render help()}
  </div>
{:else if p.kind === 'number'}
  <div class="field">
    {@render head()}
    <input type="number" bind:value={model[p.name]} />
    {@render help()}
  </div>
{:else if p.kind === 'text' || p.kind === 'js' || p.kind === 'json' || p.kind === 'jsonata'}
  <div class="field">
    {@render head()}
    <textarea class="code lang-{p.kind}" spellcheck="false" rows={p.kind === 'js' ? 12 : 5} bind:value={model[p.name]} onkeydown={tab}></textarea>
    {@render help()}
  </div>
{:else if p.kind === 'typedInput'}
  <div class="field">
    {@render head()}
    <div class="typed-input">
      <select bind:value={model[typeKeyOf(p)]}>
        {#each typesOf(p) as t (t)}<option value={t}>{TYPE_LABELS[t] ?? t}</option>{/each}
      </select>
      <input type="text" placeholder={p.placeholder ?? ''} disabled={model[typeKeyOf(p)] === 'date'} bind:value={model[p.name]} />
    </div>
    {@render help()}
  </div>
{:else if p.kind === 'configRef'}
  <div class="field">
    {@render head()}
    <select bind:value={model[p.name]}>
      <option value="">— none —</option>
      {#each candidates as c (c.id)}<option value={c.id}>{(c.name as string) || c.id}</option>{/each}
    </select>
    {#if candidates.length === 0}
      <div class="field-help">No {p.configType} is configured yet. Add one from the palette first.</div>
    {/if}
    {@render help()}
  </div>
{:else if p.kind === 'credential'}
  <div class="field">
    {@render head()}
    <input type="password" autocomplete="new-password" placeholder={hasExisting ? '•••••••• (unchanged)' : ''} bind:value={model[p.name]} />
    <div class="field-help">
      {hasExisting ? 'A value is stored. Leave blank to keep it, or type to replace it.' : 'Stored encrypted, never written to the flow file.'}
    </div>
  </div>
{:else if p.kind === 'list'}
  <div class="field">
    {@render head()}
    <div class="list-rows">
      {#each rows as row, i (row)}
        <div class="list-row">
          <div class="list-row-fields">
            {#each p.fields ?? [] as sub (sub.name)}
              <PropField p={sub} model={row} entry={row} graph={undefined} />
            {/each}
          </div>
          <button class="icon" type="button" title="Remove" onclick={() => removeRow(i)}>✕</button>
        </div>
      {/each}
    </div>
    <button class="ghost" type="button" onclick={addRow}>Add {label}</button>
    {@render help()}
  </div>
{:else}
  <div class="field">
    {@render head()}
    <input type="text" placeholder={p.placeholder ?? ''} bind:value={model[p.name]} />
    {@render help()}
  </div>
{/if}
