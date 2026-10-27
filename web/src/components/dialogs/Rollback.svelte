<script lang="ts">
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

<Dialog title="Roll back to deployment {d.seq}?" onclose={() => done(null)}>
  <p>This deploys {describe(d)} again, flows and credentials, as a new deployment. Nothing in the history is changed or removed.</p>
  {#if d.note}<p>Its note: "{d.note}"</p>{/if}
  {#if unsaved}
    <p class="banner warn">You have changes in the editor that were never deployed. A rollback throws them away.</p>
  {/if}
  <label for="rollback-note">Note</label>
  <textarea id="rollback-note" class="note" placeholder="Why? This goes in the deployment log." data-test="rollback-note" bind:this={area} bind:value={note}></textarea>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" data-test="rollback-cancel" onclick={() => done(null)}>Cancel</button>
    <button class="primary" data-test="rollback-confirm" onclick={() => done(note)}>Roll back</button>
  {/snippet}
</Dialog>
