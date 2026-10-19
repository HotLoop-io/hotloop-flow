// Dialogs for the two things that are not nodes: subflows and groups.
//
// A subflow has two faces. The template is edited on a canvas of its own, and
// its properties here: name, colour, how many ports, and the parameters an
// instance can set. An instance on a flow is edited here too: a name, and a
// value for each parameter the template declares.

import type { FlowEntry, Graph } from './graph';
import { footButton, shell } from './modal';

const ENV_TYPES: [string, string][] = [
  ['str', 'string'], ['num', 'number'], ['bool', 'boolean'], ['json', 'JSON'], ['env', 'env variable'],
];

interface EnvVar { name: string; type: string; value: string }

function el<K extends keyof HTMLElementTagNameMap>(tag: K, attrs: Record<string, string> = {}, ...kids: (Node | string)[]): HTMLElementTagNameMap[K] {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') n.className = v; else n.setAttribute(k, v);
  }
  n.append(...kids);
  return n;
}

function field(label: string, control: HTMLElement, help?: string): HTMLElement {
  const wrap = el('div', { class: 'field' }, el('label', {}, label), control);
  if (help) wrap.append(el('div', { class: 'field-help' }, help));
  return wrap;
}

function typeSelect(current: string): HTMLSelectElement {
  const s = el('select');
  for (const [v, label] of ENV_TYPES) {
    const o = el('option', { value: v }, label);
    if (v === current) o.selected = true;
    s.append(o);
  }
  return s;
}

function envOf(entry: FlowEntry, key = 'env'): EnvVar[] {
  const raw = entry[key];
  if (!Array.isArray(raw)) return [];
  return (raw as Record<string, unknown>[]).map((v) => ({
    name: String(v.name ?? ''),
    type: String(v.type ?? 'str'),
    value: v.value === undefined || v.value === null ? '' : String(v.value),
  }));
}

export type InstanceOutcome = { kind: 'save'; name: string; env: EnvVar[] } | { kind: 'edit' } | null;

/** An instance on a flow: its name, and a value for each parameter its template has. */
export function instanceDialog(entry: FlowEntry, template: FlowEntry): Promise<InstanceOutcome> {
  return new Promise((resolve) => {
    let outcome: InstanceOutcome = null;
    const dlg = shell(`Subflow: ${String(template.name || 'subflow')}`, () => resolve(outcome));

    const name = el('input', { type: 'text', 'data-test': 'instance-name', placeholder: String(template.name || 'subflow') });
    name.value = String(entry.name ?? '');
    dlg.body.append(field('Name', name));

    const declared = envOf(template);
    const mine = new Map(envOf(entry).map((v) => [v.name, v]));
    const rows: { decl: EnvVar; type: HTMLSelectElement; value: HTMLInputElement }[] = [];

    if (declared.length > 0) {
      dlg.body.append(el('div', { class: 'field-help' }, 'Properties this subflow takes. Each instance can set its own.'));
    }
    for (const decl of declared) {
      const set = mine.get(decl.name);
      const type = typeSelect(set?.type ?? decl.type);
      const value = el('input', { type: 'text', 'data-test': `instance-env-${decl.name}` });
      value.value = set?.value ?? decl.value;
      type.dataset.test = `instance-env-type-${decl.name}`;
      rows.push({ decl, type, value });
      dlg.body.append(field(decl.name, el('div', { class: 'typed-input' }, type, value)));
    }

    const info = typeof template.info === 'string' && template.info ? template.info : '';
    if (info) dlg.body.append(el('div', { class: 'field-help' }, info));

    const done = footButton('Done', 'instance-done', () => {
      const env: EnvVar[] = [];
      for (const r of rows) {
        const was = mine.get(r.decl.name);
        const changed = r.value.value !== r.decl.value || r.type.value !== r.decl.type;
        if (was || changed) env.push({ name: r.decl.name, type: r.type.value, value: r.value.value });
      }
      outcome = { kind: 'save', name: name.value, env };
      dlg.close();
    });
    const edit = footButton('Edit subflow', 'instance-edit', () => {
      outcome = { kind: 'edit' };
      dlg.close();
    }, true);
    dlg.foot.append(edit, footButton('Cancel', 'instance-cancel', () => dlg.close(), true), done);
    name.focus();
  });
}

export type PropertiesOutcome = 'saved' | 'deleted' | null;

/** A template's own properties. */
export function subflowDialog(graph: Graph, template: FlowEntry): Promise<PropertiesOutcome> {
  return new Promise((resolve) => {
    let outcome: PropertiesOutcome = null;
    const dlg = shell('Subflow properties', () => resolve(outcome));

    const name = el('input', { type: 'text', 'data-test': 'subflow-name' });
    name.value = String(template.name ?? '');
    const category = el('input', { type: 'text', 'data-test': 'subflow-category' });
    category.value = String(template.category ?? '');
    const info = el('textarea', { 'data-test': 'subflow-info', rows: '3' });
    info.value = String(template.info ?? '');
    const color = el('input', { type: 'text', 'data-test': 'subflow-color', placeholder: '#DDAA99' });
    color.value = String(template.color ?? '');

    const inputs = el('select', { 'data-test': 'subflow-inputs' }, el('option', { value: '0' }, 'none'), el('option', { value: '1' }, 'one'));
    inputs.value = String(Array.isArray(template.in) ? Math.min(1, (template.in as unknown[]).length) : 0);
    const outputs = el('input', { type: 'number', min: '0', max: '20', 'data-test': 'subflow-outputs' });
    outputs.value = String(Array.isArray(template.out) ? (template.out as unknown[]).length : 0);

    dlg.body.append(
      field('Name', name),
      field('Category', category, 'Groups it in the palette.'),
      field('Colour', color, 'A hex colour for the node. Blank for the default.'),
      field('Inputs', inputs),
      field('Outputs', outputs, 'Instances on flows follow when this changes.'),
      field('Description', info),
    );

    const rowsHost = el('div', { class: 'list-rows', 'data-test': 'subflow-env' });
    const rows: { name: HTMLInputElement; type: HTMLSelectElement; value: HTMLInputElement; row: HTMLElement }[] = [];
    const addRow = (v: EnvVar) => {
      const n = el('input', { type: 'text', placeholder: 'name', 'data-test': 'subflow-env-name' });
      n.value = v.name;
      const t = typeSelect(v.type);
      const val = el('input', { type: 'text', placeholder: 'default', 'data-test': 'subflow-env-value' });
      val.value = v.value;
      const remove = el('button', { class: 'icon', type: 'button', title: 'Remove' }, '✕');
      const row = el('div', { class: 'list-row' }, el('div', { class: 'list-row-fields' }, n, el('div', { class: 'typed-input' }, t, val)), remove);
      remove.onclick = () => {
        rows.splice(rows.findIndex((r) => r.row === row), 1);
        row.remove();
      };
      rows.push({ name: n, type: t, value: val, row });
      rowsHost.append(row);
    };
    for (const v of envOf(template)) addRow(v);
    const add = el('button', { class: 'ghost', type: 'button', 'data-test': 'subflow-env-add' }, 'Add property');
    add.onclick = () => addRow({ name: '', type: 'str', value: '' });
    dlg.body.append(field('Properties', el('div', {}, rowsHost, add), 'Each instance can set a value for these.'));

    const err = el('div', { class: 'dialog-error', 'data-test': 'subflow-error' });
    dlg.body.append(err);

    const save = footButton('Done', 'subflow-done', () => {
      const outs = Number(outputs.value);
      if (!Number.isInteger(outs) || outs < 0 || outs > 20) { err.textContent = 'Outputs has to be a whole number from 0 to 20.'; return; }
      if (color.value !== '' && !/^#[0-9a-fA-F]{3,8}$/.test(color.value)) { err.textContent = 'Colour has to be a hex colour like #DDAA99.'; return; }
      const names = rows.map((r) => r.name.value.trim()).filter((n) => n !== '');
      if (new Set(names).size !== names.length) { err.textContent = 'Two properties have the same name.'; return; }
      graph.updateSubflow(template.id, {
        name: name.value,
        info: info.value,
        category: category.value,
        color: color.value || '#DDAA99',
        inputs: Number(inputs.value),
        outputs: outs,
        env: rows.map((r) => ({ name: r.name.value.trim(), type: r.type.value, value: r.value.value })),
      });
      outcome = 'saved';
      dlg.close();
    });
    const del = footButton('Delete subflow', 'subflow-delete', () => {
      const problem = graph.deleteSubflow(template.id);
      if (problem) { err.textContent = problem; return; }
      outcome = 'deleted';
      dlg.close();
    }, true);
    dlg.foot.append(del, footButton('Cancel', 'subflow-cancel', () => dlg.close(), true), save);
    name.focus();
  });
}

/** A group's name and how it is drawn. */
export function groupDialog(graph: Graph, group: FlowEntry): Promise<boolean> {
  return new Promise((resolve) => {
    let saved = false;
    const dlg = shell('Group', () => resolve(saved));
    const style = (group.style as Record<string, unknown> | undefined) ?? {};

    const name = el('input', { type: 'text', 'data-test': 'group-name' });
    name.value = String(group.name ?? '');
    const label = el('input', { type: 'checkbox', 'data-test': 'group-label' });
    label.checked = style.label !== false;
    const fill = el('input', { type: 'text', 'data-test': 'group-fill', placeholder: 'default' });
    fill.value = String(style.fill ?? '');
    const stroke = el('input', { type: 'text', 'data-test': 'group-stroke', placeholder: 'default' });
    stroke.value = String(style.stroke ?? '');
    const err = el('div', { class: 'dialog-error', 'data-test': 'group-error' });

    dlg.body.append(
      field('Name', name),
      el('div', { class: 'field field-inline' }, el('label', {}, label, ' Show the name on the canvas')),
      field('Fill', fill, 'A hex colour. Blank for the default.'),
      field('Border', stroke, 'A hex colour. Blank for the default.'),
      err,
    );

    const hex = /^#[0-9a-fA-F]{3,8}$/;
    dlg.foot.append(
      footButton('Cancel', 'group-cancel', () => dlg.close(), true),
      footButton('Done', 'group-done', () => {
        for (const [what, v] of [['Fill', fill.value], ['Border', stroke.value]] as const) {
          if (v !== '' && !hex.test(v)) { err.textContent = `${what} has to be a hex colour like #3366cc.`; return; }
        }
        graph.updateGroup(group.id, { name: name.value, fill: fill.value, stroke: stroke.value, label: label.checked });
        saved = true;
        dlg.close();
      }),
    );
    name.focus();
  });
}
