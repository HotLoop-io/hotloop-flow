// The node dialog's form, as data. Every node type declares its properties in
// its descriptor, and the dialog is made from them: there is no per-node markup
// anywhere, so a node that declares itself correctly gets a working dialog.
//
// A model is the form's state for one entry (or one row of a list). initModel
// fills it from the entry, the dialog's fields edit it, and readModel turns it
// back into the properties to save, or into the first thing wrong with it.

import type { PropDef } from './api';
import type { FlowEntry, Graph } from './graph';
import { IN_KEY, OUT_KEY } from './wiretypes';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Model = Record<string, any>;
export type Read = { values?: Record<string, unknown>; error?: string };

/** The sentinel the runtime understands as "keep the stored credential". */
export const UNCHANGED = '__PWRD__';

export const TYPE_LABELS: Record<string, string> = {
  msg: 'msg.', flow: 'flow.', global: 'global.', str: 'string', num: 'number',
  bool: 'boolean', json: 'JSON', bin: 'buffer', re: 'regex', date: 'timestamp',
  jsonata: 'JSONata', env: 'env',
};

export const typeKeyOf = (p: PropDef): string => p.typeProp ?? `${p.name}Type`;
export const typesOf = (p: PropDef): string[] => p.typedInputTypes ?? ['msg', 'flow', 'global', 'str', 'num', 'bool', 'json', 'env'];

/** Fills a model for one property from the entry it edits. */
export function initProp(p: PropDef, entry: Model, model: Model): void {
  switch (p.kind) {
    case 'bool':
      model[p.name] = Boolean(entry[p.name] ?? p.default ?? false);
      break;
    case 'number':
      model[p.name] = String(entry[p.name] ?? p.default ?? '');
      break;
    case 'typedInput': {
      const types = typesOf(p);
      model[typeKeyOf(p)] = String(entry[typeKeyOf(p)] ?? types[0] ?? 'str');
      model[p.name] = String(entry[p.name] ?? p.default ?? '');
      break;
    }
    case 'credential':
      model[p.name] = '';
      break;
    case 'list': {
      const initial = Array.isArray(entry[p.name]) ? (entry[p.name] as Model[]) : [];
      model[p.name] = initial.map((v) => initRow(p, v));
      break;
    }
    default:
      model[p.name] = String(entry[p.name] ?? p.default ?? '');
  }
}

/** A row of a list: its own model, filled from the row's values. */
export function initRow(p: PropDef, values: Model): Model {
  const row: Model = {};
  for (const sub of p.fields ?? []) initProp(sub, values, row);
  return row;
}

/** Reads one property back out of its model. */
export function readProp(p: PropDef, model: Model, entry: Model, graph?: Graph): Read {
  const label = p.label ?? p.name;
  switch (p.kind) {
    case 'bool':
      return { values: { [p.name]: Boolean(model[p.name]) } };
    case 'select': {
      const raw = String(model[p.name] ?? '');
      const match = (p.options ?? []).find((o) => String(o.value) === raw);
      return { values: { [p.name]: match ? match.value : raw } };
    }
    case 'number': {
      // A number input answers null when it is empty.
      const v = model[p.name] == null ? '' : String(model[p.name]);
      if (v === '') return p.required ? { error: `${label} is required` } : { values: {} };
      const n = Number(v);
      if (!Number.isFinite(n)) return { error: `${label} must be a number` };
      return { values: { [p.name]: n } };
    }
    case 'text':
    case 'js':
    case 'json':
    case 'jsonata': {
      const v = String(model[p.name] ?? '');
      if (p.kind === 'json' && v.trim() !== '') {
        try {
          JSON.parse(v);
        } catch (ex) {
          return { error: `${label} is not valid JSON: ${(ex as Error).message}` };
        }
      }
      if (p.required && !v.trim()) return { error: `${label} is required` };
      return { values: { [p.name]: v } };
    }
    case 'typedInput': {
      const t = String(model[typeKeyOf(p)] ?? '');
      const v = String(model[p.name] ?? '');
      if (p.required && t !== 'date' && !v) return { error: `${label} is required` };
      return { values: { [p.name]: v, [typeKeyOf(p)]: t } };
    }
    case 'configRef': {
      const v = String(model[p.name] ?? '');
      if (p.required && !v) return { error: `${label} is required` };
      return { values: { [p.name]: v } };
    }
    case 'credential': {
      const creds = (entry.credentials as Record<string, unknown> | undefined) ?? {};
      const hasExisting = creds[p.name] !== undefined;
      const typed = String(model[p.name] ?? '');
      const value = typed === '' && hasExisting ? UNCHANGED : typed;
      return { values: { credentials: { ...creds, [p.name]: value } } };
    }
    case 'list': {
      const out: Model[] = [];
      for (const row of (model[p.name] as Model[]) ?? []) {
        const r: Model = {};
        for (const sub of p.fields ?? []) {
          const outcome = readProp(sub, row, row, graph);
          if (outcome.error) return { error: outcome.error };
          Object.assign(r, outcome.values);
        }
        out.push(r);
      }
      return { values: { [p.name]: out } };
    }
    default: {
      const v = String(model[p.name] ?? '');
      if (p.required && !v) return { error: `${label} is required` };
      return { values: { [p.name]: v } };
    }
  }
}

/** The Types section's model: a JSON Schema in, and one per output port. */
export interface TypesModel { accepts: string; promises: string[]; ports: number }

export function initTypes(entry: FlowEntry, ports: number): TypesModel {
  const show = (v: unknown) => (v === undefined || v === null ? '' : JSON.stringify(v, null, 2));
  const outs = Array.isArray(entry[OUT_KEY]) ? (entry[OUT_KEY] as unknown[]) : [];
  return { accepts: show(entry[IN_KEY]), promises: Array.from({ length: ports }, (_, i) => show(outs[i])), ports };
}

export function readTypes(m: TypesModel): Read {
  const parse = (label: string, text: string): { value?: unknown; error?: string } => {
    const t = text.trim();
    if (t === '') return {};
    try {
      const v = JSON.parse(t) as unknown;
      if (typeof v !== 'object' || v === null || Array.isArray(v)) return { error: `${label} is a JSON Schema: an object` };
      return { value: v };
    } catch (ex) {
      return { error: `${label} is not valid JSON: ${(ex as Error).message}` };
    }
  };
  const accepted = parse('Accepts', m.accepts);
  if (accepted.error) return { error: accepted.error };
  const promised: unknown[] = [];
  for (let i = 0; i < m.promises.length; i++) {
    const r = parse(m.ports === 1 ? 'Promises' : `Promises (output ${i + 1})`, m.promises[i]!);
    if (r.error) return { error: r.error };
    promised.push(r.value ?? null);
  }
  // Only what was written is kept: an empty section takes the key off the node,
  // so a flow that never used types never grows the keys.
  const any = promised.some((v) => v !== null);
  return { values: { [IN_KEY]: accepted.value, [OUT_KEY]: any ? promised : undefined } };
}

/** The keys a raw JSON edit may not touch. */
export const STRUCTURAL = new Set(['id', 'type', 'z', 'g', 'x', 'y', 'wires', 'd']);

export function rawOf(entry: FlowEntry): string {
  const editable: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(entry)) if (!STRUCTURAL.has(k)) editable[k] = v;
  return JSON.stringify(editable, null, 2);
}

export function readRaw(text: string): Read {
  try {
    const parsed = JSON.parse(text) as Record<string, unknown>;
    for (const k of Object.keys(parsed)) {
      if (STRUCTURAL.has(k)) return { error: `${k} is managed by the editor and cannot be set here` };
    }
    return { values: parsed };
  } catch (ex) {
    return { error: `Configuration is not valid JSON: ${(ex as Error).message}` };
  }
}
