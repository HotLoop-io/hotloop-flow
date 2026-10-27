// Flow tests in the editor: the outcome of a run, drawn on the nodes. The tests
// themselves are the engine's: the runtime runs them the same way the command
// line and the deploy gate do, so the three can't disagree about what passed.
// The dialogs and the report panel are Svelte components.

import type { TestReport } from './api';

/** How a node came out of the last run. */
export interface TestMark {
  state: 'pass' | 'fail';
  title: string;
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
