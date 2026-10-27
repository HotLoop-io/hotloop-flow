<script lang="ts">
  // Shows a scope's flows as JSON to copy or save. The rules for what goes out
  // are in lib/flowio.ts; this is the form around them.
  import { copyText } from '../../lib/clipboard';
  import { type ExportScope, exportEntries, toText } from '../../lib/flowio';
  import type { Graph } from '../../lib/graph';
  import Dialog from './Dialog.svelte';

  interface Props { graph: Graph; done: (answer: null) => void }
  let { graph, done }: Props = $props();

  const selected = [...graph.selection].filter((id) => graph.byId(id) && !id.startsWith('@')).length;
  let scope: ExportScope = $state(selected > 0 ? 'selection' : 'flow');
  let status = $state('');
  let area: HTMLTextAreaElement | undefined = $state();
  let copyButton: HTMLButtonElement | undefined = $state();
  $effect(() => copyButton?.focus());

  const entries = $derived(exportEntries(graph, scope));
  const text = $derived(toText(entries));

  const choices: [ExportScope, string, boolean][] = [
    ['selection', `Selected nodes (${selected})`, selected === 0],
    ['flow', 'This flow', false],
    ['all', 'Every flow', false],
  ];

  async function copy(): Promise<void> {
    const ok = await copyText(text, area);
    status = ok ? 'Copied.' : 'The browser would not copy. Select the text and copy it by hand.';
  }
  function download(): void {
    const blob = new Blob([text], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `hotloop-flow-${scope}.json`;
    document.body.append(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(a.href);
    status = 'Saved.';
  }
</script>

<Dialog title="Export" backdropClose onclose={() => done(null)}>
  <div class="field">
    <label>What to export</label>
    {#each choices as [value, label, disabled] (value)}
      <div class="field-inline">
        <label><input type="radio" name="export-scope" {value} data-test="export-{value}" {disabled} checked={scope === value} onchange={() => (scope = value)} /> {label}</label>
      </div>
    {/each}
  </div>
  <div class="field">
    <textarea class="code lang-json" readonly spellcheck="false" rows="14" data-test="export-text" bind:this={area} value={text}></textarea>
    <div class="field-help" data-test="export-info">{entries.length} entr{entries.length === 1 ? 'y' : 'ies'}. Passwords and other credentials are never exported.</div>
    <div class="field-help" data-test="export-status">{status}</div>
  </div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="primary" type="button" data-test="export-copy" bind:this={copyButton} onclick={copy}>Copy</button>
    <button type="button" data-test="export-download" onclick={download}>Download</button>
    <button class="ghost" type="button" data-test="export-close" onclick={() => done(null)}>Close</button>
  {/snippet}
</Dialog>
