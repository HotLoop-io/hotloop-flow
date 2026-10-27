// Finding things across every flow and subflow.
//
// A flow with three hundred nodes on eleven tabs is not searched by looking.
// This matches what somebody remembers about a node: what it is called, what
// type it is, a topic, an address, a line of a function body. The match is
// plain text, case blind, over every setting that holds a string or a number,
// which is how people actually search.

import type { FlowEntry, Graph } from './graph';

export interface Hit {
  id: string;
  /** The flow or subflow it lives on, or "" for configuration shared by all. */
  scope: string;
  scopeLabel: string;
  label: string;
  type: string;
  /** Which setting matched, so a hit on a topic says it was the topic. */
  field: string;
  /** A short piece of the matching text. */
  snippet: string;
  /** Configuration nodes are not on a canvas: the hit opens their dialog instead. */
  config: boolean;
}

/** Settings never searched: structure and layout say nothing a person remembers. */
const SKIP = new Set(['id', 'z', 'g', 'x', 'y', 'w', 'h', 'wires', 'nodes', 'credentials', 'in', 'out', 'style']);

const MAX_HITS = 200;

export function search(graph: Graph, query: string): Hit[] {
  const q = query.trim().toLowerCase();
  if (q === '') return [];

  const hits: Hit[] = [];
  const scopeLabel = (id: string | undefined): string => {
    if (!id) return 'all flows';
    const s = graph.byId(id);
    if (!s) return id;
    return String((s.type === 'subflow' ? s.name : s.label) || (s.type === 'subflow' ? 'subflow' : 'Flow'));
  };

  for (const e of graph.all()) {
    if (hits.length >= MAX_HITS) break;
    const match = firstMatch(graph, e, q);
    if (!match) continue;
    const isScope = e.type === 'tab' || e.type === 'subflow';
    hits.push({
      id: e.id,
      scope: isScope ? e.id : (e.z ?? ''),
      scopeLabel: isScope ? scopeLabel(e.id) : scopeLabel(e.z),
      label: isScope ? scopeLabel(e.id) : graph.label(e),
      type: e.type === 'tab' ? 'flow' : e.type,
      field: match.field,
      snippet: match.snippet,
      config: !isScope && e.type !== 'group' && graph.isConfig(e),
    });
  }
  return hits;
}

function firstMatch(graph: Graph, e: FlowEntry, q: string): { field: string; snippet: string } | null {
  // The things on a node's face first, so the reason shown is the obvious one.
  const label = e.type === 'tab' ? String(e.label ?? '') : graph.label(e);
  if (label.toLowerCase().includes(q)) return { field: 'name', snippet: label };
  if (e.type.toLowerCase().includes(q)) return { field: 'type', snippet: e.type };
  if (e.id.toLowerCase() === q || (q.length >= 6 && e.id.toLowerCase().startsWith(q))) return { field: 'id', snippet: e.id };

  for (const [k, v] of Object.entries(e)) {
    if (SKIP.has(k)) continue;
    const text = scalar(v);
    if (text === null) continue;
    if (text.toLowerCase().includes(q)) return { field: k, snippet: around(text, q) };
  }
  return null;
}

/** A setting's text, looking one level into lists such as a Change node's rules. */
function scalar(v: unknown): string | null {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  if (Array.isArray(v) || (typeof v === 'object' && v !== null)) {
    try {
      return JSON.stringify(v);
    } catch {
      return null;
    }
  }
  return null;
}

function around(text: string, q: string): string {
  const flat = text.replace(/\s+/g, ' ');
  const at = Math.max(0, flat.toLowerCase().indexOf(q));
  const len = q.length;
  const start = Math.max(0, at - 24);
  const end = Math.min(flat.length, at + len + 40);
  return (start > 0 ? '…' : '') + flat.slice(start, end) + (end < flat.length ? '…' : '');
}
