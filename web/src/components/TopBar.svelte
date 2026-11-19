<script lang="ts">
  import { tr } from '../lib/i18n.svelte';
  // The top bar: who this is, whether it is connected, the editor's few global
  // actions, and Deploy.
  import Icon from './Icon.svelte';
  import LanguagePicker from './LanguagePicker.svelte';

  interface Props {
    version: string;
    /** null while the first connection is being made. */
    live: boolean | null;
    canSignOut: boolean;
    deployDisabled: boolean;
    importDisabled: boolean;
    dark: boolean;
    onsearch: () => void;
    onimport: () => void;
    onexport: () => void;
    onfit: () => void;
    ontheme: () => void;
    ondeploy: () => void;
    onmfa: () => void;
    onsignout: () => void;
  }
  let { version, live, canSignOut, deployDisabled, importDisabled, dark, onsearch, onimport, onexport, onfit, ontheme, ondeploy, onmfa, onsignout }: Props = $props();
</script>

<div class="topbar">
  <div class="brand">
    <img src="brand/logo-mark.svg" alt="" width="26" height="26" />
    <span>{tr('topbar.hotloop_flow')}</span>
    <span class="version">{version}</span>
  </div>
  <div class="conn mono">
    <span class="dot {live === null ? 'grey' : live ? 'green' : 'red'}"></span>
    <span>{live === null ? tr('topbar.connecting') : live ? tr('topbar.live') : tr('topbar.reconnecting')}</span>
  </div>
  <div class="spacer"></div>
  <button class="ghost" data-test="search-open" title={tr('topbar.search_every_flow_ctrlf')} onclick={onsearch}><Icon name="search" />{tr('topbar.search')}<kbd>{tr('topbar.ctrl_f')}</kbd></button>
  <button class="ghost" data-test="import-open" title={tr('topbar.import_flows_from_json_or')} disabled={importDisabled} onclick={onimport}>{tr('topbar.import')}</button>
  <button class="ghost" data-test="export-open" title={tr('topbar.export_the_selection_this_flow')} onclick={onexport}>{tr('topbar.export')}</button>
  <span class="sep"></span>
  <button class="ghost icon" title={tr('topbar.fit_to_view')} aria-label={tr('topbar.fit_to_view')} onclick={onfit}><Icon name="fit" /></button>
  <LanguagePicker compact />
  <button class="ghost" title={tr('topbar.switch_light_and_dark')} onclick={ontheme}>{dark ? tr('topbar.light') : tr('topbar.dark')}</button>
  {#if canSignOut}
    <button class="ghost" data-test="mfa-open" title={tr('topbar.twofactor_signin')} onclick={onmfa}>{tr('topbar.twofactor')}</button>
    <button class="ghost" onclick={onsignout}>{tr('topbar.sign_out')}</button>
  {:else}
    <!-- No Sign out without a login to go back to. The badge takes its place so
         nobody forgets this editor is open to anyone who can reach it. -->
    <span class="no-auth mono" title={tr('topbar.started_with_hotloopflowinsecure_anyone_who')}>{tr('topbar.no_login')}</span>
  {/if}
  <button class="primary" disabled={deployDisabled} onclick={ondeploy}>{tr('topbar.deploy')}</button>
</div>
