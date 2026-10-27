<script lang="ts">
  // Deploy doesn't deploy. It shows what deploying would change, drawn on the
  // canvas and listed in words, from the engine's own diff against what is live,
  // and asks for a note. The second button deploys. Nobody should push to a line
  // without seeing what they're pushing, and the note is what the next person
  // reads in the history when they're trying to work out why.
  import type { DiffResult } from '../../lib/api';
  import { summarise } from '../../lib/diff';

  interface Props {
    diff: DiffResult;
    deploying: boolean;
    ondeploy: (note: string) => void;
    oncancel: () => void;
  }
  let { diff, deploying, ondeploy, oncancel }: Props = $props();

  let note = $state('');
  let box: HTMLTextAreaElement | undefined = $state();
  $effect(() => box?.focus());

  // The words the engine wrote, with what a line is about marked by its first
  // sign, so an addition, a removal and a change read at a glance.
  const lines = $derived(diff.text.split('\n').map((l) => {
    const t = l.trim();
    const kind = /^\+|: added\b|^wire added/.test(t) ? 'add' : /^-|: removed\b|^wire removed|^removed/.test(t) ? 'del' : t.startsWith('~') ? 'chg' : '';
    return { l, kind };
  }));
</script>

<section class="section review" data-test="review">
  <div class="section-head"><h3>Review and deploy</h3></div>
  <div class="section-body">
    <div class="mono review-summary" data-test="review-summary">{summarise(diff)}</div>
    <pre class="diff-text" data-test="review-text">{#each lines as ln, i (i)}<span class={ln.kind}>{ln.l}</span>{i < lines.length - 1 ? '\n' : ''}{/each}</pre>
    <textarea placeholder="What changed and why. This goes in the deployment log." data-test="deploy-note" bind:this={box} bind:value={note}></textarea>
    <div class="actions">
      <button class="primary" data-test="deploy-confirm" disabled={deploying} onclick={() => ondeploy(note)}>{deploying ? 'Deploying…' : 'Deploy'}</button>
      <button class="ghost" data-test="deploy-cancel" onclick={oncancel}>Keep editing</button>
    </div>
  </div>
</section>
