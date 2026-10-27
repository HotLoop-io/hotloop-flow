// Opens a Svelte dialog from plain code and resolves with its answer.
//
// The editor's flows are written as "ask, then act": await the dialog, look at
// what came back. A dialog is a component that is given `done` and calls it once
// with its answer (or null for backing out). This mounts it in the page, waits,
// and takes it down again.

import { mount, unmount, type Component } from 'svelte';

/** What every dialog component is given. */
export interface DialogProps<R> {
  done: (answer: R) => void;
}

export function openDialog<R, P extends Record<string, unknown> = Record<string, never>>(
  component: Component<P & DialogProps<R>>,
  props: P,
): Promise<R> {
  return new Promise((resolve) => {
    const target = document.createElement('div');
    document.body.append(target);
    let instance: Record<string, unknown> | null = null;
    let finished = false;
    const done = (answer: R) => {
      if (finished) return;
      finished = true;
      // Unmounted after the current handler returns, so a button that closes
      // its own dialog is not torn down in the middle of its click.
      queueMicrotask(() => {
        if (instance) void unmount(instance);
        target.remove();
        resolve(answer);
      });
    };
    instance = mount(component, { target, props: { ...props, done } as P & DialogProps<R> });
  });
}
