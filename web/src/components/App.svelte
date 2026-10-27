<script lang="ts">
  // The whole editor is this one island: it asks the runtime who is signed in,
  // then shows the sign-in, the dashboard-only notice, or the editor.
  import '../tokens.css';
  import '../fonts.css';
  import '../flow.css';
  import { onMount } from 'svelte';
  import { Api, ApiError, type Descriptor, type Settings } from '../lib/api';
  import { applyTheme, followParentTheme, initialTheme, watchSystemTheme } from '../lib/theme';
  import Editor from './Editor.svelte';
  import SignIn from './SignIn.svelte';

  const api = new Api('');

  type View =
    | { kind: 'boot' }
    | { kind: 'login'; message: string }
    | { kind: 'dashboard' }
    | { kind: 'editor'; settings: Settings; descriptors: Descriptor[] };
  let view: View = $state({ kind: 'boot' });

  async function start(): Promise<void> {
    // Remembered before the calls, because a 401 drops the token: a stale one
    // means "session expired", no token at all means "sign in".
    const hadToken = api.authenticated;
    try {
      const [settings, descriptors] = await Promise.all([api.settings(), api.nodes()]);
      view = { kind: 'editor', settings, descriptors };
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        if (!hadToken && (await api.authMode()).hideLogin) {
          view = { kind: 'dashboard' };
          return;
        }
        view = { kind: 'login', message: hadToken ? 'Session expired. Sign in again.' : '' };
        return;
      }
      view = { kind: 'login', message: ex instanceof Error ? ex.message : 'could not reach the runtime' };
    }
  }

  function signOut(): void {
    api.logout();
    view = { kind: 'login', message: '' };
  }

  onMount(() => {
    applyTheme(initialTheme(), false);
    watchSystemTheme();
    followParentTheme();
    // Ask the runtime rather than assume. It answers 401 when authentication is
    // on and there is no valid token, which lands on the sign-in, and answers
    // when HOTLOOP_FLOW_INSECURE turned authentication off.
    void start();
  });
</script>

{#if view.kind === 'login'}
  {#key view}
    <SignIn {api} message={view.message} onsignedin={() => void start()} />
  {/key}
{:else if view.kind === 'dashboard'}
  <div class="login-wrap">
    <div class="login">
      <div class="brand"><img src="brand/logo-mark.svg" alt="" width="30" height="30" />HotLoop Flow</div>
      <p>Open this from the EmberCORE dashboard. If you were signed in there and still see this, your role does not include Flow.</p>
    </div>
  </div>
{:else if view.kind === 'editor'}
  <!-- With authentication off there is nobody to sign out and no screen to sign
       back in on, so the editor gets no Sign out button at all. Nor when the
       dashboard signed this person in: signing out of Flow would only land on a
       form the dashboard has taken away. -->
  <Editor
    {api}
    descriptors={view.descriptors}
    version={view.settings.version}
    canSignOut={view.settings.auth.enabled && !view.settings.auth.sso}
    onsignout={signOut}
    onsessionlost={() => (view = { kind: 'login', message: 'Session expired. Sign in again.' })}
  />
{/if}
