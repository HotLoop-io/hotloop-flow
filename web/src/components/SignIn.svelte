<script lang="ts">
  // The runtime's own sign-in. A code field appears only once the runtime says
  // this account has two-factor sign-in on.
  import { type Api, ApiError } from '../lib/api';

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
      error = ex instanceof ApiError ? ex.message : 'could not reach the runtime';
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
      <div class="brand"><img src="brand/logo-mark.svg" alt="" width="30" height="30" />HotLoop Flow</div>
      <p>Sign in to the runtime.</p>
      <div class="error">{error}</div>
      <div>
        <label for="u">Username</label>
        <input id="u" type="text" autocomplete="username" bind:value={username} />
      </div>
      <div>
        <label for="p">Password</label>
        <input id="p" type="password" autocomplete="current-password" bind:this={passwordBox} bind:value={password} />
      </div>
      <div hidden={!needCode}>
        <label for="c">Code from your authenticator app</label>
        <input id="c" type="text" autocomplete="one-time-code" inputmode="numeric" maxlength={6} placeholder="123456" bind:this={codeBox} bind:value={code} />
      </div>
      <button class="primary" type="submit" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
    </form>
  </div>
</div>
