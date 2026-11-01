<script lang="ts">
  // The panel of a diagram: its controls and indicators in a page an operator can
  // open on a phone while a run is going. It is a client of the same API the
  // editor uses and watches the same event stream, so several people can watch one
  // run and a headless run can have a panel opened on it later.
  import { onMount } from 'svelte';
  import { type Api, type DiagramDescription, type PanelChart, type PanelTerminal, type RuntimeEvent } from '../lib/api';

  interface Props { api: Api; id: string; onsessionlost: () => void }
  let { api, id, onsessionlost }: Props = $props();

  let desc = $state.raw<DiagramDescription | null>(null);
  let indicators = $state.raw<Record<string, unknown>>({});
  let controls = $state.raw<Record<string, unknown>>({});
  let charts = $state.raw<Record<string, PanelChart>>({});
  let runState = $state('idle');
  let runError = $state('');
  let live = $state<boolean | null>(null);
  let notice = $state('');

  function apply(d: DiagramDescription): void {
    desc = d;
    indicators = { ...d.panel.indicators };
    controls = { ...d.panel.controls };
    charts = { ...d.panel.charts };
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
    const deltas = (e.data.charts ?? {}) as Record<string, { replace?: boolean; t?: number[]; x?: number[]; y: number[] }>;
    if (Object.keys(deltas).length) {
      const next = { ...charts };
      for (const [k, d] of Object.entries(deltas)) {
        const cur = next[k] ?? { mode: 'chart', history: 1024, y: [], dropped: 0 };
        const keep = cur.history || 1024;
        if (d.replace) next[k] = { ...cur, t: d.t, x: d.x, y: d.y ?? [] };
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

  const label = (t: PanelTerminal) => t.label || t.id;
  const fmt = (t: PanelTerminal, v: unknown): string => {
    if (v === null || v === undefined) return 'waiting';
    if (typeof v === 'number') return v.toFixed(t.digits ?? 2);
    if (typeof v === 'object') return JSON.stringify(v);
    return String(v);
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
        <section class="panel-card" data-test="ind" data-id={t.id}>
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
        <section class="panel-card wide" data-test="chart" data-id={t.id}>
          <div class="cap">{label(t)} <span style="text-transform:none">({charts[t.id]?.mode ?? t.mode ?? 'chart'})</span></div>
          <svg class="chart-svg" viewBox="0 0 600 180" preserveAspectRatio="none" role="img" aria-label={`${label(t)} chart`}>
            <line class="axis" x1="0" y1="172" x2="600" y2="172" />
            <polyline data-test="chart-line" points={points(charts[t.id], 600, 180)} />
          </svg>
          <div class="chart-note"><span data-test="chart-count">{charts[t.id]?.y.length ?? 0} of {t.history ?? 1024} points</span><span>{range(charts[t.id])}{t.unit ? ` ${t.unit}` : ''}</span></div>
        </section>
      {:else if t.type === 'dia-control'}
        <section class="panel-card" data-test="ctl" data-id={t.id}>
          <div class="cap">{label(t)}</div>
          {#if t.kind === 'bool'}
            {#if t.style === 'button'}
              <button class="primary" data-test="ctl-button" onclick={() => set(t, true)}>{label(t)}</button>
            {:else}
              <label class="switch"><input type="checkbox" data-test="ctl-switch" checked={controls[t.id] === true} onchange={(e) => set(t, (e.currentTarget as HTMLInputElement).checked)} /> {controls[t.id] === true ? 'on' : 'off'}</label>
            {/if}
          {:else if t.kind === 'enum'}
            <select data-test="ctl-enum" value={String(controls[t.id] ?? t.default ?? '')} onchange={(e) => set(t, (e.currentTarget as HTMLSelectElement).value)}>
              {#each t.options ?? [] as o}<option value={o}>{o}</option>{/each}
            </select>
          {:else if t.kind === 'string'}
            <input type="text" data-test="ctl-text" value={String(controls[t.id] ?? t.default ?? '')} onchange={(e) => set(t, (e.currentTarget as HTMLInputElement).value)} />
          {:else if t.style === 'slider' && t.min !== undefined && t.max !== undefined}
            <input type="range" data-test="ctl-slider" min={t.min} max={t.max} step={t.step ?? 'any'} value={Number(controls[t.id] ?? t.default ?? 0)} onchange={(e) => set(t, Number((e.currentTarget as HTMLInputElement).value))} />
            <div class="mono">{controls[t.id] ?? t.default}</div>
          {:else}
            <input type="number" data-test="ctl-number" min={t.min} max={t.max} step={t.step ?? 'any'} value={Number(controls[t.id] ?? t.default ?? 0)} onchange={(e) => set(t, Number((e.currentTarget as HTMLInputElement).value))} />
          {/if}
        </section>
      {/if}
    {/each}
  </div>
</div>
