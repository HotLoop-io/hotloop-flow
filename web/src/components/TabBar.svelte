<script lang="ts">
  // One tab per flow and per subflow template, and the actions that belong to
  // the flow on show.
  import Icon from './Icon.svelte';

  interface Props {
    flows: { id: string; label: string }[];
    subflows: { id: string; label: string; color: string }[];
    active: string;
    /** Tabs with something changed on them, in a review or a deployment on show. */
    changed: Set<string>;
    editing: boolean;
    canMakeSubflow: boolean;
    showFlowProps: boolean;
    showSubflowProps: boolean;
    state: string;
    dirty: boolean;
    onpick: (id: string) => void;
    onedit: (id: string) => void;
    onaddflow: () => void;
    onaddsubflow: () => void;
    ontosubflow: () => void;
    onflowprops: () => void;
    onsubflowprops: () => void;
  }
  let { flows, subflows, active, changed, editing, canMakeSubflow, showFlowProps, showSubflowProps, state, dirty, onpick, onedit, onaddflow, onaddsubflow, ontosubflow, onflowprops, onsubflowprops }: Props = $props();
</script>

<div class="tabbar">
  {#each flows as t (t.id)}
    <button class="tab" class:active={t.id === active} class:has-diff={changed.has(t.id)} data-test="tab-flow" data-scope={t.id} onclick={() => onpick(t.id)} ondblclick={() => onedit(t.id)}>{t.label}</button>
  {/each}
  {#each subflows as t (t.id)}
    <button class="tab tab-subflow" class:active={t.id === active} class:has-diff={changed.has(t.id)} data-test="tab-subflow" data-scope={t.id} title="Subflow" style:--pip={t.color} onclick={() => onpick(t.id)}>{t.label}</button>
  {/each}
  {#if editing}
    <button class="tab tab-add" data-test="tab-add" title="Add a flow" aria-label="Add a flow" onclick={onaddflow}><Icon name="plus" /></button>
    <button class="tab tab-add" data-test="tab-add-subflow" title="Add a subflow" onclick={onaddsubflow}><Icon name="plus" />subflow</button>
  {/if}
  <div class="spacer"></div>
  {#if editing}
    <button class="ghost" data-test="selection-to-subflow" title="Move the selected nodes into a subflow and put an instance where they were" disabled={!canMakeSubflow} onclick={ontosubflow}>Selection to subflow</button>
  {/if}
  {#if showFlowProps}
    <button class="ghost" data-test="flow-properties" title="Rename or delete this flow" onclick={onflowprops}>Flow properties</button>
  {/if}
  {#if showSubflowProps}
    <button class="ghost" data-test="subflow-properties" onclick={onsubflowprops}>Subflow properties</button>
  {/if}
  <div class="tab-state mono" class:dirty data-test="tab-state">{state}</div>
</div>
