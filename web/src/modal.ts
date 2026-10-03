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
