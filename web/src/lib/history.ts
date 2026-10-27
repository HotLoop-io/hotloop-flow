// What the deployment log says about a deployment. The rollback and conflict
// dialogs are Svelte components, and they show their words with these.

import type { Deployment } from './api';

/** Who, when and why, in one line. */
export function describe(d: Deployment): string {
  const who = d.user || 'nobody signed in';
  const when = new Date(d.time).toLocaleString();
  return `#${d.seq} ${d.kind} by ${who}, ${when}`;
}

export interface Conflict {
  /** What is live now. */
  latest: Deployment | undefined;
  /** What changed between the revision this editor loaded and what's live. */
  theirChanges: string | null;
  /** False when the revision this editor loaded isn't in the log any more. */
  baseKnown: boolean;
}
