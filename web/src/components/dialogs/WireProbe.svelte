<script lang="ts">
  import { fmtDate, tr } from '../../lib/i18n.svelte';
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

<Dialog title={tr('dialogs.wireprobe.wire')} sub={title} backdropClose onclose={() => done(null)}>
  <div class="field field-inline"><label><input type="checkbox" data-test="probe-on" bind:checked={probe} /> {tr('dialogs.wireprobe.probe_keep_the_last_100')}</label></div>
  <div class="field field-inline"><label><input type="checkbox" data-test="break-on" bind:checked={brk} /> {tr('dialogs.wireprobe.break_pause_when_a_value')}</label></div>
  <div class="field-help">{tr('dialogs.wireprobe.values_this_wire_carried_newest')}</div>
  <ol class="probe-history" data-test="probe-history">
    {#each history.slice(-20) as h}
      <li class="mono"><span>{fmtDate(h.at, { timeStyle: 'medium', hour12: false })}</span> {show(h.value)}</li>
    {:else}
      <li class="mono">{tr('dialogs.wireprobe.nothing_yet_run_the_diagram')}</li>
    {/each}
  </ol>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" onclick={() => done(null)}>{tr('dialogs.wireprobe.cancel')}</button>
    <button class="primary" type="button" data-test="probe-done" onclick={() => done({ probe, brk })}>{tr('dialogs.wireprobe.done')}</button>
  {/snippet}
</Dialog>
