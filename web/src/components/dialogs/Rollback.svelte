<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
  // Asks before a rollback, and asks why. Resolves with the note, or null when
  // the operator backs out.
  import type { Deployment } from '../../lib/api';
  import { describe } from '../../lib/history';
  import Dialog from './Dialog.svelte';

  interface Props {
    deployment: Deployment;
    unsaved: boolean;
    done: (note: string | null) => void;
  }
  let { deployment: d, unsaved, done }: Props = $props();

  let note = $state('');
  let area: HTMLTextAreaElement | undefined = $state();
  $effect(() => area?.focus());
</script>

<Dialog title={tr('dialogs.rollback.title', { n: d.seq })} onclose={() => done(null)}>
  <p>{tr('dialogs.rollback.explain', { what: describe(d) })}</p>
  {#if d.note}<p>{tr('dialogs.rollback.its_note', { note: d.note ?? '' })}</p>{/if}
  {#if unsaved}
    <p class="banner warn">{tr('dialogs.rollback.you_have_changes_in_the')}</p>
  {/if}
  <label for="rollback-note">{tr('dialogs.rollback.note')}</label>
  <textarea id="rollback-note" class="note" placeholder={tr('dialogs.rollback.why_this_goes_in_the')} data-test="rollback-note" bind:this={area} bind:value={note}></textarea>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" data-test="rollback-cancel" onclick={() => done(null)}>{tr('dialogs.rollback.cancel')}</button>
    <button class="primary" data-test="rollback-confirm" onclick={() => done(note)}>{tr('dialogs.rollback.roll_back')}</button>
  {/snippet}
</Dialog>
