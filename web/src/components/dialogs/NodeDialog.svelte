<script lang="ts">
  // A node's settings. The form is made from the node type's descriptor, so every
  // type gets a working dialog with no front-end change, and a type this runtime
  // does not implement is edited as raw JSON and kept exactly as it was.
  import type { Descriptor, PropDef } from '../../lib/api';
  import type { FlowEntry, Graph } from '../../lib/graph';
  import { IN_KEY, OUT_KEY } from '../../lib/wiretypes';
  import { type Model, type Read, initProp, initTypes, rawOf, readProp, readRaw, readTypes } from '../../lib/propform';
  import Dialog from './Dialog.svelte';
  import PropField from './PropField.svelte';

  interface Props {
    graph: Graph;
    entry: FlowEntry;
    descriptor: Descriptor | undefined;
    done: (r: { props: Record<string, unknown> } | null) => void;
  }
  let { graph, entry, descriptor, done }: Props = $props();

  const nameProp: PropDef = { name: 'name', kind: 'string', label: 'Name' };
  const hasName = !!descriptor?.props?.some((p) => p.name === 'name');
  const fields: PropDef[] = [...(hasName ? [] : [nameProp]), ...(descriptor?.props ?? [])];
  const model: Model = $state({});
  for (const p of fields) initProp(p, entry, model);

  const showTypes = !!descriptor && entry.id !== '' && !graph.isConfig(entry);
  const types = $state(initTypes(entry, graph.outputCount(entry)));
  const typesOpen = entry[IN_KEY] !== undefined || entry[OUT_KEY] !== undefined;
  let raw = $state(rawOf(entry));
  let error = $state('');

  const level = descriptor?.compatibility.level ?? '';

  function commit(): void {
    const values: Record<string, unknown> = {};
    const problems: string[] = [];
    const take = (r: Read) => {
      if (r.error) problems.push(r.error);
      else Object.assign(values, r.values);
    };
    for (const p of fields) take(readProp(p, model, entry, graph));
    if (!descriptor) take(readRaw(raw));
    if (showTypes) take(readTypes(types));
    if (problems.length > 0) {
      error = problems[0]!;
      return;
    }
    done({ props: values });
  }
</script>

<Dialog title={descriptor?.paletteLabel ?? entry.type} chip={descriptor?.color ?? ''} backdropClose focusField onclose={() => done(null)} onsubmit={commit}>
  {#snippet badge()}
    {#if descriptor}
      <span class="badge {level === 'hotloop-flow-only' ? 'only' : level}" title={descriptor.compatibility.notes ?? ''}>{level}</span>
    {/if}
  {/snippet}

  {#if !descriptor}
    <div class="banner">
      {entry.type} is not installed in this runtime. Its configuration is shown as raw JSON and will be preserved exactly as it is.
    </div>
  {/if}

  {#each fields as p (p.name)}
    <PropField {p} {model} {entry} {graph} />
  {/each}

  {#if !descriptor}
    <div class="field">
      <label>Configuration</label>
      <textarea class="code lang-json" spellcheck="false" rows="10" bind:value={raw}></textarea>
    </div>
  {/if}

  {#if showTypes}
    <details class="dialog-types" data-test="types" open={typesOpen}>
      <summary>Types</summary>
      <div class="field-help">
        Optional JSON Schemas over the whole message, so the payload is properties.payload. A wire between two typed ends that can never agree is refused at deploy; anything else that breaks a schema goes to a Catch node with the path of the bad field.
      </div>
      <div class="field">
        <label>Accepts (input)</label>
        <textarea class="code lang-json" spellcheck="false" rows="4" data-test="schema-in" bind:value={types.accepts}></textarea>
      </div>
      {#each types.promises as _, i (i)}
        <div class="field">
          <label>{types.ports === 1 ? 'Promises (output)' : `Promises (output ${i + 1})`}</label>
          <textarea class="code lang-json" spellcheck="false" rows="4" data-test="schema-out-{i + 1}" bind:value={types.promises[i]}></textarea>
        </div>
      {/each}
    </details>
  {/if}

  {#snippet help()}
    {#if descriptor?.help}
      <details class="dialog-help">
        <summary>About this node</summary>
        <p>{descriptor.help}</p>
        {#if descriptor.compatibility.notes}<p class="muted">{descriptor.compatibility.notes}</p>{/if}
      </details>
    {/if}
  {/snippet}

  {#snippet foot()}
    <div class="dialog-error">{error}</div>
    <span class="hint">Ctrl+Enter finishes</span>
    <button class="ghost" onclick={() => done(null)}>Cancel</button>
    <button class="primary" onclick={commit}>Done</button>
  {/snippet}
</Dialog>
