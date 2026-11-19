<script lang="ts">
  import { tr } from '../lib/i18n.svelte';
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
    onadddiagram: () => void;
    ontosubflow: () => void;
    onlayout: () => void;
    onflowprops: () => void;
    onsubflowprops: () => void;
  }
  let { flows, subflows, active, changed, editing, canMakeSubflow, showFlowProps, showSubflowProps, state, dirty, onpick, onedit, onaddflow, onaddsubflow, onadddiagram, ontosubflow, onlayout, onflowprops, onsubflowprops }: Props = $props();
</script>

<div class="tabbar">
  {#each flows as t (t.id)}
    <button class="tab" class:active={t.id === active} class:has-diff={changed.has(t.id)} data-test="tab-flow" data-scope={t.id} onclick={() => onpick(t.id)} ondblclick={() => onedit(t.id)}>{t.label}</button>
  {/each}
  {#each subflows as t (t.id)}
    <button class="tab tab-subflow" class:active={t.id === active} class:has-diff={changed.has(t.id)} data-test="tab-subflow" data-scope={t.id} title={tr('tabbar.subflow')} style:--pip={t.color} onclick={() => onpick(t.id)}>{t.label}</button>
  {/each}
  {#if editing}
    <button class="tab tab-add" data-test="tab-add" title={tr('tabbar.add_a_flow')} aria-label={tr('tabbar.add_a_flow')} onclick={onaddflow}><Icon name="plus" /></button>
    <button class="tab tab-add" data-test="tab-add-subflow" title={tr('tabbar.add_a_subflow')} onclick={onaddsubflow}><Icon name="plus" />{tr('tabbar.subflow_2')}</button>
    <button class="tab tab-add" data-test="tab-add-diagram" title={tr('tabbar.add_a_flowlab_diagram_nodes')} onclick={onadddiagram}><Icon name="plus" />{tr('tabbar.flowlab')}</button>
  {/if}
  <div class="spacer"></div>
  {#if editing}
    <button class="ghost" data-test="auto-layout" title={tr('tabbar.lay_the_nodes_out_so')} onclick={onlayout}>{tr('tabbar.tidy_layout')}</button>
    <button class="ghost" data-test="selection-to-subflow" title={tr('tabbar.move_the_selected_nodes_into')} disabled={!canMakeSubflow} onclick={ontosubflow}>{tr('tabbar.selection_to_subflow')}</button>
  {/if}
  {#if showFlowProps}
    <button class="ghost" data-test="flow-properties" title={tr('tabbar.rename_or_delete_this_flow')} onclick={onflowprops}>{tr('tabbar.flow_properties')}</button>
  {/if}
  {#if showSubflowProps}
    <button class="ghost" data-test="subflow-properties" onclick={onsubflowprops}>{tr('tabbar.subflow_properties')}</button>
  {/if}
  <div class="tab-state mono" class:dirty data-test="tab-state">{state}</div>
</div>
