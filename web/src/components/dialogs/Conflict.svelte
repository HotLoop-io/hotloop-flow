<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
  // Somebody deployed while you were editing. Shows what they changed and lets
  // you pick: keep yours and overwrite theirs, or throw yours away and load
  // theirs. Resolves with 'mine', 'theirs', or null to back out and keep editing.
  import { type Conflict, describe } from '../../lib/history';
  import Dialog from './Dialog.svelte';

  interface Props {
    conflict: Conflict;
    done: (answer: 'mine' | 'theirs' | null) => void;
  }
  let { conflict: c, done }: Props = $props();
</script>

<Dialog title={tr('dialogs.conflict.your_flows_are_out_of')} onclose={() => done(null)}>
  {#if c.latest}
    <p>{tr('dialogs.conflict.deployed_since', { what: describe(c.latest) })}</p>
    {#if c.latest.note}<p>{tr('dialogs.conflict.their_note', { note: c.latest.note ?? '' })}</p>{/if}
  {:else}
    <p>{tr('dialogs.conflict.somebody_deployed_since_you_loaded')}</p>
  {/if}
  {#if c.theirChanges}
    <p>{tr('dialogs.conflict.what_they_changed')}</p>
    <pre class="diff-text" data-test="their-changes">{c.theirChanges}</pre>
  {:else if !c.baseKnown}
    <p>{tr('dialogs.conflict.the_revision_you_loaded_isnt')}</p>
  {/if}
  <p>{tr('dialogs.conflict.overwriting_deploys_your_version_over')}</p>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" data-test="conflict-cancel" onclick={() => done(null)}>{tr('dialogs.conflict.keep_editing')}</button>
    <button class="ghost" data-test="conflict-theirs" onclick={() => done('theirs')}>{tr('dialogs.conflict.load_theirs')}</button>
    <button class="primary" data-test="conflict-mine" onclick={() => done('mine')}>{tr('dialogs.conflict.overwrite_with_mine')}</button>
  {/snippet}
</Dialog>
