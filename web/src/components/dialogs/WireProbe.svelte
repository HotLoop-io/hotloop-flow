<script lang="ts">
  // A wire of a diagram: the last values it carried, with when, and the two things
  // to do to it while debugging: probe it, which keeps more of what goes by, and
  // break on it, which pauses the run when a value is put on it. A probe never
  // changes a run.
  import Dialog from './Dialog.svelte';

  interface Props {
    title: string;
    history: { value: unknown; at: string }[];
    probed: boolean;
    broken: boolean;
    done: (r: { probe: boolean; brk: boolean } | null) => void;
  }
  let { title, history, probed, broken, done }: Props = $props();
  // svelte-ignore state_referenced_locally
  let probe = $state(probed);
  // svelte-ignore state_referenced_locally
  let brk = $state(broken);
  const show = (v: unknown) => (typeof v === 'string' ? JSON.stringify(v) : JSON.stringify(v));
</script>

<Dialog title="Wire" sub={title} backdropClose onclose={() => done(null)}>
  <div class="field field-inline"><label><input type="checkbox" data-test="probe-on" bind:checked={probe} /> Probe: keep the last 100 values</label></div>
  <div class="field field-inline"><label><input type="checkbox" data-test="break-on" bind:checked={brk} /> Break: pause when a value is put on this wire</label></div>
  <div class="field-help">Values this wire carried, newest last.</div>
  <ol class="probe-history" data-test="probe-history">
    {#each history.slice(-20) as h}
      <li class="mono"><span>{new Date(h.at).toLocaleTimeString([], { hour12: false })}</span> {show(h.value)}</li>
    {:else}
      <li class="mono">Nothing yet. Run the diagram.</li>
    {/each}
  </ol>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" onclick={() => done(null)}>Cancel</button>
    <button class="primary" type="button" data-test="probe-done" onclick={() => done({ probe, brk })}>Done</button>
  {/snippet}
</Dialog>
