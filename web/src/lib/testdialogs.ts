// Flow tests in the editor: a test drafted from something the debug dock
// showed, and the whole suite as YAML. The tests themselves are the engine's:
// everything here asks the runtime, which runs them the same way the command
// line and the deploy gate do, so the three can't disagree about what passed.

import { type Api, ApiError } from './api';
import { openDialog } from './dialogs';
import TextEdit from '../components/dialogs/TextEdit.svelte';

interface EditProps extends Record<string, unknown> {
  title: string;
  intro?: string;
  text: string;
  rows: number;
  textTest: string;
  cancelTest: string;
  okLabel: string;
  okTest: string;
}
const edit = (p: EditProps) => openDialog<string | null, EditProps>(TextEdit, p);

/**
 * Drafts a test from a debug message and offers it for the suite. That's two
 * clicks: the one that opened this, and Add. The draft is editable, because the
 * message it was made from is exactly what the flow did, and a reading that
 * carries a timestamp is better checked with an assert than pinned to the
 * millisecond.
 *
 * Resolves with the names of the tests added, or null when nothing was.
 */
export function draftTestDialog(api: Api, node: string, msgId: string): Promise<string[] | null> {
  return offerDraft(api, () => api.draftTest(node, msgId), 'What went in at the source, and what this Debug node showed. Change anything you like before adding it.');
}

/** The same two clicks, for a test made from a whole trace: every place the message ended. */
export function draftTestFromTraceDialog(api: Api, traceId: string): Promise<string[] | null> {
  return offerDraft(api, () => api.draftTestFromTrace(traceId), 'What went in at the source, and what every Debug node or other end of the trace was handed. Change anything you like before adding it.');
}

async function offerDraft(api: Api, make: () => Promise<{ name: string; yaml: string }>, intro: string): Promise<string[] | null> {
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
    const answer = await edit({
      title: 'New flow test', intro, text, rows: Math.min(18, text.split('\n').length + 1),
      textTest: 'test-draft', cancelTest: 'test-cancel', okLabel: 'Add to tests', okTest: 'test-add',
    });
    if (answer === null) return null;
    text = answer;
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
    const answer = await edit({
      title: current.file || 'Flow tests', text, rows: 22,
      textTest: 'tests-yaml', cancelTest: 'tests-cancel', okLabel: 'Save', okTest: 'tests-save',
    });
    if (answer === null) return false;
    text = answer;
    try {
      await api.saveTests(text);
      return true;
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 401) throw ex;
      alert(`The tests weren't saved: ${ex instanceof Error ? ex.message : ex}`);
    }
  }
}
