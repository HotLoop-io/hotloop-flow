// Two-factor sign-in, set up and turned off from the editor.
//
// Free and on for anybody who wants it. Setting it up shows a QR code for the
// phone, the same secret as text for a phone that can't scan, and asks for one
// code back before anything changes, so a setup abandoned half way never locks
// anybody out.

import { type Api, ApiError, type MFASetup } from './api';
import { openDialog } from './dialogs';
import Mfa, { type Step } from '../components/dialogs/Mfa.svelte';

type Answer = { answer: string | null; code: string };
const ask = (step: Step) => openDialog<Answer, { step: Step }>(Mfa, { step });

const reason = (ex: unknown) => (ex instanceof ApiError ? ex.message : 'could not reach the runtime');

/** Opens the two-factor dialog for the signed-in user. */
export async function twoFactorDialog(api: Api): Promise<void> {
  let enabled: boolean;
  try {
    enabled = (await api.mfaStatus()).enabled;
  } catch (ex) {
    alert(`Could not read your two-factor settings: ${ex instanceof Error ? ex.message : ex}`);
    return;
  }

  if (enabled) {
    await turnOff(api);
    return;
  }

  if ((await ask({ kind: 'intro' })).answer !== 'mfa-start') return;

  let setup: MFASetup;
  try {
    setup = await api.mfaSetup();
  } catch (ex) {
    alert(`Could not start two-factor setup: ${ex instanceof Error ? ex.message : ex}`);
    return;
  }

  // Asked again on a wrong code rather than starting over: the phone already
  // has the secret, and making somebody rescan for a typo is just rude.
  let problem = '';
  for (;;) {
    const { answer, code } = await ask({ kind: 'setup', secret: setup.secret, qr: setup.qr, problem });
    if (answer !== 'mfa-confirm') return;
    try {
      await api.mfaConfirm(code);
      await ask({ kind: 'done' });
      return;
    } catch (ex) {
      problem = reason(ex);
    }
  }
}

async function turnOff(api: Api): Promise<void> {
  let problem = '';
  for (;;) {
    const { answer, code } = await ask({ kind: 'off', problem });
    if (answer !== 'mfa-disable') return;
    try {
      await api.mfaDisable(code);
      return;
    } catch (ex) {
      problem = reason(ex);
    }
  }
}
