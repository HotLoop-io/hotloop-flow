<script lang="ts">
  // A rotary control for the panel: a dial that sweeps 270 degrees from the lower
  // left to the lower right, turned by dragging round it with a finger or a mouse,
  // or from the keyboard. It writes when the operator lets go, or on each key.
  interface Props {
    value: number;
    min: number;
    max: number;
    step?: number | undefined;
    disabled?: boolean;
    label: string;
    unit?: string;
    oncommit: (v: number) => void;
  }
  let { value, min, max, step, disabled = false, label, unit = '', oncommit }: Props = $props();

  const START = 135;
  const SWEEP = 270;
  // While a finger is down the dial follows it and nothing is sent; the value it ends on is.
  let live = $state<number | null>(null);
  let dial: SVGSVGElement | undefined = $state();

  const span = $derived(max - min);
  const shown = $derived(clamp(live ?? value));
  const frac = $derived(span > 0 ? (shown - min) / span : 0);
  const hand = $derived(polar(START + SWEEP * frac, 28).split(' '));
  const decimals = $derived(step && step < 1 ? Math.min(6, Math.ceil(-Math.log10(step))) : 0);

  function clamp(v: number): number {
    if (!Number.isFinite(v)) return min;
    return Math.min(max, Math.max(min, v));
  }
  function snapTo(v: number): number {
    if (step && step > 0) v = min + Math.round((v - min) / step) * step;
    return Number(clamp(v).toFixed(10));
  }
  function polar(deg: number, r: number): string {
    const a = (deg * Math.PI) / 180;
    return `${(50 + r * Math.cos(a)).toFixed(2)} ${(50 + r * Math.sin(a)).toFixed(2)}`;
  }
  function arc(from: number, to: number, r: number): string {
    if (to - from < 0.01) return '';
    return `M ${polar(from, r)} A ${r} ${r} 0 ${to - from > 180 ? 1 : 0} 1 ${polar(to, r)}`;
  }
  function fromPointer(e: PointerEvent): number {
    const box = dial!.getBoundingClientRect();
    const dx = e.clientX - (box.left + box.width / 2);
    const dy = e.clientY - (box.top + box.height / 2);
    let rel = (Math.atan2(dy, dx) * 180) / Math.PI - START;
    rel = ((rel % 360) + 360) % 360;
    // The gap at the bottom belongs to whichever end of the dial is nearer.
    if (rel > SWEEP) rel = rel - SWEEP < (360 - SWEEP) / 2 ? SWEEP : 0;
    return snapTo(min + (rel / SWEEP) * span);
  }

  function down(e: PointerEvent): void {
    if (disabled) return;
    e.preventDefault();
    dial!.setPointerCapture(e.pointerId);
    dial!.focus();
    live = fromPointer(e);
  }
  function move(e: PointerEvent): void {
    if (live === null) return;
    live = fromPointer(e);
  }
  function up(e: PointerEvent): void {
    if (live === null) return;
    const v = fromPointer(e);
    live = null;
    if (v !== value) oncommit(v);
  }
  function cancel(): void { live = null; }

  function key(e: KeyboardEvent): void {
    if (disabled) return;
    const unitStep = step && step > 0 ? step : span / 100;
    const big = unitStep * 10;
    let next: number | null = null;
    switch (e.key) {
      case 'ArrowUp': case 'ArrowRight': next = value + unitStep; break;
      case 'ArrowDown': case 'ArrowLeft': next = value - unitStep; break;
      case 'PageUp': next = value + big; break;
      case 'PageDown': next = value - big; break;
      case 'Home': next = min; break;
      case 'End': next = max; break;
      default: return;
    }
    e.preventDefault();
    const v = snapTo(next);
    if (v !== value) oncommit(v);
  }
</script>

<div class="knob" class:off={disabled} data-test="ctl-knob-box">
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <svg
    bind:this={dial}
    class="knob-dial" viewBox="0 0 100 100" width="104" height="104"
    role="slider" tabindex={disabled ? -1 : 0}
    aria-label={label} aria-valuemin={min} aria-valuemax={max} aria-valuenow={shown} aria-disabled={disabled}
    data-test="ctl-knob" data-value={shown}
    onpointerdown={down} onpointermove={move} onpointerup={up} onpointercancel={cancel} onkeydown={key}
  >
    <path class="knob-track" d={arc(START, START + SWEEP, 40)} />
    <path class="knob-fill" d={arc(START, START + SWEEP * frac, 40)} />
    <circle class="knob-cap" cx="50" cy="50" r="30" />
    <line class="knob-hand" x1="50" y1="50" x2={hand[0]} y2={hand[1]} />
  </svg>
  <div class="knob-read mono" data-test="ctl-knob-value">{shown.toFixed(decimals)}{unit ? ` ${unit}` : ''}</div>
  <div class="knob-ends mono"><span>{min}</span><span>{max}</span></div>
</div>

<style>
  .knob { display: flex; flex-direction: column; align-items: center; gap: 4px; }
  .knob.off { opacity: .5; }
  .knob-dial { touch-action: none; cursor: grab; border-radius: 50%; user-select: none; -webkit-user-select: none; }
  .knob-dial:active { cursor: grabbing; }
  .knob-dial:focus-visible { outline: 2px solid var(--hl-focus); outline-offset: 2px; }
  .knob-track { fill: none; stroke: var(--hl-line-strong); stroke-width: 6; stroke-linecap: round; opacity: .45; }
  .knob-fill { fill: none; stroke: var(--hl-orange); stroke-width: 6; stroke-linecap: round; }
  .knob-cap { fill: var(--hl-sunken); stroke: var(--hl-line-strong); stroke-width: 2; }
  .knob-hand { stroke: var(--hl-ink); stroke-width: 4; stroke-linecap: round; }
  .knob-read { font-size: var(--hl-fs-md); font-variant-numeric: tabular-nums; }
  .knob-ends { display: flex; justify-content: space-between; width: 104px; font-size: var(--hl-fs-2xs); color: var(--hl-ink-soft); }
</style>
