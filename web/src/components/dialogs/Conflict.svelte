<script lang="ts">
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

<Dialog title="Your flows are out of date" onclose={() => done(null)}>
  {#if c.latest}
    <p>Somebody deployed since you loaded these flows: {describe(c.latest)}.</p>
    {#if c.latest.note}<p>Their note: "{c.latest.note}"</p>{/if}
  {:else}
    <p>Somebody deployed since you loaded these flows.</p>
  {/if}
  {#if c.theirChanges}
    <p>What they changed:</p>
    <pre class="diff-text" data-test="their-changes">{c.theirChanges}</pre>
  {:else if !c.baseKnown}
    <p>The revision you loaded isn't in the deployment log any more, so what they changed can't be shown.</p>
  {/if}
  <p>Overwriting deploys your version over theirs. Loading theirs throws your changes away.</p>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" data-test="conflict-cancel" onclick={() => done(null)}>Keep editing</button>
    <button class="ghost" data-test="conflict-theirs" onclick={() => done('theirs')}>Load theirs</button>
    <button class="primary" data-test="conflict-mine" onclick={() => done('mine')}>Overwrite with mine</button>
  {/snippet}
</Dialog>
