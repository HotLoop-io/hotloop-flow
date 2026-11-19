<script lang="ts">
  import { tr } from '../../lib/i18n.svelte';
  // A template's own properties: name, colour, how many ports, and the
  // parameters an instance can set.
  import type { FlowEntry, Graph } from '../../lib/graph';
  import { ENV_TYPES, envOf } from '../../lib/subflowenv';
  import Dialog from './Dialog.svelte';

  interface Props {
    graph: Graph;
    template: FlowEntry;
    done: (o: 'saved' | 'deleted' | null) => void;
  }
  let { graph, template, done }: Props = $props();

  let name = $state(String(template.name ?? ''));
  let category = $state(String(template.category ?? ''));
  let info = $state(String(template.info ?? ''));
  let color = $state(String(template.color ?? ''));
  let inputs = $state(String(Array.isArray(template.in) ? Math.min(1, (template.in as unknown[]).length) : 0));
  let outputs = $state<string | number | null>(String(Array.isArray(template.out) ? (template.out as unknown[]).length : 0));
  const rows = $state(envOf(template).map((v) => ({ ...v })));
  let error = $state('');
  let nameInput: HTMLInputElement | undefined = $state();
  $effect(() => nameInput?.focus());

  function save(): void {
    const outs = outputs == null || String(outputs).trim() === '' ? NaN : Number(outputs);
    if (!Number.isInteger(outs) || outs < 0 || outs > 20) { error = 'Outputs has to be a whole number from 0 to 20.'; return; }
    if (color !== '' && !/^#[0-9a-fA-F]{3,8}$/.test(color)) { error = 'Colour has to be a hex colour like #DDAA99.'; return; }
    const names = rows.map((r) => r.name.trim()).filter((n) => n !== '');
    if (new Set(names).size !== names.length) { error = 'Two properties have the same name.'; return; }
    graph.updateSubflow(template.id, {
      name, info, category,
      color: color || '#DDAA99',
      inputs: Number(inputs),
      outputs: outs,
      env: rows.map((r) => ({ name: r.name.trim(), type: r.type, value: r.value })),
    });
    done('saved');
  }

  function remove(): void {
    const problem = graph.deleteSubflow(template.id);
    if (problem) { error = problem; return; }
    done('deleted');
  }
</script>

<Dialog title={tr('dialogs.subflowprops.subflow_properties')} backdropClose onclose={() => done(null)}>
  <div class="field"><label>{tr('dialogs.subflowprops.name_2')}</label><input type="text" data-test="subflow-name" bind:this={nameInput} bind:value={name} /></div>
  <div class="field"><label>{tr('dialogs.subflowprops.category')}</label><input type="text" data-test="subflow-category" bind:value={category} /><div class="field-help">{tr('dialogs.subflowprops.groups_it_in_the_palette')}</div></div>
  <div class="field"><label>{tr('dialogs.subflowprops.colour')}</label><input type="text" data-test="subflow-color" placeholder={tr('dialogs.subflowprops.ddaa99')} bind:value={color} /><div class="field-help">{tr('dialogs.subflowprops.a_hex_colour_for_the')}</div></div>
  <div class="field">
    <label>{tr('dialogs.subflowprops.inputs')}</label>
    <select data-test="subflow-inputs" bind:value={inputs}><option value="0">{tr('dialogs.subflowprops.none')}</option><option value="1">{tr('dialogs.subflowprops.one')}</option></select>
  </div>
  <div class="field"><label>{tr('dialogs.subflowprops.outputs')}</label><input type="number" min="0" max="20" data-test="subflow-outputs" bind:value={outputs} /><div class="field-help">{tr('dialogs.subflowprops.instances_on_flows_follow_when')}</div></div>
  <div class="field"><label>{tr('dialogs.subflowprops.description')}</label><textarea rows="3" data-test="subflow-info" bind:value={info}></textarea></div>
  <div class="field">
    <label>{tr('dialogs.subflowprops.properties')}</label>
    <div>
      <div class="list-rows" data-test="subflow-env">
        {#each rows as r, i (r)}
          <div class="list-row">
            <div class="list-row-fields">
              <input type="text" placeholder={tr('dialogs.subflowprops.name')} data-test="subflow-env-name" bind:value={r.name} />
              <div class="typed-input">
                <select bind:value={r.type}>{#each ENV_TYPES as [v, label] (v)}<option value={v}>{label}</option>{/each}</select>
                <input type="text" placeholder={tr('dialogs.subflowprops.default')} data-test="subflow-env-value" bind:value={r.value} />
              </div>
            </div>
            <button class="icon" type="button" title={tr('dialogs.subflowprops.remove')} onclick={() => rows.splice(i, 1)}>✕</button>
          </div>
        {/each}
      </div>
      <button class="ghost" type="button" data-test="subflow-env-add" onclick={() => rows.push({ name: '', type: 'str', value: '' })}>{tr('dialogs.subflowprops.add_property')}</button>
    </div>
    <div class="field-help">{tr('dialogs.subflowprops.each_instance_can_set_a')}</div>
  </div>
  <div class="dialog-error" data-test="subflow-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="subflow-delete" onclick={remove}>{tr('dialogs.subflowprops.delete_subflow')}</button>
    <button class="ghost" type="button" data-test="subflow-cancel" onclick={() => done(null)}>{tr('dialogs.subflowprops.cancel')}</button>
    <button class="primary" type="button" data-test="subflow-done" onclick={save}>{tr('dialogs.subflowprops.done')}</button>
  {/snippet}
</Dialog>
