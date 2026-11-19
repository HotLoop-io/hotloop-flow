<script lang="ts" module>
  import { tr } from '../../lib/i18n.svelte';
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
    step.kind === 'intro' ? tr('dialogs.mfa.title')
      : step.kind === 'setup' ? tr('dialogs.mfa.title_setup')
        : step.kind === 'done' ? tr('dialogs.mfa.title_on') : tr('dialogs.mfa.title'),
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
    <p>{tr('dialogs.mfa.off_turn_it_on_and')}</p>
  {:else if step.kind === 'setup'}
    {@const qr = qrPath(step.qr)}
    <p>{tr('dialogs.mfa.scan_this_with_your_authenticator')}</p>
    <svg class="qr" viewBox="0 0 {qr.size} {qr.size}" role="img" aria-label={tr('dialogs.mfa.qr_code_for_your_authenticator')}>
      <rect width="100%" height="100%" fill="#ffffff" />
      <path d={qr.d} fill="#000000" />
    </svg>
    <p>{tr('dialogs.mfa.cant_scan_enter_this_key')}</p>
    <code class="mfa-secret" data-test="mfa-secret">{step.secret.replace(/(.{4})/g, '$1 ').trim()}</code>
    {#if step.problem}<p class="dialog-error">{step.problem}</p>{/if}
    <input class="mfa-code" type="text" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="123456" data-test="mfa-confirm-code" bind:this={input} bind:value={code} />
  {:else if step.kind === 'done'}
    <p>{tr('dialogs.mfa.from_now_on_signing_in')}</p>
  {:else}
    <p>{tr('dialogs.mfa.on_turning_it_off_takes')}</p>
    {#if step.problem}<p class="dialog-error">{step.problem}</p>{/if}
    <input class="mfa-code" type="text" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="123456" data-test="mfa-disable-code" bind:this={input} bind:value={code} />
  {/if}
  {#snippet foot()}
    <div class="spacer"></div>
    {#if step.kind === 'intro'}
      <button class="ghost" data-test="mfa-cancel" onclick={() => answer('mfa-cancel')}>{tr('dialogs.mfa.not_now')}</button>
      <button class="primary" data-test="mfa-start" onclick={() => answer('mfa-start')}>{tr('dialogs.mfa.set_it_up')}</button>
    {:else if step.kind === 'setup'}
      <button class="ghost" data-test="mfa-cancel" onclick={() => answer('mfa-cancel')}>{tr('dialogs.mfa.cancel')}</button>
      <button class="primary" data-test="mfa-confirm" onclick={() => answer('mfa-confirm')}>{tr('dialogs.mfa.turn_it_on')}</button>
    {:else if step.kind === 'done'}
      <button class="primary" data-test="mfa-done" onclick={() => answer('mfa-done')}>{tr('dialogs.mfa.done')}</button>
    {:else}
      <button class="ghost" data-test="mfa-cancel" onclick={() => answer('mfa-cancel')}>{tr('dialogs.mfa.keep_it_on')}</button>
      <button class="primary" data-test="mfa-disable" onclick={() => answer('mfa-disable')}>{tr('dialogs.mfa.turn_it_off')}</button>
    {/if}
  {/snippet}
</Dialog>
