// The dialogs that go with the deployment log: confirming a rollback, and
// telling somebody what the other person deployed when theirs and yours
// collide.
//
// The conflict dialog replaces a browser confirm() that asked "overwrite theirs
// or lose yours?" without saying what "theirs" was. Nobody can answer that
// question blind, so now it shows the other person's change, node by node, from
// the engine's diff, with who did it and why.

import type { Deployment } from './api';

interface Button {
  label: string;
  ghost?: boolean;
  test: string;
}

/** Opens a modal and resolves with the test id of the button pressed, or null. */
function modal(title: string, body: Node[], buttons: Button[], focus?: HTMLElement): Promise<string | null> {
  return new Promise((resolve) => {
    const backdrop = document.createElement('div');
    backdrop.className = 'dialog-backdrop';
    const dialog = document.createElement('div');
    dialog.className = 'dialog';
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');

    const head = document.createElement('div');
    head.className = 'dialog-head';
    const h = document.createElement('h2');
    h.textContent = title;
    head.append(h);

    const main = document.createElement('div');
    main.className = 'dialog-body';
    main.append(...body);

    const foot = document.createElement('div');
    foot.className = 'dialog-foot';
    const spacer = document.createElement('div');
    spacer.className = 'spacer';
    foot.append(spacer);

    const close = (answer: string | null) => {
      document.removeEventListener('keydown', onKey);
      backdrop.remove();
      resolve(answer);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close(null);
    };
    for (const b of buttons) {
      const btn = document.createElement('button');
      btn.textContent = b.label;
      if (b.ghost) btn.className = 'ghost';
      btn.dataset.test = b.test;
      btn.onclick = () => close(b.test);
      foot.append(btn);
    }

    dialog.append(head, main, foot);
    backdrop.append(dialog);
    document.body.append(backdrop);
    document.addEventListener('keydown', onKey);
    (focus ?? foot.querySelector('button'))?.focus();
  });
}

function para(text: string): HTMLParagraphElement {
  const p = document.createElement('p');
  p.textContent = text;
  return p;
}

/** Who, when and why, in one line. */
export function describe(d: Deployment): string {
  const who = d.user || 'nobody signed in';
  const when = new Date(d.time).toLocaleString();
  return `#${d.seq} ${d.kind} by ${who}, ${when}`;
}

/**
 * Asks before a rollback, and asks why. Resolves with the note, or null when
 * the operator backs out.
 */
export async function confirmRollback(d: Deployment, unsaved: boolean): Promise<string | null> {
  const note = document.createElement('textarea');
  note.className = 'note';
  note.placeholder = 'Why? This goes in the deployment log.';
  note.dataset.test = 'rollback-note';

  const body: Node[] = [
    para(`This deploys ${describe(d)} again, flows and credentials, as a new deployment. ` +
      'Nothing in the history is changed or removed.'),
  ];
  if (d.note) body.push(para(`Its note: "${d.note}"`));
  if (unsaved) {
    const warn = para('You have changes in the editor that were never deployed. A rollback throws them away.');
    warn.className = 'banner warn';
    body.push(warn);
  }
  const label = document.createElement('label');
  label.textContent = 'Note';
  body.push(label, note);

  const answer = await modal(`Roll back to deployment ${d.seq}?`, body, [
    { label: 'Cancel', ghost: true, test: 'rollback-cancel' },
    { label: 'Roll back', test: 'rollback-confirm' },
  ], note);
  return answer === 'rollback-confirm' ? note.value : null;
}

export interface Conflict {
  /** What is live now. */
  latest: Deployment | undefined;
  /** What changed between the revision this editor loaded and what's live. */
  theirChanges: string | null;
  /** False when the revision this editor loaded isn't in the log any more. */
  baseKnown: boolean;
}

/**
 * Somebody deployed while you were editing. Shows what they changed and lets
 * you pick: keep yours and overwrite theirs, or throw yours away and load
 * theirs. Resolves with 'mine', 'theirs', or null to back out and keep editing.
 */
export async function resolveConflict(c: Conflict): Promise<'mine' | 'theirs' | null> {
  const body: Node[] = [];
  if (c.latest) {
    body.push(para(`Somebody deployed since you loaded these flows: ${describe(c.latest)}.`));
    if (c.latest.note) body.push(para(`Their note: "${c.latest.note}"`));
  } else {
    body.push(para('Somebody deployed since you loaded these flows.'));
  }
  if (c.theirChanges) {
    body.push(para('What they changed:'));
    const pre = document.createElement('pre');
    pre.className = 'diff-text';
    pre.dataset.test = 'their-changes';
    pre.textContent = c.theirChanges;
    body.push(pre);
  } else if (!c.baseKnown) {
    body.push(para("The revision you loaded isn't in the deployment log any more, so what they changed can't be shown."));
  }
  body.push(para('Overwriting deploys your version over theirs. Loading theirs throws your changes away.'));

  const answer = await modal('Your flows are out of date', body, [
    { label: 'Keep editing', ghost: true, test: 'conflict-cancel' },
    { label: 'Load theirs', ghost: true, test: 'conflict-theirs' },
    { label: 'Overwrite with mine', test: 'conflict-mine' },
  ]);
  if (answer === 'conflict-mine') return 'mine';
  if (answer === 'conflict-theirs') return 'theirs';
  return null;
}
