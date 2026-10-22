// Typed wires, the editor's half.
//
// A node can declare what it accepts (ew_inSchema) and what each output port
// promises (ew_outSchemas), as JSON Schema over the whole message. This file
// reads those two keys and says how to write a schema down in a few characters
// for a wire. Whether two ends of a wire can ever agree is not decided here: the
// runtime decides that, in Go, and the editor asks it (POST /flows/types), so
// what the canvas shows and what a deploy refuses are the same answer.

import type { FlowEntry } from './graph';

export const IN_KEY = 'ew_inSchema';
export const OUT_KEY = 'ew_outSchemas';

/** One thing the runtime found wrong with a typed wire or a schema. */
export interface WireProblem {
  nodeId: string;
  name?: string;
  type: string;
  /** 1-based output port the wire leaves from; 0 when the problem is the node's own schema. */
  port?: number;
  to?: string;
  toName?: string;
  message: string;
}

type Schema = Record<string, unknown>;

const isSchema = (v: unknown): v is Schema => typeof v === 'object' && v !== null && !Array.isArray(v);

/** What a node accepts, or undefined. */
export function inSchema(e: FlowEntry): Schema | undefined {
  const v = e[IN_KEY];
  return isSchema(v) ? v : undefined;
}

/** What a node promises on one output port, or undefined. */
export function outSchema(e: FlowEntry, port: number): Schema | undefined {
  const list = e[OUT_KEY];
  if (!Array.isArray(list)) return undefined;
  const v = list[port];
  return isSchema(v) ? v : undefined;
}

/**
 * A schema in a few characters: "number", "string|null", "{temp: number}",
 * "[string]". A schema over the whole message reads as its payload, because that
 * is the part people mean: "payload {temp: number}".
 */
export function summarize(schema: Schema, whole = true): string {
  if (whole) {
    const props = schema.properties;
    if (isSchema(props) && isSchema(props.payload)) return `payload ${summarize(props.payload, false)}`;
    if (isSchema(props) && Object.keys(props).length > 0) return summarize(schema, false);
  }
  if ('const' in schema) return JSON.stringify(schema.const);
  if (Array.isArray(schema.enum)) return (schema.enum as unknown[]).map((v) => JSON.stringify(v)).join('|');
  const t = schema.type;
  const types = typeof t === 'string' ? [t] : Array.isArray(t) ? (t as string[]) : [];
  if (isSchema(schema.properties) && (types.length === 0 || types.includes('object'))) {
    const parts = Object.entries(schema.properties).map(([k, v]) => `${k}: ${isSchema(v) ? summarize(v, false) : 'any'}`);
    const shown = parts.slice(0, 3).join(', ');
    return `{${shown}${parts.length > 3 ? ', …' : ''}}`;
  }
  if (isSchema(schema.items) && (types.length === 0 || types.includes('array'))) return `[${summarize(schema.items, false)}]`;
  if (types.length > 0) return types.join('|');
  return 'any';
}

export type WireTypeState =
  /** Both ends declare a type and they can agree. */
  | 'ok'
  /** Only one end declares one. */
  | 'half'
  /** Both declare one and they can never agree: a deploy would refuse it. */
  | 'clash';

export interface WireTypeLabel {
  state: WireTypeState;
  /** What is drawn on the wire. */
  text: string;
  /** The full story, for the tooltip. */
  title: string;
}

/** The key a wire's clash is filed under. */
export const clashKey = (from: string, port: number, to: string): string => `${from}:${port}:${to}`;

/**
 * What to draw on one wire, or null for a wire with no types on it. `clashes`
 * maps clashKey to the runtime's messages for that wire.
 */
export function wireTypeLabel(
  from: FlowEntry, port: number, to: FlowEntry, clashes: Map<string, string[]>,
): WireTypeLabel | null {
  const out = outSchema(from, port);
  const inn = inSchema(to);
  if (!out && !inn) return null;

  const sends = out ? summarize(out) : undefined;
  const takes = inn ? summarize(inn) : undefined;
  const lines = [
    `${from.name || from.type} output ${port + 1} to ${to.name || to.type}`,
    sends !== undefined ? `sends: ${sends}` : 'sends: nothing declared',
    takes !== undefined ? `takes: ${takes}` : 'takes: nothing declared',
  ];

  const found = clashes.get(clashKey(from.id, port + 1, to.id));
  if (found && found.length > 0) {
    // "payload.temp: the sender sends string and ..." leads with the field.
    const field = found[0]!.split(':')[0] ?? '';
    return { state: 'clash', text: `≠ ${field}`, title: [...lines, '', 'A deploy would refuse this wire:', ...found].join('\n') };
  }
  if (out && inn) return { state: 'ok', text: sends!, title: lines.join('\n') };
  return { state: 'half', text: sends ?? takes!, title: lines.join('\n') };
}

/** Groups the runtime's problems by the wire they are about. */
export function clashesByWire(problems: WireProblem[]): Map<string, string[]> {
  const by = new Map<string, string[]>();
  for (const p of problems) {
    if (!p.to || !p.port) continue;
    const key = clashKey(p.nodeId, p.port, p.to);
    const list = by.get(key) ?? [];
    list.push(p.message);
    by.set(key, list);
  }
  return by;
}
