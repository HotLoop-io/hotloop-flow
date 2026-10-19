// Flow tests in the editor: a test drafted from something the debug sidebar
// showed, the suite run against the working copy, and the outcome drawn on the
// nodes. The tests themselves are the engine's: everything here asks the
// runtime, which runs them the same way the command line and the deploy gate
// do, so the three can't disagree about what passed.

import type { Api, TestReport, TestResult } from './api';
import { ApiError } from './api';
import { modal } from './modal';

/** How a node came out of the last run. */
export interface TestMark {
  state: 'pass' | 'fail';
  title: string;
}

/**
 * Drafts a test from a debug message and offers it for the suite. That's two
 * clicks: the one that opened this, and Add. The draft is editable, because the
 * message it was made from is exactly what the flow did, and a reading that
 * carries a timestamp is better checked with an assert than pinned to the
 * millisecond.
 *
 * Resolves with the names of the tests added, or null when nothing was.
 */
export async function draftTestDialog(api: Api, node: string, msgId: string): Promise<string[] | null> {
  return offerDraft(api, () => api.draftTest(node, msgId), 'What went in at the source, and what this Debug node showed. Change anything you like before adding it.');
}

/** The same two clicks, for a test made from a whole trace: every place the message ended. */
export async function draftTestFromTraceDialog(api: Api, traceId: string): Promise<string[] | null> {
  return offerDraft(api, () => api.draftTestFromTrace(traceId), 'What went in at the source, and what every Debug node or other end of the trace was handed. Change anything you like before adding it.');
}

async function offerDraft(api: Api, make: () => Promise<{ name: string; yaml: string }>, blurb: string): Promise<string[] | null> {
  let draft: { name: string; yaml: string };
  try {
    draft = await make();
  } catch (ex) {
    if (ex instanceof ApiError && ex.status === 401) throw ex;
    alert(`Could not make a test from that message: ${ex instanceof Error ? ex.message : ex}`);
    return null;
  }

  let text = draft.yaml;
  for (;;) {
    const intro = document.createElement('p');
    intro.textContent = blurb;
    const area = document.createElement('textarea');
    area.className = 'code test-yaml';
    area.value = text;
    area.rows = Math.min(18, text.split('\n').length + 1);
    area.spellcheck = false;
    area.dataset.test = 'test-draft';
    const answer = await modal('New flow test', [intro, area], [
      { label: 'Cancel', ghost: true, test: 'test-cancel' },
      { label: 'Add to tests', test: 'test-add' },
    ], area);
    if (answer !== 'test-add') return null;
    text = area.value;
    try {
      const res = await api.addTests(text);
      return res.added;
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) throw ex;
      // Back to the draft with the reason, rather than losing the edit.
      alert(`The test wasn't added: ${ex instanceof Error ? ex.message : ex}`);
    }
  }
}

/** Opens the whole suite as YAML. Resolves true when it was saved. */
export async function editSuiteDialog(api: Api): Promise<boolean> {
  let current: { tests: string; file: string };
  try {
    current = await api.tests();
  } catch (ex) {
    if (ex instanceof ApiError && ex.status === 401) throw ex;
    alert(`Could not load the flow tests: ${ex instanceof Error ? ex.message : ex}`);
    return false;
  }
  let text = current.tests || 'tests:\n';
  for (;;) {
    const area = document.createElement('textarea');
    area.className = 'code test-yaml';
    area.value = text;
    area.rows = 22;
    area.spellcheck = false;
    area.dataset.test = 'tests-yaml';
    const answer = await modal(current.file || 'Flow tests', [area], [
      { label: 'Cancel', ghost: true, test: 'tests-cancel' },
      { label: 'Save', test: 'tests-save' },
    ], area);
    if (answer !== 'tests-save') return false;
    text = area.value;
    try {
      await api.saveTests(text);
      return true;
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) throw ex;
      alert(`The tests weren't saved: ${ex instanceof Error ? ex.message : ex}`);
    }
  }
}

/**
 * What a run means for each node: failed if any problem is about it, passed if
 * a test that passed put a message into it or watched it. Fail wins, because a
 * node that passes one test and fails another is a node to look at.
 */
export function marksFrom(report: TestReport): Map<string, TestMark> {
  const marks = new Map<string, TestMark>();
  const tests = report.tests ?? [];
  for (const t of tests) {
    if (t.status !== 'pass') continue;
    for (const id of t.nodes ?? []) {
      marks.set(id, { state: 'pass', title: `passed: ${t.name}` });
    }
  }
  for (const t of tests) {
    for (const p of t.problems ?? []) {
      if (!p.node) continue;
      const was = marks.get(p.node);
      const line = `${t.name}: ${p.message.split('\n')[0]}`;
      marks.set(p.node, {
        state: 'fail',
        title: was?.state === 'fail' ? `${was.title}\n${line}` : line,
      });
    }
  }
  return marks;
}

/** Draws a report into the sidebar. A problem about a node selects it. */
export function renderReport(host: HTMLElement, report: TestReport, pick: (nodeId: string) => void): void {
  host.replaceChildren();
  const tests = report.tests ?? [];
  const ok = report.failed === 0 && report.errored === 0;
  const summary = document.createElement('div');
  summary.className = `test-summary ${ok ? 'pass' : 'fail'}`;
  summary.dataset.test = 'tests-summary';
  summary.textContent = tests.length === 0
    ? 'No tests ran.'
    : `${report.passed} passed, ${report.failed} failed, ${report.errored} couldn't run`;
  host.append(summary);
  for (const t of tests) host.append(resultRow(t, pick));
}

function resultRow(t: TestResult, pick: (nodeId: string) => void): HTMLElement {
  const row = document.createElement('div');
  row.className = `test-result ${t.status}`;
  row.dataset.test = 'test-result';
  row.dataset.status = t.status;

  const head = document.createElement('div');
  head.className = 'meta';
  const chip = document.createElement('span');
  chip.className = `kind test-${t.status}`;
  chip.textContent = t.status === 'error' ? "couldn't run" : t.status;
  const name = document.createElement('span');
  name.className = 'name';
  name.textContent = t.name;
  head.append(chip, name);
  row.append(head);

  for (const p of t.problems ?? []) {
    const line = document.createElement('pre');
    line.className = 'diff-text test-problem';
    line.textContent = (p.expect ? `expect ${p.expect}: ` : '') + p.message;
    if (p.node) {
      line.classList.add('pickable');
      line.title = 'Show the node';
      line.onclick = () => pick(p.node!);
    }
    row.append(line);
  }
  return row;
}
