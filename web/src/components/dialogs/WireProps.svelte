<script lang="ts">
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
      if (!Number.isInteger(tries) || tries < 1) { error = 'Tries has to be a whole number of 1 or more, or blank.'; return; }
    }
    graph.setWireSettings(from.id, to.id, { durable, attempts: tries });
    done(true);
  }
</script>

<Dialog title="Wire" backdropClose onclose={() => done(false)}>
  <div class="field-help" data-test="wire-ends">{graph.label(from)} (output {port + 1}) to {graph.label(to)}</div>
  <div class="field field-inline"><label><input type="checkbox" data-test="wire-durable" bind:this={box} bind:checked={durable} /> Durable</label></div>
  <div class="field-help">
    While the target keeps up, nothing is written. When it stops taking messages, this wire holds them on disk in order, and delivers them when it comes back, even across a restart.
  </div>
  <div class="field" style:opacity={durable ? '1' : '0.5'}>
    <label>Give up on a message after this many tries</label>
    <input type="number" min="1" step="1" placeholder="never" data-test="wire-attempts" disabled={!durable} bind:value={attempts} />
    <div class="field-help">
      Blank keeps trying one message for as long as the target refuses it, and nothing behind it is delivered. With a number, a message refused that many times goes to a Catch node as an error with the message attached, and the wire moves on.
    </div>
  </div>
  <div class="dialog-error" data-test="wire-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="wire-cancel" onclick={() => done(false)}>Cancel</button>
    <button class="primary" type="button" data-test="wire-done" onclick={save}>Done</button>
  {/snippet}
</Dialog>
