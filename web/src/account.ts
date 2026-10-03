// Two-factor sign-in, set up and turned off from the editor.
//
// Free and on for anybody who wants it. Setting it up shows a QR code for the
// phone, the same secret as text for a phone that can't scan, and asks for one
// code back before anything changes, so a setup abandoned half way never locks
// anybody out.

import { type Api, ApiError, type MFASetup } from './api';
import { modal } from './modal';

const SVG_NS = 'http://www.w3.org/2000/svg';

/** Draws the QR code as SVG squares: nothing to fetch, nothing for a CSP to refuse. */
function qrCode(rows: string[]): SVGSVGElement {
  const size = rows.length;
  const quiet = 4; // the margin scanners need around the code
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', `0 0 ${size + quiet * 2} ${size + quiet * 2}`);
  svg.setAttribute('class', 'qr');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', 'QR code for your authenticator app');
  const bg = document.createElementNS(SVG_NS, 'rect');
  bg.setAttribute('width', '100%');
  bg.setAttribute('height', '100%');
  bg.setAttribute('fill', '#ffffff');
  svg.append(bg);
  let d = '';
  rows.forEach((row, y) => {
    for (let x = 0; x < row.length; x++) {
      if (row[x] === '1') d += `M${x + quiet} ${y + quiet}h1v1h-1z`;
    }
  });
  const path = document.createElementNS(SVG_NS, 'path');
  path.setAttribute('d', d);
  path.setAttribute('fill', '#000000');
  svg.append(path);
  return svg;
}

function para(text: string, cls = ''): HTMLParagraphElement {
  const p = document.createElement('p');
  p.textContent = text;
  if (cls) p.className = cls;
  return p;
}

function codeInput(test: string): HTMLInputElement {
  const input = document.createElement('input');
  input.type = 'text';
  input.inputMode = 'numeric';
  input.autocomplete = 'one-time-code';
  input.maxLength = 6;
  input.placeholder = '123456';
  input.className = 'mfa-code';
  input.dataset.test = test;
  return input;
}

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

  const go = await modal('Two-factor sign-in', [
    para('Off. Turn it on and signing in takes your password and a six-digit code from an ' +
      'authenticator app on your phone, so a stolen password alone gets nobody in.'),
  ], [
    { label: 'Not now', ghost: true, test: 'mfa-cancel' },
    { label: 'Set it up', test: 'mfa-start' },
  ]);
  if (go !== 'mfa-start') return;

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
    const input = codeInput('mfa-confirm-code');
    const secret = document.createElement('code');
    secret.className = 'mfa-secret';
    secret.dataset.test = 'mfa-secret';
    secret.textContent = setup.secret.replace(/(.{4})/g, '$1 ').trim();
    const body: Node[] = [
      para('Scan this with your authenticator app, then type the code it shows.'),
      qrCode(setup.qr),
      para("Can't scan? Enter this key by hand, as a time-based code:"),
      secret,
    ];
    if (problem) body.push(para(problem, 'dialog-error'));
    body.push(input);

    const answer = await modal('Set up two-factor sign-in', body, [
      { label: 'Cancel', ghost: true, test: 'mfa-cancel' },
      { label: 'Turn it on', test: 'mfa-confirm' },
    ], input);
    if (answer !== 'mfa-confirm') return;
    try {
      await api.mfaConfirm(input.value);
      await modal('Two-factor sign-in is on', [
        para('From now on, signing in asks for a code from your phone. If you lose the phone, ' +
          'an account with auth.admin can turn it off for you.'),
      ], [{ label: 'Done', test: 'mfa-done' }]);
      return;
    } catch (ex) {
      problem = ex instanceof ApiError ? ex.message : 'could not reach the runtime';
    }
  }
}

async function turnOff(api: Api): Promise<void> {
  let problem = '';
  for (;;) {
    const input = codeInput('mfa-disable-code');
    const body: Node[] = [
      para('On. Turning it off takes a code from your phone, so a session left open on a ' +
        'shared screen is not enough to strip it off your account.'),
    ];
    if (problem) body.push(para(problem, 'dialog-error'));
    body.push(input);
    const answer = await modal('Two-factor sign-in', body, [
      { label: 'Keep it on', ghost: true, test: 'mfa-cancel' },
      { label: 'Turn it off', test: 'mfa-disable' },
    ], input);
    if (answer !== 'mfa-disable') return;
    try {
      await api.mfaDisable(input.value);
      return;
    } catch (ex) {
      problem = ex instanceof ApiError ? ex.message : 'could not reach the runtime';
    }
  }
}
