// The one place a theme is applied.
//
// The editor follows the Industrial Dashboard's mechanism, a dark-mode class on
// body, so that it feels native inside the dashboard's iframe. The HotLoop tokens
// flip on data-theme instead. Setting both here, together, is what keeps the two
// from ever disagreeing.

export function setDarkMode(dark: boolean): void {
  document.body.classList.toggle('dark-mode', dark);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
}

export function isDark(): boolean {
  return document.documentElement.dataset.theme === 'dark';
}

/** Applies a theme and, when the person chose it, remembers it. */
export function applyTheme(dark: boolean, remember = true): void {
  setDarkMode(dark);
  if (!remember) return;
  try {
    localStorage.setItem('theme', dark ? 'dark' : 'light');
  } catch {
    /* private mode */
  }
}

export function initialTheme(): boolean {
  try {
    const stored = localStorage.getItem('theme');
    if (stored) return stored === 'dark';
  } catch {
    /* fall through */
  }
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

/** Follows the system until the person picks for themselves. */
export function watchSystemTheme(): void {
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
    let hasChoice = false;
    try {
      hasChoice = localStorage.getItem('theme') !== null;
    } catch {
      /* ignore */
    }
    if (!hasChoice) applyTheme(e.matches, false);
  });
}

/**
 * Follows the dashboard's theme when embedded in its iframe.
 *
 * HotLoop Flow opens inside a fullscreen overlay in the Industrial Dashboard.
 * Without this, toggling the dashboard's theme leaves the embedded app on the
 * other one, which looks broken even though both are individually correct.
 */
export function followParentTheme(): void {
  if (window.parent === window) return;
  window.addEventListener('message', (ev) => {
    // Same-origin only: the dashboard proxies this app so the browser sees one
    // origin. A message from anywhere else is not ours to trust.
    if (ev.origin !== window.location.origin) return;
    const data = ev.data as { type?: string; theme?: string } | null;
    if (data?.type === 'embernet:theme' && (data.theme === 'dark' || data.theme === 'light')) {
      applyTheme(data.theme === 'dark', false);
    }
  });
  window.parent.postMessage({ type: 'embernet:ready', app: 'hotloop-flow' }, window.location.origin);
}
