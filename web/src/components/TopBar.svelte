<script lang="ts">
  // The top bar: who this is, whether it is connected, the editor's few global
  // actions, and Deploy.
  import Icon from './Icon.svelte';

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
    <span>HotLoop Flow</span>
    <span class="version">{version}</span>
  </div>
  <div class="conn mono">
    <span class="dot {live === null ? 'grey' : live ? 'green' : 'red'}"></span>
    <span>{live === null ? 'connecting' : live ? 'live' : 'reconnecting'}</span>
  </div>
  <div class="spacer"></div>
  <button class="ghost" data-test="search-open" title="Search every flow (Ctrl+F)" onclick={onsearch}><Icon name="search" />Search<kbd>Ctrl F</kbd></button>
  <button class="ghost" data-test="import-open" title="Import flows from JSON or a file" disabled={importDisabled} onclick={onimport}>Import</button>
  <button class="ghost" data-test="export-open" title="Export the selection, this flow, or everything" onclick={onexport}>Export</button>
  <span class="sep"></span>
  <button class="ghost icon" title="Fit to view" aria-label="Fit to view" onclick={onfit}><Icon name="fit" /></button>
  <button class="ghost" title="Switch light and dark" onclick={ontheme}>{dark ? 'Light' : 'Dark'}</button>
  {#if canSignOut}
    <button class="ghost" data-test="mfa-open" title="Two-factor sign-in" onclick={onmfa}>Two-factor</button>
    <button class="ghost" onclick={onsignout}>Sign out</button>
  {:else}
    <!-- No Sign out without a login to go back to. The badge takes its place so
         nobody forgets this editor is open to anyone who can reach it. -->
    <span class="no-auth mono" title="Started with HOTLOOP_FLOW_INSECURE: anyone who can reach this port can deploy.">no login</span>
  {/if}
  <button class="primary" disabled={deployDisabled} onclick={ondeploy}>Deploy</button>
</div>
