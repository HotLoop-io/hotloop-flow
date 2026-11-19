<script lang="ts">
  import { initLanguage, lang, serverText, tr } from '../lib/i18n.svelte';
  // The whole editor is this one island: it asks the runtime who is signed in,
  // then shows the sign-in, the dashboard-only notice, or the editor.
  import '../tokens.css';
  import '../fonts.css';
  import '../flow.css';
  import { onMount } from 'svelte';
  import { Api, ApiError, type Descriptor, type Settings } from '../lib/api';
  import { applyTheme, followParentTheme, initialTheme, watchSystemTheme } from '../lib/theme';
  import Editor from './Editor.svelte';
  import Panel from './Panel.svelte';
  import SignIn from './SignIn.svelte';
  import LanguagePicker from './LanguagePicker.svelte';

  const api = new Api('');

  type View =
    | { kind: 'boot' }
    | { kind: 'login'; message: string }
    | { kind: 'dashboard' }
    | { kind: 'editor'; settings: Settings; descriptors: Descriptor[] };
  let view: View = $state({ kind: 'boot' });
  // A panel address is the editor's address with #/panel/<diagram>, so an
  // operator signs in the same way and gets the page without the editor.
  const panelId = (): string | null => {
    const m = /^#\/panel\/(.+)$/.exec(location.hash);
    return m ? decodeURIComponent(m[1]!) : null;
  };

  async function start(): Promise<void> {
    // Remembered before the calls, because a 401 drops the token: a stale one
    // means "session expired", no token at all means "sign in".
    const hadToken = api.authenticated;
    try {
      const [settings, descriptors] = await Promise.all([api.settings(), api.nodes(lang())]);
      view = { kind: 'editor', settings, descriptors };
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) {
        if (!hadToken && (await api.authMode()).hideLogin) {
          view = { kind: 'dashboard' };
          return;
        }
        view = { kind: 'login', message: hadToken ? tr('app.session_expired') : '' };
        return;
      }
      view = { kind: 'login', message: ex instanceof Error ? serverText(ex.message) : tr('app.could_not_reach_runtime') };
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
    void initLanguage().then(start);
  });
</script>

{#if view.kind === 'login'}
  {#key view}
    <SignIn {api} message={view.message} onsignedin={() => void start()} />
  {/key}
{:else if view.kind === 'dashboard'}
  <div class="login-wrap">
    <div class="login">
      <div class="brand"><img src="brand/logo-mark.svg" alt="" width="30" height="30" />{tr('app.hotloop_flow')}</div>
      <p>{tr('app.open_this_from_the_embercore')}</p>
      <div class="login-lang"><LanguagePicker /></div>
    </div>
  </div>
{:else if view.kind === 'editor' && panelId()}
  <Panel {api} id={panelId()!} onsessionlost={() => (view = { kind: 'login', message: tr('app.session_expired') })} />
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
    onsessionlost={() => (view = { kind: 'login', message: tr('app.session_expired') })}
  />
{/if}
