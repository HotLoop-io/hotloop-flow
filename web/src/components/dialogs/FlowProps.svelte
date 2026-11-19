<script lang="ts">
  import { tr, trn } from '../../lib/i18n.svelte';
  // A flow's name, and taking the flow away.
  import type { FlowEntry, Graph } from '../../lib/graph';
  import Dialog from './Dialog.svelte';

  interface Props { graph: Graph; tab: FlowEntry; done: (o: 'saved' | 'deleted' | null) => void }
  let { graph, tab, done }: Props = $props();

  let name = $state(String(tab.label ?? ''));
  let error = $state('');
  let confirming = $state(false);
  let confirmText = $state('');
  let nameInput: HTMLInputElement | undefined = $state();
  $effect(() => {
    nameInput?.focus();
    nameInput?.select();
  });

  function save(): void {
    if (name.trim() === '') { error = 'A flow needs a name.'; return; }
    graph.renameTab(tab.id, name.trim());
    done('saved');
  }
  function askDelete(): void {
    const n = graph.countOn(tab.id);
    if (graph.tabs().length <= 1) { error = 'This is the only flow. Add another before deleting it.'; return; }
    error = '';
    confirming = true;
    confirmText = n === 0
      ? tr('dialogs.flowprops.delete_empty')
      : trn('dialogs.flowprops.delete_with_nodes', n);
  }
  function reallyDelete(): void {
    const problem = graph.deleteTab(tab.id);
    if (problem) { error = problem; return; }
    done('deleted');
  }
</script>

<Dialog title={tr('dialogs.flowprops.flow')} backdropClose onclose={() => done(null)}>
  <div class="field">
    <label>{tr('dialogs.flowprops.name')}</label>
    <input type="text" data-test="flow-name" bind:this={nameInput} bind:value={name} onkeydown={(e) => { if (e.key === 'Enter') save(); }} />
  </div>
  <div class="dialog-error" data-test="flow-error">{error}</div>
  {#if confirming}
    <div class="field-help">
      <span data-test="flow-confirm-text">{confirmText}</span>
      <button class="ghost" type="button" data-test="flow-delete-confirm" onclick={reallyDelete}>{tr('dialogs.flowprops.yes_delete_it')}</button>
      <button class="ghost" type="button" data-test="flow-delete-cancel" onclick={() => (confirming = false)}>{tr('dialogs.flowprops.keep_it')}</button>
    </div>
  {/if}
  {#snippet foot()}
    <div class="spacer"></div>
    {#if !confirming}<button class="ghost" type="button" data-test="flow-delete" onclick={askDelete}>{tr('dialogs.flowprops.delete_flow')}</button>{/if}
    <button class="ghost" type="button" data-test="flow-cancel" onclick={() => done(null)}>{tr('dialogs.flowprops.cancel')}</button>
    <button class="primary" type="button" data-test="flow-done" onclick={save}>{tr('dialogs.flowprops.done')}</button>
  {/snippet}
</Dialog>
