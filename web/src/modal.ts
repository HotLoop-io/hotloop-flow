// A small modal: a title, a body, a row of buttons, Escape to back out.
// Shared by the history's dialogs and the account's two-factor dialog, so
// every dialog in the editor behaves the same way under the keyboard.

export interface Button {
  label: string;
  ghost?: boolean;
  test: string;
}

/** Opens a modal and resolves with the test id of the button pressed, or null. */
export function modal(title: string, body: Node[], buttons: Button[], focus?: HTMLElement): Promise<string | null> {
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

export interface Shell {
  /** Where the content goes. */
  body: HTMLElement;
  /** The row of buttons along the bottom, with a spacer already in it. */
  foot: HTMLElement;
  close(): void;
}

/**
 * The same frame as modal(), for a dialog that decides for itself when it is
 * finished: an import that has to say what is wrong with the text and stay
 * open, a form that checks its fields. Escape and a click outside close it.
 */
export function shell(title: string, onClose?: () => void): Shell {
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

  const body = document.createElement('div');
  body.className = 'dialog-body';
  const foot = document.createElement('div');
  foot.className = 'dialog-foot';
  const spacer = document.createElement('div');
  spacer.className = 'spacer';
  foot.append(spacer);

  const close = () => {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
    onClose?.();
  };
  const onKey = (e: KeyboardEvent) => {
    if (e.key === 'Escape') close();
  };
  backdrop.onclick = (e) => { if (e.target === backdrop) close(); };
  document.addEventListener('keydown', onKey);

  dialog.append(head, body, foot);
  backdrop.append(dialog);
  document.body.append(backdrop);
  return { body, foot, close };
}

/** A button for a shell's foot. */
export function footButton(label: string, test: string, onClick: () => void, ghost = false): HTMLButtonElement {
  const b = document.createElement('button');
  b.textContent = label;
  b.dataset.test = test;
  b.type = 'button';
  if (ghost) b.className = 'ghost';
  b.onclick = onClick;
  return b;
}

/** Asks for one line of text. Resolves with it, or null if the person backed out or left it blank. */
export function askText(title: string, label: string, initial: string, test: string): Promise<string | null> {
  return new Promise((resolve) => {
    let answer: string | null = null;
    const dlg = shell(title, () => resolve(answer));
    const wrap = document.createElement('div');
    wrap.className = 'field';
    const l = document.createElement('label');
    l.textContent = label;
    const input = document.createElement('input');
    input.type = 'text';
    input.value = initial;
    input.dataset.test = `${test}-input`;
    wrap.append(l, input);
    dlg.body.append(wrap);
    const ok = footButton('Create', `${test}-ok`, () => {
      const v = input.value.trim();
      if (v === '') return;
      answer = v;
      dlg.close();
    });
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') ok.click(); });
    dlg.foot.append(footButton('Cancel', `${test}-cancel`, () => dlg.close(), true), ok);
    input.focus();
    input.select();
  });
}
