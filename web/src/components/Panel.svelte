<script lang="ts">
  // The panel of a diagram: its controls and indicators in a page an operator can
  // open on a phone while a run is going. It is a client of the same API the
  // editor uses and watches the same event stream, so several people can watch one
  // run and a headless run can have a panel opened on it later.
  import { onMount } from 'svelte';
  import Knob from './Knob.svelte';
  import { type Api, type DiagramDescription, type PanelChart, type PanelTerminal, type RuntimeEvent } from '../lib/api';

  interface Props { api: Api; id: string; onsessionlost: () => void }
  let { api, id, onsessionlost }: Props = $props();

  let desc = $state.raw<DiagramDescription | null>(null);
  let indicators = $state.raw<Record<string, unknown>>({});
  let controls = $state.raw<Record<string, unknown>>({});
  let charts = $state.raw<Record<string, PanelChart>>({});
  // What the run has done to panel objects: visible, enabled, color, label, min, max.
  let attrs = $state.raw<Record<string, Record<string, unknown>>>({});
  let runState = $state('idle');
  let runError = $state('');
  let live = $state<boolean | null>(null);
  let notice = $state('');

  function apply(d: DiagramDescription): void {
    desc = d;
    indicators = { ...d.panel.indicators };
    controls = { ...d.panel.controls };
    charts = { ...d.panel.charts };
    attrs = { ...(d.panel.attributes ?? {}) };
    runState = d.run?.state ?? 'idle';
    runError = d.run?.error ?? '';
  }

  async function load(): Promise<void> {
    try {
      apply(await api.diagram(id));
    } catch (ex) {
      if ((ex as { status?: number }).status === 401) onsessionlost();
      else notice = ex instanceof Error ? ex.message : String(ex);
    }
  }

  function onEvent(e: RuntimeEvent): void {
    if (e.data.diagram !== id) return;
    if (e.topic === `diagram/${id}/run`) {
      runState = String(e.data.state);
      runError = e.data.error ? String(e.data.error) : '';
      if (runState === 'running') void load();
      return;
    }
    if (e.topic !== `diagram/${id}/values`) return;
    const inds = (e.data.indicators ?? {}) as Record<string, unknown>;
    const nextInd = { ...indicators };
    const nextCtl = { ...controls };
    for (const [k, v] of Object.entries(inds)) {
      if (k.startsWith('control:')) nextCtl[k.slice(8)] = v;
      else nextInd[k] = v;
    }
    indicators = nextInd;
    controls = nextCtl;
    const changed = (e.data.attributes ?? {}) as Record<string, Record<string, unknown>>;
    if (Object.keys(changed).length) {
      const next = { ...attrs };
      for (const [k, a] of Object.entries(changed)) next[k] = { ...(next[k] ?? {}), ...a };
      attrs = next;
    }
    const deltas = (e.data.charts ?? {}) as Record<string, { replace?: boolean; t?: number[]; x?: number[]; y: number[]; z?: number[][] }>;
    if (Object.keys(deltas).length) {
      const next = { ...charts };
      for (const [k, d] of Object.entries(deltas)) {
        const cur = next[k] ?? { mode: 'chart', history: 1024, y: [], dropped: 0 };
        const keep = cur.history || 1024;
        if (d.replace) next[k] = { ...cur, t: d.t, x: d.x, y: d.y ?? [], z: d.z };
        else {
          const y = [...cur.y, ...(d.y ?? [])];
          const over = Math.max(0, y.length - keep);
          next[k] = {
            ...cur, y: y.slice(over),
            t: cur.t || d.t ? [...(cur.t ?? []), ...(d.t ?? [])].slice(over) : undefined,
            x: cur.x || d.x ? [...(cur.x ?? []), ...(d.x ?? [])].slice(over) : undefined,
            dropped: cur.dropped + over,
          };
        }
      }
      charts = next;
    }
  }

  onMount(() => {
    void load();
    const stop = api.connectEvents(onEvent, (up) => { live = up; if (up) void load(); });
    return stop;
  });

  const label = (t: PanelTerminal) => String(attrs[t.id]?.label ?? (t.label || t.id));
  const shown = (t: PanelTerminal) => attrs[t.id]?.visible !== false;
  const off = (t: PanelTerminal) => attrs[t.id]?.enabled === false;
  const tint = (t: PanelTerminal) => String(attrs[t.id]?.color ?? 'default');
  const lo = (t: PanelTerminal) => (typeof attrs[t.id]?.min === 'number' ? (attrs[t.id]!.min as number) : t.min);
  const hi = (t: PanelTerminal) => (typeof attrs[t.id]?.max === 'number' ? (attrs[t.id]!.max as number) : t.max);
  const fmt = (t: PanelTerminal, v: unknown): string => {
    if (v === null || v === undefined) return 'waiting';
    if (typeof v === 'number') return v.toFixed(t.digits ?? 2);
    if (typeof v === 'object') return JSON.stringify(v);
    return String(v);
  };

  const zero = (kind: string | undefined): unknown => (kind === 'bool' ? false : kind === 'string' ? '' : 0);
  // The list an array control holds, and the object a record control holds, with
  // every field present, whatever the run or the operator has set so far.
  const items = (t: PanelTerminal): unknown[] => {
    const v = controls[t.id] ?? t.default;
    return Array.isArray(v) ? v : [];
  };
  const record = (t: PanelTerminal): Record<string, unknown> => {
    const v = (controls[t.id] ?? t.default) as Record<string, unknown> | undefined;
    const out: Record<string, unknown> = {};
    for (const f of t.fields ?? []) out[f.name] = v && f.name in v ? v[f.name] : zero(f.kind);
    return out;
  };
  const hasRange = (t: PanelTerminal) => lo(t) !== undefined && hi(t) !== undefined;
  const numberOf = (t: PanelTerminal): number => { const n = Number(controls[t.id] ?? t.default ?? 0); return Number.isFinite(n) ? n : 0; };
  const stepOf = (t: PanelTerminal): number => (typeof t.step === 'number' && t.step > 0 ? t.step : 1);
  /** One step up or down, held to the range, with the float noise a step of 0.1 leaves taken off. */
  const nudge = (t: PanelTerminal, dir: 1 | -1): number => {
    let v = Number((numberOf(t) + dir * stepOf(t)).toFixed(10));
    const a = lo(t), b = hi(t);
    if (a !== undefined) v = Math.max(a, v);
    if (b !== undefined) v = Math.min(b, v);
    return v;
  };

  async function set(t: PanelTerminal, value: unknown): Promise<void> {
    try {
      const res = await api.setControl(id, t.id, value);
      controls = { ...controls, [t.id]: res.value };
      notice = '';
    } catch (ex) {
      notice = `${label(t)}: ${ex instanceof Error ? ex.message : ex}`;
    }
  }

  async function run(): Promise<void> { try { await api.runDiagram(id); notice = ''; } catch (ex) { notice = ex instanceof Error ? ex.message : String(ex); } }
  async function stop(): Promise<void> { try { await api.stopDiagram(id); } catch (ex) { notice = ex instanceof Error ? ex.message : String(ex); } }

  function points(c: PanelChart | undefined, w: number, h: number): string {
    if (!c || c.y.length === 0) return '';
    const xs = c.mode === 'chart' ? c.y.map((_, i) => i) : (c.x && c.x.length === c.y.length ? c.x : c.y.map((_, i) => i));
    const x0 = Math.min(...xs), x1 = Math.max(...xs);
    const y0 = Math.min(...c.y), y1 = Math.max(...c.y);
    const sx = x1 === x0 ? 1 : w / (x1 - x0);
    const sy = y1 === y0 ? 1 : (h - 16) / (y1 - y0);
    return c.y.map((y, i) => `${((xs[i]! - x0) * sx).toFixed(1)},${(h - 8 - (y - y0) * sy).toFixed(1)}`).join(' ');
  }
  // An intensity chart: each value of the grid as a color from the smallest to the largest.
  function ramp(v: number): [number, number, number] {
    const stops: [number, number, number][] = [[30, 60, 140], [30, 160, 190], [240, 220, 80], [255, 90, 0]];
    const pos = Math.min(1, Math.max(0, v)) * (stops.length - 1);
    const i = Math.min(stops.length - 2, Math.floor(pos));
    const f = pos - i;
    const a = stops[i]!, b = stops[i + 1]!;
    return [a[0] + (b[0] - a[0]) * f, a[1] + (b[1] - a[1]) * f, a[2] + (b[2] - a[2]) * f];
  }
  function paintGrid(node: HTMLCanvasElement, z: number[][] | undefined) {
    const draw = (grid: number[][] | undefined) => {
      const rows = grid?.length ?? 0, cols = rows ? grid![0]!.length : 0;
      node.width = Math.max(1, cols);
      node.height = Math.max(1, rows);
      const ctx = node.getContext('2d');
      if (!ctx || !rows) return;
      let lo = Infinity, hi = -Infinity;
      for (const r of grid!) for (const v of r) { if (v < lo) lo = v; if (v > hi) hi = v; }
      const img = ctx.createImageData(cols, rows);
      grid!.forEach((r, y) => r.forEach((v, x) => {
        const [R, G, B] = ramp(hi === lo ? 0.5 : (v - lo) / (hi - lo));
        const o = (y * cols + x) * 4;
        img.data[o] = R; img.data[o + 1] = G; img.data[o + 2] = B; img.data[o + 3] = 255;
      }));
      ctx.putImageData(img, 0, 0);
    };
    draw(z);
    return { update: draw };
  }
  const gridRange = (c?: PanelChart): string => {
    if (!c?.z?.length) return 'no data yet';
    let lo = Infinity, hi = -Infinity;
    for (const r of c.z) for (const v of r) { if (v < lo) lo = v; if (v > hi) hi = v; }
    return `${c.z.length} by ${c.z[0]!.length}, ${lo.toFixed(2)} to ${hi.toFixed(2)}`;
  };
  const range = (c?: PanelChart): string => (c && c.y.length ? `${Math.min(...c.y).toFixed(2)} to ${Math.max(...c.y).toFixed(2)}` : 'no data yet');
</script>

<div class="panel-page" data-test="panel">
  <div class="panel-head">
    <h1 data-test="panel-title">{desc?.label ?? 'Panel'}</h1>
    <span class="run-state mono" data-test="panel-state" data-state={runState}>{runState}</span>
    <span class="conn mono"><span class="dot {live === null ? 'grey' : live ? 'green' : 'red'}"></span> {live === null ? 'connecting' : live ? 'live' : 'reconnecting'}</span>
    <div class="spacer" style="flex:1"></div>
    <button class="primary" data-test="panel-run" disabled={runState === 'running' || runState === 'paused'} onclick={run}>Run</button>
    <button class="ghost" data-test="panel-stop" disabled={!(runState === 'running' || runState === 'paused')} onclick={stop}>Stop</button>
  </div>
  {#if runError}<div class="diagram-error" style="position:static;margin:12px 16px 0" data-test="panel-run-error">{runError}</div>{/if}
  {#if notice}<div class="diagram-error" style="position:static;margin:12px 16px 0" data-test="panel-notice">{notice}</div>{/if}
  <div class="panel-grid">
    {#each desc?.terminals ?? [] as t (t.id)}
      {#if t.type === 'dia-indicator'}
        <section class="panel-card tint-{tint(t)}" hidden={!shown(t)} data-test="ind" data-id={t.id} data-color={tint(t)}>
          <div class="cap">{label(t)}</div>
          {#if t.kind === 'bool'}
            <span class="led" class:on={indicators[t.id] === true} data-on={indicators[t.id] === true} aria-label={label(t)}></span>
          {:else if t.kind === 'error'}
            {@const e = indicators[t.id] as { status?: boolean; code?: number; source?: string } | undefined}
            <span class="led" class:bad={e?.status === true} class:on={e !== undefined && e.status !== true}></span>
            <div class:err-text={e?.status === true} data-test="ind-value">{e?.status ? `${e.source} (code ${e.code})` : 'no error'}</div>
          {:else if t.kind === 'number'}
            <div class="big-value" data-test="ind-value">{fmt(t, indicators[t.id])}{#if t.unit}<span class="unit">{t.unit}</span>{/if}</div>
          {:else}
            <div data-test="ind-value" style="font-size:var(--hl-fs-md)">{fmt(t, indicators[t.id])}{t.unit ? ` ${t.unit}` : ''}</div>
          {/if}
        </section>
      {:else if t.type === 'dia-chart'}
        <section class="panel-card wide tint-{tint(t)}" hidden={!shown(t)} data-test="chart" data-id={t.id}>
          <div class="cap">{label(t)} <span style="text-transform:none">({charts[t.id]?.mode ?? t.mode ?? 'chart'})</span></div>
          {#if (charts[t.id]?.mode ?? t.mode) === 'intensity'}
            <canvas class="chart-grid" data-test="chart-grid" aria-label={`${label(t)} intensity chart`} use:paintGrid={charts[t.id]?.z}></canvas>
            <div class="chart-note"><span data-test="chart-count">{gridRange(charts[t.id])}</span><span>{t.unit ?? ''}</span></div>
          {:else}
            <svg class="chart-svg" viewBox="0 0 600 180" preserveAspectRatio="none" role="img" aria-label={`${label(t)} chart`}>
            <line class="axis" x1="0" y1="172" x2="600" y2="172" />
            <polyline data-test="chart-line" points={points(charts[t.id], 600, 180)} />
            </svg>
            <div class="chart-note"><span data-test="chart-count">{charts[t.id]?.y.length ?? 0} of {t.history ?? 1024} points</span><span>{range(charts[t.id])}{t.unit ? ` ${t.unit}` : ''}</span></div>
          {/if}
        </section>
      {:else if t.type === 'dia-control'}
        <section class="panel-card tint-{tint(t)}" hidden={!shown(t)} data-test="ctl" data-id={t.id} data-enabled={!off(t)}>
          <div class="cap">{label(t)}</div>
          {#if t.kind === 'bool'}
            {#if t.style === 'button'}
              <button class="primary" data-test="ctl-button" disabled={off(t)} onclick={() => set(t, true)}>{label(t)}</button>
            {:else if t.style === 'led'}
              <button type="button" class="led-toggle" role="switch" aria-checked={controls[t.id] === true} aria-label={label(t)} data-test="ctl-led" data-on={controls[t.id] === true} disabled={off(t)} onclick={() => set(t, controls[t.id] !== true)}>
                <span class="led" class:on={controls[t.id] === true}></span>{controls[t.id] === true ? 'on' : 'off'}
              </button>
            {:else}
              <label class="switch"><input type="checkbox" data-test="ctl-switch" disabled={off(t)} checked={controls[t.id] === true} onchange={(e) => set(t, (e.currentTarget as HTMLInputElement).checked)} /> {controls[t.id] === true ? 'on' : 'off'}</label>
            {/if}
          {:else if t.kind === 'enum'}
            <select data-test="ctl-enum" disabled={off(t)} value={String(controls[t.id] ?? t.default ?? '')} onchange={(e) => set(t, (e.currentTarget as HTMLSelectElement).value)}>
              {#each t.options ?? [] as o}<option value={o}>{o}</option>{/each}
            </select>
          {:else if t.kind === 'string'}
            <input type="text" data-test="ctl-text" disabled={off(t)} value={String(controls[t.id] ?? t.default ?? '')} onchange={(e) => set(t, (e.currentTarget as HTMLInputElement).value)} />
          {:else if t.kind === 'array'}
            <div class="compound" data-test="ctl-array" data-id={t.id} data-count={items(t).length}>
              {#each items(t) as item, i (i)}
                <div class="item-row" data-test="ctl-array-item">
                  {#if t.elem === 'bool'}
                    <label class="switch"><input type="checkbox" data-test="ctl-array-value" aria-label={`${label(t)} item ${i + 1}`} disabled={off(t)} checked={item === true} onchange={(e) => set(t, items(t).map((x, j) => (j === i ? (e.currentTarget as HTMLInputElement).checked : x)))} /> {item === true ? 'on' : 'off'}</label>
                  {:else if t.elem === 'string'}
                    <input type="text" data-test="ctl-array-value" aria-label={`${label(t)} item ${i + 1}`} disabled={off(t)} value={String(item ?? '')} onchange={(e) => set(t, items(t).map((x, j) => (j === i ? (e.currentTarget as HTMLInputElement).value : x)))} />
                  {:else}
                    <input type="number" data-test="ctl-array-value" aria-label={`${label(t)} item ${i + 1}`} disabled={off(t)} min={lo(t)} max={hi(t)} step={t.step ?? 'any'} value={Number(item ?? 0)} onchange={(e) => set(t, items(t).map((x, j) => (j === i ? Number((e.currentTarget as HTMLInputElement).value) : x)))} />
                  {/if}
                  <button type="button" class="ghost" data-test="ctl-array-remove" aria-label={`Remove item ${i + 1} of ${label(t)}`} disabled={off(t)} onclick={() => set(t, items(t).filter((_, j) => j !== i))}>Remove</button>
                </div>
              {/each}
              {#if items(t).length === 0}<div class="mono hint" data-test="ctl-array-empty">no items</div>{/if}
              <button type="button" data-test="ctl-array-add" disabled={off(t) || (typeof t.maxlen === 'number' && t.maxlen > 0 && items(t).length >= t.maxlen)} onclick={() => set(t, [...items(t), zero(t.elem)])}>Add an item</button>
            </div>
          {:else if t.kind === 'record'}
            <div class="compound" data-test="ctl-record" data-id={t.id}>
              {#each t.fields ?? [] as f (f.name)}
                <label class="field-row" data-test="ctl-field" data-field={f.name}>
                  <span class="mono">{f.name}</span>
                  {#if f.kind === 'bool'}
                    <input type="checkbox" data-test="ctl-field-value" disabled={off(t)} checked={record(t)[f.name] === true} onchange={(e) => set(t, { ...record(t), [f.name]: (e.currentTarget as HTMLInputElement).checked })} />
                  {:else if f.kind === 'string'}
                    <input type="text" data-test="ctl-field-value" disabled={off(t)} value={String(record(t)[f.name] ?? '')} onchange={(e) => set(t, { ...record(t), [f.name]: (e.currentTarget as HTMLInputElement).value })} />
                  {:else}
                    <input type="number" step="any" data-test="ctl-field-value" disabled={off(t)} value={Number(record(t)[f.name] ?? 0)} onchange={(e) => set(t, { ...record(t), [f.name]: Number((e.currentTarget as HTMLInputElement).value) })} />
                  {/if}
                </label>
              {/each}
            </div>
          {:else if t.style === 'knob' && hasRange(t)}
            <Knob value={numberOf(t)} min={lo(t)!} max={hi(t)!} step={t.step} unit={t.unit} label={label(t)} disabled={off(t)} oncommit={(v) => set(t, v)} />
          {:else if t.style === 'slider' && hasRange(t)}
            <input type="range" data-test="ctl-slider" disabled={off(t)} min={lo(t)} max={hi(t)} step={t.step ?? 'any'} value={Number(controls[t.id] ?? t.default ?? 0)} onchange={(e) => set(t, Number((e.currentTarget as HTMLInputElement).value))} />
            <div class="mono">{controls[t.id] ?? t.default}</div>
          {:else if t.style === 'spin'}
            <div class="spin" data-test="ctl-spin">
              <button type="button" data-test="ctl-spin-down" aria-label={`Lower ${label(t)}`} disabled={off(t) || (lo(t) !== undefined && numberOf(t) <= lo(t)!)} onclick={() => set(t, nudge(t, -1))}>-</button>
              <input type="number" data-test="ctl-spin-input" aria-label={label(t)} disabled={off(t)} min={lo(t)} max={hi(t)} step={t.step ?? 'any'} value={numberOf(t)} onchange={(e) => set(t, Number((e.currentTarget as HTMLInputElement).value))} />
              <button type="button" data-test="ctl-spin-up" aria-label={`Raise ${label(t)}`} disabled={off(t) || (hi(t) !== undefined && numberOf(t) >= hi(t)!)} onclick={() => set(t, nudge(t, 1))}>+</button>
            </div>
          {:else}
            <input type="number" data-test="ctl-number" disabled={off(t)} min={lo(t)} max={hi(t)} step={t.step ?? 'any'} value={Number(controls[t.id] ?? t.default ?? 0)} onchange={(e) => set(t, Number((e.currentTarget as HTMLInputElement).value))} />
          {/if}
        </section>
      {/if}
    {/each}
  </div>
</div>
