<script lang="ts">
  // The replay control of a diagram: play the last run again, slowed down, on the
  // canvas, from what the run recorded. It sits at the bottom of the canvas so it
  // never shares a row with the run bar, and wraps on a phone.
  interface Props {
    playing: boolean;
    index: number;
    total: number;
    speed: number;
    disabled: boolean;
    onreplay: () => void;
    onstop: () => void;
    onspeed: (ms: number) => void;
  }
  let { playing, index, total, speed, disabled, onreplay, onstop, onspeed }: Props = $props();
</script>

<div class="replay-bar" data-test="diagram-replay-bar">
  {#if playing}
    <button class="primary" data-test="diagram-replay-stop" onclick={onstop}>Stop replay</button>
    <span class="replay-progress mono" data-test="diagram-replay-progress" data-index={index} data-total={total}>Replaying {index} of {total}</span>
  {:else}
    <button class="ghost" data-test="diagram-replay" disabled={disabled} title="Play the last run again on the canvas, slowed down, from what it recorded" onclick={onreplay}>Replay last run</button>
  {/if}
  <label class="check replay-speed">Speed
    <select data-test="diagram-replay-speed" value={String(speed)} onchange={(e) => onspeed(Number((e.currentTarget as HTMLSelectElement).value))}>
      <option value="700">Slow</option>
      <option value="350">Normal</option>
      <option value="120">Fast</option>
      <option value="30">Very fast</option>
    </select>
  </label>
</div>
