// The Export and Import dialogs. The rules for what goes in and out are in
// flowio.ts; this is the form around them.

import { type ExportScope, type ImportResult, exportEntries, importEntries, parseEntries, toText } from './flowio';
import type { Graph, Point } from './graph';
import { footButton, shell } from './modal';

function el<K extends keyof HTMLElementTagNameMap>(tag: K, attrs: Record<string, string> = {}, ...kids: (Node | string)[]): HTMLElementTagNameMap[K] {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') n.className = v; else n.setAttribute(k, v);
  }
  n.append(...kids);
  return n;
}

/** Puts text on the clipboard. Falls back to a selection copy where the page is not a secure context. */
export async function copyText(text: string, from?: HTMLTextAreaElement): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* denied: try the old way */
  }
  if (!from) return false;
  from.select();
  try {
    return document.execCommand('copy');
  } catch {
    return false;
  }
}

/** Shows a scope's flows as JSON to copy or save. */
export function exportDialog(graph: Graph): void {
  const selected = [...graph.selection].filter((id) => graph.byId(id) && !id.startsWith('@')).length;
  let scope: ExportScope = selected > 0 ? 'selection' : 'flow';

  const dlg = shell('Export');
  const text = el('textarea', { class: 'code lang-json', readonly: '', 'data-test': 'export-text', spellcheck: 'false', rows: '14' });
  const info = el('div', { class: 'field-help', 'data-test': 'export-info' });
  const status = el('div', { class: 'field-help', 'data-test': 'export-status' });

  const refresh = () => {
    const entries = exportEntries(graph, scope);
    text.value = toText(entries);
    info.textContent = `${entries.length} entr${entries.length === 1 ? 'y' : 'ies'}. Passwords and other credentials are never exported.`;
  };

  const choices: [ExportScope, string, boolean][] = [
    ['selection', `Selected nodes (${selected})`, selected === 0],
    ['flow', 'This flow', false],
    ['all', 'Every flow', false],
  ];
  const radios = el('div', { class: 'field' }, el('label', {}, 'What to export'));
  for (const [value, label, disabled] of choices) {
    const input = el('input', { type: 'radio', name: 'export-scope', value, 'data-test': `export-${value}` });
    input.checked = value === scope;
    input.disabled = disabled;
    input.onchange = () => { scope = value; refresh(); };
    radios.append(el('div', { class: 'field-inline' }, el('label', {}, input, ' ' + label)));
  }

  dlg.body.append(radios, el('div', { class: 'field' }, text, info, status));

  const copy = footButton('Copy', 'export-copy', async () => {
    const ok = await copyText(text.value, text);
    status.textContent = ok ? 'Copied.' : 'The browser would not copy. Select the text and copy it by hand.';
  });
  const download = footButton('Download', 'export-download', () => {
    const blob = new Blob([text.value], { type: 'application/json' });
    const a = el('a', { href: URL.createObjectURL(blob), download: `hotloop-flow-${scope}.json` });
    document.body.append(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(a.href);
    status.textContent = 'Saved.';
  });
  dlg.foot.append(copy, download, footButton('Close', 'export-close', () => dlg.close(), true));
  refresh();
  copy.focus();
}

/**
 * Takes pasted or chosen flow JSON. Resolves with what was added, or null if
 * it was cancelled. A problem is shown in the dialog, which stays open.
 */
export function importDialog(graph: Graph, at: Point): Promise<ImportResult | null> {
  return new Promise((resolve) => {
    let done: ImportResult | null = null;
    const dlg = shell('Import', () => resolve(done));

    const inSubflow = graph.isSubflowScope(graph.activeTab);
    const text = el('textarea', {
      class: 'code lang-json', 'data-test': 'import-text', spellcheck: 'false', rows: '12',
      placeholder: 'Paste flow JSON here, or choose a file.',
    });
    const file = el('input', { type: 'file', accept: '.json,application/json', 'data-test': 'import-file' });
    file.onchange = async () => {
      const f = file.files?.[0];
      if (f) text.value = await f.text();
    };

    const target = el('select', { 'data-test': 'import-target' },
      el('option', { value: 'here' }, inSubflow ? 'This subflow' : 'This flow'),
      el('option', { value: 'new' }, 'A new flow'));
    const err = el('div', { class: 'dialog-error', 'data-test': 'import-error' });

    dlg.body.append(
      el('div', { class: 'field' }, el('label', {}, 'Flow JSON'), text),
      el('div', { class: 'field' }, el('label', {}, 'Or a file'), file),
      el('div', { class: 'field' }, el('label', {}, 'Put it on'), target,
        el('div', { class: 'field-help' }, 'A file that brings its own flows keeps them, whatever you pick here.')),
      err,
    );

    const go = footButton('Import', 'import-go', () => {
      const parsed = parseEntries(text.value);
      if (!parsed.entries) {
        err.textContent = parsed.error ?? 'That could not be read.';
        return;
      }
      done = importEntries(graph, parsed.entries, {
        scope: graph.activeTab,
        at,
        ...(target.value === 'new' ? { intoNewFlow: 'Imported flow' } : {}),
      });
      dlg.close();
    });
    dlg.foot.append(go, footButton('Cancel', 'import-cancel', () => dlg.close(), true));
    text.focus();
  });
}
