<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
  // Takes pasted or chosen flow JSON. Resolves with what was added, or null if it
  // was cancelled. A problem is shown in the dialog, which stays open.
  import { type ImportResult, importEntries, parseEntries } from '../../lib/flowio';
  import type { Graph, Point } from '../../lib/graph';
  import Dialog from './Dialog.svelte';

  interface Props { graph: Graph; at: Point; done: (r: ImportResult | null) => void }
  let { graph, at, done }: Props = $props();

  const inSubflow = graph.isSubflowScope(graph.activeTab);
  let text = $state('');
  let target = $state('here');
  let error = $state('');
  let area: HTMLTextAreaElement | undefined = $state();
  $effect(() => area?.focus());

  async function chosen(e: Event): Promise<void> {
    const f = (e.currentTarget as HTMLInputElement).files?.[0];
    if (f) text = await f.text();
  }

  function go(): void {
    const parsed = parseEntries(text);
    if (!parsed.entries) {
      error = parsed.error ?? 'That could not be read.';
      return;
    }
    done(importEntries(graph, parsed.entries, {
      scope: graph.activeTab,
      at,
      ...(target === 'new' ? { intoNewFlow: 'Imported flow' } : {}),
    }));
  }
</script>

<Dialog title={tr('dialogs.import.import')} backdropClose onclose={() => done(null)}>
  <div class="field">
    <label>{tr('dialogs.import.flow_json')}</label>
    <textarea class="code lang-json" spellcheck="false" rows="12" data-test="import-text" placeholder={tr('dialogs.import.paste_flow_json_here_or')} bind:this={area} bind:value={text}></textarea>
  </div>
  <div class="field">
    <label>{tr('dialogs.import.or_a_file')}</label>
    <input type="file" accept=".json,application/json" data-test="import-file" onchange={chosen} />
  </div>
  <div class="field">
    <label>{tr('dialogs.import.put_it_on')}</label>
    <select data-test="import-target" bind:value={target}>
      <option value="here">{inSubflow ? tr('dialogs.import.this_subflow') : tr('dialogs.import.this_flow')}</option>
      <option value="new">{tr('dialogs.import.a_new_flow')}</option>
    </select>
    <div class="field-help">{tr('dialogs.import.a_file_that_brings_its')}</div>
  </div>
  <div class="dialog-error" data-test="import-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="primary" type="button" data-test="import-go" onclick={go}>{tr('dialogs.import.import')}</button>
    <button class="ghost" type="button" data-test="import-cancel" onclick={() => done(null)}>{tr('dialogs.import.cancel')}</button>
  {/snippet}
</Dialog>
