<script lang="ts">
  // One row a node: what came in, what went out, what failed, and how full its
  // queue is. Clicking a row selects the node on the canvas, which is how you get
  // from "this node is erroring" to the node itself.
  import type { NodeStat } from '../../lib/api';

  interface Props {
    nodes: NodeStat[];
    name: (id: string) => string;
    onpick: (id: string) => void;
  }
  let { nodes, name, onpick }: Props = $props();
</script>

<section class="runtime">
  <div class="section-head"><h3>Runtime</h3></div>
  <div class="stats scroll-x">
    <table>
      <thead>
        <tr><th>Node</th><th class="num">In</th><th class="num">Out</th><th class="num">Err</th><th class="num">Queue</th></tr>
      </thead>
      <tbody>
        {#each nodes as n (n.nodeId)}
          {@const ratio = n.queueCap > 0 ? n.queueLen / n.queueCap : 0}
          <tr class="row" onclick={() => onpick(n.nodeId)}>
            <td class="mono">{name(n.nodeId)}</td>
            <td class="num">{n.received}</td>
            <td class="num">{n.sent}</td>
            <td class="num err" class:some={n.errors > 0}>{n.errors > 0 ? n.errors : '0'}</td>
            <td class="num">{n.queueLen}<span class="meter"><span class={ratio > 0.8 ? 'hot' : ratio > 0.4 ? 'warn' : ''} style:width="{Math.min(100, Math.round(ratio * 100))}%"></span></span></td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
</section>
