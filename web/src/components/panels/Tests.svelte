<script lang="ts">
  // The flow tests: how many there are, a run of them against what is on the
  // canvas, and what each one found. A problem about a node selects it.
  import type { TestReport } from '../../lib/api';

  export type TestsView =
    | { kind: 'info'; note: string; text: string }
    | { kind: 'running' }
    | { kind: 'report'; report: TestReport };

  interface Props {
    view: TestsView;
    onrun: () => void;
    onedit: () => void;
    onpick: (nodeId: string) => void;
  }
  let { view, onrun, onedit, onpick }: Props = $props();
</script>

<section class="section">
  <div class="section-head">
    <h3>Tests</h3>
    <div class="spacer"></div>
    <button class="ghost sm" data-test="tests-run" title="Run the flow tests against what is on the canvas, deployed or not" onclick={onrun}>Run</button>
    <button class="ghost sm" data-test="tests-edit" title="Edit the flow tests" onclick={onedit}>Edit</button>
  </div>
  <div class="section-body">
    <div class="tests" data-test="tests">
      {#if view.kind === 'running'}
        <div class="empty">Running…</div>
      {:else if view.kind === 'info'}
        {#if view.note}<div class="test-summary pass" data-test="tests-note">{view.note}</div>{/if}
        <div class="empty" data-test="tests-info">{view.text}</div>
      {:else}
        {@const tests = view.report.tests ?? []}
        {@const ok = view.report.failed === 0 && view.report.errored === 0}
        <div class="test-summary {ok ? 'pass' : 'fail'}" data-test="tests-summary">
          {tests.length === 0 ? 'No tests ran.' : `${view.report.passed} passed, ${view.report.failed} failed, ${view.report.errored} couldn't run`}
        </div>
        {#each tests as t, i (i)}
          <div class="test-result {t.status}" data-test="test-result" data-status={t.status}>
            <div class="meta">
              <span class="kind test-{t.status}">{t.status === 'error' ? "couldn't run" : t.status}</span>
              <span class="name">{t.name}</span>
            </div>
            {#each t.problems ?? [] as p, j (j)}
              <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
              <pre class="diff-text test-problem" class:pickable={!!p.node} title={p.node ? 'Show the node' : undefined} onclick={() => { if (p.node) onpick(p.node); }}>{(p.expect ? `expect ${p.expect}: ` : '') + p.message}</pre>
            {/each}
          </div>
        {/each}
      {/if}
    </div>
  </div>
</section>
