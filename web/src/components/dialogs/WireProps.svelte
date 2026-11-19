<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
  // A wire's settings: whether it is durable, and when it stops trying a message
  // its target refuses. The file keeps both per pair of nodes, so they hold for
  // every wire between the two.
  import type { FlowEntry, Graph } from '../../lib/graph';
  import Dialog from './Dialog.svelte';

  interface Props { graph: Graph; from: FlowEntry; port: number; to: FlowEntry; done: (saved: boolean) => void }
  let { graph, from, port, to, done }: Props = $props();

  const now = graph.wireSettings(from.id, to.id);
  let durable = $state(now.durable);
  let attempts = $state<string | number | null>(now.attempts === null ? '' : String(now.attempts));
  let error = $state('');
  let box: HTMLInputElement | undefined = $state();
  $effect(() => box?.focus());

  function save(): void {
    let tries: number | null = null;
    // A number input answers a number, or null when it is empty.
    const typed = attempts == null ? '' : String(attempts).trim();
    if (durable && typed !== '') {
      tries = Number(typed);
      if (!Number.isInteger(tries) || tries < 1) { error = tr('dialogs.wireprops.tries_error'); return; }
    }
    graph.setWireSettings(from.id, to.id, { durable, attempts: tries });
    done(true);
  }
</script>

<Dialog title={tr('dialogs.wireprops.wire')} backdropClose onclose={() => done(false)}>
  <div class="field-help" data-test="wire-ends">{tr('dialogs.wireprops.ends', { from: graph.label(from), n: port + 1, to: graph.label(to) })}</div>
  <div class="field field-inline"><label><input type="checkbox" data-test="wire-durable" bind:this={box} bind:checked={durable} /> {tr('dialogs.wireprops.durable')}</label></div>
  <div class="field-help">
    {tr('dialogs.wireprops.while_the_target_keeps_up')}
  </div>
  <div class="field" style:opacity={durable ? '1' : '0.5'}>
    <label>{tr('dialogs.wireprops.give_up_on_a_message')}</label>
    <input type="number" min="1" step="1" placeholder={tr('dialogs.wireprops.never')} data-test="wire-attempts" disabled={!durable} bind:value={attempts} />
    <div class="field-help">
      {tr('dialogs.wireprops.blank_keeps_trying_one_message')}
    </div>
  </div>
  <div class="dialog-error" data-test="wire-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="wire-cancel" onclick={() => done(false)}>{tr('dialogs.wireprops.cancel')}</button>
    <button class="primary" type="button" data-test="wire-done" onclick={save}>{tr('dialogs.wireprops.done')}</button>
  {/snippet}
</Dialog>
