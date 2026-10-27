<script lang="ts" module>
  export type Step =
    | { kind: 'intro' }
    | { kind: 'setup'; secret: string; qr: string[]; problem: string }
    | { kind: 'done' }
    | { kind: 'off'; problem: string };
</script>

<script lang="ts">
  // The four things two-factor sign-in asks of a person: whether to set it up,
  // the setup itself with the code that proves the phone has it, a word that it
  // is on, and the code that turns it off. account.ts walks them in order.
  import Dialog from './Dialog.svelte';

  interface Props {
    step: Step;
    done: (answer: { answer: string | null; code: string }) => void;
  }
  let { step, done }: Props = $props();

  let code = $state('');
  let input: HTMLInputElement | undefined = $state();
  $effect(() => input?.focus());

  const answer = (a: string | null) => done({ answer: a, code });
  const title = $derived(
    step.kind === 'intro' ? 'Two-factor sign-in'
      : step.kind === 'setup' ? 'Set up two-factor sign-in'
        : step.kind === 'done' ? 'Two-factor sign-in is on' : 'Two-factor sign-in',
  );

  /** Draws the QR code as squares: nothing to fetch, nothing for a CSP to refuse. */
  function qrPath(rows: string[]): { d: string; size: number } {
    const quiet = 4; // the margin scanners need around the code
    let d = '';
    rows.forEach((row, y) => {
      for (let x = 0; x < row.length; x++) if (row[x] === '1') d += `M${x + quiet} ${y + quiet}h1v1h-1z`;
    });
    return { d, size: rows.length + quiet * 2 };
  }
</script>

<Dialog {title} size="narrow" onclose={() => answer(null)}>
  {#if step.kind === 'intro'}
    <p>Off. Turn it on and signing in takes your password and a six-digit code from an authenticator app on your phone, so a stolen password alone gets nobody in.</p>
  {:else if step.kind === 'setup'}
    {@const qr = qrPath(step.qr)}
    <p>Scan this with your authenticator app, then type the code it shows.</p>
    <svg class="qr" viewBox="0 0 {qr.size} {qr.size}" role="img" aria-label="QR code for your authenticator app">
      <rect width="100%" height="100%" fill="#ffffff" />
      <path d={qr.d} fill="#000000" />
    </svg>
    <p>Can't scan? Enter this key by hand, as a time-based code:</p>
    <code class="mfa-secret" data-test="mfa-secret">{step.secret.replace(/(.{4})/g, '$1 ').trim()}</code>
    {#if step.problem}<p class="dialog-error">{step.problem}</p>{/if}
    <input class="mfa-code" type="text" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="123456" data-test="mfa-confirm-code" bind:this={input} bind:value={code} />
  {:else if step.kind === 'done'}
    <p>From now on, signing in asks for a code from your phone. If you lose the phone, an account with auth.admin can turn it off for you.</p>
  {:else}
    <p>On. Turning it off takes a code from your phone, so a session left open on a shared screen is not enough to strip it off your account.</p>
    {#if step.problem}<p class="dialog-error">{step.problem}</p>{/if}
    <input class="mfa-code" type="text" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="123456" data-test="mfa-disable-code" bind:this={input} bind:value={code} />
  {/if}
  {#snippet foot()}
    <div class="spacer"></div>
    {#if step.kind === 'intro'}
      <button class="ghost" data-test="mfa-cancel" onclick={() => answer('mfa-cancel')}>Not now</button>
      <button class="primary" data-test="mfa-start" onclick={() => answer('mfa-start')}>Set it up</button>
    {:else if step.kind === 'setup'}
      <button class="ghost" data-test="mfa-cancel" onclick={() => answer('mfa-cancel')}>Cancel</button>
      <button class="primary" data-test="mfa-confirm" onclick={() => answer('mfa-confirm')}>Turn it on</button>
    {:else if step.kind === 'done'}
      <button class="primary" data-test="mfa-done" onclick={() => answer('mfa-done')}>Done</button>
    {:else}
      <button class="ghost" data-test="mfa-cancel" onclick={() => answer('mfa-cancel')}>Keep it on</button>
      <button class="primary" data-test="mfa-disable" onclick={() => answer('mfa-disable')}>Turn it off</button>
    {/if}
  {/snippet}
</Dialog>
