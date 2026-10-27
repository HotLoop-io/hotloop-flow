// The parameters a subflow takes: a name, a type, a default. The template
// declares them, and each instance may set its own value.

import type { FlowEntry } from './graph';

export const ENV_TYPES: [string, string][] = [
  ['str', 'string'], ['num', 'number'], ['bool', 'boolean'], ['json', 'JSON'], ['env', 'env variable'],
];

export interface EnvVar { name: string; type: string; value: string }

export function envOf(entry: FlowEntry, key = 'env'): EnvVar[] {
  const raw = entry[key];
  if (!Array.isArray(raw)) return [];
  return (raw as Record<string, unknown>[]).map((v) => ({
    name: String(v.name ?? ''),
    type: String(v.type ?? 'str'),
    value: v.value === undefined || v.value === null ? '' : String(v.value),
  }));
}
