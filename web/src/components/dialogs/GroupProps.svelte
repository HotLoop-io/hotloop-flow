<script lang="ts">
  // A group's name and how it is drawn.
  import type { FlowEntry, Graph } from '../../lib/graph';
  import Dialog from './Dialog.svelte';

  interface Props { graph: Graph; group: FlowEntry; done: (saved: boolean) => void }
  let { graph, group, done }: Props = $props();

  const style = (group.style as Record<string, unknown> | undefined) ?? {};
  let name = $state(String(group.name ?? ''));
  let label = $state(style.label !== false);
  let fill = $state(String(style.fill ?? ''));
  let stroke = $state(String(style.stroke ?? ''));
  let error = $state('');
  let nameInput: HTMLInputElement | undefined = $state();
  $effect(() => nameInput?.focus());

  const hex = /^#[0-9a-fA-F]{3,8}$/;
  function save(): void {
    for (const [what, v] of [['Fill', fill], ['Border', stroke]] as const) {
      if (v !== '' && !hex.test(v)) { error = `${what} has to be a hex colour like #3366cc.`; return; }
    }
    graph.updateGroup(group.id, { name, fill, stroke, label });
    done(true);
  }
</script>

<Dialog title="Group" backdropClose onclose={() => done(false)}>
  <div class="field"><label>Name</label><input type="text" data-test="group-name" bind:this={nameInput} bind:value={name} /></div>
  <div class="field field-inline"><label><input type="checkbox" data-test="group-label" bind:checked={label} /> Show the name on the canvas</label></div>
  <div class="field"><label>Fill</label><input type="text" placeholder="default" data-test="group-fill" bind:value={fill} /><div class="field-help">A hex colour. Blank for the default.</div></div>
  <div class="field"><label>Border</label><input type="text" placeholder="default" data-test="group-stroke" bind:value={stroke} /><div class="field-help">A hex colour. Blank for the default.</div></div>
  <div class="dialog-error" data-test="group-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="group-cancel" onclick={() => done(false)}>Cancel</button>
    <button class="primary" type="button" data-test="group-done" onclick={save}>Done</button>
  {/snippet}
</Dialog>
