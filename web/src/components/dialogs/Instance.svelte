<script lang="ts" module>
  import type { EnvVar as Env } from '../../lib/subflowenv';
  export type InstanceOutcome = { kind: 'save'; name: string; env: Env[] } | { kind: 'edit' } | null;
</script>

<script lang="ts">
  // An instance on a flow: its name, and a value for each parameter its template has.
  import type { FlowEntry } from '../../lib/graph';
  import { type EnvVar, ENV_TYPES, envOf } from '../../lib/subflowenv';
  import Dialog from './Dialog.svelte';

  interface Props {
    entry: FlowEntry;
    template: FlowEntry;
    done: (o: InstanceOutcome) => void;
  }
  let { entry, template, done }: Props = $props();

  const declared = envOf(template);
  const mine = new Map(envOf(entry).map((v) => [v.name, v]));
  let name = $state(String(entry.name ?? ''));
  const rows = $state(declared.map((decl) => {
    const set = mine.get(decl.name);
    return { decl, type: set?.type ?? decl.type, value: set?.value ?? decl.value };
  }));
  const info = typeof template.info === 'string' && template.info ? template.info : '';
  let nameInput: HTMLInputElement | undefined = $state();
  $effect(() => nameInput?.focus());

  function save(): void {
    const env: EnvVar[] = [];
    for (const r of rows) {
      const was = mine.get(r.decl.name);
      const changed = r.value !== r.decl.value || r.type !== r.decl.type;
      if (was || changed) env.push({ name: r.decl.name, type: r.type, value: r.value });
    }
    done({ kind: 'save', name, env });
  }
</script>

<Dialog title="Subflow: {String(template.name || 'subflow')}" backdropClose onclose={() => done(null)}>
  <div class="field">
    <label>Name</label>
    <input type="text" data-test="instance-name" placeholder={String(template.name || 'subflow')} bind:this={nameInput} bind:value={name} />
  </div>
  {#if declared.length > 0}
    <div class="field-help">Properties this subflow takes. Each instance can set its own.</div>
  {/if}
  {#each rows as r (r.decl.name)}
    <div class="field">
      <label>{r.decl.name}</label>
      <div class="typed-input">
        <select data-test="instance-env-type-{r.decl.name}" bind:value={r.type}>
          {#each ENV_TYPES as [v, label] (v)}<option value={v}>{label}</option>{/each}
        </select>
        <input type="text" data-test="instance-env-{r.decl.name}" bind:value={r.value} />
      </div>
    </div>
  {/each}
  {#if info}<div class="field-help">{info}</div>{/if}
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" type="button" data-test="instance-edit" onclick={() => done({ kind: 'edit' })}>Edit subflow</button>
    <button class="ghost" type="button" data-test="instance-cancel" onclick={() => done(null)}>Cancel</button>
    <button class="primary" type="button" data-test="instance-done" onclick={save}>Done</button>
  {/snippet}
</Dialog>
