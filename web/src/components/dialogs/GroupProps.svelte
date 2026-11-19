<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
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
    for (const [what, v] of [[tr('dialogs.groupprops.fill'), fill], [tr('dialogs.groupprops.border'), stroke]] as const) {
      if (v !== '' && !hex.test(v)) { error = tr('dialogs.groupprops.hex_error', { what }); return; }
    }
    graph.updateGroup(group.id, { name, fill, stroke, label });
    done(true);
  }
</script>

<Dialog title={tr('dialogs.groupprops.group')} backdropClose onclose={() => done(false)}>
  <div class="field"><label>{tr('dialogs.groupprops.name')}</label><input type="text" data-test="group-name" bind:this={nameInput} bind:value={name} /></div>
  <div class="field field-inline"><label><input type="checkbox" data-test="group-label" bind:checked={label} /> {tr('dialogs.groupprops.show_the_name_on_the')}</label></div>
  <div class="field"><label>{tr('dialogs.groupprops.fill')}</label><input type="text" placeholder={tr('dialogs.groupprops.default')} data-test="group-fill" bind:value={fill} /><div class="field-help">{tr('dialogs.groupprops.a_hex_colour_blank_for')}</div></div>
  <div class="field"><label>{tr('dialogs.groupprops.border')}</label><input type="text" placeholder={tr('dialogs.groupprops.default')} data-test="group-stroke" bind:value={stroke} /><div class="field-help">{tr('dialogs.groupprops.a_hex_colour_blank_for')}</div></div>
  <div class="dialog-error" data-test="group-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="group-cancel" onclick={() => done(false)}>{tr('dialogs.groupprops.cancel')}</button>
    <button class="primary" type="button" data-test="group-done" onclick={save}>{tr('dialogs.groupprops.done')}</button>
  {/snippet}
</Dialog>
