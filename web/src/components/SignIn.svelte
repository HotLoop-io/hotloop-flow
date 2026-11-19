<script lang="ts">
  import { tr } from '../lib/i18n.svelte';
  // The runtime's own sign-in. A code field appears only once the runtime says
  // this account has two-factor sign-in on.
  import { type Api, ApiError } from '../lib/api';
  import LanguagePicker from './LanguagePicker.svelte';

  interface Props {
    api: Api;
    message?: string;
    onsignedin: () => void;
  }
  let { api, message = '', onsignedin }: Props = $props();

  // svelte-ignore state_referenced_locally
  let error = $state(message);
  let username = $state('admin');
  let password = $state('');
  let code = $state('');
  let needCode = $state(false);
  let busy = $state(false);
  let passwordBox: HTMLInputElement | undefined = $state();
  let codeBox: HTMLInputElement | undefined = $state();
  $effect(() => passwordBox?.focus());

  async function submit(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      await api.login(username, password, needCode ? code : '');
      onsignedin();
    } catch (ex) {
      error = ex instanceof ApiError ? ex.message : tr('app.could_not_reach_runtime');
      busy = false;
      if (ex instanceof ApiError && ex.mfaRequired) {
        needCode = true;
        code = '';
        queueMicrotask(() => codeBox?.focus());
      } else {
        passwordBox?.focus();
      }
    }
  }
</script>

<div class="login-wrap">
  <div class="login">
    <form onsubmit={submit}>
      <div class="login-lang"><LanguagePicker /></div>
      <div class="brand"><img src="brand/logo-mark.svg" alt="" width="30" height="30" />{tr('signin.hotloop_flow')}</div>
      <p>{tr('signin.sign_in_to_the_runtime')}</p>
      <div class="error">{error}</div>
      <div>
        <label for="u">{tr('signin.username')}</label>
        <input id="u" type="text" autocomplete="username" bind:value={username} />
      </div>
      <div>
        <label for="p">{tr('signin.password')}</label>
        <input id="p" type="password" autocomplete="current-password" bind:this={passwordBox} bind:value={password} />
      </div>
      <div hidden={!needCode}>
        <label for="c">{tr('signin.code_from_your_authenticator_app')}</label>
        <input id="c" type="text" autocomplete="one-time-code" inputmode="numeric" maxlength={6} placeholder="123456" bind:this={codeBox} bind:value={code} />
      </div>
      <button class="primary" type="submit" disabled={busy}>{busy ? tr('signin.signing_in') : tr('signin.sign_in')}</button>
    </form>
  </div>
</div>
