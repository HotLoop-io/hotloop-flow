<script lang="ts">
  // The frame every dialog in the editor shares: a backdrop, a header, a body,
  // an optional help drawer, and a foot of buttons. Escape backs out of all of
  // them, so every dialog behaves the same way under the keyboard.
  import type { Snippet } from 'svelte';

  interface Props {
    title: string;
    sub?: string;
    /** The node's category color, as a strip beside the title. */
    chip?: string;
    /** 'narrow' or 'wide', for the dialogs that want a different measure. */
    size?: '' | 'narrow' | 'wide';
    /** Whether a click outside backs out. The forms that can lose typing do not. */
    backdropClose?: boolean;
    onclose: () => void;
    /** Ctrl+Enter, for the dialogs that finish that way. */
    onsubmit?: () => void;
    /** Focus the first field rather than the first button when nothing else took it. */
    focusField?: boolean;
    badge?: Snippet;
    children: Snippet;
    help?: Snippet;
    foot: Snippet;
  }

  let { title, sub = '', chip = '', size = '', backdropClose = false, focusField = false, onclose, onsubmit, badge, children, help, foot }: Props = $props();

  let frame: HTMLElement | undefined = $state();
  // Whoever asked for a dialog may have focused a field of their own. Otherwise
  // it opens on its first field or its first button, so the keyboard works at once.
  $effect(() => {
    const el = frame;
    const t = setTimeout(() => {
      if (!el || el.contains(document.activeElement)) return;
      el.querySelector<HTMLElement>(focusField ? 'input, textarea, select' : '.dialog-foot button')?.focus();
    }, 0);
    return () => clearTimeout(t);
  });

  function key(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      onclose();
    } else if (onsubmit && e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      onsubmit();
    }
  }
</script>

<svelte:document onkeydown={key} />

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="dialog-backdrop" onclick={(e) => { if (backdropClose && e.target === e.currentTarget) onclose(); }}>
  <div class="dialog {size}" bind:this={frame} role="dialog" aria-modal="true" aria-label={title}>
    <div class="dialog-head">
      {#if chip}<span class="chip" style:background={chip}></span>{/if}
      <h2>{title}</h2>
      {#if sub}<span class="sub">{sub}</span>{/if}
      {@render badge?.()}
    </div>
    <div class="dialog-body">{@render children()}</div>
    {@render help?.()}
    <div class="dialog-foot">{@render foot()}</div>
  </div>
</div>
