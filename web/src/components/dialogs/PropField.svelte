<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
  // One property of a node, drawn from what its descriptor says it is. Lists
  // draw rows of these, so this draws itself.
  import type { PropDef } from '../../lib/api';
  import type { Graph } from '../../lib/graph';
  import { type Model, typeLabel, initRow, typeKeyOf, typesOf } from '../../lib/propform';
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

  // The diagram tabs a diagram property picks from. A value written by hand as a
  // tab's name is shown as that tab, and saved as its id, which the runtime
  // reads the same way.
  const diagramTabs = $derived(graph ? graph.tabs().filter((t) => graph.isDiagramTab(t.id)) : []);
  const tabLabel = (t: { id: string; label?: unknown }): string => String(t.label ?? t.id);
  function asTabId(v: string): string {
    if (!v || diagramTabs.some((t) => t.id === v)) return v;
    const named = diagramTabs.filter((t) => tabLabel(t) === v);
    return named.length === 1 ? named[0]!.id : v;
  }
  if (p.kind === 'diagram') model[p.name] = asTabId(String(model[p.name] ?? ''));

  // A case structure's rows: the stored value is a JSON object of case name to
  // diagram, and this is that object drawn as one row for each case.
  function parseCases(v: unknown): { name: string; body: string }[] {
    try {
      const o = JSON.parse(String(v ?? '{}') || '{}') as Record<string, unknown>;
      return Object.entries(o).map(([name, body]) => ({ name, body: asTabId(String(body ?? '')) }));
    } catch {
      return [];
    }
  }
  let caseRows = $state<{ name: string; body: string }[]>(p.kind === 'cases' ? parseCases(model[p.name]) : []);
  // The stored value follows the rows: whatever they say now is what is saved.
  $effect(() => {
    if (p.kind !== 'cases') return;
    const o: Record<string, string> = {};
    for (const r of caseRows) if (r.name.trim()) o[r.name.trim()] = r.body;
    model[p.name] = JSON.stringify(o);
  });
  function addCase(): void {
    caseRows.push({ name: '', body: diagramTabs[0]?.id ?? '' });
  }
  function removeCase(i: number): void {
    caseRows.splice(i, 1);
  }

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
  <label>{label}{#if p.required}<span class="required">{' *'}</span>{/if}</label>
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
        {#each typesOf(p) as t (t)}<option value={t}>{typeLabel(t)}</option>{/each}
      </select>
      <input type="text" placeholder={p.placeholder ?? ''} disabled={model[typeKeyOf(p)] === 'date'} bind:value={model[p.name]} />
    </div>
    {@render help()}
  </div>
{:else if p.kind === 'configRef'}
  <div class="field">
    {@render head()}
    <select bind:value={model[p.name]}>
      <option value="">{tr('dialogs.propfield.none')}</option>
      {#each candidates as c (c.id)}<option value={c.id}>{(c.name as string) || c.id}</option>{/each}
    </select>
    {#if candidates.length === 0}
      <div class="field-help">{tr('dialogs.propfield.no_config_yet', { type: p.configType ?? '' })}</div>
    {/if}
    {@render help()}
  </div>
{:else if p.kind === 'diagram'}
  <div class="field">
    {@render head()}
    <select data-test="diagram-pick" bind:value={model[p.name]}>
      <option value="">{p.required ? tr('dialogs.propfield.choose_a_diagram') : tr('dialogs.propfield.none')}</option>
      {#each diagramTabs as t (t.id)}<option value={t.id}>{tabLabel(t)}</option>{/each}
      {#if model[p.name] && !diagramTabs.some((t) => t.id === model[p.name])}
        <option value={model[p.name]}>{tr('dialogs.propfield.no_such_diagram', { name: String(model[p.name]) })}</option>
      {/if}
    </select>
    {#if diagramTabs.length === 0}
      <div class="field-help">{tr('dialogs.propfield.this_flow_file_has_no')}</div>
    {/if}
    {@render help()}
  </div>
{:else if p.kind === 'cases'}
  <div class="field" data-test="cases">
    {@render head()}
    <div class="list-rows">
      {#each caseRows as row, i (i)}
        <div class="list-row" data-test="case-row">
          <div class="list-row-fields">
            <input type="text" data-test="case-name" placeholder={tr('dialogs.propfield.case_name_true_2_run')} bind:value={row.name} />
            <select data-test="case-body" bind:value={row.body}>
              <option value="">{tr('dialogs.propfield.choose_a_diagram')}</option>
              {#each diagramTabs as t (t.id)}<option value={t.id}>{tabLabel(t)}</option>{/each}
              {#if row.body && !diagramTabs.some((t) => t.id === row.body)}
                <option value={row.body}>{tr('dialogs.propfield.no_such_diagram', { name: row.body })}</option>
              {/if}
            </select>
          </div>
          <button class="icon" type="button" title={tr('dialogs.propfield.remove')} onclick={() => removeCase(i)}>✕</button>
        </div>
      {/each}
    </div>
    <button class="ghost" type="button" data-test="case-add" onclick={addCase}>{tr('dialogs.propfield.add_a_case')}</button>
    {@render help()}
  </div>
{:else if p.kind === 'credential'}
  <div class="field">
    {@render head()}
    <input type="password" autocomplete="new-password" placeholder={hasExisting ? tr('dialogs.propfield.unchanged') : ''} bind:value={model[p.name]} />
    <div class="field-help">
      {hasExisting ? tr('dialogs.propfield.value_stored') : tr('dialogs.propfield.stored_encrypted')}
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
          <button class="icon" type="button" title={tr('dialogs.propfield.remove')} onclick={() => removeRow(i)}>✕</button>
        </div>
      {/each}
    </div>
    <button class="ghost" type="button" onclick={addRow}>{tr('dialogs.propfield.add_label', { label })}</button>
    {@render help()}
  </div>
{:else}
  <div class="field">
    {@render head()}
    <input type="text" placeholder={p.placeholder ?? ''} bind:value={model[p.name]} />
    {@render help()}
  </div>
{/if}
