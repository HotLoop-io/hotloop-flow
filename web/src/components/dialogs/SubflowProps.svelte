<script lang="ts">
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

<Dialog title="Subflow properties" backdropClose onclose={() => done(null)}>
  <div class="field"><label>Name</label><input type="text" data-test="subflow-name" bind:this={nameInput} bind:value={name} /></div>
  <div class="field"><label>Category</label><input type="text" data-test="subflow-category" bind:value={category} /><div class="field-help">Groups it in the palette.</div></div>
  <div class="field"><label>Colour</label><input type="text" data-test="subflow-color" placeholder="#DDAA99" bind:value={color} /><div class="field-help">A hex colour for the node. Blank for the default.</div></div>
  <div class="field">
    <label>Inputs</label>
    <select data-test="subflow-inputs" bind:value={inputs}><option value="0">none</option><option value="1">one</option></select>
  </div>
  <div class="field"><label>Outputs</label><input type="number" min="0" max="20" data-test="subflow-outputs" bind:value={outputs} /><div class="field-help">Instances on flows follow when this changes.</div></div>
  <div class="field"><label>Description</label><textarea rows="3" data-test="subflow-info" bind:value={info}></textarea></div>
  <div class="field">
    <label>Properties</label>
    <div>
      <div class="list-rows" data-test="subflow-env">
        {#each rows as r, i (r)}
          <div class="list-row">
            <div class="list-row-fields">
              <input type="text" placeholder="name" data-test="subflow-env-name" bind:value={r.name} />
              <div class="typed-input">
                <select bind:value={r.type}>{#each ENV_TYPES as [v, label] (v)}<option value={v}>{label}</option>{/each}</select>
                <input type="text" placeholder="default" data-test="subflow-env-value" bind:value={r.value} />
              </div>
            </div>
            <button class="icon" type="button" title="Remove" onclick={() => rows.splice(i, 1)}>✕</button>
          </div>
        {/each}
      </div>
      <button class="ghost" type="button" data-test="subflow-env-add" onclick={() => rows.push({ name: '', type: 'str', value: '' })}>Add property</button>
    </div>
    <div class="field-help">Each instance can set a value for these.</div>
  </div>
  <div class="dialog-error" data-test="subflow-error">{error}</div>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="subflow-delete" onclick={remove}>Delete subflow</button>
    <button class="ghost" type="button" data-test="subflow-cancel" onclick={() => done(null)}>Cancel</button>
    <button class="primary" type="button" data-test="subflow-done" onclick={save}>Done</button>
  {/snippet}
</Dialog>
