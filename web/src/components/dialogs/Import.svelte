<script lang="ts">
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

<Dialog title="Import" backdropClose onclose={() => done(null)}>
  <div class="field">
    <label>Flow JSON</label>
    <textarea class="code lang-json" spellcheck="false" rows="12" data-test="import-text" placeholder="Paste flow JSON here, or choose a file." bind:this={area} bind:value={text}></textarea>
  </div>
  <div class="field">
    <label>Or a file</label>
    <input type="file" accept=".json,application/json" data-test="import-file" onchange={chosen} />
  </div>
  <div class="field">
    <label>Put it on</label>
    <select data-test="import-target" bind:value={target}>
      <option value="here">{inSubflow ? 'This subflow' : 'This flow'}</option>
      <option value="new">A new flow</option>
    </select>
    <div class="field-help">A file that brings its own flows keeps them, whatever you pick here.</div>
  </div>
  <div class="dialog-error" data-test="import-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="primary" type="button" data-test="import-go" onclick={go}>Import</button>
    <button class="ghost" type="button" data-test="import-cancel" onclick={() => done(null)}>Cancel</button>
  {/snippet}
</Dialog>
